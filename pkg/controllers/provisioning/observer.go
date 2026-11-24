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

package provisioning

import (
	"context"

	"github.com/awslabs/operatorpkg/option"

	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
)

// SchedulingObserver is told about each provisioning decision, for example to record it outside the cluster.
type SchedulingObserver interface {
	// OnScheduled is called once per provisioning solve, after its NodeClaims are created. nodeClaimNames[i] is the
	// name of the NodeClaim created for results.NewNodeClaims[i], or "" if creating it failed. It must not block.
	OnScheduled(ctx context.Context, results scheduler.Results, nodeClaimNames []string)
}

type ProvisionerOptions struct {
	observer SchedulingObserver
}

// WithSchedulingObserver registers an observer of provisioning decisions.
func WithSchedulingObserver(observer SchedulingObserver) option.Function[ProvisionerOptions] {
	return func(o *ProvisionerOptions) {
		o.observer = observer
	}
}

func (p *Provisioner) observeScheduled(ctx context.Context, results scheduler.Results, nodeClaimNames []string) {
	if p.observer == nil || results.ID == "" {
		return
	}
	p.observer.OnScheduled(ctx, results, nodeClaimNames)
}
