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

// Package parity checks Karpenter's scheduling simulation against the real
// kube-scheduler Filter plugins.
package parity

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/testing/defaults"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	schedulermetrics "k8s.io/kubernetes/pkg/scheduler/metrics"
)

// FilterPlugins are the kube-scheduler plugins checked, in the scheduler's default order.
var FilterPlugins = []string{
	names.TaintToleration,
	names.NodeAffinity,
	names.NodePorts,
	names.NodeResourcesFit,
	names.PodTopologySpread,
	names.InterPodAffinity,
}

// KubeScheduler runs kube-scheduler Filter plugins against a snapshot of nodes and bound pods.
type KubeScheduler struct {
	registry        frameworkruntime.Registry
	args            map[string]runtime.Object
	informerFactory informers.SharedInformerFactory
}

func NewKubeScheduler(informerFactory informers.SharedInformerFactory) *KubeScheduler {
	// Plugins record into scheduler metrics, which are nil until registered.
	schedulermetrics.Register()
	args := map[string]runtime.Object{}
	for _, pc := range defaults.PluginConfigsV1 {
		args[pc.Name] = pc.Args
	}
	return &KubeScheduler{
		registry:        plugins.NewInTreeRegistry(),
		args:            args,
		informerFactory: informerFactory,
	}
}

// Rejection names the plugin that rejected a pod and why.
type Rejection struct {
	Plugin string
	Reason string
}

func (r *Rejection) String() string {
	return fmt.Sprintf("%s: %s", r.Plugin, r.Reason)
}

// Filter runs PreFilter and Filter for pod against nodeName, with boundPods already placed.
// boundPods must not contain pod. It returns nil if every plugin accepts the node.
func (k *KubeScheduler) Filter(ctx context.Context, pod *corev1.Pod, nodeName string, nodes []*corev1.Node, boundPods []*corev1.Pod) (*Rejection, error) {
	snapshot := cache.NewSnapshot(boundPods, nodes)
	fh, err := frameworkruntime.NewFramework(ctx, nil, nil,
		frameworkruntime.WithSnapshotSharedLister(snapshot),
		frameworkruntime.WithInformerFactory(k.informerFactory))
	if err != nil {
		return nil, err
	}
	nodeInfos, err := snapshot.NodeInfos().List()
	if err != nil {
		return nil, err
	}
	nodeInfo, err := snapshot.NodeInfos().Get(nodeName)
	if err != nil {
		return nil, err
	}
	state := framework.NewCycleState()
	for _, name := range FilterPlugins {
		p, err := k.registry[name](ctx, k.args[name], fh)
		if err != nil {
			return nil, fmt.Errorf("creating %s, %w", name, err)
		}
		if pre, ok := p.(fwk.PreFilterPlugin); ok {
			_, status := pre.PreFilter(ctx, state, pod, nodeInfos)
			if status.IsSkip() {
				continue
			}
			if !status.IsSuccess() {
				return &Rejection{Plugin: name, Reason: "PreFilter: " + strings.Join(status.Reasons(), "; ")}, nil
			}
		}
		if f, ok := p.(fwk.FilterPlugin); ok {
			if status := f.Filter(ctx, state, pod, nodeInfo); !status.IsSuccess() {
				return &Rejection{Plugin: name, Reason: strings.Join(status.Reasons(), "; ")}, nil
			}
		}
	}
	return nil, nil
}
