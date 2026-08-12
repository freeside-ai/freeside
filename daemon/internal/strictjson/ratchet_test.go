package strictjson_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// structuralTokenWalkers are the deliberately different JSON token walkers.
// They inspect duplicate keys or prebound collection sizes rather than decode
// one typed value, so routing them through strictjson would erase their gate.
var structuralTokenWalkers = map[string]map[string]bool{
	"internal/importer/commit_plan.go": {
		"scanCommitPlanStrings":   true,
		"preboundCommitPlan":      true,
		"preboundCommitPlanGroup": true,
	},
	"internal/importer/evidence.go": {
		"manifestEntryCountExceeds": true,
		"skipJSONValue":             true,
		"skipUntilJSONClose":        true,
	},
	"internal/inference/client.go": {
		"rejectDuplicateKeys": true,
	},
	"internal/publish/janitor_snapshot.go": {
		"rejectDuplicateJSONKeys": true,
	},
	"internal/store/local_backup.go": {
		"rejectBackupDuplicateJSONKeys": true,
		"checkBackupJSONValue":          true,
	},
	"internal/ward/runtime_cli.go": {
		"RejectDuplicateJSONKeys": true,
		"checkJSONValue":          true,
	},
}

// nonJSONDecodeMethods names method calls spelled Decode in files that also
// import encoding/json but whose receiver is a different, reviewed codec.
var nonJSONDecodeMethods = map[string]map[string]bool{
	"internal/ward/codex_review.go": {
		"jwtExpiry": true, // base64.Encoding.Decode
	},
}

func inspectJSONFile(rel string, body []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), rel, body, 0)
	if err != nil {
		return nil, err
	}
	jsonPackages, dotImported := encodingJSONPackages(file)
	if dotImported {
		return []string{"dot-imports encoding/json"}, nil
	}
	if len(jsonPackages) == 0 {
		return nil, nil
	}
	importedPackages := importedPackageNames(file)

	var findings []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		decoderVariables := jsonDecoderVariables(function, jsonPackages)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.AssignStmt:
				for index, rhs := range node.Rhs {
					if index >= len(node.Lhs) || !isJSONNewDecoder(rhs, jsonPackages) {
						continue
					}
					if name, ok := node.Lhs[index].(*ast.Ident); ok {
						decoderVariables[name.Name] = true
					}
				}
			case *ast.ValueSpec:
				for index, value := range node.Values {
					if index < len(node.Names) && isJSONNewDecoder(value, jsonPackages) {
						decoderVariables[node.Names[index].Name] = true
					}
				}
			}
			return true
		})
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if node, ok := node.(*ast.CallExpr); ok {
				selector, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// No typed encoding/json Decoder.Decode call belongs outside
				// strictjson. This deliberately catches extracted EOF helpers and
				// decoders returned or held through shapes local tracking cannot type.
				if selector.Sel.Name == "Decode" &&
					!isPackageSelector(selector.X, importedPackages) &&
					!nonJSONDecodeMethods[rel][function.Name.Name] {
					findings = append(findings, function.Name.Name+": typed Decode outside strictjson")
					return true
				}
				if !isJSONDecoderReceiver(selector.X, decoderVariables, jsonPackages) {
					return true
				}
				switch selector.Sel.Name {
				case "DisallowUnknownFields":
					findings = append(findings, function.Name.Name+": DisallowUnknownFields")
				case "Token":
					if !structuralTokenWalkers[rel][function.Name.Name] {
						findings = append(findings, function.Name.Name+": unregistered JSON token walker")
					}
				}
			}
			return true
		})
	}
	return findings, nil
}

func importedPackageNames(file *ast.File) map[string]bool {
	packages := make(map[string]bool)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name != "." && name != "_" {
			packages[name] = true
		}
	}
	return packages
}

func isPackageSelector(expression ast.Expr, packages map[string]bool) bool {
	name, ok := expression.(*ast.Ident)
	return ok && packages[name.Name]
}

