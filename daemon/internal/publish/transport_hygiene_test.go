package publish

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scanTreeForTokenForms walks every file under root and fails on any
// occurrence of the token in raw or base64 form.
func scanTreeForTokenForms(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: test-owned fixture tree
		if err != nil {
			return err
		}
		for _, form := range stubTokenForms() {
			if strings.Contains(string(data), form) {
				t.Errorf("token bytes on disk in %s", path)
			}
		}
		if strings.Contains(strings.ToLower(string(data)), "extraheader") {
			t.Errorf("credential header config persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTransportLeavesNoTokenOnDisk drives the full fetch-then-push
// flow against real git and then byte-scans everything that survives
// the calls — the daemon-owned checkout (including .git and its
// config) and the remote repository — for the token in any form, and
// for any persisted credential header config. Acceptance 2's
// never-lands-on-disk property, proven on the real flow rather than
// the stub.
func TestTransportLeavesNoTokenOnDisk(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	if _, err := remote.transport.PushHead(t.Context(), co, testIdentityInput(remote.repo, head)); err != nil {
		t.Fatal(err)
	}
	scanTreeForTokenForms(t, co.Dir())
	scanTreeForTokenForms(t, remote.bare)
}

// TestPinRepoRefusesForeignGitDir is the discovery-walk-up
// refutation: a checkout directory that holds no repository but sits
// under some ancestor repository must fail closed, never silently pin
// the ancestor — an authenticated push under a foreign repository
// would honor that repository's local config.
func TestPinRepoRefusesForeignGitDir(t *testing.T) {
	outer := t.TempDir()
	gitOut(t, "", "init", "--initial-branch=main", outer)
	inner := filepath.Join(outer, "nested", "checkout")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := newNetRunner("git", t.TempDir(), "file")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.pinRepo(t.Context(), inner); err == nil {
		t.Fatal("pinRepo accepted a directory whose repository is a foreign ancestor")
	}
	for _, e := range r.env {
		if strings.HasPrefix(e, "GIT_DIR=") {
			t.Errorf("refused pin still recorded a git dir: %q", e)
		}
	}
}

// TestFetchBaseFailureDoesNotWedgeTheDir proves a failed
// materialization removes the repository it created, so a retry of
// the same checkout path is not refused by the existing-repository
// guard.
func TestFetchBaseFailureDoesNotWedgeTheDir(t *testing.T) {
	remote := newLocalRemote(t)
	dir := checkoutDir(t)
	if _, err := remote.transport.FetchBase(t.Context(), remote.repo, "absent", remote.baseSHA, dir); err == nil {
		t.Fatal("fetch of an absent branch succeeded")
	}
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, dir)
	if err != nil {
		t.Fatalf("retry after a failed materialization: %v", err)
	}
	if co.BaseSHA() != remote.baseSHA {
		t.Errorf("retry checkout = %+v", co)
	}
}

// TestPushRefusesRedirectingCheckoutConfig is the credential-redirect
// refutation: a checkout whose local config carries url.*.insteadOf
// (or any other key the daemon did not write) would have git rewrite
// the explicit repository URL and carry the installation-token header
// to the substituted host. Every such key must fail the invocation
// closed, before any token is minted.
func TestPushRefusesRedirectingCheckoutConfig(t *testing.T) {
	hostile := map[string]string{
		"url.file:///evil.insteadOf":         "file://",
		"http.https://evil.test.extraHeader": "X-Evil: 1",
		"credential.helper":                  "!echo evil",
		"remote.origin.url":                  "file:///evil",
		"include.path":                       "/tmp/evil-config",
		"core.sshCommand":                    "/bin/false",
	}
	for key, value := range hostile {
		t.Run(key, func(t *testing.T) {
			remote := newLocalRemote(t)
			counter := &countingTokenSource{}
			tr := &Transport{tokens: counter, gitPath: "git", remoteBase: remote.transport.remoteBase, scheme: "file"}
			co, err := tr.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
			if err != nil {
				t.Fatal(err)
			}
			head := candidateHead(t, co)
			mints := counter.mints
			gitOut(t, co.Dir(), "config", "--local", key, value)
			if _, err := tr.PushHead(t.Context(), co, testIdentityInput(remote.repo, head)); err == nil {
				t.Fatalf("PushHead ran against a checkout carrying %s", key)
			}
			if counter.mints != mints {
				t.Errorf("refused push still minted a token (%d → %d)", mints, counter.mints)
			}
			refs := gitOut(t, remote.bare, "for-each-ref", "--format=%(refname)")
			if refs != "refs/heads/main" {
				t.Errorf("refused push had a remote effect: %s", refs)
			}
		})
	}
}

// TestAuthenticatedInvocationRechecksConfig pins that the config
// allowlist is bound to the authenticated invocation itself, not to a
// call site: a redirecting key introduced after the call-site gate
// (the window a token mint would otherwise sit inside) must still
// stop the credentialed process from starting.
func TestAuthenticatedInvocationRechecksConfig(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	r, err := newNetRunner("git", t.TempDir(), "file")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.pinRepo(t.Context(), co.Dir()); err != nil {
		t.Fatal(err)
	}
	// The call-site gate passes here...
	if err := r.assertPristineConfig(t.Context()); err != nil {
		t.Fatalf("freshly fetched checkout failed the gate: %v", err)
	}
	// ...and the redirect is introduced afterwards, as a concurrent
	// writer would during the token mint.
	gitOut(t, co.Dir(), "config", "--local", "url.file:///evil.insteadOf", "file://")
	url := remote.transport.remoteBase + "/" + remote.repo + ".git"
	if _, _, err := r.runAuthed(t.Context(), stubToken(), "ls-remote", url, "refs/heads/main"); err == nil {
		t.Error("an authenticated invocation ran against a config modified after the call-site gate")
	}
	// The unauthenticated path is unchanged: no credential, no gate.
	if _, _, err := r.run(t.Context(), nil, "rev-parse", "HEAD"); err != nil {
		t.Errorf("unauthenticated invocation should not be gated: %v", err)
	}
}

// TestPushRefusesCheckoutFromAnotherTransport pins per-instance
// provenance: a checkout fetched by a transport pointed at a
// different endpoint must not be pushable by this one, however alike
// the repository and base-ref labels look.
func TestPushRefusesCheckoutFromAnotherTransport(t *testing.T) {
	remote := newLocalRemote(t)
	other := newLocalRemote(t)
	// Same repo and base-ref labels, different endpoint.
	other.repo = remote.repo
	co, err := other.transport.FetchBase(t.Context(), other.repo, "main", other.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	if _, err := remote.transport.PushHead(t.Context(), co, testIdentityInput(remote.repo, head)); err == nil {
		t.Error("PushHead accepted a checkout minted by a different transport instance")
	}
	if refs := gitOut(t, remote.bare, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main" {
		t.Errorf("refused push had a remote effect: %s", refs)
	}
}

// TestPushLeaseRefusesExistingRefAtProtocolLevel bypasses PushHead's
// ls-remote pre-check and drives the raw push argv against a remote
// whose branch already exists — the exact state a race would produce
// between observation and push. The empty --force-with-lease
// expectation must refuse the update at the protocol level (classified
// stale-lease) and the remote ref must not move, even though the
// existing ref is an ancestor a plain push would happily fast-forward.
func TestPushLeaseRefusesExistingRefAtProtocolLevel(t *testing.T) {
	remote := newLocalRemote(t)
	co, err := remote.transport.FetchBase(t.Context(), remote.repo, "main", remote.baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatal(err)
	}
	head := candidateHead(t, co)
	branch := "freeside/publish/raced"
	gitOut(t, remote.work, "push", remote.bare, remote.baseSHA+":refs/heads/"+branch)

	scratch := t.TempDir()
	r, err := newNetRunner("git", scratch, "file")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.pinRepo(t.Context(), co.Dir()); err != nil {
		t.Fatal(err)
	}
	url := remote.transport.remoteBase + "/" + remote.repo + ".git"
	_, _, pushErr := r.runAuthed(t.Context(), stubToken(), pushArgs(url, head, branch)...)
	if pushErr == nil {
		t.Fatal("raced push succeeded; the create-only lease did not hold")
	}
	var tge *TransportGitError
	if !errors.As(pushErr, &tge) || tge.Refusal != RefusalStaleLease {
		t.Errorf("refusal = %v, want stale-lease", pushErr)
	}
	if got := gitOut(t, remote.bare, "rev-parse", "refs/heads/"+branch); got != remote.baseSHA {
		t.Errorf("remote ref moved to %s during a refused push", got)
	}
}
