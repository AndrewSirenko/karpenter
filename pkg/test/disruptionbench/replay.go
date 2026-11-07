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

package disruptionbench

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
)

// ReplayController is the controller name Replay runs under. Scheduling simulations are counted by this label.
const ReplayController = "disruptionbench"

type ReplayOptions struct {
	// MaxIterations caps applied commands. 0 means 1000.
	MaxIterations int
	// MaxWall caps wall time. 0 means 30 minutes.
	MaxWall time.Duration
	// MaxSimSeconds caps cumulative scheduling simulation time. 0 means no cap.
	MaxSimSeconds float64
	// Logf, when set, gets one line per command.
	Logf func(format string, args ...any)
}

// Point is the cluster after one applied command. Points[0] is the starting cluster.
type Point struct {
	Iteration         int
	Elapsed           time.Duration
	Sims              int
	SimSeconds        float64
	CostPerHour       float64
	Savings           float64
	PlantedCaptured   float64
	PlantsCaptured    int
	PodsEvicted       int
	NodeClaimsCreated int
	Method            string
	ConsolidationType string
	Decision          string
	CommandSavings    float64
	Candidates        []string
	Replacements      []string
}

type Curve struct {
	Planted      []PlantedMove
	PlantedTotal float64
	Points       []Point
	// CapturedAt is when each planted move's nodes were all gone, aligned with Planted, or -1.
	CapturedAt []time.Duration
	StopReason string
	// TimedOutPasses counts passes that found no command because a method timed out.
	TimedOutPasses int
	// Elapsed, Sims and SimSeconds cover the whole run, including passes after the last command.
	Elapsed    time.Duration
	Sims       int
	SimSeconds float64
}

// Replay runs methods against env the way the disruption controller does, first command wins, and applies each
// command to the in-memory cluster until no method returns one or a cap is hit.
func Replay(ctx context.Context, env *Env, methods []disruption.Method, opts ReplayOptions) (*Curve, error) {
	opts.MaxIterations = lo.Ternary(opts.MaxIterations == 0, 1000, opts.MaxIterations)
	opts.MaxWall = lo.Ternary(opts.MaxWall == 0, 30*time.Minute, opts.MaxWall)
	ctx = injection.WithControllerName(ctx, ReplayController)
	r := &replayer{env: env, simsBase: 0}
	r.simsBase, r.simSecondsBase = SimulationCount()
	curve := &Curve{Planted: env.Fixture.Planted, CapturedAt: lo.Times(len(env.Fixture.Planted), func(int) time.Duration { return -1 })}
	curve.PlantedTotal = lo.SumBy(curve.Planted, func(p PlantedMove) float64 { return p.SavingsPerHour })
	start := time.Now()
	initial, err := r.cost(ctx)
	if err != nil {
		return nil, err
	}
	curve.Points = append(curve.Points, Point{CostPerHour: initial})
	for iter := 1; ; iter++ {
		sims, simSeconds := r.sims()
		switch {
		case iter > opts.MaxIterations:
			curve.StopReason = "iteration cap"
		case time.Since(start) > opts.MaxWall:
			curve.StopReason = "wall cap"
		case opts.MaxSimSeconds > 0 && simSeconds > opts.MaxSimSeconds:
			curve.StopReason = "sim-time cap"
		}
		if curve.StopReason != "" {
			break
		}
		timeouts := consolidationTimeouts()
		m, cmds, err := r.compute(ctx, methods)
		if err != nil {
			return curve, err
		}
		if len(cmds) == 0 {
			// A method that timed out hasn't seen every candidate, so the controller would try again.
			if consolidationTimeouts() > timeouts {
				curve.TimedOutPasses++
				iter--
				if opts.Logf != nil {
					opts.Logf("pass timed out without a command, t=%s", time.Since(start).Round(time.Millisecond))
				}
				continue
			}
			curve.StopReason = "no command"
			break
		}
		p := Point{Iteration: iter, Method: fmt.Sprintf("%T", m), ConsolidationType: m.ConsolidationType()}
		p.Method = strings.TrimPrefix(p.Method, "*disruption.")
		for _, cmd := range cmds {
			p.Decision = string(cmd.Decision())
			p.Candidates = append(p.Candidates, cmd.SourceNodeNames()...)
			evicted, created, err := r.apply(ctx, cmd)
			if err != nil {
				return curve, fmt.Errorf("applying %s, %w", cmd.String(), err)
			}
			p.Replacements = append(p.Replacements, created...)
			p.PodsEvicted += evicted
			p.NodeClaimsCreated += len(created)
		}
		cost, err := r.cost(ctx)
		if err != nil {
			return curve, err
		}
		prev := curve.Points[len(curve.Points)-1]
		sims, simSeconds = r.sims()
		p.Elapsed, p.Sims, p.SimSeconds = time.Since(start), sims, simSeconds
		p.CostPerHour, p.CommandSavings, p.Savings = cost, prev.CostPerHour-cost, initial-cost
		p.PodsEvicted += prev.PodsEvicted
		p.NodeClaimsCreated += prev.NodeClaimsCreated
		nodes, err := r.nodeNames(ctx)
		if err != nil {
			return curve, err
		}
		for i, pm := range curve.Planted {
			if curve.CapturedAt[i] < 0 && lo.NoneBy(pm.NodeNames, func(n string) bool { return nodes[n] }) {
				curve.CapturedAt[i] = p.Elapsed
			}
			if curve.CapturedAt[i] >= 0 {
				p.PlantedCaptured += pm.SavingsPerHour
				p.PlantsCaptured++
			}
		}
		curve.Points = append(curve.Points, p)
		if opts.Logf != nil {
			opts.Logf("iter=%d t=%s sims=%d %s/%s %s %v -> %v saved=$%.4f/hr planted=%d/%d",
				iter, p.Elapsed.Round(time.Millisecond), p.Sims, p.Method, p.ConsolidationType, p.Decision, p.Candidates, p.Replacements,
				p.CommandSavings, p.PlantsCaptured, len(curve.Planted))
		}
	}
	curve.Elapsed = time.Since(start)
	curve.Sims, curve.SimSeconds = r.sims()
	return curve, nil
}

