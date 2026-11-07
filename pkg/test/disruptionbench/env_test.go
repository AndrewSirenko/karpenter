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

package disruptionbench_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/test/disruptionbench"
)

// halfFullFixture returns n 8-cpu nodes, each running four 1-cpu pods, so any one node's pods fit on the others.
func halfFullFixture(n int) *disruptionbench.Fixture {
	it := fake.NewInstanceType("bench-8x", fake.WithResources(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("8"),
		corev1.ResourceMemory: resource.MustParse("32Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}))
	np := test.NodePool(v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	np.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
	f := &disruptionbench.Fixture{NodePools: []*v1.NodePool{np}, InstanceTypes: []*cloudprovider.InstanceType{it}}
	zones := []string{"test-zone-1", "test-zone-2", "test-zone-3"}
	for i := range n {
		name := fmt.Sprintf("node-%d", i)
		nc, node := test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
				v1.NodePoolLabelKey:            np.Name,
				corev1.LabelInstanceTypeStable: it.Name,
				v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
				corev1.LabelTopologyZone:       zones[i%len(zones)],
				corev1.LabelHostname:           name,
			}},
			Status: v1.NodeClaimStatus{NodeName: name, Capacity: it.Capacity, Allocatable: it.Allocatable()},
		})
		node.Name = name
		f.NodeClaims = append(f.NodeClaims, nc)
		f.Nodes = append(f.Nodes, node)
		for range 4 {
			f.Pods = append(f.Pods, test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "app", UID: "app", Controller: lo.ToPtr(true),
				}}},
				NodeName:             name,
				Phase:                corev1.PodRunning,
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
			}))
		}
	}
	return f
}

func testContext() context.Context {
	return log.IntoContext(options.ToContext(context.Background(), test.Options()), logr.Discard())
}

func TestNewEnv(t *testing.T) {
	ctx := testContext()
	env, err := disruptionbench.NewEnv(ctx, halfFullFixture(20))
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := env.Candidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 20 {
		t.Fatalf("expected 20 candidates, got %d", len(candidates))
	}

	cmds, err := env.ComputeCommands(ctx, env.Methods().SingleNode)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Decision() != disruption.DeleteDecision || len(cmds[0].Candidates) != 1 {
		t.Fatalf("expected one single-node delete, got %v", cmds)
	}

	cmds, err = env.ComputeCommands(ctx, env.Methods().MultiNode)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds[0].Decision() != disruption.DeleteDecision || len(cmds[0].Candidates) < 2 {
		t.Fatalf("expected one multi-node delete, got %v", cmds)
	}

	// Starting the command writes through the client, and the writes reach cluster state.
	cmd := cmds[0]
	if err := env.StartCommand(ctx, env.Methods().MultiNode, &cmd); err != nil {
		t.Fatal(err)
	}
	for _, c := range cmd.Candidates {
		node := &corev1.Node{}
		if err := env.Client.Get(ctx, client.ObjectKey{Name: c.Name()}, node); err != nil {
			t.Fatal(err)
		}
		if !lo.ContainsBy(node.Spec.Taints, func(taint corev1.Taint) bool { return taint.MatchTaint(&v1.DisruptedNoScheduleTaint) }) {
			t.Fatalf("expected %s to be tainted", c.Name())
		}
	}
	tainted := 0
	for n := range env.Cluster.Nodes() {
		if n.MarkedForDeletion() && lo.ContainsBy(n.Taints(), func(taint corev1.Taint) bool { return taint.MatchTaint(&v1.DisruptedNoScheduleTaint) }) {
			tainted++
		}
	}
	if tainted != len(cmd.Candidates) {
		t.Fatalf("expected %d tainted nodes marked for deletion in cluster state, got %d", len(cmd.Candidates), tainted)
	}
	// The command nominates the nodes its pods move to, which hides them from disruption until the nomination expires.
	env.Clock.Step(time.Minute)
	candidates, err = env.Candidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 20-len(cmd.Candidates) {
		t.Fatalf("expected %d candidates after the command, got %d", 20-len(cmd.Candidates), len(candidates))
	}
}
