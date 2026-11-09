package strictjson_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
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
	findings := jsonV2DecodeFindings(file)
	jsonPackages, dotImported := encodingJSONPackages(file)
	if dotImported {
		return append(findings, "dot-imports encoding/json"), nil
	}
	if len(jsonPackages) == 0 {
		return findings, nil
	}
	importedPackages := importedPackageNames(file)

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

// jsonV2DecodeEntryPoints maps each encoding/json/v2-era package to the
// names that start a decode. Any reference counts, not only a call, so a
// function value cannot carry a decode past the scan. The jsontext.Decoder
// type is listed because a zero Decoder and Reset build one without
// NewDecoder. v1's json.Unmarshal stays allowed; it has no v2 options to
// drift from strictjson's.
var jsonV2DecodeEntryPoints = map[string]map[string]bool{
	"encoding/json/v2": {
		"Unmarshal":       true,
		"UnmarshalDecode": true,
		"UnmarshalRead":   true,
	},
	"encoding/json/jsontext": {
		"NewDecoder": true,
		"Decoder":    true,
	},
}

// jsonV2DefaultNames are the package names the v2 import paths declare;
// filepath.Base would name encoding/json/v2 "v2".
var jsonV2DefaultNames = map[string]string{
	"encoding/json/v2":       "json",
	"encoding/json/jsontext": "jsontext",
}

func jsonV2DecodeFindings(file *ast.File) []string {
	var findings []string
	entryPoints := make(map[string]map[string]bool)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || jsonV2DecodeEntryPoints[path] == nil {
			continue
		}
		name := jsonV2DefaultNames[path]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		switch name {
		case ".":
			findings = append(findings, "dot-imports "+path)
		case "_":
		default:
			entryPoints[name] = jsonV2DecodeEntryPoints[path]
		}
	}
	if len(entryPoints) == 0 {
		return findings
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if ok && entryPoints[qualifier.Name][selector.Sel.Name] {
			findings = append(findings, qualifier.Name+"."+selector.Sel.Name+": encoding/json/v2 decode outside strictjson")
		}
		return true
	})
	return findings
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
	// go test runs a package with its directory as the working directory;
	// under -trimpath, runtime.Caller returns import paths, not file paths.
	strictJSONDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	daemonRoot := filepath.Clean(filepath.Join(strictJSONDir, "..", ".."))

	err = filepath.WalkDir(daemonRoot, func(path string, entry fs.DirEntry, walkErr error) error {
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
		{"v2 unmarshal", `package p; import "encoding/json/v2"; func f(b []byte, v any) { _ = json.Unmarshal(b, v) }`, true},
		{"v2 unmarshal decode", `package p; import "encoding/json/v2"; import "encoding/json/jsontext"; func f(d *jsontext.Decoder, v any) { _ = json.UnmarshalDecode(d, v) }`, true},
		{"v2 unmarshal read", `package p; import "encoding/json/v2"; import "io"; func f(r io.Reader, v any) { _ = json.UnmarshalRead(r, v) }`, true},
		{"aliased v2 unmarshal", `package p; import jsonv2 "encoding/json/v2"; func f(b []byte, v any) { _ = jsonv2.Unmarshal(b, v) }`, true},
		{"v2 unmarshal function value", `package p; import "encoding/json/v2"; var decode = json.Unmarshal`, true},
		{"jsontext decoder", `package p; import "encoding/json/jsontext"; import "strings"; func f() { _ = jsontext.NewDecoder(strings.NewReader("{}")) }`, true},
		{"aliased jsontext decoder", `package p; import jt "encoding/json/jsontext"; import "strings"; func f() { _ = jt.NewDecoder(strings.NewReader("{}")) }`, true},
		{"v2 dot import", `package p; import . "encoding/json/v2"; func f() { _ = Marshal }`, true},
		{"jsontext dot import", `package p; import . "encoding/json/jsontext"; func f() { _ = NewEncoder }`, true},
		{"zero jsontext decoder", `package p; import "encoding/json/jsontext"; import "io"; func f(r io.Reader) { var d jsontext.Decoder; d.Reset(r) }`, true},
		{"jsontext decoder parameter", `package p; import "encoding/json/jsontext"; func f(d *jsontext.Decoder) { _, _ = d.ReadToken() }`, true},
		{"v1 unmarshal beside v2 marshal", `package p; import jsonv1 "encoding/json"; import "encoding/json/v2"; func f(b []byte, v any) ([]byte, error) { _ = jsonv1.Unmarshal(b, v); return json.Marshal(v) }`, false},
		{"v2 marshal", `package p; import "encoding/json/v2"; func f(v any) ([]byte, error) { return json.Marshal(v) }`, false},
		{"jsontext encoder", `package p; import "encoding/json/jsontext"; import "io"; func f(w io.Writer) { _ = jsontext.NewEncoder(w) }`, false},
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
