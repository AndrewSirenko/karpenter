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

package perf

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Suite registers each scenario as a Ginkgo spec for the provider. Specs for scenarios the
// provider has no levels for are skipped, so a provider opts in phase by phase.
func Suite(provider func() *Provider, scenarios ...Scenario) bool {
	return Describe("Performance", func() {
		for i := range scenarios {
			s := &scenarios[i]
			Context(s.Name, Label("perf", "vector:"+string(s.Vector)), func() {
				It("should hold its performance levels", func(ctx context.Context) {
					p := provider()
					Expect(Validate(p, scenarios)).To(Succeed())
					if !p.enabled(s) {
						Skip("scenario " + s.Name + " is off for provider " + p.Name)
					}
					r := newRun(ctx, p, s)
					r.expectClean()
					DeferCleanup(func(ctx SpecContext) { r.cleanup(ctx) }, NodeTimeout(20*time.Minute))
					s.Run(r)
				})
			})
		}
	})
}

func (p *Provider) enabled(s *Scenario) bool {
	for id := range p.Levels {
		if strings.HasPrefix(id, s.Name+"/") {
			return true
		}
	}
	return false
}
