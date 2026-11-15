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
	"math/rand/v2"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/disruptionbench"
)

type simOutcome struct {
	Err       string
	NewClaims []string
	Existing  []string
	PodErrors []string
}

func podKey(p *corev1.Pod) string { return client.ObjectKeyFromObject(p).String() }

func podKeys(pods []*corev1.Pod) string {
	keys := lo.Map(pods, func(p *corev1.Pod, _ int) string { return podKey(p) })
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func requirementsString(nc *pscheduling.NodeClaim) string {
	keys := lo.Filter(lo.Keys(nc.Requirements), func(k string, _ int) bool { return k != corev1.LabelHostname })
	sort.Strings(keys)
	return strings.Join(lo.Map(keys, func(k string, _ int) string { return nc.Requirements.Get(k).String() }), ";")
}

// outcome is the comparable part of a simulation: new claims (pool, instance type options in order, requirements
// without the random hostname, pods), pods placed on each existing node, and pod errors.
func outcome(r pscheduling.Results, err error) simOutcome {
	if err != nil {
		return simOutcome{Err: err.Error()}
	}
	o := simOutcome{}
	for _, nc := range r.NewNodeClaims {
		names := make([]string, 0, len(nc.InstanceTypeOptions))
		for _, it := range nc.InstanceTypeOptions {
			names = append(names, it.Name)
		}
		o.NewClaims = append(o.NewClaims, fmt.Sprintf("pool=%s its=%s reqs=%s pods=%s", nc.NodePoolName, strings.Join(names, ","), requirementsString(nc), podKeys(nc.Pods)))
	}
	sort.Strings(o.NewClaims)
	for _, n := range r.ExistingNodes {
		if len(n.Pods) > 0 {
			o.Existing = append(o.Existing, fmt.Sprintf("%s=%s", n.Name(), podKeys(n.Pods)))
		}
	}
	sort.Strings(o.Existing)
	for p, e := range r.PodErrors {
		o.PodErrors = append(o.PodErrors, fmt.Sprintf("%s: %v", podKey(p), e))
	}
	sort.Strings(o.PodErrors)
	return o
}

type snapshotDiffCase struct {
	name string
	spec disruptionbench.Spec
}

func snapshotDiffCases() []snapshotDiffCase {
	plants := []disruptionbench.Plant{{Kind: disruptionbench.PlantComplementPair, Count: 1}, {Kind: disruptionbench.PlantSmallMerge, Count: 1}, {Kind: disruptionbench.PlantHiddenDelete, Count: 2}}
	var cases []snapshotDiffCase
	for seed := int64(1); seed <= 2; seed++ {
		cases = append(cases,
			snapshotDiffCase{fmt.Sprintf("plain/seed=%d", seed), disruptionbench.Spec{Seed: seed, Nodes: 60, Plants: plants}},
			snapshotDiffCase{fmt.Sprintf("antiaffinity/seed=%d", seed), disruptionbench.Spec{Seed: seed, Nodes: 60, AntiAffinity: 0.3, DaemonSets: 2, Plants: plants}},
			snapshotDiffCase{fmt.Sprintf("tsc/seed=%d", seed), disruptionbench.Spec{Seed: seed, Nodes: 60, ZonalTSC: 0.3, HostnameTSC: 0.2, Plants: plants}},
			snapshotDiffCase{fmt.Sprintf("mixed/seed=%d", seed), disruptionbench.Spec{Seed: seed, Nodes: 80, NodePools: 3, SpotFraction: 0.3, DaemonSets: 3, AntiAffinity: 0.1, ZonalTSC: 0.2, HostnameTSC: 0.1, PDBs: true, Plants: plants}},
		)
	}
	return cases
}

// addHostPorts gives every 25th workload pod a host port, so simulations write host port usage on existing nodes.
func addHostPorts(f *disruptionbench.Fixture) {
	for i, p := range f.Pods {
		if i%25 != 0 || len(p.OwnerReferences) == 0 || p.OwnerReferences[0].Kind == "DaemonSet" || len(p.Spec.Containers) == 0 {
			continue
		}
		p.Spec.Containers[0].Ports = append(p.Spec.Containers[0].Ports, corev1.ContainerPort{ContainerPort: 9100, HostPort: 9100, Protocol: corev1.ProtocolTCP})
	}
}

func snapshotTestContext() context.Context {
	ctx := options.ToContext(injection.WithControllerName(context.Background(), "disruption"), test.Options())
	return log.IntoContext(ctx, logr.Discard())
}

// candidateSets picks 20 random single candidates, random pairs and triples, and a sorted prefix.
func candidateSets(rng *rand.Rand, cs []*disruption.Candidate) [][]*disruption.Candidate {
	var sets [][]*disruption.Candidate
	for _, i := range rng.Perm(len(cs))[:min(len(cs), 20)] {
		sets = append(sets, []*disruption.Candidate{cs[i]})
	}
	for k := 2; k <= 4; k++ {
		for range 6 {
			if len(cs) < k {
				break
			}
			perm := rng.Perm(len(cs))[:k]
			sets = append(sets, lo.Map(perm, func(i int, _ int) *disruption.Candidate { return cs[i] }))
		}
	}
	sets = append(sets, cs[:min(len(cs), 15)])
	return sets
}

func simulate(ctx context.Context, env *disruptionbench.Env, set []*disruption.Candidate) simOutcome {
	r, err := disruption.SimulateScheduling(ctx, env.Client, env.Cluster, env.Provisioner, env.Clock, env.Recorder,
		[]pscheduling.Options{pscheduling.IsConsolidationSimulation}, disruption.SimulationOptions{}, set...)
	return outcome(r, err)
}

// TestSimulateSchedulingSnapshotEquivalence compares SimulateScheduling over the shared node snapshot with the
// per-call deep copy path on generated clusters, across cluster changes between rounds.
func TestSimulateSchedulingSnapshotEquivalence(t *testing.T) {
	ctx := snapshotTestContext()
	total, nonTrivial := 0, 0
	for _, tc := range snapshotDiffCases() {
		t.Run(tc.name, func(t *testing.T) {
			f, err := disruptionbench.Generate(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			addHostPorts(f)
			env, err := disruptionbench.NewEnv(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(uint64(tc.spec.Seed), 7))
			for round := range 4 {
				cs, err := env.Candidates(ctx)
				if err != nil {
					t.Fatal(err)
				}
				slices.SortFunc(cs, func(a, b *disruption.Candidate) int { return strings.Compare(a.Name(), b.Name()) })
				sets := candidateSets(rng, cs)
				want := make([]simOutcome, len(sets))
				restore := disruption.SetSimulateFromSnapshot(false)
				for i, set := range sets {
					want[i] = simulate(ctx, env, set)
					if again := simulate(ctx, env, set); !reflect.DeepEqual(want[i], again) {
						t.Errorf("round %d set %d: deep copy path is nondeterministic", round, i)
					}
				}
				restore()
				for i, set := range sets {
					got := simulate(ctx, env, set)
					if !reflect.DeepEqual(want[i], got) {
						t.Fatalf("round %d, candidates %v: snapshot path differs\nwant %+v\ngot  %+v", round, lo.Map(set, func(c *disruption.Candidate, _ int) string { return c.Name() }), want[i], got)
					}
					total++
					if len(got.Existing)+len(got.NewClaims)+len(got.PodErrors) > 0 {
						nonTrivial++
					}
				}
				// Concurrent readers of one snapshot must agree with the deep copy path too.
				var wg sync.WaitGroup
				errs := make([]string, len(sets))
				for i := range sets {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if got := simulate(ctx, env, sets[i]); !reflect.DeepEqual(want[i], got) {
							errs[i] = fmt.Sprintf("concurrent set %d differs\nwant %+v\ngot  %+v", i, want[i], got)
						}
					}()
				}
				wg.Wait()
				for _, e := range errs {
					if e != "" {
						t.Fatalf("round %d: %s", round, e)
					}
				}
				total += len(sets)
				mutateCluster(ctx, t, env, rng, cs, round)
			}
		})
	}
	t.Logf("compared %d simulations (%d with placements or errors), all equal", total, nonTrivial)
}

// mutateCluster changes cluster state between rounds: apply a consolidation command, mark a node for deletion,
// nominate a node, or unmark the earlier deletion.
func mutateCluster(ctx context.Context, t *testing.T, env *disruptionbench.Env, rng *rand.Rand, cs []*disruption.Candidate, round int) {
	t.Helper()
	switch round % 4 {
	case 0:
		m := env.Methods()
		if _, err := disruptionbench.Replay(ctx, env, []disruption.Method{m.Emptiness, m.MultiNode, m.SingleNode}, disruptionbench.ReplayOptions{MaxIterations: 1}); err != nil {
			t.Fatal(err)
		}
	case 1:
		if len(cs) > 0 {
			env.Cluster.MarkForDeletion(cs[rng.IntN(len(cs))].ProviderID())
		}
		if len(cs) > 1 {
			env.Cluster.NominateNodeForPod(ctx, cs[rng.IntN(len(cs))].ProviderID())
		}
	case 2:
		m := env.Methods()
		if _, err := disruptionbench.Replay(ctx, env, []disruption.Method{m.SingleNode}, disruptionbench.ReplayOptions{MaxIterations: 1}); err != nil {
			t.Fatal(err)
		}
	}
}
