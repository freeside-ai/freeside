package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestMigrationNamesReferencedFromGoExist guards the data-migration hook,
// which matches on file name alone: a migration renamed at rebase whose Go
// reference was missed would otherwise skip its backfill silently. Test files
// are outside the scan because their fixtures use names such as 0001_ok.sql
// that no embedded file carries.
func TestMigrationNamesReferencedFromGoExist(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil || !migrationName.MatchString(value) {
				return true
			}
			found++
			if _, err := fs.Stat(migrations.FS, value); err != nil {
				t.Errorf("%s: migration %q names no file in daemon/migrations", fset.Position(lit.Pos()), value)
			}
			return true
		})
	}
	// The hook alone references ten migrations; fewer means the scan broke.
	if found < 10 {
		t.Fatalf("found %d migration-name literals in non-test files, want at least 10", found)
	}
}
