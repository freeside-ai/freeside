package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestRFC3339NanoRoutedThroughHelpers is the #553 ratchet: every RFC3339Nano
// format or parse in the store package must go through formatTime/parseTime,
// which own the UTC-normalization contract. It fails if a time.RFC3339Nano
// selector appears in any non-test source file outside the two helper bodies,
// so the class of ad-hoc, un-normalized call sites cannot silently regrow.
//
// A deliberate new site (a helper the two do not cover) extends allowed with a
// comment saying why; the point is that adding one is a visible, reviewed
// decision, not an accident. The scan is AST-based, so RFC3339Nano in a comment
// or string literal is not a call site and never trips it.
func TestRFC3339NanoRoutedThroughHelpers(t *testing.T) {
	t.Parallel()

	// The only sanctioned time.RFC3339Nano references, keyed "file:function".
	allowed := map[string]bool{
		"pairing.go:formatTime": true, // renders every stored RFC3339Nano column
		"pairing.go:parseTime":  true, // reads every stored RFC3339Nano column
	}

	// go test runs with the package directory as the working directory.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "RFC3339Nano" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "time" {
				return true
			}
			key := name + ":" + enclosingFuncName(file, sel.Pos())
			if !allowed[key] {
				t.Errorf("time.RFC3339Nano used at %s (%s): route it through formatTime/parseTime, or extend the allowed list with a justification",
					fset.Position(sel.Pos()), key)
			}
			return true
		})
	}
}

// enclosingFuncName returns the name of the top-level function whose body spans
// pos, or "<file-scope>" when pos is outside every function body.
func enclosingFuncName(file *ast.File, pos token.Pos) string {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Body != nil && fn.Body.Pos() <= pos && pos <= fn.Body.End() {
			return fn.Name.Name
		}
	}
	return "<file-scope>"
}
