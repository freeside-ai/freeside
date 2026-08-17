package signet_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestBootstrapReconstructsInboxAfterMissedNotifications is §5.14 test 3:
// bootstrap needs no notification cursor or event history; the canonical
// store snapshot alone reconstructs every current inbox item.
func TestBootstrapReconstructsInboxAfterMissedNotifications(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	second := f.item
	second.ID = "item-2"
	second.Reason = "a second decision arrived while the client was offline"
	if err := f.service.PutItem(ctx, second); err != nil {
		t.Fatalf("PutItem: %v", err)
	}

	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if bootstrap.SyncEpoch == "" || bootstrap.Revision != f.revision(t) {
		t.Errorf("bootstrap state = %q/%d, want current non-empty epoch/revision",
			bootstrap.SyncEpoch, bootstrap.Revision)
	}
	if len(bootstrap.AttentionItems) != 2 ||
		bootstrap.AttentionItems[0].Item.ID != "item-1" ||
		bootstrap.AttentionItems[1].Item.ID != "item-2" {
		t.Fatalf("bootstrap items = %+v, want item-1 and item-2 in canonical order", bootstrap.AttentionItems)
	}
	if bootstrap.AttentionDeliveries == nil || bootstrap.Runs == nil || bootstrap.Conversations == nil {
		t.Fatal("empty bootstrap collections must encode as [] rather than null")
	}
	for _, item := range bootstrap.AttentionItems {
		if item.EntityVersion < 1 || item.AsOfRevision < 1 || item.AsOfRevision > bootstrap.Revision {
			t.Errorf("item %q metadata = v%d/r%d outside bootstrap revision %d",
				item.Item.ID, item.EntityVersion, item.AsOfRevision, bootstrap.Revision)
		}
	}
}

// TestBootstrapProjectsOneTransactionalSnapshot covers #66 acceptance 4 at
// the service boundary. The store's permanent concurrent-write test proves
// isolation; this pins that signet reads ServerState and all four collections
// through that one callback and preserves every row's stamped metadata.
func TestBootstrapProjectsOneTransactionalSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seedSyncResources(t, f)

	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(bootstrap.AttentionItems) != 1 || len(bootstrap.AttentionDeliveries) != 1 ||
		len(bootstrap.Runs) != 1 || len(bootstrap.Conversations) != 1 {
		t.Fatalf("bootstrap collection sizes = %d/%d/%d/%d, want 1/1/1/1",
			len(bootstrap.AttentionItems), len(bootstrap.AttentionDeliveries),
			len(bootstrap.Runs), len(bootstrap.Conversations))
	}
	for name, snapshot := range map[string]struct {
		entityVersion int64
		asOfRevision  int64
	}{
		"item":         {bootstrap.AttentionItems[0].EntityVersion, bootstrap.AttentionItems[0].AsOfRevision},
		"delivery":     {bootstrap.AttentionDeliveries[0].EntityVersion, bootstrap.AttentionDeliveries[0].AsOfRevision},
		"run":          {bootstrap.Runs[0].EntityVersion, bootstrap.Runs[0].AsOfRevision},
		"conversation": {bootstrap.Conversations[0].EntityVersion, bootstrap.Conversations[0].AsOfRevision},
	} {
		if snapshot.entityVersion < 1 || snapshot.asOfRevision < 1 || snapshot.asOfRevision > bootstrap.Revision {
			t.Errorf("%s metadata = v%d/r%d outside bootstrap revision %d",
				name, snapshot.entityVersion, snapshot.asOfRevision, bootstrap.Revision)
		}
	}
}

