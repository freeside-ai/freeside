package contract

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

const (
	StageCaseStart                = "start_and_duplicate_start"
	StageCaseUnknownInvocation    = "unknown_invocation"
	StageCaseCollectBeforeReady   = "collect_before_ready"
	StageCaseCancel               = "cancel"
	StageCaseIdempotentRedelivery = "idempotent_redelivery"
	StageCaseCrashBeforeResult    = "crash_before_result_after_restart"
	StageCaseCrashAfterResult     = "crash_after_result_after_restart"
	StageCaseStatusVocabulary     = "status_vocabulary_after_restart"
	StageCaseStreamReplay         = "stream_replays_from_beginning"
)

var stageCases = map[string]struct{}{
	StageCaseStart: {}, StageCaseUnknownInvocation: {},
	StageCaseCollectBeforeReady: {}, StageCaseCancel: {},
	StageCaseIdempotentRedelivery: {}, StageCaseCrashBeforeResult: {},
	StageCaseCrashAfterResult: {}, StageCaseStatusVocabulary: {},
	StageCaseStreamReplay: {},
}

// RunStageDriverContract runs the reusable StageDriver contract against one
// implementation factory.
func RunStageDriverContract(t *testing.T, factory StageDriverFactory) {
	t.Helper()
	if factory.New == nil {
		t.Fatal("stage driver contract: nil factory")
	}
	divergences := divergenceMap(t, factory.KnownDivergences, stageCases)

	runCase(t, StageCaseStart, divergences, func(t *testing.T) error {
		h, id, spec := newStageScenario(t, factory, OutcomeComplete)
		driver := h.Driver()
		if err := driver.Start(t.Context(), id, spec); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		if err := driver.Start(t.Context(), id, spec); !errors.Is(err, exec.ErrDuplicateStart) {
			return wrongError("duplicate Start", "ErrDuplicateStart", err)
		}
		h.AwaitReady(t, id)
		driver = h.Restart(t)
		if err := driver.Start(t.Context(), id, spec); !errors.Is(err, exec.ErrDuplicateStart) {
			return wrongError("Start after restart", "ErrDuplicateStart", err)
		}
		return nil
	})

	runCase(t, StageCaseUnknownInvocation, divergences, func(t *testing.T) error {
		h, _, _ := newStageScenario(t, factory, OutcomeComplete)
		driver := h.Driver()
		id := domain.InvocationID("contract-stage-unknown")
		if _, err := driver.Inspect(t.Context(), id); !errors.Is(err, exec.ErrUnknownInvocation) {
			return wrongError("Inspect unknown", "ErrUnknownInvocation", err)
		}
		if _, err := driver.Stream(t.Context(), id); !errors.Is(err, exec.ErrUnknownInvocation) {
			return wrongError("Stream unknown", "ErrUnknownInvocation", err)
		}
		if err := driver.Cancel(t.Context(), id); !errors.Is(err, exec.ErrUnknownInvocation) {
			return wrongError("Cancel unknown", "ErrUnknownInvocation", err)
		}
		if _, err := driver.Collect(t.Context(), id); !errors.Is(err, exec.ErrUnknownInvocation) {
			return wrongError("Collect unknown", "ErrUnknownInvocation", err)
		}
		return nil
	})

	runCase(t, StageCaseCollectBeforeReady, divergences, func(t *testing.T) error {
		h, id, spec := newStageScenario(t, factory, OutcomeComplete)
		if err := h.Driver().Start(t.Context(), id, spec); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		h.AwaitReady(t, id)
		if _, err := h.Driver().Collect(t.Context(), id); !errors.Is(err, exec.ErrResultNotReady) {
			return wrongError("Collect before ready", "ErrResultNotReady", err)
		}
		return nil
	})

	runCase(t, StageCaseCancel, divergences, func(t *testing.T) error {
		h, id, spec := newStageScenario(t, factory, OutcomeComplete)
		driver := h.Driver()
		if err := driver.Start(t.Context(), id, spec); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		h.AwaitReady(t, id)
		if err := driver.Cancel(t.Context(), id); err != nil {
			return fmt.Errorf("cancel: %w", err)
		}
		first, err := driver.Collect(t.Context(), id)
		if err != nil {
			return fmt.Errorf("collect canceled result: %w", err)
		}
		if first.Status != exec.StatusCanceled {
			return fmt.Errorf("canceled result status = %q, want %q", first.Status, exec.StatusCanceled)
		}
		if err := driver.Cancel(t.Context(), id); err != nil {
			return fmt.Errorf("cancel committed result: %w", err)
		}
		second, err := driver.Collect(t.Context(), id)
		if err != nil || !reflect.DeepEqual(first, second) {
			return changedValue("Collect after terminal Cancel", first, second, err)
		}
		return nil
	})

	runCase(t, StageCaseIdempotentRedelivery, divergences, func(t *testing.T) error {
		h, id, spec := newStageScenario(t, factory, OutcomeFail)
		driver := h.Driver()
		if err := driver.Start(t.Context(), id, spec); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		h.AwaitReady(t, id)
		h.Finish(t, id)
		first, err := driver.Collect(t.Context(), id)
		if err != nil {
			return fmt.Errorf("first Collect: %w", err)
		}
		if first.Status != exec.StatusFailed {
			return fmt.Errorf("failed result status = %q, want %q", first.Status, exec.StatusFailed)
		}
		second, err := driver.Collect(t.Context(), id)
		if err != nil || !reflect.DeepEqual(first, second) {
			return changedValue("repeated Collect", first, second, err)
		}
		driver = h.Restart(t)
		third, err := driver.Collect(t.Context(), id)
		if err != nil || !reflect.DeepEqual(first, third) {
			return changedValue("Collect after restart", first, third, err)
		}
		return nil
	})

	runCase(t, StageCaseCrashBeforeResult, divergences, func(t *testing.T) error {
		h, id, spec := newStageScenario(t, factory, OutcomeCrashBeforeResult)
		if err := h.Driver().Start(t.Context(), id, spec); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		h.Finish(t, id)
		driver := h.Restart(t)
		inspection, err := driver.Inspect(t.Context(), id)
		if err != nil || inspection.Status != exec.StatusGone {
			return wrongValue("Inspect after crash-before-result", inspection, err, "StatusGone")
		}
		if _, err := driver.Collect(t.Context(), id); !errors.Is(err, exec.ErrNoResult) {
			return wrongError("Collect after crash-before-result", "ErrNoResult", err)
		}
		return nil
	})

	runCase(t, StageCaseCrashAfterResult, divergences, func(t *testing.T) error {
		h, id, spec := newStageScenario(t, factory, OutcomeCrashAfterResult)
		if err := h.Driver().Start(t.Context(), id, spec); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		h.AwaitReady(t, id)
		h.Finish(t, id)
		driver := h.Restart(t)
		inspection, err := driver.Inspect(t.Context(), id)
		if err != nil || inspection.Status != exec.StatusGone {
			return wrongValue("Inspect after crash-after-result", inspection, err, "StatusGone")
		}
		first, err := driver.Collect(t.Context(), id)
		if err != nil {
			return fmt.Errorf("collect crash-after result: %w", err)
		}
		second, err := driver.Collect(t.Context(), id)
		if err != nil || !reflect.DeepEqual(first, second) {
			return changedValue("crash-after Collect", first, second, err)
		}
		return nil
	})

	runCase(t, StageCaseStatusVocabulary, divergences, func(t *testing.T) error {
		h, id, spec := newStageScenario(t, factory, OutcomeFail)
		driver := h.Driver()
		if err := driver.Start(t.Context(), id, spec); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		h.AwaitReady(t, id)
		pending, err := driver.Inspect(t.Context(), id)
		if err != nil || pending.Status != exec.StatusPending {
			return wrongValue("first Inspect", pending, err, "StatusPending")
		}
		if err := validInspection(pending); err != nil {
			return err
		}
		running, err := driver.Inspect(t.Context(), id)
		if err != nil || running.Status != exec.StatusRunning {
			return wrongValue("second Inspect", running, err, "StatusRunning")
		}
		if err := validInspection(running); err != nil {
			return err
		}
		h.Finish(t, id)
		terminal, err := driver.Inspect(t.Context(), id)
		if err != nil || !terminal.Status.Terminal() {
			return wrongValue("terminal Inspect", terminal, err, "a terminal status")
		}
		driver = h.Restart(t)
		restarted, err := driver.Inspect(t.Context(), id)
		if err != nil {
			return fmt.Errorf("inspect after terminal restart: %w", err)
		}
		return validInspection(restarted)
	})

	runCase(t, StageCaseStreamReplay, divergences, func(t *testing.T) error {
		h := factory.New(t)
		id := domain.InvocationID("contract-stage-stream")
		want := []byte("first line\nsecond line\n")
		spec := h.Prepare(t, id, Scenario{Outcome: OutcomeComplete, Transcript: want})
		driver := h.Driver()
		if err := driver.Start(t.Context(), id, spec); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		h.AwaitReady(t, id)
		first, err := readStageStream(t, driver, id)
		if err != nil {
			return err
		}
		second, err := readStageStream(t, driver, id)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(first, want) {
			return fmt.Errorf("first stream = %q, want %q", first, want)
		}
		if !reflect.DeepEqual(second, want) {
			return fmt.Errorf("second stream = %q, want %q", second, want)
		}
		if !reflect.DeepEqual(first, second) {
			return fmt.Errorf("stream did not replay from beginning: first=%q second=%q", first, second)
		}
		return nil
	})
}

