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

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/disruptionbench"
)

type capturingRecorder struct {
	mu        sync.Mutex
	events    []string
	onPublish func()
}

func (r *capturingRecorder) Publish(evts ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.onPublish != nil {
		r.onPublish()
	}
	for _, e := range evts {
		name := ""
		if o, err := meta.Accessor(e.InvolvedObject); err == nil {
			name = o.GetName()
		}
		r.events = append(r.events, fmt.Sprintf("%s/%s/%s/%s", e.Type, e.Reason, name, e.Message))
	}
}

func parallelPodKeys(pods []*corev1.Pod) []string {
	return lo.Map(pods, func(p *corev1.Pod, _ int) string { return client.ObjectKeyFromObject(p).String() })
}

// resultsFingerprint is the decision-relevant content of a simulation, without pointers or unexported caches.
func resultsFingerprint(r pscheduling.Results) []string {
	var out []string
	for _, nc := range r.NewNodeClaims {
		out = append(out, fmt.Sprintf("new reqs=%s its=%v pods=%v", nc.Requirements.String(),
			lo.Map(nc.InstanceTypeOptions, func(it *cloudprovider.InstanceType, _ int) string { return it.Name }), parallelPodKeys(nc.Pods)))
	}
	sort.Strings(out)
	existing := lo.FilterMap(r.ExistingNodes, func(n *pscheduling.ExistingNode, _ int) (string, bool) {
		return fmt.Sprintf("existing %s pods=%v", n.Name(), parallelPodKeys(n.Pods)), len(n.Pods) > 0
	})
	sort.Strings(existing)
	out = append(out, existing...)
	errs := lo.MapToSlice(r.PodErrors, func(p *corev1.Pod, err error) string {
		return fmt.Sprintf("error %s %v", client.ObjectKeyFromObject(p), err)
	})
	sort.Strings(errs)
	return append(out, errs...)
}

// firstDiff describes the first line where a and b differ, or returns "".
func firstDiff(a, b []string) string {
	for i := range max(len(a), len(b)) {
		if i >= len(a) || i >= len(b) || a[i] != b[i] {
			return fmt.Sprintf("line %d of %d/%d:\n  got  %q\n  want %q", i, len(a), len(b), lo.NthOr(a, i, "<none>"), lo.NthOr(b, i, "<none>"))
		}
	}
	return ""
}

func commandFingerprint(c disruption.Command) []string {
	out := []string{fmt.Sprintf("decision=%s candidates=%v", c.Decision(), c.SourceNodeNames())}
	for _, r := range c.Replacements {
		out = append(out, fmt.Sprintf("replacement reqs=%s its=%v", r.Requirements.String(),
			lo.Map(r.InstanceTypeOptions, func(it *cloudprovider.InstanceType, _ int) string { return it.Name })))
	}
	costs := slices.Sorted(maps.Keys(c.PoolDisruptionCosts))
	out = append(out, lo.Map(costs, func(k string, _ int) string { return fmt.Sprintf("cost %s=%v", k, c.PoolDisruptionCosts[k]) })...)
	return append(out, resultsFingerprint(c.Results)...)
}

func commandsFingerprint(cmds []disruption.Command) [][]string {
	return lo.Map(cmds, func(c disruption.Command, _ int) []string { return commandFingerprint(c) })
}

func parallelSpec(seed int64, nodes int) disruptionbench.Spec {
	plants := lo.Clamp(nodes/50, 1, 5)
	return disruptionbench.Spec{
		Seed: seed, Nodes: nodes, PodsPerNode: 10, NodePools: 1 + int(seed%3), Zones: 3, DaemonSets: 2, SpotFraction: 0.3,
		AntiAffinity: 0.1, ZonalTSC: 0.2, HostnameTSC: 0.1, PDBs: true,
		Plants: []disruptionbench.Plant{
			{Kind: disruptionbench.PlantComplementPair, Count: plants},
			{Kind: disruptionbench.PlantSmallMerge, Count: plants},
			{Kind: disruptionbench.PlantHiddenDelete, Count: plants},
		},
	}
}

