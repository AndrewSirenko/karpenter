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
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

func randomExistingNodes(r *rand.Rand, n int) []*ExistingNode {
	nodes := make([]*ExistingNode, 0, n)
	for i := range n {
		name := fmt.Sprintf("node-%03d", r.Intn(n/2+1))
		sn := state.NewNode()
		labels := map[string]string{}
		if r.Intn(2) == 0 {
			labels[v1.NodeRegisteredLabelKey] = "true"
		}
		if r.Intn(2) == 0 {
			labels[v1.NodeInitializedLabelKey] = "true"
		}
		switch r.Intn(4) {
		case 0:
			sn.Node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
		case 1:
			sn.NodeClaim = &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("claim-%03d", i%7)}}
		default:
			sn.Node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
			sn.NodeClaim = &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("claim-%03d", r.Intn(n))}}
		}
		nodes = append(nodes, &ExistingNode{StateNode: sn})
	}
	return nodes
}

var _ = Describe("sortExistingNodes", func() {
	It("should order nodes exactly as a stable sort on Initialized() then Name()", func() {
		for seed := range int64(50) {
			r := rand.New(rand.NewSource(seed))
			nodes := randomExistingNodes(r, 1+r.Intn(400))

			expected := append([]*ExistingNode(nil), nodes...)
			sort.SliceStable(expected, func(i, j int) bool {
				if expected[i].Initialized() && !expected[j].Initialized() {
					return true
				}
				if !expected[i].Initialized() && expected[j].Initialized() {
					return false
				}
				return expected[i].Name() < expected[j].Name()
			})

			s := &Scheduler{existingNodes: append([]*ExistingNode(nil), nodes...)}
			s.sortExistingNodes()
			Expect(s.existingNodes).To(HaveLen(len(expected)))
			for i := range expected {
				Expect(s.existingNodes[i]).To(BeIdenticalTo(expected[i]), "seed %d, index %d", seed, i)
			}
		}
	})
})
