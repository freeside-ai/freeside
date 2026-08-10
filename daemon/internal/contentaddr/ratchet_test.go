package contentaddr_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

var openCodedDigestPattern = regexp.MustCompile(`"sha256:"\s*\+|sha256:%x`)

func findOpenCodedDigest(body []byte) (string, error) {
	if match := openCodedDigestPattern.Find(body); match != nil {
		return string(match), nil
	}

	file, err := parser.ParseFile(token.NewFileSet(), "", body, 0)
	if err != nil {
		return "", err
	}
	stringPackages := make(map[string]bool)
	dotImportedStrings := false
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil || importPath != "strings" {
			continue
		}
		switch {
		case spec.Name == nil:
			stringPackages["strings"] = true
		case spec.Name.Name == ".":
			dotImportedStrings = true
		case spec.Name.Name != "_":
			stringPackages[spec.Name.Name] = true
		}
	}

	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		prefix, ok := call.Args[1].(*ast.BasicLit)
		if !ok || prefix.Kind != token.STRING {
			return true
		}
		prefixValue, err := strconv.Unquote(prefix.Value)
		if err != nil || prefixValue != "sha256:" {
			return true
		}

		switch function := call.Fun.(type) {
		case *ast.SelectorExpr:
			qualifier, ok := function.X.(*ast.Ident)
			found = ok && stringPackages[qualifier.Name] && function.Sel.Name == "TrimPrefix"
		case *ast.Ident:
			found = dotImportedStrings && function.Name == "TrimPrefix"
		}
		return !found
	})
	if found {
		return `strings.TrimPrefix(..., "sha256:")`, nil
	}
	return "", nil
}

func TestOpenCodedDigestDetection(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"same-line inverse", `package p; import "strings"; var _ = strings.TrimPrefix(addr, "sha256:")`, true},
		{"multiline inverse", "package p\nimport \"strings\"\nvar _ = strings.TrimPrefix(\n\taddr,\n\t\"sha256:\",\n)\n", true},
		{"aliased inverse", `package p; import text "strings"; var _ = text.TrimPrefix(addr, "sha256:")`, true},
		{"dot-imported inverse", `package p; import . "strings"; var _ = TrimPrefix(addr, "sha256:")`, true},
		{"other package", `package p; var _ = other.TrimPrefix(addr, "sha256:")`, false},
		{"other prefix", `package p; import "strings"; var _ = strings.TrimPrefix(addr, "sha512:")`, false},
		{"concatenated writer", `package p; var _ = "sha256:" + digest`, true},
		{"formatted writer", `package p; const format = "sha256:%x"`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, err := findOpenCodedDigest([]byte(tt.body))
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			if got := match != ""; got != tt.want {
				t.Fatalf("findOpenCodedDigest() matched = %v, want %v (match %q)", got, tt.want, match)
			}
		})
	}
}

// TestDigestFormattingIsCentralized keeps non-test daemon code from
// reintroducing the writer and inverse spellings centralized by issue #564.
// Test fixtures remain independent so their expected values can catch helper
// regressions instead of computing expectations through the code under test.
func TestDigestFormattingIsCentralized(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate ratchet test source")
	}
	daemonRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	contentaddrDir := filepath.Dir(sourceFile)

	err := filepath.WalkDir(daemonRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == contentaddrDir {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path) //nolint:gosec // repository files under the test's daemon root
		if err != nil {
			return err
		}
		match, err := findOpenCodedDigest(body)
		if err != nil {
			return err
		}
		if match != "" {
			rel, err := filepath.Rel(daemonRoot, path)
			if err != nil {
				return err
			}
			t.Errorf("%s open-codes content-address formatting with %q", rel, match)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan daemon source: %v", err)
	}
}
