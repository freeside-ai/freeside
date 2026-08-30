package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttentionItemProducersPopulateDisplayNames(t *testing.T) {
	t.Parallel()
	daemonRoot := filepath.Join("..", "..")
	packages := []string{
		filepath.Join(daemonRoot, "internal", "engine"),
		filepath.Join(daemonRoot, "internal", "signet"),
		filepath.Join(daemonRoot, "internal", "store"),
		filepath.Join(daemonRoot, "internal", "operations"),
		filepath.Join(daemonRoot, "cmd", "freesided"),
	}
	fset := token.NewFileSet()
	for _, pkg := range packages {
		err := filepath.WalkDir(pkg, func(
			path string, entry os.DirEntry, err error,
		) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" ||
				strings.HasSuffix(path, "_test.go") || entry.Name() == "migration_codex_reenrollment.go" {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !isNewAttentionItemCall(call.Fun) || len(call.Args) == 0 {
					return true
				}
				input, ok := call.Args[0].(*ast.CompositeLit)
				if !ok || !isAttentionItemInput(input.Type) {
					return true
				}
				if !attentionItemInputHasField(input, "DisplayNames") {
					t.Errorf("%s:%d attention-item producer omits DisplayNames",
						path, fset.Position(call.Pos()).Line)
				}
				if isSystemHealthInput(input) && !attentionItemInputHasField(input, "HealthDiagnostic") {
					t.Errorf("%s:%d system-health producer omits HealthDiagnostic",
						path, fset.Position(call.Pos()).Line)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func attentionItemInputHasField(input *ast.CompositeLit, name string) bool {
	for _, element := range input.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, keyOK := field.Key.(*ast.Ident); keyOK && key.Name == name {
			return true
		}
	}
	return false
}

func isSystemHealthInput(input *ast.CompositeLit) bool {
	for _, element := range input.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, keyOK := field.Key.(*ast.Ident)
		if !keyOK || key.Name != "Type" {
			continue
		}
		value, valueOK := field.Value.(*ast.SelectorExpr)
		return valueOK && value.Sel.Name == "AttentionSystemHealth"
	}
	return false
}

func isNewAttentionItemCall(fun ast.Expr) bool {
	switch value := fun.(type) {
	case *ast.Ident:
		return value.Name == "NewAttentionItem"
	case *ast.SelectorExpr:
		return value.Sel.Name == "NewAttentionItem"
	default:
		return false
	}
}

func isAttentionItemInput(value ast.Expr) bool {
	switch typed := value.(type) {
	case *ast.Ident:
		return typed.Name == "AttentionItemInput"
	case *ast.SelectorExpr:
		return typed.Sel.Name == "AttentionItemInput"
	default:
		return false
	}
}
