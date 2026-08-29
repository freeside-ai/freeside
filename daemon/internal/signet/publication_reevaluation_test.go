package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestRerunTrustEvaluationCommitsCommandResolutionAndIntentAtomically(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seedPublicationReevaluationAuthority(t, f, false)
	item := publicationBlockedFixture(t, f.item)
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	command := f.command("cmd-rerun", domain.ActionRerunTrustEvaluation)
	command.Payload.ItemID = item.ID
	command.Payload.ItemVersion = item.ItemVersion
	command.Payload.PRHeadSHA = item.PRHeadSHA
	command.Payload.ArtifactDigests = item.ArtifactDigests

	before := f.revision(t)
	result, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != before+1 {
		t.Fatalf("result revision = %d, want %d", result.Revision, before+1)
	}
	key := signet.PublicationReevaluationKey(*item.Subject.RunID, command.CommandID)
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		storedItem, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if storedItem.Status != domain.StatusResolved || storedItem.DecidedAt == nil ||
			storedItem.ItemVersion != item.ItemVersion+1 {
			t.Fatalf("resolved item = status %q version %d decided %v",
				storedItem.Status, storedItem.ItemVersion, storedItem.DecidedAt)
		}
		storedCommand, err := tx.GetCommand(ctx, command.CommandID)
		if err != nil {
			return err
		}
		entry, err := tx.GetOutbox(ctx, key)
		if err != nil {
			return err
		}
		request, err := signet.DecodePublicationReevaluationRequest(entry.Payload)
		if err != nil {
			return err
		}
		if storedCommand.Action != domain.ActionRerunTrustEvaluation ||
			entry.Kind != signet.PublicationReevaluationRequestedKind ||
			request.RunID != *item.Subject.RunID || request.ItemID != item.ID ||
			request.ItemVersion != item.ItemVersion || request.CommandID != command.CommandID ||
			request.PRHeadSHA != item.PRHeadSHA || request.TrustProfileDigest == "" ||
			request.ReviewRound != 1 {
			t.Fatalf("durable reevaluation = command %#v entry %#v request %#v",
				storedCommand, entry, request)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	replayBefore := f.revision(t)
	replayed, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Revision != result.Revision || f.revision(t) != replayBefore {
		t.Fatalf("replay revision = %d store %d, want %d unchanged",
			replayed.Revision, f.revision(t), result.Revision)
	}
	second := command
	second.CommandID = "cmd-rerun-second"
	if _, err := f.service.Submit(ctx, second); err == nil {
		t.Fatal("second decision succeeded, want ClosedItemError")
	} else {
		var closed *signet.ClosedItemError
		if !errors.As(err, &closed) {
			t.Fatalf("second decision error = %v, want ClosedItemError", err)
		}
	}
}

func TestRerunTrustEvaluationPreservesFakePublicationConclusion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	item := publicationBlockedFixture(t, f.item)
	item.ID = fakepublication.BlockedItemID(*item.Subject.RunID)
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	command := f.command("cmd-rerun-fake", domain.ActionRerunTrustEvaluation)
	command.Payload.ItemID = item.ID
	command.Payload.ItemVersion = item.ItemVersion
	command.Payload.PRHeadSHA = item.PRHeadSHA
	command.Payload.ArtifactDigests = item.ArtifactDigests

	if _, err := f.service.Submit(ctx, command); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if stored.Status != domain.StatusResolved || stored.ItemVersion != item.ItemVersion+1 {
			t.Fatalf("concluded fake item = status %q version %d", stored.Status, stored.ItemVersion)
		}
		if _, err := tx.GetCommand(ctx, command.CommandID); err != nil {
			return err
		}
		if _, err := tx.GetOutbox(ctx,
			signet.PublicationReevaluationKey(*item.Subject.RunID, command.CommandID),
		); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("fake reevaluation intent = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRerunTrustEvaluationRejectsMalformedProductionActionSet(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	item := publicationBlockedFixture(t, f.item)
	item.RequestedDecision = []domain.Action{domain.ActionRerunTrustEvaluation}
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	command := f.command("cmd-rerun-malformed-actions", domain.ActionRerunTrustEvaluation)
	command.Payload.ItemID = item.ID
	command.Payload.ItemVersion = item.ItemVersion
	command.Payload.PRHeadSHA = item.PRHeadSHA
	command.Payload.ArtifactDigests = item.ArtifactDigests

	if _, err := f.service.Submit(ctx, command); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("Submit error = %v, want ErrParentKeyMismatch", err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if stored.Status != domain.StatusOpen || stored.ItemVersion != item.ItemVersion {
			t.Fatalf("rejected production item = status %q version %d", stored.Status, stored.ItemVersion)
		}
		if _, err := tx.GetCommand(ctx, command.CommandID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected production command = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRerunTrustEvaluationRollsBackOnConflictingIntent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seedPublicationReevaluationAuthority(t, f, false)
	item := publicationBlockedFixture(t, f.item)
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	command := f.command("cmd-rerun-conflict", domain.ActionRerunTrustEvaluation)
	command.Payload.ItemID = item.ID
	command.Payload.ItemVersion = item.ItemVersion
	command.Payload.PRHeadSHA = item.PRHeadSHA
	command.Payload.ArtifactDigests = item.ArtifactDigests
	key := signet.PublicationReevaluationKey(*item.Subject.RunID, command.CommandID)
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, key, signet.PublicationReevaluationRequestedKind, []byte(`{"forged":true}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.service.Submit(ctx, command); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("Submit error = %v, want ErrImmutableConflict", err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if stored.Status != domain.StatusOpen || stored.ItemVersion != item.ItemVersion || stored.DecidedAt != nil {
			t.Fatalf("rolled-back item = status %q version %d decided %v",
				stored.Status, stored.ItemVersion, stored.DecidedAt)
		}
		if _, err := tx.GetCommand(ctx, command.CommandID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rolled-back command = %v, want ErrNotFound", err)
		}
		entry, err := tx.GetOutbox(ctx, key)
		if err != nil {
			return err
		}
		if !bytes.Equal(entry.Payload, []byte(`{"forged":true}`)) {
			t.Fatalf("conflicting intent payload = %s", entry.Payload)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRerunTrustEvaluationRejectsAlreadyEvaluatedCurrentProfile(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	profile := seedPublicationReevaluationAuthority(t, f, true)
	item := publicationBlockedFixture(t, f.item)
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	command := f.command("cmd-rerun-same-profile", domain.ActionRerunTrustEvaluation)
	command.Payload.ItemID = item.ID
	command.Payload.ItemVersion = item.ItemVersion
	command.Payload.PRHeadSHA = item.PRHeadSHA
	command.Payload.ArtifactDigests = item.ArtifactDigests
	before := f.revision(t)

	if _, err := f.service.Submit(ctx, command); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("Submit error = %v, want ErrDuplicate", err)
	}
	if got := f.revision(t); got != before {
		t.Fatalf("rejected reevaluation moved revision %d -> %d", before, got)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if stored.Status != domain.StatusOpen || stored.ItemVersion != item.ItemVersion || stored.DecidedAt != nil {
			t.Fatalf("rejected item = status %q version %d decided %v",
				stored.Status, stored.ItemVersion, stored.DecidedAt)
		}
		if _, err := tx.GetCommand(ctx, command.CommandID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected command = %v, want ErrNotFound", err)
		}
		if _, err := tx.GetOutbox(ctx,
			signet.PublicationReevaluationKey(*item.Subject.RunID, command.CommandID),
		); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected intent = %v, want ErrNotFound", err)
		}
		latest, err := tx.LatestTrustProfile(ctx, profile.Repo)
		if err != nil {
			return err
		}
		if latest.ProfileDigest != profile.ProfileDigest {
			t.Fatalf("latest profile = %q, want %q", latest.ProfileDigest, profile.ProfileDigest)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	revised := configRecoveryTestProfile(t, "sha256:publication-reevaluation-review-revised")
	if revised.ProfileDigest == profile.ProfileDigest {
		t.Fatal("revised profile unexpectedly has the original digest")
	}
	revisedAt := f.now.Add(time.Minute)
	*f.now = revisedAt
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, revised, revisedAt)
	}); err != nil {
		t.Fatal(err)
	}
	review, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "publication-reevaluation-review-1", RunID: *item.Subject.RunID, Round: 1,
		Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "test", BaseSHA: "beefcafe", HeadSHA: item.PRHeadSHA,
		CompletedAt:        revisedAt,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: *item.Subject.RunID, ProjectID: item.ProjectID,
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		}); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, review, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Submit(ctx, command); err != nil {
		t.Fatalf("Submit after profile revision: %v", err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if stored.Status != domain.StatusResolved || stored.ItemVersion != item.ItemVersion+1 {
			t.Fatalf("accepted item = status %q version %d", stored.Status, stored.ItemVersion)
		}
		entry, err := tx.GetOutbox(ctx,
			signet.PublicationReevaluationKey(*item.Subject.RunID, command.CommandID),
		)
		if err != nil {
			return err
		}
		request, err := signet.DecodePublicationReevaluationRequest(entry.Payload)
		if err != nil {
			return err
		}
		if request.TrustProfileDigest != revised.ProfileDigest || request.ReviewRound != 2 {
			t.Fatalf("pinned reevaluation = profile %q round %d, want %q round 2",
				request.TrustProfileDigest, request.ReviewRound, revised.ProfileDigest)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func seedPublicationReevaluationAuthority(
	t *testing.T, f fixture, recordAuthorization bool,
) domain.AutomationTrustProfile {
	t.Helper()
	ctx := context.Background()
	profile := configRecoveryTestProfile(t, "sha256:publication-reevaluation-review")
	project, err := domain.NewProject(f.item.ProjectID, profile.Repo, profile.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		if err := tx.RecordTrustProfile(ctx, profile, *f.now); err != nil {
			return err
		}
		if !recordAuthorization {
			return nil
		}
		authorization, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
			Repo: profile.Repo, BaseSHA: "beefcafe", HeadSHA: f.item.PRHeadSHA,
			ImportResultDigest:       "sha256:publication-reevaluation-import",
			VerificationRecipeDigest: "sha256:publication-reevaluation-recipe",
			EvidenceSnapshotDigest:   "sha256:publication-reevaluation-evidence",
			VerificationOutcome:      domain.VerificationFailed,
			TrustProfileDigest:       profile.ProfileDigest,
			InvocationID:             "publication-reevaluation-original",
			CreatedAt:                f.now.Add(-time.Minute),
		})
		if err != nil {
			return err
		}
		return tx.RecordCandidateAuthorization(ctx, authorization)
	}); err != nil {
		t.Fatal(err)
	}
	return profile
}

func publicationBlockedFixture(t *testing.T, base domain.AttentionItem) domain.AttentionItem {
	t.Helper()
	runID := domain.RunID("run-1")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionBlockedItemID(runID), ProjectID: base.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
		Reason: domain.PublicationBlockTrust,
		RequestedDecision: []domain.Action{
			domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
		},
		PRHeadSHA: base.PRHeadSHA, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestPublicationReevaluationIdentityRoundTrips(t *testing.T) {
	runID := domain.RunID("run/with/reevaluation/segments")
	commandID := "command/with/slash"
	key := signet.PublicationReevaluationKey(runID, commandID)
	gotRunID, gotCommandID, ok := signet.PublicationReevaluationCoordinates(key)
	if !ok || gotRunID != runID || gotCommandID != commandID {
		t.Fatalf("key round trip = %q, %q, %v", gotRunID, gotCommandID, ok)
	}
	itemID := signet.ReevaluatedBlockedItemID(runID, commandID)
	gotRunID, gotCommandID, ok = signet.ReevaluatedBlockedItemCoordinates(itemID)
	if !ok || gotRunID != runID || gotCommandID != commandID {
		t.Fatalf("item round trip = %q, %q, %v", gotRunID, gotCommandID, ok)
	}
	for _, malformed := range []struct {
		name string
		key  string
		item domain.ItemID
	}{
		{
			name: "noncanonical trailing bits",
			key:  "production-reevaluation/cnVuLTF/command",
			item: "production-publish-blocked-reevaluation/cnVuLTF/command",
		},
		{
			name: "ignored newline",
			key:  "production-reevaluation/cnVuLTE\n/command",
			item: "production-publish-blocked-reevaluation/cnVuLTE\n/command",
		},
	} {
		t.Run(malformed.name, func(t *testing.T) {
			if _, _, ok := signet.PublicationReevaluationCoordinates(malformed.key); ok {
				t.Fatalf("malformed key %q accepted", malformed.key)
			}
			if _, _, ok := signet.ReevaluatedBlockedItemCoordinates(malformed.item); ok {
				t.Fatalf("malformed item %q accepted", malformed.item)
			}
		})
	}
	payload, err := json.Marshal(signet.PublicationReevaluationRequest{
		RunID: runID, ItemID: itemID, ItemVersion: 1, CommandID: commandID, PRHeadSHA: "cafebabe",
		TrustProfileDigest: "sha256:profile", ReviewRound: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signet.DecodePublicationReevaluationRequest(append(payload, 'x')); err == nil {
		t.Fatal("trailing payload accepted")
	}
	entry := store.QueueEntry{
		IdempotencyKey: key,
		Kind:           signet.PublicationReevaluationRequestedKind,
		Payload:        payload,
	}
	if digests, err := signet.PublicationReevaluationBackupPayloadDigests(entry); err != nil || len(digests) != 0 {
		t.Fatalf("backup payload digests = %v, %v", digests, err)
	}
	entry.IdempotencyKey = signet.PublicationReevaluationKey("foreign-run", commandID)
	if _, err := signet.PublicationReevaluationBackupPayloadDigests(entry); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("retargeted backup payload error = %v, want ErrParentKeyMismatch", err)
	}
	entry.IdempotencyKey = key
	entry.Kind = "foreign-kind"
	if _, err := signet.PublicationReevaluationBackupPayloadDigests(entry); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("foreign-kind backup payload error = %v, want ErrParentKeyMismatch", err)
	}
	completion := signet.PublicationReevaluationCompletion{
		RunID: runID, CommandID: commandID, IntentKey: key,
		Outcome: signet.PublicationReevaluationPublished, PRHeadSHA: "cafebabe",
		EvidenceItemID: itemID, EvidenceItemVersion: 2,
		TerminalInvocationID: signet.PublicationReevaluationTerminalInvocationID(commandID),
	}
	completionPayload, err := signet.EncodePublicationReevaluationCompletion(completion)
	if err != nil {
		t.Fatal(err)
	}
	completionEntry := store.QueueEntry{
		IdempotencyKey: signet.PublicationReevaluationCompletionKey(commandID),
		Kind:           signet.PublicationReevaluationCompletedKind, Payload: completionPayload,
		Status: "dispatched",
	}
	if digests, err := signet.PublicationReevaluationCompletionBackupPayloadDigests(completionEntry); err != nil || len(digests) != 0 {
		t.Fatalf("completion backup payload digests = %v, %v", digests, err)
	}
	completionEntry.Status = "pending"
	if _, err := signet.PublicationReevaluationCompletionBackupPayloadDigests(completionEntry); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("pending completion backup error = %v, want ErrParentKeyMismatch", err)
	}
	retargeted := completion
	retargeted.TerminalInvocationID = "inv-original-production-terminal"
	retargetedPayload, err := json.Marshal(retargeted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signet.DecodePublicationReevaluationCompletion(retargetedPayload); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("retargeted terminal error = %v, want ErrParentKeyMismatch", err)
	}
}
