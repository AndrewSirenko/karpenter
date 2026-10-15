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
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
)

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Self Anti-Affinity Deployment Interference", func() {
		It("should efficiently scale two deployments with self anti-affinity", func() {
			env.ExpectCreated(nodePool, nodeClass)
			smallDeployment := test.Deployment(test.CreateDeploymentOptions("small-resource-app", 500, "900m", "3100Mi", test.WithPodAntiAffinityHostname()))
			largeDeployment := test.Deployment(test.CreateDeploymentOptions("large-resource-app", 500, "3500m", "28Gi", test.WithPodAntiAffinityHostname()))

			By("Scaling out 500 self anti-affine pods")
			scaleOut := StartPhase(env, "interference/scaleOut")
			env.ExpectCreated(smallDeployment)
			scaleOut.AwaitScaleOut(500, 15*time.Minute)

			By("Scaling the first deployment in to 250 while creating the second at 500")
			interference := StartPhase(env, "interference/interference")
			smallDeployment.Spec.Replicas = new(int32(250))
			env.ExpectUpdated(smallDeployment)
			env.ExpectCreated(largeDeployment)
			interference.AwaitScaleOut(750, 5*time.Minute)

			By("Scaling the first deployment out to 400 and the second in to 200")
			consolidation := StartPhase(env, "interference/consolidation")
			smallDeployment.Spec.Replicas = new(int32(400))
			largeDeployment.Spec.Replicas = new(int32(200))
			env.ExpectUpdated(smallDeployment, largeDeployment)
			report := consolidation.AwaitConsolidation(750, 600, 15*time.Minute)
			Expect(report.PodsNetChange).To(Equal(-150))
		})
	})
})
