package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// handoffConflictInputForTest is the part of a remediation input a handoff
// conflict's first round adds or empties.
type handoffConflictInputForTest struct {
	BaseSHA              string           `json:"base_sha"`
	HeadSHA              string           `json:"head_sha"`
	CandidatePatchBase64 []byte           `json:"candidate_patch_base64"`
	Adjudication         json.RawMessage  `json:"adjudication"`
	Findings             []domain.Finding `json:"findings"`
	HandoffConflict      *json.RawMessage `json:"handoff_conflict"`
}

type handoffConflictAccountForTest struct {
	TipSHA                 string                      `json:"tip_sha"`
	Cause                  domain.HandoffConflictCause `json:"cause"`
	ConflictedPaths        []string                    `json:"conflicted_paths"`
	ReportDigest           domain.Digest               `json:"report_digest"`
	VerificationSteps      []json.RawMessage           `json:"verification_steps"`
	TranscriptDigest       domain.Digest               `json:"transcript_digest"`
	VerificationTranscript string                      `json:"verification_transcript"`
}

// handoffConflictRemediation is what a conflict at handoff started: the
// authority it sealed, and the remediation's admission and input.
type handoffConflictRemediation struct {
	authority domain.PublicationSuccessor
	check     domain.HandoffCheck
	id        domain.InvocationID
	admission domain.ExecutionAdmission
	input     handoffConflictInputForTest
	account   handoffConflictAccountForTest
}

// startHandoffConflictRemediation reconciles until a conflict at the current
// publication's handoff sealed its authority and the remediation of round
// started, restarting the engine between the seal and the dispatch.
func (p *productionPublicationHarness) startHandoffConflictRemediation(
	t *testing.T, round int,
) handoffConflictRemediation {
	t.Helper()
	return p.startHandoffConflictRemediationWith(t, round, fake.StageScript{
		PendingInspects: 1, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Summary: "Handoff conflict remediated."},
	})
}

// startHandoffConflictRemediationWith is startHandoffConflictRemediation with
// the remediation's driver script.
func (p *productionPublicationHarness) startHandoffConflictRemediationWith(
	t *testing.T, round int, script fake.StageScript,
) handoffConflictRemediation {
	t.Helper()
	r := handoffConflictRemediation{id: domain.InvocationID(fmt.Sprintf("inv-remediate-%d-%s", round, p.runID))}
	sealed := func() bool {
		found := false
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			current, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
			if err != nil {
				return err
			}
			if current != nil && current.RemediatesHandoffConflict() && current.ReviewRound == round+1 {
				r.authority, found = *current, true
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return found
	}
	// The conflicted publication is current until the authority seals; its
	// handoff check is the one that sealed the tip.
	conflicted := p.handoffCheckPublication(t)
	// The publication lane seals the authority, and the dispatch lane runs
	// only before it does, so the remediation starts after the restart.
	for pass := 0; ; pass++ {
		if pass == 8 {
			t.Fatalf("no handoff conflict authority for round %d within %d passes", round+1, pass)
		}
		p.now = p.now.Add(time.Minute)
		if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
			t.Fatal(err)
		}
		if sealed() {
			break
		}
		if _, err := p.workflow.Reconcile(p.ctx); err != nil {
			t.Fatal(err)
		}
	}
	// The seal, the task's end, and the remediation's intent are one write:
	// a restart here dispatches the remediation and audits nothing again.
	var check domain.HandoffCheck
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		check, err = tx.GetHandoffCheck(p.ctx, p.runID, conflicted)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r.check = check
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.driver.Script(r.id, script)
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("handoff remediation dispatch = %#v, %v", result, err)
	}
	var artifact domain.Artifact
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		if r.admission, err = tx.GetExecutionAdmissionRecord(p.ctx, r.id); err != nil {
			return err
		}
		if artifact, err = tx.GetArtifact(
			p.ctx, domain.ArtifactID(fmt.Sprintf("remediation-input-%d-%s", round, p.runID))); err != nil {
			return err
		}
		sealedCheck, err := tx.GetHandoffCheck(p.ctx, p.runID, check.PublicationID)
		if err == nil && sealedCheck != check {
			t.Errorf("handoff check after the restart = %#v, want %#v", sealedCheck, check)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r.admission.Base.BaseSHA != r.authority.Reentry.BaseSHA {
		t.Fatalf("remediation admitted at %s, want the sealed tip %s",
			r.admission.Base.BaseSHA, r.authority.Reentry.BaseSHA)
	}
	body, err := p.blobs.Open(artifact.Digest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &r.input); err != nil {
		t.Fatal(err)
	}
	if r.input.HandoffConflict == nil {
		t.Fatalf("remediation input carries no handoff_conflict: %s", encoded)
	}
	if err := json.Unmarshal(*r.input.HandoffConflict, &r.account); err != nil {
		t.Fatal(err)
	}
	if r.input.BaseSHA != r.authority.Reentry.BaseSHA || r.input.HeadSHA != r.authority.Reentry.HeadSHA ||
		len(r.input.Findings) != 0 || r.input.Adjudication != nil || len(r.input.CandidatePatchBase64) == 0 ||
		r.account.TipSHA != r.authority.Reentry.BaseSHA || r.account.Cause != r.authority.HandoffConflictCause {
		t.Fatalf("handoff remediation input = %s", encoded)
	}
	return r
}

