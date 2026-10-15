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
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/debug"
)

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	Context("Drift Performance", func() {
		It("should efficiently handle drift replacement of pods with topology constraints", func() {
			env.ExpectCreated(nodePool, nodeClass)
			hostnameSpreadDeployment := test.Deployment(test.CreateDeploymentOptions("hostname-spread-app", 300, "900m", "3100Mi", test.WithHostnameSpread()))
			standardDeployment := test.Deployment(test.CreateDeploymentOptions("standard-app", 300, "3500m", "28Gi"))

			By("Deploying 600 pods")
			initial := StartPhase(env, "drift/initial")
			env.ExpectCreated(hostnameSpreadDeployment, standardDeployment)
			initial.AwaitScaleOut(600, 20*time.Minute)

			By("Allowing system to stabilize before triggering drift")
			time.Sleep(30 * time.Second)

			By("Triggering drift by updating the NodePool template")
			drift := StartPhase(env, "drift/drift")
			if nodePool.Spec.Template.Annotations == nil {
				nodePool.Spec.Template.Annotations = map[string]string{}
			}
			nodePool.Spec.Template.Annotations["test-drift-trigger"] = fmt.Sprintf("drift-%d", time.Now().Unix())
			env.ExpectUpdated(nodePool)
			drift.AwaitDrift(600, 30*time.Minute)

			env.EventuallyExpectHealthyPodCount(labels.SelectorFromSet(map[string]string{test.DiscoveryLabel: "unspecified"}), 600)
		})
	})
})
