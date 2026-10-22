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
	_ "embed"

	"sigs.k8s.io/karpenter/test/suites/performancev2/env"
	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

//go:embed calibration/kwok-ci.json
var kwokCICalibration []byte

// KWOKCI is the core KWOK provider on a GitHub Actions kind runner, for the per-PR smoke test and the
// periodics. Runners are small and noisy, so sizes are small and only gross regressions gate.
func KWOKCI(c env.Clients) *perf.Provider {
	p := KWOK(c)
	p.Name = "kwok-ci"
	p.Summary = "Per-PR smoke and periodics on a GitHub Actions kind runner. Stable only for gross regressions."
	p.Sizes = map[string]map[perf.Vector]int{
		"pods":      {perf.VectorPods: 300},
		"nodepools": {perf.VectorNodePools: 100, perf.VectorPods: 100},
		"topology":  {perf.VectorPods: 300},
	}
	p.Calibration = perf.MustLoadCalibration(kwokCICalibration)
	p.Levels = perf.Levels{
		"pods/scaleOut":        {},
		"pods/consolidation":   {},
		"nodepools/scheduling": {},
		"nodepools/startup":    {},
		"topology/scaleOut":    {},
	}
	return p
}
