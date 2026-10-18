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

// Package eventdedupe requires every Karpenter events.Event literal to set
// DedupeValues. Without it the recorder dedupes only on reason and message,
// so per-object events either collapse together or flood the API server when
// the message embeds changing text.
//
// Inspired by review feedback on event volume and duplication:
//   - kubernetes-sigs/karpenter#2962: "concerned about the volume of events here".
//   - kubernetes-sigs/karpenter#3311: "Does this end up emitting 3 identical events?".
//   - kubernetes-sigs/karpenter#3367: "Should we log\event here?".
//   - kubernetes-sigs/karpenter#3028: a missing Unconsolidatable event.
package eventdedupe

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const eventsPath = "sigs.k8s.io/karpenter/pkg/events"

var Analyzer = &analysis.Analyzer{
	Name:     "eventdedupe",
	Doc:      "requires events.Event literals to set DedupeValues (#2962, #3311)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.Preorder([]ast.Node{(*ast.CompositeLit)(nil)}, func(n ast.Node) {
		lit := n.(*ast.CompositeLit)
		if strings.HasSuffix(pass.Fset.File(lit.Pos()).Name(), "_test.go") {
			return
		}
		named, ok := types.Unalias(pass.TypesInfo.TypeOf(lit)).(*types.Named)
		if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != eventsPath || named.Obj().Name() != "Event" {
			return
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "DedupeValues" {
					return
				}
			}
		}
		pass.Reportf(lit.Pos(), "events.Event without DedupeValues; set it to the involved object's identity (#3311)")
	})
	return nil, nil
}
