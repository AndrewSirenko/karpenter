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

package scheduling_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/state/virtualpods"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

type hookedCloudProvider struct {
	*fake.CloudProvider
	hook func()
}

func (h *hookedCloudProvider) GetInstanceTypes(ctx context.Context, np *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	if hook := h.hook; hook != nil {
		h.hook = nil
		hook()
	}
	return h.CloudProvider.GetInstanceTypes(ctx, np)
}

var _ = Describe("StatefulSet pod replaced while Schedule runs", func() {
	DescribeTable("should only request capacity for a StatefulSet pod that has no replacement",
		func(preferred bool, replacementOrdinal int, bindToDeletingNode bool, expectedNodeClaims int) {
			labels := map[string]string{"app": "sts"}
			term := corev1.PodAffinityTerm{
				LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
				TopologyKey:   corev1.LabelHostname,
			}
			podOptions := test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			}
			if preferred {
				podOptions.PodAntiPreferences = []corev1.WeightedPodAffinityTerm{{Weight: 100, PodAffinityTerm: term}}
			} else {
				podOptions.PodAntiRequirements = []corev1.PodAffinityTerm{term}
			}
			nodePool := test.NodePool()
			ss := test.StatefulSet(test.StatefulSetOptions{Replicas: int32(lo.Max([]int{replacementOrdinal, 0}) + 1), PodOptions: podOptions})
			ExpectApplied(ctx, env.Client, nodePool, ss)

			stsPod := func(ordinal int) *corev1.Pod {
				opts := podOptions
				opts.ObjectMeta = metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-%d", ss.Name, ordinal),
					Namespace: ss.Namespace,
					Labels:    labels,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion:         "apps/v1",
						Kind:               "StatefulSet",
						Name:               ss.Name,
						UID:                ss.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					}},
				}
				return test.Pod(opts)
			}
			newNode := func() (*v1.NodeClaim, *corev1.Node) {
				capacity := corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("110"),
				}
				nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1.NodePoolLabelKey:            nodePool.Name,
							corev1.LabelInstanceTypeStable: "small-instance-type",
							v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
							corev1.LabelTopologyZone:       "test-zone-1a",
						},
					},
					Status: v1.NodeClaimStatus{Capacity: capacity, Allocatable: capacity},
				})
				node.Labels[corev1.LabelHostname] = node.Name
				return nodeClaim, node
			}

			nodeClaim, node := newNode()
			deletingNodeClaim, deletingNode := newNode()
			ExpectApplied(ctx, env.Client, nodeClaim, node, deletingNodeClaim, deletingNode)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController,
				[]*corev1.Node{node, deletingNode}, []*v1.NodeClaim{nodeClaim, deletingNodeClaim})
			cluster.MarkForDeletion(deletingNode.Spec.ProviderID)

			old := stsPod(0)
			ExpectApplied(ctx, env.Client, old)
			ExpectManualBinding(ctx, env.Client, old, deletingNode)
			ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(old))
			ExpectEvicted(ctx, env.Client, old)
			old = ExpectExists(ctx, env.Client, old)
			Expect(old.DeletionTimestamp.IsZero()).To(BeFalse())

			var replacement *corev1.Pod
			hooked := &hookedCloudProvider{CloudProvider: cloudProvider}
			hooked.hook = func() {
				Expect(env.Client.Delete(ctx, old, client.GracePeriodSeconds(0))).To(Succeed())
				ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(old))
				if replacementOrdinal >= 0 {
					replacement = stsPod(replacementOrdinal)
					ExpectApplied(ctx, env.Client, replacement)
					ExpectManualBinding(ctx, env.Client, replacement, lo.Ternary(bindToDeletingNode, deletingNode, node))
					ExpectReconcileSucceeded(ctx, podStateController, client.ObjectKeyFromObject(replacement))
					Expect(replacement.UID).ToNot(Equal(old.UID))
				}
			}
			racing := provisioning.NewProvisioner(env.Client, events.NewRecorder(&record.FakeRecorder{}), hooked, cluster, env.Clock,
				deviceallocation.NewController(env.Client), virtualpods.NewVirtualPodCache(env.Client), predictionStore)

			results, err := racing.Schedule(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(hooked.hook).To(BeNil(), "the hook did not run during Schedule")
			Expect(results.PodErrors).To(BeEmpty())

			claimed := lo.FlatMap(results.NewNodeClaims, func(nc *scheduling.NodeClaim, _ int) []string {
				return lo.Map(nc.Pods, func(p *corev1.Pod, _ int) string { return fmt.Sprintf("%s (uid %s)", p.Name, p.UID) })
			})
			Expect(results.NewNodeClaims).To(HaveLen(expectedNodeClaims),
				"pods on new NodeClaims: %v; terminating pod uid %s; pod bound to %s during Schedule: %v", claimed, old.UID, node.Name,
				lo.Ternary(replacement == nil, "none", fmt.Sprintf("%s (uid %s)", lo.FromPtr(replacement).Name, lo.FromPtr(replacement).UID)))
			if replacementOrdinal < 0 || bindToDeletingNode {
				Expect(results.ExistingNodeToPodMapping()[nodeClaim.Name]).To(ConsistOf(HaveField("UID", old.UID)))
			}
		},
		Entry("preferred hostname anti-affinity, ordinal 0 is recreated and bound", true, 0, false, 0),
		Entry("required hostname anti-affinity, ordinal 0 is recreated and bound", false, 0, false, 0),
		Entry("ordinal 0 is not recreated", true, -1, false, 0),
		Entry("ordinal 0 is recreated and bound to the deleting node", true, 0, true, 0),
		Entry("another ordinal is bound to the node that stays", false, 1, false, 1),
	)
})
