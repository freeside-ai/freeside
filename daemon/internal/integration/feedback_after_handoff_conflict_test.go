package integration_test

import (
	"context"
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
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// tipFeedback is a return to the agent started on an item a handoff
// conflict remediation published: the feedback's invocation, its admission,
// and the base its input names.
type tipFeedback struct {
	id        domain.InvocationID
	admission domain.ExecutionAdmission
	inputBase string
}

// publishThroughAHandoffConflict publishes the run's first pull request
// through a handoff conflict remediated at the tip, and returns what that
// conflict started.
func publishThroughAHandoffConflict(t *testing.T) (*productionPublicationHarness, handoffConflictRemediation) {
	t.Helper()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.advanceBaseBeforePublication(t, "README.md", "conflicting upstream change\n")
	r := p.startHandoffConflictRemediation(t, 1)
	p.finishHandoffConflictRemediation(t, r, "conflicting upstream change\nproduction change\n")
	p.reconcileUntilReady(t, 6)
	p.assertHandoffConflictPublished(t, r)
	return p, r
}

// feedbackDevice is the paired device the tests decide from.
const feedbackDevice domain.DeviceID = "feedback-device"

// submitReturnToAgent pairs the feedback device and returns the run's open
// ready item to the agent under commandID, and names the feedback.
func (p *productionPublicationHarness) submitReturnToAgent(t *testing.T, commandID string) domain.InvocationID {
	t.Helper()
	ready, err := p.attention.GetAttentionItem(p.ctx, p.currentReadyItem(t).ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Item.Status != domain.StatusOpen || !ready.Item.Offers(domain.ActionReturnToAgent) {
		t.Fatalf("ready item = %s offering %v, want open with return to agent",
			ready.Item.Status, ready.Item.RequestedDecision)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{
			ID: feedbackDevice, DisplayName: "Feedback device", Status: domain.DeviceActive, PairedAt: p.now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	p.decide(t, ready, commandID, domain.ActionReturnToAgent, "Match the README to the help text.")
	return domain.InvocationID("inv-operator-feedback-" + commandID)
}

// decide submits action on item from the feedback device.
func (p *productionPublicationHarness) decide(
	t *testing.T, item signet.AttentionItemSnapshot, commandID string, action domain.Action, message string,
) {
	t.Helper()
	if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: commandID, DeviceID: feedbackDevice, ExpectedEntityVersion: item.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.Item.ID, ItemVersion: item.Item.ItemVersion,
			PRHeadSHA: item.Item.PRHeadSHA, ArtifactDigests: item.Item.ArtifactDigests,
			Action: action, Message: message,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// dispatchFeedback runs lane passes until feedback id is admitted under
// script, and reads its admission and the base its input names.
func (p *productionPublicationHarness) dispatchFeedback(
	t *testing.T, id domain.InvocationID, script fake.StageScript,
) tipFeedback {
	t.Helper()
	p.driver.Script(id, script)
	f := tipFeedback{id: id}
	var artifact domain.Artifact
	for pass := 0; ; pass++ {
		if pass == 3 {
			t.Fatalf("feedback %s was not admitted within %d passes", id, pass)
		}
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		admitted := false
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			f.admission, err = tx.GetExecutionAdmissionRecord(p.ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			admitted = true
			request, err := tx.OperatorFeedbackIntent(p.ctx, id)
			if err != nil {
				return err
			}
			artifact, err = tx.GetArtifact(p.ctx, request.InputArtifactID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if admitted {
			break
		}
	}
	body, err := p.blobs.Open(artifact.Digest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil {
		t.Fatal(err)
	}
	var input struct {
		BaseSHA string `json:"base_sha"`
	}
	if err := json.Unmarshal(encoded, &input); err != nil {
		t.Fatal(err)
	}
	f.inputBase = input.BaseSHA
	return f
}

// feedbackCompletes is a feedback attempt whose export the test records.
var feedbackCompletes = fake.StageScript{
	PendingInspects: 1, Outcome: fake.OutcomeComplete,
	Result: exec.StageResult{Summary: "Feedback export completed."},
}

// candidateAtAdmission records invocation's export as the pipeline would:
// a candidate with content written on the base the invocation was admitted
// at, which is the export's observed base. record writes the export.
func (p *productionPublicationHarness) candidateAtAdmission(
	t *testing.T, invocation domain.InvocationID, admission domain.ExecutionAdmission, content string,
	record func(domain.ExecutionExport, engine.ProductionReplay) error,
) {
	t.Helper()
	base := admission.Base.BaseSHA
	workspace := t.TempDir()
	runGit(t, workspace, "clone", "-q", "--no-hardlinks", p.baseDir, ".")
	runGit(t, workspace, "checkout", "-q", "--detach", base)
	if err := os.RemoveAll(filepath.Join(workspace, ".git")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, workspace, "README.md", content)
	// The import runs in the base checkout, which must be at that base.
	if head := strings.TrimSpace(runGit(t, p.baseDir, "rev-parse", "HEAD")); head != base {
		runGit(t, p.baseDir, "checkout", "-q", "--detach", base)
		defer runGit(t, p.baseDir, "checkout", "-q", "-")
	}
	var run domain.Run
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p.now = p.now.Add(time.Minute)
	replay := exportProductionReplay(
		t, p.publicationHarness, base, workspace, p.runID, run.SpecDigest,
		submissionSpecification(string(p.runID)), nil, invocation, p.now, []string{"README.md"},
	)
	exported, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocation, AdmissionID: admission.ID,
		ObservedBaseSHA: base, HeadSHA: replay.HeadSHA,
		ManifestDigest: replay.ManifestDigest, RecordedAt: replay.ImportOptions.CommitDate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := record(exported, replay); err != nil {
		t.Fatal(err)
	}
	p.replay = replay
	p.now = p.now.Add(time.Minute)
}

func (p *productionPublicationHarness) recordFeedbackExport(
	exported domain.ExecutionExport, replay engine.ProductionReplay,
) error {
	return engine.RecordExecutionExport(p.ctx, p.store, exported, replay, nil, nil)
}

func (p *productionPublicationHarness) recordRemediationExport(
	exported domain.ExecutionExport, replay engine.ProductionReplay,
) error {
	return engine.RecordProductionExecutionExport(p.ctx, p.store, exported, replay)
}

// assertFeedbackPublishedAtTheTip checks the run's current cycle is the
// feedback successor, that its head met a handoff check at tip needing no
// merge, and that it updated pull request number to that head.
func (p *productionPublicationHarness) assertFeedbackPublishedAtTheTip(
	t *testing.T, tip string, round, number int,
) {
	t.Helper()
	var successor *domain.PublicationSuccessor
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		if successor, err = tx.CurrentPublicationSuccessor(p.ctx, p.runID); err != nil {
			return err
		}
		published, err := tx.PublishedPublicationInvocationID(p.ctx, p.runID)
		if err == nil && successor != nil && published != successor.PublicationID() {
			t.Errorf("published invocation = %s, want the feedback cycle's %s",
				published, successor.PublicationID())
		}
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err == nil && (review.Round != round || review.BaseSHA != tip || review.HeadSHA != p.replay.HeadSHA) {
			t.Errorf("latest review = round %d at %s/%s, want round %d at the tip %s",
				review.Round, review.BaseSHA, review.HeadSHA, round, tip)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if successor == nil || successor.EffectiveOrigin() != domain.PublicationSuccessorFeedback {
		t.Fatalf("current successor = %#v, want the feedback cycle", successor)
	}
	check := p.handoffCheck(t)
	if check.PublicationID != successor.PublicationID() || check.HeadSHA != p.replay.HeadSHA ||
		check.EvidenceBaseSHA != tip || check.TipSHA != tip || check.Merge != nil {
		t.Fatalf("feedback handoff check = %#v, want %s at the tip %s", check, p.replay.HeadSHA, tip)
	}
	if ready := p.currentReadyItem(t); ready.ID != successor.ReadyItemID() || ready.PRHeadSHA != p.replay.HeadSHA ||
		ready.Status != domain.StatusOpen {
		t.Fatalf("ready item = %#v, want the feedback cycle's open item at %s", ready, p.replay.HeadSHA)
	}
	prs := p.forge.PullRequests()
	if len(prs) != 1 || prs[0].Number != number || prs[0].HeadSHA != p.replay.HeadSHA {
		t.Fatalf("pull requests = %#v, want #%d at %s", prs, number, p.replay.HeadSHA)
	}
}

// assertAdmittedAtTheTip checks a feedback was admitted, seeded, and handed
// its input at tip.
func assertAdmittedAtTheTip(t *testing.T, f tipFeedback, tip string) {
	t.Helper()
	if f.admission.Base.BaseSHA != tip || f.inputBase != tip {
		t.Fatalf("feedback admitted at %s with input base %s, want the sealed tip %s",
			f.admission.Base.BaseSHA, f.inputBase, tip)
	}
}

// TestReturnToAgentAfterAHandoffConflictPublishes: issue #2699. A return to
// the agent on an item a handoff conflict remediation published starts at
// the tip its candidate was written on, so the feedback's successor seals
// and its head updates the same pull request.
func TestReturnToAgentAfterAHandoffConflictPublishes(t *testing.T) {
	t.Parallel()
	p, r := publishThroughAHandoffConflict(t)
	tip := r.authority.Reentry.BaseSHA
	number := p.forge.PullRequests()[0].Number
	f := p.dispatchFeedback(t, p.submitReturnToAgent(t, "return-after-conflict"), feedbackCompletes)
	assertAdmittedAtTheTip(t, f, tip)
	p.candidateAtAdmission(t, f.id, f.admission,
		"conflicting upstream change\nproduction change\nreturned feedback\n", p.recordFeedbackExport)
	round := r.authority.ReviewRound + 1
	p.scriptCleanReview(round, tip, p.replay.HeadSHA)
	p.reconcileUntilReady(t, 8)
	p.assertFeedbackPublishedAtTheTip(t, tip, round, number)
}

// TestRemediationAfterAHandoffConflictFeedbackPublishes: the remediation of
// a finding in that feedback cycle starts at the same tip, and its head
// updates the same pull request.
func TestRemediationAfterAHandoffConflictFeedbackPublishes(t *testing.T) {
	t.Parallel()
	p, r := publishThroughAHandoffConflict(t)
	tip := r.authority.Reentry.BaseSHA
	number := p.forge.PullRequests()[0].Number
	f := p.dispatchFeedback(t, p.submitReturnToAgent(t, "return-after-conflict"), feedbackCompletes)
	assertAdmittedAtTheTip(t, f, tip)
	p.candidateAtAdmission(t, f.id, f.admission,
		"conflicting upstream change\nproduction change\nreturned feedback\n", p.recordFeedbackExport)
	round := r.authority.ReviewRound + 1
	configureSuccessorJudgments(t, p)
	finding := domain.Finding{
		ID: "tip-feedback-finding", RunID: p.runID, Source: "codex_local", Severity: domain.FindingSeverityP1,
		Location:  &domain.FindingLocation{Path: "README.md", StartLine: 2, EndLine: 2},
		Message:   "The returned feedback needs one more correction.",
		RawText:   "The returned feedback needs one more correction.",
		CreatedAt: p.now,
	}
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, round), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: tip, HeadSHA: p.replay.HeadSHA, CompletedAt: p.now,
			CompletionEvidence: productionDigest([]byte("tip feedback review findings")),
			Findings:           []domain.Finding{finding},
		},
	})
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	remediation := domain.InvocationID(fmt.Sprintf("inv-remediate-%d-%s", round, p.runID))
	p.driver.Script(remediation, fake.StageScript{
		PendingInspects: 1, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Summary: "Feedback finding fixed."},
	})
	started := 0
	for pass := 0; pass < 8 && started == 0; pass++ {
		p.now = p.now.Add(time.Minute)
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		started += result.InvocationsStarted
	}
	var admission domain.ExecutionAdmission
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(p.ctx, remediation)
		return err
	}); err != nil {
		t.Fatalf("remediation %s was not admitted after %d starts: %v", remediation, started, err)
	}
	if admission.Base.BaseSHA != tip {
		t.Fatalf("remediation admitted at %s, want the sealed tip %s", admission.Base.BaseSHA, tip)
	}
	p.candidateAtAdmission(t, remediation, admission,
		"conflicting upstream change\nproduction change\nreturned feedback\nfinding fixed\n",
		p.recordRemediationExport)
	p.scriptCleanReview(round+1, tip, p.replay.HeadSHA)
	p.reconcileUntilReady(t, 8)
	p.assertFeedbackPublishedAtTheTip(t, tip, round+1, number)
}

// TestFeedbackRetryAfterAHandoffConflictStartsAtTheTip: a retry of a failed
// return to the agent on that item resolves to the same published candidate
// through its retry ancestry, so it starts at the same tip.
func TestFeedbackRetryAfterAHandoffConflictStartsAtTheTip(t *testing.T) {
	t.Parallel()
	p, r := publishThroughAHandoffConflict(t)
	tip := r.authority.Reentry.BaseSHA
	failed := p.dispatchFeedback(t, p.submitReturnToAgent(t, "return-after-conflict"), fake.StageScript{
		Outcome: fake.OutcomeFail, Result: exec.StageResult{Summary: "feedback launch failed"},
	})
	assertAdmittedAtTheTip(t, failed, tip)
	var failure signet.AttentionItemSnapshot
	for pass := 0; ; pass++ {
		if pass == 4 {
			t.Fatalf("no retryable failure for %s within %d passes", failed.id, pass)
		}
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		var err error
		failure, err = p.attention.GetAttentionItem(p.ctx, domain.ItemID("execution-failure-"+string(failed.id)))
		if err == nil && failure.Item.Offers(domain.ActionRetry) {
			break
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
	}
	p.decide(t, failure, "retry-after-conflict", domain.ActionRetry, "")
	retry := p.dispatchFeedback(t, "inv-operator-feedback-retry-after-conflict", feedbackCompletes)
	assertAdmittedAtTheTip(t, retry, tip)
}

// TestReturnToAgentWithoutAHandoffConflictKeepsTheDerivedBase: a feedback
// whose returned candidate's base differs from the derived base, with no
// handoff_conflict authority naming it, keeps the derived base. Here the
// operator restarted the daemon on a newer base after the run published;
// only a sealed authority moves the feedback to its candidate's base.
func TestReturnToAgentWithoutAHandoffConflictKeepsTheDerivedBase(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.reconcileUntilReady(t, 6)
	written := p.baseSHA
	restarted := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	p.workflow = p.newEngineForMode(
		t, productionCrashSeams{}, true, map[domain.Digest]bool{p.recipeD: true},
		domain.ModeUnattended, false,
		engine.WithAdmissionDerivation(func(
			_ context.Context, _ domain.InvocationID,
		) (string, domain.BaseRevision, error) {
			return "freeside-handoff-production-publication-ws", domain.BaseRevision{
				Repo: fakePublicationRepo, RepositoryID: p.profile.RepositoryID,
				BaseRef: "main", BaseSHA: restarted,
			}, nil
		}),
	)
	f := p.dispatchFeedback(t, p.submitReturnToAgent(t, "return-after-restart"), feedbackCompletes)
	if f.inputBase != written || f.admission.Base.BaseSHA != restarted {
		t.Fatalf("feedback with input base %s admitted at %s, want the derived base %s",
			f.inputBase, f.admission.Base.BaseSHA, restarted)
	}
}

// TestReturnToAgentAfterAHandoffConflictRefusesAnotherRepository: the tip's
// repository coordinates are the run's production admission's, so a
// feedback whose derived base names another repository, repository ID, or
// branch is refused rather than admitted at the tip.
func TestReturnToAgentAfterAHandoffConflictRefusesAnotherRepository(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*domain.BaseRevision){
		"repository":    func(b *domain.BaseRevision) { b.Repo = "freeside-ai/other-repo" },
		"repository-id": func(b *domain.BaseRevision) { b.RepositoryID++ },
		"branch":        func(b *domain.BaseRevision) { b.BaseRef = "release" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, _ := publishThroughAHandoffConflict(t)
			p.workflow = p.newEngineForMode(
				t, productionCrashSeams{}, true, map[domain.Digest]bool{p.recipeD: true},
				domain.ModeUnattended, false,
				engine.WithAdmissionDerivation(func(
					_ context.Context, _ domain.InvocationID,
				) (string, domain.BaseRevision, error) {
					base := domain.BaseRevision{
						Repo: fakePublicationRepo, RepositoryID: p.profile.RepositoryID,
						BaseRef: "main", BaseSHA: p.baseSHA,
					}
					mutate(&base)
					return "freeside-handoff-production-publication-ws", base, nil
				}),
			)
			id := p.submitReturnToAgent(t, "return-to-another-repository")
			p.driver.Script(id, feedbackCompletes)
			for range 3 {
				if _, err := p.reconcileLanes(); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				if _, err := tx.GetExecutionAdmissionRecord(p.ctx, id); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("feedback admission = %v, want none", err)
				}
				hold, found, err := tx.GetRunHold(p.ctx, p.runID)
				if err != nil || !found || hold.Reason != domain.HoldAdmissionPolicyRefused || hold.Detail == nil ||
					hold.Detail.Check != domain.AdmissionCheckRepositoryIdentityMismatch {
					t.Errorf("run hold = %#v, %t, %v; want a repository identity refusal", hold, found, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
