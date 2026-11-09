package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// oldLayoutCheckout commits the baseline tree as adoption rendered it before
// #2599, agent and offer documents included, and returns the checkout and the
// commit. The fixture is what agentbaseline rendered at main cda293d14 for
// two writers, both review agents, and the judgment call agent, in that
// package's golden format: "=== <path>" headers, each followed by the file's
// bytes.
func oldLayoutCheckout(t *testing.T) (checkout, commit string) {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join("testdata", "migrate_tree_old_layout.txt"))
	if err != nil {
		t.Fatal(err)
	}
	checkout = t.TempDir()
	sections := strings.Split(strings.TrimPrefix(string(fixture), "=== "), "\n=== ")
	for i, section := range sections {
		path, body, ok := strings.Cut(section, "\n")
		if !ok {
			t.Fatalf("fixture section %q has no body", section)
		}
		// Split removed the newline that ends every file but the last.
		if i < len(sections)-1 {
			body += "\n"
		}
		full := filepath.Join(checkout, agenttree.Root, filepath.FromSlash(path))
		//nolint:gosec // G703: a test-owned temporary directory and a checked-in fixture path.
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		//nolint:gosec // G703: a test-owned temporary directory and a checked-in fixture path.
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitForAdoptTest(t, checkout, nil, "init", "-q")
	gitForAdoptTest(t, checkout, nil, "add", ".")
	gitForAdoptTest(t, checkout, nil, "commit", "-q", "-m", "old layout")
	return checkout, gitForAdoptTest(t, checkout, nil, "rev-parse", "HEAD")
}

func runMigrateTree(t *testing.T, args ...string) (patch []byte, report authMigrateTreeReport) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runAuthCommand(context.Background(), append([]string{"migrate-tree"}, args...),
		nil, &stdout, &stderr); err != nil {
		t.Fatalf("migrate-tree: %v\n%s", err, stderr.String())
	}
	if err := json.Unmarshal(stderr.Bytes(), &report); err != nil {
		t.Fatalf("report %q: %v", stderr.String(), err)
	}
	return stdout.Bytes(), report
}

// The proof is end to end, as the operator runs it: migrate a committed
// old-layout tree, commit the patch, and read a tree the daemon parses. The
// migrated commit then migrates to itself.
func TestAuthMigrateTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	checkout, commit := oldLayoutCheckout(t)
	if _, err := agenttree.ReadCommit(ctx, checkout, t.TempDir(), commit); err != nil {
		t.Fatalf("read old layout: %v", err)
	}
	if _, _, err := readAgentTree(ctx, checkout, commit); err == nil ||
		!strings.Contains(err.Error(), "freesided auth migrate-tree") {
		t.Fatalf("daemon read of the old layout = %v, want a refusal naming migrate-tree", err)
	}

	patch, report := runMigrateTree(t, "-agent-tree", checkout, "-agent-tree-commit", commit)
	golden.Assert(t, "migrate_tree_patch", patch)
	if !report.Changed || report.Patch != "stdout" {
		t.Fatalf("report = %+v, want a changed tree on stdout", report)
	}

	gitForAdoptTest(t, checkout, patch, "apply", "--index", "-")
	gitForAdoptTest(t, checkout, nil, "commit", "-q", "-m", "migrate")
	migrated := gitForAdoptTest(t, checkout, nil, "rev-parse", "HEAD")
	tree, revision, err := readAgentTree(ctx, checkout, migrated)
	if err != nil {
		t.Fatalf("daemon read of the migrated tree: %v", err)
	}
	if revision != report.LineupRevision {
		t.Fatalf("migrated revision %s, report says %s", revision, report.LineupRevision)
	}
	if len(tree.Lineup) == 0 {
		t.Fatal("migrated tree has no lineup")
	}
	if listing := gitForAdoptTest(t, checkout, nil, "ls-files", agenttree.Root); strings.Contains(listing, "agents/") ||
		strings.Contains(listing, "offers/") {
		t.Fatalf("migrated tree still holds removed documents:\n%s", listing)
	}

	again, rerun := runMigrateTree(t, "-agent-tree", checkout, "-agent-tree-commit", migrated)
	if len(again) != 0 || rerun.Changed || rerun.LineupRevision != revision {
		t.Fatalf("migrating a migrated tree = %q, %+v", again, rerun)
	}
}

func TestAuthMigrateTreeRefusals(t *testing.T) {
	t.Parallel()
	checkout, commit := oldLayoutCheckout(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no checkout", []string{"-agent-tree-commit", commit}, "-agent-tree and -agent-tree-commit are required"},
		{"no commit", []string{"-agent-tree", checkout}, "-agent-tree and -agent-tree-commit are required"},
		{"a branch name", []string{"-agent-tree", checkout, "-agent-tree-commit", "HEAD"}, "not an exact commit"},
		{
			"a patch inside the checkout",
			[]string{"-agent-tree", checkout, "-agent-tree-commit", commit, "-patch", filepath.Join(checkout, "p")},
			"inside the checkout",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			err := runAuthCommand(context.Background(), append([]string{"migrate-tree"}, tc.args...),
				nil, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("migrate-tree %v = %v, want %q", tc.args, err, tc.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("a refused migration printed %q", stdout.String())
			}
		})
	}
}
