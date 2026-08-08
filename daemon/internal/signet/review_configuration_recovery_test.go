package signet_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func configRecoveryTestProfile(t *testing.T, configDigest domain.Digest) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       "acme/widgets",
		RepositoryID:               7,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: configDigest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func seedReviewConfigurationRecoveryItem(t *testing.T, f fixture) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	run := domain.Run{
		ID: "run-config-recovery", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	superseded := configRecoveryTestProfile(t, "sha256:config-old")
	superseding := configRecoveryTestProfile(t, "sha256:config-new")
	failure := domain.ReviewFailure{
		RunID: run.ID, InvocationID: "review-config-recovery-2", Round: 2,
		BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureConfiguration,
		Reason: "trust profile no longer approves the reviewer configuration", ObservedAt: *f.now,
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewFailure(ctx, failure)
	}); err != nil {
		t.Fatalf("seed failure: %v", err)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, superseded, f.now.Add(-time.Hour)); err != nil {
			return err
		}
		return tx.RecordTrustProfile(ctx, superseding, *f.now)
	}); err != nil {
		t.Fatalf("seed profiles: %v", err)
	}
	var digest domain.Digest
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		digest, err = tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
		return err
	}); err != nil {
		t.Fatalf("ReviewFailureBodyDigest: %v", err)
	}
	binding := domain.ReviewConfigurationRecoveryBinding{
		RunID: failure.RunID, InvocationID: failure.InvocationID, Round: failure.Round,
		BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA, FailureDigest: digest,
		Repo: superseded.Repo, RepositoryID: superseded.RepositoryID,
		SupersededProfileDigest: superseded.ProfileDigest,
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "review-config-recovery-item", ProjectID: run.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID,
		},
		Type: domain.AttentionReviewConfiguration, Priority: domain.PriorityHigh,
		Reason:            "review parked on an unapproved configuration",
		RequestedDecision: []domain.Action{domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop},
		PRHeadSHA:         failure.HeadSHA, ReviewConfigurationRecovery: &binding,
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
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

func TestSubmitReviewConfigurationRecoveryIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	item := seedReviewConfigurationRecoveryItem(t, f)
	before := f.revision(t)
	command := commandOn(item, "command-adopt-review-configuration", domain.ActionAdoptReviewConfiguration)

	result, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatalf("Submit(adopt_review_configuration): %v", err)
	}
	if after := f.revision(t); after != before+1 {
		t.Fatalf("revision moved %d -> %d, want one accepting transaction", before, after)
	}

	var transition domain.ReviewConfigurationRecoveryTransition
	var found bool
	var decided domain.AttentionItem
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		transition, found, err = tx.LatestReviewConfigurationRecoveryTransition(
			ctx, item.ReviewConfigurationRecovery.RunID)
		if err != nil {
			return err
		}
		decided, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatalf("read accepted recovery: %v", err)
	}
	if !found || transition.Binding() != *item.ReviewConfigurationRecovery {
		t.Fatalf("transition = %+v (found %v), want item binding %+v",
			transition, found, *item.ReviewConfigurationRecovery)
	}
	// The adoption target is resolved at decision time as the repository's
	// currently approved revision, not copied from the item.
	if transition.SupersedingProfileDigest != configRecoveryTestProfile(t, "sha256:config-new").ProfileDigest {
		t.Fatalf("superseding profile = %s, want the latest activated revision",
			transition.SupersedingProfileDigest)
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
