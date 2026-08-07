package ward

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

type unusedMaterializationTokenSource struct{}

func (unusedMaterializationTokenSource) Token(
	context.Context, string,
) (publish.InstallationToken, error) {
	return publish.InstallationToken{}, fmt.Errorf("materialization unexpectedly requested a token")
}

// TestPublishMaterializationPassesWardBaseProof is the producer/prover
// contract test. The implementations intentionally remain independent: this
// test composes the publish materializer with Ward's actual raw-byte observer
// and strict proof verifier instead of sharing their tree-walking logic.
func TestPublishMaterializationPassesWardBaseProof(t *testing.T) {
	checkout := initLiveSeedCheckout(t, t.TempDir())
	if err := os.WriteFile(
		filepath.Join(checkout, ".gitattributes"),
		[]byte("*.txt ident eol=crlf\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	const rawPayload = "$Id$\nraw payload\n"
	if err := os.WriteFile(filepath.Join(checkout, "payload.txt"), []byte(rawPayload), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "tool.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // executable fixture
		t.Fatal(err)
	}
	base := commitLiveSeedCheckout(t, checkout)

	transport, err := publish.NewTransport(unusedMaterializationTokenSource{}, publish.TransportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(t.TempDir(), "retained")
	if err := transport.RetainWorktree(t.Context(), checkout, retained, base.BaseSHA); err != nil {
		t.Fatal(err)
	}
	if payload, err := os.ReadFile(filepath.Join(retained, "payload.txt")); err != nil || string(payload) != rawPayload { //nolint:gosec // test-owned checkout path
		t.Fatalf("materialized attribute-controlled payload = %q, %v", payload, err)
	}

	scratch := t.TempDir()
	script := "h=\"$(cat " + shellQuote(filepath.Join(retained, ".git", "HEAD")) + ")\"; " +
		"d=yes; s=none; w=error; r=error; " +
		observerGitScript(retained, filepath.Join(scratch, "git")) +
		"printf '%s\\n%s\\n%s\\n' \"$s\" \"$w\" \"$r\""
	shell := "sh"
	if dash, lookErr := osexec.LookPath("dash"); lookErr == nil {
		shell = dash
	}
	cmd := osexec.Command(shell, "-c", script) //nolint:gosec // fixed shell and test-owned script
	cmd.Env = scrubbedLiveGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Ward observer: %v: %s", err, out)
	}
	observations := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(observations) != 3 {
		t.Fatalf("Ward observer output = %q", out)
	}

	const nonce = "materializer-cross-check"
	const treeDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	proof := []byte(fmt.Sprintf(
		"%s=%s\n%s=present\n%s=yes\n%s=%s\n%s=%s\n%s=%s\n%s=absent\n%s=%s\n",
		baseProofNonceKey, nonce,
		baseProofGitDirKey,
		baseProofDetachedKey,
		baseProofSHAKey, observations[0],
		baseProofWorktreeKey, observations[1],
		baseProofReplacementsKey, observations[2],
		baseProofIrregularKey,
		baseProofTreeKey, treeDigest,
	))
	if observed, verifyErr := verifyBaseProof(proof, nonce, treeDigest); verifyErr != nil || observed != base.BaseSHA {
		t.Fatalf("Ward base proof = %q, %v, want %q", observed, verifyErr, base.BaseSHA)
	}
}
