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

import "context"

// SetSimulateFromSnapshot switches SimulateScheduling between the shared snapshot and per-call deep copies, and
// returns a func that restores the previous mode.
func SetSimulateFromSnapshot(v bool) func() {
	prev := simulateFromSnapshot
	simulateFromSnapshot = v
	return func() { simulateFromSnapshot = prev }
}

// ComputeSorted runs single-node consolidation over candidates already in SortCandidates order.
func (s *SingleNodeConsolidation) ComputeSorted(ctx context.Context, budgets map[string]int, candidates []*Candidate) ([]Command, error) {
	return s.computeSorted(ctx, budgets, candidates)
}
