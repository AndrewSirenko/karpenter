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

// Package statewriter flags writes to Karpenter's in-memory cluster state from
// outside pkg/controllers/state. The informer controllers own state.Cluster
// and state.NodePoolState. Writes from elsewhere ("seeds" ahead of the watch)
// carry no resourceVersion ordering, so they can resurrect an object the
// informer already deleted or be clobbered by the next informer event.
//
// Each intended seed must be acknowledged with a
// "//karpenter:state-seed <reason>" comment on the call's line or the line
// above, so that reviewers re-check it against both races.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3176 (fixed by #3190 with a GC controller): the
//     provisioner's UpdateNodeClaim seed landed after the informer's delete and
//     re-inserted a ghost entry, so Synced() stayed false for hours.
//   - kubernetes-sigs/karpenter#3250: the informer's NodePoolState.UpdateNodeClaim
//     reset the PendingDisruption mark set by the disruption queue, and static
//     deprovisioning then deleted the replacement NodeClaim.
package statewriter

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const (
	statePkg  = "sigs.k8s.io/karpenter/pkg/controllers/state"
	directive = "//karpenter:state-seed"
)

var Analyzer = &analysis.Analyzer{
	Name:     "statewriter",
	Doc:      "flags cluster-state writes outside pkg/controllers/state without a //karpenter:state-seed justification (#3176, #3250)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// writers are the methods that mirror informer events or move a NodeClaim
// between NodePoolState buckets. Node-count reservations are excluded; they are
// owned by their callers.
var writers = map[string]map[string]bool{
	"Cluster": {
		"UpdateNodeClaim": true, "DeleteNodeClaim": true, "UpdateNode": true, "DeleteNode": true,
		"UpdatePod": true, "DeletePod": true, "UpdateDaemonSet": true, "DeleteDaemonSet": true,
	},
	"NodePoolState": {
		"SetNodeClaimMapping": true, "MarkNodeClaimActive": true, "MarkNodeClaimDeleting": true,
		"MarkNodeClaimPendingDisruption": true, "Cleanup": true, "UpdateNodeClaim": true,
	},
}

func run(pass *analysis.Pass) (any, error) {
	path := pass.Pkg.Path()
	if path == statePkg || strings.HasPrefix(path, statePkg+"/") || strings.HasPrefix(path, "sigs.k8s.io/karpenter/pkg/test") {
		return nil, nil
	}
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.WithStack([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node, push bool, stack []ast.Node) bool {
		if !push {
			return true
		}
		call := n.(*ast.CallExpr)
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != statePkg {
			return true
		}
		recv := fn.Signature().Recv()
		if recv == nil {
			return true
		}
		t := recv.Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		named, ok := t.(*types.Named)
		if !ok || !writers[named.Obj().Name()][fn.Name()] {
			return true
		}
		file, _ := stack[0].(*ast.File)
		if file == nil || strings.HasSuffix(pass.Fset.Position(file.Pos()).Filename, "_test.go") || acknowledged(pass, file, call) {
			return true
		}
		pass.Reportf(call.Pos(), "%s.%s writes cluster state outside the informer controllers; it can race the informer (#3176, #3250). Add a %s <reason> comment if this seed is intended",
			named.Obj().Name(), fn.Name(), directive)
		return true
	})
	return nil, nil
}

func acknowledged(pass *analysis.Pass, file *ast.File, call *ast.CallExpr) bool {
	line := pass.Fset.Position(call.Pos()).Line
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			l := pass.Fset.Position(c.Pos()).Line
			if (l == line || l == line-1) && strings.HasPrefix(c.Text, directive) {
				return true
			}
		}
	}
	return false
}
