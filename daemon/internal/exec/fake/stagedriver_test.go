package fake_test

import (
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// inspectUntilTerminalOrGone drives Inspect and returns the observed status
// sequence, stopping after a terminal or gone status (or limit calls, a
// runaway guard for broken scripts).
func inspectUntilTerminalOrGone(t *testing.T, d *fake.StageDriver, id domain.InvocationID) []exec.Status {
	t.Helper()
	var seen []exec.Status
	for range 32 {
		status, err := d.Inspect(t.Context(), id)
		if err != nil {
			t.Fatalf("inspect %s: %v", id, err)
		}
		seen = append(seen, status)
		if status.Terminal() || status == exec.StatusGone {
			return seen
		}
	}
	t.Fatalf("inspect %s: no terminal or gone status after 32 calls (broken script?)", id)
	return nil
}

// TestStageDriverNormalCompletion is scenario 3a: start, observe progress,
// collect the committed result, replay the transcript.
func TestStageDriverNormalCompletion(t *testing.T) {
	d := fake.NewStageDriver()
	d.Script("inv-1", fake.StageScript{
		RunningInspects: 1,
		Outcome:         fake.OutcomeComplete,
		Result: exec.StageResult{
			HeadSHA:   "cafebabe",
			Artifacts: []domain.Digest{"sha256:transcript"},
			Summary:   "did the thing",
		},
		Transcript: []byte("agent transcript\n"),
	})

	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{RunID: "run-1", StageID: "stage-1"}); err != nil {
		t.Fatal(err)
	}
	seen := inspectUntilTerminalOrGone(t, d, "inv-1")
	want := []exec.Status{exec.StatusRunning, exec.StatusCompleted}
	if !slices.Equal(seen, want) {
		t.Errorf("status sequence = %v, want %v", seen, want)
	}

	result, err := d.Collect(t.Context(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.InvocationID != "inv-1" {
		t.Errorf("result invocation_id = %q, want stamped %q", result.InvocationID, "inv-1")
	}
	if result.Status != exec.StatusCompleted {
		t.Errorf("result status = %q, want %q", result.Status, exec.StatusCompleted)
	}
	if err := result.Validate(); err != nil {
		t.Errorf("committed result must validate: %v", err)
	}

	// The transcript stream is replayable: two reads, same bytes.
	for range 2 {
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
			t.Errorf("transcript = %q", b)
		}
	}
}

// TestStageDriverCrashBeforeResult is scenario 3b: the session is lost
// before any result is committed; there is nothing to recover.
func TestStageDriverCrashBeforeResult(t *testing.T) {
	d := fake.NewStageDriver()
	d.Script("inv-1", fake.StageScript{
		PendingInspects: 1,
		Outcome:         fake.OutcomeCrashBeforeResult,
	})

	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{}); err != nil {
		t.Fatal(err)
	}
	seen := inspectUntilTerminalOrGone(t, d, "inv-1")
	want := []exec.Status{exec.StatusPending, exec.StatusGone}
	if !slices.Equal(seen, want) {
		t.Errorf("status sequence = %v, want %v", seen, want)
	}

	// Gone stays gone, and there is no result to collect, now or later.
	for range 2 {
		status, err := d.Inspect(t.Context(), "inv-1")
		if err != nil || status != exec.StatusGone {
			t.Errorf("inspect after crash = %v, %v; want gone", status, err)
		}
		if _, err := d.Collect(t.Context(), "inv-1"); !errors.Is(err, exec.ErrNoResult) {
			t.Errorf("collect after crash-before-result = %v, want ErrNoResult", err)
		}
	}
}

