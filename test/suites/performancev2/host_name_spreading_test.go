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

type hostNameSpreadingScale struct {
	context, id                           string
	perDeployment, scaledInPerDeployment  int32
	scaleOutTimeout, consolidationTimeout time.Duration
}

var _ = Describe("Performance", Label(debug.NoWatch), func() {
	for _, s := range []hostNameSpreadingScale{
		{"Host Name Spreading Deployment Reg", "hostNameSpreading", 500, 350, 15 * time.Minute, 30 * time.Minute},
		{"Host Name Spreading Deployment XL", "hostNameSpreadingXL", 1000, 700, 25 * time.Minute, 25 * time.Minute},
	} {
		Context(s.context, func() {
			It("should efficiently scale two deployments with host name topology spreading", func() {
				env.ExpectCreated(nodePool, nodeClass)
				smallDeployment := test.Deployment(test.CreateDeploymentOptions("small-resource-app", s.perDeployment, "900m", "3100Mi", test.WithHostnameSpread()))
				largeDeployment := test.Deployment(test.CreateDeploymentOptions("large-resource-app", s.perDeployment, "3500m", "28Gi"))
				total, scaledIn := int(2*s.perDeployment), int(2*s.scaledInPerDeployment)

				By("Scaling out with hostname spreading")
				scaleOut := StartPhase(env, s.id+"/scaleOut")
				env.ExpectCreated(smallDeployment, largeDeployment)
				scaleOut.AwaitScaleOut(total, s.scaleOutTimeout)

				By("Scaling in to trigger consolidation")
				consolidation := StartPhase(env, s.id+"/consolidation")
				smallDeployment.Spec.Replicas = new(s.scaledInPerDeployment)
				largeDeployment.Spec.Replicas = new(s.scaledInPerDeployment)
				env.ExpectUpdated(smallDeployment, largeDeployment)
				report := consolidation.AwaitConsolidation(total, scaledIn, s.consolidationTimeout)
				Expect(report.PodsNetChange).To(Equal(scaledIn - total))
			})
		})
	}
})
