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

package options

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/awslabs/operatorpkg/docs"
	"github.com/samber/lo"
)

// HighCardinalityMetric is a metric, or a dimension of one, whose series count grows with cluster size. Each one can
// be turned off by naming it in --disable-high-cardinality-metrics. Everything is emitted by default.
type HighCardinalityMetric struct {
	// Name is the value that turns this entry off in --disable-high-cardinality-metrics, e.g. "PodName".
	Name string
	// Metrics are the metrics whose emission changes when this entry is turned off.
	Metrics []string
	// Help is what changes when this entry is turned off.
	Help string
	// Cardinality justifies the entry with its measured or worst-case series count.
	Cardinality string
	// Stage is the stability of turning this entry off.
	Stage docs.Stage
}

var PodNameHighCardinalityMetric = HighCardinalityMetric{
	Name: "PodName",
	Metrics: []string{
		"karpenter_pods_state",
		"karpenter_pods_unstarted_time_seconds",
		"karpenter_pods_unbound_time_seconds",
		"karpenter_pods_provisioning_unstarted_time_seconds",
		"karpenter_pods_provisioning_unbound_time_seconds",
		"karpenter_pods_provisioning_scheduling_undecided_time_seconds",
	},
	Help: "karpenter_pods_state sets the pod name dimension to empty and counts the pods that share each remaining " +
		"label set. The per-pod karpenter_pods_*_time_seconds gauges are not emitted. The pod duration histograms are unchanged.",
	Cardinality: "One series per pod. 20,273 karpenter_pods_* series at 20k pods, 491 with PodName turned off.",
	Stage:       docs.Alpha,
}

// KarpenterHighCardinalityMetrics are the entries --disable-high-cardinality-metrics accepts. Cloud providers append
// their own. Entries are never removed, so that a configured value keeps parsing; a retired entry becomes a no-op.
var KarpenterHighCardinalityMetrics = []HighCardinalityMetric{
	PodNameHighCardinalityMetric,
}

var disabledHighCardinalityMetrics atomic.Pointer[[]string]

// SetDisabledHighCardinalityMetrics records which entries are disabled. Metrics are package globals created before
// options are parsed and Set takes no context, so Parse records --disable-high-cardinality-metrics here.
func SetDisabledHighCardinalityMetrics(names []string) {
	disabledHighCardinalityMetrics.Store(&names)
}

// HighCardinalityMetricDisabled returns whether m was named in --disable-high-cardinality-metrics.
func HighCardinalityMetricDisabled(m HighCardinalityMetric) bool {
	names := disabledHighCardinalityMetrics.Load()
	return names != nil && lo.Contains(*names, m.Name)
}

// ParseHighCardinalityMetrics parses a comma-separated list of HighCardinalityMetric names, e.g. "PodName".
func ParseHighCardinalityMetrics(raw string) ([]string, error) {
	names := lo.Map(KarpenterHighCardinalityMetrics, func(m HighCardinalityMetric, _ int) string { return m.Name })
	disabled := []string{}
	for name := range strings.SplitSeq(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !lo.Contains(names, name) {
			return nil, fmt.Errorf("unknown high cardinality metric %q, must be one of %s", name, strings.Join(names, ", "))
		}
		disabled = append(disabled, name)
	}
	return lo.Uniq(disabled), nil
}

func highCardinalityMetricsHelp() string {
	names := lo.Map(KarpenterHighCardinalityMetrics, func(m HighCardinalityMetric, _ int) string { return m.Name })
	return fmt.Sprintf("Comma-separated high cardinality metrics to stop emitting. Empty emits everything. Current options are: %s.",
		strings.Join(names, ", "))
}
