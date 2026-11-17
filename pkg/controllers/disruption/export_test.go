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

// SetProbesBeforeTimeout makes each multi-node search time out after n probes. Zero disables it.
func (m *MultiNodeConsolidation) SetProbesBeforeTimeout(n int) {
	m.probesBeforeTimeout = n
}

// PendingSearch returns the batch sizes a timed-out multi-node search will continue with.
func (m *MultiNodeConsolidation) PendingSearch() (minBatch, maxBatch int, ok bool) {
	if m.search == nil {
		return 0, 0, false
	}
	return m.search.min + 1, m.search.max + 1, true
}
