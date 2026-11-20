package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const failureRetryCommandID = "command-failure-retry"

// TestRetryAfterLostImplementationStartsOneAttempt is the F5/F30 proof
// (#3440): a lost implementation attempt with no content evidence offers
// Retry, accepting it starts exactly one new attempt bound to the command,
// and replayed passes, on the same engine and after a restart, start
// nothing more.
func TestRetryAfterLostImplementationStartsOneAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	f := openWideCapabilityRetryFixture(t, root, true)
	failure := driveImplementationFailure(t, f, capabilityRetryManifest(t), fake.StageScript{
		Outcome: fake.OutcomeCrashBeforeResult,
	})
	card := failure.card
	facts := card.Item.ExecutionFailure
	if facts.Outcome != domain.ExecutionOutcomeLost || facts.RetryOutlook != domain.RetryOutlookMaySucceed ||
		!card.Item.Offers(domain.ActionRetry) {
		t.Fatalf("lost card = %+v offering %v, want a may_succeed loss offering retry",
			facts, card.Item.RequestedDecision)
	}
	if _, err := f.signet.Submit(ctx, signet.ClientCommand{
		CommandID: failureRetryCommandID, DeviceID: deviceA,
		ExpectedEntityVersion: card.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: card.Item.ID, Action: domain.ActionRetry, ItemVersion: card.Item.ItemVersion,
		},
	}); err != nil {
		t.Fatalf("submit retry: %v", err)
	}
	retryInvocation := productionInvocationForRun(failure.retryRunID)
	f.driver.Script(retryInvocation, fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.StageResult{Summary: "The retry ran."},
	})

	started := reconcileCapabilityRetryPass(t, f, "failure retry")
	if started.InvocationsStarted != 1 {
		t.Fatalf("failure retry pass = %+v, want one start", started)
	}
	assertOnePlainRetryAttempt(t, f, failure)
	reconcileCapabilityRetryPass(t, f, "failure retry replay")
	assertOnePlainRetryAttempt(t, f, failure)

	f.close(t)
	restarted := openWideCapabilityRetryFixture(t, root, false)
	reconcileCapabilityRetryPass(t, restarted, "failure retry restart")
	assertOnePlainRetryAttempt(t, restarted, failure)
}

// TestPlainRetryAttemptRefusesForgedBindings: the store binds a plain
// retry attempt to an accepted Retry command on the failure card it
// resolved. A command for another action, or another failed invocation,
// allocates nothing.
func TestPlainRetryAttemptRefusesForgedBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openWideCapabilityRetryFixture(t, t.TempDir(), true)
	failure := driveImplementationFailure(t, f, capabilityRetryManifest(t), fake.StageScript{
		Outcome: fake.OutcomeCrashBeforeResult,
	})
	if _, err := f.signet.Submit(ctx, signet.ClientCommand{
		CommandID: failureRetryCommandID, DeviceID: deviceA,
		ExpectedEntityVersion: failure.card.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: failure.card.Item.ID, Action: domain.ActionRetry, ItemVersion: failure.card.Item.ItemVersion,
		},
	}); err != nil {
		t.Fatalf("submit retry: %v", err)
	}
	for name, spec := range map[string]engine.ProductionReattemptSpec{
		"another action's command": {
			ParentRunID: capabilityRetryRunID, Reason: "operator retry approve-capability-retry",
			OperatorCommandID: "approve-capability-retry", RetryOfInvocationID: failure.invocation,
		},
		"another failed invocation": {
			ParentRunID: capabilityRetryRunID, Reason: "operator retry " + failureRetryCommandID,
			OperatorCommandID: failureRetryCommandID, RetryOfInvocationID: "inv-implement-elsewhere",
		},
	} {
		if _, err := engine.ReattemptProductionRun(ctx, f.store, spec); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("%s: reattempt = %v, want ErrParentKeyMismatch", name, err)
		}
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		latest, err := tx.LatestProductionAttempt(ctx, failure.campaignID)
		if err == nil && latest.AttemptNumber != 1 {
			t.Errorf("a forged binding allocated attempt %d", latest.AttemptNumber)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestWouldRepeatFailureOffersNoRetry: a failure whose cause a repeat would
// meet again says so on the card and offers no Retry.
func TestWouldRepeatFailureOffersNoRetry(t *testing.T) {
	t.Parallel()
	f := openWideCapabilityRetryFixture(t, t.TempDir(), true)
	failure := driveImplementationFailure(t, f, capabilityRetryManifest(t), fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result: exec.StageResult{
			Summary: "The stage configuration is invalid.", Cause: domain.FailureCauseInvalidConfiguration,
		},
	})
	facts := failure.card.Item.ExecutionFailure
	if facts.Cause != domain.FailureCauseInvalidConfiguration || facts.RetryOutlook != domain.RetryOutlookWouldRepeat ||
		failure.card.Item.Offers(domain.ActionRetry) {
		t.Fatalf("card = %+v offering %v, want would_repeat and no retry",
			facts, failure.card.Item.RequestedDecision)
	}
}

// assertOnePlainRetryAttempt: attempt 2 is the latest, binds the Retry
// command and the failed invocation with no capability or agent choice,
// and its run's implement stage holds one attempt.
func assertOnePlainRetryAttempt(t *testing.T, f *workflowFixture, failure implementationFailure) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		attempt, err := tx.LatestProductionAttempt(ctx, failure.campaignID)
		if err != nil {
			return err
		}
		if attempt.AttemptNumber != 2 || attempt.ImplementationRunID != failure.retryRunID ||
			attempt.OperatorCommandID == nil || *attempt.OperatorCommandID != failureRetryCommandID ||
			attempt.RetryOfInvocationID == nil || *attempt.RetryOfInvocationID != failure.invocation ||
			attempt.CapabilityManifestDigest != nil || attempt.AlternateAgentRetryID != nil {
			t.Errorf("latest attempt = %+v, want plain retry attempt 2 bound to the command", attempt)
		}
		run, err := tx.GetRun(ctx, failure.retryRunID)
		if err != nil {
			return err
		}
		if attempts := productionStageAttempts(t, run); attempts != 1 {
			t.Errorf("retry run implement-stage attempts = %d, want one", attempts)
		}
		return nil
	}); err != nil {
		t.Fatalf("read retry attempt: %v", err)
	}
}
