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

// Package parammut flags functions that write into, or sort in place, a slice
// they received as a parameter. Callers routinely pass slices that
// alias informer-cache objects or shared scheduler state, so the write leaks
// back to every other reader.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3104: NewRequirement normalizes label values by
//     writing `values[i] = mapped` into its variadic argument, which rewrites
//     the caller's slice (pod and NodePool specs from the informer cache) whenever
//     NormalizedLabelValues is registered.
//   - kubernetes-sigs/karpenter#3022: NodeClaims shared one host-port usage
//     structure, so ports recorded for one NodeClaim showed up on others.
//   - kubernetes-sigs/karpenter#3116: DRA counter tracking mutated shared state,
//     causing a race between allocations.
//
// Functions whose doc comment says they modify the argument in place
// ("in place", "in-place", "mutates", "modifies") are skipped, as are
// parameters reassigned (e.g. `p = slices.Clone(p)`) before the write. Map
// parameters are not checked: passing a map to be filled is a common,
// intentional idiom, and on main it made up half the hits, all intended.
package parammut

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
)

var Analyzer = &analysis.Analyzer{
	Name:     "parammut",
	Doc:      "flags writes into or in-place sorts of slice parameters, which alias caller memory (#3104, #3022, #3116)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// sorters mutate their first argument.
var sorters = map[string]bool{
	"sort.Strings": true, "sort.Ints": true, "sort.Float64s": true,
	"sort.Slice": true, "sort.SliceStable": true, "sort.Sort": true, "sort.Stable": true,
	"slices.Sort": true, "slices.SortFunc": true, "slices.SortStableFunc": true, "slices.Reverse": true,
}

var docMarkers = []string{"in place", "in-place", "mutates", "modifies"}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.Preorder([]ast.Node{(*ast.FuncDecl)(nil)}, func(n ast.Node) {
		fd := n.(*ast.FuncDecl)
		fname := pass.Fset.Position(fd.Pos()).Filename
		if fd.Body == nil || strings.HasSuffix(fname, "_test.go") || strings.Contains(fname, "zz_generated") || documentsMutation(fd) {
			return
		}
		params := collectionParams(pass, fd)
		if len(params) == 0 {
			return
		}
		reassigned := map[types.Object]token.Pos{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						if obj := pass.TypesInfo.Uses[id]; params[obj] != "" {
							if _, seen := reassigned[obj]; !seen {
								reassigned[obj] = x.Pos()
							}
						}
					}
				}
			}
			return true
		})
		live := func(obj types.Object, at token.Pos) bool {
			p, ok := reassigned[obj]
			return !ok || at < p
		}
		reportWrite := func(l ast.Expr, at token.Pos) {
			ix, ok := l.(*ast.IndexExpr)
			if !ok {
				return
			}
			if id, ok := ix.X.(*ast.Ident); ok {
				obj := pass.TypesInfo.Uses[id]
				if kind := params[obj]; kind != "" && live(obj, at) {
					pass.Reportf(at, "%s writes into its %s parameter %s, which aliases the caller's memory; copy it first (#3104)", fd.Name.Name, kind, id.Name)
				}
			}
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					reportWrite(l, x.Pos())
				}
			case *ast.IncDecStmt:
				reportWrite(x.X, x.Pos())
			case *ast.CallExpr:
				fn, ok := typeutil.Callee(pass.TypesInfo, x).(*types.Func)
				if !ok || fn.Pkg() == nil || !sorters[fn.Pkg().Path()+"."+fn.Name()] || len(x.Args) == 0 {
					return true
				}
				if id, ok := x.Args[0].(*ast.Ident); ok {
					obj := pass.TypesInfo.Uses[id]
					if kind := params[obj]; kind != "" && live(obj, x.Pos()) {
						pass.Reportf(x.Pos(), "%s sorts its %s parameter %s in place, reordering the caller's slice; sort a copy (#3104)", fd.Name.Name, kind, id.Name)
					}
				}
			}
			return true
		})
	})
	return nil, nil
}

func collectionParams(pass *analysis.Pass, fd *ast.FuncDecl) map[types.Object]string {
	params := map[types.Object]string{}
	for _, fl := range fd.Type.Params.List {
		_, variadic := fl.Type.(*ast.Ellipsis)
		for _, nm := range fl.Names {
			obj := pass.TypesInfo.Defs[nm]
			if obj == nil {
				continue
			}
			switch obj.Type().Underlying().(type) {
			case *types.Slice:
				params[obj] = map[bool]string{true: "variadic", false: "slice"}[variadic]
			}
		}
	}
	return params
}

func documentsMutation(fd *ast.FuncDecl) bool {
	if fd.Doc == nil {
		return false
	}
	doc := strings.ToLower(fd.Doc.Text())
	for _, m := range docMarkers {
		if strings.Contains(doc, m) {
			return true
		}
	}
	return false
}
