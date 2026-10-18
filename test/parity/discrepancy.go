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

package parity

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Class is the kind of disagreement between Karpenter and kube-scheduler.
type Class string

const (
	// LaunchedUnusable: Karpenter placed the pod on a node that kube-scheduler rejects.
	LaunchedUnusable Class = "A-placed-on-rejected-node"
	// OrderDependent: every bind order tried strands some pod Karpenter placed, but no single pod in all of them.
	OrderDependent Class = "A2-no-bind-order-found"
	// MissedExisting: Karpenter launched a node for the pod while an existing node passes every filter.
	MissedExisting Class = "B-missed-existing-capacity"
	// FalseUnschedulable: Karpenter left the pod pending while some node passes every filter.
	FalseUnschedulable Class = "C-false-unschedulable"
)

// Discrepancy is one disagreement for one pod.
type Discrepancy struct {
	Class   Class
	Plugin  string
	Reason  string
	Detail  string
	Pod     string
	Node    string
	NewNode bool
	Seed    int64
	Profile string
}

var digits = regexp.MustCompile(`\d+`)

var trace = os.Getenv("PARITY_TRACE") != ""

// Key groups discrepancies with the same shape.
func (d Discrepancy) Key() string {
	return fmt.Sprintf("%s|%s|%s", d.Class, d.Plugin, digits.ReplaceAllString(d.Reason, "N"))
}

func (d Discrepancy) String() string {
	return fmt.Sprintf("[%s] seed=%d/%s pod=%s node=%s (new=%t) %s: %s %s", d.Class, d.Seed, d.Profile, d.Pod, d.Node, d.NewNode, d.Plugin, d.Reason, d.Detail)
}

// Outcome is the cluster after Karpenter provisioned and bound pods.
type Outcome struct {
	Nodes           []*corev1.Node
	NewNodes        map[string]bool
	Pods            []*corev1.Pod
	Pending         []string
	KarpenterErrors map[string]string
}

