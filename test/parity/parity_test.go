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
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/metrics"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/test/parity"
)

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envStr(key, def string) string {
	return lo.CoalesceOrEmpty(os.Getenv(key), def)
}

// run provisions s with Karpenter against envtest and returns the resulting cluster.
func run(s *parity.Scenario) *parity.Outcome {
	GinkgoHelper()
	s = s.DeepCopy()
	ExpectCleanedUp(ctx, env.Client)
	cluster.Reset()
	cloudProvider.Reset()
	cloudProvider.InstanceTypes = parity.InstanceTypes()
	scheduling.MaxInstanceTypes = 60

	for _, np := range s.NodePools {
		ExpectApplied(ctx, env.Client, np)
	}
	var nodes []*corev1.Node
	var nodeClaims []*v1.NodeClaim
	for _, e := range s.ExistingNodes {
		ExpectApplied(ctx, env.Client, e.NodeClaim, e.Node)
		nodes = append(nodes, e.Node)
		nodeClaims = append(nodeClaims, e.NodeClaim)
	}
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
	for _, p := range s.BoundPods {
		ExpectApplied(ctx, env.Client, p)
		ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(p))
	}
	for _, p := range s.PendingPods {
		ExpectApplied(ctx, env.Client, p)
	}
	// Karpenter provisions in a loop. Later rounds see the nodes earlier rounds launched as initialized capacity.
	karpenterErrors := map[string]string{}
	for range envInt("PARITY_ROUNDS", 3) {
		karpenterErrors = map[string]string{}
		results, err := prov.Schedule(ctx)
		Expect(err).ToNot(HaveOccurred())
		for p, err := range results.PodErrors {
			karpenterErrors[p.Name] = err.Error()
		}
		var launchedNodes []*corev1.Node
		var launchedNodeClaims []*v1.NodeClaim
		bound := 0
		for _, nc := range results.NewNodeClaims {
			name, err := prov.Create(ctx, nc, provisioning.WithReason(metrics.ProvisionedReason))
			Expect(err).ToNot(HaveOccurred())
			nodeClaim := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: name}, nodeClaim)).To(Succeed())
			nodeClaim, node := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, nodeClaim)
			Expect(node).ToNot(BeNil())
			launchedNodes = append(launchedNodes, node)
			launchedNodeClaims = append(launchedNodeClaims, nodeClaim)
			for _, p := range nc.Pods {
				ExpectManualBinding(ctx, env.Client, p, node)
				Expect(cluster.UpdatePod(ctx, p)).To(Succeed())
				bound++
			}
		}
		for _, en := range results.ExistingNodes {
			for _, p := range en.Pods {
				ExpectManualBinding(ctx, env.Client, p, en.Node)
				Expect(cluster.UpdatePod(ctx, p)).To(Succeed())
				bound++
			}
		}
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, launchedNodes, launchedNodeClaims)
		if bound == 0 || len(results.PodErrors) == 0 {
			break
		}
	}

	existing := lo.SliceToMap(s.ExistingNodes, func(e parity.ExistingNode) (string, bool) { return e.Node.Name, true })
	startupTaints := map[string][]corev1.Taint{}
	for _, nc := range ExpectNodeClaims(ctx, env.Client) {
		startupTaints[nc.Status.ProviderID] = nc.Spec.StartupTaints
	}
	o := &parity.Outcome{NewNodes: map[string]bool{}, KarpenterErrors: karpenterErrors, Pending: lo.Map(s.PendingPods, func(p *corev1.Pod, _ int) string { return p.Name })}
	for _, n := range ExpectNodes(ctx, env.Client) {
		n = n.DeepCopy()
		// Kubelet sets the hostname label. Startup, registration and not-ready taints are gone once the node is initialized.
		n.Labels = lo.Assign(map[string]string{corev1.LabelHostname: n.Name}, n.Labels)
		n.Spec.Taints = lo.Reject(n.Spec.Taints, func(t corev1.Taint, _ int) bool {
			return t.MatchTaint(&v1.UnregisteredNoExecuteTaint) || t.Key == corev1.TaintNodeNotReady || t.Key == corev1.TaintNodeUnreachable || lo.ContainsBy(startupTaints[n.Spec.ProviderID], func(st corev1.Taint) bool { return st.MatchTaint(&t) })
		})
		o.Nodes = append(o.Nodes, n)
		o.NewNodes[n.Name] = !existing[n.Name]
	}
	podList := &corev1.PodList{}
	Expect(env.Client.List(ctx, podList, client.InNamespace(parity.Namespace))).To(Succeed())
	o.Pods = lo.Map(podList.Items, func(p corev1.Pod, _ int) *corev1.Pod { return p.DeepCopy() })
	return o
}

