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

// poolUsageReaders are the usage observation record's readers.
var poolUsageReaders = map[string]bool{
	"GetPoolUsageObservation":           true,
	"ListPoolUsageObservations":         true,
	"ListPoolUsageObservationSnapshots": true,
	"ExhaustedMarks":                    true,
	"ExhaustedMarksForModel":            true,
	"WindowsForModel":                   true,
}

// poolUsageReaderFiles are the only non-test daemon files, or directories,
// that may name a reader, relative to daemon/. The record is observation,
// never authority (plan §5.4): no preflight, admission, or scheduling path
// reads it. A new reader, such as the router, joins this list as a visible
// change. The bootstrap's usage projection reads it because the record
// informs the account usage view in the clients (plan §5.4, usage
// observation); it projects, and decides nothing.
var poolUsageReaderFiles = []string{
	"internal/domain/pool_usage_observation.go",
	"internal/store/pool_usage_observation.go",
	"internal/usagecollect/",
	"cmd/freesided/auth_usage.go",
	"internal/signet/usage.go",
}

func TestOnlyAllowlistedFilesReadPoolUsageObservations(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	inspected := 0
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		for _, allowed := range poolUsageReaderFiles {
			if relative == allowed || strings.HasSuffix(allowed, "/") && strings.HasPrefix(relative, allowed) {
				return nil
			}
		}
		inspected++
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok && poolUsageReaders[identifier.Name] {
				t.Errorf("%s references the pool usage record reader %s", relative, identifier.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A broken walk must not pass by inspecting nothing.
	if inspected < 500 {
		t.Fatalf("inspected %d daemon files", inspected)
	}
}
