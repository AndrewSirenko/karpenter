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

package performancev2

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/labels"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

func scaleAndSettle(env *common.Environment, dep *appsv1.Deployment, targetReplicas int32, timeout time.Duration) {
	dep.Spec.Replicas = lo.ToPtr(targetReplicas)
	env.ExpectUpdated(dep)
	sel := labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"})
	env.EventuallyExpectHealthyPodCountWithTimeout(timeout, sel, int(targetReplicas))
	time.Sleep(2 * lo.FromPtr(nodePool.Spec.Disruption.ConsolidateAfter.Duration))
}

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Balanced Churn Chain", func() {
		It("should measure churn under Balanced across three scale-in / scale-out rounds", func() {
			By("Pinning ConsolidationPolicy for this run")
			nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyBalanced
			env.ExpectCreated(nodePool, nodeClass)

			By("Scaling out to the churn-chain fixture (400 pods)")
			dep := test.Deployment(test.CreateDeploymentOptions("churn-chain-app", 400, "900m", "3100Mi"))
			scaleOut := StartPhase(env, "balancedChurn/scaleOut")
			env.ExpectCreated(dep)
			scaleOut.AwaitScaleOut(400, 15*time.Minute)

			churn := StartPhase(env, "balancedChurn/consolidation")

			By("Round 1: scale in to 200 pods")
			scaleAndSettle(env, dep, 200, 10*time.Minute)
			By("Round 2: scale back out to 400 pods")
			scaleAndSettle(env, dep, 400, 10*time.Minute)
			By("Round 3: scale in to 200 pods")
			scaleAndSettle(env, dep, 200, 10*time.Minute)

			By("Waiting for consolidation to settle after the last round")
			report := churn.AwaitConsolidation(400, 200, 20*time.Minute)

			expectBalancedDecisionsMatchThreshold(report)
		})
	})
})
