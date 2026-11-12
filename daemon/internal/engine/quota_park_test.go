package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const quotaTestWindow = "five_hour"

func quotaRefusalScript(cause domain.FailureCause, refusal *domain.QuotaRefusal) execfake.StageScript {
	return execfake.StageScript{Outcome: execfake.OutcomeFail, Result: exec.StageResult{
		Summary: "provider refused the run", Cause: cause, QuotaRefusal: refusal,
	}}
}

// quotaPool is the fixture identity's pool with one enrolled client, so a
// refusal mark has a collector to own the pool's record.
type quotaPool struct {
	id         domain.UsagePoolID
	enrollment domain.ClientEnrollmentID
	generation int
}

func enrollQuotaPool(t *testing.T, f failedImplementationFixture) quotaPool {
	t.Helper()
	ctx := t.Context()
	// The bubble's clock stands still; step past the fixture's own writes so
	// the identity's new revision orders after them.
	time.Sleep(time.Second)
	at := time.Now().UTC()
	var pool quotaPool
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		identity, err := tx.GetAuthIdentity(ctx, "auth-1")
		if err != nil {
			return err
		}
		identity.AccountBinding = "acct-quota"
		if err := tx.RecordAuthIdentity(ctx, identity, at); err != nil {
			return err
		}
		enrollment := domain.ClientEnrollment{
			ID: "enrollment-quota", AuthIdentityID: identity.ID,
			HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
			AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
			RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
			AccountBinding: identity.AccountBinding,
		}
		if err := tx.RecordClientEnrollment(ctx, enrollment, at); err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, identity.ID, "inv-store-quota",
			&domain.LeaseGenerationBinding{
				EnrollmentID: enrollment.ID, AuthStoreVolume: "codex-quota-store",
				StoreManifestDigest: usageTestManifest,
			}, at, at.Add(10*time.Minute))
		if err != nil {
			return err
		}
		generation, err := tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: enrollment.ID, AuthStoreVolume: "codex-quota-store",
			StoreManifestDigest: usageTestManifest, LeaseFence: lease.Fence,
			AccountBinding: identity.AccountBinding, TokenExpiry: new(at.Add(24 * time.Hour)), RecordedAt: at,
		}, at)
		if err != nil {
			return err
		}
		pool = quotaPool{id: identity.EffectiveUsagePool(), enrollment: enrollment.ID, generation: generation.Ordinal}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return pool
}

// recordReading replaces the pool record's reading of the test window, as a
// successful collection observed at observedAt.
func (p quotaPool) recordReading(
	t *testing.T, f failedImplementationFixture, status domain.WindowStatus, resetsAt *time.Time, observedAt time.Time,
) {
	t.Helper()
	ctx := t.Context()
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		record, err := tx.GetPoolUsageObservation(ctx, p.id)
		if errors.Is(err, store.ErrNotFound) {
			record, err = domain.PoolUsageObservation{Pool: p.id, Collector: p.enrollment}, nil
		}
		if err != nil {
			return err
		}
		record.LastAttempt = &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: observedAt}
		record.Windows = []domain.WindowReading{{
			Window: quotaTestWindow, EnrollmentID: p.enrollment, Generation: p.generation,
			Status: status, ResetsAt: resetsAt, ObservedAt: observedAt,
		}}
		return tx.RecordPoolUsageObservation(ctx, record, observedAt)
	}); err != nil {
		t.Fatal(err)
	}
}