// SimulationCount reads the count and summed duration of scheduling simulations run under ReplayController.
func SimulationCount() (int, float64) {
	families, err := crmetrics.Registry.Gather()
	if err != nil {
		return 0, 0
	}
	for _, f := range families {
		if f.GetName() != "karpenter_scheduler_scheduling_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			if lo.ContainsBy(m.GetLabel(), func(l *dto.LabelPair) bool {
				return l.GetName() == "controller" && l.GetValue() == ReplayController
			}) {
				return int(m.GetHistogram().GetSampleCount()), m.GetHistogram().GetSampleSum()
			}
		}
	}
	return 0, 0
}

func consolidationTimeouts() float64 {
	families, err := crmetrics.Registry.Gather()
	if err != nil {
		return 0
	}
	total := 0.0
	for _, f := range families {
		if f.GetName() == "karpenter_voluntary_disruption_consolidation_timeouts_total" {
			total += lo.SumBy(f.GetMetric(), func(m *dto.Metric) float64 { return m.GetCounter().GetValue() })
		}
	}
	return total
}

type replayer struct {
	env            *Env
	simsBase       int
	simSecondsBase float64
	seq            int
}

func (r *replayer) sims() (int, float64) {
	n, s := SimulationCount()
	return n - r.simsBase, s - r.simSecondsBase
}

// compute mirrors Controller.Reconcile and Controller.disrupt without starting commands.
func (r *replayer) compute(ctx context.Context, methods []disruption.Method) (disruption.Method, []disruption.Command, error) {
	e := r.env
	for _, m := range methods {
		candidates, totals, err := disruption.GetCandidatesWithTotals(ctx, e.Cluster, e.Client, e.Recorder, e.Clock, e.CloudProvider, m.ShouldDisrupt, m.Class(), e.Queue, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("determining candidates, %w", err)
		}
		if len(candidates) == 0 {
			continue
		}
		if setter, ok := m.(disruption.NodePoolTotalsSetter); ok {
			setter.SetNodePoolTotals(totals)
		}
		budgets, err := disruption.BuildDisruptionBudgetMapping(ctx, e.Cluster, e.Clock, e.Client, e.CloudProvider, e.Recorder, m.Reason())
		if err != nil {
			return nil, nil, fmt.Errorf("building disruption budgets, %w", err)
		}
		cmds, err := m.ComputeCommands(ctx, budgets, candidates...)
		if err != nil {
			return nil, nil, fmt.Errorf("computing commands, %w", err)
		}
		cmds = lo.Filter(cmds, func(c disruption.Command, _ int) bool { return c.Decision() != disruption.NoOpDecision })
		if len(cmds) > 0 {
			return m, cmds, nil
		}
	}
	return nil, nil, nil
}

