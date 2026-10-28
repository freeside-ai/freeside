package ward

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestLiveClaudeAuthProbe pins the refusal classifier to the actual CLI's
// response through the production proxy. A valid token is optional; its
// output is never logged.
func TestLiveClaudeAuthProbe(t *testing.T) {
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
	rt := NewCLIRuntime(bin)
	probe := func(t *testing.T, raw []byte) (LineupProbeOutput, SetupToken) {
		t.Helper()
		token, err := NewSetupToken(raw)
		if err != nil {
			t.Fatal(err)
		}
		out, err := ProbeLineupImage(t.Context(), rt, LineupProbe{
			Harness: domain.HarnessClientClaudeCode, Image: image,
			Credential: SetupTokenProbeCredential(token),
		}, nil)
		if err != nil {
			t.Fatal("provider probe could not run")
		}
		if !out.ProviderReached {
			t.Fatal("provider probe received no provider bytes")
		}
		return out, token
	}
	t.Run("invalid token", func(t *testing.T) {
		var random [48]byte
		if _, err := rand.Read(random[:]); err != nil {
			t.Fatal(err)
		}
		out, token := probe(t, []byte("sk-ant-oat01-"+hex.EncodeToString(random[:])))
		refusal := JudgeLineupProbe(domain.HarnessClientClaudeCode, "", out).Refusal
		if refusalCarriesToken(refusal, token) {
			refusal = claudeProbeRefusalWithheld
		}
		t.Logf("CLI %s; invalid-token refusal: %s", firstLine(out.Version), refusal)
		if !errors.Is(classifyEnrollmentProbe(out, token), ErrSetupTokenRejected) {
			t.Fatal("invalid setup token did not classify as rejected")
		}
	})
	t.Run("valid token", func(t *testing.T) {
		raw := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
		if raw == "" {
			t.Skip("set CLAUDE_CODE_OAUTH_TOKEN to also prove a valid token passes")
		}
		out, token := probe(t, []byte(raw))
		if classifyEnrollmentProbe(out, token) != nil {
			t.Fatal("valid setup token did not pass the provider probe")
		}
	})
}
