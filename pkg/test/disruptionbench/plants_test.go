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

package disruptionbench_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/test/disruptionbench"
)

const tolerance = 1e-6

// moveSavings applies computeConsolidation's rules to a simulated removal of candidates: every pod must schedule, at
// most one new node, and the new node must be cheaper than the candidates. It returns false for an illegal move.
func moveSavings(ctx context.Context, env *disruptionbench.Env, candidates []*disruption.Candidate) (float64, bool) {
	results, err := disruption.SimulateScheduling(ctx, env.Client, env.Cluster, env.Provisioner, env.Clock, env.Recorder,
		[]pscheduling.Options{pscheduling.IsConsolidationSimulation}, disruption.SimulationOptions{}, candidates...)
	Expect(err).ToNot(HaveOccurred())
	if !results.AllNonPendingPodsScheduled() || len(results.NewNodeClaims) > 1 {
		return 0, false
	}
	cost := lo.SumBy(candidates, func(c *disruption.Candidate) float64 { return c.Price })
	if len(results.NewNodeClaims) == 0 {
		return cost, true
	}
	nc := results.NewNodeClaims[0]
	replacement := math.MaxFloat64
	for _, it := range nc.InstanceTypeOptions {
		if launchable := it.Offerings.Launchable(); launchable.HasCompatible(nc.Requirements) {
			replacement = math.Min(replacement, launchable.WorstLaunchPrice(nc.Requirements))
		}
	}
	if replacement >= cost {
		return 0, false
	}
	return cost - replacement, true
}

func subsets(n, k int, visit func([]int)) {
	idx := make([]int, k)
	var rec func(start, depth int)
	rec = func(start, depth int) {
		if depth == k {
			visit(append([]int(nil), idx...))
			return
		}
		for i := start; i < n; i++ {
			idx[depth] = i
			rec(i+1, depth+1)
		}
	}
	rec(0, 0)
}

var _ = Describe("Generate", func() {
	It("should be deterministic from the seed", func() {
		a, err := disruptionbench.Generate(smallSpec(7))
		Expect(err).ToNot(HaveOccurred())
		b, err := disruptionbench.Generate(smallSpec(7))
		Expect(err).ToNot(HaveOccurred())
		Expect(a.Planted).To(Equal(b.Planted))
		name := func(p *corev1.Pod, _ int) string { return p.Name }
		Expect(lo.Map(a.Pods, name)).To(Equal(lo.Map(b.Pods, name)))
		for i := range a.Pods {
			Expect(a.Pods[i].Spec).To(Equal(b.Pods[i].Spec))
		}
		for i := range a.Nodes {
			Expect(a.Nodes[i].Labels).To(Equal(b.Nodes[i].Labels))
		}
		c, err := disruptionbench.Generate(smallSpec(8))
		Expect(err).ToNot(HaveOccurred())
		Expect(c.Planted).ToNot(Equal(a.Planted))
	})
	It("should generate large fixtures", func() {
		for _, n := range []int{200, 2000} {
			f, err := disruptionbench.Generate(disruptionbench.Spec{Seed: 1, Nodes: n, NodePools: 3, SpotFraction: 0.3, DaemonSets: 4, AntiAffinity: 0.1, ZonalTSC: 0.2, HostnameTSC: 0.1, PDBs: true,
				Plants: []disruptionbench.Plant{{Kind: disruptionbench.PlantComplementPair, Count: 5}, {Kind: disruptionbench.PlantSmallMerge, Count: 5}, {Kind: disruptionbench.PlantHiddenDelete, Count: 5}}})
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Nodes).To(HaveLen(n))
			Expect(f.Planted).To(HaveLen(15))
		}
	})
})

