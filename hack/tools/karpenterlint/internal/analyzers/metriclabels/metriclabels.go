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

// Package metriclabels checks that every emission or deletion of an
// operatorpkg metric passes exactly the label names the metric declares.
// A missing label panics at runtime, an extra label panics, and a Delete
// with too few labels silently matches nothing, leaking the series.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3359: #3269 added a nodepool label to the
//     cloudprovider error metric and the Reboot call site, which merged later,
//     never passed it. Tests caught it after main broke. That site builds its
//     map in a helper, which this analyzer does not follow yet; it checks
//     literal maps, which are most emission sites.
//   - Label churn in #3091, #3096, #3179 and #3284 touched every emission site
//     by hand.
package metriclabels

import (
	"go/ast"
	"go/constant"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const opmetricsPath = "github.com/awslabs/operatorpkg/metrics"

var Analyzer = &analysis.Analyzer{
	Name:      "metriclabels",
	Doc:       "checks that operatorpkg metric Inc/Add/Set/Observe/Delete calls pass exactly the declared label names (#3359)",
	Requires:  []*analysis.Analyzer{inspect.Analyzer},
	Run:       run,
	FactTypes: []analysis.Fact{new(labelFact), new(metricFact)},
}

// labelFact records the Prometheus label name of a package-level opmetrics.Label var.
type labelFact struct{ Name string }

func (*labelFact) AFact()           {}
func (f *labelFact) String() string { return "label(" + f.Name + ")" }

// metricFact records the declared label names of a package-level metric var.
type metricFact struct{ Labels []string }

func (*metricFact) AFact()           {}
func (f *metricFact) String() string { return "metric(" + strings.Join(f.Labels, ",") + ")" }

// labelArg maps each emission method to the index of its labels argument.
var labelArg = map[string]int{
	"Inc":     0,
	"Add":     1,
	"Set":     1,
	"Observe": 1,
	"Delete":  0,
}

var constructors = []string{"NewPrometheusCounter", "NewPrometheusGauge", "NewPrometheusHistogram", "NewPrometheusSummary"}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	specs := packageVarSpecs(pass)

	// Labels first so metrics declared in the same package can resolve them.
	for _, s := range specs {
		if isLabelType(pass.TypesInfo.TypeOf(s.value)) {
			if name, ok := labelName(pass, s.value); ok {
				pass.ExportObjectFact(s.obj, &labelFact{Name: name})
			}
		}
	}
	for _, s := range specs {
		if labels, ok := declaredLabels(pass, s.value); ok {
			pass.ExportObjectFact(s.obj, &metricFact{Labels: labels})
		}
	}

	ins.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return
		}
		idx, ok := labelArg[sel.Sel.Name]
		if !ok || len(call.Args) <= idx {
			return
		}
		obj := objOf(pass, sel.X)
		if obj == nil {
			return
		}
		var fact metricFact
		if !pass.ImportObjectFact(obj, &fact) {
			return
		}
		lit, ok := call.Args[idx].(*ast.CompositeLit)
		if !ok {
			return
		}
		var keys []string
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				return
			}
			k, ok := constString(pass, kv.Key)
			if !ok {
				return
			}
			keys = append(keys, k)
		}
		missing := diff(fact.Labels, keys)
		extra := diff(keys, fact.Labels)
		if len(missing) == 0 && len(extra) == 0 {
			return
		}
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "missing "+strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			parts = append(parts, "undeclared "+strings.Join(extra, ", "))
		}
		pass.Reportf(lit.Pos(), "%s.%s labels do not match the declaration (%s); declared: %s (#3359)",
			obj.Name(), sel.Sel.Name, strings.Join(parts, "; "), strings.Join(fact.Labels, ", "))
	})
	return nil, nil
}

type varSpec struct {
	obj   types.Object
	value ast.Expr
}

func packageVarSpecs(pass *analysis.Pass) []varSpec {
	var out []varSpec
	for _, f := range pass.Files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, name := range vs.Names {
					if obj := pass.TypesInfo.Defs[name]; obj != nil {
						out = append(out, varSpec{obj: obj, value: vs.Values[i]})
					}
				}
			}
		}
	}
	return out
}

func isLabelType(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == opmetricsPath && obj.Name() == "Label"
}

func labelName(pass *analysis.Pass, e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return "", false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Name" {
			return constString(pass, kv.Value)
		}
	}
	return "", false
}

func declaredLabels(pass *analysis.Pass, e ast.Expr) ([]string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) < 3 {
		return nil, false
	}
	fn, ok := typeutilCallee(pass, call).(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != opmetricsPath || !slices.Contains(constructors, fn.Name()) {
		return nil, false
	}
	lit, ok := call.Args[2].(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	var names []string
	for _, el := range lit.Elts {
		if name, ok := labelName(pass, el); ok {
			names = append(names, name)
			continue
		}
		obj := objOf(pass, el)
		var lf labelFact
		if obj == nil || !pass.ImportObjectFact(obj, &lf) {
			return nil, false
		}
		names = append(names, lf.Name)
	}
	return names, true
}

func typeutilCallee(pass *analysis.Pass, call *ast.CallExpr) types.Object {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return pass.TypesInfo.Uses[fun]
	case *ast.SelectorExpr:
		return pass.TypesInfo.Uses[fun.Sel]
	}
	return nil
}

func objOf(pass *analysis.Pass, e ast.Expr) types.Object {
	switch x := e.(type) {
	case *ast.Ident:
		return pass.TypesInfo.Uses[x]
	case *ast.SelectorExpr:
		return pass.TypesInfo.Uses[x.Sel]
	case *ast.ParenExpr:
		return objOf(pass, x.X)
	}
	return nil
}

func constString(pass *analysis.Pass, e ast.Expr) (string, bool) {
	tv, ok := pass.TypesInfo.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// diff returns the elements of a that are not in b.
func diff(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}
