//go:build test_performance

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

package scheduling

import (
	"fmt"
	"math/rand"
	"testing"
)

func BenchmarkSortExistingNodes(b *testing.B) {
	for _, n := range []int{1000, 5000, 10000} {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			nodes := randomExistingNodes(rand.New(rand.NewSource(1)), n)
			s := &Scheduler{existingNodes: make([]*ExistingNode, n)}
			b.ReportAllocs()
			for b.Loop() {
				copy(s.existingNodes, nodes)
				s.sortExistingNodes()
			}
		})
	}
}
