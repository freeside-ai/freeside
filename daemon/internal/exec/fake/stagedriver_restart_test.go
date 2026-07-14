package fake_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// reopenStageDriver simulates a daemon restart: it discards the live fake and
// reconstructs one from the same persistence dir. Everything durable (scripts,
// committed intents, committed results) survives; live session progress does
// not, which is the point.
func reopenStageDriver(t *testing.T, dir string) *fake.StageDriver {
	t.Helper()
	d, err := fake.NewStageDriverAt(dir)
	if err != nil {
		t.Fatalf("reopen stage driver at %s: %v", dir, err)
	}
	return d
}

// TestStageDriverRestartBeforeIntentDispatch is acceptance #2 case 1: the
// daemon dies after a scenario is known but before Start commits the intent.
// After restart the id has no committed intent, so Start succeeds (no phantom
// intent) and the invocation runs to completion.
func TestStageDriverRestartBeforeIntentDispatch(t *testing.T) {
	dir := t.TempDir()
	d, err := fake.NewStageDriverAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	d.Script("inv-1", fake.StageScript{
		RunningInspects: 1,
		Outcome:         fake.OutcomeComplete,
		Result:          exec.StageResult{HeadSHA: "cafebabe", Summary: "ran after restart"},
	})

	// Kill before Start: reconstruct, then the intent is still dispatchable.
	d = reopenStageDriver(t, dir)
	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{RunID: "run-1"}); err != nil {
		t.Fatalf("start after restart must succeed (no phantom intent): %v", err)
	}
	inspectUntilTerminalOrGone(t, d, "inv-1")
	result, err := d.Collect(t.Context(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != exec.StatusCompleted || result.InvocationID != "inv-1" {
		t.Errorf("result = %+v; want inv-1, completed", result)
	}
}

// TestStageDriverRestartAfterIntentBeforeResult is acceptance #2 case 2: the
// daemon dies after Start commits the intent but before any result. The
// provider session is lost across the restart, so the invocation reads gone
// with no result, and the committed intent blocks a duplicate Start.
func TestStageDriverRestartAfterIntentBeforeResult(t *testing.T) {
	dir := t.TempDir()
	d, err := fake.NewStageDriverAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	d.Script("inv-1", fake.StageScript{
		RunningInspects: 2,
		Outcome:         fake.OutcomeComplete,
		Result:          exec.StageResult{HeadSHA: "cafebabe", Summary: "never delivered"},
	})
	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	// One non-committing inspect, then the process dies.
	if status, err := d.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusRunning {
		t.Fatalf("pre-restart inspect = %v, %v; want running", status, err)
	}

	d = reopenStageDriver(t, dir)
	if status, err := d.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusGone {
		t.Errorf("inspect after restart = %v, %v; want gone (session lost)", status, err)
	}
	if _, err := d.Collect(t.Context(), "inv-1"); !errors.Is(err, exec.ErrNoResult) {
		t.Errorf("collect after restart = %v, want ErrNoResult", err)
	}
	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{RunID: "run-1"}); !errors.Is(err, exec.ErrDuplicateStart) {
		t.Errorf("re-start of a committed intent = %v, want ErrDuplicateStart", err)
	}
}

