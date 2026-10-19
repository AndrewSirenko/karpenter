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
	for id, gates := range p.Levels {
		bound := map[string]float64{}
		for _, g := range gates {
			bound[g.Metric.Name] = g.Bound
		}
		if _, ok := bound[TotalTimeSeconds.Name]; ok {
			if _, ok := bound[PodReady.Max.Name]; ok {
				errs = append(errs, fmt.Sprintf("provider %s gates %s on both %s and %s, which measure nearly the same thing; keep one", p.Name, id, TotalTimeSeconds.Name, PodReady.Max.Name))
			}
		}
		last, lastName := 0.0, ""
		for _, m := range []Metric{PodReady.P50, PodReady.P95, PodReady.P99, PodReady.Max} {
			if b, ok := bound[m.Name]; ok {
				if b < last {
					errs = append(errs, fmt.Sprintf("provider %s sets %s %s < %g below %s < %g; quantile bounds must not decrease", p.Name, id, m.Name, b, lastName, last))
				}
				last, lastName = b, m.Name
			}
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
	fmt.Fprintf(&b, "ALARM %s on %s (%s)\n%s\n%s\n", id, r.Provider.Name, sizesLabel(r.Sizes, "=", " "), r.Scenario.Summary, ph.Summary)
	for _, g := range breached {
		m := metricByName(g.Metric)
		fmt.Fprintf(&b, "\n%s\n  measures: %s\n  check: %s\n", g, m.Measures, m.OnBreach)
	}
	fmt.Fprintf(&b, "\nreproduce: go test ./test/suites/performancev2/... --ginkgo.focus=%s, then diff the report JSON against the last green main run\n", r.Scenario.Name)
	return b.String()
}

func (p *Provider) sizes(s *Scenario) map[Vector]int {
	out := map[Vector]int{}
	for v, def := range s.Vectors {
		out[v] = p.size(s.Name, v, def)
	}
	return out
}

func cells(grid map[Vector][]int) []map[Vector]int {
	out := []map[Vector]int{{}}
	keys := make([]string, 0, len(grid))
	for v := range grid {
		keys = append(keys, string(v))
	}
	sort.Strings(keys)
	for _, k := range keys {
		var next []map[Vector]int
		for _, c := range out {
			for _, n := range grid[Vector(k)] {
				cc := map[Vector]int{Vector(k): n}
				for kk, vv := range c {
					cc[kk] = vv
				}
				next = append(next, cc)
			}
		}
		out = next
	}
	return out
}

// Inventory renders what a provider runs: every scenario and phase with its level per metric, the sweep
// grids, and what each metric means. It is generated from the same values the suite uses.
func Inventory(p *Provider, scenarios []Scenario) (string, error) {
	if err := Validate(p, scenarios); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Provider %s\n\n| scenario | sizes | phase | metric | level | bound |\n|---|---|---|---|---|---|\n", p.Name)
	for i := range scenarios {
		s := &scenarios[i]
		sz := sizesLabel(p.sizes(s), "=", " ")
		for _, ph := range s.Phases {
			gates, ok := p.Levels[s.Name+"/"+ph.Name]
			switch {
			case !ok:
				fmt.Fprintf(&b, "| %s | %s | %s | - | off | - |\n", s.Name, sz, ph.Name)
			case len(gates) == 0:
				fmt.Fprintf(&b, "| %s | %s | %s | - | measured only | - |\n", s.Name, sz, ph.Name)
			}
			for _, g := range gates {
				op := ">"
				if g.Metric.Upper {
					op = "<"
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s %g %s |\n", s.Name, sz, ph.Name, g.Metric.Name, g.Tier, op, g.Bound, g.Metric.Unit)
			}
		}
	}
	b.WriteString("\n## Pod latency by quantile (pod_ready_seconds)\n\n| scenario/phase | p50 | p95 | p99 | max (p100) |\n|---|---|---|---|---|\n")
	for i := range scenarios {
		s := &scenarios[i]
		for _, ph := range s.Phases {
			gates, ok := p.Levels[s.Name+"/"+ph.Name]
			if !ok {
				continue
			}
			cell := func(m Metric) string {
				for _, g := range gates {
					if g.Metric.Name == m.Name {
						return fmt.Sprintf("%s < %g %s", g.Tier, g.Bound, m.Unit)
					}
				}
				return "-"
			}
			row := []string{cell(PodReady.P50), cell(PodReady.P95), cell(PodReady.P99), cell(PodReady.Max)}
			if strings.Join(row, "") == "----" {
				continue
			}
			fmt.Fprintf(&b, "| %s/%s | %s |\n", s.Name, ph.Name, strings.Join(row, " | "))
		}
	}
	b.WriteString("\n## Sweeps (measured only, never gate; run with KARPENTER_PERF_SWEEP=<scenario>)\n\n| scenario | grid | cells |\n|---|---|---|\n")
	for i := range scenarios {
		s := &scenarios[i]
		if len(s.Sweep) == 0 {
			continue
		}
		keys := make([]string, 0, len(s.Sweep))
		for v := range s.Sweep {
			keys = append(keys, string(v))
		}
		sort.Strings(keys)
		axes := make([]string, 0, len(keys))
		for _, k := range keys {
			axes = append(axes, fmt.Sprintf("%s %v", k, s.Sweep[Vector(k)]))
		}
		fmt.Fprintf(&b, "| %s | %s | %d |\n", s.Name, strings.Join(axes, " x "), len(cells(s.Sweep)))
	}
	b.WriteString("\n## Scenarios\n\n")
	for i := range scenarios {
		s := &scenarios[i]
		fmt.Fprintf(&b, "- **%s** (%s): %s\n", s.Name, sizesLabel(s.Vectors, "=", " "), s.Summary)
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