func TestRunSummariesAndTimelineProjectOneStoreRevision(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	campaignID, err := engine.ProductionCampaignIDForImplementation("run-1")
	if err != nil {
		t.Fatal(err)
	}
	elaborationRunID, err := engine.ElaborationRunIDForImplementation("run-1")
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: "run-1", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		CampaignID: campaignID, AttemptNumber: 1,
		Stages: []domain.Stage{{
			ID: "stage-1", RunID: "run-1", Name: "implementation",
			Attempts: []domain.Attempt{{
				ID: "attempt-1", StageID: "stage-1", Number: 1,
				InvocationID: "inv-1",
			}},
		}},
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		elaborationInvocationID := domain.InvocationID("inv-elaborate-" + string(elaborationRunID) + "-1")
		source, err := domain.NewArtifact(domain.ArtifactInput{ID: "artifact-source", Type: domain.ArtifactKindSpecification, Digest: "sha256:source", Provenance: domain.Provenance{ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-elaborate", HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal}}, map[domain.Digest]bool{})
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, source); err != nil {
			return err
		}
		if err := tx.PutProductionAttempt(ctx, domain.ProductionAttempt{
			CampaignID: campaignID, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
			SourceDigest: "sha256:source", PublicationDigest: "sha256:publication",
			ElaborationRunID: elaborationRunID, ImplementationRunID: run.ID,
		}); err != nil {
			return err
		}
		policy, err := domain.NewResolvedPolicy(elaborationRunID, []domain.PolicyKey{{
			Key: "gates.spec_approval", Value: "true",
			Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:policy-source"},
		}})
		if err != nil {
			return err
		}
		request, err := json.Marshal(map[string]any{
			"version": "freeside.elaboration-request/v1", "elaboration_run_id": elaborationRunID,
			"implementation_run_id": run.ID, "project_id": run.ProjectID,
			"invocation_id": elaborationInvocationID, "iteration": 1,
			"campaign_id": campaignID, "attempt_number": 1,
			"publication_digest": "sha256:publication", "input_artifact_ids": []domain.ArtifactID{source.ID},
		})
		if err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, string(elaborationInvocationID),
			string(domain.ElaborationInvocationRequestedKind), request); err != nil {
			return err
		}
		elaborationRun := domain.Run{
			ID: elaborationRunID, ProjectID: run.ProjectID,
			SpecDigest: source.Digest, PolicyDigest: policy.Digest,
			CampaignID: campaignID, AttemptNumber: 1,
			Stages: []domain.Stage{{
				ID: domain.StageID("elaborate-" + string(elaborationRunID)), RunID: elaborationRunID,
				Name: "elaboration", Attempts: []domain.Attempt{{
					ID:      domain.AttemptID("attempt-" + string(elaborationInvocationID)),
					StageID: domain.StageID("elaborate-" + string(elaborationRunID)),
					Number:  1, InvocationID: elaborationInvocationID,
				}},
			}},
		}
		if err := tx.PutRun(ctx, elaborationRun); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, string(elaborationInvocationID)); err != nil {
			return err
		}
		specification, err := domain.NewArtifact(domain.ArtifactInput{
			ID: "spec-run-1-1", Type: domain.ArtifactKindSpecification, Digest: run.SpecDigest,
			Provenance: domain.Provenance{
				ProducerClass:        domain.ProducerAgent,
				ProducerInvocationID: elaborationInvocationID, HeadBinding: domain.HeadIndependent,
				SensitivityClass: domain.SensitivityNormal,
			},
		}, map[domain.Digest]bool{})
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, specification); err != nil {
			return err
		}
		approvalID := domain.ItemID("spec-approval-run-1-1")
		terminal, err := json.Marshal(map[string]any{
			"invocation_id": elaborationInvocationID, "iteration": 1, "status": "completed",
			"research_artifact_ids": []domain.ArtifactID{}, "spec_artifact_id": specification.ID,
			"approval_item_id": approvalID,
		})
		if err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(ctx, string(elaborationInvocationID),
			"elaboration_stage_terminal", terminal); err != nil {
			return err
		}
		createdAt := time.Date(2026, 8, 12, 11, 0, 0, 0, time.UTC)
		approval, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: approvalID, ProjectID: run.ProjectID,
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(elaborationRunID), RunID: &elaborationRunID},
			Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal, Reason: "Approve specification.",
			RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionRequestChanges, domain.ActionStop},
			AgentClaims: []domain.AgentClaim{{
				Label: "Specification", Artifact: specification.ID,
				Digest: specification.Digest, Provenance: specification.Provenance,
			}},
			ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
			Status: domain.StatusOpen, CreatedAt: &createdAt,
		}, map[domain.Digest]bool{})
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, approval); err != nil {
			return err
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "approve-run-1", DeviceID: "device-1", ItemID: approval.ID,
			ItemVersion: approval.ItemVersion, ArtifactDigests: approval.ArtifactDigests,
			Action: domain.ActionApprove,
		})
		if err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		approval.Status = domain.StatusResolved
		approval.ItemVersion++
		approval, err = approval.WithDecidedAt(createdAt)
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, approval); err != nil {
			return err
		}
		if _, err := tx.ApproveProductionAttempt(ctx, campaignID, 1, run.SpecDigest); err != nil {
			return err
		}
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("PutRun: %v", err)
	}
	beforeObservation, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision before observation: %v", err)
	}
	invocationID := domain.InvocationID("inv-1")
	recordedAt := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, _, err := tx.EnqueueOutbox(ctx, string(invocationID), string(domain.ProductionInvocationRequestedKind),
			[]byte(`{"invocation_id":"inv-1","run_id":"run-1","stage_id":"stage-1"}`)); err != nil {
			return err
		}
		if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: run.ID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &invocationID, RecordedAt: recordedAt,
		}); err != nil {
			return err
		}
		return tx.RecordRunHold(ctx, domain.RunHoldObservation{
			RunID: run.ID, InvocationID: &invocationID,
			Reason:          domain.HoldAttendedModeActive,
			FirstObservedAt: recordedAt.Add(time.Minute),
			LastObservedAt:  recordedAt.Add(2 * time.Minute),
		})
	}); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	afterObservation, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision after observation: %v", err)
	}
	if afterObservation.Revision != beforeObservation.Revision+1 {
		t.Fatalf("observation revision %d -> %d, want one client-visible bump",
			beforeObservation.Revision, afterObservation.Revision)
	}

	runs, err := f.service.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	runIndex := -1
	for index := range runs {
		if runs[index].Run.ID == run.ID {
			runIndex = index
			break
		}
	}
	if runIndex < 0 || runs[runIndex].Run.LatestMilestone == nil ||
		*runs[runIndex].Run.LatestMilestone != domain.MilestoneRunSubmitted ||
		runs[runIndex].Run.Outcome != domain.RunOutcomePending ||
		runs[runIndex].Run.HoldReason == nil || *runs[runIndex].Run.HoldReason != domain.HoldAttendedModeActive {
		t.Fatalf("ListRuns summary = %+v", runs)
	}
	if runs[runIndex].Run.CampaignID == nil || *runs[runIndex].Run.CampaignID != run.CampaignID ||
		runs[runIndex].Run.AttemptNumber == nil || *runs[runIndex].Run.AttemptNumber != 1 ||
		runs[runIndex].Run.AttemptReason != nil || runs[runIndex].Run.ParentRunID != nil {
		t.Fatalf("ListRuns attempt lineage = %+v, want %+v", runs[runIndex].Run, run)
	}

	timeline, err := f.service.GetRunTimeline(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRunTimeline: %v", err)
	}
	state, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if timeline.AsOfRevision != state.Revision || timeline.RunID != run.ID ||
		len(timeline.Milestones) != 1 || timeline.Hold == nil ||
		timeline.Invocations == nil {
		t.Fatalf("timeline = %+v at server revision %+v", timeline, state)
	}
	if _, err := f.service.GetRunTimeline(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRunTimeline(missing) error = %v, want ErrNotFound", err)
	}
}

