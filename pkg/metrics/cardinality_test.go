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

package metrics_test

import (
	opmetrics "github.com/awslabs/operatorpkg/metrics"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("HighCardinalityGauge", func() {
	var gauge *metrics.HighCardinalityGauge
	labels := map[string]string{"label_1": "a"}

	BeforeEach(func() {
		if gauge == nil {
			gauge = metrics.NewHighCardinalityGauge(options.PodNameHighCardinalityMetric,
				opmetrics.NewPrometheusGauge(crmetrics.Registry, prometheus.GaugeOpts{Name: "test_high_cardinality_gauge"}, []opmetrics.Label{{Name: "label_1"}}, opmetrics.Alpha))
		}
		gauge.Reset()
	})
	AfterEach(func() {
		options.SetDisabledHighCardinalityMetrics(nil)
	})
	It("should emit by default", func() {
		gauge.Set(1, labels)
		ExpectMetricGaugeValue(gauge, 1, labels)
	})
	It("should not emit while its entry is disabled", func() {
		options.SetDisabledHighCardinalityMetrics([]string{options.PodNameHighCardinalityMetric.Name})
		gauge.Set(1, labels)
		_, found := FindMetricWithLabelValues("test_high_cardinality_gauge", labels)
		Expect(found).To(BeFalse())
	})
	It("should emit while only other entries are disabled", func() {
		options.SetDisabledHighCardinalityMetrics([]string{"SomethingElse"})
		gauge.Set(1, labels)
		ExpectMetricGaugeValue(gauge, 1, labels)
	})
	It("should still delete while its entry is disabled", func() {
		gauge.Set(1, labels)
		options.SetDisabledHighCardinalityMetrics([]string{options.PodNameHighCardinalityMetric.Name})
		gauge.Delete(labels)
		_, found := FindMetricWithLabelValues("test_high_cardinality_gauge", labels)
		Expect(found).To(BeFalse())
	})
})
