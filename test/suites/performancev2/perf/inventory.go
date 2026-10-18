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
	"fmt"
	"sort"
	"strings"
)

func (s *Scenario) phase(name string) (PhaseSpec, bool) {
	for _, p := range s.Phases {
		if p.Name == name {
			return p, true
		}
	}
	return PhaseSpec{}, false
}

// Validate checks that every level a provider sets names a declared scenario phase.
func Validate(p *Provider, scenarios []Scenario) error {
	var errs []string
	for id := range p.Levels {
		name, phase, _ := strings.Cut(id, "/")
		found := false
		for i := range scenarios {
			if scenarios[i].Name == name {
				_, found = scenarios[i].phase(phase)
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf("provider %s sets levels for %q, which no scenario declares", p.Name, id))
		}
	}
	sort.Strings(errs)
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func metricByName(name string) Metric {
	for _, m := range Metrics {
		if m.Name == name {
			return m
		}
	}
	return Metric{Name: name}
}

func (r *Run) alarm(id string, breached []GateResult) string {
	_, name, _ := strings.Cut(id, "/")
	ph, _ := r.Scenario.phase(name)
	var b strings.Builder
	fmt.Fprintf(&b, "ALARM %s on %s (size %d)\n%s\n%s\n", id, r.Provider.Name, r.Size, r.Scenario.Summary, ph.Summary)
	for _, g := range breached {
		m := metricByName(g.Metric)
		fmt.Fprintf(&b, "\n%s\n  measures: %s\n  check: %s\n", g, m.Measures, m.OnBreach)
	}
	fmt.Fprintf(&b, "\nreproduce: go test ./test/suites/performancev2/... --ginkgo.focus=%s, then diff the report JSON against the last green main run\n", r.Scenario.Name)
	return b.String()
}

// Inventory renders what a provider runs: every scenario and phase with its level per metric, plus what
// each metric means. It is generated from the same values the suite uses.
func Inventory(p *Provider, scenarios []Scenario) (string, error) {
	if err := Validate(p, scenarios); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Provider %s\n\n| scenario | vector | size | phase | metric | level | bound |\n|---|---|---|---|---|---|---|\n", p.Name)
	for i := range scenarios {
		s := &scenarios[i]
		for _, ph := range s.Phases {
			gates, ok := p.Levels[s.Name+"/"+ph.Name]
			switch {
			case !ok:
				fmt.Fprintf(&b, "| %s | %s | %d | %s | - | off | - |\n", s.Name, s.Vector, p.size(s.Vector, s.DefaultSize), ph.Name)
			case len(gates) == 0:
				fmt.Fprintf(&b, "| %s | %s | %d | %s | - | measured only | - |\n", s.Name, s.Vector, p.size(s.Vector, s.DefaultSize), ph.Name)
			}
			for _, g := range gates {
				op := ">"
				if g.Metric.Upper {
					op = "<"
				}
				fmt.Fprintf(&b, "| %s | %s | %d | %s | %s | %s | %s %g %s |\n", s.Name, s.Vector, p.size(s.Vector, s.DefaultSize), ph.Name, g.Metric.Name, g.Tier, op, g.Bound, g.Metric.Unit)
			}
		}
	}
	b.WriteString("\n## Scenarios\n\n")
	for i := range scenarios {
		s := &scenarios[i]
		fmt.Fprintf(&b, "- **%s** (vector %s): %s\n", s.Name, s.Vector, s.Summary)
		for _, ph := range s.Phases {
			fmt.Fprintf(&b, "  - `%s/%s`: %s\n", s.Name, ph.Name, ph.Summary)
		}
	}
	b.WriteString("\n## Metrics\n\n| metric | unit | breach when | measures | check on breach |\n|---|---|---|---|---|\n")
	for _, m := range Metrics {
		dir := "below bound"
		if m.Upper {
			dir = "above bound"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", m.Name, m.Unit, dir, m.Measures, m.OnBreach)
	}
	return b.String(), nil
}
