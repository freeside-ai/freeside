package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestProductionReviewInvocationKeepsItsFirstAdmission: a review invocation
// is admitted once, before its source is asked, and every later pass reuses
// that snapshot. The admitter here answers differently on every call, as a
// lineup that changed would, and the invocation crashes before its launch and
// is picked up by a restarted daemon.
func TestProductionReviewInvocationKeepsItsFirstAdmission(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	id := engine.ProductionReviewInvocationID(p.runID, 1)
	script := fake.ReviewScript{PendingInspects: 2, Outcome: fake.OutcomeComplete, Result: exec.ReviewResult{
		BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
		CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("review admitted once")),
	}}
	calls := 0
	p.reviewAdmission = func(
		_ context.Context, role domain.RoleName, runID domain.RunID, invocation domain.InvocationID, at time.Time,
	) (domain.ReviewAdmission, error) {
		calls++
		admission := fake.ReviewAdmission(role, runID, invocation)
		admission.AuthIdentityID = domain.AuthIdentityID(fmt.Sprintf("review-identity-%d", calls))
		admission.AgentBinding.RouteModelID = fmt.Sprintf("review-model-%d", calls)
		admission.AdmittedAt = at
		return admission, nil
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.RecordAuthIdentity(p.ctx, domain.AuthIdentity{ID: "review-identity-1", Provider: domain.AuthProviderOpenAI}, p.now)
	}); err != nil {
		t.Fatal(err)
	}
	faults := &faultReviewSource{ReviewSource: p.reviewer}
	p.reviewSource = faults
	crash := errors.New("crash before review launch")
	p.workflow = p.newEngine(t, productionCrashSeams{transitionHook: func(transition engine.DurableTransition, side engine.DurableTransitionSide) error {
		if transition == engine.DurableTransitionReviewRequest && side == engine.DurableTransitionBefore {
			return crash
		}
		return nil
	}}, true)
	p.reviewer.Script(id, script)
	p.startAndRecordExport(t)
	admittedAt := p.now
	if _, err := p.reconcileLanes(); !errors.Is(err, crash) {
		t.Fatalf("crash seam: %v", err)
	}
	stored := func() domain.ReviewAdmission {
		t.Helper()
		var admission domain.ReviewAdmission
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			admission, err = tx.GetReviewAdmission(p.ctx, id)
			return err
		}); err != nil {
			t.Fatalf("read the stored admission: %v", err)
		}
		return admission
	}
	// The snapshot exists before the source has been asked anything.
	first := stored()
	if calls != 1 || faults.requestCalls != 0 || first.InvocationID != id || first.RunID != p.runID ||
		first.Role != domain.RoleReviewer || first.AuthIdentityID != "review-identity-1" ||
		first.AgentBinding.RouteModelID != "review-model-1" || !first.AdmittedAt.Equal(admittedAt) {
		t.Fatalf("admission before the launch = %+v after %d admitter calls and %d requests",
			first, calls, faults.requestCalls)
	}

	p.restartDurableState(t)
	p.now = p.now.Add(time.Hour)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.reviewer.Script(id, script)
	for range 6 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("the invocation was admitted %d times", calls)
	}
	if faults.requestCalls != 1 || !faults.requestedAdmission.Equal(first) {
		t.Fatalf("the source was asked %d times with admission %+v, want once with %+v",
			faults.requestCalls, faults.requestedAdmission, first)
	}
	if got := stored(); !got.Equal(first) {
		t.Fatalf("stored admission after the restart = %+v, want %+v", got, first)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		record, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if record.InvocationID != id || record.Outcome != domain.ReviewClean {
			t.Fatalf("review record = %+v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProductionReviewRefusedAdmissionAsksTheSourceNothing: an invocation the
// lineup refuses fails as a configuration failure, stores no snapshot, and is
// never requested.
func TestProductionReviewRefusedAdmissionAsksTheSourceNothing(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	id := engine.ProductionReviewInvocationID(p.runID, 1)
	refusal := fmt.Errorf("the reviewer line no longer resolves: %w", engine.ErrAgentNotAdmissible)
	p.reviewAdmission = func(
		context.Context, domain.RoleName, domain.RunID, domain.InvocationID, time.Time,
	) (domain.ReviewAdmission, error) {
		return domain.ReviewAdmission{}, refusal
	}
	faults := &faultReviewSource{ReviewSource: p.reviewer}
	p.reviewSource = faults
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.BlockedItemsCreated != 0 || result.PublicationTasksCompleted != 0 || faults.requestCalls != 0 {
		t.Fatalf("refused admission result = %#v after %d requests", result, faults.requestCalls)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		failure, err := tx.LatestReviewFailure(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if failure.InvocationID != id || failure.Class != domain.ReviewFailureConfiguration || failure.Round != 1 {
			t.Fatalf("refused admission failure = %#v", failure)
		}
		if _, err := tx.GetReviewAdmission(p.ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a refused invocation stored an admission: %v", err)
		}
		if _, err := tx.GetReviewRequest(p.ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a refused invocation recorded a request: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProductionReviewWaitsForItsUsagePool: a review past its pool's limit
// waits under the pool hold with nothing admitted or requested and no retry
// budget spent, and is requested once a slot frees.
func TestProductionReviewWaitsForItsUsagePool(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	id := engine.ProductionReviewInvocationID(p.runID, 1)
	blocker := engine.ProductionReviewInvocationID(p.runID, 2)
	identity := fake.ReviewIdentities()[0]
	pool := identity.EffectiveUsagePool()
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.RecordUsagePool(p.ctx, domain.UsagePool{
			ID: pool, Provider: identity.Provider, MaxParallelExecutions: 1,
		}, p.now.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewAdmission(p.ctx, fake.ReviewAdmission(domain.RoleReviewer, p.runID, blocker))
	}); err != nil {
		t.Fatal(err)
	}
	faults := &faultReviewSource{ReviewSource: p.reviewer}
	p.reviewSource = faults
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.reviewer.Script(id, fake.ReviewScript{Outcome: fake.OutcomeComplete, Result: exec.ReviewResult{
		BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
		CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("review after the wait")),
	}})
	p.startAndRecordExport(t)
	for range 2 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	requireUsagePoolHold(t, observeProductionRun(t, p.store, p.runID).Hold, pool)
	if faults.requestCalls != 0 {
		t.Fatalf("the source was asked %d times at the limit", faults.requestCalls)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetReviewAdmission(p.ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a waiting review stored an admission: %v", err)
		}
		if _, err := tx.LatestReviewFailure(p.ctx, p.runID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the wait recorded a review failure: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseReviewSlot(p.ctx, blocker, p.now)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if faults.requestIDs[id] != 1 {
		t.Fatalf("requests after the slot freed = %v", faults.requestIDs)
	}
	if hold := observeProductionRun(t, p.store, p.runID).Hold; hold != nil {
		t.Fatalf("the admitted review kept the hold: %#v", hold)
	}
}

// recoveredAbortSource writes the ward's proof when the fake reaps a recovered
// review, as CodexReviewSource marks the outcome ready once its containers are
// gone. fail, when set, is returned once before the abort reaches the fake.
type recoveredAbortSource struct {
	*fake.ReviewSource
	store *store.Store
	fail  error
}

func (s *recoveredAbortSource) AbortRecoveredReview(ctx context.Context, id domain.InvocationID) error {
	if err := s.fail; err != nil {
		s.fail = nil
		return err
	}
	if err := s.ReviewSource.AbortRecoveredReview(ctx, id); err != nil {
		return err
	}
	return s.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordCodexReviewOutcome(ctx, string(id),
			[]byte(fmt.Sprintf(`{"invocation_id":%q,"status":"aborted"}`, id))); err != nil {
			return err
		}
		return tx.MarkCodexReviewOutcomeReady(ctx, string(id))
	})
}

// TestProductionRecoveredReviewContradictionFreesItsSlot: a started review
// that hit a contradiction holds its limit-1 pool slot while it is parked,
// because its containers may still run. Once the operator recovers it, the
// next pass reaps it, which ends the slot, and requests the next round with no
// restart. A failed reap keeps the slot: a transient one retries on a later
// pass, and any other fails loud.
func TestProductionRecoveredReviewContradictionFreesItsSlot(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fail      error
		retryable bool
	}{
		"reaped":          {},
		"transient retry": {fail: &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: errors.New("runtime unavailable")}, retryable: true},
		"contradiction":   {fail: &exec.ReviewSourceFailure{Class: domain.ReviewFailureContradiction, Err: errors.New("foreign container")}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newProductionPublicationHarness(t, "")
			firstID := engine.ProductionReviewInvocationID(p.runID, 1)
			secondID := engine.ProductionReviewInvocationID(p.runID, 2)
			identity := fake.ReviewIdentities()[0]
			pool := identity.EffectiveUsagePool()
			if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
				return tx.RecordUsagePool(p.ctx, domain.UsagePool{
					ID: pool, Provider: identity.Provider, MaxParallelExecutions: 1,
				}, p.now.Add(time.Second))
			}); err != nil {
				t.Fatal(err)
			}
			source := &recoveredAbortSource{ReviewSource: p.reviewer, store: p.store}
			p.reviewSource = source
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			p.reviewer.Script(firstID, fake.ReviewScript{
				Outcome: fake.OutcomeComplete,
				Result: exec.ReviewResult{
					BaseSHA: strings.Repeat("e", 40), HeadSHA: p.replay.HeadSHA,
					CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("contradiction")),
				},
			})
			p.startAndRecordExport(t)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatalf("park contradiction: %v", err)
			}
			// The fake writes no ward journal. A real launch leaves its request
			// and a started intent, and nothing marks its outcome ready while the
			// contradiction is parked, so the slot stays held.
			if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
				intent := func(state string) []byte {
					return []byte(fmt.Sprintf(`{"run_id":%q,"state":%q}`, firstID, state))
				}
				if err := tx.RecordCodexReviewRequest(p.ctx, string(firstID), []byte(`{}`)); err != nil {
					return err
				}
				if err := tx.BeginCodexReviewIntent(p.ctx, string(firstID), "preparing", intent("preparing")); err != nil {
					return err
				}
				return tx.UpdateCodexReviewIntent(p.ctx, string(firstID),
					"preparing", "started", intent("preparing"), intent("started"))
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatalf("parked pass: %v", err)
			}
			if aborts := p.reviewer.RecoveredAborts(firstID); aborts != 0 {
				t.Fatalf("an unrecovered contradiction was reaped %d times", aborts)
			}

			snapshot, err := p.attention.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 1))
			if err != nil {
				t.Fatal(err)
			}
			deviceID := domain.DeviceID("device-recovered-slot")
			if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
				return tx.PutDevice(p.ctx, domain.Device{
					ID: deviceID, DisplayName: "Recovered slot device",
					Status: domain.DeviceActive, PairedAt: p.now,
				})
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
				CommandID: "recover-review-slot", DeviceID: deviceID,
				ExpectedEntityVersion: snapshot.EntityVersion,
				Payload: signet.DecisionPayload{
					ItemID: snapshot.Item.ID, ItemVersion: snapshot.Item.ItemVersion,
					PRHeadSHA:       snapshot.Item.PRHeadSHA,
					ArtifactDigests: snapshot.Item.ArtifactDigests,
					Action:          domain.ActionRecoverReview,
				},
			}); err != nil {
				t.Fatalf("submit recovery: %v", err)
			}
			p.now = p.now.Add(time.Second)
			p.reviewer.Script(secondID, fake.ReviewScript{
				Outcome: fake.OutcomeComplete,
				Result: exec.ReviewResult{
					BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
					CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("review after the reap")),
				},
			})
			requested := func() bool {
				t.Helper()
				var found bool
				if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
					_, err := tx.GetReviewRequest(p.ctx, secondID)
					found = err == nil
					if errors.Is(err, store.ErrNotFound) {
						return nil
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return found
			}

			source.fail = tc.fail
			_, err = p.reconcileLanes()
			if tc.fail != nil {
				if tc.retryable != (err == nil) {
					t.Fatalf("failed reap: retryable=%v, pass error %v", tc.retryable, err)
				}
				if p.reviewer.RecoveredAborts(firstID) != 0 || requested() {
					t.Fatal("a failed reap freed the slot")
				}
				if !tc.retryable {
					return
				}
				if hold := observeProductionRun(t, p.store, p.runID).Hold; hold == nil ||
					hold.Reason != domain.HoldPublicationEnvironment {
					t.Fatalf("hold after a transient reap failure = %#v", hold)
				}
				p.now = p.now.Add(time.Hour)
				_, err = p.reconcileLanes()
			}
			if err != nil {
				t.Fatalf("recovered pass: %v", err)
			}
			if aborts := p.reviewer.RecoveredAborts(firstID); aborts != 1 {
				t.Fatalf("the recovered review was reaped %d times, want once", aborts)
			}
			if !requested() {
				t.Fatalf("round 2 was not requested; hold = %#v", observeProductionRun(t, p.store, p.runID).Hold)
			}
			if hold := observeProductionRun(t, p.store, p.runID).Hold; hold != nil {
				t.Fatalf("the next round kept a hold: %#v", hold)
			}
		})
	}
}
