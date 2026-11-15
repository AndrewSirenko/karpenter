//go:build test_performance

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

package disruption_test

// Consolidation simulation benchmarks over a disruptionbench.Env. Sub-benchmarks follow the
// vector=<name>/<param>=<n> naming of the scheduling benchmarks. DISRUPTIONBENCH_NODES overrides the node
// counts, e.g. DISRUPTIONBENCH_NODES=1000,5000,10000,20000.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	prometheusmodel "github.com/prometheus/client_model/go"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/disruptionbench"
)

const benchPodsPerNode = 20

// benchConstraint is the constraint carried by a fraction of deployments.
type benchConstraint string

const (
	constraintNone         benchConstraint = "none"
	constraintAntiAffinity benchConstraint = "antiaffinity"
	constraintZonalTSC     benchConstraint = "zonaltsc"
	constraintHostnameTSC  benchConstraint = "hostnametsc"
)

type benchScenario struct {
	Nodes       int
	PodsPerNode int
	Constraint  benchConstraint
	// Percent of deployments carrying Constraint.
	Percent int
}

var benchModes = []benchScenario{
	{Constraint: constraintNone},
	{Constraint: constraintAntiAffinity, Percent: 10},
	{Constraint: constraintZonalTSC, Percent: 30},
	{Constraint: constraintHostnameTSC, Percent: 30},
}

func (s benchScenario) vector() string {
	if s.Constraint == constraintNone {
		return "nodes"
	}
	return fmt.Sprintf("nodes_%s%d", s.Constraint, s.Percent)
}

func benchNodeCounts(b *testing.B, def ...int) []int {
	v := os.Getenv("DISRUPTIONBENCH_NODES")
	if v == "" {
		return def
	}
	return lo.Map(strings.Split(v, ","), func(s string, _ int) int { return lo.Must(strconv.Atoi(strings.TrimSpace(s))) })
}

func benchContext() context.Context {
	ctx := options.ToContext(injection.WithControllerName(context.Background(), "disruption"), test.Options())
	return log.IntoContext(ctx, logr.Discard())
}

const benchReplicas = 10

// benchFixture builds s.Nodes half-full nodes over three instance types and three zones. Each deployment has
// benchReplicas pods on consecutive nodes, so its replicas never share a node and spread evenly over zones.
func benchFixture(s benchScenario) *disruptionbench.Fixture {
	rng := rand.New(rand.NewPCG(uint64(s.Nodes), uint64(s.Percent)))
	its := lo.Map([]int{4, 8, 16}, func(cpu int, _ int) *cloudprovider.InstanceType {
		return fake.NewInstanceType(fmt.Sprintf("bench-%dx", cpu), fake.WithResources(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(strconv.Itoa(cpu)),
			corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", cpu*4)),
			corev1.ResourcePods:   resource.MustParse("110"),
		}))
	})
	np := test.NodePool(v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	np.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "10%"}}
	f := &disruptionbench.Fixture{NodePools: []*v1.NodePool{np}, InstanceTypes: its}
	zones := []string{"test-zone-1", "test-zone-2", "test-zone-3"}
	perSlot := (s.Nodes + benchReplicas - 1) / benchReplicas
	constrained := map[int]bool{}
	for d := range perSlot * s.PodsPerNode {
		constrained[d] = rng.IntN(100) < s.Percent
	}
	for i := range s.Nodes {
		it := its[i%len(its)]
		name := fmt.Sprintf("node-%05d", i)
		nc, node := test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
				v1.NodePoolLabelKey:            np.Name,
				corev1.LabelInstanceTypeStable: it.Name,
				v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
				corev1.LabelTopologyZone:       zones[i%len(zones)],
				corev1.LabelHostname:           name,
			}},
			Status: v1.NodeClaimStatus{NodeName: name, Capacity: it.Capacity, Allocatable: it.Allocatable()},
		})
		node.Name = name
		f.NodeClaims = append(f.NodeClaims, nc)
		f.Nodes = append(f.Nodes, node)
		alloc := it.Allocatable()
		cpu := resource.NewMilliQuantity(alloc.Cpu().MilliValue()/int64(2*s.PodsPerNode), resource.DecimalSI)
		mem := resource.NewQuantity(alloc.Memory().Value()/int64(2*s.PodsPerNode), resource.BinarySI)
		for slot := range s.PodsPerNode {
			d := slot*perSlot + i/benchReplicas
			app := fmt.Sprintf("app-%d", d)
			selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}}
			opts := test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Name:   fmt.Sprintf("%s-%d", app, i%benchReplicas),
					Labels: map[string]string{"app": app},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "apps/v1", Kind: "ReplicaSet", Name: app, UID: types.UID(app), Controller: lo.ToPtr(true),
					}},
				},
				NodeName:             name,
				Phase:                corev1.PodRunning,
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: *cpu, corev1.ResourceMemory: *mem}},
			}
			if constrained[d] {
				switch s.Constraint {
				case constraintAntiAffinity:
					opts.PodAntiRequirements = []corev1.PodAffinityTerm{{LabelSelector: selector, TopologyKey: corev1.LabelHostname}}
				case constraintZonalTSC:
					opts.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
						MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule, LabelSelector: selector,
					}}
				case constraintHostnameTSC:
					opts.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
						MaxSkew: 1, TopologyKey: corev1.LabelHostname, WhenUnsatisfiable: corev1.DoNotSchedule, LabelSelector: selector,
					}}
				}
			}
			f.Pods = append(f.Pods, test.Pod(opts))
		}
	}
	return f
}

