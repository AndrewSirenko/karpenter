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

package scenarios

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

var All = []perf.Scenario{Pods, NodePools}

var Pods = perf.Scenario{
	Name:        "pods",
	Vector:      perf.VectorPods,
	DefaultSize: 1000,
	Summary:     "Provisions then consolidates N pods of two shapes (900m/3100Mi and 3500m/28Gi) on one NodePool, the baseline every change touches.",
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "N pending pods until all are healthy: provisioning throughput (batching, scheduler Solve, NodeClaim launch)."},
		{Name: "consolidation", Summary: "Scale to 70% until nodes stop being disrupted: disruption evaluation and SimulateScheduling cost."},
	},
	Run: func(r *perf.Run) {
		half := r.Size / 2
		small := r.Deployment("small", half, "900m", "3100Mi")
		large := r.Deployment("large", half, "3500m", "28Gi")

		scaleOut := r.StartPhase("scaleOut")
		r.Create(r.NodeClass, r.NodePool, small, large)
		scaleOut.AwaitScaleOut(2*half, 15*time.Minute)

		consolidation := r.StartPhase("consolidation")
		r.Scale(half*7/10, small, large)
		consolidation.AwaitConsolidation(2*half, 2*(half*7/10), 20*time.Minute)
	},
}

const tenantTaint = "perf.karpenter.sh/tenant"

var NodePools = perf.Scenario{
	Name:        "nodepools",
	Vector:      perf.VectorNodePools,
	DefaultSize: 1000,
	Summary:     "N NodePools where all but one carry a tenant NoSchedule taint the pods don't tolerate, so every pass rules out N-1 NodePools (the multi-tenant platform pattern).",
	Phases: []perf.PhaseSpec{
		{Name: "scheduling", Summary: "100 pods onto the one open NodePool: per-NodePool cost in NewScheduler and Solve (templates, instance type filtering, topology domain groups). Find the function with BenchmarkNewScheduler/vector=nodepools."},
		{Name: "startup", Summary: "Restart the controller with N NodePools present: per-NodePool cost at startup (cluster state sync, offerings, cost tracking, NodeOverlay store)."},
	},
	Run: func(r *perf.Run) {
		r.Create(r.NodeClass)
		pools := make([]client.Object, 0, r.Size)
		for i := range r.Size {
			np := r.NodePool.DeepCopy()
			np.Name = fmt.Sprintf("%s-%d", r.NodePool.Name, i)
			if i > 0 {
				np.Spec.Template.Spec.Taints = append(np.Spec.Template.Spec.Taints, corev1.Taint{
					Key: tenantTaint, Value: fmt.Sprint(i), Effect: corev1.TaintEffectNoSchedule,
				})
			}
			pools = append(pools, np)
		}
		r.CreateParallel(pools)
		app := r.Deployment("app", 100, "1", "2Gi")

		scheduling := r.StartPhase("scheduling")
		r.Create(app)
		scheduling.AwaitScaleOut(100, 10*time.Minute)

		r.Restart("startup", 10*time.Minute)
	},
}
