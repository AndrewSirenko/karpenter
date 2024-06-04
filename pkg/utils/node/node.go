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

package node

import (
	"context"
	"fmt"

	storagev1 "k8s.io/api/storage/v1"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/utils/pod"
)

// GetPods grabs all pods that are currently bound to the passed nodes
func GetPods(ctx context.Context, kubeClient client.Client, nodes ...*v1.Node) ([]*v1.Pod, error) {
	var pods []*v1.Pod
	for _, node := range nodes {
		var podList v1.PodList
		if err := kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
			return nil, fmt.Errorf("listing pods, %w", err)
		}
		for i := range podList.Items {
			pods = append(pods, &podList.Items[i])
		}
	}
	return pods, nil
}

// GetReschedulablePods grabs all pods from the passed nodes that satisfy the IsReschedulable criteria
func GetReschedulablePods(ctx context.Context, kubeClient client.Client, nodes ...*v1.Node) ([]*v1.Pod, error) {
	pods, err := GetPods(ctx, kubeClient, nodes...)
	if err != nil {
		return nil, fmt.Errorf("listing pods, %w", err)
	}
	return lo.Filter(pods, func(p *v1.Pod, _ int) bool {
		return pod.IsReschedulable(p)
	}), nil
}

// GetProvisionablePods grabs all the pods from the passed nodes that satisfy the IsProvisionable criteria
func GetProvisionablePods(ctx context.Context, kubeClient client.Client) ([]*v1.Pod, error) {
	var podList v1.PodList
	if err := kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": ""}); err != nil {
		return nil, fmt.Errorf("listing pods, %w", err)
	}
	return lo.FilterMap(podList.Items, func(p v1.Pod, _ int) (*v1.Pod, bool) {
		return &p, pod.IsProvisionable(&p)
	}), nil
}

// GetVolumeAttachments grabs all attached volumeAttachments to passed node
func GetVolumeAttachments(ctx context.Context, kubeClient client.Client, node *v1.Node) ([]*storagev1.VolumeAttachment, error) {
	var volumeAttachments []*storagev1.VolumeAttachment

	var volumeAttachmentList storagev1.VolumeAttachmentList
	if err := kubeClient.List(ctx, &volumeAttachmentList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return nil, fmt.Errorf("listing volumeattachments, %w", err)
	}
	for i := range volumeAttachmentList.Items {
		if volumeAttachmentList.Items[i].Spec.NodeName == node.Name {
			volumeAttachments = append(volumeAttachments, &volumeAttachmentList.Items[i])
		}
	}

	return volumeAttachments, nil
}

func GetCondition(n *v1.Node, match v1.NodeConditionType) v1.NodeCondition {
	for _, condition := range n.Status.Conditions {
		if condition.Type == match {
			return condition
		}
	}
	return v1.NodeCondition{}
}
