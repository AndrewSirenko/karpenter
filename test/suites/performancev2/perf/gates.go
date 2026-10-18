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

package perf

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type Metric struct {
	Name     string
	Unit     string
	Upper    bool
	Measures string
	OnBreach string
	Value    func(*Result) float64
}

var (
	TotalTimeMinutes = Metric{Name: "total_time_minutes", Unit: "min", Upper: true,
		Measures: "Wall time from the start of the phase (before the trigger) to its end state.",
		OnBreach: "The end state arrived later. With flat cpu_cores Karpenter is waiting (batching, NodeClaim launch, registration, API throttling); with higher cpu_cores it is computing more (scheduler Solve, disruption simulation).",
		Value:    func(r *Result) float64 { return r.Total.Minutes() }}
	HeapMB = Metric{Name: "heap_mb", Unit: "MB", Upper: true,
		Measures: "P95 of the leader's go_gc_heap_live_bytes, sampled every 2s.",
		OnBreach: "Karpenter retains more per pod, node or NodePool (caches, cluster state, per-NodePool offerings). Diff the report's counters and latency_stats against the last green run.",
		Value:    func(r *Result) float64 { return r.Resources.P95HeapMB }}
	CPUCores = Metric{Name: "cpu_cores", Unit: "cores", Upper: true,
		Measures: "Average CPU of the leader over the phase (process_cpu_seconds_total delta over wall time).",
		OnBreach: "More work per event: scheduler Solve, disruption simulation, or extra reconciles (controller_runtime_reconcile_total in the report's counters shows which controller).",
		Value:    func(r *Result) float64 { return r.Resources.AvgCPUCores }}
	StartupSeconds = Metric{Name: "startup_seconds", Unit: "s", Upper: true,
		Measures: "Time from deleting the controller pods to the new leader reporting karpenter_cluster_state_synced=1.",
		OnBreach: "Startup got slower: informer sync, cluster state sync, or a startup-time pass over every object. A crashloop here is an outage at scale.",
		Value:    func(r *Result) float64 { return r.StartupSeconds }}

	// TODO: gate on scheduling latency, the mean of karpenter_scheduler_scheduling_duration_seconds over a
	// phase (it's still in the report as latency_stats). Cut for now because in 13 single runs a negative
	// control (revert #2013) moved it +42%, as much as real regressions did, so it would flake. It needs
	// repeated runs or base-vs-head on the same cluster before it can gate.
	Metrics = []Metric{TotalTimeMinutes, HeapMB, CPUCores, StartupSeconds}
)

// Tier decides what a breached gate does. Every new gate starts Experimental; a provider's owner
// promotes it to Stable once it has held on main (see test/suites/performancev2/README.md).
type Tier string

const (
	TierStable       Tier = "stable"
	TierExperimental Tier = "experimental"
)

type Gate struct {
	Metric Metric
	Bound  float64
	Tier   Tier
}

func (m Metric) Stable(bound float64) Gate { return Gate{Metric: m, Bound: bound, Tier: TierStable} }
func (m Metric) Experimental(bound float64) Gate {
	return Gate{Metric: m, Bound: bound, Tier: TierExperimental}
}

type GateResult struct {
	Metric string  `json:"metric"`
	Value  float64 `json:"value"`
	Bound  float64 `json:"bound"`
	Upper  bool    `json:"upper"`
	Tier   Tier    `json:"tier"`
	Passed bool    `json:"passed"`
}

func (g GateResult) String() string {
	op := ">"
	if g.Upper {
		op = "<"
	}
	return fmt.Sprintf("%s = %.4g, want %s %.4g (%s)", g.Metric, g.Value, op, g.Bound, g.Tier)
}

func overrides() (map[string]map[string]float64, error) {
	o := map[string]map[string]float64{}
	inline := os.Getenv("KARPENTER_PERF_THRESHOLDS")
	if inline == "" {
		return o, nil
	}
	if err := json.NewDecoder(strings.NewReader(inline)).Decode(&o); err != nil {
		return nil, fmt.Errorf("parsing KARPENTER_PERF_THRESHOLDS: %w", err)
	}
	for id, byMetric := range o {
		for name := range byMetric {
			known := false
			for _, m := range Metrics {
				known = known || m.Name == name
			}
			if !known {
				return nil, fmt.Errorf("KARPENTER_PERF_THRESHOLDS %s: unknown metric %q", id, name)
			}
		}
	}
	return o, nil
}

func evaluate(gates []Gate, id string, r *Result) []GateResult {
	GinkgoHelper()
	o, err := overrides()
	Expect(err).ToNot(HaveOccurred())
	results := make([]GateResult, 0, len(gates))
	for _, g := range gates {
		bound := g.Bound
		if v, ok := o[id][g.Metric.Name]; ok {
			bound = v
		}
		v := g.Metric.Value(r)
		results = append(results, GateResult{
			Metric: g.Metric.Name, Value: v, Bound: bound, Upper: g.Metric.Upper, Tier: g.Tier,
			Passed: (g.Metric.Upper && v < bound) || (!g.Metric.Upper && v > bound),
		})
	}
	return results
}

func assertGates(r *Run, id string, results []GateResult) {
	GinkgoHelper()
	var failed []GateResult
	for _, g := range results {
		switch {
		case g.Passed:
			GinkgoWriter.Printf("gate ok   %s: %s\n", id, g)
		case g.Tier == TierExperimental:
			GinkgoWriter.Printf("gate WARN %s: %s\n", id, g)
			AddReportEntry("experimental gate breached", r.alarm(id, []GateResult{g}))
			fmt.Printf("::warning title=perf %s::%s\n", id, g)
		default:
			failed = append(failed, g)
		}
	}
	if len(failed) > 0 {
		Fail(r.alarm(id, failed))
	}
}
