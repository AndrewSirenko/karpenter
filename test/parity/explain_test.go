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

package parity_test

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/test/parity"
)

func compactPod(p *corev1.Pod) string {
	var b strings.Builder
	fmt.Fprintf(&b, "app=%s", p.Labels["app"])
	if len(p.Spec.NodeSelector) > 0 {
		fmt.Fprintf(&b, " sel=%v", p.Spec.NodeSelector)
	}
	if a := p.Spec.Affinity; a != nil {
		if na := a.NodeAffinity; na != nil {
			if r := na.RequiredDuringSchedulingIgnoredDuringExecution; r != nil {
				for i, t := range r.NodeSelectorTerms {
					fmt.Fprintf(&b, " req[%d]=%s", i, exprs(t.MatchExpressions))
				}
			}
			for _, t := range na.PreferredDuringSchedulingIgnoredDuringExecution {
				fmt.Fprintf(&b, " pref(%d)=%s", t.Weight, exprs(t.Preference.MatchExpressions))
			}
		}
		if a.PodAffinity != nil {
			for _, t := range a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
				fmt.Fprintf(&b, " podAff(%v@%s)", t.LabelSelector.MatchLabels, short(t.TopologyKey))
			}
		}
		if a.PodAntiAffinity != nil {
			for _, t := range a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
				fmt.Fprintf(&b, " antiAff(%v@%s)", t.LabelSelector.MatchLabels, short(t.TopologyKey))
			}
		}
	}
	for _, t := range p.Spec.TopologySpreadConstraints {
		fmt.Fprintf(&b, " tsc(%s skew=%d %s sel=%v", short(t.TopologyKey), t.MaxSkew, t.WhenUnsatisfiable, t.LabelSelector.MatchLabels)
		if t.MinDomains != nil {
			fmt.Fprintf(&b, " minDomains=%d", *t.MinDomains)
		}
		if t.NodeAffinityPolicy != nil {
			fmt.Fprintf(&b, " affPolicy=%s", *t.NodeAffinityPolicy)
		}
		if t.NodeTaintsPolicy != nil {
			fmt.Fprintf(&b, " taintPolicy=%s", *t.NodeTaintsPolicy)
		}
		b.WriteString(")")
	}
	for _, t := range p.Spec.Tolerations {
		if strings.HasPrefix(t.Key, "parity") {
			fmt.Fprintf(&b, " tol=%s", t.Value)
		}
	}
	return b.String()
}

func short(k string) string { return k[strings.LastIndex(k, "/")+1:] }

func exprs(es []corev1.NodeSelectorRequirement) string {
	return "{" + strings.Join(lo.Map(es, func(e corev1.NodeSelectorRequirement, _ int) string {
		return fmt.Sprintf("%s %s %v", short(e.Key), e.Operator, e.Values)
	}), ", ") + "}"
}

func compactNode(n *corev1.Node) string {
	keys := []string{"topology.kubernetes.io/zone", "karpenter.sh/capacity-type", "kubernetes.io/arch", "size", "special", "integer", "parity.test/team", "node.kubernetes.io/instance-type", "karpenter.sh/nodepool"}
	return strings.Join(lo.Map(keys, func(k string, _ int) string { return short(k) + "=" + n.Labels[k] }), " ") + fmt.Sprintf(" taints=%d", len(n.Spec.Taints))
}

var _ = Describe("Explain", func() {
	It("explains discrepancies from a report", func() {
		path := os.Getenv("PARITY_EXPLAIN")
		if path == "" {
			Skip("set PARITY_EXPLAIN=<discrepancies.json>")
		}
		raw, err := os.ReadFile(path)
		Expect(err).ToNot(HaveOccurred())
		var ds []parity.Discrepancy
		Expect(json.Unmarshal(raw, &ds)).To(Succeed())
		filter := os.Getenv("PARITY_EXPLAIN_KEY")
		ds = lo.Filter(ds, func(d parity.Discrepancy, _ int) bool { return strings.Contains(d.Key(), filter) })
		sort.Slice(ds, func(i, j int) bool { return ds[i].Seed < ds[j].Seed })
		for _, d := range lo.Slice(ds, 0, envInt("PARITY_EXPLAIN_MAX", 8)) {
			s := parity.Generate(d.Seed, parity.Profiles[d.Profile])
			o := run(s)
			fresh, err := parity.Check(ctx, ks, s, o)
			Expect(err).ToNot(HaveOccurred())
			fmt.Printf("EXP ==== %s\n", d)
			for _, f := range fresh {
				if f.Key() == d.Key() {
					fmt.Printf("EXP   rerun: %s\n", f)
				}
			}
			for _, p := range o.Pods {
				where := "pending"
				if p.Spec.NodeName != "" {
					where = p.Spec.NodeName
				}
				fmt.Printf("EXP   %s -> %s: %s\n", p.Name, where, compactPod(p))
			}
			for _, n := range o.Nodes {
				fmt.Printf("EXP   node %s new=%t %s\n", n.Name, o.NewNodes[n.Name], compactNode(n))
			}
		}
	})
})
