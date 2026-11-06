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

package inplace_test

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/inplace"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var controller *inplace.Controller
var env *test.Environment
var cloudProvider *fake.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "InPlace")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...), test.WithFieldIndexers(test.NodeProviderIDFieldIndexer(ctx)))
	ctx = options.ToContext(ctx, test.Options())
	cloudProvider = fake.NewCloudProvider()
	controller = inplace.NewController(env.Client, cloudProvider)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cloudProvider.Reset()
})

func initializedNodeClaim(annotations map[string]string) *v1.NodeClaim {
	nc, _ := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}})
	nc.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
	ExpectApplied(ctx, env.Client, nc)
	return nc
}

func updating(nc *v1.NodeClaim) (bool, string) {
	c := ExpectExists(ctx, env.Client, nc).StatusConditions().Get(v1.ConditionTypeUpdating)
	if c == nil {
		return false, ""
	}
	return c.IsTrue(), c.Reason
}

const override = `{"blockDeviceMappings":{"/dev/xvda":{"iops":6000}}}`

var _ = Describe("InPlace", func() {
	It("does nothing without an override", func() {
		nc := initializedNodeClaim(nil)
		ExpectObjectReconciled(ctx, env.Client, controller, nc)
		Expect(cloudProvider.UpdateOperationIDs).To(BeEmpty())
		_, reason := updating(nc)
		Expect(reason).To(BeEmpty())
	})
	It("issues the update and keeps polling until the provider reports done", func() {
		done := false
		cloudProvider.UpdateFunc = func(*v1.NodeClaim, string) (bool, error) { return done, nil }
		nc := initializedNodeClaim(map[string]string{v1.NodeClassOverrideAnnotationKey: override})

		result := ExpectObjectReconciled(ctx, env.Client, controller, nc)
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		status, reason := updating(nc)
		Expect(status).To(BeTrue())
		Expect(reason).To(Equal(v1.UpdateReasonIssued))

		done = true
		ExpectObjectReconciled(ctx, env.Client, controller, nc)
		status, reason = updating(nc)
		Expect(status).To(BeFalse())
		Expect(reason).To(Equal(v1.UpdateReasonSucceeded))
		nc = ExpectExists(ctx, env.Client, nc)
		Expect(nc.Annotations).To(HaveKey(v1.NodeClassOverrideAppliedAnnotationKey))
		Expect(cloudProvider.UpdateOperationIDs).To(HaveLen(2))
		Expect(cloudProvider.UpdateOperationIDs[0]).To(Equal(cloudProvider.UpdateOperationIDs[1]), "same operation across retries")

		By("not calling the provider again once applied")
		ExpectObjectReconciled(ctx, env.Client, controller, nc)
		Expect(cloudProvider.UpdateOperationIDs).To(HaveLen(2))
	})
	It("reverts to the NodeClass when the override is removed", func() {
		cloudProvider.UpdateFunc = func(nc *v1.NodeClaim, _ string) (bool, error) { return true, nil }
		nc := initializedNodeClaim(map[string]string{v1.NodeClassOverrideAnnotationKey: override})
		ExpectObjectReconciled(ctx, env.Client, controller, nc)
		nc = ExpectExists(ctx, env.Client, nc)
		delete(nc.Annotations, v1.NodeClassOverrideAnnotationKey)
		ExpectApplied(ctx, env.Client, nc)
		ExpectObjectReconciled(ctx, env.Client, controller, nc)
		Expect(cloudProvider.UpdateOperationIDs).To(HaveLen(2))
		Expect(cloudProvider.UpdateOperationIDs[0]).ToNot(Equal(cloudProvider.UpdateOperationIDs[1]))
		Expect(ExpectExists(ctx, env.Client, nc).Annotations).ToNot(HaveKey(v1.NodeClassOverrideAppliedAnnotationKey))
	})
	It("marks a rejected override and stops retrying it", func() {
		cloudProvider.UpdateFunc = func(*v1.NodeClaim, string) (bool, error) {
			return false, cloudprovider.NewNodeUpdateFailedError(true, errors.New("unknown device"))
		}
		nc := initializedNodeClaim(map[string]string{v1.NodeClassOverrideAnnotationKey: override})
		ExpectObjectReconciled(ctx, env.Client, controller, nc)
		status, reason := updating(nc)
		Expect(status).To(BeFalse())
		Expect(reason).To(Equal(v1.UpdateReasonRejected))
		ExpectObjectReconciled(ctx, env.Client, controller, nc)
		Expect(cloudProvider.UpdateOperationIDs).To(HaveLen(1))
	})
	It("requeues after the provider's RetryAfter when throttled", func() {
		cloudProvider.UpdateFunc = func(*v1.NodeClaim, string) (bool, error) {
			return false, cloudprovider.NewNodeUpdateThrottledError(6*time.Hour, errors.New("cooldown"))
		}
		nc := initializedNodeClaim(map[string]string{v1.NodeClassOverrideAnnotationKey: override})
		result := ExpectObjectReconciled(ctx, env.Client, controller, nc)
		Expect(result.RequeueAfter).To(Equal(6 * time.Hour))
		_, reason := updating(nc)
		Expect(reason).To(Equal(v1.UpdateReasonThrottled))
	})
	It("rejects overrides when the provider doesn't support in-place updates", func() {
		nc := initializedNodeClaim(map[string]string{v1.NodeClassOverrideAnnotationKey: override})
		ExpectObjectReconciled(ctx, env.Client, controller, nc)
		_, reason := updating(nc)
		Expect(reason).To(Equal(v1.UpdateReasonRejected))
	})
	It("waits for the NodeClaim to initialize", func() {
		nc, _ := test.NodeClaimAndNode(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1.NodeClassOverrideAnnotationKey: override}}})
		ExpectApplied(ctx, env.Client, nc)
		ExpectObjectReconciled(ctx, env.Client, controller, nc)
		Expect(cloudProvider.UpdateOperationIDs).To(BeEmpty())
	})
})