func TestRunSummaryAuthenticatesSubmittedReservationBeforeAnAttemptExists(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := domain.Run{
		ID: "run-submitted", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{ID: "stage-submitted", RunID: "run-submitted", Name: "implementation"}},
	}
	invocation := domain.InvocationID("inv-submitted")
	payload := []byte(`{"invocation_id":"inv-submitted","run_id":"run-submitted","stage_id":"stage-submitted"}`)
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.ProductionInvocationRequestedKind), payload); err != nil {
			return err
		}
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: run.ID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &invocation, RecordedAt: time.Date(2026, 8, 12, 15, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatalf("seed submitted run: %v", err)
	}

	runs, err := f.service.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Run.LatestMilestone == nil ||
		*runs[0].Run.LatestMilestone != domain.MilestoneRunSubmitted {
		t.Fatalf("ListRuns = %+v, want submitted zero-attempt run", runs)
	}
}

// TestUnobservedLegacyRunProjectsWithoutBackfill is the acceptance for #733: a
// pre-migration-0024 run has no observation milestones (0024 backfills none),
// so the projection must report the unobserved outcome and synthesize no
// milestones, distinct from the pending state a submitted run reports.
func TestUnobservedLegacyRunProjectsWithoutBackfill(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	run := domain.Run{
		ID: "run-legacy", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{ID: "stage-legacy", RunID: "run-legacy", Name: "implementation"}},
	}
	// Persist the run with no milestone: the reconstructed observation history
	// is empty, exactly as a run created before 0024 reads after upgrade.
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("PutRun: %v", err)
	}

	runs, err := f.service.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Run.Outcome != domain.RunOutcomeUnobserved ||
		runs[0].Run.LatestMilestone != nil {
		t.Fatalf("ListRuns summary = %+v, want unobserved with no milestone", runs)
	}

	timeline, err := f.service.GetRunTimeline(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRunTimeline: %v", err)
	}
	if len(timeline.Milestones) != 0 {
		t.Fatalf("timeline milestones = %+v, want no synthesized milestones", timeline.Milestones)
	}
}