// checkGroundTruth simulates every removal of up to maxK nodes. Each planted move must be legal and save exactly its
// SavingsPerHour, and no move may save more than the planted moves it touches.
func checkGroundTruth(f *disruptionbench.Fixture, maxK int) {
	env := mustEnv(f)
	candidates, err := disruption.GetCandidates(ctx, env.Cluster, env.Client, env.Recorder, env.Clock, env.CloudProvider,
		func(context.Context, *disruption.Candidate) bool { return true }, disruption.GracefulDisruptionClass, env.Queue)
	Expect(err).ToNot(HaveOccurred())
	Expect(candidates).To(HaveLen(len(f.Nodes)))
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name() < candidates[j].Name() })
	index := lo.SliceToMap(lo.Range(len(candidates)), func(i int) (string, int) { return candidates[i].Name(), i })

	start := time.Now()
	var mu sync.Mutex
	feasible, sims := 0, 0
	worst, worstExcess := "", math.Inf(-1)
	saving := map[string]float64{}
	work := make(chan []int)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for set := range work {
				cs := lo.Map(set, func(i int, _ int) *disruption.Candidate { return candidates[i] })
				s, ok := moveSavings(ctx, env, cs)
				names := sets.New(lo.Map(cs, func(c *disruption.Candidate, _ int) string { return c.Name() })...)
				bound := lo.SumBy(f.Planted, func(p disruptionbench.PlantedMove) float64 {
					return lo.Ternary(lo.SomeBy(p.NodeNames, names.Has), p.SavingsPerHour, 0)
				})
				key := strings.Join(sets.List(names), ",")
				mu.Lock()
				sims++
				if ok {
					feasible++
					saving[key] = s
					if s-bound > worstExcess {
						worst, worstExcess = key, s-bound
					}
				}
				mu.Unlock()
			}
		})
	}
	for k := 1; k <= maxK; k++ {
		subsets(len(candidates), k, func(set []int) { work <- set })
	}
	close(work)
	wg.Wait()

	GinkgoWriter.Printf("%d nodes, %d planted, %d removals of 1-%d nodes simulated in %s, %d legal\n", len(candidates), len(f.Planted), sims, maxK, time.Since(start).Round(time.Millisecond), feasible)
	for _, p := range f.Planted {
		key := strings.Join(sets.List(sets.New(p.NodeNames...)), ",")
		Expect(p.NodeNames).To(HaveEach(BeKeyOf(index)))
		if len(p.NodeNames) > maxK {
			continue
		}
		GinkgoWriter.Printf("  planted %-16s %-60s planted $%.4f/hr brute force $%.4f/hr\n", p.Kind, key, p.SavingsPerHour, saving[key])
		Expect(saving).To(HaveKey(key), "planted %s %s isn't a legal move", p.Kind, key)
		Expect(saving[key]).To(BeNumerically("~", p.SavingsPerHour, tolerance))
	}
	GinkgoWriter.Printf("  max over legal moves of (savings - planted savings it touches): $%.4f/hr (%s)\n", worstExcess, worst)
	Expect(worstExcess).To(BeNumerically("<=", tolerance), "move %s saves more than its plants", worst)
}

var _ = Describe("Plants", func() {
	for _, seed := range []int64{1, 2, 3} {
		It(fmt.Sprintf("should be ground truth by brute force over removals of 1-4 nodes, seed %d", seed), func() {
			f, err := disruptionbench.Generate(smallSpec(seed))
			Expect(err).ToNot(HaveOccurred())
			checkGroundTruth(f, 4)
		})
	}
	It("should be ground truth over removals of 1-2 nodes at 200 nodes", func() {
		f, err := disruptionbench.Generate(disruptionbench.Spec{Seed: 1, Nodes: 200, PodsPerNode: 10, NodePools: 3, Zones: 3, DaemonSets: 4, SpotFraction: 0.3,
			AntiAffinity: 0.1, ZonalTSC: 0.2, HostnameTSC: 0.1, PDBs: true,
			Plants: []disruptionbench.Plant{{Kind: disruptionbench.PlantComplementPair, Count: 5}, {Kind: disruptionbench.PlantSmallMerge, Count: 5}, {Kind: disruptionbench.PlantHiddenDelete, Count: 5}}})
		Expect(err).ToNot(HaveOccurred())
		checkGroundTruth(f, 2)
	})
})
