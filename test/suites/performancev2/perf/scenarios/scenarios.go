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

	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

var All = []perf.Scenario{Pods, NodePools, Topology}

var Pods = perf.Scenario{
	Name:    "pods",
	Summary: "Provisions then consolidates N pods of two shapes (900m/3100Mi and 3500m/28Gi) on one NodePool, the baseline every change touches.",
	Vectors: map[perf.Vector]int{perf.VectorPods: 1000},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "N pending pods until all are healthy: provisioning throughput (batching, scheduler Solve, NodeClaim launch)."},
		{Name: "consolidation", Summary: "Scale to 70% until nodes stop being disrupted: disruption evaluation and SimulateScheduling cost."},
	},
	Run: func(r *perf.Run) {
		half := r.Size(perf.VectorPods) / 2
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
	Name:    "nodepools",
	Summary: "N NodePools where all but one carry a tenant NoSchedule taint the pods don't tolerate, so every pass rules out N-1 NodePools (the multi-tenant platform pattern).",
	Vectors: map[perf.Vector]int{perf.VectorNodePools: 1000, perf.VectorPods: 100},
	Sweep:   map[perf.Vector][]int{perf.VectorNodePools: {1, 10, 100, 1000, 10000}, perf.VectorPods: {100, 1000, 5000}},
	Phases: []perf.PhaseSpec{
		{Name: "scheduling", Summary: "Pods onto the one open NodePool: per-NodePool cost in NewScheduler and Solve (templates, instance type filtering, topology domain groups). Find the function with BenchmarkNewScheduler/vector=nodepools."},
		{Name: "startup", Summary: "Restart the controller with the NodePools and nodes present: startup cost (cluster state sync, offerings, cost tracking, NodeOverlay store)."},
	},
	Run: func(r *perf.Run) {
		n, pods := r.Size(perf.VectorNodePools), r.Size(perf.VectorPods)
		r.Create(r.NodeClass)
		pools := make([]client.Object, 0, n)
		for i := range n {
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
		app := r.Deployment("app", pods, "1", "2Gi")

		scheduling := r.StartPhase("scheduling")
		r.Create(app)
		scheduling.AwaitScaleOut(pods, 15*time.Minute)

		r.Restart("startup", 10*time.Minute)
	},
}

var Topology = perf.Scenario{
	Name:    "topology",
	Summary: "N pods with hard scheduling constraints: 10% hostname anti-affinity, 45% zonal topology spread, 45% hostname topology spread. Exercises topology domain tracking, the code path pods and nodepools don't touch.",
	Vectors: map[perf.Vector]int{perf.VectorPods: 1000},
	Phases: []perf.PhaseSpec{
		{Name: "scaleOut", Summary: "Constrained pods until all are healthy: topology groups, domain counting and anti-affinity search (pkg/controllers/provisioning/scheduling/topology*.go)."},
	},
	Run: func(r *perf.Run) {
		n := r.Size(perf.VectorPods)
		anti := r.Deployment("anti", n/10, "900m", "3100Mi", test.WithPodAntiAffinityHostname())
		zone := r.Deployment("zone", n*45/100, "900m", "3100Mi", test.WithZoneSpread())
		host := r.Deployment("host", n-n/10-n*45/100, "900m", "3100Mi", test.WithHostnameSpread())

		scaleOut := r.StartPhase("scaleOut")
		r.Create(r.NodeClass, r.NodePool, anti, zone, host)
		scaleOut.AwaitScaleOut(n, 20*time.Minute)
	},
}
