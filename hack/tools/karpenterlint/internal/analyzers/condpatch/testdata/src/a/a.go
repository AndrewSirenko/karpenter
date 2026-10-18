package a

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Shape of kubernetes-sigs/karpenter#3360.
func terminal(ctx context.Context, c client.Client, nodeClaim *v1.NodeClaim) error {
	stored := nodeClaim.DeepCopy()
	nodeClaim.StatusConditions().SetFalse("Rebooting", "Succeeded", "")
	return c.Status().Patch(ctx, nodeClaim, client.MergeFrom(stored)) // want `patched with client.MergeFrom`
}

func inClosure(ctx context.Context, c client.Client, nodeClaim *v1.NodeClaim) error {
	return retry(func() error {
		stored := nodeClaim.DeepCopy()
		nodeClaim.StatusConditions().SetTrue("Disrupted")
		return c.Status().Patch(ctx, nodeClaim, client.MergeFrom(stored)) // want `patched with client.MergeFrom`
	})
}

func fixed(ctx context.Context, c client.Client, nodeClaim *v1.NodeClaim) error {
	stored := nodeClaim.DeepCopy()
	nodeClaim.StatusConditions().SetFalse("Rebooting", "Succeeded", "")
	return c.Status().Patch(ctx, nodeClaim, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{}))
}

// Metadata-only patches don't touch the conditions list.
func labelsOnly(ctx context.Context, c client.Client, nodeClaim *v1.NodeClaim) error {
	stored := nodeClaim.DeepCopy()
	nodeClaim.Name = "x"
	return c.Patch(ctx, nodeClaim, client.MergeFrom(stored))
}

// Conditions are set on a different object than the one patched.
func otherObject(ctx context.Context, c client.Client, nodeClaim, other *v1.NodeClaim) error {
	stored := other.DeepCopy()
	nodeClaim.StatusConditions().SetTrue("Ready")
	return c.Patch(ctx, other, client.MergeFrom(stored))
}

func retry(f func() error) error { return f() }
