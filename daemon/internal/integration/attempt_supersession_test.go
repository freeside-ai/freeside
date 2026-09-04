package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Contract #1127: admitting a later attempt of a campaign supersedes every
// open execution-failure recovery card an earlier attempt left behind, and
// each retired card records the superseding attempt and its reason. The
// campaign here is driven the real way — a specification run is approved and
// its implementation attempt fails, so the failure card under test is the one
// production code raises, not a hand-seeded shape.

const (
	attemptSweepProject domain.ProjectID = "project-attempt-sweep"
	attemptSweepRunID   domain.RunID     = "run-implementation-attempt-sweep"
)

// reattemptReason is the operator's stated reason; the appended supersession
// sentence must quote it verbatim.
const reattemptReason = "operator reattempt after a transient failure"

// expectedSupersessionSuffix is the sentence the daemon appends to a retired
// card's reason. It names the superseding attempt and the reattempt reason.
func expectedSupersessionSuffix(attemptNumber int, reason string) string {
	return fmt.Sprintf(
		" Superseded by attempt %d of this campaign (reattempt reason: %s); no recovery choice is needed.",
		attemptNumber, reason,
	)
}

// productionFailureCardID mirrors the engine's failure-card identity for a run
// whose implementation attempt ended without an accepted result.
func productionFailureCardID(runID domain.RunID) domain.ItemID {
	return domain.ItemID("execution-failure-" + string(productionInvocationForRun(runID)))
}

// driveApprovedCampaign submits and approves a specification run, so the
// campaign's attempt 1 implementation run exists and is ready to dispatch. It
// returns the campaign and the derived specification run id.
func driveApprovedCampaign(t *testing.T, f *workflowFixture) (domain.CampaignID, domain.RunID) {
	t.Helper()
	ctx := context.Background()
	f.seedDevices(t)
	manifest := capabilityRetryManifest(t)

	specificationRunID, err := engine.SpecificationRunIDForImplementation(attemptSweepRunID)
	if err != nil {
		t.Fatalf("derive specification run: %v", err)
	}
	campaignID, err := engine.ProductionCampaignIDForImplementation(attemptSweepRunID)
	if err != nil {
		t.Fatalf("derive campaign: %v", err)
	}
	source, policyArtifact, resolved := registerSubmissionArtifactsWithPolicyKeys(
		t, f.store, string(specificationRunID), capabilityRetryPolicy(t, manifest, specificationRunID))
	policyBody, err := json.Marshal(resolved.Keys)
	if err != nil {
		t.Fatalf("marshal policy keys: %v", err)
	}
	for _, input := range []struct {
		digest domain.Digest
		body   []byte
	}{
		{source.Digest, submissionSpecification(string(specificationRunID))},
		{policyArtifact.Digest, policyBody},
		{capabilityRetrySpecificationPrompt, capabilityRetrySpecificationPromptBody},
	} {
		if _, err := f.blobs.Put(input.digest, bytes.NewReader(input.body)); err != nil {
			t.Fatalf("put blob %s: %v", input.digest, err)
		}
	}
	specificationInvocation := domain.InvocationID(engine.SpecificationDispatchMarkerKey(specificationRunID))
	if err := specifyfake.Script(f.driver, specificationInvocation, 0, 0, specify.Output{
		Specification: &specify.Specification{
			Summary:    "The implementation plan is ready.",
			Body:       "# Approved Specification\n\nRun the implementation stage.",
			Addressals: []specify.Addressal{},
		},
	}); err != nil {
		t.Fatalf("script specification: %v", err)
	}
	publicationBytes, err := json.Marshal(productionPublicationMetadata())
	if err != nil {
		t.Fatalf("marshal publication: %v", err)
	}
	if _, err := engine.SubmitSpecificationRun(ctx, f.store, engine.SpecificationRunSpec{
		SpecificationRunID: specificationRunID, ImplementationRunID: attemptSweepRunID,
		ProjectID: attemptSweepProject, SourceArtifactID: source.ID,
		PolicyArtifactID: policyArtifact.ID, ResolvedPolicy: resolved,
		Publication:       productionPublicationMetadata(),
		PublicationDigest: domain.Digest(contentaddr.Sum(publicationBytes)),
		PublicationBytes:  publicationBytes,
		CampaignID:        campaignID, AttemptNumber: 1,
	}); err != nil {
		t.Fatalf("submit specification run: %v", err)
	}

	reconcileCapabilityRetryPass(t, f, "specification")
	approval, err := f.signet.GetAttentionItem(ctx,
		domain.ItemID("spec-approval-"+string(attemptSweepRunID)+"-1"))
	if err != nil {
		t.Fatalf("get specification approval: %v", err)
	}
	if _, err := f.signet.Submit(ctx, signet.ClientCommand{
		CommandID: "approve-attempt-sweep", DeviceID: deviceA,
		ExpectedEntityVersion: approval.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: approval.Item.ID, Action: domain.ActionApprove,
			ItemVersion: approval.Item.ItemVersion, ArtifactDigests: approval.Item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatalf("approve specification: %v", err)
	}
	return campaignID, specificationRunID
}

// driveRunToFailureCard scripts the run's implementation invocation to fail
// and reconciles until the engine's own failure producer raises the open
// recovery card, returning its id.
func driveRunToFailureCard(t *testing.T, f *workflowFixture, runID domain.RunID) domain.ItemID {
	t.Helper()
	ctx := context.Background()
	f.driver.Script(productionInvocationForRun(runID), fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.StageResult{Summary: "The stage ended without an accepted result."},
	})
	cardID := productionFailureCardID(runID)
	for pass := 0; pass < 6; pass++ {
		reconcileCapabilityRetryPass(t, f, fmt.Sprintf("drive %s failure pass %d", runID, pass))
		card, err := f.signet.GetAttentionItem(ctx, cardID)
		if err == nil && card.Item.Status == domain.StatusOpen {
			return cardID
		}
	}
	t.Fatalf("failure card %q did not open for run %q within six passes", cardID, runID)
	return ""
}

