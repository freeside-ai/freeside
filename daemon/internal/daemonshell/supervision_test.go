package daemonshell

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func TestExitClassificationIsComplete(t *testing.T) {
	t.Parallel()
	want := map[ComponentKind]exitDisposition{
		ComponentHTTP:                  exitRestartSafe,
		ComponentPairingControl:        exitRestartSafe,
		ComponentWorkflow:              exitDurableStop,
		ComponentLocalBackups:          exitDurableStop,
		ComponentScheduler:             exitDurableStop,
		ComponentProductionPublication: exitDurableStop,
		ComponentActiveResource:        exitDurableStop,
		ComponentLabelIntake:           exitDurableStop,
		ComponentFollowUpFiling:        exitDurableStop,
		ComponentReviewReplies:         exitDurableStop,
		ComponentHandBackLabels:        exitDurableStop,
		ComponentAttentionDeliveries:   exitDurableStop,
		ComponentPanic:                 exitInvoluntary,
	}
	if len(want) != len(AllComponentKinds) {
		t.Fatalf("classification table has %d entries, want %d", len(want), len(AllComponentKinds))
	}
	for _, kind := range AllComponentKinds {
		if !kind.valid() {
			t.Fatalf("registered component kind %q is invalid", kind)
		}
		if got := classifyComponentExit(kind); got != want[kind] {
			t.Errorf("classifyComponentExit(%s) = %s, want %s", kind, got, want[kind])
		}
	}
	for _, disposition := range AllExitDispositions {
		if !disposition.valid() {
			t.Fatalf("registered exit disposition %q is invalid", disposition)
		}
	}
}

// The readiness shape is a daemon-local handshake the app parses with an
// exact key set (#1504), so its keys are pinned here.
func TestReadinessGolden(t *testing.T) {
	t.Parallel()
	got, err := json.MarshalIndent(Readiness{
		APIURL:      "http://127.0.0.1:8421",
		PairingCode: "ABCD-2345",
		Environment: "dev",
		RunID:       "0123456789abcdef0123456789abcdef",
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "readiness", append(got, '\n'))
}

func TestDurableStopReasonExplainsRecovery(t *testing.T) {
	t.Parallel()
	want := "The daemon stopped unattended operation after a health failure requiring a durable stop: janitor failed. " +
		"Acknowledge and Run Doctor leave unattended admission closed. " +
		"Restart the daemon, then choose Resume Unattended on each durable-stop item to reopen admission. " +
		"A restart alone does not reopen it."
	if got := durableStopReason(errors.New("janitor failed")); got != want {
		t.Fatalf("reason = %q", got)
	}
}
