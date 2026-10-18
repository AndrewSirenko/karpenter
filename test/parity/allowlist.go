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

package parity

import "strings"

// Match selects discrepancies by class, plugin and a substring of the reason.
type Match struct {
	Class          Class
	Plugin         string
	ReasonContains string
}

func (m Match) Matches(d Discrepancy) bool {
	return m.Class == d.Class && (m.Plugin == "" || m.Plugin == d.Plugin) && strings.Contains(d.Reason, m.ReasonContains)
}

// AllowEntry is a reviewed, intentional divergence from kube-scheduler. Allowed discrepancies are dropped.
type AllowEntry struct {
	Match
	Why string
}

// KnownIssue is a divergence tracked by an open issue. It's reported but doesn't fail the run.
type KnownIssue struct {
	Match
	Issue string
}

var Allowlist = []AllowEntry{
	{
		Match: Match{Class: MissedExisting, ReasonContains: "node fails only a preference"},
		Why: "Karpenter treats preferred node affinity, preferred pod (anti-)affinity and ScheduleAnyway spread as required " +
			"until they can't be met, so it launches a node that satisfies a preference instead of using an existing node that doesn't. " +
			"https://karpenter.sh/docs/concepts/scheduling/#preferences",
	},
}

var KnownIssues = []KnownIssue{
	{
		Match: Match{Class: LaunchedUnusable, Plugin: "NodeAffinity", ReasonContains: "[conflicting requirements on one key]"},
		Issue: "#3423 #3425",
	},
}

// Allowed returns the allowlist reason for d, if any.
func Allowed(d Discrepancy) (string, bool) {
	for _, e := range Allowlist {
		if e.Matches(d) {
			return e.Why, true
		}
	}
	return "", false
}

// Known returns the tracking issue for d, if any.
func Known(d Discrepancy) (string, bool) {
	for _, k := range KnownIssues {
		if k.Matches(d) {
			return k.Issue, true
		}
	}
	return "", false
}