// Check compares Karpenter's placements in o with kube-scheduler's Filter plugins.
//
// kube-scheduler binds pods one at a time, so a final state can be fine even if some pod's spread or affinity
// constraint doesn't hold for it in hindsight. Class A therefore asks whether there is an order in which
// kube-scheduler accepts every placement Karpenter made: pods are bound greedily in rounds, in several orders.
func Check(ctx context.Context, ks *KubeScheduler, s *Scenario, o *Outcome) ([]Discrepancy, error) {
	var out []Discrepancy
	pending := lo.SliceToMap(o.Pending, func(n string) (string, bool) { return n, true })
	prebound := lo.Filter(o.Pods, func(p *corev1.Pod, _ int) bool { return !pending[p.Name] && p.Spec.NodeName != "" })
	placed := lo.Filter(o.Pods, func(p *corev1.Pod, _ int) bool { return pending[p.Name] && p.Spec.NodeName != "" })

	// Try several orders. A pod is reported only if it can't be bound in any of them; if every order strands some
	// pod but no pod is stranded in all of them, the placements are order dependent.
	// A pod whose required pod affinity matches itself can only be the first of its group, so try those first.
	selfAffinityFirst := append([]*corev1.Pod{}, placed...)
	sort.SliceStable(selfAffinityFirst, func(i, j int) bool { return selfAffine(selfAffinityFirst[i]) && !selfAffine(selfAffinityFirst[j]) })
	orders := [][]*corev1.Pod{placed, lo.Reverse(append([]*corev1.Pod{}, placed...)), selfAffinityFirst}
	r := rand.New(rand.NewSource(s.Seed))
	for range 6 {
		shuffled := append([]*corev1.Pod{}, placed...)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		orders = append(orders, shuffled)
	}
	var always map[string]*Rejection
	var firstStuck map[string]*Rejection
	for _, order := range orders {
		left, err := bindInOrder(ctx, ks, o.Nodes, prebound, order)
		if err != nil {
			return nil, err
		}
		if len(left) == 0 {
			always, firstStuck = nil, nil
			break
		}
		if firstStuck == nil {
			firstStuck, always = left, left
			continue
		}
		always = lo.PickBy(always, func(name string, _ *Rejection) bool { return left[name] != nil })
	}
	stuck := map[string]*Rejection{}
	for _, p := range placed {
		if rej, ok := always[p.Name]; ok {
			stuck[p.Name] = rej
			d := Discrepancy{Class: LaunchedUnusable, Plugin: rej.Plugin, Reason: rej.Reason, Pod: p.Name, Node: p.Spec.NodeName, NewNode: o.NewNodes[p.Spec.NodeName], Seed: s.Seed, Profile: s.Profile}
			if rej.Plugin == "NodeAffinity" {
				colocated := lo.Filter(placed, func(x *corev1.Pod, _ int) bool { return x.Spec.NodeName == p.Spec.NodeName })
				if keys := ConflictingKeys(colocated...); len(keys) > 0 {
					d.Reason += " [conflicting requirements on one key]"
					d.Detail = fmt.Sprintf("keys=%v", keys)
				}
			}
			out = append(out, d)
		}
	}
	// A pod whose affinity partner on the same node is itself stuck fails because of that partner.
	for i := range out {
		d := &out[i]
		if d.Class != LaunchedUnusable || d.Plugin != "InterPodAffinity" || !strings.Contains(d.Reason, "pod affinity rules") {
			continue
		}
		p, _ := lo.Find(placed, func(x *corev1.Pod) bool { return x.Name == d.Pod })
		partners := lo.Filter(placed, func(x *corev1.Pod, _ int) bool {
			return x.Name != p.Name && x.Spec.NodeName == p.Spec.NodeName && always[x.Name] != nil && affinityMatches(p, x)
		})
		if len(partners) > 0 {
			d.Reason += " [partner pod also rejected]"
			d.Detail = fmt.Sprintf("partner=%s (%s)", partners[0].Name, always[partners[0].Name].Plugin)
		}
	}
	if len(always) == 0 {
		for _, p := range placed {
			if rej, ok := firstStuck[p.Name]; ok {
				stuck[p.Name] = rej
				out = append(out, Discrepancy{Class: OrderDependent, Plugin: rej.Plugin, Reason: rej.Reason, Pod: p.Name, Node: p.Spec.NodeName, NewNode: o.NewNodes[p.Spec.NodeName], Seed: s.Seed, Profile: s.Profile})
			}
		}
	}

	// B and C ask whether a node Karpenter didn't use would accept the pod in the final state.
	without := func(p *corev1.Pod) []*corev1.Pod {
		return lo.Filter(o.Pods, func(x *corev1.Pod, _ int) bool { return x.Name != p.Name && x.Spec.NodeName != "" })
	}
	for _, p := range o.Pods {
		if !pending[p.Name] || stuck[p.Name] != nil {
			continue
		}
		if p.Spec.NodeName != "" && !o.NewNodes[p.Spec.NodeName] {
			continue
		}
		for _, n := range o.Nodes {
			if p.Spec.NodeName != "" && o.NewNodes[n.Name] {
				continue
			}
			rej, err := ks.Filter(ctx, p, n.Name, o.Nodes, without(p))
			if err != nil {
				return nil, err
			}
			if rej != nil {
				continue
			}
			// Karpenter treats preferences as requirements until it relaxes them, so a node that only fails a
			// preference is a different finding from one that satisfies everything.
			hardRej, err := ks.Filter(ctx, Harden(p), n.Name, o.Nodes, without(p))
			if err != nil {
				return nil, err
			}
			plugin, reason := "-", "node passes all filters and preferences"
			if hardRej != nil {
				plugin, reason = hardRej.Plugin, "node fails only a preference: "+hardRej.Reason
			}
			// With spread or pod affinity, a node that fits in the final state may not have fit when Karpenter
			// placed the pod, so these are weaker findings.
			if HasTopologyConstraints(p) {
				reason += " [topology-dependent]"
			}
			if p.Spec.NodeName != "" {
				out = append(out, Discrepancy{Class: MissedExisting, Plugin: plugin, Reason: reason, Pod: p.Name, Node: n.Name, Seed: s.Seed, Profile: s.Profile})
			} else {
				out = append(out, Discrepancy{Class: FalseUnschedulable, Plugin: plugin, Reason: reason + "; karpenter: " + karpenterReason(o.KarpenterErrors[p.Name]), Pod: p.Name, Node: n.Name, NewNode: o.NewNodes[n.Name], Seed: s.Seed, Profile: s.Profile})
			}
			break
		}
	}
	return out, nil
}

// bindInOrder binds pods to their nodes in rounds until no more can be bound, and returns the ones left.
func bindInOrder(ctx context.Context, ks *KubeScheduler, nodes []*corev1.Node, bound []*corev1.Pod, order []*corev1.Pod) (map[string]*Rejection, error) {
	bound = append([]*corev1.Pod{}, bound...)
	left := map[string]*Rejection{}
	remaining := order
	for progress := true; progress && len(remaining) > 0; {
		progress = false
		var next []*corev1.Pod
		for _, p := range remaining {
			rej, err := ks.Filter(ctx, p, p.Spec.NodeName, nodes, bound)
			if err != nil {
				return nil, err
			}
			if trace {
				fmt.Printf("TRACE bind %s -> %s: %v\n", p.Name, p.Spec.NodeName, rej)
			}
			if rej != nil {
				left[p.Name] = rej
				next = append(next, p)
				continue
			}
			delete(left, p.Name)
			bound = append(bound, p)
			progress = true
		}
		remaining = next
	}
	return left, nil
}

var karpenterReasonDetail = regexp.MustCompile(`, requirements=.*$|\(.*$|, key=.*$|^key \S+, `)

