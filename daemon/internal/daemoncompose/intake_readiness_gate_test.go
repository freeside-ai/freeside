package daemoncompose

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestIntakeAdmitChecksProjectReadiness is the daemoncompose half of the
// #3205 enumeration (engine's TestEveryNewWorkPathChecksProjectReadiness):
// label intake admits new work in intakeReconciler.admit, so that method
// must apply the readiness gate. TestIntakeDefersOccurrenceWhileProjectNotReady
// covers that it applies it before any write.
func TestIntakeAdmitChecksProjectReadiness(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "intake_reconcile.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "admit" || fn.Recv == nil {
			continue
		}
		gated := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "gate" {
					gated = true
				}
			}
			return !gated
		})
		if !gated {
			t.Fatal("intakeReconciler.admit admits new work without calling the project readiness gate")
		}
		return
	}
	t.Fatal("intakeReconciler.admit not found; update this test to the method that admits label intake")
}
