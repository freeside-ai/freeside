package observedb

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// wantSurface is every exported name this package may have. The follow view's
// containment proof (internal/observe/containment_test.go) bounds which
// packages it can name, never which methods of a permitted package it calls,
// so it stops here and rests on this surface staying small. That makes the
// surface the load-bearing claim, and a claim in a comment is one a later
// edit walks past; this pins it.
var wantSurface = map[string]bool{
	"Store":            true,
	"Open":             true,
	"Store.ObserveRun": true,
	"Store.Close":      true,
}

// TestExportedSurfaceStaysNarrow fails when this package grows an exported
// name. Adding one is not forbidden, but it widens what the follow path can
// reach, so it is a deliberate edit to wantSurface with the containment
// reasoning in view, never a silent addition.
func TestExportedSurfaceStaysNarrow(t *testing.T) {
	got := map[string]bool{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		collectExported(t, fset, name, nil, got)
	}
	if files == 0 {
		t.Fatal("no non-test Go files found; the surface assertion would pass vacuously")
	}
	for name := range got {
		if !wantSurface[name] {
			t.Errorf("package exports %s, which widens what the follow path can reach; "+
				"add it to wantSurface deliberately or keep it unexported", name)
		}
	}
	for name := range wantSurface {
		if !got[name] {
			t.Errorf("expected export %s is gone; wantSurface is stale", name)
		}
	}
}

// collectExported records path's exported surface into `into`. src is nil for
// a real file and non-nil only for the synthetic sources that prove this
// collector sees what it claims to.
func collectExported(
	t *testing.T, fset *token.FileSet, path string, src any, into map[string]bool,
) {
	t.Helper()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			if d.Recv == nil {
				into[d.Name.Name] = true
				continue
			}
			into[receiverName(d.Recv)+"."+d.Name.Name] = true
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				collectSpec(spec, into)
			}
		}
	}
}

// collectSpec records an exported type, var, or const, and then everything
// that type itself exposes. A name alone is not the surface: an exported
// field (`Raw *store.Store`) hands a caller the wrapped API directly, an
// embedded type promotes its whole method set onto this one, and an exported
// interface's methods are callable the same way. Each is a route to the
// store's write, checkpoint, and restore methods that changes no import, so
// each is enumerated rather than assumed away.
func collectSpec(spec ast.Spec, into map[string]bool) {
	for _, name := range specNames(spec) {
		if name.IsExported() {
			into[name.Name] = true
		}
	}
	typeSpec, ok := spec.(*ast.TypeSpec)
	if !ok || !typeSpec.Name.IsExported() {
		return
	}
	switch t := typeSpec.Type.(type) {
	case *ast.StructType:
		collectFields(typeSpec.Name.Name, t.Fields, into)
	case *ast.InterfaceType:
		collectFields(typeSpec.Name.Name, t.Methods, into)
	}
}

// collectFields records the exported members a type exposes. An anonymous
// member is an embedding, so it is recorded under the embedded type's name
// with an explicit marker: its promoted methods are reachable without ever
// naming them here, which is exactly the case a name-only pin would miss.
func collectFields(owner string, fields *ast.FieldList, into map[string]bool) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		if len(field.Names) == 0 {
			into[owner+" embeds "+typeName(field.Type)] = true
			continue
		}
		for _, name := range field.Names {
			if name.IsExported() {
				into[owner+"."+name.Name] = true
			}
		}
	}
}

// typeName renders an embedded type's name for the surface record, following
// pointers and qualified names so `*store.Store` reads as store.Store.
func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		return typeName(t.X) + "." + t.Sel.Name
	case *ast.Ident:
		return t.Name
	default:
		return "unknown"
	}
}

func receiverName(recv *ast.FieldList) string {
	if len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

func specNames(spec ast.Spec) []*ast.Ident {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return []*ast.Ident{s.Name}
	case *ast.ValueSpec:
		return s.Names
	default:
		return nil
	}
}

// TestSurfaceCollectorSeesFieldsAndEmbedding proves the collector above
// actually catches the routes it claims to. Without this, the surface pin
// would be another unverified assertion, which is the exact mistake that made
// three review rounds necessary: a check that looks like a boundary and holds
// nothing. Each case is a way to hand a caller the wrapped store's write,
// checkpoint, and restore methods while changing no import.
func TestSurfaceCollectorSeesFieldsAndEmbedding(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{
			name: "exported field",
			src:  "package p\ntype Store struct {\n\tRaw *store.Store\n}\n",
			want: "Store.Raw",
		},
		{
			name: "embedded pointer",
			src:  "package p\ntype Store struct {\n\t*store.Store\n}\n",
			want: "Store embeds store.Store",
		},
		{
			name: "embedded value",
			src:  "package p\ntype Store struct {\n\tstore.Store\n}\n",
			want: "Store embeds store.Store",
		},
		{
			name: "interface method",
			src:  "package p\ntype Reader interface {\n\tWriteInternal() error\n}\n",
			want: "Reader.WriteInternal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]bool{}
			collectExported(t, token.NewFileSet(), "synthetic.go", tc.src, got)
			if !got[tc.want] {
				t.Fatalf("collector missed %q; it recorded %v", tc.want, keys(got))
			}
			if wantSurface[tc.want] {
				t.Fatalf("%q is in wantSurface, so this case proves nothing", tc.want)
			}
		})
	}

	// An unexported field is not a route: a caller outside this package
	// cannot name it, so recording one would only make the pin noisy.
	got := map[string]bool{}
	collectExported(t, token.NewFileSet(), "synthetic.go",
		"package p\ntype Store struct {\n\traw *store.Store\n}\n", got)
	if got["Store.raw"] {
		t.Error("collector recorded an unexported field")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
