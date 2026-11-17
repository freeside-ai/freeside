package store

import (
	"go/ast"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdmissionAndPolicyPackagesCannotReadUsageObservations(t *testing.T) {
	t.Parallel()
	parsed, err := daemonSources()
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{"domain", "store", "intake", "signet", "exec", "inference", "publish", "engine"}
	inspected := 0
	for _, target := range targets {
		matched := false
		for _, source := range parsed.sources {
			if !strings.HasPrefix(source.rel, "internal/"+target+"/") {
				continue
			}
			matched = true
			if name := path.Base(source.rel); target == "store" && (name == "usage_observation.go" || name == "tx.go") {
				continue
			}
			inspected++
			// The path from this package's directory, as the scan named it
			// before it shared the parse.
			filename := filepath.Join("..", filepath.FromSlash(strings.TrimPrefix(source.rel, "internal/")))
			ast.Inspect(source.file, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				if ok && (identifier.Name == "ReadUsage" || identifier.Name == "UsageReadTx") {
					t.Errorf("%s references the observation-only usage reader %s", filename, identifier.Name)
				}
				return true
			})
		}
		// A renamed or removed target must fail, not drop out of the scan.
		if !matched {
			t.Fatalf("no non-test Go files under %s", filepath.Join("..", target))
		}
	}
	if inspected < 200 {
		t.Fatalf("inspected %d admission/policy files", inspected)
	}
}