// TestRestoreForcesFreshBootstrap is §5.14 test 8's server half, driven by the
// real checkpoint/restore path (#165) rather than a bare epoch hook: a restore
// atomically rolls the database back to the checkpoint and rotates the sync
// epoch. The rollback regresses the item version and the revision below what a
// client cached from the advanced world; the epoch change is what invalidates
// that client's cursor even though its cached revision is now the higher one.
// The eviction itself is client-side (SyncCoordinator/DecisionModel, #162);
// this pins that the server exposes a fresh epoch over regressed state through
// the real restore, not through store.NewEpoch.
func TestRestoreForcesFreshBootstrap(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// Checkpoint the seeded state (item-1 at version 1), then advance past it.
	checkpoint := filepath.Join(t.TempDir(), "checkpoint.db")
	if err := f.store.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	atCheckpoint, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision at checkpoint: %v", err)
	}

	advanced := f.item
	advanced.ItemVersion = 2
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, advanced)
	}); err != nil {
		t.Fatalf("advance item: %v", err)
	}
	// The state a client caches from the advanced world.
	cached, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision after advance: %v", err)
	}
	if cached.Revision <= atCheckpoint.Revision {
		t.Fatalf("advance did not move revision %d -> %d", atCheckpoint.Revision, cached.Revision)
	}

	// Restore: data rolls back and the epoch rotates in one operation.
	if _, err := f.store.Restore(ctx, checkpoint); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	after, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision after restore: %v", err)
	}
	// The epoch the cached client holds is now stale: it must discard and
	// bootstrap regardless of its (higher) cached revision.
	if after.SyncEpoch == cached.SyncEpoch {
		t.Fatalf("epoch stayed %q across restore; a cached cursor would never evict", cached.SyncEpoch)
	}
	// Revision legitimately regressed to the checkpoint under the new epoch;
	// revisions compare only within an epoch, so the lower value is unambiguous.
	if after.Revision != atCheckpoint.Revision {
		t.Fatalf("restore revision = %d, want checkpoint revision %d", after.Revision, atCheckpoint.Revision)
	}
	if after.Revision >= cached.Revision {
		t.Fatalf("restore revision %d did not regress below the cached %d", after.Revision, cached.Revision)
	}

	// The canonical bootstrap carries the new epoch and the regressed item.
	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if bootstrap.SyncEpoch != after.SyncEpoch || bootstrap.Revision != after.Revision {
		t.Errorf("bootstrap state = %q/%d, heartbeat = %q/%d",
			bootstrap.SyncEpoch, bootstrap.Revision, after.SyncEpoch, after.Revision)
	}
	if len(bootstrap.AttentionItems) != 1 ||
		bootstrap.AttentionItems[0].Item.ItemVersion != 1 ||
		bootstrap.AttentionItems[0].EntityVersion != 1 {
		t.Fatalf("bootstrap item = %+v, want item-1 regressed to version 1 / entity_version 1", bootstrap.AttentionItems)
	}
}

func seedSyncResources(t *testing.T, f fixture) {
	t.Helper()
	ctx := context.Background()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	run := domain.Run{
		ID: "run-1", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{ID: "stage-1", RunID: "run-1", Name: "implementation"}},
	}
	conversation := domain.Conversation{ID: "conv-1", Status: domain.ConversationIdle}
	delivery := domain.AttentionDelivery{
		ItemID: f.item.ID, DeviceID: "device-1", Channel: "ntfy", Attempt: 1,
		SubmittedAt: ts, Status: domain.DeliverySubmitted,
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutConversation(ctx, conversation); err != nil {
			return err
		}
		return tx.PutAttentionDelivery(ctx, delivery)
	}); err != nil {
		t.Fatalf("seed sync resources: %v", err)
	}
}
