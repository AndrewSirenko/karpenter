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

// Package wellknownkeys flags string literals spelling a karpenter.sh
// label, annotation or taint key outside pkg/apis. Keys must be declared once
// as API constants (and registered in the well-known lists) and referenced
// everywhere else, so docs and validation see every key.
//
// Inspired by:
//   - kubernetes-sigs/karpenter#3364: well-known annotations had to be
//     documented as wellknown.Annotations after the fact; 5 annotation
//     constants are still missing from the list.
//   - kubernetes-sigs/karpenter#3375: an unused
//     compatibility.karpenter.sh/provider annotation lingered undetected.
//   - kubernetes-sigs/karpenter#3376: feature gates had to be documented as
//     options.FeatureGates for the same reason.
package wellknownkeys

import (
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:     "wellknownkeys",
	Doc:      "flags karpenter.sh label/annotation/taint keys spelled as string literals outside pkg/apis (#3364, #3375, #3376)",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// key matches a whole literal that is a karpenter.sh (or subdomain) key,
// optionally followed by "=value" as used for taints and selectors.
var key = regexp.MustCompile(`^([a-z0-9-]+\.)*karpenter\.sh/[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?(=.*)?$`)

func run(pass *analysis.Pass) (any, error) {
	ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	ins.Preorder([]ast.Node{(*ast.BasicLit)(nil)}, func(n ast.Node) {
		lit := n.(*ast.BasicLit)
		if lit.Kind != token.STRING {
			return
		}
		file := pass.Fset.File(lit.Pos()).Name()
		if strings.HasSuffix(file, "_test.go") || strings.Contains(file, "/pkg/apis/") || strings.HasPrefix(file, "pkg/apis/") {
			return
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil || !key.MatchString(v) {
			return
		}
		pass.Reportf(lit.Pos(), "%q spells a karpenter.sh key; reference the constant from pkg/apis (#3364)", v)
	})
	return nil, nil
}
