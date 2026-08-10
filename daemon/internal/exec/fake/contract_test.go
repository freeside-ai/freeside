package fake_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/contract"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

type fakeStageContractHarness struct {
	dir    string
	driver *fake.StageDriver
}

func newFakeStageContractHarness(t *testing.T) contract.StageDriverHarness {
	t.Helper()
	dir := t.TempDir()
	driver, err := fake.NewStageDriverAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeStageContractHarness{dir: dir, driver: driver}
}

func (h *fakeStageContractHarness) Prepare(
	t *testing.T, id domain.InvocationID, scenario contract.Scenario,
) exec.StartSpec {
	t.Helper()
	h.driver.Script(id, fake.StageScript{
		PendingInspects: 1,
		RunningInspects: 1,
		Outcome:         fakeOutcome(t, scenario.Outcome),
		Result: exec.StageResult{
			HeadSHA: "contract-head", Summary: "contract result",
		},
		Transcript: scenario.Transcript,
	})
	return exec.StartSpec{}
}

func (h *fakeStageContractHarness) Driver() exec.StageDriver { return h.driver }

func (*fakeStageContractHarness) AwaitReady(*testing.T, domain.InvocationID) {}

func (h *fakeStageContractHarness) Finish(t *testing.T, id domain.InvocationID) {
	t.Helper()
	for range 8 {
		inspection, err := h.driver.Inspect(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if inspection.Status.Terminal() || inspection.Status == exec.StatusGone {
			return
		}
	}
	t.Fatal("fake stage scenario did not finish")
}

func (h *fakeStageContractHarness) Restart(t *testing.T) exec.StageDriver {
	t.Helper()
	driver, err := fake.NewStageDriverAt(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	h.driver = driver
	return driver
}

type fakeReviewContractHarness struct {
	dir      string
	source   *fake.ReviewSource
	scenario contract.Scenario
}

func newFakeReviewContractHarness(t *testing.T) contract.ReviewSourceHarness {
	t.Helper()
	dir := t.TempDir()
	source, err := fake.NewReviewSourceAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeReviewContractHarness{dir: dir, source: source}
}

func (h *fakeReviewContractHarness) Prepare(
	t *testing.T, id domain.InvocationID, scenario contract.Scenario,
) exec.ReviewRequest {
	t.Helper()
	h.scenario = scenario
	request := reviewRequest("contract-head")
	h.source.Script(id, fake.ReviewScript{
		PendingInspects: 1,
		PendingPolls:    1,
		Outcome:         fakeOutcome(t, scenario.Outcome),
		Result:          exec.ReviewResult{HeadSHA: request.HeadSHA},
	})
	return request
}

func (h *fakeReviewContractHarness) Source() exec.ReviewSource { return h.source }

func (*fakeReviewContractHarness) AwaitReady(*testing.T, domain.InvocationID) {}

func (h *fakeReviewContractHarness) Finish(t *testing.T, id domain.InvocationID) {
	t.Helper()
	for range 8 {
		status, err := h.source.Inspect(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if status.Terminal() || status == exec.StatusGone {
			if h.scenario.Outcome == contract.OutcomeComplete {
				for range 8 {
					if _, err := h.source.Poll(t.Context(), id); err == nil {
						return
					} else if !errors.Is(err, exec.ErrResultNotReady) {
						t.Fatal(err)
					}
				}
				t.Fatal("fake review result did not become pollable")
			}
			return
		}
	}
	t.Fatal("fake review scenario did not finish")
}

func (h *fakeReviewContractHarness) Restart(t *testing.T) exec.ReviewSource {
	t.Helper()
	source, err := fake.NewReviewSourceAt(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	h.source = source
	return source
}

func (h *fakeReviewContractHarness) AuthorityRejectionComplete(
	t *testing.T, id domain.InvocationID,
) error {
	t.Helper()
	_, err := h.source.Inspect(t.Context(), id)
	if !errors.Is(err, exec.ErrUnknownInvocation) {
		if err == nil {
			return errors.New("inspect after authority rejection returned nil, want ErrUnknownInvocation")
		}
		return fmt.Errorf("inspect after authority rejection: %w", err)
	}
	return nil
}

func fakeOutcome(t *testing.T, outcome contract.Outcome) fake.Outcome {
	t.Helper()
	switch outcome {
	case contract.OutcomeComplete:
		return fake.OutcomeComplete
	case contract.OutcomeFail:
		return fake.OutcomeFail
	case contract.OutcomeCrashBeforeResult:
		return fake.OutcomeCrashBeforeResult
	case contract.OutcomeCrashAfterResult:
		return fake.OutcomeCrashAfterResult
	}
	t.Fatalf("unknown contract outcome %q", outcome)
	return ""
}

func TestStageDriverContract(t *testing.T) {
	contract.RunStageDriverContract(t, contract.StageDriverFactory{
		New: newFakeStageContractHarness,
	})
}

func TestReviewSourceContract(t *testing.T) {
	contract.RunReviewSourceContract(t, contract.ReviewSourceFactory{
		New: newFakeReviewContractHarness,
	})
}

var (
	_ contract.StageDriverHarness  = (*fakeStageContractHarness)(nil)
	_ contract.ReviewSourceHarness = (*fakeReviewContractHarness)(nil)
)
