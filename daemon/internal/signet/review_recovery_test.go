package signet_test

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func seedReviewRecoveryItem(t *testing.T, f fixture) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	run := domain.Run{
		ID: "run-recovery", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	failure := domain.ReviewFailure{
		RunID: run.ID, InvocationID: "review-recovery-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureContradiction,
		Reason: "review contradicted its contract", ObservedAt: *f.now,
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewFailure(ctx, failure)
	}); err != nil {
		t.Fatalf("seed failure: %v", err)
	}
	var digest domain.Digest
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		digest, err = tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
		return err
	}); err != nil {
		t.Fatalf("ReviewFailureBodyDigest: %v", err)
	}
	binding := domain.ReviewRecoveryBinding{
		RunID: failure.RunID, InvocationID: failure.InvocationID, Round: failure.Round,
		BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA, FailureDigest: digest,
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "review-recovery-item", ProjectID: run.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID,
		},
		Type: domain.AttentionReviewContradiction, Priority: domain.PriorityHigh,
		Reason:            "review contradicted its contract",
		RequestedDecision: []domain.Action{domain.ActionRecoverReview},
		PRHeadSHA:         failure.HeadSHA, ReviewRecoveryBinding: &binding,
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	return item
}

func TestSubmitReviewRecoveryIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	item := seedReviewRecoveryItem(t, f)
	before := f.revision(t)
	command := commandOn(item, "command-recover-review", domain.ActionRecoverReview)

	result, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatalf("Submit(recover_review): %v", err)
	}
	if after := f.revision(t); after != before+1 {
		t.Fatalf("revision moved %d -> %d, want one accepting transaction", before, after)
	}

	var transition domain.ReviewRecoveryTransition
	var found bool
	var decided domain.AttentionItem
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		transition, found, err = tx.LatestReviewRecoveryTransition(ctx, item.ReviewRecoveryBinding.RunID)
		if err != nil {
			return err
		}
		decided, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatalf("read accepted recovery: %v", err)
	}
	if !found || transition.Binding() != *item.ReviewRecoveryBinding {
		t.Fatalf("transition = %+v (found %v), want item binding %+v",
			transition, found, *item.ReviewRecoveryBinding)
	}
	if transition.CommandID == nil || *transition.CommandID != command.CommandID {
		t.Fatalf("transition command = %v, want %s", transition.CommandID, command.CommandID)
	}
	if decided.Status != domain.StatusResolved || decided.DecidedAt == nil ||
		!decided.DecidedAt.Equal(*f.now) {
		t.Fatalf("decided item = status %q at %v, want resolved at %v",
			decided.Status, decided.DecidedAt, *f.now)
	}

	replay, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatalf("Submit(replay): %v", err)
	}
	if replay.Revision != result.Revision || f.revision(t) != result.Revision {
		t.Fatalf("replay changed result/revision: result %d replay %d current %d",
			result.Revision, replay.Revision, f.revision(t))
	}
}
