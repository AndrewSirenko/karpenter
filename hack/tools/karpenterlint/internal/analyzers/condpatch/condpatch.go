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

// Package condpatch flags a status condition change that is written with a
// plain client.MergeFrom patch. A JSON merge patch replaces the whole
// status.conditions list and carries no resourceVersion, so a concurrent writer's
// conditions are silently dropped and a stale reconcile can repeat a terminal
// transition. Use client.MergeFromWithOptions(stored,
// client.MergeFromWithOptimisticLock{}) so the API server rejects the stale write.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3360: the reboot terminal transition was patched
//     with client.MergeFrom(stored), so a stale RebootIssued reconcile recorded
//     the reboot's terminal metrics twice.
//   - kubernetes-sigs/karpenter#3371: an e2e helper wrote Node conditions from a
//     stale read and reverted a do-not-repair label written in between.
package condpatch

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const (
	clientPkg = "sigs.k8s.io/controller-runtime/pkg/client"
	statusPkg = "github.com/awslabs/operatorpkg/status"
)

var Analyzer = &analysis.Analyzer{
	Name:     "condpatch",
	Doc:      "flags status conditions written with a plain client.MergeFrom patch; use MergeFromWithOptimisticLock (#3360, #3371)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

var writeMethods = map[string]bool{
	"Set": true, "SetTrue": true, "SetTrueWithReason": true, "SetFalse": true,
	"SetUnknown": true, "SetUnknownWithReason": true, "Clear": true,
}

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	reported := map[*ast.CallExpr]bool{}
	ins.Preorder([]ast.Node{(*ast.FuncDecl)(nil), (*ast.FuncLit)(nil)}, func(n ast.Node) {
		var body *ast.BlockStmt
		switch f := n.(type) {
		case *ast.FuncDecl:
			body = f.Body
		case *ast.FuncLit:
			body = f.Body
		}
		if body == nil {
			return
		}
		written := conditionWrites(pass, body)
		if len(written) == 0 {
			return
		}
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Patch" {
				return true
			}
			patch, ok := call.Args[2].(*ast.CallExpr)
			if !ok || !isFunc(pass, patch, clientPkg, "MergeFrom") || reported[patch] {
				return true
			}
			obj := objOf(pass, call.Args[1])
			if obj == nil {
				return true
			}
			for _, pos := range written[obj] {
				if pos < call.Pos() {
					reported[patch] = true
					pass.Reportf(patch.Pos(), "status conditions on %s are patched with client.MergeFrom, which replaces the conditions list without a resourceVersion check; use client.MergeFromWithOptions(..., client.MergeFromWithOptimisticLock{}) (#3360)", obj.Name())
					break
				}
			}
			return true
		})
	})
	return nil, nil
}

// conditionWrites maps each object to the positions where X.StatusConditions(...).Set*/Clear is called on it.
func conditionWrites(pass *analysis.Pass, body *ast.BlockStmt) map[types.Object][]token.Pos {
	out := map[types.Object][]token.Pos{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !writeMethods[sel.Sel.Name] {
			return true
		}
		inner, ok := sel.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		innerSel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok || innerSel.Sel.Name != "StatusConditions" || !returnsConditionSet(pass, innerSel) {
			return true
		}
		if obj := objOf(pass, innerSel.X); obj != nil {
			out[obj] = append(out[obj], call.Pos())
		}
		return true
	})
	return out
}

func returnsConditionSet(pass *analysis.Pass, sel *ast.SelectorExpr) bool {
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok {
		return false
	}
	res := fn.Signature().Results()
	if res.Len() != 1 {
		return false
	}
	named, ok := types.Unalias(res.At(0).Type()).(*types.Named)
	return ok && named.Obj().Name() == "ConditionSet" && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == statusPkg
}

func isFunc(pass *analysis.Pass, call *ast.CallExpr, pkg, name string) bool {
	var id *ast.Ident
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return false
	}
	fn, ok := pass.TypesInfo.Uses[id].(*types.Func)
	return ok && fn.Name() == name && fn.Pkg() != nil && fn.Pkg().Path() == pkg
}

func objOf(pass *analysis.Pass, e ast.Expr) types.Object {
	e = ast.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = u.X
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return nil
	}
	return pass.TypesInfo.ObjectOf(id)
}
