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

package provisioning_test

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/state/virtualpods"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

type schedulingRecorder struct {
	mu      sync.Mutex
	results []pscheduling.Results
	names   [][]string
}

func (r *schedulingRecorder) OnScheduled(_ context.Context, results pscheduling.Results, nodeClaimNames []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, results)
	r.names = append(r.names, nodeClaimNames)
}

var _ = Describe("SchedulingObserver", func() {
	var observer *schedulingRecorder
	var observed *provisioning.Provisioner
	BeforeEach(func() {
		observer = &schedulingRecorder{}
		observed = provisioning.NewProvisioner(env.Client, events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster, env.Clock, deviceallocation.NewController(env.Client), virtualpods.NewVirtualPodCache(env.Client), predictionStore, provisioning.WithSchedulingObserver(observer))
	})
	reconcile := func(pods ...*corev1.Pod) {
		for _, pod := range pods {
			observed.Trigger(pod.UID)
		}
		if len(pods) == 0 {
			observed.Trigger("none")
		}
		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			ExpectSingletonReconciled(ctx, observed)
		}()
		Eventually(func() bool { return env.Clock.HasWaiters() }, time.Second).Should(BeTrue())
		env.Clock.Step(11 * time.Second)
		wg.Wait()
	}
	It("should be told the scheduling ID and the NodeClaims a solve created", func() {
		pod := test.UnschedulablePod()
		ExpectApplied(ctx, env.Client, test.NodePool(), pod)
		reconcile(pod)

		nodeClaims := ExpectNodeClaims(ctx, env.Client)
		Expect(nodeClaims).To(HaveLen(1))
		Expect(observer.results).To(HaveLen(1))
		results := observer.results[0]
		Expect(results.ID).ToNot(BeEmpty())
		Expect(results.NewNodeClaims).To(HaveLen(1))
		Expect(lo.Map(results.NewNodeClaims[0].Pods, func(p *corev1.Pod, _ int) string { return string(p.UID) })).To(ConsistOf(string(pod.UID)))
		Expect(observer.names[0]).To(Equal([]string{nodeClaims[0].Name}))
	})
	It("should give each solve its own scheduling ID", func() {
		pod := test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.NodePoolLabelKey: "missing"}})
		ExpectApplied(ctx, env.Client, test.NodePool(), pod)
		reconcile(pod)
		reconcile(pod)
		Expect(observer.results).To(HaveLen(2))
		Expect(observer.results[0].ID).ToNot(Equal(observer.results[1].ID))
	})
	It("should be told about a solve that created no NodeClaims", func() {
		pod := test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.NodePoolLabelKey: "missing"}})
		ExpectApplied(ctx, env.Client, test.NodePool(), pod)
		reconcile(pod)

		Expect(ExpectNodeClaims(ctx, env.Client)).To(BeEmpty())
		Expect(observer.results).To(HaveLen(1))
		Expect(observer.results[0].ID).ToNot(BeEmpty())
		Expect(observer.results[0].PodErrors).To(HaveLen(1))
		Expect(observer.names[0]).To(BeEmpty())
	})
	It("should not be told when nothing was pending", func() {
		ExpectApplied(ctx, env.Client, test.NodePool())
		reconcile()
		Expect(observer.results).To(BeEmpty())
	})
})
