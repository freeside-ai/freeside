package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdmissionAndPolicyPackagesCannotReadComprehension enforces the §5.13
// advisory-segregation discipline for comprehension telemetry (issue #924):
// admission and policy packages must not reach the observation-only reader, so
// events can never enter policy evaluation. It fails the build if any non-test
// file in those packages references the ReadComprehension or ComprehensionReadTx
// identifiers. store/comprehension.go (which defines them) is skipped, as is
// tx.go; the measurement package (observe) and the operator command are not in
// the target set, so they may read the surface.
func TestAdmissionAndPolicyPackagesCannotReadComprehension(t *testing.T) {
	root := filepath.Join("..")
	targets := []string{"domain", "store", "intake", "signet", "exec", "inference", "publish", "engine"}
	inspected := 0
	fset := token.NewFileSet()
	for _, target := range targets {
		err := filepath.WalkDir(filepath.Join(root, target), func(
			path string, entry os.DirEntry, err error,
		) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if target == "store" && (entry.Name() == "comprehension.go" || entry.Name() == "tx.go") {
				return nil
			}
			inspected++
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				if ok && (identifier.Name == "ReadComprehension" || identifier.Name == "ComprehensionReadTx") {
					t.Errorf("%s references the observation-only comprehension reader %s", path, identifier.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if inspected < 200 {
		t.Fatalf("inspected %d admission/policy files", inspected)
	}
}