// allowedCounts tallies allowlisted discrepancies by key so the report still shows them.
var allowedCounts = map[string]int{}

func check(s *parity.Scenario) []parity.Discrepancy {
	GinkgoHelper()
	ds, err := parity.Check(ctx, ks, s, run(s))
	Expect(err).ToNot(HaveOccurred())
	return lo.Reject(ds, func(d parity.Discrepancy, _ int) bool {
		_, ok := parity.Allowed(d)
		if ok {
			allowedCounts[d.Key()]++
		}
		return ok
	})
}

// shrink removes pods, nodes and constraints from s while a discrepancy with key still reproduces.
func shrink(s *parity.Scenario, key string) *parity.Scenario {
	GinkgoHelper()
	budget := envInt("PARITY_SHRINK_BUDGET", 120)
	// Karpenter's choices aren't fully deterministic, so a candidate counts if any of a few runs reproduces.
	reproduces := func(c *parity.Scenario) bool {
		for range envInt("PARITY_SHRINK_TRIES", 3) {
			if budget <= 0 {
				return false
			}
			budget--
			if lo.ContainsBy(check(c), func(d parity.Discrepancy) bool { return d.Key() == key }) {
				return true
			}
		}
		return false
	}
	try := func(mutate func(c *parity.Scenario) bool) bool {
		c := s.DeepCopy()
		if !mutate(c) {
			return false
		}
		if reproduces(c) {
			s = c
			return true
		}
		return false
	}
	for changed := true; changed && budget > 0; {
		changed = false
		for i := len(s.PendingPods) - 1; i >= 0; i-- {
			changed = try(func(c *parity.Scenario) bool {
				c.PendingPods = append(c.PendingPods[:i:i], c.PendingPods[i+1:]...)
				return true
			}) || changed
		}
		for i := len(s.BoundPods) - 1; i >= 0; i-- {
			changed = try(func(c *parity.Scenario) bool {
				c.BoundPods = append(c.BoundPods[:i:i], c.BoundPods[i+1:]...)
				return true
			}) || changed
		}
		for i := len(s.ExistingNodes) - 1; i >= 0; i-- {
			changed = try(func(c *parity.Scenario) bool {
				name := c.ExistingNodes[i].Node.Name
				c.ExistingNodes = append(c.ExistingNodes[:i:i], c.ExistingNodes[i+1:]...)
				c.BoundPods = lo.Reject(c.BoundPods, func(p *corev1.Pod, _ int) bool { return p.Spec.NodeName == name })
				return true
			}) || changed
		}
		for i := len(s.NodePools) - 1; i >= 0 && len(s.NodePools) > 1; i-- {
			changed = try(func(c *parity.Scenario) bool {
				c.NodePools = append(c.NodePools[:i:i], c.NodePools[i+1:]...)
				return true
			}) || changed
		}
		for i := range s.PendingPods {
			for _, m := range podSimplifications {
				changed = try(func(c *parity.Scenario) bool { return m(c.PendingPods[i]) }) || changed
			}
		}
	}
	return s
}