// finishHandoffConflictRemediation records the remediation's export, its
// candidate built on the sealed tip with content, and scripts the clean review
// of the cycle's first round there.
func (p *productionPublicationHarness) finishHandoffConflictRemediation(
	t *testing.T, r handoffConflictRemediation, content string,
) {
	t.Helper()
	tip := r.authority.Reentry.BaseSHA
	workspace := t.TempDir()
	runGit(t, workspace, "clone", "-q", "--no-hardlinks", p.baseDir, ".")
	runGit(t, workspace, "checkout", "-q", "--detach", tip)
	if err := os.RemoveAll(filepath.Join(workspace, ".git")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, workspace, "README.md", content)
	var run domain.Run
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	remediated := exportProductionReplay(
		t, p.publicationHarness, tip, workspace, p.runID, run.SpecDigest,
		submissionSpecification(string(p.runID)), nil, r.id, p.now, []string{"README.md"},
	)
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: r.id, AdmissionID: r.admission.ID,
		ObservedBaseSHA: tip, HeadSHA: remediated.HeadSHA,
		ManifestDigest: remediated.ManifestDigest, RecordedAt: remediated.ImportOptions.CommitDate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, remediated); err != nil {
		t.Fatal(err)
	}
	p.replay = remediated
	p.now = p.now.Add(time.Minute)
	p.scriptCleanReview(r.authority.ReviewRound, tip, remediated.HeadSHA)
}

