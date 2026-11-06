package ward

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestLiveCodexModelDiscovery pins that the pinned image's Codex CLI answers
// the app-server's model/list with at least one model under an access-only
// credential. The CLI lists a bundled set even without a credential (a hand
// run of 0.162.1 with an empty CODEX_HOME), so the test logs whether the
// provider was reached; whether the list then reflects the account is read
// from the logged models. The auth.json is copied into a private root and
// never logged.
//
//nolint:paralleltest // Serial: live suite on the shared Apple container runtime; not run in parallel
func TestLiveCodexModelDiscovery(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_WARD_LIVE_TEST=1, FREESIDE_WARD_CODEX_AGENT_IMAGE, and FREESIDE_WARD_CODEX_REVIEW_AUTH; " +
			"requires Apple container running")
	}
	image, authPath := os.Getenv("FREESIDE_WARD_CODEX_AGENT_IMAGE"), os.Getenv("FREESIDE_WARD_CODEX_REVIEW_AUTH")
	if image == "" || authPath == "" {
		t.Skip("set FREESIDE_WARD_CODEX_AGENT_IMAGE to the digest-pinned Codex image and " +
			"FREESIDE_WARD_CODEX_REVIEW_AUTH to a Codex auth.json")
	}
	bin, err := exec.LookPath("container")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := os.ReadFile(authPath) //nolint:gosec // explicit operator opt-in snapshot, never logged
	if err != nil {
		t.Fatal("read the supplied private auth snapshot")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // fixture is a private directory
		t.Fatal(err)
	}
	snapshot := filepath.Join(root, "auth.json")
	if err := os.WriteFile(snapshot, auth, 0o600); err != nil { //nolint:gosec // test-owned private root
		t.Fatal(err)
	}
	credential, err := ReviewSnapshotProbeCredential(
		domain.HarnessClientCodexCLI, CodexAuthSubscription, root, snapshot, time.Now().UTC())
	if err != nil {
		t.Fatalf("derive the probe credential: %v", err)
	}
	out, err := ProbeLineupImage(t.Context(), NewCLIRuntime(bin), LineupProbe{
		Harness: domain.HarnessClientCodexCLI, Image: image, Initialize: true, Credential: credential,
	}, nil)
	if err != nil {
		t.Fatalf("model/list probe could not run: %v", err)
	}
	secrets, err := codexProbeSecrets(credential)
	if err != nil {
		t.Fatal(err)
	}
	stderr := lastLines(out.Stderr)
	if carriesAnySecretRun(out.Stderr, secrets) {
		stderr = "(withheld: it carries a credential secret)"
	}
	t.Logf("exit %d, stdout %d bytes, provider reached %v, stderr tail: %s",
		out.ExitStatus, len(out.Stdout), out.ProviderReached, stderr)
	report, err := DecodeCodexModelReport(out, credential)
	if err != nil {
		t.Fatalf("decode the CLI's answer (exit %d): %v", out.ExitStatus, err)
	}
	t.Logf("CLI %s reported %d models", report.CLIBuild, len(report.Models))
	for _, model := range report.Models {
		t.Logf("  %s (%s) efforts %v", model.ID, model.DisplayName, model.EffortLevels)
	}
}
