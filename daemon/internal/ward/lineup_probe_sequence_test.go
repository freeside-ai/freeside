package ward

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// lineupProbeContainerPrefix is the probe container's name before its
// per-run ownership suffix.
const lineupProbeContainerPrefix = "freeside-lineup-probe-"

// lineupProbeSequenceSteps are the probe container's steps a failure row
// injects at: the helper-role steps plus the credential copy and the
// delete.
var lineupProbeSequenceSteps = []string{"create", "inspect", "allowlist", "start", "copy", "stopped", "delete"}

// newLineupProbeSequenceRun builds the run probeLineupImage builds, so a test
// can read the container claim between probe and teardown.
func newLineupProbeSequenceRun(t *testing.T, rt Runtime) *lineupProbeRun {
	t.Helper()
	cfg := Config{}.withDefaults()
	owner, err := newOwnershipLabel()
	if err != nil {
		t.Fatal(err)
	}
	return &lineupProbeRun{
		cfg: cfg, ops: newRuntimeOps(rt, cfg), rt: rt, owner: owner,
		name: lineupProbeContainerName(owner.Value[:12]), network: lineupProbeNetworkName(owner.Value[:12]),
		labels: append(runLabels("lineup-probe-"+owner.Value[:12]), owner),
	}
}

func newLineupProbeSequenceRuntime(t *testing.T) *lineupProbeFakeRuntime {
	t.Helper()
	return newLineupProbeFakeRuntime(t, lineupProbeOutputFiles("2.1.282 (Claude Code)\n", `{"type":"result"}`, "", "0\n")...)
}

// TestLineupProbeContainerSequence characterizes the lineup probe container
// from the outside, so moving it onto the helper-role runner shows every
// behavior change as a golden diff. It pins the runtime calls of one
// successful probe and, for each step, the error class, check,
// content-evidence mark, the container claim the failure leaves, whether
// teardown succeeded, and what the runtime holds afterward.
func TestLineupProbeContainerSequence(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	out.WriteString("# Successful Claude writer probe: runtime calls in order.\n")
	rt := newLineupProbeSequenceRuntime(t)
	run := newLineupProbeSequenceRun(t, rt)
	if _, err := run.probe(t.Context(), claudeWriterProbe(t)); err != nil {
		t.Fatalf("probe = %v, want success", err)
	}
	if err := run.teardown(t.Context()); err != nil {
		t.Fatalf("teardown = %v, want success", err)
	}
	normalize := claudeEnrollmentCallNormalizer()
	for _, call := range rt.calls {
		out.WriteString(normalize(normalizeHelperRoleCall(call)) + "\n")
	}
	rows := make([]string, len(lineupProbeSequenceSteps))
	// The group returns once every parallel row has run, so rows is complete
	// before the golden comparison.
	t.Run("failures", func(t *testing.T) {
		for i, step := range lineupProbeSequenceSteps {
			t.Run(step, func(t *testing.T) {
				t.Parallel()
				rows[i] = "probe-container " + step + " " + lineupProbeFailureRow(t, step)
			})
		}
	})
	out.WriteString("\n# Injected failures: step, error class, content evidence, claim, teardown, runtime.\n")
	for _, row := range rows {
		out.WriteString(row + "\n")
	}
	golden.Assert(t, "lineup-probe-sequence", out.Bytes())
}

// lineupProbeFailureRow runs one probe that fails its container at step and
// renders the outcome. Each row is a fresh fixture.
func lineupProbeFailureRow(t *testing.T, step string) string {
	t.Helper()
	rt := newLineupProbeSequenceRuntime(t)
	isProbe := func(id string) bool { return strings.HasPrefix(id, lineupProbeContainerPrefix) }
	injected := errors.New("injected " + step + " failure")
	switch step {
	case "copy":
		rt.onCopyIntoContainer = func(id, _, _ string) error {
			if isProbe(id) {
				return injected
			}
			return nil
		}
	case "delete":
		// Only the probe's own delete fails, so teardown can still reap.
		deletes := 0
		rt.onDeleteContainer = func(id string) (bool, error) {
			if !isProbe(id) {
				return false, nil
			}
			deletes++
			if deletes == 1 {
				return false, injected
			}
			return false, nil
		}
	default:
		injectHelperRoleFailure(t, rt.fakeRuntime, "", step, isProbe)
	}
	run := newLineupProbeSequenceRun(t, rt)
	_, err := run.probe(t.Context(), claudeWriterProbe(t))
	if err == nil {
		t.Errorf("probe container %s: probe succeeded, want an error", step)
		return "succeeded"
	}
	fingerprint := "empty"
	if run.container.fingerprint != "" {
		fingerprint = "set"
	}
	claim := fmt.Sprintf("attempted=%t owned=%t fingerprint=%s",
		run.container.attempted, run.container.owned, fingerprint)
	teardown := "teardown=ok"
	if err := run.teardown(t.Context()); err != nil {
		teardown = "teardown=failed"
	}
	return helperRoleErrorClass(err) + " " + helperRoleEvidence(err) + " " + claim + " " + teardown + " " + rt.leftovers()
}
