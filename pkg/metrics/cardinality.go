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

package metrics

import (
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"

	"sigs.k8s.io/karpenter/pkg/operator/options"
)

// HighCardinalityGauge is a gauge that stops emitting when its HighCardinalityMetric is disabled.
type HighCardinalityGauge struct {
	opmetrics.GaugeMetric
	Entry options.HighCardinalityMetric
}

// NewHighCardinalityGauge wraps g so that Set does nothing while entry is disabled.
func NewHighCardinalityGauge(entry options.HighCardinalityMetric, g opmetrics.GaugeMetric) *HighCardinalityGauge {
	return &HighCardinalityGauge{GaugeMetric: g, Entry: entry}
}

func (g *HighCardinalityGauge) Set(v float64, labels map[string]string) {
	if options.HighCardinalityMetricDisabled(g.Entry) {
		return
	}
	g.GaugeMetric.Set(v, labels)
}

func (g *HighCardinalityGauge) Describe(ch chan<- *prometheus.Desc) {
	g.GaugeMetric.(prometheus.Collector).Describe(ch)
}

func (g *HighCardinalityGauge) Collect(ch chan<- prometheus.Metric) {
	g.GaugeMetric.(prometheus.Collector).Collect(ch)
}
