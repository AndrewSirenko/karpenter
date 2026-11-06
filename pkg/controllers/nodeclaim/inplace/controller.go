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

// Package inplace is a proof of concept for designs/in-place-drainless-drift.md. It applies the
// karpenter.sh/nodeclass-override annotation to a running NodeClaim through CloudProvider.Update,
// without draining, cordoning or rebooting the node.
package inplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

const pollInterval = 15 * time.Second

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{kubeClient: kubeClient, cloudProvider: cloudProvider}
}

func (c *Controller) Name() string {
	return "nodeclaim.inplace"
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

//nolint:gocyclo
func (c *Controller) Reconcile(ctx context.Context, nc *v1.NodeClaim) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())
	if nc.Status.NodeName != "" {
		ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KRef("", nc.Status.NodeName)))
	}
	if !nodeclaimutils.IsManaged(nc, c.cloudProvider) || !nc.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	if !nc.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue() {
		return reconcile.Result{}, nil
	}
	if cond := nc.StatusConditions().Get(v1.ConditionTypeRebooting); cond != nil && cond.IsTrue() {
		return reconcile.Result{RequeueAfter: pollInterval}, nil
	}

	desired, hasDesired := nc.Annotations[v1.NodeClassOverrideAnnotationKey]
	applied, hasApplied := nc.Annotations[v1.NodeClassOverrideAppliedAnnotationKey]
	// Nothing was ever overridden, or the current override is already applied.
	if !hasDesired && !hasApplied {
		return reconcile.Result{}, nil
	}
	target := hashOf(desired)
	if hasDesired && hasApplied && applied == target {
		return reconcile.Result{}, nil
	}
	// The current target already failed or was rejected. Wait for the operator to change it.
	if cond := nc.StatusConditions().Get(v1.ConditionTypeUpdating); cond != nil && cond.IsFalse() &&
		(cond.Reason == v1.UpdateReasonFailed || cond.Reason == v1.UpdateReasonRejected) &&
		nc.Annotations[v1.UpdateOperationIDAnnotationKey] == c.operationID(nc, target) {
		return reconcile.Result{}, nil
	}

	opID := c.operationID(nc, target)
	if nc.Annotations[v1.UpdateOperationIDAnnotationKey] != opID {
		stored := nc.DeepCopy()
		if nc.Annotations == nil {
			nc.Annotations = map[string]string{}
		}
		nc.Annotations[v1.UpdateOperationIDAnnotationKey] = opID
		if err := c.kubeClient.Patch(ctx, nc, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
		msg := "applying override"
		if !hasDesired {
			msg = "reverting override to NodeClass values"
		}
		if err := c.setCondition(ctx, nc, true, v1.UpdateReasonRequested, msg); err != nil {
			return reconcile.Result{}, err
		}
		log.FromContext(ctx).Info("in-place update requested", "operationID", opID, "override", desired)
	}

	done, err := c.cloudProvider.Update(ctx, nc, opID)
	switch {
	case cloudprovider.IsNodeUpdateNotImplementedError(err):
		return reconcile.Result{}, c.setCondition(ctx, nc, false, v1.UpdateReasonRejected, err.Error())
	case cloudprovider.IsNodeUpdateFailedError(err):
		var fe *cloudprovider.NodeUpdateFailedError
		reason := v1.UpdateReasonFailed
		if ok := asFailed(err, &fe); ok && fe.Rejected {
			reason = v1.UpdateReasonRejected
		}
		log.FromContext(ctx).Error(err, "in-place update failed", "operationID", opID)
		return reconcile.Result{}, c.setCondition(ctx, nc, false, reason, err.Error())
	case cloudprovider.IsNodeUpdateThrottledError(err):
		var te *cloudprovider.NodeUpdateThrottledError
		retry := pollInterval
		if asThrottled(err, &te) && te.RetryAfter > 0 {
			retry = te.RetryAfter
		}
		return reconcile.Result{RequeueAfter: retry}, c.setCondition(ctx, nc, true, v1.UpdateReasonThrottled, err.Error())
	case err != nil:
		return reconcile.Result{}, err
	}
	if !done {
		return reconcile.Result{RequeueAfter: pollInterval}, c.setCondition(ctx, nc, true, v1.UpdateReasonIssued, "provider is applying the update")
	}

	stored := nc.DeepCopy()
	if hasDesired {
		nc.Annotations[v1.NodeClassOverrideAppliedAnnotationKey] = target
	} else {
		delete(nc.Annotations, v1.NodeClassOverrideAppliedAnnotationKey)
	}
	if err := c.kubeClient.Patch(ctx, nc, client.MergeFrom(stored)); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	log.FromContext(ctx).Info("in-place update succeeded", "operationID", opID)
	return reconcile.Result{}, c.setCondition(ctx, nc, false, v1.UpdateReasonSucceeded, "instance matches its target")
}

func (c *Controller) operationID(nc *v1.NodeClaim, target string) string {
	return fmt.Sprintf("%s-%s", nc.UID, target)
}

func (c *Controller) setCondition(ctx context.Context, nc *v1.NodeClaim, status bool, reason, msg string) error {
	cond := nc.StatusConditions().Get(v1.ConditionTypeUpdating)
	if cond != nil && cond.IsTrue() == status && cond.Reason == reason && cond.Message == msg {
		return nil
	}
	stored := nc.DeepCopy()
	if status {
		nc.StatusConditions().SetTrueWithReason(v1.ConditionTypeUpdating, reason, msg)
	} else {
		nc.StatusConditions().SetFalse(v1.ConditionTypeUpdating, reason, msg)
	}
	return client.IgnoreNotFound(c.kubeClient.Status().Patch(ctx, nc, client.MergeFrom(stored)))
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1.NodeClaim{}, builder.WithPredicates(nodeclaimutils.IsManagedPredicateFuncs(c.cloudProvider))).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func asFailed(err error, target **cloudprovider.NodeUpdateFailedError) bool {
	return errors.As(err, target)
}

func asThrottled(err error, target **cloudprovider.NodeUpdateThrottledError) bool {
	return errors.As(err, target)
}