var (
	benchEnvMu sync.Mutex
	benchEnvs  = map[benchScenario]*disruptionbench.Env{}
)

// benchEnv returns a memoized Env, since b.Run reruns the body as b.N grows. Benchmarks must not mutate it.
func benchEnv(b *testing.B, ctx context.Context, s benchScenario) *disruptionbench.Env {
	b.Helper()
	benchEnvMu.Lock()
	defer benchEnvMu.Unlock()
	if s.PodsPerNode == 0 {
		s.PodsPerNode = benchPodsPerNode
	}
	if env, ok := benchEnvs[s]; ok {
		return env
	}
	// Keep one large Env alive at a time.
	clear(benchEnvs)
	start := time.Now()
	env, err := disruptionbench.NewEnv(ctx, benchFixture(s))
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("built %+v in %s", s, time.Since(start))
	benchEnvs[s] = env
	return env
}

// benchCandidate returns the candidate with the lowest name, so every run simulates the same node.
func benchCandidate(b *testing.B, ctx context.Context, env *disruptionbench.Env) (*disruption.Candidate, int) {
	b.Helper()
	candidates, err := env.Candidates(ctx)
	if err != nil {
		b.Fatal(err)
	}
	if len(candidates) != len(env.Fixture.Nodes) {
		b.Fatalf("expected %d candidates, got %d", len(env.Fixture.Nodes), len(candidates))
	}
	return slices.MinFunc(candidates, func(a, c *disruption.Candidate) int { return strings.Compare(a.Name(), c.Name()) }), len(candidates)
}

func runSimulateScheduling(b *testing.B, s benchScenario, schedulerOpts ...pscheduling.Options) {
	ctx := benchContext()
	env := benchEnv(b, ctx, s)
	candidate, _ := benchCandidate(b, ctx, env)
	opts := append([]pscheduling.Options{pscheduling.IsConsolidationSimulation}, schedulerOpts...)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		results, err := disruption.SimulateScheduling(ctx, env.Client, env.Cluster, env.Provisioner, env.Clock, env.Recorder, opts, disruption.SimulationOptions{}, candidate)
		if err != nil {
			b.Fatal(err)
		}
		if !results.AllNonPendingPodsScheduled() {
			b.Fatalf("expected the candidate's pods to schedule, %s", results.NonPendingPodSchedulingErrors())
		}
		if len(results.NewNodeClaims) != 0 {
			b.Fatalf("expected the candidate's pods to fit existing nodes, got %d new nodeclaims", len(results.NewNodeClaims))
		}
	}
}

// BenchmarkSimulateScheduling measures one single-candidate consolidation simulation.
func BenchmarkSimulateScheduling(b *testing.B) {
	for _, mode := range benchModes {
		for _, n := range benchNodeCounts(b, 1000, 5000, 10000) {
			s := mode
			s.Nodes = n
			b.Run(fmt.Sprintf("vector=%s/n=%d", s.vector(), n), func(b *testing.B) { runSimulateScheduling(b, s) })
		}
	}
}

