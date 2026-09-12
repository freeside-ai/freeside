package signet_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/export"
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
	summaryText := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown,
		Content:   "The implementation is ready; the agent still reports one open assumption.",
	}
	second.AgentClaims = append(second.AgentClaims, domain.AgentClaim{
		Label: export.SummaryEvidenceLabel, Artifact: "summary-artifact",
		Digest: summaryText.ComputeDigest(), Text: &summaryText,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-2",
			HeadBinding: domain.HeadBound, SourceHeadSHA: "cafebabe",
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: int64(len(summaryText.Content)),
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Source:    domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	})
	second.ArtifactDigests = append(second.ArtifactDigests, summaryText.ComputeDigest())
	slices.Sort(second.ArtifactDigests)
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
	syncedSummary := bootstrap.AttentionItems[1].Item.AgentClaims[len(second.AgentClaims)-1]
	if syncedSummary.Label != export.SummaryEvidenceLabel || syncedSummary.Text == nil ||
		syncedSummary.Text.Content != summaryText.Content ||
		syncedSummary.Provenance.ProducerInvocationID != "inv-2" {
		t.Fatalf("synced summary = %+v, want bound reserved summary claim", syncedSummary)
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

// seedSpecificationCampaign seeds one production campaign through its resolved
// spec-approval item: the specification run, its unapproved initial production
// attempt, the spec artifact, and the resolved approval command. It leaves the
// attempt unapproved and the implementation run unpersisted, returning the
// campaign id, the specification run id, and the implementation run value the
// caller persists after approving the attempt. Every seeded identifier derives
// from implRunID so several campaigns can share one store. Both the
// summary/timeline projection test and the specification hand-off tests
// (#1183) build on this one scenario.
func seedSpecificationCampaign(
	t *testing.T, ctx context.Context, f fixture, implRunID domain.RunID,
) (domain.CampaignID, domain.RunID, domain.Run) {
	t.Helper()
	campaignID, err := engine.ProductionCampaignIDForImplementation(implRunID)
	if err != nil {
		t.Fatal(err)
	}
	specificationRunID, err := engine.SpecificationRunIDForImplementation(implRunID)
	if err != nil {
		t.Fatal(err)
	}
	stageID := domain.StageID("stage-" + string(implRunID))
	run := domain.Run{
		ID: implRunID, ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		CampaignID: campaignID, AttemptNumber: 1,
		Stages: []domain.Stage{{
			ID: stageID, RunID: implRunID, Name: "implementation",
			Attempts: []domain.Attempt{{
				ID: domain.AttemptID("attempt-" + string(implRunID)), StageID: stageID, Number: 1,
				InvocationID: domain.InvocationID("inv-" + string(implRunID)),
			}},
		}},
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		specificationInvocationID := domain.InvocationID("inv-specify-" + string(specificationRunID) + "-1")
		source, err := domain.NewArtifact(domain.ArtifactInput{ID: domain.ArtifactID("artifact-source-" + string(implRunID)), Type: domain.ArtifactKindSpecification, Digest: "sha256:source", Provenance: domain.Provenance{ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-specify", HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal}, Metadata: domain.EvidenceMetadata{MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: 1, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable}}, map[domain.Digest]bool{})
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, source); err != nil {
			return err
		}
		if err := tx.PutProductionAttempt(ctx, domain.ProductionAttempt{
			CampaignID: campaignID, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
			SourceDigest: "sha256:source", PublicationDigest: "sha256:publication",
			SpecificationRunID: specificationRunID, ImplementationRunID: run.ID,
		}); err != nil {
			return err
		}
		policy, err := domain.NewResolvedPolicy(specificationRunID, []domain.PolicyKey{{
			Key: "gates.spec_approval", Value: "true",
			Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:policy-source"},
		}})
		if err != nil {
			return err
		}
		request, err := json.Marshal(map[string]any{
			"version": "freeside.specification-request/v1", "specification_run_id": specificationRunID,
			"implementation_run_id": run.ID, "project_id": run.ProjectID,
			"invocation_id": specificationInvocationID, "iteration": 1,
			"campaign_id": campaignID, "attempt_number": 1,
			"publication_digest": "sha256:publication", "input_artifact_ids": []domain.ArtifactID{source.ID},
		})
		if err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, string(specificationInvocationID),
			string(domain.SpecificationInvocationRequestedKind), request); err != nil {
			return err
		}
		specificationRun := domain.Run{
			ID: specificationRunID, ProjectID: run.ProjectID,
			SpecDigest: source.Digest, PolicyDigest: policy.Digest,
			CampaignID: campaignID, AttemptNumber: 1,
			Stages: []domain.Stage{{
				ID: domain.StageID("specify-" + string(specificationRunID)), RunID: specificationRunID,
				Name: "specification", Attempts: []domain.Attempt{{
					ID:      domain.AttemptID("attempt-" + string(specificationInvocationID)),
					StageID: domain.StageID("specify-" + string(specificationRunID)),
					Number:  1, InvocationID: specificationInvocationID,
				}},
			}},
		}
		if err := tx.PutRun(ctx, specificationRun); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, string(specificationInvocationID)); err != nil {
			return err
		}
		// The specification run's own run_submitted milestone; without it the run
		// concludes unobserved, which is already finished, and would mask the
		// active-to-finished transition the hand-off rule drives.
		if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: specificationRunID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &specificationInvocationID,
			RecordedAt:   time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
		}); err != nil {
			return err
		}
		specification, err := domain.NewArtifact(domain.ArtifactInput{
			ID: domain.ArtifactID("spec-" + string(implRunID) + "-1"), Type: domain.ArtifactKindSpecification, Digest: run.SpecDigest,
			Provenance: domain.Provenance{
				ProducerClass:        domain.ProducerAgent,
				ProducerInvocationID: specificationInvocationID, HeadBinding: domain.HeadIndependent,
				SensitivityClass: domain.SensitivityNormal,
			},
			Metadata: domain.EvidenceMetadata{
				MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: 1,
				CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				Source:    domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
			},
		}, map[domain.Digest]bool{})
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, specification); err != nil {
			return err
		}
		approvalID := domain.ItemID("spec-approval-" + string(implRunID) + "-1")
		terminal, err := json.Marshal(struct {
			InvocationID        domain.InvocationID `json:"invocation_id"`
			Iteration           int                 `json:"iteration"`
			Status              string              `json:"status"`
			ResearchArtifactIDs []domain.ArtifactID `json:"research_artifact_ids"`
			SpecArtifactID      *domain.ArtifactID  `json:"spec_artifact_id,omitempty"`
			ApprovalItemID      *domain.ItemID      `json:"approval_item_id,omitempty"`
		}{
			InvocationID: specificationInvocationID, Iteration: 1, Status: "completed",
			ResearchArtifactIDs: []domain.ArtifactID{}, SpecArtifactID: &specification.ID,
			ApprovalItemID: &approvalID,
		})
		if err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(ctx, string(specificationInvocationID),
			"specification_stage_terminal", terminal); err != nil {
			return err
		}
		createdAt := time.Date(2026, 8, 12, 11, 0, 0, 0, time.UTC)
		approval, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: approvalID, ProjectID: run.ProjectID,
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(specificationRunID), RunID: &specificationRunID},
			Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal, Reason: "Approve specification.",
			RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionRequestChanges, domain.ActionDiscuss, domain.ActionStop},
			AgentClaims: []domain.AgentClaim{{
				Label: "Specification", Artifact: specification.ID,
				Digest: specification.Digest, Provenance: specification.Provenance,
				Metadata: domain.EvidenceMetadata{
					MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: 1,
					CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
					Source:    domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
				},
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
			CommandID: "approve-" + string(implRunID), DeviceID: "device-1", ItemID: approval.ID,
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
		return tx.PutAttentionItem(ctx, approval)
	}); err != nil {
		t.Fatalf("seed specification campaign %q: %v", implRunID, err)
	}
	return campaignID, specificationRunID, run
}