// TestStageDriverRestartAfterResultBeforeAcceptance is acceptance #2 case 3,
// the crux: the result committed before the daemon died but was not yet
// accepted. After restart the result re-delivers identically and the acceptor,
// which stands in for the store's durable at-most-once ledger, admits it
// exactly once across the restart: the workflow never advances twice.
func TestStageDriverRestartAfterResultBeforeAcceptance(t *testing.T) {
	dir := t.TempDir()
	d, err := fake.NewStageDriverAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	d.Script("inv-1", fake.StageScript{
		Outcome: fake.OutcomeComplete,
		Result:  exec.StageResult{HeadSHA: "cafebabe", Artifacts: []domain.Digest{"sha256:x"}, Summary: "committed, then died"},
	})
	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	inspectUntilTerminalOrGone(t, d, "inv-1")

	// The result was committed pre-restart and accepted once. The acceptor
	// survives the restart (it models the store's durable ledger).
	acc := newAcceptor()
	before, err := d.Collect(t.Context(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if !acc.accept(t, "inv-1", before) {
		t.Fatal("first delivery must be accepted")
	}

	// Kill after the result, before the workflow recorded acceptance durably.
	d = reopenStageDriver(t, dir)
	if status, err := d.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusGone {
		t.Errorf("inspect after restart = %v, %v; want gone (session lost, result durable)", status, err)
	}
	after, err := d.Collect(t.Context(), "inv-1")
	if err != nil {
		t.Fatalf("committed result must survive restart: %v", err)
	}
	// acc.accept fails the test on any divergence, so a false return proves the
	// recovered result is byte-identical to the pre-restart one and admitted at
	// most once across the restart.
	if acc.accept(t, "inv-1", after) {
		t.Error("post-restart delivery accepted as a new result; the workflow would advance twice")
	}
}

// TestStageDriverRestartTranscriptSurvives proves the durably-recorded
// transcript (§5.3) stays streamable after a restart.
func TestStageDriverRestartTranscriptSurvives(t *testing.T) {
	dir := t.TempDir()
	d, err := fake.NewStageDriverAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	d.Script("inv-1", fake.StageScript{
		Outcome:    fake.OutcomeComplete,
		Result:     exec.StageResult{Summary: "with transcript"},
		Transcript: []byte("agent transcript\n"),
	})
	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{}); err != nil {
		t.Fatal(err)
	}

	d = reopenStageDriver(t, dir)
	r, err := d.Stream(t.Context(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if string(b) != "agent transcript\n" {
		t.Errorf("transcript after restart = %q", b)
	}
}

// TestStageDriverPersistDeterministic proves the on-disk state is clock-free:
// two independent dirs driven through identical operations produce
// byte-identical files, so reconstruction is a pure function of the dir.
func TestStageDriverPersistDeterministic(t *testing.T) {
	drive := func(dir string) []byte {
		t.Helper()
		d, err := fake.NewStageDriverAt(dir)
		if err != nil {
			t.Fatal(err)
		}
		d.Script("inv-1", fake.StageScript{
			RunningInspects: 1,
			Outcome:         fake.OutcomeComplete,
			Result:          exec.StageResult{HeadSHA: "cafebabe", Artifacts: []domain.Digest{"sha256:x"}, Summary: "s"},
		})
		if err := d.Start(t.Context(), "inv-1", exec.StartSpec{RunID: "run-1", StageID: "stage-1"}); err != nil {
			t.Fatal(err)
		}
		inspectUntilTerminalOrGone(t, d, "inv-1")
		// G304: dir is a test-owned t.TempDir(), never external input.
		b, err := os.ReadFile(filepath.Join(dir, "stage_state.json")) //nolint:gosec // test-owned temp dir
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if a, b := drive(t.TempDir()), drive(t.TempDir()); string(a) != string(b) {
		t.Errorf("persisted state differs across identical runs (non-determinism):\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

// TestStageDriverPersistFailurePanics proves a durable-write failure fails loud
// and atomic: a mutator on an unwritable persistence dir panics rather than
// commit its in-memory change while the disk write silently fails, which would
// diverge the fake from the restart state it models.
func TestStageDriverPersistFailurePanics(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("write-permission bits are ignored when running as root")
	}
	dir := t.TempDir()
	d, err := fake.NewStageDriverAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Directory perms (traversable, not writable) to force the write to fail;
	// G302's 0600 ceiling is for files, not a dir a test must still enter.
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // test dir perms, not a file
		t.Fatal(err)
	}
	// Restore write before t.TempDir's own cleanup tries to remove the dir.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // test dir perms, not a file

	defer func() {
		if recover() == nil {
			t.Error("Script on an unwritable persistence dir did not panic")
		}
	}()
	d.Script("inv-1", fake.StageScript{Outcome: fake.OutcomeComplete})
}