// TestStageDriverCrashAfterResultRecoverable is scenario 3c: the session is
// lost after the result committed; the result stays recoverable by
// invocation id (§5.3 reconciliation).
func TestStageDriverCrashAfterResultRecoverable(t *testing.T) {
	d := fake.NewStageDriver()
	d.Script("inv-1", fake.StageScript{
		RunningInspects: 1,
		Outcome:         fake.OutcomeCrashAfterResult,
		Result:          exec.StageResult{HeadSHA: "cafebabe", Summary: "finished, then the VM died"},
	})

	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{}); err != nil {
		t.Fatal(err)
	}
	seen := inspectUntilTerminalOrGone(t, d, "inv-1")
	want := []exec.Status{exec.StatusRunning, exec.StatusGone}
	if !slices.Equal(seen, want) {
		t.Errorf("status sequence = %v, want %v", seen, want)
	}

	first, err := d.Collect(t.Context(), "inv-1")
	if err != nil {
		t.Fatalf("result must be recoverable by invocation id: %v", err)
	}
	if first.InvocationID != "inv-1" || first.Status != exec.StatusCompleted {
		t.Errorf("recovered result = %+v; want inv-1, completed", first)
	}
	// Recovery is stable: the acceptor sees the second delivery as an
	// identical replay, not a new result.
	acc := newAcceptor()
	acc.accept(t, "inv-1", first)
	second, err := d.Collect(t.Context(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.accept(t, "inv-1", second) {
		t.Error("second collect was accepted as a new result; want identical replay")
	}
}

// TestStageDriverDelayedCompletion is scenario 3d: delay is scripted
// call-steps, and collecting before the result commits says not-ready.
func TestStageDriverDelayedCompletion(t *testing.T) {
	d := fake.NewStageDriver()
	d.Script("inv-1", fake.StageScript{
		PendingInspects: 2,
		RunningInspects: 2,
		Outcome:         fake.OutcomeComplete,
		Result:          exec.StageResult{Summary: "slow but fine"},
	})

	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Collect(t.Context(), "inv-1"); !errors.Is(err, exec.ErrResultNotReady) {
		t.Fatalf("collect before completion = %v, want ErrResultNotReady", err)
	}
	seen := inspectUntilTerminalOrGone(t, d, "inv-1")
	want := []exec.Status{
		exec.StatusPending, exec.StatusPending,
		exec.StatusRunning, exec.StatusRunning,
		exec.StatusCompleted,
	}
	if !slices.Equal(seen, want) {
		t.Errorf("status sequence = %v, want %v", seen, want)
	}
	if _, err := d.Collect(t.Context(), "inv-1"); err != nil {
		t.Fatal(err)
	}
}

// TestStageDriverDuplicateDeliveryAcceptsOnce is scenario 3e: Collect
// re-delivers the identical committed result; acceptance keyed by
// invocation id admits exactly one (§5.3).
func TestStageDriverDuplicateDeliveryAcceptsOnce(t *testing.T) {
	d := fake.NewStageDriver()
	d.Script("inv-1", fake.StageScript{
		Outcome: fake.OutcomeComplete,
		Result:  exec.StageResult{HeadSHA: "cafebabe", Summary: "once"},
	})

	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{}); err != nil {
		t.Fatal(err)
	}
	inspectUntilTerminalOrGone(t, d, "inv-1")

	acc := newAcceptor()
	accepted := 0
	for range 3 {
		result, err := d.Collect(t.Context(), "inv-1")
		if err != nil {
			t.Fatal(err)
		}
		if acc.accept(t, "inv-1", result) {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("accepted %d results across 3 deliveries, want exactly 1", accepted)
	}
}

// TestStageDriverCancel is scenario 3f: cancel commits a canceled result, so
// cancellation reconciles like any other outcome; a later cancel is a no-op.
func TestStageDriverCancel(t *testing.T) {
	d := fake.NewStageDriver()
	d.Script("inv-1", fake.StageScript{
		PendingInspects: 1,
		RunningInspects: 8,
		Outcome:         fake.OutcomeComplete,
		Result:          exec.StageResult{Summary: "would have finished"},
	})

	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{}); err != nil {
		t.Fatal(err)
	}
	if status, err := d.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusPending {
		t.Fatalf("first inspect = %v, %v; want pending", status, err)
	}
	if err := d.Cancel(t.Context(), "inv-1"); err != nil {
		t.Fatal(err)
	}
	if status, err := d.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusCanceled {
		t.Errorf("inspect after cancel = %v, %v; want canceled", status, err)
	}
	result, err := d.Collect(t.Context(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != exec.StatusCanceled || result.InvocationID != "inv-1" {
		t.Errorf("canceled result = %+v; want inv-1, canceled", result)
	}
	if err := result.Validate(); err != nil {
		t.Errorf("canceled result must validate: %v", err)
	}
	// Cancel after the committed result is a no-op; the result stands.
	if err := d.Cancel(t.Context(), "inv-1"); err != nil {
		t.Fatal(err)
	}
	again, err := d.Collect(t.Context(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != exec.StatusCanceled {
		t.Errorf("result after second cancel = %+v; want unchanged canceled", again)
	}
}

// TestStageDriverGuards covers the contract's identity guards: one committed
// intent per id, loud unscripted starts, unknown ids.
func TestStageDriverGuards(t *testing.T) {
	d := fake.NewStageDriver()
	d.Script("inv-1", fake.StageScript{Outcome: fake.OutcomeComplete})

	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(t.Context(), "inv-1", exec.StartSpec{}); !errors.Is(err, exec.ErrDuplicateStart) {
		t.Errorf("second start = %v, want ErrDuplicateStart", err)
	}
	if err := d.Start(t.Context(), "inv-unscripted", exec.StartSpec{}); !errors.Is(err, fake.ErrUnscripted) {
		t.Errorf("unscripted start = %v, want ErrUnscripted", err)
	}
	if _, err := d.Inspect(t.Context(), "inv-unknown"); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Errorf("unknown inspect = %v, want ErrUnknownInvocation", err)
	}
	if _, err := d.Collect(t.Context(), "inv-unknown"); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Errorf("unknown collect = %v, want ErrUnknownInvocation", err)
	}
	if err := d.Cancel(t.Context(), "inv-unknown"); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Errorf("unknown cancel = %v, want ErrUnknownInvocation", err)
	}
	if _, err := d.Stream(t.Context(), "inv-unknown"); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Errorf("unknown stream = %v, want ErrUnknownInvocation", err)
	}
}
