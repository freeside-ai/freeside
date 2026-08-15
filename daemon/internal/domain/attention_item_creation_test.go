package domain_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestDaemonAttentionItemInputsDeclareCreatedAt prevents a new daemon emission
// site from silently inheriting the legacy nil allowance. Replay builders and
// migration code may declare nil explicitly, then preserve or stamp it at the
// persistence boundary; every other constructor declares its creation source.
func TestDaemonAttentionItemInputsDeclareCreatedAt(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", ".."))
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok || !isAttentionItemInput(literal.Type) {
				return true
			}
			for _, element := range literal.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if ok {
					if name, ok := field.Key.(*ast.Ident); ok && name.Name == "CreatedAt" {
						return true
					}
				}
			}
			t.Errorf("%s: AttentionItemInput omits CreatedAt", fset.Position(literal.Pos()))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func isAttentionItemInput(expr ast.Expr) bool {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name == "AttentionItemInput"
	case *ast.SelectorExpr:
		return value.Sel.Name == "AttentionItemInput"
	default:
		return false
	}
}
