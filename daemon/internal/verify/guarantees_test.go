package verify

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyRunnersImportExec statically pins the execution lens: only
// the hardened git runner and the process room shell out. A new
// os/exec import anywhere else in the package is a structural change
// to the trust story and must show up as a failing guarantee, not a
// silent widening.
func TestOnlyRunnersImportExec(t *testing.T) {
	allowed := map[string]bool{"gitrunner.go": true, "procroom.go": true}
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
			if imp.Path.Value == `"os/exec"` && !allowed[filepath.Base(name)] {
				t.Errorf("%s imports os/exec; only gitrunner.go and procroom.go may shell out", name)
			}
		}
	}
}

// TestVerifyFiresNoHooksAndLeavesCheckoutClean plants executable hooks
// in the daemon-owned checkout and proves a full verification never
// fires them and never disturbs the checkout: config identical, HEAD
// still at base, worktree clean. The hardened runner pins
// core.hooksPath and a scratch index, and this is the executable proof.
func TestVerifyFiresNoHooksAndLeavesCheckoutClean(t *testing.T) {
	checkout, opts, _ := verifyFixture(t, map[string]string{"main.go": "package main\n"}, nil)
	hookDir := filepath.Join(checkout, ".git", "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil { //nolint:gosec // G301: test-owned fixture checkout
		t.Fatalf("mkdir hooks: %v", err)
	}
	sentinel := filepath.Join(t.TempDir(), "hook-fired")
	hook := "#!/bin/sh\necho fired > " + sentinel + "\n"
	for _, name := range []string{"post-checkout", "post-index-change", "pre-auto-gc", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(hookDir, name), []byte(hook), 0o700); err != nil { //nolint:gosec // G306: a hook must be executable; this is the attack we prove inert
			t.Fatalf("write hook %s: %v", name, err)
		}
	}
	configBefore, err := os.ReadFile(filepath.Join(checkout, ".git", "config")) //nolint:gosec // G304: test-owned fixture checkout
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if _, err := Verify(context.Background(), checkout, opts); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Error("a checkout hook fired during verification")
	}
	configAfter, err := os.ReadFile(filepath.Join(checkout, ".git", "config")) //nolint:gosec // G304: test-owned fixture checkout
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(configBefore) != string(configAfter) {
		t.Error("verification changed the checkout's config")
	}
	if got := runGit(t, checkout, "rev-parse", "HEAD"); got != opts.BaseSHA {
		t.Errorf("checkout HEAD moved to %s, want base %s", got, opts.BaseSHA)
	}
	if status := runGit(t, checkout, "status", "--porcelain"); status != "" {
		t.Errorf("checkout status not clean after verification:\n%s", status)
	}
}
