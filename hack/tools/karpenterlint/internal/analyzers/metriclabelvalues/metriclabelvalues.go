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

// Package metriclabelvalues requires constant metric label values to be
// non-empty lowercase snake_case, both where they are emitted and where they
// are documented in an opmetrics.Label's Values. Values mirrored verbatim from
// an API constant (a package under /apis/ or k8s.io/api) are exempt.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3284: the reason label mixed casings and had
//     to be standardized to lowercase in a breaking change.
//   - kubernetes-sigs/karpenter#3323: the cloudprovider error label gained an
//     undocumented "unknown" next to CamelCase values.
//   - kubernetes-sigs/karpenter#3179: consolidation_policy is emitted as ""
//     at several sites although the PR described "none".
//   - kubernetes-sigs/karpenter#3404: new decision values shipped kebab-case
//     (no-reserved-capacity, static-at-limit) while reasons are snake_case.
package metriclabelvalues

import (
	"go/ast"
	"go/constant"
	"go/types"
	"regexp"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const (
	opmetricsPath = "github.com/awslabs/operatorpkg/metrics"
	docsPath      = "github.com/awslabs/operatorpkg/docs"
)

var Analyzer = &analysis.Analyzer{
	Name:     "metriclabelvalues",
	Doc:      "requires constant metric label values to be non-empty lowercase snake_case (#3284, #3323, #3179, #3404)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

var snakeCase = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)

var emitters = map[string]int{
	"Inc":                0,
	"Add":                1,
	"Set":                1,
	"Observe":            1,
	"Delete":             0,
	"DeletePartialMatch": 0,
}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.Preorder([]ast.Node{(*ast.CallExpr)(nil), (*ast.CompositeLit)(nil)}, func(n ast.Node) {
		switch x := n.(type) {
		case *ast.CallExpr:
			checkEmission(pass, x)
		case *ast.CompositeLit:
			if isNamed(pass.TypesInfo.TypeOf(x), docsPath, "Value") {
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Name" {
							checkValue(pass, kv.Value)
						}
					}
				}
			}
		}
	})
	return nil, nil
}

func checkEmission(pass *analysis.Pass, call *ast.CallExpr) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	idx, ok := emitters[sel.Sel.Name]
	if !ok || len(call.Args) <= idx || !fromPkg(pass.TypesInfo.TypeOf(sel.X), opmetricsPath) {
		return
	}
	lit, ok := call.Args[idx].(*ast.CompositeLit)
	if !ok {
		return
	}
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			checkValue(pass, kv.Value)
		}
	}
}

func checkValue(pass *analysis.Pass, e ast.Expr) {
	tv, ok := pass.TypesInfo.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String || fromAPIConst(pass, e) {
		return
	}
	v := constant.StringVal(tv.Value)
	switch {
	case v == "":
		pass.Reportf(e.Pos(), "metric label value is empty; use a documented constant such as \"none\" or \"unknown\" (#3179)")
	case !snakeCase.MatchString(v):
		pass.Reportf(e.Pos(), "metric label value %q is not lowercase snake_case (#3284)", v)
	}
}

// fromAPIConst reports whether e names a constant declared in an API package,
// whose value the metric mirrors verbatim (e.g. capacity type "on-demand").
func fromAPIConst(pass *analysis.Pass, e ast.Expr) bool {
	// Unwrap conversions such as string(corev1.PodPending).
	if call, ok := e.(*ast.CallExpr); ok && len(call.Args) == 1 && pass.TypesInfo.Types[call.Fun].IsType() {
		e = call.Args[0]
	}
	var id *ast.Ident
	switch x := e.(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	default:
		return false
	}
	c, ok := pass.TypesInfo.Uses[id].(*types.Const)
	if !ok || c.Pkg() == nil {
		return false
	}
	p := c.Pkg().Path()
	return strings.Contains(p, "/apis/") || strings.HasPrefix(p, "k8s.io/api/")
}

func isNamed(t types.Type, pkg, name string) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg && n.Obj().Name() == name
}

func fromPkg(t types.Type, pkg string) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg
}
