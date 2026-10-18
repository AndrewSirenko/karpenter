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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/parity"
)

func instanceType(name string) *cloudprovider.InstanceType {
	it, ok := lo.Find(parity.InstanceTypes(), func(it *cloudprovider.InstanceType) bool { return it.Name == name })
	Expect(ok).To(BeTrue(), name)
	return it
}

func offering(it *cloudprovider.InstanceType, zone, capacityType string) *cloudprovider.Offering {
	o, ok := lo.Find(it.Offerings, func(o *cloudprovider.Offering) bool {
		return o.Requirements.Get(corev1.LabelTopologyZone).Any() == zone && o.Requirements.Get(v1.CapacityTypeLabelKey).Any() == capacityType
	})
	Expect(ok).To(BeTrue())
	return o
}

func pendingPod(name string, mutate func(*corev1.Pod)) *corev1.Pod {
	p := test.UnschedulablePod(test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: parity.Namespace, Labels: map[string]string{parity.AppLabel: "a"}},
		ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		}},
	})
	mutate(p)
	return p
}

func requiredTerms(terms ...[]corev1.NodeSelectorRequirement) *corev1.Affinity {
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: lo.Map(terms, func(t []corev1.NodeSelectorRequirement, _ int) corev1.NodeSelectorTerm {
			return corev1.NodeSelectorTerm{MatchExpressions: t}
		}),
	}}}
}

