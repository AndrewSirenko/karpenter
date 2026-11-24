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
	"sync"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

type observedCommand struct {
	phase        disruption.CommandPhase
	id           uuid.UUID
	schedulingID types.UID
	succeeded    bool
	candidates   []string
	replacements []string
}

type commandRecorder struct {
	mu       sync.Mutex
	observed []observedCommand
}

func (r *commandRecorder) OnCommand(_ context.Context, cmd *disruption.Command, phase disruption.CommandPhase) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, observedCommand{
		phase:        phase,
		id:           cmd.ID,
		schedulingID: cmd.Results.ID,
		succeeded:    cmd.Succeeded,
		candidates:   lo.Map(cmd.Candidates, func(c *disruption.Candidate, _ int) string { return c.ProviderID() }),
		replacements: lo.Map(cmd.Replacements, func(r *disruption.Replacement, _ int) string { return r.Name }),
	})
}

var _ = Describe("CommandObserver", func() {
	var observer *commandRecorder
	var observedQueue *disruption.Queue
	var candidateNodeClaim *v1.NodeClaim
	var candidateNode *corev1.Node
	BeforeEach(func() {
		observer = &commandRecorder{}
		observedQueue = disruption.NewQueue(env.Client, recorder, cluster, env.Clock, prov, nodePoolBackoff, disruption.WithCommandObserver(observer))
		nodePool = test.NodePool()
		candidateNodeClaim, candidateNode = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[0].Name,
					v1.CapacityTypeLabelKey:        cloudProvider.InstanceTypes[0].Offerings.Cheapest().Requirements.Get(v1.CapacityTypeLabelKey).Any(),
					corev1.LabelTopologyZone:       cloudProvider.InstanceTypes[0].Offerings.Cheapest().Requirements.Get(corev1.LabelTopologyZone).Any(),
				},
			},
			Status: v1.NodeClaimStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("32")},
			},
		})
		ExpectApplied(ctx, env.Client, candidateNodeClaim, candidateNode, nodePool)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{candidateNode}, []*v1.NodeClaim{candidateNodeClaim})
	})
	newCommand := func() *disruption.Command {
		nct := scheduling.NewNodeClaimTemplate(nodePool)
		nct.InstanceTypeOptions = append([]*cloudprovider.InstanceType{}, cloudProvider.InstanceTypes...)
		return &disruption.Command{
			Method:            disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock, nodePoolBackoff),
			CreationTimestamp: env.Clock.Now(),
			ID:                uuid.New(),
			Results:           scheduling.Results{ID: types.UID(uuid.NewString())},
			Candidates:        []*disruption.Candidate{{StateNode: ExpectStateNodeExistsForNodeClaim(cluster, candidateNodeClaim), NodePool: nodePool}},
			Replacements:      []*disruption.Replacement{{NodeClaim: &scheduling.NodeClaim{NodeClaimTemplate: *nct}}},
		}
	}
	It("should be told when a command starts and completes, with its ID, candidates and replacements", func() {
		cmd := newCommand()
		Expect(observedQueue.StartCommand(ctx, cmd)).To(Succeed())
		Expect(observer.observed).To(HaveLen(1))
		started := observer.observed[0]
		Expect(started.phase).To(Equal(disruption.CommandStarted))
		Expect(started.id).To(Equal(cmd.ID))
		Expect(started.schedulingID).To(Equal(cmd.Results.ID))
		Expect(started.candidates).To(ConsistOf(candidateNodeClaim.Status.ProviderID))
		Expect(started.replacements).To(HaveLen(1))
		Expect(started.replacements[0]).ToNot(BeEmpty())

		replacement := &v1.NodeClaim{}
		Expect(env.Client.Get(ctx, types.NamespacedName{Name: started.replacements[0]}, replacement)).To(Succeed())
		replacement, replacementNode := ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, replacement)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{replacementNode}, []*v1.NodeClaim{replacement})
		ExpectObjectReconciled(ctx, env.Client, observedQueue, candidateNodeClaim)

		Expect(observer.observed).To(HaveLen(2))
		completed := observer.observed[1]
		Expect(completed.phase).To(Equal(disruption.CommandCompleted))
		Expect(completed.id).To(Equal(cmd.ID))
		Expect(completed.succeeded).To(BeTrue())
		Expect(completed.replacements).To(Equal(started.replacements))
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, candidateNodeClaim)
	})
	It("should be told when a command fails to start", func() {
		Expect(observedQueue.StartCommand(ctx, newCommand())).To(Succeed())
		again := newCommand()
		Expect(observedQueue.StartCommand(ctx, again)).ToNot(Succeed())
		Expect(observer.observed).To(HaveLen(2))
		Expect(observer.observed[1].phase).To(Equal(disruption.CommandStartFailed))
		Expect(observer.observed[1].id).To(Equal(again.ID))
	})
	It("should expose the candidate's instance type, zone, capacity type and pods", func() {
		candidates, err := disruption.GetCandidates(ctx, cluster, env.Client, recorder, env.Clock, cloudProvider, func(context.Context, *disruption.Candidate) bool { return true }, disruption.GracefulDisruptionClass, observedQueue)
		Expect(err).ToNot(HaveOccurred())
		Expect(candidates).To(HaveLen(1))
		Expect(candidates[0].InstanceTypeName()).To(Equal(candidateNodeClaim.Labels[corev1.LabelInstanceTypeStable]))
		Expect(candidates[0].Zone()).To(Equal(candidateNodeClaim.Labels[corev1.LabelTopologyZone]))
		Expect(candidates[0].CapacityType()).To(Equal(candidateNodeClaim.Labels[v1.CapacityTypeLabelKey]))
		Expect(candidates[0].ReschedulablePods()).To(BeEmpty())
	})
})
