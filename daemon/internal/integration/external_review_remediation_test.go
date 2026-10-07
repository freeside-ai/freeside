package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// remediationInputForTest is the part of a remediation input these tests read.
type remediationInputForTest struct {
	Instruction          string            `json:"instruction"`
	CandidatePatchBase64 []byte            `json:"candidate_patch_base64"`
	Findings             []domain.Finding  `json:"findings"`
	ExternalFindings     []json.RawMessage `json:"external_findings"`
}

// remediateExternalCycle drives the remediation an external review cycle's
// first round (round) started to its re-review: it authenticates the dispatch
// intent across a restart, dispatches the remediator, records its export as a
// new head, and reconciles until the remediated task completes with a clean
// review of round+1. It returns the remediated replay and what the remediator
// was handed.
func (p *productionPublicationHarness) remediateExternalCycle(
	t *testing.T, round int, content string,
) (engine.ProductionReplay, remediationInputForTest) {
	t.Helper()
	return p.remediateExternalCycleWith(t, round, content, externalRemediationOptions{})
}

// externalRemediationOptions vary remediateExternalCycle: pushback names the
// findings the remediator declines in its export, and blocked expects the
// remediated task to end on a blocked item instead of a ready one.
type externalRemediationOptions struct {
	pushback []domain.FindingID
	blocked  bool
}

