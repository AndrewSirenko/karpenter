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

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	realclock "k8s.io/utils/clock"

	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/disruptionbench"
)

// unconsolidatableRecorder keeps the Unconsolidatable messages published for each node.
type unconsolidatableRecorder struct {
	events.Recorder
	mu       sync.Mutex
	messages map[string]map[string]int
}

func (r *unconsolidatableRecorder) Publish(evts ...events.Event) {
	r.mu.Lock()
	for _, e := range evts {
		if n, ok := e.InvolvedObject.(*corev1.Node); ok && e.Reason == events.Unconsolidatable {
			if r.messages[n.Name] == nil {
				r.messages[n.Name] = map[string]int{}
			}
			r.messages[n.Name][e.Message]++
		}
	}
	r.mu.Unlock()
	r.Recorder.Publish(evts...)
}

func oracleSpec(nodes, plants int) disruptionbench.Spec {
	return disruptionbench.Spec{
		Seed: 1, Nodes: nodes, PodsPerNode: 10, NodePools: 3, Zones: 3, DaemonSets: 4, SpotFraction: 0.3,
		AntiAffinity: 0.1, ZonalTSC: 0.2, HostnameTSC: 0.1, PDBs: true,
		Plants: []disruptionbench.Plant{
			{Kind: disruptionbench.PlantComplementPair, Count: plants},
			{Kind: disruptionbench.PlantSmallMerge, Count: plants},
			{Kind: disruptionbench.PlantHiddenDelete, Count: plants},
		},
	}
}

// TestOracleBaseline replays today's Emptiness, MultiNode and SingleNode consolidation over fixtures with planted
// moves and reports which plants they find. Methods run on a real clock so their timeouts apply; validation is a nop.
//
//	go test -tags test_performance ./pkg/controllers/disruption/ -run TestOracleBaseline -v -timeout 2h
//
// DISRUPTIONBENCH_OUT sets the CSV directory, DISRUPTIONBENCH_SIZES the fixtures (default "200,2000").
func TestOracleBaseline(t *testing.T) {
	out := lo.CoalesceOrEmpty(os.Getenv("DISRUPTIONBENCH_OUT"), os.TempDir())
	sizes := strings.Split(lo.CoalesceOrEmpty(os.Getenv("DISRUPTIONBENCH_SIZES"), "200,2000"), ",")
	for _, size := range sizes {
		var nodes, plants int
		var wall time.Duration
		switch size {
		case "200":
			nodes, plants, wall = 200, 5, 15*time.Minute
		case "2000":
			nodes, plants, wall = 2000, 10, 45*time.Minute
		default:
			t.Fatalf("unknown size %q", size)
		}
		t.Run(fmt.Sprintf("nodes=%d", nodes), func(t *testing.T) {
			ctx := options.ToContext(context.Background(), test.Options())
			f, err := disruptionbench.Generate(oracleSpec(nodes, plants))
			if err != nil {
				t.Fatal(err)
			}
			env, err := disruptionbench.NewEnv(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			rec := &unconsolidatableRecorder{Recorder: env.Recorder, messages: map[string]map[string]int{}}
			c := disruption.MakeConsolidation(realclock.RealClock{}, env.Cluster, env.Client, env.Provisioner, env.CloudProvider, rec, env.Queue)
			methods := []disruption.Method{
				disruption.NewEmptiness(c, disruption.WithValidator(NopValidator{})),
				disruption.NewMultiNodeConsolidation(c, disruption.WithValidator(NopValidator{})),
				disruption.NewSingleNodeConsolidation(c, disruption.WithValidator(NopValidator{})),
			}
			curve, err := disruptionbench.Replay(ctx, env, methods, disruptionbench.ReplayOptions{MaxWall: wall, Logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(out, fmt.Sprintf("oracle-baseline-nodes-%d.csv", nodes))
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := curve.WriteCSV(file); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s", path)
			t.Logf("summary nodes=%d:\n%s", nodes, curve.Summary(0))
			reportPlants(t, curve, rec)
		})
	}
}

// reportPlants says, for each planted move, whether it was captured and which commands touched its nodes. For a
// missed plant it lists the Unconsolidatable messages SingleNode published for its nodes.
func reportPlants(t *testing.T, curve *disruptionbench.Curve, rec *unconsolidatableRecorder) {
	touched := map[string][]string{}
	for _, p := range curve.Points[1:] {
		for _, n := range p.Candidates {
			touched[n] = append(touched[n], fmt.Sprintf("iter %d %s %s %v", p.Iteration, p.Method, p.Decision, p.Candidates))
		}
	}
	for i, pm := range curve.Planted {
		status := "MISSED"
		if curve.CapturedAt[i] >= 0 {
			status = fmt.Sprintf("captured at %s", curve.CapturedAt[i].Round(time.Millisecond))
		}
		t.Logf("plant %-16s $%.4f/hr %v: %s", pm.Kind, pm.SavingsPerHour, pm.NodeNames, status)
		for _, n := range pm.NodeNames {
			for _, cmd := range touched[n] {
				t.Logf("    %s: command %s", n, cmd)
			}
			if len(touched[n]) == 0 {
				msgs := lo.Keys(rec.messages[n])
				sort.Strings(msgs)
				for _, m := range msgs {
					t.Logf("    %s: no command; Unconsolidatable x%d %q", n, rec.messages[n][m], m)
				}
				if len(msgs) == 0 {
					t.Logf("    %s: no command, no Unconsolidatable event", n)
				}
			}
		}
	}
}
