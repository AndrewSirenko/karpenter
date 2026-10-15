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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
)

func buildFamilyRestrictedNodePool(base *v1.NodePool, family string) *v1.NodePool {
	np := base.DeepCopy()
	np.Name = fmt.Sprintf("%s-%s", family, base.Name)
	test.ReplaceRequirements(np, v1.NodeSelectorRequirementWithMinValues{
		Key:      v1alpha1.InstanceFamilyLabelKey,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{family},
	})
	return np
}

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Balanced Heterogeneous NodePools", func() {
		BeforeEach(func() {
			if !env.IsDefaultNodeClassKWOK() {
				Skip("heterogeneous NodePool fixture uses KWOK-only instance-family labels")
			}
		})
		It("should split load across two heterogeneous NodePools under Balanced", func() {
			By("Building two family-restricted NodePools")
			nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyBalanced
			poolC := buildFamilyRestrictedNodePool(nodePool, "c")
			poolM := buildFamilyRestrictedNodePool(nodePool, "m")
			env.ExpectCreated(nodeClass, poolC, poolM)

			By("Deploying a dense workload on the c-family pool and a sparse one on the m-family pool")
			denseDep := test.Deployment(test.CreateDeploymentOptions("het-dense-app", 300, "500m", "1Gi",
				test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: poolC.Name})))
			sparseDep := test.Deployment(test.CreateDeploymentOptions("het-sparse-app", 100, "2500m", "8Gi",
				test.WithNodeSelector(map[string]string{v1.NodePoolLabelKey: poolM.Name})))
			scaleOut := StartPhase(env, "balancedHeterogeneous/scaleOut")
			env.ExpectCreated(denseDep, sparseDep)
			scaleOut.AwaitScaleOut(400, 15*time.Minute)

			By("Scaling both deployments down to trigger cross-pool consolidation")
			consolidation := StartPhase(env, "balancedHeterogeneous/consolidation")
			denseDep.Spec.Replicas = lo.ToPtr(int32(180))
			sparseDep.Spec.Replicas = lo.ToPtr(int32(60))
			env.ExpectUpdated(denseDep, sparseDep)
			report := consolidation.AwaitConsolidation(400, 240, 25*time.Minute)

			By("Checking the scored moves belong to the two fixture NodePools")
			scoredPools := expectBalancedDecisionsMatchThreshold(report)
			Expect(scoredPools.Difference(sets.New(poolC.Name, poolM.Name)).UnsortedList()).To(BeEmpty(),
				"Balanced scored a move against a NodePool this fixture did not create")
		})
	})
})
