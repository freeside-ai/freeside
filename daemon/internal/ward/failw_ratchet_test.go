package ward

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errorNamedFailfArgs reports the file:line of every failf argument whose
// name says it holds an error, in source parsed from body. A call like that
// flattens its cause into Reason; failw keeps it reachable through Err.
func errorNamedFailfArgs(filename string, body []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, body, 0)
	if err != nil {
		return nil, err
	}
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if fun, ok := call.Fun.(*ast.Ident); !ok || fun.Name != "failf" {
			return true
		}
		for _, arg := range call.Args {
			if isErrorName(argName(arg)) {
				pos := fset.Position(arg.Pos())
				found = append(found, fmt.Sprintf("%s:%d: %s", pos.Filename, pos.Line, argName(arg)))
			}
		}
		return true
	})
	return found, nil
}

func argName(arg ast.Expr) string {
	switch expr := arg.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.SelectorExpr:
		return expr.Sel.Name
	default:
		return ""
	}
}

// isErrorName is a naming heuristic, not a type check: every flagged name
// in ward held an error when the ratchet landed. A non-error value that
// trips it gets renamed rather than exempted.
func isErrorName(name string) bool {
	if strings.HasSuffix(name, "stderr") || strings.HasSuffix(name, "Stderr") {
		return false
	}
	return strings.HasSuffix(name, "err") || strings.HasSuffix(name, "Err")
}

func TestErrorNamedFailfArgDetection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want int
	}{
		{"plain err", `package p; var _ = failf(c, "x: %v", err)`, 1},
		{"suffixed err", `package p; var _ = failf(c, "x: %v", ferr)`, 1},
		{"camel Err", `package p; var _ = failf(c, "x: %v", cleanupErr)`, 1},
		{"selector Err", `package p; var _ = failf(c, "x: %v", result.Err)`, 1},
		{"multiline", "package p\nvar _ = failf(\n\tc,\n\t\"x: %v\",\n\terr,\n)\n", 1},
		{"nested in wrapper", `package p; var _ = fail(failf(c, "x: %v", err))`, 1},
		{"stderr", `package p; var _ = failf(c, "x: %s", stderr)`, 0},
		{"camel Stderr", `package p; var _ = failf(c, "x: %s", out.Stderr)`, 0},
		{"no error", `package p; var _ = failf(c, "state %q", state)`, 0},
		{"failw", `package p; var _ = failw(c, err, "x: %v", err)`, 0},
		{"call on err", `package p; var _ = failf(c, "x: %s", err.Error())`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, err := errorNamedFailfArgs("fixture.go", []byte(tt.body))
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			if len(found) != tt.want {
				t.Fatalf("errorNamedFailfArgs() = %q, want %d matches", found, tt.want)
			}
		})
	}
}

// TestFailfCarriesNoErrorCause keeps ward's failf calls from flattening an
// error cause into Reason again (#574): a failure caused by an error goes
// through failw so ConformanceFailure.Err keeps it.
func TestFailfCarriesNoErrorCause(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package source: %v", err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		body, err := os.ReadFile(path) //nolint:gosec // package source files named by the test's own glob
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		found, err := errorNamedFailfArgs(path, body)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, site := range found {
			t.Errorf("%s: failf formats an error; use failw with it as the cause", site)
		}
	}
}