type methodRun struct {
	cmds   [][]string
	events []string
	unseen []string
	err    error
}

func (r methodRun) lines() []string {
	out := []string{fmt.Sprintf("err=%v unseen=%v", r.err, r.unseen)}
	for i, c := range r.cmds {
		out = append(out, lo.Map(c, func(l string, _ int) string { return fmt.Sprintf("cmd %d %s", i, l) })...)
	}
	return append(out, lo.Map(r.events, func(e string, _ int) string { return "event " + e })...)
}

type parallelFixture struct {
	env        *disruptionbench.Env
	candidates []*disruption.Candidate
	totals     map[string]disruption.NodePoolTotals
	budgets    map[string]int
	onPublish  func()
}

// newParallelFixture generates s and then removes each workload pod with probability drop, so that multi-node
// consolidation has prefixes that pass and fail.
func newParallelFixture(ctx context.Context, s disruptionbench.Spec, drop float64) *parallelFixture {
	f, err := disruptionbench.Generate(s)
	Expect(err).ToNot(HaveOccurred())
	rng := rand.New(rand.NewPCG(uint64(s.Seed), uint64(drop*100)))
	f.Pods = lo.Filter(f.Pods, func(p *corev1.Pod, _ int) bool {
		owner := metav1.GetControllerOf(p)
		return (owner != nil && owner.Kind == "DaemonSet") || rng.Float64() >= drop
	})
	benv, err := disruptionbench.NewEnv(ctx, f)
	Expect(err).ToNot(HaveOccurred())
	single := benv.Methods().SingleNode
	candidates, totals, err := disruption.GetCandidatesWithTotals(ctx, benv.Cluster, benv.Client, benv.Recorder, benv.Clock, benv.CloudProvider,
		single.ShouldDisrupt, single.Class(), benv.Queue, nil)
	Expect(err).ToNot(HaveOccurred())
	budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, benv.Cluster, benv.Clock, benv.Client, benv.CloudProvider, benv.Recorder, single.Reason())
	Expect(err).ToNot(HaveOccurred())
	return &parallelFixture{env: benv, candidates: candidates, totals: totals, budgets: budgets}
}

// runMulti runs MultiNodeConsolidation.ComputeCommands from the same candidate order at parallelism w.
func (p *parallelFixture) runMulti(ctx context.Context, w int) methodRun {
	rec := &capturingRecorder{}
	c := disruption.MakeConsolidation(p.env.Clock, p.env.Cluster, p.env.Client, p.env.Provisioner, p.env.CloudProvider, rec, p.env.Queue)
	m := disruption.NewMultiNodeConsolidation(c, disruption.WithValidator(disruptionbench.NopValidator{}))
	m.SetNodePoolTotals(p.totals)
	m.SetParallelism(w)
	cmds, err := m.ComputeCommands(ctx, maps.Clone(p.budgets), slices.Clone(p.candidates)...)
	return methodRun{cmds: commandsFingerprint(cmds), events: rec.events, err: err}
}

// runSingle runs single-node consolidation over sorted at parallelism w. SortCandidates interleaves NodePools in map
// order, so the sort happens once outside.
func (p *parallelFixture) runSingle(ctx context.Context, w int, sorted []*disruption.Candidate) methodRun {
	rec := &capturingRecorder{onPublish: p.onPublish}
	c := disruption.MakeConsolidation(p.env.Clock, p.env.Cluster, p.env.Client, p.env.Provisioner, p.env.CloudProvider, rec, p.env.Queue)
	s := disruption.NewSingleNodeConsolidation(c, disruption.WithValidator(disruptionbench.NopValidator{}))
	s.SetNodePoolTotals(p.totals)
	s.SetParallelism(w)
	cmds, err := s.ComputeSorted(ctx, maps.Clone(p.budgets), slices.Clone(sorted))
	return methodRun{cmds: commandsFingerprint(cmds), events: rec.events, unseen: slices.Sorted(maps.Keys(s.PreviouslyUnseenNodePools)), err: err}
}