// handoffCheckPublication is the run's current publication, whose handoff
// check a conflict would seal.
func (p *productionPublicationHarness) handoffCheckPublication(t *testing.T) domain.InvocationID {
	t.Helper()
	var publication domain.InvocationID
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		publication, err = tx.CurrentPublicationInvocationID(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return publication
}

// restartHandoffConflict restarts the engine and checks the run still holds
// r's authority and the handoff check that sealed its tip, unchanged; the
// caller's next pass runs on the restarted engine.
func (p *productionPublicationHarness) restartHandoffConflict(t *testing.T, r handoffConflictRemediation) {
	t.Helper()
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		current, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if current == nil || *current.Reentry != *r.authority.Reentry ||
			current.PublicationID() != r.authority.PublicationID() {
			t.Errorf("authority after the restart = %#v, want %#v", current, r.authority)
		}
		check, err := tx.GetHandoffCheck(p.ctx, p.runID, r.check.PublicationID)
		if err == nil && check != r.check {
			t.Errorf("sealing handoff check after the restart = %#v, want %#v", check, r.check)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// assertHandoffConflictPublished checks the remediated head met a fresh
// handoff check at the tip it was written on, needing no merge, and is the
// head the run's one pull request carries.
func (p *productionPublicationHarness) assertHandoffConflictPublished(
	t *testing.T, r handoffConflictRemediation,
) {
	t.Helper()
	tip := r.authority.Reentry.BaseSHA
	check := p.handoffCheck(t)
	if check.PublicationID != r.authority.PublicationID() || check.HeadSHA != p.replay.HeadSHA ||
		check.EvidenceBaseSHA != tip || check.TipSHA != tip || check.Merge != nil {
		t.Fatalf("remediated handoff check = %#v, want %s at the tip %s", check, p.replay.HeadSHA, tip)
	}
	ready := p.currentReadyItem(t)
	if ready.ID != r.authority.ReadyItemID() || ready.PRHeadSHA != p.replay.HeadSHA ||
		ready.Status != domain.StatusOpen {
		t.Fatalf("ready item = %#v, want the authority's open item at the remediated head", ready)
	}
	prs := p.forge.PullRequests()
	if len(prs) != 1 || prs[0].HeadSHA != p.replay.HeadSHA {
		t.Fatalf("pull requests = %#v, want one at the remediated head %s", prs, p.replay.HeadSHA)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if review.Round != r.authority.ReviewRound || review.BaseSHA != tip || review.HeadSHA != p.replay.HeadSHA {
			t.Errorf("latest review = round %d at %s/%s, want round %d at the tip",
				review.Round, review.BaseSHA, review.HeadSHA, r.authority.ReviewRound)
		}
		published, err := tx.PublishedPublicationInvocationID(p.ctx, p.runID)
		if err == nil && published != r.authority.PublicationID() {
			t.Errorf("published invocation = %s, want the authority's %s", published, r.authority.PublicationID())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionBlockedItemID(p.runID)); err == nil {
		t.Fatal("a remediated handoff conflict wrote a blocked item")
	}
}

// TestHandoffConflictRemediatesAtTheTip: plan §7, revision 110. A head that
// conflicts with the tip is remediated at that tip, and the new head is
// verified, reviewed, handed off, and published as the run's first pull
// request. The conflicted head is never published.
func TestHandoffConflictRemediatesAtTheTip(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	conflicted := p.replay.HeadSHA
	advanced, reviews := p.advanceBaseBeforePublication(t, "README.md", "conflicting upstream change\n")
	r := p.startHandoffConflictRemediation(t, 1)
	if r.authority.HandoffConflictCause != domain.HandoffConflictTextual || !r.authority.FirstPublication ||
		r.authority.Reentry.BaseSHA != advanced || r.authority.Reentry.HeadSHA != conflicted {
		t.Fatalf("authority = %#v, want a textual first publication at %s", r.authority, advanced)
	}
	if len(r.account.ConflictedPaths) != 1 || r.account.ConflictedPaths[0] != `"README.md"` ||
		r.account.ReportDigest != "" || r.account.VerificationTranscript != "" {
		t.Fatalf("handoff conflict account = %#v, want the quoted conflicted path", r.account)
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("conflicted head reached the forge: %d refs, %d prs", refs, prs)
	}
	// A restart after the remediation's admission, and another after its
	// import, resume the same authority and sealed tip.
	p.restartHandoffConflict(t, r)
	p.finishHandoffConflictRemediation(t, r, "conflicting upstream change\nproduction change\n")
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	p.restartHandoffConflict(t, r)
	p.reconcileUntilReady(t, 6)

	p.assertHandoffConflictPublished(t, r)
	if len(reviews.requests) != 2 || reviews.requests[1].BaseSHA != advanced {
		t.Fatalf("review requests = %#v, want the run's round and one at the tip", reviews.requests)
	}
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d prs, want one of each", refs, prs)
	}
}

// TestHandoffVerificationFailureRemediatesAtTheTip: a merge into the tip that
// fails verification is remediated like a textual conflict, and the input
// carries the failed verification's steps and transcript.
func TestHandoffVerificationFailureRemediatesAtTheTip(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	advanced, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.failHandoffVerification(t, func(room *productionRoom) { room.fail = true })
	r := p.startHandoffConflictRemediation(t, 1)
	if r.authority.HandoffConflictCause != domain.HandoffConflictMergeVerificationFailed ||
		r.authority.Reentry.BaseSHA != advanced {
		t.Fatalf("authority = %#v, want a failed merge verification at %s", r.authority, advanced)
	}
	if len(r.account.ConflictedPaths) != 0 || r.account.ReportDigest == "" ||
		len(r.account.VerificationSteps) == 0 || r.account.TranscriptDigest == "" ||
		!strings.Contains(r.account.VerificationTranscript, "injected verification failure") {
		t.Fatalf("handoff conflict account = %#v, want the failed verification's evidence", r.account)
	}
	if r.check.Merge != nil {
		t.Fatalf("failed handoff recorded merge %#v", r.check.Merge)
	}
	p.room.fail = false
	p.finishHandoffConflictRemediation(t, r, "production change\nfixed against the tip\n")
	p.reconcileUntilReady(t, 6)
	p.assertHandoffConflictPublished(t, r)
}

// TestHandoffConflictRemediationWithoutAChangeEscalates: a remediation whose
// candidate is the tip's own tree dropped the conflicted change, so it ends as
// any unchanged remediation does, on a review-dispute item, and nothing
// reaches the forge.
func TestHandoffConflictRemediationWithoutAChangeEscalates(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.advanceBaseBeforePublication(t, "README.md", "conflicting upstream change\n")
	r := p.startHandoffConflictRemediation(t, 1)
	p.finishHandoffConflictRemediation(t, r, "conflicting upstream change\n")
	for range 3 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	item, err := p.attention.GetAttentionItem(p.ctx,
		domain.ItemID(fmt.Sprintf("production-remediation-dissent-%s-1", p.runID)))
	if err != nil || item.Item.Type != domain.AttentionReviewDispute || item.Item.Status != domain.StatusOpen {
		t.Fatalf("unchanged remediation item = %#v, %v, want an open review dispute", item, err)
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("an unchanged remediation reached the forge: %d refs, %d prs", refs, prs)
	}
}

// TestHandoffConflictRemediationFailureRaisesTheFailureCard: a remediation
// that fails ends as any failed invocation does, on its execution-failure
// card, and the conflicted head never reaches the forge.
func TestHandoffConflictRemediationFailureRaisesTheFailureCard(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.advanceBaseBeforePublication(t, "README.md", "conflicting upstream change\n")
	r := p.startHandoffConflictRemediationWith(t, 1, fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.StageResult{Summary: "The remediation ended without an accepted result."},
	})
	cardID := domain.ItemID("execution-failure-" + string(r.id))
	for pass := 0; ; pass++ {
		if pass == 6 {
			t.Fatalf("failure card %q did not open within %d passes", cardID, pass)
		}
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		card, err := p.attention.GetAttentionItem(p.ctx, cardID)
		if err == nil && card.Item.Status == domain.StatusOpen {
			break
		}
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("a failed remediation reached the forge: %d refs, %d prs", refs, prs)
	}
}

// TestHandoffConflictWithoutRoomForAReviewBlocks: a remediation whose review
// the hard round limit has no room for never starts; the run ends on today's
// publish_blocked item naming the conflict.
func TestHandoffConflictWithoutRoomForAReviewBlocks(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithPolicyKeys(t, "", []domain.PolicyKey{{
		Key: "review.hard_round_limit", Value: fmt.Sprint(1),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest("run-production-publication", "review-hard-round-limit"),
		},
	}})
	p.startAndRecordExport(t)
	_, reviews := p.advanceBaseBeforePublication(t, "README.md", "conflicting upstream change\n")
	for range 3 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	p.assertHandoffBlocked(t, domain.PublicationBlockHandoffConflict)
	p.assertNoReviewAtTheTip(t, reviews)
	if len(reviews.requests) != 1 || p.room.runs != 1 {
		t.Fatalf("conflicting handoff reviewed %d times, verified %d times, want 1 and 1",
			len(reviews.requests), p.room.runs)
	}
}

// TestHandoffConflictRemediatesAgainAtANewerTip: the remediated head meets a
// fresh handoff check under its own publication. A tip that moved again into
// a second conflict starts a second remediation within the same bound.
func TestHandoffConflictRemediatesAgainAtANewerTip(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.advanceBaseBeforePublication(t, "README.md", "conflicting upstream change\n")
	first := p.startHandoffConflictRemediation(t, 1)
	p.finishHandoffConflictRemediation(t, first, "conflicting upstream change\nproduction change\n")
	newer := p.advanceBase(t, "README.md", "a second upstream change\n")
	p.audit.AuditedCommitSHA = newer
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	second := p.startHandoffConflictRemediation(t, 2)
	if second.authority.PredecessorItemID != first.authority.ReadyItemID() ||
		second.authority.Reentry.BaseSHA != newer || !second.authority.FirstPublication {
		t.Fatalf("second authority = %#v, want a first publication after %s at %s",
			second.authority, first.authority.ReadyItemID(), newer)
	}
	p.finishHandoffConflictRemediation(t, second, "a second upstream change\nproduction change\n")
	p.reconcileUntilReady(t, 6)
	p.assertHandoffConflictPublished(t, second)
}

// TestHandoffConflictOnAFeedbackSuccessorUpdatesThePullRequest: a feedback
// return whose head conflicts with the tip at its handoff is remediated there
// and updates the run's one pull request, never opening a second.
func TestHandoffConflictOnAFeedbackSuccessorUpdatesThePullRequest(t *testing.T) {
	t.Parallel()
	p, _, round, _ := returnToAgentAfterRemediation(t, false)
	published := p.forge.PullRequests()
	if len(published) != 1 {
		t.Fatalf("pull requests before the feedback cycle = %d, want 1", len(published))
	}
	advanced := p.advanceBase(t, "README.md", "conflicting upstream change\n")
	p.audit.AuditedCommitSHA = advanced
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	r := p.startHandoffConflictRemediation(t, round)
	if r.authority.FirstPublication || r.authority.Reentry.BaseSHA != advanced ||
		!strings.HasPrefix(string(r.authority.PredecessorItemID), "production-ready-feedback-") {
		t.Fatalf("authority = %#v, want a pull request update after the feedback cycle", r.authority)
	}
	p.finishHandoffConflictRemediation(t, r,
		"conflicting upstream change\nproduction change\nremediated review finding\nreturned feedback\n")
	p.reconcileUntilReady(t, 6)
	p.assertHandoffConflictPublished(t, r)
	if prs := p.forge.PullRequests(); prs[0].Number != published[0].Number {
		t.Fatalf("updated pull request #%d, want #%d", prs[0].Number, published[0].Number)
	}
}
