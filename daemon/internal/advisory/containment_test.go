package advisory

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPolicyAndTelemetryPackagesCannotReadAdvisoryStore(t *testing.T) {
	root := filepath.Join("..")
	targets := []string{"domain", "publish", "store", "observe"}
	inspected := 0
	fset := token.NewFileSet()
	for _, target := range targets {
		err := filepath.WalkDir(filepath.Join(root, target), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			inspected++
			file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range file.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if strings.HasSuffix(imported, "/internal/advisory") {
					t.Errorf("%s imports the advisory store", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if inspected < 20 {
		t.Fatalf("inspected %d policy/telemetry files", inspected)
	}
}
