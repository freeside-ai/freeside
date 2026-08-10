package procbound

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const procboundPath = "github.com/freeside-ai/freeside/daemon/internal/procbound"

// unboundSites names the functions that build an *exec.Cmd without this
// package. The enumeration that motivated the helper was a one-time grep,
// and a grep does not survive the session that ran it; this keeps it.
//
// Each entry is a deliberate exemption with the reason it is safe, so the
// list stays short enough to check by eye. An entry that no longer builds
// a command fails too: a stale exemption reads as coverage that is not
// there.
var unboundSites = map[string]string{
	"internal/verify/procroom.go:Run": "hand-rolled bounds predating this package; " +
		"converging it is a follow-up, not this unit (#544)",
	"internal/ward/runtime_cli.go:runTo": "builds the command and hands it straight to " +
		"runPrepared in the same file, which binds it",
}

// TestEveryCommandSiteIsBound is the enumeration as a ratchet. The bug is
// easy to reintroduce and invisible when it is: assigning a bytes.Buffer
// to Stdout is the ordinary way to capture output, and nothing about it
// signals that Wait now blocks on a pipe rather than on the process.
//
// What it proves is narrow and worth stating: that a function building a
// command also names Bind or Run, not that it uses them on the command it
// built or in the right order. It stops a forgotten site, not a misused
// helper; the behavioural tests in this package cover the second.
func TestEveryCommandSiteIsBound(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var found []string
	matched := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The root arrives as "../..", whose base name would read as a
			// dot directory and skip the whole tree.
			if name := d.Name(); path != root && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		execNames := aliasesOf(file, "os/exec")
		if len(execNames) == 0 {
			return nil
		}
		boundNames := aliasesOf(file, procboundPath)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !calls(fn, execNames, "Command", "CommandContext") {
				continue
			}
			if calls(fn, boundNames, "Bind", "Run") {
				continue
			}
			site := filepath.ToSlash(rel) + ":" + fn.Name.Name
			if _, exempt := unboundSites[site]; exempt {
				matched[site] = true
				continue
			}
			found = append(found, site)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk daemon sources: %v", err)
	}

	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("these functions build a subprocess without procbound.Bind or procbound.Run:\n\t%s\n"+
			"os/exec builds a pipe for any non-*os.File Stdout or Stderr, and Wait then blocks on a "+
			"descendant holding that pipe. Bind the command, or exempt it in unboundSites with a reason.",
			strings.Join(found, "\n\t"))
	}
	for site, reason := range unboundSites {
		if !matched[site] {
			t.Errorf("unboundSites exempts %s (%s), but it no longer builds a command there", site, reason)
		}
	}
}

// aliasesOf returns the names under which file refers to an import path,
// covering a renamed import and the same path imported twice.
func aliasesOf(file *ast.File, path string) map[string]bool {
	names := map[string]bool{}
	for _, imp := range file.Imports {
		value, err := strconv.Unquote(imp.Path.Value)
		if err != nil || value != path {
			continue
		}
		if imp.Name != nil {
			names[imp.Name.Name] = true
			continue
		}
		names[value[strings.LastIndex(value, "/")+1:]] = true
	}
	return names
}

func calls(fn *ast.FuncDecl, pkgs map[string]bool, names ...string) bool {
	var hit bool
	ast.Inspect(fn, func(n ast.Node) bool {
		if hit {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || !pkgs[ident.Name] {
			return true
		}
		for _, name := range names {
			if sel.Sel.Name == name {
				hit = true
				return false
			}
		}
		return true
	})
	return hit
}