func newStageScenario(
	t *testing.T, factory StageDriverFactory, outcome Outcome,
) (StageDriverHarness, domain.InvocationID, exec.StartSpec) {
	t.Helper()
	if !outcome.valid() {
		t.Fatalf("unknown stage contract outcome %q", outcome)
	}
	h := factory.New(t)
	id := domain.InvocationID("contract-stage-invocation")
	spec := h.Prepare(t, id, Scenario{Outcome: outcome})
	return h, id, spec
}

func validInspection(inspection exec.Inspection) error {
	if !slices.Contains(exec.AllStatuses, inspection.Status) {
		return fmt.Errorf("inspect returned undeclared status %q", inspection.Status)
	}
	if err := inspection.Validate(); err != nil {
		return fmt.Errorf("inspect returned invalid observation %#v: %w", inspection, err)
	}
	return nil
}

func readStageStream(
	t *testing.T, driver exec.StageDriver, id domain.InvocationID,
) ([]byte, error) {
	t.Helper()
	stream, err := driver.Stream(t.Context(), id)
	if err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	body, readErr := io.ReadAll(stream)
	closeErr := stream.Close()
	if readErr != nil || closeErr != nil {
		return nil, fmt.Errorf("read Stream: %w", errors.Join(readErr, closeErr))
	}
	return body, nil
}
