package publish

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyGitNetImportsExec pins the process-boundary discipline the
// importer established, for the publish lane: no publish package file
// may execute a subprocess except the single hardened transport
// runner. A new os/exec import anywhere else is the escape this test
// exists to catch.
func TestOnlyGitNetImportsExec(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue // test helpers legitimately run git to build fixtures
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"os/exec"` && filepath.Base(name) != "gitnet.go" {
				t.Errorf("%s imports os/exec; only gitnet.go may", name)
			}
		}
	}
}
