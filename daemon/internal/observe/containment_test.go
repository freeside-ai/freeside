package observe

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// permittedImports is the complete set this package's non-test files may
// import. It is an allowlist, not a denylist of dangerous packages, because a
// denylist has to anticipate every way to name a file or a process (os,
// os/exec, io/fs, path/filepath, syscall, net, a second database/sql driver)
// and silently admits the one it forgot.
//
// internal/store is deliberately absent. Permitting it would admit its whole
// method surface, including WriteInternal, Checkpoint, Restore, and the
// backup files, none of which a run observer has any business holding; the
// follow path reaches the database only through observe/observedb, whose
// three exported functions open, read one aggregate, and close.
var permittedImports = map[string]bool{
	"context": true,
	"errors":  true,
	"flag":    true,
	"fmt":     true,
	"io":      true,
	"slices":  true,
	"strconv": true,
	"strings": true,
	"time":    true,

	"github.com/freeside-ai/freeside/daemon/internal/domain":            true,
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb": true,
}

// TestFollowReachesNoWriterSurface is the containment boundary, pinned
// mechanically rather than described. Monitoring must never read a live
// writer's filesystem, stdout, stderr, or transcript (#394's contract,
// #409's acceptance), and this package is the whole of the follow verb, so
// what it can reach is what the command can reach.
//
// The allowlist above carries the proof. It names no way to open a file
// (no os, io/fs, or path/filepath), no way to start a process (no os/exec or
// syscall), and no way to open a socket (no net). The only module packages in
// it are the observation vocabulary and the store, whose reachable surface is
// the daemon's own durable records; no stage driver, container runtime,
// workspace, or artifact store is reachable at all. The single filesystem
// path the command handles is the operator's -db string, which it passes to
// store.Open and never opens itself.
//
// The limit worth stating, because two review rounds were spent on weaker
// forms of this assertion: an import allowlist bounds which packages a caller
// can name, never which methods of a permitted package it calls. So the proof
// only reaches as far as the smallest permitted surface, and the regress has
// to stop somewhere a human can check by eye. It stops at observe/observedb:
// one short file exporting open, observe-one-run, and close. Opening the
// operator's -db path is the intended capability, and nothing here
// distinguishes a careful use of it from a careless one.
func TestFollowReachesNoWriterSurface(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	files := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" ||
			len(name) > 8 && name[len(name)-8:] == "_test.go" {
			continue
		}
		files++
		assertImportsPermitted(t, fset, name)
	}
	// Guard against the assertion passing because it inspected nothing: a
	// rename or a move would otherwise turn the boundary off silently.
	if files < 2 {
		t.Fatalf("inspected %d non-test files; expected the display and the command", files)
	}
}

func assertImportsPermitted(t *testing.T, fset *token.FileSet, path string) {
	t.Helper()
	file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("parse %s import %s: %v", path, spec.Path.Value, err)
		}
		if permittedImports[imported] {
			continue
		}
		t.Errorf("%s imports %s, which is outside the containment boundary; "+
			"widening it is a deliberate change to permittedImports, not a passing test",
			path, imported)
	}
}
