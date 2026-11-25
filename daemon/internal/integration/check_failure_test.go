package integration_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// failedCheckJob is the one job the tests' required check fails on.
const failedCheckJob = "daemon / linux (test)"

// failRequiredCheck writes what the reconciler leaves when the published
// head's required check fails in class: the reading on the open ready item,
// and the pull request recorded open on that head. It stores log's screened
// excerpt and returns the failure record the trigger builds from it.
func (p *productionPublicationHarness) failRequiredCheck(
	t *testing.T, published domain.AttentionItem, class domain.CheckFailureClass, log string,
) domain.RequiredCheckFailure {
	t.Helper()
	excerpt := domain.ScreenCheckFailureExcerpt([]byte(log))
	digest := domain.Digest(contentaddr.Sum([]byte(excerpt)))
	if _, err := p.blobs.Put(digest, bytes.NewReader([]byte(excerpt))); err != nil {
		t.Fatal(err)
	}
	var failure domain.RequiredCheckFailure
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(p.ctx, published.ID)
		if err != nil {
			return err
		}
		item.RequiredCheck = &domain.RequiredCheck{
			State: domain.RequiredCheckFailed, FailureClass: &class, HeadSHA: item.PRHeadSHA,
			FailedJobs: []string{failedCheckJob}, ObservedAt: p.now.UTC(),
		}
		item.ItemVersion++
		if err := tx.PutAttentionItem(p.ctx, item); err != nil {
			return err
		}
		binding, err := tx.GetReadyItemPRBinding(p.ctx, item.ID)
		if err != nil {
			return err
		}
		if _, err := tx.AppendPullMergeFact(p.ctx, domain.PullMergeFact{
			Repo: binding.Repo, RepositoryID: binding.RepositoryID, PRNumber: binding.PRNumber,
			State: domain.PullRequestOpen, BaseRef: binding.BaseRef, HeadSHA: binding.HeadSHA, ObservedAt: p.now,
		}); err != nil {
			return err
		}
		failure = domain.RequiredCheckFailure{
			Version: domain.RequiredCheckFailureVersion, RunID: p.runID,
			Repo: binding.Repo, PRNumber: binding.PRNumber, HeadSHA: binding.HeadSHA,
			Jobs: []domain.CheckFailureJob{{
				Name: failedCheckJob, JobID: 41, WorkflowRunID: 7, Attempt: 1, ExcerptDigest: digest,
			}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return failure
}

// startCheckFailure runs the trigger as the reconciler does: the probe, then
// the writer with failure. It reports whether a cycle started.
func (p *productionPublicationHarness) startCheckFailure(
	t *testing.T, itemID domain.ItemID, failure domain.RequiredCheckFailure,
) bool {
	t.Helper()
	var due bool
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		due, err = engine.CheckFailureReentryDue(p.ctx, tx, itemID, p.reentryGate)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !due {
		return false
	}
	var started bool
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		var err error
		started, err = engine.StartCheckFailureReentry(p.ctx, tx, itemID, failure, p.reentryGate)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("the probe reported a check failure cycle due and the writer started none")
	}
	return true
}

// TestCheckFailureCycleFixesTheHead drives issue #3598 end to end. The
// published head's required check fails on a test. One failure starts one
// cycle, which reviews the head again, hands it off in place, and starts a
// remediation that carries the failed job and its log excerpt. The fix is
// published under the cycle and the run is ready again on the new head, whose
// check is read from nothing. The in-place handoff's merge into a moved tip
// is the hand-back cycle's, which TestHandBackCycleHandsOffTheAdoptedHead
// drives.
func TestCheckFailureCycleFixesTheHead(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	first := p.publishReady(t)
	head := first.PRHeadSHA
	failure := p.failRequiredCheck(t, first, domain.CheckFailureCode,
		"--- FAIL: TestHandle (0.01s)\n    handle_test.go:12: want 2, got 3\nFAIL\n")

	if !p.startCheckFailure(t, first.ID, failure) {
		t.Fatal("a code failure on the published head started no cycle")
	}
	if p.startCheckFailure(t, first.ID, failure) {
		t.Fatal("one failure started a second cycle")
	}
	p.scriptCleanReview(2, p.baseSHA, head)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	var successor domain.PublicationSuccessor
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		current, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil || current == nil || !current.ReworksCheckFailure() {
			return errors.Join(err, errors.New("no check_failure authority is current"))
		}
		successor = *current
		check, err := tx.GetHandoffCheck(p.ctx, p.runID, successor.InPlaceHandoffID())
		if err != nil {
			return err
		}
		if !check.Proved() || check.TipSHA != p.baseSHA || check.HeadSHA != head {
			t.Errorf("in-place handoff check = %#v, want %s handed off at %s", check, head, p.baseSHA)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if successor.Reentry.HeadSHA != head || successor.Reentry.BaseSHA != p.baseSHA || successor.ReviewRound != 2 {
		t.Fatalf("authority = %#v (re-entry %#v)", successor, successor.Reentry)
	}

	remediated, input := p.remediateExternalCycle(t, 2, "production change\nfixed the failing test\n")
	if len(input.Findings) != 0 || len(input.ExternalFindings) != 0 || !strings.Contains(input.Instruction, "check_failure") {
		t.Fatalf("remediation input = %#v, want the check's failure and no finding", input)
	}
	account := input.CheckFailure
	if account == nil || account.RecordDigest != successor.CheckFailureDigest || len(account.Jobs) != 1 ||
		account.Jobs[0].Name != failedCheckJob || account.Jobs[0].ExcerptDigest != failure.Jobs[0].ExcerptDigest ||
		!strings.Contains(account.Jobs[0].Excerpt, "want 2, got 3") {
		t.Fatalf("check failure account = %#v", account)
	}
	pr := p.forge.PullRequests()[0]
	p.forge.Mu.Lock()
	pushed := p.forge.Refs[pr.HeadRef]
	p.forge.Mu.Unlock()
	if pushed != remediated.HeadSHA {
		t.Fatalf("pull request head = %s, want the fix %s", pushed, remediated.HeadSHA)
	}
	ready := p.currentReadyItem(t)
	if ready.ID != successor.ReadyItemID() || ready.Status != domain.StatusOpen ||
		ready.PRHeadSHA != remediated.HeadSHA || ready.RequiredCheck != nil {
		t.Fatalf("ready item = %#v, want the fix ready with no check reading", ready)
	}
	// The run's timeline shows the fix as the remediation of the clean
	// round that answered the check, though no finding was routed to it.
	timeline, err := p.attention.GetRunTimeline(p.ctx, p.runID)
	if err != nil {
		t.Fatalf("run timeline after the fix: %v", err)
	}
	if timeline.Review == nil || len(timeline.Review.Rounds) != 3 {
		t.Fatalf("timeline review = %#v, want three rounds", timeline.Review)
	}
	if fix := timeline.Review.Rounds[1].Remediation; fix == nil || len(fix.FindingIDs) != 0 || fix.DecidedAt != nil {
		t.Errorf("round 2 remediation = %#v, want the fix with no finding and no decision", fix)
	}
	if subject := timeline.Review.Rounds[2].Subject; subject == nil || subject.Kind != domain.ReviewSubjectRemediation {
		t.Errorf("round 3 subject = %#v, want the remediation", subject)
	}
	// The fix is Freeside's own head, so its own failed check starts the
	// next cycle under a new record.
	again := p.failRequiredCheck(t, ready, domain.CheckFailureCode, "--- FAIL: TestHandle again\n")
	if !p.startCheckFailure(t, ready.ID, again) {
		t.Fatal("the fix's failed check started no cycle")
	}
}

// TestCheckFailureCycleSendsReviewFindingsToAPerson: when the cycle's review
// of the head finds something, the findings go to a person, as on every
// re-entered cycle, and no fix of the check starts.
func TestCheckFailureCycleSendsReviewFindingsToAPerson(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	first := p.publishReady(t)
	head := first.PRHeadSHA
	if !p.startCheckFailure(t, first.ID, p.failRequiredCheck(t, first, domain.CheckFailureCode, "FAIL\n")) {
		t.Fatal("a code failure started no cycle")
	}
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: head,
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("check failure findings")),
			Findings: []domain.Finding{{
				ID: "check-failure-finding-1", RunID: p.runID, Source: "codex_local", Severity: "P1",
				Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
				Message:  "the handle leaks", RawText: "the handle leaks", CreatedAt: p.now,
			}},
		},
	})
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("findings result = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 2))
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionReviewDispute || item.Status != domain.StatusOpen || item.PRHeadSHA != head {
			t.Errorf("review item = %#v", item)
		}
		if _, err := tx.GetOutbox(p.ctx, string(domain.RemediationInvocationID(p.runID, 2))); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("findings on the head started a remediation: %v", err)
		}
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			t.Errorf("findings left %d publication tasks pending", len(pending))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCheckFailureStartsNothingButACodeFailure: an infrastructure failure is
// rerun and an unclassified one is a person's to read, so neither starts a
// cycle; nor does a code failure while the run's latest review is of another
// head.
func TestCheckFailureStartsNothingButACodeFailure(t *testing.T) {
	t.Parallel()
	for _, class := range []domain.CheckFailureClass{domain.CheckFailureInfrastructure, domain.CheckFailureOther} {
		t.Run(string(class), func(t *testing.T) {
			t.Parallel()
			p := newProductionPublicationHarness(t, "")
			first := p.publishReady(t)
			if p.startCheckFailure(t, first.ID, p.failRequiredCheck(t, first, class, "FAIL\n")) {
				t.Fatalf("a %s failure started a cycle", class)
			}
		})
	}
}

