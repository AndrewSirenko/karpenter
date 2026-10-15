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

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
)

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Balanced Basic Fixture", func() {
		It("should consolidate the basic fixture under Balanced", func() {
			nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyBalanced
			smallDeployment := test.Deployment(test.CreateDeploymentOptions("small-resource-app", 500, "900m", "3100Mi"))
			largeDeployment := test.Deployment(test.CreateDeploymentOptions("large-resource-app", 500, "3500m", "28Gi"))

			scaleOut := StartPhase(env, "balancedBasic/scaleOut")
			env.ExpectCreated(nodePool, nodeClass, smallDeployment, largeDeployment)
			scaleOut.AwaitScaleOut(1000, 15*time.Minute)

			consolidation := StartPhase(env, "balancedBasic/consolidation")
			smallDeployment.Spec.Replicas = lo.ToPtr(int32(350))
			largeDeployment.Spec.Replicas = lo.ToPtr(int32(350))
			env.ExpectUpdated(smallDeployment, largeDeployment)
			report := consolidation.AwaitConsolidation(1000, 700, 20*time.Minute)

			expectBalancedDecisionsMatchThreshold(report)
		})
	})
})