// approveAndPersistImplementationRun approves run's initial production attempt
// with its spec digest and persists the implementation run, the two writes the
// engine commits when a specification is approved and its implementation
// submitted.
func approveAndPersistImplementationRun(t *testing.T, ctx context.Context, f fixture, campaignID domain.CampaignID, run domain.Run) {
	t.Helper()
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.ApproveProductionAttempt(ctx, campaignID, 1, run.SpecDigest); err != nil {
			return err
		}
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("approve and persist implementation run %q: %v", run.ID, err)
	}
}

func TestRunSummariesAndTimelineProjectOneStoreRevision(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	campaignID, _, run := seedSpecificationCampaign(t, ctx, f, "run-1")
	approveAndPersistImplementationRun(t, ctx, f, campaignID, run)
	beforeObservation, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision before observation: %v", err)
	}
	invocationID := run.Stages[0].Attempts[0].InvocationID
	recordedAt := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	productionRequest, err := json.Marshal(map[string]any{
		"invocation_id": invocationID, "run_id": run.ID, "stage_id": run.Stages[0].ID,
	})
	if err != nil {
		t.Fatalf("marshal production request: %v", err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, _, err := tx.EnqueueOutbox(ctx, string(invocationID), string(domain.ProductionInvocationRequestedKind),
			productionRequest); err != nil {
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

// seedProductionSubmission records an implementation run's production-invocation
// outbox and its run_submitted milestone, so the run projects the pending
// (active) outcome a bound specification run has left behind.
func seedProductionSubmission(t *testing.T, ctx context.Context, f fixture, run domain.Run, recordedAt time.Time) {
	t.Helper()
	invocationID := run.Stages[0].Attempts[0].InvocationID
	request, err := json.Marshal(map[string]any{
		"invocation_id": invocationID, "run_id": run.ID, "stage_id": run.Stages[0].ID,
	})
	if err != nil {
		t.Fatalf("marshal production request: %v", err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, _, err := tx.EnqueueOutbox(ctx, string(invocationID),
			string(domain.ProductionInvocationRequestedKind), request); err != nil {
			return err
		}
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: run.ID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &invocationID, RecordedAt: recordedAt,
		})
	}); err != nil {
		t.Fatalf("seed production submission %q: %v", run.ID, err)
	}
}

// TestSpecificationRunFinishesWhenImplementationBound is the #1183 acceptance:
// a specification run leaves the active list once its production attempt is
// approved and its implementation run bound, superseded by that implementation
// run with its own outcome still pending. Before approval it is active and
// unsuperseded, and the implementation run is never superseded by the
// specification run.
func TestSpecificationRunFinishesWhenImplementationBound(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	campaignID, specificationRunID, run := seedSpecificationCampaign(t, ctx, f, "run-1")

	before, err := f.service.GetRun(ctx, specificationRunID)
	if err != nil {
		t.Fatalf("GetRun(specification) before approval: %v", err)
	}
	if before.Run.Lifecycle != domain.RunLifecycleActive || before.Run.SupersededBy != nil ||
		before.Run.Outcome != domain.RunOutcomePending {
		t.Fatalf("specification run before approval = lifecycle %s outcome %s superseded_by %v, want active/pending/nil",
			before.Run.Lifecycle, before.Run.Outcome, before.Run.SupersededBy)
	}

	approveAndPersistImplementationRun(t, ctx, f, campaignID, run)

	assertFinishedBoundToImplementation := func(source string, lifecycle domain.RunLifecycle, outcome domain.RunOutcome, superseded *domain.RunID) {
		t.Helper()
		if lifecycle != domain.RunLifecycleFinished || outcome != domain.RunOutcomePending ||
			superseded == nil || *superseded != run.ID {
			t.Errorf("%s: specification run = lifecycle %s outcome %s superseded_by %v, want finished/pending superseded by %s",
				source, lifecycle, outcome, superseded, run.ID)
		}
	}

	got, err := f.service.GetRun(ctx, specificationRunID)
	if err != nil {
		t.Fatalf("GetRun(specification) after approval: %v", err)
	}
	assertFinishedBoundToImplementation("GetRun", got.Run.Lifecycle, got.Run.Outcome, got.Run.SupersededBy)

	runs, err := f.service.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var sawSpecification, sawImplementation bool
	for _, snapshot := range runs {
		switch snapshot.Run.ID {
		case specificationRunID:
			sawSpecification = true
			assertFinishedBoundToImplementation("ListRuns", snapshot.Run.Lifecycle, snapshot.Run.Outcome, snapshot.Run.SupersededBy)
		case run.ID:
			sawImplementation = true
			if snapshot.Run.SupersededBy != nil {
				t.Errorf("implementation run superseded_by = %v, want nil", snapshot.Run.SupersededBy)
			}
		}
	}
	if !sawSpecification || !sawImplementation {
		t.Fatalf("ListRuns missing runs: specification %v implementation %v", sawSpecification, sawImplementation)
	}

	// GetRunTimeline runs the same projection read; it must not fail closed on
	// the new attempt read even though its wire shape carries no lifecycle.
	timeline, err := f.service.GetRunTimeline(ctx, specificationRunID)
	if err != nil {
		t.Fatalf("GetRunTimeline(specification): %v", err)
	}
	if timeline.RunID != specificationRunID {
		t.Fatalf("GetRunTimeline run id = %q, want %q", timeline.RunID, specificationRunID)
	}
}

// TestBoundSpecificationRunsLeaveActiveList is the #1183 list-level acceptance:
// over several campaigns, each with an approved attempt and an active
// implementation run, no run left on the active list is a specification run.
func TestBoundSpecificationRunsLeaveActiveList(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	implRunIDs := []domain.RunID{"run-1", "run-2", "run-3"}
	specificationRuns := map[domain.RunID]bool{}
	for index, implRunID := range implRunIDs {
		campaignID, specificationRunID, run := seedSpecificationCampaign(t, ctx, f, implRunID)
		specificationRuns[specificationRunID] = true
		approveAndPersistImplementationRun(t, ctx, f, campaignID, run)
		seedProductionSubmission(t, ctx, f, run, time.Date(2026, 8, 12, 12, index, 0, 0, time.UTC))
	}

	runs, err := f.service.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	activeImplementationRuns := 0
	for _, snapshot := range runs {
		if specificationRuns[snapshot.Run.ID] {
			if snapshot.Run.Lifecycle != domain.RunLifecycleFinished || snapshot.Run.SupersededBy == nil {
				t.Errorf("specification run %s = lifecycle %s superseded_by %v, want finished and superseded",
					snapshot.Run.ID, snapshot.Run.Lifecycle, snapshot.Run.SupersededBy)
			}
			continue
		}
		if snapshot.Run.Lifecycle == domain.RunLifecycleActive {
			activeImplementationRuns++
		}
	}
	if activeImplementationRuns != len(implRunIDs) {
		t.Fatalf("active implementation runs = %d, want %d", activeImplementationRuns, len(implRunIDs))
	}
}

func TestRunSummaryAuthenticatesSubmittedReservationBeforeAnAttemptExists(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
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
	f := newRunFixture(t)
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
