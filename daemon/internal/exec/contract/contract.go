// Package contract holds reusable tests for implementations of the execution
// interfaces. Implementations call the runners from their own test packages;
// production binaries do not import this package.
package contract

import (
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// Outcome names the durable/crash scenario an implementation harness must
// realize. It deliberately mirrors the permanent fake's crash taxonomy.
type Outcome string

const (
	OutcomeComplete          Outcome = "complete"
	OutcomeFail              Outcome = "fail"
	OutcomeCrashBeforeResult Outcome = "crash_before_result"
	OutcomeCrashAfterResult  Outcome = "crash_after_result"
)

// AllOutcomes is the single registration point for contract scenarios.
var AllOutcomes = []Outcome{
	OutcomeComplete,
	OutcomeFail,
	OutcomeCrashBeforeResult,
	OutcomeCrashAfterResult,
}

func (o Outcome) valid() bool {
	switch o {
	case OutcomeComplete, OutcomeFail, OutcomeCrashBeforeResult, OutcomeCrashAfterResult:
		return true
	default:
		return false
	}
}

// Scenario is one implementation-independent lifecycle setup.
type Scenario struct {
	Outcome    Outcome
	Transcript []byte
}

// KnownDivergence admits one already-filed implementation mismatch. The case
// still runs; only the observed failure is skipped, and a now-passing case
// fails until the stale allowance is removed.
type KnownDivergence struct {
	Case  string
	Issue int
	// Failure is the exact cited observed failure. A different failure in the
	// same scenario remains a contract failure.
	Failure string
}

// StageDriverHarness realizes scenarios while the runner owns assertions.
type StageDriverHarness interface {
	Prepare(*testing.T, domain.InvocationID, Scenario) exec.StartSpec
	Driver() exec.StageDriver
	AwaitReady(*testing.T, domain.InvocationID)
	Finish(*testing.T, domain.InvocationID)
	Restart(*testing.T) exec.StageDriver
}

// StageDriverFactory constructs an isolated implementation harness per case.
type StageDriverFactory struct {
	New              func(*testing.T) StageDriverHarness
	KnownDivergences []KnownDivergence
}

// ReviewSourceHarness realizes review scenarios while the runner owns
// assertions.
type ReviewSourceHarness interface {
	Prepare(*testing.T, domain.InvocationID, Scenario) exec.ReviewRequest
	Source() exec.ReviewSource
	AwaitReady(*testing.T, domain.InvocationID)
	Finish(*testing.T, domain.InvocationID)
	Restart(*testing.T) exec.ReviewSource
	AuthorityRejectionComplete(*testing.T, domain.InvocationID) error
}

// ReviewSourceFactory constructs an isolated implementation harness per case.
type ReviewSourceFactory struct {
	New              func(*testing.T) ReviewSourceHarness
	KnownDivergences []KnownDivergence
}

func runCase(
	t *testing.T,
	name string,
	divergences map[string]KnownDivergence,
	check func(*testing.T) error,
) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		err := check(t)
		divergence, allowed := divergences[name]
		if err == nil {
			if allowed {
				t.Fatalf("known divergence #%d is no longer observed; remove its allowance", divergence.Issue)
			}
			return
		}
		if allowed && err.Error() == divergence.Failure {
			t.Skipf("known divergence, issue #%d: %v", divergence.Issue, err)
		}
		fatalContractFailure(t, err)
	})
}

func divergenceMap(
	t *testing.T,
	known []KnownDivergence,
	valid map[string]struct{},
) map[string]KnownDivergence {
	t.Helper()
	got := make(map[string]KnownDivergence, len(known))
	for _, divergence := range known {
		if _, ok := valid[divergence.Case]; !ok {
			t.Fatalf("known divergence names unknown contract case %q", divergence.Case)
		}
		if divergence.Issue <= 0 {
			t.Fatalf("known divergence %q has no filed issue", divergence.Case)
		}
		if divergence.Failure == "" {
			t.Fatalf("known divergence %q has no observed failure", divergence.Case)
		}
		if prior, ok := got[divergence.Case]; ok {
			t.Fatalf("known divergence %q cites both #%d and #%d",
				divergence.Case, prior.Issue, divergence.Issue)
		}
		got[divergence.Case] = divergence
	}
	return got
}

func fatalContractFailure(t *testing.T, err error) {
	t.Helper()
	t.Fatal(err)
}

func wrongError(operation, want string, got error) error {
	if got == nil {
		return fmt.Errorf("%s returned nil, want %s", operation, want)
	}
	return fmt.Errorf("%s returned the wrong error, want %s: %w", operation, want, got)
}

func wrongValue(operation string, got any, err error, want string) error {
	if err != nil {
		return fmt.Errorf("%s failed, want %s: %w", operation, want, err)
	}
	return fmt.Errorf("%s = %#v, want %s", operation, got, want)
}

func changedValue(operation string, first, second any, err error) error {
	if err != nil {
		return fmt.Errorf("%s failed: %w", operation, err)
	}
	return fmt.Errorf("%s changed value: first=%#v second=%#v", operation, first, second)
}
