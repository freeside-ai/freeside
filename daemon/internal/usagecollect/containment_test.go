package usagecollect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// permittedImports is the complete set this package's non-test files may
// import. Collection redeems no reset credit, refreshes no token, and writes no
// auth store (plan §10), so the auth-store adapters (internal/wardstore) and
// the process runner (internal/exec) are absent, and of internal/ward only the
// collector interface is used (permittedWardNames). internal/store is
// present: the record lives there, and the generation check in
// TestScheduledPassCollectsEveryPool covers what the import can't.
var permittedImports = map[string]bool{
	"cmp":      true,
	"context":  true,
	"errors":   true,
	"fmt":      true,
	"log/slog": true,
	"slices":   true,
	"sync":     true,
	"time":     true,
	"github.com/freeside-ai/freeside/daemon/internal/domain": true,
	"github.com/freeside-ai/freeside/daemon/internal/store":  true,
	"github.com/freeside-ai/freeside/daemon/internal/ward":   true,
}

// permittedWardNames is everything this package may name in internal/ward:
// the collector interface and its types, none of which reaches a credential.
var permittedWardNames = map[string]bool{
	"UsageCollector":         true,
	"UsageCollectionRequest": true,
	"UsageCollection":        true,
	"UsageThrottleError":     true,
}

// TestCollectionReachesNoCredentialSurface pins the package's imports and its
// use of ward. Widening either is a deliberate change to the allowlists
// above, not a passing test.
func TestCollectionReachesNoCredentialSurface(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Name != nil || !permittedImports[imported] {
				t.Errorf("%s imports %s outside the collection boundary", name, spec.Path.Value)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "ward" && !permittedWardNames[selector.Sel.Name] {
				t.Errorf("%s names ward.%s, outside the collector interface", name, selector.Sel.Name)
			}
			return true
		})
	}
	// A rename or move must not turn the boundary off silently.
	if files < 3 {
		t.Fatalf("inspected %d non-test files", files)
	}
}