func encodingJSONPackages(file *ast.File) (map[string]bool, bool) {
	packages := make(map[string]bool)
	dotImported := false
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err == nil && path == "encoding/json" {
			if spec.Name == nil {
				packages["json"] = true
			} else if spec.Name.Name == "." {
				dotImported = true
			} else if spec.Name.Name != "_" {
				packages[spec.Name.Name] = true
			}
		}
	}
	return packages, dotImported
}

func jsonDecoderVariables(function *ast.FuncDecl, jsonPackages map[string]bool) map[string]bool {
	variables := make(map[string]bool)
	if function.Type.Params == nil {
		return variables
	}
	for _, field := range function.Type.Params.List {
		pointer, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		selector, ok := pointer.X.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Decoder" {
			continue
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok || !jsonPackages[qualifier.Name] {
			continue
		}
		for _, name := range field.Names {
			variables[name.Name] = true
		}
	}
	return variables
}

func isJSONNewDecoder(expression ast.Expr, jsonPackages map[string]bool) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "NewDecoder" {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	return ok && jsonPackages[qualifier.Name]
}

func isJSONDecoderReceiver(
	expression ast.Expr,
	variables map[string]bool,
	jsonPackages map[string]bool,
) bool {
	if name, ok := expression.(*ast.Ident); ok {
		return variables[name.Name]
	}
	return isJSONNewDecoder(expression, jsonPackages)
}

func TestStrictJSONDecodingIsCentralized(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate ratchet test source")
	}
	daemonRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	strictJSONDir := filepath.Dir(sourceFile)

	err := filepath.WalkDir(daemonRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == strictJSONDir {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(daemonRoot, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path) //nolint:gosec // repository files under the test's daemon root
		if err != nil {
			return err
		}
		findings, err := inspectJSONFile(filepath.ToSlash(rel), body)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		for _, finding := range findings {
			t.Errorf("%s %s", filepath.ToSlash(rel), finding)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan daemon source: %v", err)
	}
}

func TestForbiddenJSONPatternDetection(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"strict decoder", `package p; import "encoding/json"; func f(d *json.Decoder) { d.DisallowUnknownFields() }`, true},
		{"direct strict decoder", `package p; import "encoding/json"; import "strings"; func f() { json.NewDecoder(strings.NewReader("{}")).DisallowUnknownFields() }`, true},
		{"declared strict decoder", `package p; import "encoding/json"; import "strings"; func f() { var d = json.NewDecoder(strings.NewReader("{}")); d.DisallowUnknownFields() }`, true},
		{"raw-message trailing decode", `package p; import "encoding/json"; func f(d *json.Decoder) { _ = d.Decode(new(json.RawMessage)) }`, true},
		{"empty-struct trailing decode", `package p; import "encoding/json"; func f(d *json.Decoder) { _ = d.Decode(&struct{}{}) }`, true},
		{"extracted trailing helper", `package p; import "encoding/json"; import "io"; func eof(d *json.Decoder) bool { var extra any; return d.Decode(&extra) == io.EOF }`, true},
		{"helper-returned decoder", `package p; import "encoding/json"; func decoder() *json.Decoder { return nil }; func f() { var v any; _ = decoder().Decode(&v) }`, true},
		{"selector-held decoder", `package p; import "encoding/json"; type holder struct { decoder *json.Decoder }; func f(h holder) { var v any; _ = h.decoder.Decode(&v) }`, true},
		{"unregistered token walker", `package p; import "encoding/json"; func f(d *json.Decoder) { _, _ = d.Token() }`, true},
		{"ordinary unmarshal", `package p; import "encoding/json"; func f(b []byte, v any) { _ = json.Unmarshal(b, v) }`, false},
		{"other decoder package", `package p; type decoder struct{}; func (decoder) Token(){}; func f(d decoder) { d.Token() }`, false},
		{"dot import", `package p; import . "encoding/json"; func f() { _ = NewDecoder }`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings, err := inspectJSONFile("fixture.go", []byte(tt.body))
			if err != nil {
				t.Fatalf("inspect fixture: %v", err)
			}
			if got := len(findings) > 0; got != tt.want {
				t.Fatalf("inspectJSONFile() found = %v, want %v (%v)", got, tt.want, findings)
			}
		})
	}
}