// TestCheckFailureRefusesAnotherHeadsRecord: the writer is handed a record
// the trigger built, and refuses, writing nothing, one that names another
// head or a job the card does not name. The item stays open, and the right
// record still starts the cycle.
func TestCheckFailureRefusesAnotherHeadsRecord(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	first := p.publishReady(t)
	failure := p.failRequiredCheck(t, first, domain.CheckFailureCode, "FAIL\n")
	otherHead := failure
	otherHead.HeadSHA = strings.Repeat("e", 40)
	otherJob := failure
	otherJob.Jobs = []domain.CheckFailureJob{failure.Jobs[0]}
	otherJob.Jobs[0].Name = "app / macos (test)"
	for name, record := range map[string]domain.RequiredCheckFailure{"head": otherHead, "job": otherJob} {
		err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
			_, err := engine.StartCheckFailureReentry(p.ctx, tx, first.ID, record, p.reentryGate)
			return err
		})
		if !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("a record naming another %s = %v, want ErrParentKeyMismatch", name, err)
		}
	}
	if item := p.currentReadyItem(t); item.ID != first.ID || item.Status != domain.StatusOpen {
		t.Fatalf("a refused record moved the ready item: %#v", item)
	}
	if !p.startCheckFailure(t, first.ID, failure) {
		t.Fatal("the trigger's own record started no cycle after the refusals")
	}
}
