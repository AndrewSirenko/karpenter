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

package disruption

import (
	"context"

	"github.com/awslabs/operatorpkg/option"
)

func WithoutCapacityBound() option.Function[MethodOptions] {
	return func(o *MethodOptions) { o.skipCapacityBound = true }
}

func (m *MultiNodeConsolidation) MaxFittingPrefix(ctx context.Context, candidates ...*Candidate) int {
	return m.maxFittingPrefix(ctx, candidates)
}