// These scenarios are minimized from generated failures. Each asserts the harness still sees the divergence;
// once Karpenter is fixed the entry fails and should be flipped to expect no discrepancy.
var _ = Describe("Known divergences", func() {
	DescribeTable("reproduces",
		func(build func() *parity.Scenario, class parity.Class, plugin string) {
			ds := check(build())
			Expect(ds).To(ContainElement(SatisfyAll(
				HaveField("Class", class),
				HaveField("Plugin", plugin),
			)), "discrepancies: %v", ds)
		},
		// nodeSelector special=optional intersected with special NotIn [optional] is empty, which Karpenter
		// represents as DoesNotExist, so a node without the label looks compatible.
		Entry("#3423: contradictory requirements on one key collapse to DoesNotExist", func() *parity.Scenario {
			np := parity.NewNodePool("np-0")
			it := instanceType("c2-m16-amd64")
			return &parity.Scenario{
				NodePools:     []*v1.NodePool{np},
				ExistingNodes: []parity.ExistingNode{parity.NewExistingNode("existing-0", np, it, offering(it, "test-zone-1", v1.CapacityTypeOnDemand))},
				PendingPods: []*corev1.Pod{pendingPod("pod-0", func(p *corev1.Pod) {
					p.Spec.NodeSelector = map[string]string{fake.ExoticInstanceLabelKey: "optional"}
					p.Spec.Affinity = requiredTerms([]corev1.NodeSelectorRequirement{{Key: fake.ExoticInstanceLabelKey, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"optional"}}})
				})},
			}
		}, parity.LaunchedUnusable, "NodeAffinity"),
		// kube-scheduler ORs required nodeSelectorTerms. Karpenter tries term 0 first and launches a node for it,
		// without checking whether an existing node satisfies term 1.
		Entry("later required nodeSelectorTerm satisfied by an existing node", func() *parity.Scenario {
			np := parity.NewNodePool("np-0")
			it := instanceType("c1-m8-arm64")
			return &parity.Scenario{
				NodePools:     []*v1.NodePool{np},
				ExistingNodes: []parity.ExistingNode{parity.NewExistingNode("existing-0", np, it, offering(it, "test-zone-2", v1.CapacityTypeSpot))},
				PendingPods: []*corev1.Pod{pendingPod("pod-0", func(p *corev1.Pod) {
					p.Spec.Affinity = requiredTerms(
						[]corev1.NodeSelectorRequirement{{Key: fake.IntegerInstanceLabelKey, Operator: corev1.NodeSelectorOpGt, Values: []string{"2"}}},
						[]corev1.NodeSelectorRequirement{{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{it.Name}}},
					)
				})},
			}
		}, parity.MissedExisting, "-"),
		// pod-x can't tolerate the only NodePool's taint, so its inverse anti-affinity group has no domains, and
		// Karpenter treats every pod it selects as unschedulable. pod-x is pending, so kube-scheduler ignores it.
		Entry("pending pod's anti-affinity with no tolerated domains blocks the pods it selects", func() *parity.Scenario {
			taint := corev1.Taint{Key: parity.DedicatedTaint, Value: "red", Effect: corev1.TaintEffectNoSchedule}
			np := parity.NewNodePool("np-0")
			np.Spec.Template.Spec.Taints = []corev1.Taint{taint}
			it := instanceType("c4-m8-amd64")
			return &parity.Scenario{
				NodePools:     []*v1.NodePool{np},
				ExistingNodes: []parity.ExistingNode{parity.NewExistingNode("existing-0", np, it, offering(it, "test-zone-2", v1.CapacityTypeSpot))},
				PendingPods: []*corev1.Pod{
					pendingPod("pod-x", func(p *corev1.Pod) {
						p.Labels[parity.AppLabel] = "c"
						p.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{parity.AppLabel: "a"}}, TopologyKey: corev1.LabelTopologyZone,
						}}}}
					}),
					pendingPod("pod-y", func(p *corev1.Pod) {
						p.Spec.Tolerations = []corev1.Toleration{{Key: taint.Key, Operator: corev1.TolerationOpEqual, Value: taint.Value, Effect: taint.Effect}}
					}),
				},
			}
		}, parity.FalseUnschedulable, "-"),
		// capacity-type has two domains and minDomains=4, so kube-scheduler's global minimum is 0 and each
		// capacity type can hold one app=a pod. Karpenter puts pod-1 next to another app=a pod. Minimized from seed 1152.
		Entry("minDomains above the number of capacity-type domains", func() *parity.Scenario {
			np := parity.NewNodePool("np-0")
			np.Spec.Template.Spec.Requirements = append(np.Spec.Template.Spec.Requirements,
				v1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-zone-1", "test-zone-3"}})
			return &parity.Scenario{
				NodePools: []*v1.NodePool{np},
				PendingPods: []*corev1.Pod{
					pendingPod("pod-0", func(p *corev1.Pod) {
						p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
					}),
					pendingPod("pod-1", func(p *corev1.Pod) {
						p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("3")
						p.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
							MaxSkew: 1, MinDomains: lo.ToPtr(int32(4)), TopologyKey: v1.CapacityTypeLabelKey, WhenUnsatisfiable: corev1.DoNotSchedule,
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{parity.AppLabel: "a"}},
						}}
					}),
					pendingPod("pod-2", func(p *corev1.Pod) {
						p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("3")
					}),
					pendingPod("pod-3", func(p *corev1.Pod) {
						p.Labels[parity.AppLabel] = "c"
						p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1")
					}),
				},
			}
		}, parity.LaunchedUnusable, "PodTopologySpread"),
		// pod-1 needs an app=a pod on its node, and the only app=a pod forbids app=c pods on its node. kube-scheduler
		// can't place pod-1 anywhere; Karpenter puts both on one new node. Minimized from seed 2078.
		Entry("pod affinity satisfied by a pod whose anti-affinity rejects it", func() *parity.Scenario {
			return &parity.Scenario{
				NodePools: []*v1.NodePool{parity.NewNodePool("np-0")},
				PendingPods: []*corev1.Pod{
					pendingPod("pod-1", func(p *corev1.Pod) {
						p.Labels[parity.AppLabel] = "c"
						p.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{parity.AppLabel: "a"}}, TopologyKey: corev1.LabelHostname,
						}}}}
					}),
					pendingPod("pod-3", func(p *corev1.Pod) {
						p.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{parity.AppLabel: "c"}}, TopologyKey: corev1.LabelHostname,
						}}}}
					}),
				},
			}
		}, parity.LaunchedUnusable, "InterPodAffinity"),
	)
})
