package inference

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestInferenceImportsNoExecutionCapability(t *testing.T) {
	forbidden := []string{
		"/internal/exec", "/internal/ward", "/internal/projectimage", "/internal/verify",
		"os/exec", "github.com/apple/container",
	}
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
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, fragment := range forbidden {
				if strings.Contains(path, fragment) {
					t.Errorf("%s imports forbidden capability %s", name, path)
				}
			}
		}
	}
	if files < 4 {
		t.Fatalf("inspected %d files, want at least 4", files)
	}
}