func (p quotaPool) record(t *testing.T, f failedImplementationFixture) (domain.PoolUsageObservation, bool) {
	t.Helper()
	var (
		record domain.PoolUsageObservation
		found  bool
	)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetPoolUsageObservation(t.Context(), p.id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record, found
}

// parkedRun reads the campaign's second attempt run, if one was allocated.
func (f failedImplementationFixture) parkedRun(t *testing.T) (domain.Run, bool) {
	t.Helper()
	runID, err := ProductionAttemptRunID(f.run.CampaignID, 2)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.specificationFixture.run(runID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Run{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return run, true
}

func (f failedImplementationFixture) runExists(t *testing.T, id domain.RunID) bool {
	t.Helper()
	_, err := f.specificationFixture.run(id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func (f failedImplementationFixture) outcome(t *testing.T) domain.ExecutionOutcome {
	t.Helper()
	var outcome domain.ExecutionOutcome
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		outcome, err = tx.GetExecutionOutcomeRecord(t.Context(), f.attempt.InvocationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return outcome
}

func (f failedImplementationFixture) hasFailureItem(t *testing.T) bool {
	t.Helper()
	err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(t.Context(), f.failureItemID())
		return err
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func holdOn(t *testing.T, f failedImplementationFixture, runID domain.RunID) (domain.RunHoldObservation, bool) {
	t.Helper()
	var (
		hold  domain.RunHoldObservation
		found bool
	)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		hold, found, err = tx.GetRunHold(t.Context(), runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return hold, found
}

// dispatchParked runs the parked attempt's dispatch the way the dispatch pass
// does, recording the hold a held start classifies onto.
func dispatchParked(t *testing.T, f failedImplementationFixture, run domain.Run) error {
	t.Helper()
	ctx := t.Context()
	invocationID := productionInvocationID(run.ID)
	var entry store.QueueEntry
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(invocationID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request, err := decodeProductionRequest(entry)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := f.engine.loadProductionBinding(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	stage, ok := findProductionStage(binding.run)
	if !ok {
		t.Fatal("parked run has no production stage")
	}
	_, _, err = f.engine.dispatchIntent(ctx, entry, binding, stage, invocationID)
	if reason, ok := dispatchHoldReason(err); ok {
		if obsErr := f.engine.observeRunHold(ctx, run.ID, invocationID, reason, holdDetailFor(err)); obsErr != nil {
			t.Fatal(obsErr)
		}
	}
	return err
}

// TestQuotaRefusalParksUntilReset: a quota refusal with a reset marks the
// window, raises no card, and allocates the next attempt on the same task
// under a quota_window_exhausted hold. The parked attempt stays held until
// the reset, then starts, and its start clears the hold.
func TestQuotaRefusalParksUntilReset(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		resetsAt := time.Now().UTC().Add(time.Hour)
		f := newFailedImplementationCampaignFixtureScript(t, quotaRefusalScript(domain.FailureCauseQuota,
			&domain.QuotaRefusal{Window: quotaTestWindow, ResetsAt: &resetsAt}))
		pool := enrollQuotaPool(t, f)

		if accepted, err := f.engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil || accepted {
			t.Fatalf("acceptProductionAttempt = %t, %v; want false, nil", accepted, err)
		}
		if f.hasFailureItem(t) {
			t.Fatal("a parked quota refusal raised the execution_failure card")
		}
		if outcome := f.outcome(t); outcome.Cause != domain.FailureCauseQuota {
			t.Fatalf("outcome cause = %q, want quota", outcome.Cause)
		}
		record, _ := pool.record(t, f)
		mark, marked := record.ExhaustedWindowMark(quotaTestWindow, time.Now().UTC())
		if !marked || mark.Source != domain.ExhaustedMarkProviderRefusal || !mark.ResetsAt.Equal(resetsAt) ||
			*mark.InvocationID != f.attempt.InvocationID {
			t.Fatalf("window mark = %+v (marked %t), want the refusal's mark resetting at %s", mark, marked, resetsAt)
		}
		parked, found := f.parkedRun(t)
		if !found || parked.AttemptReason != quotaParkReattemptReason || parked.ParentRunID != f.run.ID ||
			parked.TaskID != f.run.TaskID {
			t.Fatalf("parked run = %+v (found %t), want attempt 2 on task %s", parked, found, f.run.TaskID)
		}
		hold, held := holdOn(t, f, parked.ID)
		if !held || hold.Reason != domain.HoldQuotaWindowExhausted || hold.QuotaWindow == nil ||
			hold.QuotaWindow.Pool != pool.id || hold.QuotaWindow.Window != quotaTestWindow ||
			!hold.QuotaWindow.ResetsAt.Equal(resetsAt) {
			t.Fatalf("parked hold = %+v (held %t), want quota_window_exhausted on %s until %s",
				hold, held, pool.id, resetsAt)
		}

		// A replayed acceptance converges: no second park, no card.
		if accepted, err := f.engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil || accepted {
			t.Fatalf("replayed acceptProductionAttempt = %t, %v", accepted, err)
		}
		if runID, _ := ProductionAttemptRunID(f.run.CampaignID, 3); f.runExists(t, runID) || f.hasFailureItem(t) {
			t.Fatal("a replayed terminal parked again or raised a card")
		}
		if hold, held := holdOn(t, f, parked.ID); !held || hold.Reason != domain.HoldQuotaWindowExhausted {
			t.Fatalf("hold after the replay = %+v (held %t)", hold, held)
		}

		f.driver.Script(productionInvocationID(parked.ID), execfake.StageScript{Outcome: execfake.OutcomeComplete})
		time.Sleep(time.Until(resetsAt) - time.Second)
		if err := dispatchParked(t, f, parked); !errors.Is(err, ErrQuotaWindowExhausted) || !invocationDispatchHold(err) {
			t.Fatalf("dispatch before the reset = %v, want a held ErrQuotaWindowExhausted", err)
		}
		if hold, held := holdOn(t, f, parked.ID); !held || hold.Reason != domain.HoldQuotaWindowExhausted {
			t.Fatalf("hold before the reset = %+v (held %t)", hold, held)
		}
		time.Sleep(time.Second)
		if err := dispatchParked(t, f, parked); err != nil {
			t.Fatalf("dispatch at the reset = %v, want the parked attempt to start", err)
		}
		if hold, held := holdOn(t, f, parked.ID); held {
			t.Fatalf("hold after the start = %+v, want none", hold)
		}
	})
}

// TestQuotaRefusalTakesReadingReset: a refusal that names no reset parks on
// the reset the window's reading gives, and an allowed reading observed after
// the mark lets the parked attempt start before that reset.
func TestQuotaRefusalTakesReadingReset(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFailedImplementationCampaignFixtureScript(t, quotaRefusalScript(domain.FailureCauseQuota,
			&domain.QuotaRefusal{Window: quotaTestWindow}))
		pool := enrollQuotaPool(t, f)
		readingReset := time.Now().UTC().Add(2 * time.Hour)
		pool.recordReading(t, f, domain.WindowStatusAllowed, &readingReset, time.Now().UTC().Add(-time.Minute))

		if accepted, err := f.engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil || accepted {
			t.Fatalf("acceptProductionAttempt = %t, %v; want false, nil", accepted, err)
		}
		parked, found := f.parkedRun(t)
		if !found || f.hasFailureItem(t) {
			t.Fatalf("parked %t, card %t; want a park and no card", found, f.hasFailureItem(t))
		}
		hold, _ := holdOn(t, f, parked.ID)
		if hold.QuotaWindow == nil || !hold.QuotaWindow.ResetsAt.Equal(readingReset) {
			t.Fatalf("parked hold = %+v, want the reading's reset %s", hold, readingReset)
		}

		f.driver.Script(productionInvocationID(parked.ID), execfake.StageScript{Outcome: execfake.OutcomeComplete})
		time.Sleep(time.Minute)
		pool.recordReading(t, f, domain.WindowStatusAllowed, &readingReset, time.Now().UTC())
		time.Sleep(time.Second)
		if err := dispatchParked(t, f, parked); err != nil {
			t.Fatalf("dispatch after a contrary reading = %v, want the parked attempt to start", err)
		}
	})
}

// TestQuotaRefusalWithoutParkRaisesCard: a failure nothing can resume keeps
// today's execution_failure card and parks nothing.
func TestQuotaRefusalWithoutParkRaisesCard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		script func(resetsAt time.Time) execfake.StageScript
		enroll bool
	}{
		{"quota without a reset", func(time.Time) execfake.StageScript {
			return quotaRefusalScript(domain.FailureCauseQuota, &domain.QuotaRefusal{Window: quotaTestWindow})
		}, true},
		{"quota with a reset already reached", func(resetsAt time.Time) execfake.StageScript {
			past := resetsAt.Add(-2 * time.Hour)
			return quotaRefusalScript(domain.FailureCauseQuota, &domain.QuotaRefusal{Window: quotaTestWindow, ResetsAt: &past})
		}, true},
		{"quota on a pool with no enrollment", func(resetsAt time.Time) execfake.StageScript {
			return quotaRefusalScript(domain.FailureCauseQuota, &domain.QuotaRefusal{Window: quotaTestWindow, ResetsAt: &resetsAt})
		}, false},
		{"quota naming no window", func(time.Time) execfake.StageScript {
			return quotaRefusalScript(domain.FailureCauseQuota, nil)
		}, true},
		{"service capacity", func(time.Time) execfake.StageScript {
			return quotaRefusalScript(domain.FailureCauseServiceCapacity, nil)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFailedImplementationCampaignFixtureScript(t, tc.script(time.Now().UTC().Add(time.Hour)))
				pool := quotaPool{id: "identity:auth-1"}
				if tc.enroll {
					pool = enrollQuotaPool(t, f)
				}
				if accepted, err := f.engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil || accepted {
					t.Fatalf("acceptProductionAttempt = %t, %v; want false, nil", accepted, err)
				}
				if !f.hasFailureItem(t) {
					t.Fatal("no execution_failure card")
				}
				if _, parked := f.parkedRun(t); parked {
					t.Fatal("a failure nothing resumes allocated a parked attempt")
				}
				if _, found := pool.record(t, f); found {
					t.Fatal("a failure nothing resumes marked the pool's window")
				}
			})
		})
	}
}

// TestQuotaParkGatePassesWithoutARefusal: an operator reattempt that carries
// the park's reason after a failure with no refusal starts, rather than
// stopping dispatch: the gate holds only on a refused window it can read.
func TestQuotaParkGatePassesWithoutARefusal(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFailedImplementationCampaignFixtureScript(t, quotaRefusalScript(domain.FailureCauseServiceCapacity, nil))
		enrollQuotaPool(t, f)
		if accepted, err := f.engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil || accepted {
			t.Fatalf("acceptProductionAttempt = %t, %v; want false, nil", accepted, err)
		}
		next, err := ReattemptProductionRun(t.Context(), f.store, ProductionReattemptSpec{
			ParentRunID: f.run.ID, Reason: quotaParkReattemptReason,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.driver.Script(productionInvocationID(next.Run.Run.ID), execfake.StageScript{Outcome: execfake.OutcomeComplete})
		if err := dispatchParked(t, f, next.Run.Run); err != nil {
			t.Fatalf("dispatch = %v, want the reattempt to start", err)
		}
	})
}

// TestQuotaParkOnlyFromTheRootInvocation: a quota refusal on any invocation
// but the run's root implementation, such as a remediation round or operator
// feedback, parks nothing and marks nothing, so it keeps the failure card.
func TestQuotaParkOnlyFromTheRootInvocation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFailedImplementationCampaignFixtureScript(t, quotaRefusalScript(domain.FailureCauseQuota, nil))
		pool := enrollQuotaPool(t, f)
		resetsAt := time.Now().UTC().Add(time.Hour)
		terminal := productionTerminalRecord{
			InvocationID: "inv-operator-feedback-1", RunID: f.run.ID, Status: exec.StatusFailed,
			Cause: domain.FailureCauseQuota, QuotaRefusal: &domain.QuotaRefusal{Window: quotaTestWindow, ResetsAt: &resetsAt},
		}
		var hold *domain.QuotaWindowHold
		if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
			var err error
			hold, err = quotaParkHold(t.Context(), tx, nil, f.run, terminal, time.Now().UTC())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if hold != nil {
			t.Fatalf("park from a non-root invocation = %+v, want none", hold)
		}
		if _, found := pool.record(t, f); found {
			t.Fatal("a refusal that parks nothing marked the pool's window")
		}
	})
}

