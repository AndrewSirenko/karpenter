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
	Help: `ALARM pods: provisioning or consolidation of N pods on one NodePool got slower or more expensive.
Size: total pods (two shapes, 900m/3100Mi and 3500m/28Gi, half each).
pods/scaleOut:
  - total_time_minutes up: the provisioner is slower end to end. If cpu_cores is flat, look at batching, NodeClaim launch or registration; if cpu_cores is up, look at Solve (pkg/controllers/provisioning/scheduling, pkg/scheduling).
  - heap_mb or cpu_cores up: more work per pod in the scheduler, or churn outside it (informers, cluster state). The report's latency_stats and counters (reconcile counts per controller) narrow it down.
pods/consolidation:
  - heap_mb or cpu_cores up: disruption evaluation (pkg/controllers/disruption, SimulateScheduling) costs more per pass.
Reproduce: go test ./test/suites/performancev2/... --ginkgo.focus=pods, then diff the report JSON against the last green main run.`,
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
	Help: `ALARM nodepools: Karpenter got slower at scheduling or starting up when the cluster has N NodePools.
Size: NodePools. All but one carry a tenant NoSchedule taint the pods don't tolerate, so every
scheduling pass has to rule out N-1 NodePools (the multi-tenant platform pattern).
nodepools/scheduling (100 pods onto the one open NodePool):
  - total_time_minutes up: per-NodePool work in NewScheduler or Solve grew (templates, instance type filtering, topology domain groups). Run BenchmarkNewScheduler/vector=nodepools to find the function.
  - heap_mb up: per-NodePool state (offerings, cost tracking, NodeOverlay store) grew.
nodepools/startup (controller restarted with N NodePools present):
  - startup_seconds up: cluster state sync, informer start or a startup-time pass over NodePools got slower. A crashloop here is an outage at scale.
  - heap_mb up: startup-time caches scale with NodePools.
Reproduce: go test ./test/suites/performancev2/... --ginkgo.focus=nodepools.`,
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
