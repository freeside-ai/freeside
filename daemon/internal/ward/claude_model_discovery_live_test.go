package ward

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestLiveClaudeModelDiscovery pins that the pinned image's CLI answers the
// initialize request with its models without running a turn. The CLI does
// contact the provider while it starts, even with nonessential traffic off,
// so the test logs that contact and asserts no result record instead
// (owner decision, issue #2594). An invalid token is enough for an answer; a
// valid one in CLAUDE_CODE_OAUTH_TOKEN is used instead when set, and is never
// logged.
func TestLiveClaudeModelDiscovery(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_WARD_LIVE_TEST=1 and FREESIDE_WARD_CLAUDE_AGENT_IMAGE; requires Apple container running")
	}
	image := os.Getenv("FREESIDE_WARD_CLAUDE_AGENT_IMAGE")
	if image == "" {
		t.Skip("set FREESIDE_WARD_CLAUDE_AGENT_IMAGE to the digest-pinned Claude agent image")
	}
	bin, err := exec.LookPath("container")
	if err != nil {
		t.Fatal(err)
	}
	raw := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if raw == "" {
		var random [48]byte
		if _, err := rand.Read(random[:]); err != nil {
			t.Fatal(err)
		}
		raw = "sk-ant-oat01-" + hex.EncodeToString(random[:])
	}
	token, err := NewSetupToken([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	out, err := ProbeLineupImage(t.Context(), NewCLIRuntime(bin), LineupProbe{
		Harness: domain.HarnessClientClaudeCode, Image: image, Initialize: true,
		Credential: SetupTokenProbeCredential(token),
	}, nil)
	if err != nil {
		t.Fatalf("initialize probe could not run: %v", err)
	}
	stderr := lastLines(out.Stderr)
	if refusalCarriesToken(string(out.Stderr), token) {
		stderr = "(withheld: it carries the token)"
	}
	t.Logf("exit %d, stdout %d bytes, provider reached %v, stderr tail: %s",
		out.ExitStatus, len(out.Stdout), out.ProviderReached, stderr)
	report, err := DecodeClaudeModelReport(out, token)
	if err != nil {
		t.Fatalf("decode the CLI's answer (exit %d): %v", out.ExitStatus, err)
	}
	t.Logf("CLI %s reported %d models", report.CLIBuild, len(report.Models))
	for _, model := range report.Models {
		t.Logf("  %s (%s) efforts %v", model.ID, model.DisplayName, model.EffortLevels)
	}
	for line := range bytes.SplitSeq(out.Stdout, []byte("\n")) {
		var record struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &record) == nil && record.Type == "result" {
			t.Fatal("the initialize probe ran a turn")
		}
	}
}
