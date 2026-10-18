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
		Sizes: map[perf.Vector]int{perf.VectorPods: 1000, perf.VectorNodePools: 1000},
		Levels: perf.Levels{
			"pods/scaleOut":        {perf.TotalTimeMinutes.Experimental(2), perf.HeapMB.Experimental(150), perf.CPUCores.Experimental(0.7)},
			"pods/consolidation":   {perf.HeapMB.Experimental(150), perf.CPUCores.Experimental(0.4)},
			"nodepools/scheduling": {perf.TotalTimeMinutes.Experimental(2), perf.HeapMB.Experimental(300)},
			"nodepools/startup":    {perf.StartupSeconds.Experimental(60), perf.HeapMB.Experimental(300)},
		},
	}
}
