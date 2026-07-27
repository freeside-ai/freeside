package publish

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// TestTransportCarriesHostileHandoffCleanly is acceptance 5 end to
// end: a workspace with a hostile .git travels export → hostile
// importer → transport push, and nothing of the workspace's git state
// reaches the pushed history. The fetched checkout is also the proof
// that FetchBase's no-worktree shape satisfies the importer's
// local-checkout contract unchanged.
func TestTransportCarriesHostileHandoffCleanly(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}

	// The agent workspace: one real change plus a poisoned .git the
	// exporter records as inert entries.
	ws := t.TempDir()
	for path, content := range map[string]string{
		"a.txt":                 "two\n",
		".git/hooks/pre-commit": "#!/bin/sh\necho pwned\n",
		".git/config":           "[core]\n\thooksPath = /tmp/evil\n",
	} {
		full := filepath.Join(ws, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handoff := filepath.Join(t.TempDir(), "handoff")
	if _, err := export.Export(os.DirFS(ws), handoff, export.Options{}); err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(filepath.Join(remote.bare, "config"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := importer.Import(t.Context(), handoff, co.Dir(), importer.Options{
		BaseSHA:    co.BaseSHA(),
		CommitDate: time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("importer.Import over the fetched checkout: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("clean change (only a.txt) should have imported")
	}

	in := testIdentityInput(remote.repo, res.CommitSHA)
	branch := testBranch(t, in)
	pushed, err := remote.transport.PushHead(t.Context(), co, testGatedHead(t, remote.transport, in))
	if err != nil {
		t.Fatalf("PushHead: %v", err)
	}
	if !pushed.Created {
		t.Error("push reported Created=false")
	}

	// The remote history is exactly base + the daemon-authored commit.
	wantHistory := res.CommitSHA + "\n" + remote.baseSHA
	if got := gitOut(t, remote.bare, "rev-list", "refs/heads/"+branch); got != wantHistory {
		t.Errorf("remote history:\n%s\nwant exactly:\n%s", got, wantHistory)
	}
	// The change arrived as content; the hostile git state did not.
	if got := gitOut(t, remote.bare, "show", res.CommitSHA+":a.txt"); got != "two" {
		t.Errorf("pushed a.txt = %q", got)
	}
	if gitOut(t, remote.bare, "ls-tree", "-r", "--name-only", res.CommitSHA) != "a.txt" {
		t.Error("pushed tree carries more than the candidate change")
	}
	configAfter, err := os.ReadFile(filepath.Join(remote.bare, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(configBefore) != string(configAfter) {
		t.Error("the remote repository's config changed during the flow")
	}
}