func (p *productionPublicationHarness) remediateExternalCycleWith(
	t *testing.T, round int, content string, opts externalRemediationOptions,
) (engine.ProductionReplay, remediationInputForTest) {
	t.Helper()
	var delivered []exec.StartSpec
	p.productionDelivery = func(_ context.Context, spec exec.StartSpec) error {
		delivered = append(delivered, spec)
		return nil
	}
	// The adjudication and the dispatch intent are durable; a new process
	// resumes them without the route in memory.
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)

	remediationID := domain.InvocationID(fmt.Sprintf("inv-remediate-%d-%s", round, p.runID))
	remediationStageID := domain.StageID(fmt.Sprintf("remediate-%d-%s", round, p.runID))
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(p.ctx, string(remediationID))
		if err != nil {
			return err
		}
		if _, err := engine.AuthenticateRemediationInvocationTransition(
			p.ctx, tx, entry, p.runID, remediationStageID,
		); err != nil {
			return err
		}
		if _, err := engine.AuthenticateRemediationInvocationTransition(
			p.ctx, tx, entry, "foreign-run", remediationStageID,
		); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("foreign remediation run = %v, want ErrParentKeyMismatch", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("authenticate remediation transition: %v", err)
	}
	p.driver.Script(remediationID, fake.StageScript{
		PendingInspects: 1, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Summary: "Remediation export completed."},
	})
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("remediation dispatch = %#v, %v", result, err)
	}
	if len(delivered) != 1 || delivered[0].RunID != p.runID ||
		delivered[0].StageID != remediationStageID || delivered[0].StageInputs == nil ||
		delivered[0].StageInputs.PromptPackageDigest != p.remediationPromptPackage {
		t.Fatalf("remediation delivery = %#v", delivered)
	}
	var (
		run       domain.Run
		admission domain.ExecutionAdmission
		artifact  domain.Artifact
	)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		if run, err = tx.GetRun(p.ctx, p.runID); err != nil {
			return err
		}
		if admission, err = tx.GetExecutionAdmissionRecord(p.ctx, remediationID); err != nil {
			return err
		}
		artifact, err = tx.GetArtifact(
			p.ctx, domain.ArtifactID(fmt.Sprintf("remediation-input-%d-%s", round, p.runID)))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body, err := p.blobs.Open(artifact.Digest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil {
		t.Fatal(err)
	}
	var input remediationInputForTest
	if err := json.Unmarshal(encoded, &input); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(input.Instruction) == "" ||
		!bytes.Contains(input.CandidatePatchBase64, []byte("+production change")) {
		t.Fatalf("remediation input did not reconstruct the candidate head: %#v", input)
	}

	remediated := buildProductionReplayWithContentAt(
		t, p.publicationHarness, p.runID, run.SpecDigest,
		submissionSpecification(string(p.runID)), nil, remediationID,
		fakePublicationTime.Add(time.Duration(round)*time.Minute), content,
	)
	if len(opts.pushback) > 0 {
		remediated = withRemediatorPushback(
			t, p.publicationHarness, remediated, opts.pushback, "the reviewer's finding is not in this work unit")
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: remediationID, AdmissionID: admission.ID,
		ObservedBaseSHA: p.baseSHA, HeadSHA: remediated.HeadSHA,
		ManifestDigest: remediated.ManifestDigest, RecordedAt: remediated.ImportOptions.CommitDate,
		EvidenceManifestDigest: remediated.EvidenceManifestDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordProductionExecutionExport(p.ctx, p.store, export, remediated); err != nil {
		t.Fatal(err)
	}
	p.replay = remediated
	p.now = p.now.Add(time.Minute)
	p.scriptCleanReview(round+1, p.baseSHA, remediated.HeadSHA)
	var completed engine.ReconcileResult
	for range 4 {
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		completed.PublicationTasksCompleted += result.PublicationTasksCompleted
		completed.ReadyItemsCreated += result.ReadyItemsCreated
		completed.BlockedItemsCreated += result.BlockedItemsCreated
		if completed.PublicationTasksCompleted > 0 {
			break
		}
	}
	wantReady, wantBlocked := 1, 0
	if opts.blocked {
		wantReady, wantBlocked = 0, 1
	}
	if completed.PublicationTasksCompleted != 1 || completed.ReadyItemsCreated != wantReady ||
		completed.BlockedItemsCreated != wantBlocked {
		t.Fatalf("remediation convergence = %#v", completed)
	}
	return remediated, input
}

// TestExternalReviewCycleRecordsNoFixForAPushedBackFinding: the cycle's first
// round routes the reviewer's finding to a fix, and the remediator pushes
// back on it while changing the head. The pushback escalates the run on a
// dispute item, as it does for a finding of Freeside's own, and the round
// records no outcome for the reviewer's finding: a `fixed` with no fix would
// close it for good, since a fixed finding starts no second cycle.
func TestExternalReviewCycleRecordsNoFixForAPushedBackFinding(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
	scriptAdjudication(t, driver, remediateVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 ||
		result.BlockedItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	remediated, _ := p.remediateExternalCycleWith(t, 2, "production change\nnot the reviewer's fix\n",
		externalRemediationOptions{pushback: []domain.FindingID{findings[0].ID}, blocked: true})
	if external := p.externalDispositions(t); len(external) != 0 {
		t.Fatalf("external dispositions after the pushback = %#v, want none", external)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(p.ctx, domain.ProductionFindingAdjudicationItemID(p.runID, 3, 1))
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionReviewDispute || item.PRHeadSHA != remediated.HeadSHA ||
			!strings.Contains(item.Reason, string(findings[0].ID)) ||
			!strings.Contains(item.Reason, "not in this work unit") ||
			slices.Contains(item.RequestedDecision, domain.ActionApprove) {
			t.Errorf("pushback item = %#v", item)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the pushback item: %v", err)
	}
	if end := p.externalReviewEnd(t); end.readyItem {
		t.Fatalf("a pushed-back cycle reached ready: %#v", end)
	}
}

// assertExternalFindingsRemediated checks a remediation input against issue
// #1767 decision 3: every external finding is in external_findings, quoted and
// labelled, and none is in findings.
func assertExternalFindingsRemediated(
	t *testing.T, input remediationInputForTest, findings []domain.Finding,
) {
	t.Helper()
	if len(input.ExternalFindings) != len(findings) {
		t.Fatalf("external_findings = %s, want %d findings", input.ExternalFindings, len(findings))
	}
	for index, finding := range findings {
		var got struct {
			FindingID     domain.FindingID `json:"finding_id"`
			ReviewerLogin string           `json:"reviewer_login"`
			ThreadID      string           `json:"thread_id"`
			HeadSHA       string           `json:"head_sha"`
			QuotedText    string           `json:"quoted_text"`
			Notice        string           `json:"notice"`
		}
		if err := json.Unmarshal(input.ExternalFindings[index], &got); err != nil {
			t.Fatal(err)
		}
		if got.FindingID != finding.ID || got.ReviewerLogin != finding.External.ReviewerLogin ||
			got.ThreadID != finding.External.ThreadID || got.HeadSHA != finding.External.HeadSHA ||
			got.QuotedText != `"`+finding.Message+`"` ||
			!strings.Contains(got.Notice, "quoted as data") {
			t.Errorf("external finding %d = %#v", index, got)
		}
		for _, own := range input.Findings {
			if own.ID == finding.ID || own.Message == finding.Message {
				t.Errorf("findings carries external finding %s: %#v", finding.ID, own)
			}
		}
	}
}

// TestExternalReviewCycleRemediatesAnAcceptedFinding: Freeside's own review
// of the head is clean and adjudication accepts the reviewer's finding, so
// the cycle's first round starts a remediation, as an ordinary round with a
// finding does. The remediated head is verified, reviewed, and pushed to the
// pull request; the finding is recorded fixed by round 3's record; the run
// is ready again on the new head; the remediation round's diff metrics
// compare the cycle's head with the remediated one, which proves the
// remediator's source tree came from the re-entry checkpoint; and the
// timeline's gate (signet's remediation lineage) admits the remediation.
// The cycle's authority stays the current successor: the in-flight
// remediation re-records it, and only an operator's accepted continuation
// seals a remediation successor.
func TestExternalReviewCycleRemediatesAnAcceptedFinding(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	head := p.replay.HeadSHA

	p.scriptCleanReview(2, p.baseSHA, head)
	scriptAdjudication(t, driver, remediateVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 ||
		result.BlockedItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	if end := p.externalReviewEnd(t); len(end.reviewItems) != 0 || end.readyItem {
		t.Fatalf("a cycle with a fix pending ended early: %#v", end)
	}
	remediated, input := p.remediateExternalCycle(t, 2, "production change\nfixed the handle\n")
	assertExternalFindingsRemediated(t, input, findings)
	if len(input.Findings) != 0 {
		t.Fatalf("findings = %#v, want none for a clean record", input.Findings)
	}

	pr := p.forge.pullRequests()[0]
	p.forge.mu.Lock()
	pushed := p.forge.refs[pr.HeadRef]
	p.forge.mu.Unlock()
	if pushed != remediated.HeadSHA {
		t.Fatalf("pull request head = %s, want the remediated head %s", pushed, remediated.HeadSHA)
	}
	if p.room.runs != 3 {
		t.Fatalf("verification runs = %d, want the first head, the cycle, and the remediation", p.room.runs)
	}
	if ready := p.currentReadyItem(t); ready.Status != domain.StatusOpen ||
		ready.PRHeadSHA != remediated.HeadSHA {
		t.Fatalf("ready item = %#v", ready)
	}
	dispositions := p.externalDispositions(t)
	if len(dispositions) != 1 {
		t.Fatalf("external dispositions = %#v, want one", dispositions)
	}
	fixed := dispositions[0]
	if fixed.FindingID != findings[0].ID || fixed.Round != 2 ||
		fixed.Disposition != domain.ReviewDispositionFixed || fixed.AdjudicationDigest != nil ||
		fixed.RemediationInvocationID == nil ||
		*fixed.RemediationInvocationID != engine.ProductionReviewInvocationID(p.runID, 3) ||
		!strings.Contains(fixed.Reason, "Freeside has not proven this finding fixed") {
		t.Fatalf("fixed disposition = %#v", fixed)
	}
	end := p.externalReviewEnd(t)
	if !end.readyItem || end.pending != 0 || len(end.reviewItems) != 0 {
		t.Fatalf("cycle end = %#v", end)
	}
	if end.successor.EffectiveOrigin() != domain.PublicationSuccessorExternalReview {
		t.Fatalf("current successor = %#v, want the cycle's authority", end.successor)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		metrics, err := tx.GetReviewRoundDiffMetrics(p.ctx, p.runID, 3)
		if err != nil {
			return err
		}
		if metrics.Round.BaseSHA != head || metrics.Round.HeadSHA != remediated.HeadSHA ||
			metrics.Cumulative.BaseSHA != p.baseSHA || metrics.Cumulative.HeadSHA != remediated.HeadSHA {
			t.Errorf("round 3 diff metrics = %#v, want %s..%s over %s", metrics, head, remediated.HeadSHA, p.baseSHA)
		}
		return nil
	}); err != nil {
		t.Fatalf("read round 3 diff metrics: %v", err)
	}
	timeline, err := p.attention.GetRunTimeline(p.ctx, p.runID)
	if err != nil {
		t.Fatalf("run timeline after remediation: %v", err)
	}
	if timeline.Review == nil || len(timeline.Review.Rounds) != 3 {
		t.Fatalf("timeline review = %#v, want three rounds", timeline.Review)
	}
	if subject := timeline.Review.Rounds[2].Subject; subject == nil || subject.Kind != domain.ReviewSubjectRemediation {
		t.Fatalf("round 3 subject = %#v, want the remediation", subject)
	}
	if p.startExternalReview(t) {
		t.Fatal("a fixed finding started a second cycle")
	}
	if len(adjudicatorRequests(driver)) != 1 {
		t.Fatal("the remediation round adjudicated again")
	}
}

// TestExternalReviewCycleRemediatesAMixedRound: the cycle's review finds
// something of its own beside the reviewer's finding, and both are routed to
// a fix. One remediation fixes both: the record's finding by the ordinary
// fingerprint proof, the external one by the round that reviewed the fix.
func TestExternalReviewCycleRemediatesAMixedRound(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	head := p.replay.HeadSHA

	own := domain.Finding{
		ID: "external-cycle-own-finding", RunID: p.runID, Source: "codex_local",
		Severity: domain.FindingSeverityP1,
		Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
		Message:  "the production change is incomplete",
		RawText:  "the production change is incomplete", CreatedAt: p.now,
	}
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: head,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("mixed cycle findings")),
			Findings: []domain.Finding{own},
		},
	})
	scriptAdjudication(t, driver, remediateVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 ||
		result.BlockedItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	_, input := p.remediateExternalCycle(t, 2, "production change\nfixed both findings\n")
	assertExternalFindingsRemediated(t, input, findings)
	if len(input.Findings) != 1 || input.Findings[0].ID != own.ID {
		t.Fatalf("findings = %#v, want the record's own finding", input.Findings)
	}

	external := p.externalDispositions(t)
	if len(external) != 1 || external[0].FindingID != findings[0].ID || external[0].Round != 2 ||
		external[0].Disposition != domain.ReviewDispositionFixed ||
		external[0].RemediationInvocationID == nil ||
		*external[0].RemediationInvocationID != engine.ProductionReviewInvocationID(p.runID, 3) {
		t.Fatalf("external dispositions = %#v", external)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		disposition, err := tx.GetFindingDisposition(p.ctx, own.ID, 2)
		if err != nil {
			return err
		}
		if disposition.Disposition != domain.ReviewDispositionFixed ||
			disposition.RemediationInvocationID != engine.ProductionReviewInvocationID(p.runID, 3) {
			t.Errorf("record finding disposition = %#v", disposition)
		}
		if _, err := tx.GetFindingDisposition(p.ctx, findings[0].ID, 2); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("external finding review disposition = %v, want none", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExternalReviewCycleRemediationCountsTowardTheRoundLimit: an external
// cycle that fixes a finding spends two review rounds, the cycle's own and
// the fix's. A second cycle after it starts past the hard round limit and
// ends on a person before requesting a review, as a first cycle at the limit
// does.
func TestExternalReviewCycleRemediationCountsTowardTheRoundLimit(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithPolicyKeys(t, "", []domain.PolicyKey{{
		Key: "review.hard_round_limit", Value: "3",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest("run-production-publication", "review-hard-round-limit"),
		},
	}})
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
	p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
	scriptAdjudication(t, driver, remediateVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	remediated, _ := p.remediateExternalCycle(t, 2, "production change\nfixed the handle\n")

	p.leaveExternalFinding(t, 2, externalMaintainer(), remediated.HeadSHA, "and this one races")
	if !p.startExternalReview(t) {
		t.Fatal("a finding on the remediated head started no cycle")
	}
	reviews := p.recordReviews(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("second cycle result = %#v, %v", result, err)
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
		t.Fatalf("second cycle end = %#v", end)
	}
	item := end.reviewItems[0]
	if item.Type != domain.AttentionReviewDiminishing ||
		!strings.HasPrefix(item.Reason, "Review exhausted the resolved hard limit of 3 rounds. ") ||
		!strings.Contains(item.Reason, `maintainer on review_comment/2: "and this one races"`) {
		t.Fatalf("limit item = %s: %s", item.Type, item.Reason)
	}
	if len(reviews.requests) != 0 || len(adjudicatorRequests(driver)) != 1 {
		t.Fatalf("a cycle past the round limit requested %d reviews and adjudicated %d times",
			len(reviews.requests), len(adjudicatorRequests(driver)))
	}
}
