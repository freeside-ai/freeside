package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
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