// seedNilFactsFailureCard writes an open execution-failure card without
// ExecutionFailure facts, bound to runID. It stands in for a
// specification-run recovery card (acceptance case 2) and, under a
// quarantine-style id, for a synthetic notice the sweep must leave alone
// (case 5).
func seedNilFactsFailureCard(t *testing.T, f *workflowFixture, id domain.ItemID, runID domain.RunID, reason string) {
	t.Helper()
	ctx := context.Background()
	createdAt := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	subjectRunID := runID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        id,
		ProjectID: attemptSweepProject,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &subjectRunID,
		},
		Type: domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason:            reason,
		RequestedDecision: []domain.Action{domain.ActionStop},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("build seeded card %q: %v", id, err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("seed card %q: %v", id, err)
	}
}

func readCardRecord(t *testing.T, f *workflowFixture, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	var item domain.AttentionItem
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(ctx, id)
		return err
	}); err != nil {
		t.Fatalf("read card %q: %v", id, err)
	}
	return item
}

func reattempt(t *testing.T, f *workflowFixture, parentRunID domain.RunID) engine.ProductionReattempt {
	t.Helper()
	retry, err := engine.ReattemptProductionRun(context.Background(), f.store, engine.ProductionReattemptSpec{
		ParentRunID: parentRunID, Reason: reattemptReason,
	})
	if err != nil {
		t.Fatalf("reattempt on %q: %v", parentRunID, err)
	}
	return retry
}

