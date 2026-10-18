package a

import (
	"slices"
	"sort"
	"strings"
)

type Requirement struct{ values []string }

// Shape of kubernetes-sigs/karpenter#3104.
func NewRequirement(key string, values ...string) *Requirement {
	for i, v := range values {
		values[i] = strings.ToLower(v) // want `NewRequirement writes into its variadic parameter values`
	}
	return &Requirement{values: values}
}

func NewRequirementFixed(key string, values ...string) *Requirement {
	values = slices.Clone(values)
	for i, v := range values {
		values[i] = strings.ToLower(v)
	}
	return &Requirement{values: values}
}

func sortedNames(names []string) []string {
	sort.Strings(names) // want `sortedNames sorts its slice parameter names in place`
	return names
}

func sortedNamesFixed(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}

func record(ports map[string]int, key string) {
	ports[key]++
}

// fill writes defaults into labels in place.
func fill(labels map[string]string) {
	labels["a"] = "b"
}

func (r *Requirement) normalize() {
	for i := range r.values {
		r.values[i] = strings.ToLower(r.values[i])
	}
}

func readOnly(values []string) string {
	return values[0]
}