func (p *parallelFixture) sortedForSingle(ctx context.Context) []*disruption.Candidate {
	rec := &capturingRecorder{}
	c := disruption.MakeConsolidation(p.env.Clock, p.env.Cluster, p.env.Client, p.env.Provisioner, p.env.CloudProvider, rec, p.env.Queue)
	return disruption.NewSingleNodeConsolidation(c).SortCandidates(ctx, slices.Clone(p.candidates))
}

// differentialFixtures is seeds x sizes. DISRUPTIONBENCH_PARALLEL_SEEDS and DISRUPTIONBENCH_PARALLEL_SIZES widen it.
func differentialFixtures() (seeds []int64, sizes []int) {
	n := lo.Must(strconv.Atoi(lo.CoalesceOrEmpty(os.Getenv("DISRUPTIONBENCH_PARALLEL_SEEDS"), "2")))
	for i := range n {
		seeds = append(seeds, int64(i+1))
	}
	sizes = lo.Map(strings.Split(lo.CoalesceOrEmpty(os.Getenv("DISRUPTIONBENCH_PARALLEL_SIZES"), "50,120"), ","), func(s string, _ int) int {
		return lo.Must(strconv.Atoi(strings.TrimSpace(s)))
	})
	return seeds, sizes
}

var _ = Describe("Parallel Consolidation", func() {
	var pctx context.Context
	BeforeEach(func() {
		pctx = options.ToContext(context.Background(), test.Options())
	})

	It("should give concurrent simulations the same results as simulations run alone", func() {
		p := newParallelFixture(pctx, parallelSpec(7, 120), 0.5)
		Expect(len(p.candidates)).To(BeNumerically(">", 20))
		sorted := p.sortedForSingle(pctx)
		var sets [][]*disruption.Candidate
		for i := range 8 {
			sets = append(sets, []*disruption.Candidate{sorted[i*len(sorted)/8]})
		}
		for _, n := range []int{2, 5, 13, 40} {
			sets = append(sets, sorted[:n])
		}
		simulate := func(set []*disruption.Candidate) []string {
			r, err := disruption.SimulateScheduling(pctx, p.env.Client, p.env.Cluster, p.env.Provisioner, p.env.Clock, p.env.Recorder,
				[]pscheduling.Options{pscheduling.IsConsolidationSimulation}, disruption.SimulationOptions{}, set...)
			Expect(err).ToNot(HaveOccurred())
			return resultsFingerprint(r)
		}
		alone := lo.Map(sets, func(set []*disruption.Candidate, _ int) []string { return simulate(set) })
		for i, set := range sets {
			Expect(firstDiff(simulate(set), alone[i])).To(BeEmpty(), "set %d rerun alone", i)
		}
		const copies = 3
		got := make([][]string, len(sets)*copies)
		var wg sync.WaitGroup
		for i := range got {
			wg.Go(func() {
				defer GinkgoRecover()
				got[i] = simulate(sets[i%len(sets)])
			})
		}
		wg.Wait()
		for i := range got {
			Expect(firstDiff(got[i], alone[i%len(sets)])).To(BeEmpty(), "set %d", i%len(sets))
		}
		Expect(p.runMulti(pctx, 8)).To(Equal(p.runMulti(pctx, 1)))
		Expect(p.runSingle(pctx, 8, sorted)).To(Equal(p.runSingle(pctx, 1, sorted)))
	})

	It("should consolidate at most 100 nodes in one multi-node command", func() {
		p := newParallelFixture(pctx, parallelSpec(1, 400), 0.9)
		Expect(len(p.candidates)).To(BeNumerically(">", 101))
		for pool := range p.budgets {
			p.budgets[pool] = len(p.candidates)
		}
		r := p.runMulti(pctx, 1)
		Expect(r.cmds).To(HaveLen(1))
		Expect(strings.Count(r.cmds[0][0], "-node-")).To(Equal(100))
	})

	It("should finish in-flight single-node candidates after the timeout, then stop", func() {
		p := newParallelFixture(pctx, parallelSpec(1, 50), 0)
		sorted := p.sortedForSingle(pctx)
		pools := func(cs []*disruption.Candidate) []string {
			return lo.Uniq(lo.Map(cs, func(c *disruption.Candidate, _ int) string { return c.NodePool.Name }))
		}
		Expect(len(pools(sorted))).To(Equal(2))
		for _, w := range []int{1, 4} {
			stepped := false
			p.onPublish = func() {
				if !stepped {
					stepped = true
					p.env.Clock.Step(disruption.SingleNodeConsolidationTimeoutDuration + time.Second)
				}
			}
			r := p.runSingle(pctx, w, sorted)
			p.onPublish = nil
			Expect(r.err).ToNot(HaveOccurred())
			Expect(r.cmds).To(BeEmpty(), "w=%d", w)
			evaluated := lo.Uniq(lo.Map(r.events, func(e string, _ int) string { return strings.Split(e, "/")[2] }))
			Expect(evaluated).To(Equal(lo.Map(sorted[:w], func(c *disruption.Candidate, _ int) string { return c.Name() })), "w=%d", w)
			Expect(r.unseen).To(ConsistOf(lo.Without(pools(sorted), pools(sorted[:w])...)), "w=%d", w)
		}
	})

	It("should return the sequential commands and events at every parallelism", func() {
		seeds, sizes := differentialFixtures()
		compared := 0
		for _, size := range sizes {
			for _, seed := range seeds {
				for _, drop := range []float64{0, 0.5} {
					p := newParallelFixture(pctx, parallelSpec(seed, size), drop)
					sorted := p.sortedForSingle(pctx)
					seqMulti := p.runMulti(pctx, 1)
					seqSingle := p.runSingle(pctx, 1, sorted)
					Expect(firstDiff(p.runMulti(pctx, 1).lines(), seqMulti.lines())).To(BeEmpty(), "multi rerun seed=%d nodes=%d drop=%v", seed, size, drop)
					Expect(firstDiff(p.runSingle(pctx, 1, sorted).lines(), seqSingle.lines())).To(BeEmpty(), "single rerun seed=%d nodes=%d drop=%v", seed, size, drop)
					Expect(seqMulti.err).ToNot(HaveOccurred())
					Expect(seqSingle.err).ToNot(HaveOccurred())
					for _, w := range []int{2, 4, 8, 16} {
						Expect(firstDiff(p.runMulti(pctx, w).lines(), seqMulti.lines())).To(BeEmpty(), "multi seed=%d nodes=%d drop=%v w=%d", seed, size, drop, w)
						Expect(firstDiff(p.runSingle(pctx, w, sorted).lines(), seqSingle.lines())).To(BeEmpty(), "single seed=%d nodes=%d drop=%v w=%d", seed, size, drop, w)
						compared += 2
					}
					first := func(r methodRun) string {
						return lo.Ternary(len(r.cmds) > 0, lo.FirstOrEmpty(lo.FirstOrEmpty(r.cmds)), "none")
					}
					GinkgoWriter.Printf("parallel differential seed=%d nodes=%d drop=%.1f pools=%d candidates=%d events multi=%d single=%d\n  multi: %s\n  single: %s\n",
						seed, size, drop, len(p.env.Fixture.NodePools), len(p.candidates), len(seqMulti.events), len(seqSingle.events), first(seqMulti), first(seqSingle))
				}
			}
		}
		GinkgoWriter.Printf("parallel differential compared=%d fixtures=%d all equal\n", compared, 2*len(seeds)*len(sizes))
	})
})