// apply launches each replacement on its cheapest launchable offering, rebinds evicted pods where the simulation
// placed them, and deletes the candidates.
func (r *replayer) apply(ctx context.Context, cmd disruption.Command) (int, []string, error) {
	target := map[types.NamespacedName]string{}
	var created []string
	for _, rep := range cmd.Replacements {
		name, err := r.launch(ctx, rep.NodeClaim)
		if err != nil {
			return 0, nil, err
		}
		created = append(created, name)
		for _, p := range rep.Pods {
			target[client.ObjectKeyFromObject(p)] = name
		}
	}
	for _, n := range cmd.Results.ExistingNodes {
		for _, p := range n.Pods {
			target[client.ObjectKeyFromObject(p)] = n.Name()
		}
	}
	evicted := 0
	for _, c := range cmd.Candidates {
		pods := &corev1.PodList{}
		if err := r.env.Client.List(ctx, pods, client.MatchingFields{"spec.nodeName": c.Name()}); err != nil {
			return 0, nil, err
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			key := client.ObjectKeyFromObject(p)
			if err := r.env.Client.Delete(ctx, p); err != nil {
				return 0, nil, err
			}
			r.env.Cluster.DeletePod(key)
			if !podutils.IsReschedulable(p) {
				continue
			}
			node, ok := target[key]
			if !ok {
				return 0, nil, fmt.Errorf("simulation didn't place pod %s", key)
			}
			if err := r.bind(ctx, p, node); err != nil {
				return 0, nil, err
			}
			evicted++
		}
		if err := r.env.Client.Delete(ctx, c.Node); err != nil {
			return 0, nil, err
		}
		if err := r.env.Client.Delete(ctx, c.NodeClaim); err != nil {
			return 0, nil, err
		}
		r.env.Cluster.DeleteNode(c.Node.Name)
		r.env.Cluster.DeleteNodeClaim(c.NodeClaim.Name)
	}
	return evicted, created, nil
}

func (r *replayer) bind(ctx context.Context, p *corev1.Pod, node string) error {
	moved := p.DeepCopy()
	moved.ResourceVersion = ""
	moved.UID = types.UID(fmt.Sprintf("%s-r%d", p.UID, r.seq))
	r.seq++
	moved.Spec.NodeName = node
	if err := r.env.Client.Create(ctx, moved); err != nil {
		return err
	}
	return r.env.Cluster.UpdatePod(ctx, moved)
}

func (r *replayer) launch(ctx context.Context, nc *pscheduling.NodeClaim) (string, error) {
	var best *cloudprovider.InstanceType
	var offering *cloudprovider.Offering
	for _, it := range nc.InstanceTypeOptions {
		for _, o := range it.Offerings.Launchable().Compatible(nc.Requirements) {
			if offering == nil || o.Price < offering.Price {
				best, offering = it, o
			}
		}
	}
	if offering == nil {
		return "", fmt.Errorf("no launchable offering for replacement")
	}
	claim := nc.ToNodeClaim()
	name := fmt.Sprintf("replay-%05d", r.seq)
	r.seq++
	pool := claim.Labels[v1.NodePoolLabelKey]
	v1nc, node := RenderNode(name, pool, offering.Zone(), offering.CapacityType(), claim.Labels[GroupLabelKey], best, claim.Spec.NodeClassRef)
	if err := r.env.Client.Create(ctx, v1nc); err != nil {
		return "", err
	}
	if err := r.env.Client.Create(ctx, node); err != nil {
		return "", err
	}
	r.env.Cluster.UpdateNodeClaim(v1nc)
	if err := r.env.Cluster.UpdateNode(ctx, node); err != nil {
		return "", err
	}
	for _, ds := range r.env.Fixture.DaemonSets {
		p := DaemonSetPod(ds, name)
		if err := r.env.Client.Create(ctx, p); err != nil {
			return "", err
		}
		if err := r.env.Cluster.UpdatePod(ctx, p); err != nil {
			return "", err
		}
	}
	return name, nil
}

func (r *replayer) nodeNames(ctx context.Context) (map[string]bool, error) {
	nodes := &corev1.NodeList{}
	if err := r.env.Client.List(ctx, nodes); err != nil {
		return nil, err
	}
	return lo.SliceToMap(nodes.Items, func(n corev1.Node) (string, bool) { return n.Name, true }), nil
}

// cost sums the list price of every NodeClaim's offering.
func (r *replayer) cost(ctx context.Context) (float64, error) {
	ncs := &v1.NodeClaimList{}
	if err := r.env.Client.List(ctx, ncs); err != nil {
		return 0, err
	}
	its := lo.KeyBy(r.env.Fixture.InstanceTypes, func(it *cloudprovider.InstanceType) string { return it.Name })
	total := 0.0
	for i := range ncs.Items {
		nc := &ncs.Items[i]
		it, ok := its[nc.Labels[corev1.LabelInstanceTypeStable]]
		if !ok {
			return 0, fmt.Errorf("unknown instance type on %s", nc.Name)
		}
		p, ok := it.OfferingPrice(nc.Labels[corev1.LabelTopologyZone], nc.Labels[v1.CapacityTypeLabelKey])
		if !ok {
			return 0, fmt.Errorf("no offering for %s", nc.Name)
		}
		total += p
	}
	return total, nil
}

