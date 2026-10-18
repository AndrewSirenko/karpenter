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

// Package metrichelp flags Prometheus Help strings that list a metric's
// labels in prose ("Labeled by ..."). Dimensions are documented through
// opmetrics.Label, which the docs generator renders; the prose copy drifts
// from the real label set.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3295: metric dimensions became documented
//     through built-in Label help and values, leaving the "Labeled by" prose
//     redundant. 32 Help strings still repeat it and 5 of them are wrong,
//     e.g. queue_failures_total claims "disruption method" but is labeled by
//     decision, reason and consolidation_type.
package metrichelp

import (
	"go/ast"
	"go/constant"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const prometheusPath = "github.com/prometheus/client_golang/prometheus"

var Analyzer = &analysis.Analyzer{
	Name:     "metrichelp",
	Doc:      "flags Prometheus Help strings that describe labels in prose (\"Labeled by ...\") instead of relying on opmetrics.Label docs (#3295)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

var opts = map[string]bool{"CounterOpts": true, "GaugeOpts": true, "HistogramOpts": true, "SummaryOpts": true}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.Preorder([]ast.Node{(*ast.CompositeLit)(nil)}, func(n ast.Node) {
		lit := n.(*ast.CompositeLit)
		named, ok := types.Unalias(pass.TypesInfo.TypeOf(lit)).(*types.Named)
		if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != prometheusPath || !opts[named.Obj().Name()] {
			return
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if id, ok := kv.Key.(*ast.Ident); !ok || id.Name != "Help" {
				continue
			}
			tv := pass.TypesInfo.Types[kv.Value]
			if tv.Value == nil || tv.Value.Kind() != constant.String {
				continue
			}
			if strings.Contains(strings.ToLower(constant.StringVal(tv.Value)), "labeled by") {
				pass.Reportf(kv.Value.Pos(), "Help lists labels in prose; document dimensions with opmetrics.Label Help/Values instead (#3295)")
			}
		}
	})
	return nil, nil
}