// karpenterReason trims a Karpenter scheduling error to its leading clauses so similar failures group together.
func karpenterReason(err string) string {
	if err == "" {
		return "no error recorded"
	}
	parts := lo.Uniq(lo.Map(strings.Split(err, "; "), func(part string, _ int) string {
		part = strings.TrimSpace(karpenterReasonDetail.ReplaceAllString(part, ""))
		// Requirement mismatches name the key and values; keep only the kind of failure.
		if strings.Contains(part, " not in ") || strings.HasPrefix(part, "incompatible requirements") {
			return "incompatible requirements"
		}
		return part
	}))
	sort.Strings(parts)
	return strings.Join(parts, " / ")
}

// Harden turns a pod's preferences into requirements, the way Karpenter treats them before relaxing.
func Harden(p *corev1.Pod) *corev1.Pod {
	p = p.DeepCopy()
	for i := range p.Spec.TopologySpreadConstraints {
		p.Spec.TopologySpreadConstraints[i].WhenUnsatisfiable = corev1.DoNotSchedule
	}
	a := p.Spec.Affinity
	if a == nil {
		return p
	}
	if na := a.NodeAffinity; na != nil && len(na.PreferredDuringSchedulingIgnoredDuringExecution) > 0 {
		var preferred []corev1.NodeSelectorRequirement
		for _, t := range na.PreferredDuringSchedulingIgnoredDuringExecution {
			preferred = append(preferred, t.Preference.MatchExpressions...)
		}
		if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{}}}
		}
		for i := range na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			t := &na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[i]
			t.MatchExpressions = append(t.MatchExpressions, preferred...)
		}
		na.PreferredDuringSchedulingIgnoredDuringExecution = nil
	}
	if pa := a.PodAffinity; pa != nil {
		for _, t := range pa.PreferredDuringSchedulingIgnoredDuringExecution {
			pa.RequiredDuringSchedulingIgnoredDuringExecution = append(pa.RequiredDuringSchedulingIgnoredDuringExecution, t.PodAffinityTerm)
		}
		pa.PreferredDuringSchedulingIgnoredDuringExecution = nil
	}
	if paa := a.PodAntiAffinity; paa != nil {
		for _, t := range paa.PreferredDuringSchedulingIgnoredDuringExecution {
			paa.RequiredDuringSchedulingIgnoredDuringExecution = append(paa.RequiredDuringSchedulingIgnoredDuringExecution, t.PodAffinityTerm)
		}
		paa.PreferredDuringSchedulingIgnoredDuringExecution = nil
	}
	return p
}

// ConflictingKeys returns label keys that the pods' node selectors, required terms and preferences constrain both
// to exist (In, Exists, Gt, Lt) and to possibly be absent (NotIn, DoesNotExist). Karpenter intersects these into a
// set that also admits nodes without the label (#3423, #3425).
func ConflictingKeys(pods ...*corev1.Pod) []string {
	exists, absent := map[string]bool{}, map[string]bool{}
	add := func(es []corev1.NodeSelectorRequirement) {
		for _, e := range es {
			switch e.Operator {
			case corev1.NodeSelectorOpNotIn, corev1.NodeSelectorOpDoesNotExist:
				absent[e.Key] = true
			default:
				exists[e.Key] = true
			}
		}
	}
	for _, p := range pods {
		for k := range p.Spec.NodeSelector {
			exists[k] = true
		}
		if a := p.Spec.Affinity; a != nil && a.NodeAffinity != nil {
			if r := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution; r != nil {
				for _, t := range r.NodeSelectorTerms {
					add(t.MatchExpressions)
				}
			}
			for _, t := range a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
				add(t.Preference.MatchExpressions)
			}
		}
	}
	keys := lo.Filter(lo.Keys(exists), func(k string, _ int) bool { return absent[k] })
	sort.Strings(keys)
	return keys
}

// HasTopologyConstraints reports whether the pod's placement depends on where other pods land.
func HasTopologyConstraints(p *corev1.Pod) bool {
	a := p.Spec.Affinity
	return len(p.Spec.TopologySpreadConstraints) > 0 || (a != nil && (a.PodAffinity != nil || a.PodAntiAffinity != nil))
}

func affinityTerms(p *corev1.Pod) []corev1.PodAffinityTerm {
	if p.Spec.Affinity == nil || p.Spec.Affinity.PodAffinity == nil {
		return nil
	}
	return p.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
}

// affinityMatches reports whether target satisfies one of p's required pod affinity selectors.
func affinityMatches(p, target *corev1.Pod) bool {
	return lo.ContainsBy(affinityTerms(p), func(t corev1.PodAffinityTerm) bool {
		sel, err := metav1.LabelSelectorAsSelector(t.LabelSelector)
		return err == nil && sel.Matches(labels.Set(target.Labels))
	})
}

func selfAffine(p *corev1.Pod) bool { return affinityMatches(p, p) }