// TestDecodeProductionTerminalRegatesQuotaRefusal: a stored refusal beside
// another cause, or naming no window, fails closed when read back.
func TestDecodeProductionTerminalRegatesQuotaRefusal(t *testing.T) {
	t.Parallel()
	f := newFailedImplementationCampaignFixtureScript(t, quotaRefusalScript(domain.FailureCauseQuota, nil))
	stage, ok := findProductionStage(f.run)
	if !ok {
		t.Fatal("fixture run has no production stage")
	}
	invocationID := productionInvocationID(f.run.ID)
	for _, tc := range []struct {
		name    string
		cause   domain.FailureCause
		refusal domain.QuotaRefusal
		want    error
	}{
		{
			"beside another cause", domain.FailureCauseServiceCapacity,
			domain.QuotaRefusal{Window: quotaTestWindow},
			exec.ErrQuotaRefusalWithoutQuota,
		},
		{"no window", domain.FailureCauseQuota, domain.QuotaRefusal{}, domain.ErrEmptyField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(productionTerminalRecord{
				InvocationID: invocationID, RunID: f.run.ID, StageID: stage.ID,
				Status: exec.StatusFailed, Cause: tc.cause, QuotaRefusal: &tc.refusal,
			})
			if err != nil {
				t.Fatal(err)
			}
			entry := store.QueueEntry{
				IdempotencyKey: string(invocationID), Kind: kindProductionStageTerminal, Payload: payload,
			}
			if _, err := decodeProductionTerminal(entry, f.run); !errors.Is(err, tc.want) {
				t.Fatalf("decodeProductionTerminal = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestUpdateDrainKeepsAQuotaParkedAttemptsHold: a drain doesn't replace the
// hold that names a parked attempt's window, which a parked-task move reads.
// Once the window resets, the drain holds the attempt as update_drain, and
// the lift lets it start.
func TestUpdateDrainKeepsAQuotaParkedAttemptsHold(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		resetsAt := time.Now().UTC().Add(time.Hour)
		f := newFailedImplementationCampaignFixtureScript(t, quotaRefusalScript(domain.FailureCauseQuota,
			&domain.QuotaRefusal{Window: quotaTestWindow, ResetsAt: &resetsAt}))
		enrollQuotaPool(t, f)
		if _, err := f.engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil {
			t.Fatal(err)
		}
		parked, found := f.parkedRun(t)
		if !found {
			t.Fatal("the quota refusal parked no attempt")
		}
		f.driver.Script(productionInvocationID(parked.ID), execfake.StageScript{Outcome: execfake.OutcomeComplete})
		drain := UpdateDrain{Store: f.store, Build: drainOldBuild, Now: func() time.Time { return time.Now().UTC() }}
		if _, err := drain.Request(t.Context()); err != nil {
			t.Fatal(err)
		}

		if err := dispatchParked(t, f, parked); !errors.Is(err, ErrQuotaWindowExhausted) {
			t.Fatalf("dispatch before the reset = %v, want the quota park", err)
		}
		if hold, held := holdOn(t, f, parked.ID); !held || hold.Reason != domain.HoldQuotaWindowExhausted ||
			hold.QuotaWindow == nil {
			t.Fatalf("hold under the drain = %+v (held %t), want the quota window", hold, held)
		}
		time.Sleep(time.Until(resetsAt))
		if err := dispatchParked(t, f, parked); !errors.Is(err, store.ErrUpdateDrainInForce) {
			t.Fatalf("dispatch after the reset = %v, want the drain", err)
		}
		if hold, held := holdOn(t, f, parked.ID); !held || hold.Reason != domain.HoldUpdateDrain {
			t.Fatalf("hold after the reset = %+v (held %t), want update_drain", hold, held)
		}
		if _, err := drain.Cancel(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := dispatchParked(t, f, parked); err != nil {
			t.Fatalf("dispatch after the lift = %v, want the parked attempt to start", err)
		}
	})
}
