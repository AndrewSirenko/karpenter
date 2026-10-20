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

package performancev2

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/suites/performancev2/env"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

// KWOK is the core KWOK provider's plugin: how to reach the controller, which NodeClass and
// NodePool to start from, how big each vector is, and the level of every phase it runs.
func KWOK(c env.Clients) *perf.Provider {
	return &perf.Provider{
		Name:       "kwok",
		Client:     c.Client,
		KubeClient: c.KubeClient,
		Controller: perf.Controller{
			Namespace:   "kube-system",
			Selector:    labels.SelectorFromSet(map[string]string{"app.kubernetes.io/name": "karpenter"}),
			MetricsPort: "8080",
		},
		NodeClass: func() client.Object { return env.KWOKNodeClass() },
		NodePool: func(nc client.Object) *v1.NodePool {
			np := env.NodePool(nc)
			test.ReplaceRequirements(np, v1.NodeSelectorRequirementWithMinValues{
				Key: v1alpha1.InstanceSizeLabelKey, Operator: corev1.NodeSelectorOpLt, Values: []string{"32"},
			})
			np.Spec.Limits = v1.Limits{}
			np.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
			np.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("30s")
			np.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
			return np
		},
		// pod_ready bounds are 1.1x the max of 3 baselines, rounded up to whole seconds (pod timestamps have
		// 1s resolution). Other bounds are 1.1x the max of 5 baseline runs on the karpenter-perf-v2 EKS cluster (tier-xl, controller
		// 8 CPU / 24Gi), upstream main 6eac93ba. Stable where CV <= 20% and a planted regression breaches
		// (see perf/regressions/README.md); everything else stays Experimental.
		Levels: perf.Levels{
			"pods/scaleOut": {perf.PodReady.P95.Stable(15), perf.PodReady.P99.Stable(16), perf.PodReady.Max.Experimental(17),
				perf.HeapMB.Stable(70.2), perf.CPUCores.Experimental(0.87)},
			"pods/consolidation": {perf.HeapMB.Experimental(84.4), perf.CPUCores.Experimental(0.24),
				perf.PodReady.P95.Experimental(3), perf.PodReady.Max.Experimental(4)},
			"nodepools/scheduling": {perf.HeapMB.Experimental(97), perf.CPUCores.Experimental(0.73),
				perf.PodReady.P95.Experimental(6), perf.PodReady.Max.Experimental(6)},
			"nodepools/startup": {perf.StartupSeconds.Experimental(23.8), perf.HeapMB.Experimental(80)},
			"topology/scaleOut": {perf.HeapMB.Experimental(250), perf.CPUCores.Experimental(1),
				perf.PodReady.P95.Experimental(120), perf.PodReady.Max.Experimental(300)},
		},
	}
}