var podSimplifications = []func(p *corev1.Pod) bool{
	func(p *corev1.Pod) bool {
		ok := len(p.Spec.TopologySpreadConstraints) > 0
		p.Spec.TopologySpreadConstraints = nil
		return ok
	},
	func(p *corev1.Pod) bool {
		ok := len(p.Spec.TopologySpreadConstraints) > 1
		p.Spec.TopologySpreadConstraints = lo.Slice(p.Spec.TopologySpreadConstraints, 0, 1)
		return ok
	},
	func(p *corev1.Pod) bool { ok := p.Spec.Affinity != nil; p.Spec.Affinity = nil; return ok },
	func(p *corev1.Pod) bool {
		ok := p.Spec.Affinity != nil && p.Spec.Affinity.PodAffinity != nil
		if ok {
			p.Spec.Affinity.PodAffinity = nil
		}
		return ok
	},
	func(p *corev1.Pod) bool {
		ok := p.Spec.Affinity != nil && p.Spec.Affinity.PodAntiAffinity != nil
		if ok {
			p.Spec.Affinity.PodAntiAffinity = nil
		}
		return ok
	},
	func(p *corev1.Pod) bool {
		ok := p.Spec.Affinity != nil && p.Spec.Affinity.NodeAffinity != nil && len(p.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0
		if ok {
			p.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = nil
		}
		return ok
	},
	func(p *corev1.Pod) bool { ok := len(p.Spec.NodeSelector) > 0; p.Spec.NodeSelector = nil; return ok },
	func(p *corev1.Pod) bool { ok := len(p.Spec.Tolerations) > 0; p.Spec.Tolerations = nil; return ok },
	func(p *corev1.Pod) bool { ok := len(p.Spec.InitContainers) > 0; p.Spec.InitContainers = nil; return ok },
	func(p *corev1.Pod) bool {
		ok := false
		for i := range p.Spec.Containers {
			for j := range p.Spec.Containers[i].Ports {
				ok = ok || p.Spec.Containers[i].Ports[j].HostPort != 0
				p.Spec.Containers[i].Ports[j].HostPort = 0
			}
		}
		return ok
	},
}

// dump renders a scenario as YAML for a bug report or a regression Entry.
func dump(s *parity.Scenario) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# seed=%d profile=%s\n", s.Seed, s.Profile)
	write := func(obj any) {
		out, err := yaml.Marshal(obj)
		Expect(err).ToNot(HaveOccurred())
		b.WriteString("---\n")
		b.Write(out)
	}
	for _, np := range s.NodePools {
		np = np.DeepCopy()
		np.Status = v1.NodePoolStatus{}
		write(np)
	}
	for _, e := range s.ExistingNodes {
		n := e.Node.DeepCopy()
		n.Status.Conditions = nil
		write(n)
	}
	for _, p := range append(append([]*corev1.Pod{}, s.BoundPods...), s.PendingPods...) {
		p = p.DeepCopy()
		p.Status = corev1.PodStatus{}
		write(p)
	}
	return b.String()
}

var _ = Describe("Parity", func() {
	It("should agree with kube-scheduler Filter plugins on generated scenarios", func() {
		start := envInt("PARITY_SEED_START", 1)
		count := envInt("PARITY_SEEDS", 20)
		if seed := envInt("PARITY_SEED", 0); seed != 0 {
			start, count = seed, 1
		}
		profiles := strings.Split(envStr("PARITY_PROFILES", "small,medium"), ",")
		outDir := envStr("PARITY_OUT", "")
		doShrink := envStr("PARITY_SHRINK", "") != ""

		byKey := map[string][]parity.Discrepancy{}
		first := map[string]*parity.Scenario{}
		for seed := int64(start); seed < int64(start+count); seed++ {
			for _, name := range profiles {
				s := parity.Generate(seed, parity.Profiles[name])
				for _, d := range check(s) {
					if _, ok := first[d.Key()]; !ok {
						first[d.Key()] = s
					}
					byKey[d.Key()] = append(byKey[d.Key()], d)
				}
			}
		}

		keys := lo.Keys(byKey)
		sort.Slice(keys, func(i, j int) bool { return len(byKey[keys[i]]) > len(byKey[keys[j]]) })
		var unknown []string
		var report strings.Builder
		fmt.Fprintf(&report, "seeds=%d..%d profiles=%v scenarios=%d distinct=%d\n", start, start+count-1, profiles, count*len(profiles), len(keys))
		for _, k := range keys {
			ds := byKey[k]
			seeds := lo.Uniq(lo.Map(ds, func(d parity.Discrepancy, _ int) int64 { return d.Seed }))
			issue, known := parity.Known(ds[0])
			if !known {
				issue = "NEW"
				unknown = append(unknown, k)
			}
			fmt.Fprintf(&report, "%5d pods %4d seeds  [%s] %s\n        e.g. %s\n", len(ds), len(seeds), issue, k, ds[0])
		}
		for k, n := range allowedCounts {
			fmt.Fprintf(&report, "%5d pods            [allowlisted] %s\n", n, k)
		}
		GinkgoWriter.Print(report.String())
		if outDir != "" {
			Expect(os.MkdirAll(outDir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(outDir, fmt.Sprintf("report-%d-%d.txt", start, start+count-1)), []byte(report.String()), 0o644)).To(Succeed())
			all := lo.Flatten(lo.Values(byKey))
			raw, err := json.MarshalIndent(all, "", "  ")
			Expect(err).ToNot(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(outDir, fmt.Sprintf("discrepancies-%d-%d.json", start, start+count-1)), raw, 0o644)).To(Succeed())
			for i, k := range keys {
				s := first[k]
				if doShrink {
					s = shrink(s, k)
				}
				body := fmt.Sprintf("# %s\n%s", k, dump(s))
				Expect(os.WriteFile(filepath.Join(outDir, fmt.Sprintf("repro-%d-%d-%02d.yaml", start, start+count-1, i)), []byte(body), 0o644)).To(Succeed())
			}
		}
		Expect(unknown).To(BeEmpty(), report.String())
	})
})
