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

package disruptionbench_test

import (
	"bytes"
	"encoding/csv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/karpenter/pkg/test/disruptionbench"
)

var _ = Describe("Replay", func() {
	It("should apply commands until none is left", func() {
		f, err := disruptionbench.Generate(smallSpec(1))
		Expect(err).ToNot(HaveOccurred())
		env := mustEnv(f)
		curve, err := disruptionbench.Replay(ctx, env, todayMethods(env), disruptionbench.ReplayOptions{MaxWall: 5 * time.Minute, Logf: GinkgoWriter.Printf})
		Expect(err).ToNot(HaveOccurred())
		Expect(curve.StopReason).To(Equal("no command"))
		Expect(len(curve.Points)).To(BeNumerically(">", 1))
		for i, p := range curve.Points[1:] {
			Expect(p.CostPerHour).To(BeNumerically("<", curve.Points[i].CostPerHour))
			Expect(p.Sims).To(BeNumerically(">=", curve.Points[i].Sims))
		}
		last := curve.Points[len(curve.Points)-1]
		Expect(last.Sims).To(BeNumerically(">", 0))
		Expect(last.PlantedCaptured).To(BeNumerically("<=", curve.PlantedTotal+tolerance))

		pods := &corev1.PodList{}
		Expect(env.Client.List(ctx, pods)).To(Succeed())
		for _, p := range pods.Items {
			Expect(p.Spec.NodeName).ToNot(BeEmpty(), "pod %s", p.Name)
		}
		var buf bytes.Buffer
		Expect(curve.WriteCSV(&buf)).To(Succeed())
		rows, err := csv.NewReader(&buf).ReadAll()
		Expect(err).ToNot(HaveOccurred())
		Expect(rows).To(HaveLen(len(curve.Points) + 1))
		GinkgoWriter.Print(curve.Summary(0).String())
	})
})
