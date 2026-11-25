package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

// checkFailureFixture is a published ready item whose pull request's
// required check failed on code, the failure record the daemon wrote for it,
// and the check_failure authority that state proves (issue #3598).
type checkFailureFixture struct {
	externalReviewFixture
	failure domain.RequiredCheckFailure
}

type checkFailureOptions struct {
	// leaveOpen skips superseding the predecessor.
	leaveOpen bool
	// reading edits the item's required-check reading before it is written.
	reading func(*domain.RequiredCheck)
	// pull edits the recorded pull fact before it is appended.
	pull func(*domain.PullMergeFact)
}

func codeFailureReading(head string) domain.RequiredCheck {
	class := domain.CheckFailureCode
	return domain.RequiredCheck{
		State: domain.RequiredCheckFailed, FailureClass: &class, HeadSHA: head,
		FailedJobs: []string{"daemon / linux (test)"}, ObservedAt: reentryAt,
	}
}

// putRequiredCheck writes a reading onto an open ready item, as the
// reconciler does.
func putRequiredCheck(t *testing.T, st *Store, id domain.ItemID, reading domain.RequiredCheck) {
	t.Helper()
	ctx := context.Background()
	if err := st.Write(ctx, func(tx *WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, id)
		if err != nil {
			return err
		}
		item.RequiredCheck = &reading
		item.ItemVersion++
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
}

func openPullFactOn(binding domain.ReadyItemPRBinding, head string, minute int) domain.PullMergeFact {
	return domain.PullMergeFact{
		Repo: binding.Repo, RepositoryID: binding.RepositoryID, PRNumber: binding.PRNumber,
		State: domain.PullRequestOpen, BaseRef: binding.BaseRef, HeadSHA: head,
		ObservedAt: reentryAt.Add(time.Duration(minute) * time.Minute),
	}
}

func checkFailureRecord(runID domain.RunID, binding domain.ReadyItemPRBinding, head string, workflowRun int64) domain.RequiredCheckFailure {
	return domain.RequiredCheckFailure{
		Version: domain.RequiredCheckFailureVersion, RunID: runID, Repo: binding.Repo, PRNumber: binding.PRNumber,
		HeadSHA: head,
		Jobs: []domain.CheckFailureJob{{
			Name: "daemon / linux (test)", JobID: 11, WorkflowRunID: workflowRun, Attempt: 1,
			ExcerptDigest: domain.Digest("sha256:" + strings.Repeat("1", 64)),
		}},
	}
}

func recordCheckFailure(t *testing.T, st *Store, failure domain.RequiredCheckFailure) domain.Digest {
	t.Helper()
	ctx := context.Background()
	var digest domain.Digest
	if err := st.Write(ctx, func(tx *WriteTx) (err error) {
		digest, err = tx.RecordRequiredCheckFailure(ctx, failure)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return digest
}

func checkFailureAuthority(
	runID domain.RunID, predecessor domain.ItemID, prior domain.ReviewRecord, digest domain.Digest,
) domain.PublicationSuccessor {
	return domain.PublicationSuccessor{
		Version: domain.PublicationCheckFailureVersion, Origin: domain.PublicationSuccessorCheckFailure,
		RunID: runID, PredecessorItemID: predecessor,
		PriorReviewInvocationID: prior.InvocationID, ReviewRound: prior.Round + 1,
		Reentry:            &domain.PublicationSuccessorReentry{BaseSHA: prior.BaseSHA, HeadSHA: prior.HeadSHA},
		CheckFailureDigest: digest,
	}
}

func seedCheckFailure(t *testing.T, opts checkFailureOptions) checkFailureFixture {
	t.Helper()
	f := checkFailureFixture{externalReviewFixture: seedExternalReview(t, externalReviewOptions{leaveOpen: true})}
	reading := codeFailureReading(reentryHead1)
	if opts.reading != nil {
		opts.reading(&reading)
	}
	putRequiredCheck(t, f.st, f.item.ID, reading)
	if !opts.leaveOpen {
		supersedeReadyItem(t, f.st, f.item.ID)
	}
	pull := openPullFactOn(f.binding, reentryHead1, 10)
	if opts.pull != nil {
		opts.pull(&pull)
	}
	appendPullFact(t, f.st, pull)
	f.failure = checkFailureRecord(f.run.ID, f.binding, reentryHead1, 5)
	digest := recordCheckFailure(t, f.st, f.failure)
	prior := f.priorReview(t)
	f.authority = checkFailureAuthority(f.run.ID, f.item.ID, prior, digest)
	return f
}

// priorReview reads the review the external review fixture's authority
// follows: round 1, of the published head.
func (f checkFailureFixture) priorReview(t *testing.T) domain.ReviewRecord {
	t.Helper()
	ctx := context.Background()
	var review domain.ReviewRecord
	if err := f.st.Read(ctx, func(tx *ReadTx) (err error) {
		review, err = tx.GetReviewRecord(ctx, f.authority.PriorReviewInvocationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return review
}

// TestCheckFailureAuthorityAccepted: a code failure of the required check on
// the head Freeside published seals a commandless cycle on that same base and
// head. The pull request still carries that head, so it is the head the
// cycle's push leases against, and the authority keeps reading after that
// push moves the latest recorded head.
func TestCheckFailureAuthorityAccepted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedCheckFailure(t, checkFailureOptions{})
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	if err := f.record(t, f.authority); err != nil {
		t.Fatalf("replay: %v", err)
	}
	var target publicationrecord.SuccessorTarget
	if err := f.st.Read(ctx, func(tx *ReadTx) (err error) {
		current, err := tx.CurrentProductionReadyItemID(ctx, f.run.ID)
		if err != nil || current != f.authority.ReadyItemID() {
			t.Fatalf("current ready item = %q, %v; want the cycle's %q", current, err, f.authority.ReadyItemID())
		}
		target, err = tx.PublicationSuccessorTarget(ctx, f.run.ID, f.authority.PublicationID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if target.HeadSHA != reentryHead1 || target.ItemID != f.item.ID || target.PRNumber != f.binding.PRNumber {
		t.Fatalf("check failure target = %#v", target)
	}
	appendPullFact(t, f.st, openPullFactOn(f.binding, reentryHead2, 20))
	if err := f.read(t, f.authority); err != nil {
		t.Fatalf("read after the cycle's push: %v", err)
	}
}

// TestCheckFailureAuthorityRejectsEachMismatch offers a record that
// disagrees with current state on one fact at a time. The gate itself
// refuses each, so the same record is refused on every read.
func TestCheckFailureAuthorityRejectsEachMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	class := func(c domain.CheckFailureClass) func(*domain.RequiredCheck) {
		return func(r *domain.RequiredCheck) { r.FailureClass = &c }
	}
	for _, tc := range []struct {
		name   string
		opts   checkFailureOptions
		mutate func(checkFailureFixture, *domain.PublicationSuccessor)
	}{
		{name: "head that is not published", mutate: func(f checkFailureFixture, s *domain.PublicationSuccessor) {
			s.Reentry.HeadSHA = reentryHead2
			s.CheckFailureDigest = recordCheckFailure(t, f.st, checkFailureRecord(f.run.ID, f.binding, reentryHead2, 6))
		}},
		{name: "record for another head", mutate: func(f checkFailureFixture, s *domain.PublicationSuccessor) {
			s.CheckFailureDigest = recordCheckFailure(t, f.st, checkFailureRecord(f.run.ID, f.binding, reentryHead2, 6))
		}},
		{name: "record for another pull request", mutate: func(f checkFailureFixture, s *domain.PublicationSuccessor) {
			other := f.binding
			other.PRNumber++
			s.CheckFailureDigest = recordCheckFailure(t, f.st, checkFailureRecord(f.run.ID, other, reentryHead1, 6))
		}},
		{name: "record of another run", mutate: func(f checkFailureFixture, s *domain.PublicationSuccessor) {
			s.CheckFailureDigest = recordCheckFailure(t, f.st, checkFailureRecord("run-other", f.binding, reentryHead1, 6))
		}},
		{name: "unknown record", mutate: func(_ checkFailureFixture, s *domain.PublicationSuccessor) {
			s.CheckFailureDigest = domain.Digest("sha256:" + strings.Repeat("0", 64))
		}},
		{name: "infrastructure failure", opts: checkFailureOptions{reading: class(domain.CheckFailureInfrastructure)}},
		{name: "unclassified failure", opts: checkFailureOptions{reading: class(domain.CheckFailureOther)}},
		{name: "passing check", opts: checkFailureOptions{reading: func(r *domain.RequiredCheck) {
			r.State, r.FailureClass, r.FailedJobs = domain.RequiredCheckPassed, nil, nil
		}}},
		{name: "predecessor still open", opts: checkFailureOptions{leaveOpen: true}},
		{name: "merged pull request", opts: checkFailureOptions{pull: func(p *domain.PullMergeFact) {
			p.State, p.Merged, p.MergeCommitSHA = domain.PullRequestClosed, true, strings.Repeat("d", 40)
		}}},
		{name: "closed pull request", opts: checkFailureOptions{pull: func(p *domain.PullMergeFact) {
			p.State = domain.PullRequestClosed
		}}},
		{name: "retargeted pull request", opts: checkFailureOptions{pull: func(p *domain.PullMergeFact) {
			p.BaseRef = "release"
		}}},
		{name: "base the last review did not use", mutate: func(_ checkFailureFixture, s *domain.PublicationSuccessor) {
			s.Reentry.BaseSHA = reentryBase2
		}},
		{name: "round not one above prior", mutate: func(_ checkFailureFixture, s *domain.PublicationSuccessor) {
			s.ReviewRound = 3
		}},
		{name: "unknown prior review", mutate: func(_ checkFailureFixture, s *domain.PublicationSuccessor) {
			s.PriorReviewInvocationID = "review-unknown"
		}},
		{name: "other run", mutate: func(_ checkFailureFixture, s *domain.PublicationSuccessor) { s.RunID = "run-elsewhere" }},
		{name: "unknown predecessor", mutate: func(_ checkFailureFixture, s *domain.PublicationSuccessor) {
			s.PredecessorItemID = "production-ready-reentry-unknown"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedCheckFailure(t, tc.opts)
			forged := f.authority
			reentry := *f.authority.Reentry
			forged.Reentry = &reentry
			if tc.mutate != nil {
				tc.mutate(f, &forged)
			}
			if err := forged.Validate(); err != nil {
				t.Fatalf("mutation is not a store-gate case: %v", err)
			}
			err := f.st.Read(ctx, func(tx *ReadTx) error { return tx.validatePublicationSuccessor(ctx, forged) })
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("gate error = %v, want ErrParentKeyMismatch", err)
			}
			if err := f.record(t, forged); err == nil {
				t.Fatal("forged authority sealed")
			}
		})
	}
	t.Run("predecessor invalidated", func(t *testing.T) {
		// An invalidated item re-enters through its own origin, which
		// re-checks the coordinate that moved; this origin assumes none did.
		f := seedCheckFailure(t, checkFailureOptions{leaveOpen: true})
		invalidateReadyItem(t, f.st, f.item.ID,
			reentryFact(domain.ReadinessInvalidationBaseAdvanced, reentryBase1, reentryBase2))
		if err := f.gate(f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("gate error = %v, want ErrParentKeyMismatch", err)
		}
	})
}

// TestCheckFailureRefusedOnAForeignHead: the cycle replaces the published
// head, so it is refused while that head is one someone else pushed, which a
// head_changed re-entry re-earned readiness on. A base advance over
// Freeside's own head stays admissible.
func TestCheckFailureRefusedOnAForeignHead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		reason  domain.ReadinessInvalidationReason
		foreign bool
	}{
		{"head someone else pushed", domain.ReadinessInvalidationHeadChanged, true},
		{"own head after a base advance", domain.ReadinessInvalidationBaseAdvanced, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := seedReentry(t, tc.reason, reentryOptions{})
			if err := r.record(t, r.authority); err != nil {
				t.Fatal(err)
			}
			item, binding := r.reenteredItemAndBinding(t, r.authority, r.binding)
			r.putItem(t, item)
			if err := r.recordBinding(binding); err != nil {
				t.Fatal(err)
			}
			base, head := r.authority.Reentry.BaseSHA, r.authority.Reentry.HeadSHA
			putRequiredCheck(t, r.st, item.ID, codeFailureReading(head))
			supersedeReadyItem(t, r.st, item.ID)
			appendPullFact(t, r.st, openPullFactOn(binding, head, 10))
			prior := putReentryReview(t, r.st, r.run.ID, 2, base, head)
			digest := recordCheckFailure(t, r.st, checkFailureRecord(r.run.ID, binding, head, 5))
			err := r.record(t, checkFailureAuthority(r.run.ID, item.ID, prior, digest))
			if tc.foreign && !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("check failure on a foreign head: %v, want ErrParentKeyMismatch", err)
			}
			if !tc.foreign && err != nil {
				t.Fatalf("check failure on Freeside's own head: %v", err)
			}
		})
	}
}

// TestCheckFailureSealOnlyChecks: what holds only when a cycle starts. The
// gate alone admits each record below, so each refusal is the seal's.
func TestCheckFailureSealOnlyChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("pull request moved since", func(t *testing.T) {
		f := seedCheckFailure(t, checkFailureOptions{})
		appendPullFact(t, f.st, openPullFactOn(f.binding, reentryHead2, 20))
		if err := f.gate(f.authority); err != nil {
			t.Fatal(err)
		}
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("seal on a head the pull request left: %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("record already answered", func(t *testing.T) {
		f := seedCheckFailure(t, checkFailureOptions{})
		if err := f.record(t, f.authority); err != nil {
			t.Fatal(err)
		}
		// The sealed cycle is no longer the chain's current item either, so
		// the seal check is called directly to see which refusal comes first.
		err := f.st.Read(ctx, func(tx *ReadTx) error { return tx.requireCurrentCheckFailure(ctx, f.authority) })
		if !errors.Is(err, domain.ErrParentKeyMismatch) || !strings.Contains(err.Error(), "already started a cycle") {
			t.Fatalf("reused failure record: %v, want the already-answered refusal", err)
		}
	})
}

// publishCheckFailureCycle seals f's authority and publishes its remediated
// head on the pull request, as the cycle does once it re-earns readiness,
// and returns the open ready item and binding it earned.
func publishCheckFailureCycle(t *testing.T, f checkFailureFixture, head string) (domain.AttentionItem, domain.ReadyItemPRBinding) {
	t.Helper()
	ctx := context.Background()
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	var target publicationrecord.SuccessorTarget
	if err := f.st.Read(ctx, func(tx *ReadTx) (err error) {
		target, err = tx.PublicationSuccessorTarget(ctx, f.run.ID, f.authority.PublicationID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	invocationID, attemptID := domain.InvocationID("inv-check-failure-remediation"), domain.AttemptID("attempt-check-failure-remediation")
	run := f.run
	stage := run.Stages[0]
	stage.Attempts = append(append([]domain.Attempt(nil), stage.Attempts...),
		domain.Attempt{ID: attemptID, StageID: stage.ID, Number: 2, InvocationID: invocationID})
	run.Stages = []domain.Stage{stage}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: run.ID, StageID: stage.ID, AttemptID: attemptID,
		Backend: f.admission.Backend, Capabilities: f.admission.Capabilities,
		OperatingMode: f.admission.OperatingMode, CredentialMode: f.admission.CredentialMode,
		EgressProfile: f.admission.EgressProfile, ImageRef: f.admission.ImageRef,
		SpecDigest: f.admission.SpecDigest, PolicyDigest: f.admission.PolicyDigest, InputDigest: f.admission.InputDigest,
		Base: f.admission.Base, Workspace: f.admission.Workspace, AdmittedAt: reentryAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocationID, AdmissionID: admission.ID, ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: head,
		ManifestDigest: "sha256:manifest", RecordedAt: reentryAt.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.Digest("sha256:" + strings.Repeat("e", 64))
	intent, err := json.Marshal(readyPublicationIntent{
		FormatVersion: publicationrecord.IntentFormatSuccessor, Identity: identity,
		InvocationID: f.authority.PublicationID(), Branch: target.Branch,
		Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef, SourceHeadSHA: head,
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		ProducingInvocationID: invocationID, ReservationRunID: run.ID, Successor: &target,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := json.Marshal(readyPublicationOutcome{
		Identity: identity, Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef, HeadSHA: head,
		Branch: target.Branch, PRNumber: target.PRNumber, EvidenceEligible: true, Successor: &target,
	})
	if err != nil {
		t.Fatal(err)
	}
	remediated := f.authority
	remediated.Reentry = &domain.PublicationSuccessorReentry{BaseSHA: f.authority.Reentry.BaseSHA, HeadSHA: head}
	item, binding := f.reenteredItemAndBinding(t, remediated, f.binding)
	binding.ProducingInvocationID, binding.PublicationIdentity = invocationID, identity
	f.putItem(t, item)
	intentKey := "publish/" + string(f.authority.PublicationID()) + "/" + readyPublicationIntentKind
	if err := f.st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		if err := tx.RecordExecutionExport(ctx, export); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, readyPublicationIntentKind, intent); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		_, _, err := tx.RecordInbox(ctx, "publish.outcome/"+string(identity), "publish.outcome", outcome)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.recordBinding(binding); err != nil {
		t.Fatalf("remediated check failure binding: %v", err)
	}
	return item, binding
}

// secondCheckFailure publishes f's cycle on reentryHead2, fails that head's
// check on code too, and returns the authority a second cycle would seal.
func secondCheckFailure(t *testing.T, f checkFailureFixture) domain.PublicationSuccessor {
	t.Helper()
	item, binding := publishCheckFailureCycle(t, f, reentryHead2)
	putRequiredCheck(t, f.st, item.ID, codeFailureReading(reentryHead2))
	supersedeReadyItem(t, f.st, item.ID)
	appendPullFact(t, f.st, openPullFactOn(binding, reentryHead2, 20))
	prior := putReentryReview(t, f.st, f.run.ID, f.authority.ReviewRound+1, reentryBase1, reentryHead2)
	digest := recordCheckFailure(t, f.st, checkFailureRecord(f.run.ID, binding, reentryHead2, 6))
	return checkFailureAuthority(f.run.ID, item.ID, prior, digest)
}

// TestCheckFailureSecondCycle: a cycle that published a remediated head whose
// check also failed starts the next cycle from that head, and the record it
// answers is the new head's. An earlier cycle that ended on a person leaves
// that person's item open; no new cycle starts over it.
func TestCheckFailureSecondCycle(t *testing.T) {
	t.Parallel()
	t.Run("after a published cycle", func(t *testing.T) {
		f := seedCheckFailure(t, checkFailureOptions{})
		next := secondCheckFailure(t, f)
		if err := f.record(t, next); err != nil {
			t.Fatalf("second cycle: %v", err)
		}
	})
	t.Run("over an open ending", func(t *testing.T) {
		f := seedCheckFailure(t, checkFailureOptions{})
		next := secondCheckFailure(t, f)
		putHeldItem(t, f.st, f.run, f.authority.BlockedItemID(), reentryHead2, nil)
		if err := f.gate(next); err != nil {
			t.Fatal(err)
		}
		if err := f.record(t, next); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("check failure over an open ending: %v, want ErrParentKeyMismatch", err)
		}
	})
}

// TestHandoffConflictOnACheckFailureHead: a check_failure cycle hands its
// head off under its own in-place key before it remediates. A conflict there
// seals a handoff conflict authority over that check, and the remediated
// head's push leases against the published head.
func TestHandoffConflictOnACheckFailureHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedCheckFailure(t, checkFailureOptions{})
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	check := domain.HandoffCheck{
		RunID: f.run.ID, PublicationID: f.authority.InPlaceHandoffID(),
		HeadSHA: reentryHead1, EvidenceBaseSHA: reentryBase1, TipSHA: reentryBase3,
		WorkflowAuditID: recordHandoffAudit(t, f.st, f.admission.Base.Repo, f.admission.Base.BaseRef, reentryBase3),
	}
	if err := f.st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.SealHandoffTip(ctx, check, handoffConflictAt)
	}); err != nil {
		t.Fatal(err)
	}
	prior := putReentryReview(t, f.st, f.run.ID, 2, reentryBase1, reentryHead1)
	conflict := domain.PublicationSuccessor{
		Version: domain.PublicationHandoffConflictVersion, Origin: domain.PublicationSuccessorHandoffConflict,
		RunID: f.run.ID, PredecessorItemID: f.authority.ReadyItemID(),
		PriorReviewInvocationID: prior.InvocationID, ReviewRound: 3,
		Reentry:              &domain.PublicationSuccessorReentry{BaseSHA: reentryBase3, HeadSHA: reentryHead1},
		HandoffConflictCause: domain.HandoffConflictTextual,
	}
	if err := f.record(t, conflict); err != nil {
		t.Fatalf("conflict over the in-place handoff: %v", err)
	}
	var target publicationrecord.SuccessorTarget
	if err := f.st.Read(ctx, func(tx *ReadTx) (err error) {
		target, err = tx.PublicationSuccessorTarget(ctx, f.run.ID, conflict.PublicationID())
		return err
	}); err != nil || target.HeadSHA != reentryHead1 || target.ItemID != f.item.ID {
		t.Fatalf("conflict target = %#v, %v; want head %s on %s", target, err, reentryHead1, f.item.ID)
	}
}

// TestSuccessorProducerAdmitsTheCheckFailureRemediation: the cycle's first
// remediation answers the failed check after a clean review of the cycle's
// head, so it names no finding and no adjudication. A finding in that review
// or in the request is refused: such a round takes the ordinary path.
func TestSuccessorProducerAdmitsTheCheckFailureRemediation(t *testing.T) {
	t.Parallel()
	seed := func(t *testing.T, withFinding bool) (checkFailureFixture, domain.ReviewRecord) {
		t.Helper()
		f := seedCheckFailure(t, checkFailureOptions{})
		if err := f.record(t, f.authority); err != nil {
			t.Fatal(err)
		}
		var findings []domain.Finding
		if withFinding {
			findings = append(findings, cycleInternalFinding("finding-cycle", f.run.ID))
		}
		return f, putCycleReview(t, f.st, f.run.ID, f.authority.ReviewRound, reentryBase1, reentryHead1, findings...)
	}
	t.Run("clean review", func(t *testing.T) {
		t.Parallel()
		f, review := seed(t, false)
		producer := seedCycleRemediationProducer(t, f.externalReviewFixture, review, domain.FindingAdjudication{}, nil, nil)
		if err := f.authenticateProducer(t, producer.id); err != nil {
			t.Fatalf("check failure remediation = %v", err)
		}
	})
	t.Run("review with a finding", func(t *testing.T) {
		t.Parallel()
		f, review := seed(t, true)
		producer := seedCycleRemediationProducer(t, f.externalReviewFixture, review, domain.FindingAdjudication{}, nil, nil)
		if err := f.authenticateProducer(t, producer.id); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("remediation after a review with findings = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("request naming a finding", func(t *testing.T) {
		t.Parallel()
		f, review := seed(t, false)
		producer := seedCycleRemediationProducer(t, f.externalReviewFixture, review, domain.FindingAdjudication{},
			[]domain.FindingID{"finding-unrouted"}, nil)
		if err := f.authenticateProducer(t, producer.id); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("remediation naming a finding = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("request at another base", func(t *testing.T) {
		t.Parallel()
		f, review := seed(t, false)
		producer := seedCycleRemediationProducer(t, f.externalReviewFixture, review, domain.FindingAdjudication{}, nil,
			func(r *domain.RemediationInvocationIntent) { r.BaseSHA = reentryBase2 })
		if err := f.authenticateProducer(t, producer.id); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("remediation at another base = %v, want ErrParentKeyMismatch", err)
		}
	})
}