// TestReattemptSupersedesEarlierAttemptFailureCards is the core proof:
// admitting attempt 2 retires attempt 1's open failure cards on both its
// implementation and specification runs, names the superseding attempt and
// reason, and leaves quarantine-style notices and cards on other runs alone.
// A replayed admission writes nothing further.
func TestReattemptSupersedesEarlierAttemptFailureCards(t *testing.T) {
	t.Parallel()
	f := openWideCapabilityRetryFixture(t, t.TempDir(), true)
	_, specificationRunID := driveApprovedCampaign(t, f)

	// Attempt 1's implementation run raises a real failure card (facts and
	// all), and its shared specification run carries a seeded recovery card.
	implCardID := driveRunToFailureCard(t, f, attemptSweepRunID)
	specCardID := domain.ItemID("execution-failure-inv-specify-" + string(specificationRunID))
	seedNilFactsFailureCard(t, f, specCardID, specificationRunID,
		"The specification stage ended without an accepted result.")

	// A quarantine-style notice on attempt 1's run and a recovery card on an
	// unrelated run must both stay open (acceptance case 5).
	quarantineID := domain.ItemID("production-marker-quarantined-1-" + string(attemptSweepRunID))
	seedNilFactsFailureCard(t, f, quarantineID, attemptSweepRunID,
		"A stored production marker could not be authenticated.")
	otherRunID := domain.RunID("run-unrelated")
	otherCardID := productionFailureCardID(otherRunID)
	seedNilFactsFailureCard(t, f, otherCardID, otherRunID,
		"An unrelated run's stage ended without an accepted result.")

	implBefore := readCardRecord(t, f, implCardID)
	specBefore := readCardRecord(t, f, specCardID)

	retry := reattempt(t, f, attemptSweepRunID)
	if !retry.Created || retry.Attempt.AttemptNumber != 2 {
		t.Fatalf("reattempt = %+v, want a created attempt 2", retry)
	}

	suffix := expectedSupersessionSuffix(2, reattemptReason)
	for _, tc := range []struct {
		name   string
		id     domain.ItemID
		before domain.AttentionItem
	}{
		{"implementation-run card", implCardID, implBefore},
		{"specification-run card", specCardID, specBefore},
	} {
		got := readCardRecord(t, f, tc.id)
		if got.Status != domain.StatusSuperseded {
			t.Errorf("%s status = %q, want superseded", tc.name, got.Status)
		}
		if got.ItemVersion != tc.before.ItemVersion+1 {
			t.Errorf("%s item_version = %d, want %d", tc.name, got.ItemVersion, tc.before.ItemVersion+1)
		}
		if got.DecidedAt != nil {
			t.Errorf("%s decided_at = %v, want nil (nothing was decided)", tc.name, got.DecidedAt)
		}
		if !strings.HasSuffix(got.Reason, suffix) {
			t.Errorf("%s reason = %q, want it to end with %q", tc.name, got.Reason, suffix)
		}
		if !strings.HasPrefix(got.Reason, tc.before.Reason) {
			t.Errorf("%s reason %q dropped the original text %q", tc.name, got.Reason, tc.before.Reason)
		}
	}

	for _, id := range []domain.ItemID{quarantineID, otherCardID} {
		got := readCardRecord(t, f, id)
		if got.Status != domain.StatusOpen {
			t.Errorf("card %q status = %q, want it left open", id, got.Status)
		}
		if strings.Contains(got.Reason, "Superseded by attempt") {
			t.Errorf("card %q reason %q was rewritten but should be untouched", id, got.Reason)
		}
	}

	// Acceptance case 4: replaying attempt 2's admission finds nothing open
	// and writes nothing more.
	implVersion := readCardRecord(t, f, implCardID).ItemVersion
	specVersion := readCardRecord(t, f, specCardID).ItemVersion
	replayAttemptTwoAdmission(t, f, retry)
	if got := readCardRecord(t, f, implCardID); got.ItemVersion != implVersion {
		t.Errorf("replay bumped implementation card version to %d, want %d", got.ItemVersion, implVersion)
	}
	if got := readCardRecord(t, f, specCardID); got.ItemVersion != specVersion {
		t.Errorf("replay bumped specification card version to %d, want %d", got.ItemVersion, specVersion)
	}
}

// replayAttemptTwoAdmission re-runs attempt 2's production intake with the
// converged spec ReattemptProductionRun used, so the sweep runs a second time
// against an already-superseded card set.
func replayAttemptTwoAdmission(t *testing.T, f *workflowFixture, retry engine.ProductionReattempt) {
	t.Helper()
	manifest := capabilityRetryManifest(t)
	// Attempt 2's resolved policy reuses the campaign's keys, whose provenance
	// digests derive from the shared specification run; the resolved digest is
	// key-only, so rewrapping the same keys under attempt 2's run id matches
	// the registered policy artifact.
	resolved, err := domain.NewResolvedPolicy(retry.Attempt.ImplementationRunID,
		capabilityRetryPolicy(t, manifest, retry.Attempt.SpecificationRunID))
	if err != nil {
		t.Fatalf("build replay resolved policy: %v", err)
	}
	if _, err := engine.SubmitProductionRun(context.Background(), f.store, engine.ProductionRunSpec{
		RunID: retry.Attempt.ImplementationRunID, ProjectID: attemptSweepProject,
		SpecArtifactID: retry.SourceArtifactID, PolicyArtifactID: retry.PolicyArtifactID,
		ResolvedPolicy: resolved, Publication: retry.Publication,
		CampaignID: retry.Attempt.CampaignID, AttemptNumber: retry.Attempt.AttemptNumber,
		AttemptReason: retry.Attempt.Reason, ParentRunID: retry.Attempt.ParentRunID,
	}); err != nil {
		t.Fatalf("replay attempt 2 admission: %v", err)
	}
}

