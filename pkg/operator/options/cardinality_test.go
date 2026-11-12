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

package options_test

import (
	"github.com/awslabs/operatorpkg/docs"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	"sigs.k8s.io/karpenter/pkg/operator/options"
)

var _ = Describe("KarpenterHighCardinalityMetrics", func() {
	It("should document every high cardinality metric", func() {
		for _, m := range options.KarpenterHighCardinalityMetrics {
			Expect(m.Name).ToNot(BeEmpty())
			Expect(m.Metrics).ToNot(BeEmpty(), m.Name)
			Expect(m.Help).ToNot(BeEmpty(), m.Name)
			Expect(m.Cardinality).ToNot(BeEmpty(), m.Name)
			Expect(m.Stage).To(BeElementOf(docs.Alpha, docs.Beta, docs.GA), m.Name)
		}
	})
	It("should have unique names", func() {
		names := lo.Map(options.KarpenterHighCardinalityMetrics, func(m options.HighCardinalityMetric, _ int) string { return m.Name })
		Expect(lo.Uniq(names)).To(HaveLen(len(names)))
	})
	It("should parse every high cardinality metric by its documented name", func() {
		for _, m := range options.KarpenterHighCardinalityMetrics {
			disabled, err := options.ParseHighCardinalityMetrics(m.Name)
			Expect(err).ToNot(HaveOccurred())
			Expect(disabled).To(Equal([]string{m.Name}))
		}
	})
})
