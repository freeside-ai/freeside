package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var (
	reentryTestBase     = strings.Repeat("b", 40)
	reentryTestHead     = strings.Repeat("a", 40)
	reentryTestReviewID = domain.InvocationID("review-reentry-round-1")
)

// reentryReadyItem seeds what a published production run leaves behind and the
// re-entry authority's gate reads: the run's production ready item, its pull
// request binding, and a clean review of the bound head.
func reentryReadyItem(t *testing.T, st *store.Store, runID domain.RunID) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	seedCaptureRun(t, st, runID)
	authority := seedReadyBindingAuthority(t, st, runID, reentryTestHead)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionReadyItemID(runID), ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published and verified",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionMarkSeen},
		PRHeadSHA:         reentryTestHead,
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	review, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: reentryTestReviewID, RunID: runID, Round: 1,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, runID, reentryTestReviewID),
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		BaseSHA:             reentryTestBase, HeadSHA: reentryTestHead,
		CompletedAt:        activeResourceTestTime.Add(-2 * time.Minute),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, review, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordReadyItemPRBinding(ctx, domain.ReadyItemPRBinding{
			ItemID: item.ID, RunID: runID,
			ProducingInvocationID: authority.invocationID, PublicationIdentity: authority.identity,
			PublicationInvocationID: authority.publicationInvocationID,
			Repo:                    "owner/repo", RepositoryID: 424242,
			PRNumber: 450, BaseRef: "main", HeadSHA: reentryTestHead,
			RecordedAt: activeResourceTestTime.Add(-time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

// readReentry returns the run's current re-entry authority and whether the
// production lane has a pending task under that authority's key.
func readReentry(t *testing.T, st *store.Store, runID domain.RunID) (*domain.PublicationSuccessor, bool) {
	t.Helper()
	ctx := context.Background()
	var (
		successor *domain.PublicationSuccessor
		queued    bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if successor, err = tx.CurrentPublicationSuccessor(ctx, runID); err != nil {
			return err
		}
		pending, err := tx.ListPendingOutbox(ctx, engine.KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		if successor == nil {
			queued = len(pending) != 0
			return nil
		}
		for _, entry := range pending {
			if entry.IdempotencyKey == successor.TaskKey() {
				queued = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return successor, queued
}