// TestReattemptLeavesConcludedFailureCardUntouched is acceptance case 3: an
// earlier attempt's card an operator already concluded is not reopened,
// rewritten, or reversioned by a later admission.
func TestReattemptLeavesConcludedFailureCardUntouched(t *testing.T) {
	t.Parallel()
	f := openWideCapabilityRetryFixture(t, t.TempDir(), true)
	driveApprovedCampaign(t, f)
	implCardID := driveRunToFailureCard(t, f, attemptSweepRunID)

	// Conclude the card as an operator would, out of band, before the
	// reattempt.
	concluded := concludeCard(t, f, implCardID)

	reattempt(t, f, attemptSweepRunID)

	got := readCardRecord(t, f, implCardID)
	if got.Status != concluded.Status {
		t.Errorf("status = %q, want it left at %q", got.Status, concluded.Status)
	}
	if got.ItemVersion != concluded.ItemVersion {
		t.Errorf("item_version = %d, want it left at %d", got.ItemVersion, concluded.ItemVersion)
	}
	if got.Reason != concluded.Reason {
		t.Errorf("reason = %q, want it left at %q", got.Reason, concluded.Reason)
	}
}

// concludeCard resolves one open card the way a decision would, returning the
// concluded record.
func concludeCard(t *testing.T, f *workflowFixture, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	item := readCardRecord(t, f, id)
	item.Status = domain.StatusResolved
	item.ItemVersion++
	decidedAt := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	item.DecidedAt = &decidedAt
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("conclude card %q: %v", id, err)
	}
	return readCardRecord(t, f, id)
}

// TestThirdAttemptSupersedesSecondAttemptCard is acceptance case 6: attempt 3
// retires attempt 2's open card naming attempt 3, and does not touch attempt
// 1's card, which an earlier admission already superseded.
func TestThirdAttemptSupersedesSecondAttemptCard(t *testing.T) {
	t.Parallel()
	f := openWideCapabilityRetryFixture(t, t.TempDir(), true)
	campaignID, _ := driveApprovedCampaign(t, f)
	firstCardID := driveRunToFailureCard(t, f, attemptSweepRunID)

	// Attempt 2 supersedes attempt 1's card, then fails in turn.
	second := reattempt(t, f, attemptSweepRunID)
	if second.Attempt.AttemptNumber != 2 {
		t.Fatalf("second attempt number = %d, want 2", second.Attempt.AttemptNumber)
	}
	secondRunID, err := engine.ProductionAttemptRunID(campaignID, 2)
	if err != nil {
		t.Fatalf("derive attempt 2 run: %v", err)
	}
	secondCardID := driveRunToFailureCard(t, f, secondRunID)

	firstAfterTwo := readCardRecord(t, f, firstCardID)
	if firstAfterTwo.Status != domain.StatusSuperseded {
		t.Fatalf("attempt 1 card status after attempt 2 = %q, want superseded", firstAfterTwo.Status)
	}

	// Attempt 3's admission supersedes attempt 2's card and leaves attempt 1's
	// already-superseded card exactly where attempt 2 left it.
	third := reattempt(t, f, secondRunID)
	if third.Attempt.AttemptNumber != 3 {
		t.Fatalf("third attempt number = %d, want 3", third.Attempt.AttemptNumber)
	}

	secondCard := readCardRecord(t, f, secondCardID)
	if secondCard.Status != domain.StatusSuperseded {
		t.Errorf("attempt 2 card status = %q, want superseded", secondCard.Status)
	}
	if suffix := expectedSupersessionSuffix(3, reattemptReason); !strings.HasSuffix(secondCard.Reason, suffix) {
		t.Errorf("attempt 2 card reason = %q, want it to end with %q", secondCard.Reason, suffix)
	}

	firstAfterThree := readCardRecord(t, f, firstCardID)
	if firstAfterThree.ItemVersion != firstAfterTwo.ItemVersion {
		t.Errorf("attempt 1 card version = %d, want it unchanged at %d",
			firstAfterThree.ItemVersion, firstAfterTwo.ItemVersion)
	}
	if firstAfterThree.Reason != firstAfterTwo.Reason {
		t.Errorf("attempt 1 card reason changed to %q, want it unchanged", firstAfterThree.Reason)
	}
}