// Summary describes a curve at a point in time.
type Summary struct {
	At               time.Duration
	PlantedTotal     float64
	PlantedCaptured  float64
	MissedPlanted    float64
	TotalSavings     float64
	TimeTo90         time.Duration
	Reached90        bool
	Commands         int
	Sims             int
	Elapsed          time.Duration
	StopReason       string
	TimedOutPasses   int
	CapturedByKind   map[PlantKind]int
	PlantedByKind    map[PlantKind]int
	CommandsByMethod map[string]int
}

// Summary reports the curve at time at. A zero at means the end of the run.
func (c *Curve) Summary(at time.Duration) Summary {
	last := c.Points[len(c.Points)-1]
	if at == 0 {
		at = max(last.Elapsed, c.Elapsed)
	}
	s := Summary{At: at, PlantedTotal: c.PlantedTotal, StopReason: c.StopReason, TimedOutPasses: c.TimedOutPasses, Elapsed: c.Elapsed, Sims: c.Sims,
		CapturedByKind: map[PlantKind]int{}, PlantedByKind: map[PlantKind]int{}, CommandsByMethod: map[string]int{}}
	for _, p := range c.Points[1:] {
		if p.Elapsed <= at {
			s.PlantedCaptured, s.TotalSavings = p.PlantedCaptured, p.Savings
			s.Commands++
			s.CommandsByMethod[p.Method+"/"+p.Decision]++
		}
		if !s.Reached90 && c.PlantedTotal > 0 && p.PlantedCaptured >= 0.9*c.PlantedTotal-1e-9 {
			s.TimeTo90, s.Reached90 = p.Elapsed, true
		}
	}
	s.MissedPlanted = c.PlantedTotal - s.PlantedCaptured
	for i, pm := range c.Planted {
		s.PlantedByKind[pm.Kind]++
		if c.CapturedAt[i] >= 0 && c.CapturedAt[i] <= at {
			s.CapturedByKind[pm.Kind]++
		}
	}
	return s
}

func (s Summary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "elapsed=%s sims=%d commands=%d timed-out passes=%d stop=%q\n", s.Elapsed.Round(time.Millisecond), s.Sims, s.Commands, s.TimedOutPasses, s.StopReason)
	pct := lo.Ternary(s.PlantedTotal > 0, 100*s.PlantedCaptured/s.PlantedTotal, math.NaN())
	fmt.Fprintf(&b, "planted $%.4f/hr, captured $%.4f/hr (%.1f%%), missed at %s $%.4f/hr, total savings $%.4f/hr\n",
		s.PlantedTotal, s.PlantedCaptured, pct, s.At.Round(time.Millisecond), s.MissedPlanted, s.TotalSavings)
	if s.Reached90 {
		fmt.Fprintf(&b, "time to 90%% planted: %s\n", s.TimeTo90.Round(time.Millisecond))
	} else {
		fmt.Fprintf(&b, "time to 90%% planted: never\n")
	}
	for _, k := range []PlantKind{PlantComplementPair, PlantSmallMerge, PlantHiddenDelete} {
		if s.PlantedByKind[k] > 0 {
			fmt.Fprintf(&b, "  %-16s captured %d/%d\n", k, s.CapturedByKind[k], s.PlantedByKind[k])
		}
	}
	for _, k := range lo.Keys(s.CommandsByMethod) {
		fmt.Fprintf(&b, "  commands %s: %d\n", k, s.CommandsByMethod[k])
	}
	return b.String()
}

// WriteCSV writes one row per point.
func (c *Curve) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"iteration", "elapsed_seconds", "sims", "sim_seconds", "cost_per_hour", "savings_per_hour", "planted_total_per_hour",
		"planted_captured_per_hour", "plants_captured", "pods_evicted", "nodeclaims_created", "method", "consolidation_type", "decision",
		"command_savings_per_hour", "candidates", "replacements"})
	for _, p := range c.Points {
		_ = cw.Write([]string{
			fmt.Sprint(p.Iteration), fmt.Sprintf("%.3f", p.Elapsed.Seconds()), fmt.Sprint(p.Sims), fmt.Sprintf("%.3f", p.SimSeconds),
			fmt.Sprintf("%.4f", p.CostPerHour), fmt.Sprintf("%.4f", p.Savings), fmt.Sprintf("%.4f", c.PlantedTotal),
			fmt.Sprintf("%.4f", p.PlantedCaptured), fmt.Sprint(p.PlantsCaptured), fmt.Sprint(p.PodsEvicted), fmt.Sprint(p.NodeClaimsCreated),
			p.Method, p.ConsolidationType, p.Decision, fmt.Sprintf("%.4f", p.CommandSavings), strings.Join(p.Candidates, " "), strings.Join(p.Replacements, " "),
		})
	}
	cw.Flush()
	return cw.Error()
}