// BenchmarkSimulateSchedulingAfterChange measures one simulation after writes to one node, so each sim rebuilds the
// node snapshot for that node. The writes cancel out, leaving the shared Env unchanged.
func BenchmarkSimulateSchedulingAfterChange(b *testing.B) {
	for _, n := range benchNodeCounts(b, 1000, 5000, 10000) {
		s := benchScenario{Nodes: n, Constraint: constraintNone}
		b.Run(fmt.Sprintf("vector=%s/n=%d", s.vector(), n), func(b *testing.B) {
			ctx := benchContext()
			env := benchEnv(b, ctx, s)
			candidate, _ := benchCandidate(b, ctx, env)
			other := env.Fixture.NodeClaims[len(env.Fixture.NodeClaims)-1].Status.ProviderID
			opts := []pscheduling.Options{pscheduling.IsConsolidationSimulation}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				env.Cluster.MarkForDeletion(other)
				env.Cluster.UnmarkForDeletion(other)
				if _, err := disruption.SimulateScheduling(ctx, env.Client, env.Cluster, env.Provisioner, env.Clock, env.Recorder, opts, disruption.SimulationOptions{}, candidate); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSimulateSchedulingConcurrent measures the simulation with the scheduler's existing-node scan parallelized,
// which the disruption path never enables.
func BenchmarkSimulateSchedulingConcurrent(b *testing.B) {
	for _, mode := range []benchScenario{benchModes[0], benchModes[1]} {
		for _, n := range benchNodeCounts(b, 10000) {
			for _, par := range []int{1, 4, 16} {
				s := mode
				s.Nodes = n
				b.Run(fmt.Sprintf("vector=%s_par%d/n=%d", s.vector(), par, n), func(b *testing.B) {
					runSimulateScheduling(b, s, pscheduling.NumConcurrentReconciles(par))
				})
			}
		}
	}
}

// BenchmarkGetCandidates measures building the consolidation candidates.
func BenchmarkGetCandidates(b *testing.B) {
	for _, n := range benchNodeCounts(b, 1000, 5000, 10000) {
		s := benchScenario{Nodes: n, Constraint: constraintNone}
		b.Run(fmt.Sprintf("vector=%s/n=%d", s.vector(), n), func(b *testing.B) {
			ctx := benchContext()
			env := benchEnv(b, ctx, s)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := env.Candidates(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func consolidationTimeouts(b *testing.B, consolidationType string) float64 {
	mfs, err := crmetrics.Registry.Gather()
	if err != nil {
		b.Fatal(err)
	}
	total := 0.0
	for _, mf := range mfs {
		if mf.GetName() != "karpenter_voluntary_disruption_consolidation_timeouts_total" {
			continue
		}
		for _, m := range mf.Metric {
			if lo.ContainsBy(m.Label, func(l *prometheusmodel.LabelPair) bool { return l.GetValue() == consolidationType }) {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}

func runComputeCommands(b *testing.B, s benchScenario, method func(disruptionbench.Methods) disruption.Method) {
	runComputeCommandsOn(b, benchEnv(b, benchContext(), s), 1, method)
}

func runComputeCommandsOn(b *testing.B, env *disruptionbench.Env, parallelism int, method func(disruptionbench.Methods) disruption.Method) {
	ctx := benchContext()
	b.ReportAllocs()
	b.ResetTimer()
	var nodes int
	timeouts := 0.0
	for range b.N {
		m := method(env.Methods())
		m.(interface{ SetParallelism(int) }).SetParallelism(parallelism)
		before := consolidationTimeouts(b, m.ConsolidationType())
		start := time.Now()
		cmds, err := env.ComputeCommands(ctx, m)
		if err != nil {
			b.Fatal(err)
		}
		if len(cmds) != 1 {
			b.Fatalf("expected one command, got %d after %s", len(cmds), time.Since(start))
		}
		if time.Since(start) >= disruption.MultiNodeConsolidationTimeoutDuration {
			b.Logf("ran past the multi-node timeout, %s", time.Since(start))
		}
		timeouts += consolidationTimeouts(b, m.ConsolidationType()) - before
		nodes = len(cmds[0].Candidates)
	}
	b.ReportMetric(float64(nodes), "nodes/cmd")
	b.ReportMetric(timeouts/float64(b.N), "timeouts/op")
}

var benchParallelism = []int{1, 4, 8, 16}

// BenchmarkMultiNodeConsolidationParallel is BenchmarkMultiNodeConsolidation at each consolidation parallelism.
func BenchmarkMultiNodeConsolidationParallel(b *testing.B) {
	for _, mode := range []benchScenario{benchModes[0], benchModes[1]} {
		for _, n := range benchNodeCounts(b, 1000, 5000) {
			for _, w := range benchParallelism {
				s := mode
				s.Nodes = n
				b.Run(fmt.Sprintf("vector=%s_w%d/n=%d", s.vector(), w, n), func(b *testing.B) {
					runComputeCommandsOn(b, benchEnv(b, benchContext(), s), w, func(m disruptionbench.Methods) disruption.Method { return m.MultiNode })
				})
			}
		}
	}
}

// BenchmarkSingleNodeConsolidationParallel is BenchmarkSingleNodeConsolidation at each consolidation parallelism.
func BenchmarkSingleNodeConsolidationParallel(b *testing.B) {
	for _, mode := range []benchScenario{benchModes[0], benchModes[1]} {
		for _, n := range benchNodeCounts(b, 1000, 5000) {
			for _, w := range benchParallelism {
				s := mode
				s.Nodes = n
				b.Run(fmt.Sprintf("vector=%s_w%d/n=%d", s.vector(), w, n), func(b *testing.B) {
					runComputeCommandsOn(b, benchEnv(b, benchContext(), s), w, func(m disruptionbench.Methods) disruption.Method { return m.SingleNode })
				})
			}
		}
	}
}

var (
	walkEnvMu sync.Mutex
	walkEnvs  = map[int]*disruptionbench.Env{}
)

// BenchmarkSingleNodeConsolidationWalk runs SingleNode over generated fixtures whose noise nodes are packed tight, so
// it walks the sort order to a hidden delete instead of stopping at the first candidate.
func BenchmarkSingleNodeConsolidationWalk(b *testing.B) {
	for _, n := range benchNodeCounts(b, 1000) {
		for _, w := range benchParallelism {
			b.Run(fmt.Sprintf("vector=generated_w%d/n=%d", w, n), func(b *testing.B) {
				walkEnvMu.Lock()
				env, ok := walkEnvs[n]
				if !ok {
					clear(walkEnvs)
					f, err := disruptionbench.Generate(disruptionbench.Spec{
						Seed: 1, Nodes: n, PodsPerNode: 10, NodePools: 3, Zones: 3, DaemonSets: 4, SpotFraction: 0.3,
						AntiAffinity: 0.1, ZonalTSC: 0.2, HostnameTSC: 0.1, PDBs: true,
						Plants: []disruptionbench.Plant{{Kind: disruptionbench.PlantHiddenDelete, Count: 10}},
					})
					if err != nil {
						b.Fatal(err)
					}
					if env, err = disruptionbench.NewEnv(benchContext(), f); err != nil {
						b.Fatal(err)
					}
					walkEnvs[n] = env
				}
				walkEnvMu.Unlock()
				runComputeCommandsOn(b, env, w, func(m disruptionbench.Methods) disruption.Method { return m.SingleNode })
			})
		}
	}
}

// BenchmarkMultiNodeConsolidation measures one MultiNodeConsolidation.ComputeCommands, including GetCandidates.
func BenchmarkMultiNodeConsolidation(b *testing.B) {
	for _, mode := range []benchScenario{benchModes[0], benchModes[1]} {
		for _, n := range benchNodeCounts(b, 1000, 5000) {
			s := mode
			s.Nodes = n
			b.Run(fmt.Sprintf("vector=%s/n=%d", s.vector(), n), func(b *testing.B) {
				runComputeCommands(b, s, func(m disruptionbench.Methods) disruption.Method { return m.MultiNode })
			})
		}
	}
}

// BenchmarkSingleNodeConsolidation measures one SingleNodeConsolidation.ComputeCommands, including GetCandidates.
func BenchmarkSingleNodeConsolidation(b *testing.B) {
	for _, mode := range []benchScenario{benchModes[0], benchModes[1]} {
		for _, n := range benchNodeCounts(b, 1000, 5000) {
			s := mode
			s.Nodes = n
			b.Run(fmt.Sprintf("vector=%s/n=%d", s.vector(), n), func(b *testing.B) {
				runComputeCommands(b, s, func(m disruptionbench.Methods) disruption.Method { return m.SingleNode })
			})
		}
	}
}
