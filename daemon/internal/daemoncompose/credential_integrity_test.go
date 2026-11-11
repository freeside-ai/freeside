package daemoncompose

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/operations"
)

// TestCredentialIntegrityProbeRunOnce: each call observes once and hands back
// a replay of that result, so a later pass never reuses an earlier pass's
// observation, and no probe stays no probe.
func TestCredentialIntegrityProbeRunOnce(t *testing.T) {
	t.Parallel()
	if replay := CredentialIntegrityProbe(nil).runOnce(t.Context()); replay != nil {
		t.Error("a nil probe produced a replay, so a pass with no probe would claim it probed")
	}
	calls := 0
	failure := errors.New("probe failed")
	probe := CredentialIntegrityProbe(func(context.Context) ([]operations.CredentialIntegrityOutcome, error) {
		calls++
		if calls == 2 {
			return nil, failure
		}
		return []operations.CredentialIntegrityOutcome{{EnrollmentID: "claude-main/claude_code"}}, nil
	})
	first := probe.runOnce(t.Context())
	if calls != 1 {
		t.Fatalf("runOnce observed %d times, want once before it returns", calls)
	}
	for range 2 {
		outcomes, err := first(t.Context())
		if err != nil || len(outcomes) != 1 || calls != 1 {
			t.Fatalf("replay = %+v, %v after %d observations, want the first result and no new observation", outcomes, err, calls)
		}
	}
	second := probe.runOnce(t.Context())
	if _, err := second(t.Context()); !errors.Is(err, failure) || calls != 2 {
		t.Errorf("second pass = %v after %d observations, want a fresh observation's own error", err, calls)
	}
}
