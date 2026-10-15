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
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/karpenter/test/pkg/debug"

	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Do Not Disrupt Performance Test", func() {
		It("should efficiently scale three deployments and test disruption protection", func() {
			env.ExpectCreated(nodePool, nodeClass)
			smallDeployment := test.Deployment(test.CreateDeploymentOptions("small-resource-app", 500, "900m", "3100Mi", test.WithHostnameSpread()))
			largeDeployment := test.Deployment(test.CreateDeploymentOptions("large-resource-app", 500, "3500m", "28Gi"))
			protectedDeployment := test.Deployment(test.CreateDeploymentOptions("do-not-disrupt-app", 100, "900m", "450Mi", test.WithDoNotDisrupt()))

			By("Scaling out 1100 pods, 100 of them do-not-disrupt")
			scaleOut := StartPhase(env, "doNotDisrupt/scaleOut")
			env.ExpectCreated(smallDeployment, largeDeployment, protectedDeployment)
			scaleOut.AwaitScaleOut(1100, 15*time.Minute)

			nodesWithProtectedPods := map[string]bool{}
			for _, pod := range env.Monitor.RunningPods(labels.SelectorFromSet(map[string]string{"app": "do-not-disrupt-app", test.DiscoveryLabel: "unspecified"})) {
				if pod.Spec.NodeName != "" {
					nodesWithProtectedPods[pod.Spec.NodeName] = true
				}
			}

			By("Scaling in the unprotected deployments to trigger consolidation")
			consolidation := StartPhase(env, "doNotDisrupt/consolidation")
			smallDeployment.Spec.Replicas = new(int32(250))
			largeDeployment.Spec.Replicas = new(int32(250))
			env.ExpectUpdated(smallDeployment, largeDeployment)
			consolidation.AwaitConsolidation(1100, 600, 25*time.Minute)

			// Check if nodes with do-not-disrupt pods are still present
			currentNodes := env.Monitor.CreatedNodes()
			protectedNodesStillPresent := 0
			for _, node := range currentNodes {
				if nodesWithProtectedPods[node.Name] {
					protectedNodesStillPresent++
				}
			}

			By("Validating that protected nodes were not disrupted")
			Expect(protectedNodesStillPresent).To(BeNumerically(">", 0),
				"At least some nodes with do-not-disrupt pods should remain protected")

		})
	})
})
