/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package performancev2

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

type Phase struct {
	ID string

	env               *common.Environment
	profiler          *common.KarpenterProfiler
	poller            *common.KarpenterMetricsPoller
	latency           *common.LatencyHarness
	start             time.Time
	initialNodes      int
	baselineDisrupted int
}

func StartPhase(env *common.Environment, id string) *Phase {
	GinkgoHelper()
	_, ok := gates[id]
	Expect(ok).To(BeTrue(), "no gates registered for phase %q", id)
	latency, err := common.StartLatencyHarness(env)
	Expect(err).ToNot(HaveOccurred())
	return &Phase{
		ID:                id,
		env:               env,
		profiler:          common.StartKarpenterProfiler(env),
		poller:            common.StartKarpenterMetricsPoller(env),
		latency:           latency,
		start:             time.Now(),
		initialNodes:      env.Monitor.CreatedNodeCount(),
		baselineDisrupted: getPodsDisruptedCount(env),
	}
}

func (p *Phase) AwaitScaleOut(expectedPods int, timeout time.Duration) *PerformanceReport {
	GinkgoHelper()
	p.awaitHealthyPods(expectedPods, timeout)
	return p.end("scale-out", expectedPods, expectedPods, 1, time.Now())
}

func (p *Phase) AwaitConsolidation(initialPods, finalPods int, timeout time.Duration) *PerformanceReport {
	GinkgoHelper()
	p.awaitHealthyPods(finalPods, timeout)
	rounds, lastActivity := awaitDisruptionSettled(p.env, timeout, consolidationQuiet)
	return p.end("consolidation", finalPods, finalPods-initialPods, rounds, lastActivity)
}

func (p *Phase) AwaitDrift(expectedPods int, timeout time.Duration) *PerformanceReport {
	GinkgoHelper()
	rounds, lastActivity := awaitDisruptionSettled(p.env, timeout, driftQuiet)
	p.awaitHealthyPods(expectedPods, timeout/2)
	return p.end("drift", expectedPods, 0, max(rounds, 1), lastActivity)
}

func (p *Phase) awaitHealthyPods(n int, timeout time.Duration) {
	GinkgoHelper()
	if n > 0 {
		p.env.EventuallyExpectHealthyPodCountWithTimeout(timeout, labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"}), n)
	}
}

func (p *Phase) end(testType string, pods, podsNetChange, rounds int, lastActivity time.Time) *PerformanceReport {
	GinkgoHelper()
	totalTime := time.Since(p.start)
	memProfile, cpuProfile := p.profiler.Stop()
	stats := p.poller.Stop()
	latency, err := p.latency.Stop()
	Expect(err).ToNot(HaveOccurred())

	nodes := p.env.Monitor.CreatedNodeCount()
	cpuUtil := p.env.Monitor.AvgUtilization(corev1.ResourceCPU)
	memUtil := p.env.Monitor.AvgUtilization(corev1.ResourceMemory)
	r := &PerformanceReport{
		TestName:                p.ID,
		TestType:                testType,
		TotalPods:               pods,
		TotalNodes:              nodes,
		TotalTime:               totalTime,
		ActiveTime:              lastActivity.Sub(p.start),
		PodsNetChange:           podsNetChange,
		NodesNetChange:          nodes - p.initialNodes,
		PodsDisrupted:           getPodsDisruptedCount(p.env) - p.baselineDisrupted,
		TotalReservedCPUUtil:    cpuUtil,
		TotalReservedMemoryUtil: memUtil,
		ResourceEfficiencyScore: cpuUtil*90 + memUtil*10,
		Rounds:                  rounds,
		Timestamp:               time.Now(),
		KarpenterP95MemoryMB:    stats.P95MemoryMB,
		KarpenterAvgMemoryMB:    stats.AvgMemoryMB,
		KarpenterMaxMemoryMB:    stats.MaxMemoryMB,
		KarpenterP95HeapMB:      stats.P95HeapMB,
		KarpenterAvgHeapMB:      stats.AvgHeapMB,
		KarpenterMaxHeapMB:      stats.MaxHeapMB,
		KarpenterP95CPUCores:    stats.P95CPUCores,
		KarpenterAvgCPUCores:    stats.AvgCPUCores,
		KarpenterMaxCPUCores:    stats.MaxCPUCores,
		MetricsSampleCount:      stats.SampleCount,
		LatencyStats:            latency.LatencyStats,
		Counters:                latency.Counters,
		MemoryProfileData:       memProfile,
		CPUProfileData:          cpuProfile,
	}
	if nodes > 0 {
		r.PodsPerNode = float64(pods) / float64(nodes)
	}
	r.Gates, err = evaluate(p.ID, r)
	Expect(err).ToNot(HaveOccurred())
	OutputPerformanceReport(r, strings.ReplaceAll(p.ID, "/", "_"))
	Expect(r.MetricsSampleCount).To(BeNumerically(">", 0), "collected no Karpenter metrics samples")
	assertGates(p.ID, r.Gates)
	return r
}

const (
	consolidationQuiet = 3 * time.Minute
	driftQuiet         = 2 * time.Minute
	settlePoll         = 15 * time.Second
)

func awaitDisruptionSettled(env *common.Environment, timeout, quiet time.Duration) (int, time.Time) {
	start := time.Now()
	lastActivity := start
	rounds := 0
	inRound := false
	for time.Since(start) < timeout && time.Since(lastActivity) < quiet {
		if disruptingNodes(env) > 0 {
			if !inRound {
				rounds++
			}
			inRound = true
			lastActivity = time.Now()
		} else {
			inRound = false
		}
		time.Sleep(settlePoll)
	}
	return rounds, lastActivity
}

func disruptingNodes(env *common.Environment) int {
	n := 0
	for _, node := range env.Monitor.CreatedNodes() {
		if node.DeletionTimestamp != nil || hasTaint(node, v1.DisruptedTaintKey) {
			n++
		}
	}
	return n
}

func hasTaint(node *corev1.Node, key string) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == key {
			return true
		}
	}
	return false
}
