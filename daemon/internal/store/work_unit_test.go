package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// captureFixture builds the internally consistent capture set the domain
// tests also use, bound to the admission fixture's run-1.
func captureFixture(t *testing.T) (domain.WorkUnitDeclaration, domain.WorkUnitPRBinding, domain.PullMergeFact, domain.IssueStateFact) {
	t.Helper()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	boundIssue := 443
	decl, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
		BoundIssue:          &boundIssue,
		DependsOnIssues:     []int{440, 442},
		DeclaredPaths:       []string{"daemon/", "devlog/"},
		ContractSerialized:  true,
	}, "run-1", "proj-1", ts)
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.WorkUnitPRBinding{
		UnitID: decl.ID, Repo: "owner/repo", RepositoryID: 424242,
		PRNumber: 450, BaseRef: "refs/heads/main", HeadSHA: "cafebabe",
		RecordedAt: ts.Add(time.Hour),
	}
	pull := domain.PullMergeFact{
		Repo: "owner/repo", RepositoryID: 424242, PRNumber: 450,
		State: domain.PullRequestClosed, Merged: true,
		MergeCommitSHA: "deadbeef", BaseRef: "refs/heads/main", HeadSHA: "cafebabe",
		ObservedAt: ts.Add(2 * time.Hour),
	}
	issue := domain.IssueStateFact{
		Repo: "owner/repo", RepositoryID: 424242, IssueNumber: 443,
		State: domain.IssueClosed, ClosedByCommitSHA: "deadbeef",
		ObservedAt: ts.Add(2 * time.Hour),
	}
	return decl, binding, pull, issue
}

func openCaptureStore(t *testing.T) *store.Store {
	t.Helper()
	// The declaration read re-gate re-derives the declared scope from the
	// run's resolved policy; the capture fixture declares daemon/ and
	// devlog/, so the seeded policy must state exactly that boundary, and
	// the fixture run must carry its digest (PutResolvedPolicy binds the
	// pair).
	policy, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/,devlog/",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("cd", 32)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f := newAdmissionFixture(t, nil)
	f.run.PolicyDigest = policy.Digest
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	if err := s.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.PutResolvedPolicy(context.Background(), policy)
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadyItemPRBindingAnchorsToReadyItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openCaptureStore(t)
	runID := domain.RunID("run-1")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-ready-1", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "published", RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA: "cafebabe", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	binding := domain.ReadyItemPRBinding{
		ItemID: item.ID, RunID: runID, ProducingInvocationID: "inv-1",
		PublicationInvocationID: "publish-production-run-1",
		Repo:                    "owner/repo", RepositoryID: 424242,
		PRNumber: 450, BaseRef: "refs/heads/main", HeadSHA: item.PRHeadSHA,
		RecordedAt: time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC),
	}
	var run domain.Run
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fixture := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.SpecDigest = run.SpecDigest
		in.PolicyDigest = run.PolicyDigest
	})
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: fixture.admission.InvocationID, AdmissionID: fixture.admission.ID,
		ObservedBaseSHA: fixture.admission.Base.BaseSHA, HeadSHA: binding.HeadSHA,
		ManifestDigest: "sha256:ready-manifest", RecordedAt: binding.RecordedAt.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo: binding.Repo, BaseRef: binding.BaseRef, SourceHeadSHA: binding.HeadSHA,
		ArtifactDigests: []domain.Digest{"sha256:ready-artifact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding.PublicationIdentity = identity.Digest()
	outcomePayload, err := (publish.Outcome{
		Identity: identity.Digest(), Repo: binding.Repo, BaseRef: binding.BaseRef,
		HeadSHA: binding.HeadSHA, Branch: identity.BranchName(), PRNumber: binding.PRNumber,
		EvidenceEligible: true,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	intentPayload, err := (publish.Intent{
		FormatVersion: publish.IntentFormatCurrent,
		Identity:      identity.Digest(), InvocationID: binding.PublicationInvocationID,
		Branch: identity.BranchName(),
		Repo:   binding.Repo, BaseRef: binding.BaseRef, SourceHeadSHA: binding.HeadSHA,
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("ef", 32)),
		ProducingInvocationID: binding.ProducingInvocationID, ReservationRunID: binding.RunID,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	intentKey, err := publish.IntentKey(binding.PublicationInvocationID, publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordExecutionAdmission(ctx, fixture.admission); err != nil {
			return err
		}
		if err := tx.RecordExecutionExport(ctx, export); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, publish.IntentKindPublication, intentPayload); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		_, _, err := tx.RecordInbox(ctx, publish.OutcomeKey(identity), publish.IntentKindOutcome, outcomePayload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordReadyItemPRBinding(ctx, binding)
	}); err != nil {
		t.Fatal(err)
	}
	var got domain.ReadyItemPRBinding
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetReadyItemPRBinding(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got != binding {
		t.Fatalf("binding = %+v, want %+v", got, binding)
	}

	foreign := binding
	foreign.ItemID = "item-ready-foreign"
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordReadyItemPRBinding(ctx, foreign)
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign binding error = %v, want ErrNotFound", err)
	}
	mismatched := binding
	mismatched.HeadSHA = "different"
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordReadyItemPRBinding(ctx, mismatched)
	}); err == nil {
		t.Fatal("binding with a foreign head was accepted")
	}
}

func writeInternal(t *testing.T, s *store.Store, fn func(tx *store.InternalTx) error) error {
	t.Helper()
	return s.WriteInternal(context.Background(), fn)
}

// TestWorkUnitDeclarationRoundTrip is the write-once contract: the record
// comes back as it went in, an identical replay converges, and a divergent
// re-declaration is an immutable conflict.
func TestWorkUnitDeclarationRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openCaptureStore(t)
	decl, _, _, _ := captureFixture(t)

	put := func(d domain.WorkUnitDeclaration) error {
		return writeInternal(t, s, func(tx *store.InternalTx) error {
			return tx.RecordWorkUnitDeclaration(ctx, d)
		})
	}
	if err := put(decl); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := put(decl); err != nil {
		t.Fatalf("identical replay must converge: %v", err)
	}
	divergent := decl
	divergent.ContractSerialized = false
	if err := put(divergent); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("divergent replay = %v, want %v", err, store.ErrImmutableConflict)
	}

	var got domain.WorkUnitDeclaration
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetWorkUnitDeclarationByRun(ctx, "run-1")
		return err
	}); err != nil {
		t.Fatalf("get by run: %v", err)
	}
	if got.ID != decl.ID || got.BoundIssue == nil || *got.BoundIssue != 443 ||
		len(got.DependsOnIssues) != 2 || len(got.DeclaredPaths) != 2 || !got.ContractSerialized {
		t.Fatalf("round trip = %+v, want %+v", got, decl)
	}

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetWorkUnitDeclarationByRun(ctx, "run-undeclared")
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("undeclared run = %v, want %v", err, store.ErrNotFound)
	}
}

// TestWorkUnitDeclarationRequiresRun: the declaration binds a persisted run
// (FK), so capture cannot invent a unit for a run that was never submitted.
func TestWorkUnitDeclarationRequiresRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openCaptureStore(t)
	decl, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged,
	}, "run-never-submitted", "proj-1", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitDeclaration(ctx, decl)
	}); err == nil {
		t.Fatal("put for a nonexistent run succeeded, want FK failure")
	}
}

// TestWorkUnitPRBindingRoundTrip: write-once, requires its declaration, and
// reconstructs exactly.
func TestWorkUnitPRBindingRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openCaptureStore(t)
	decl, binding, _, _ := captureFixture(t)

	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitPRBinding(ctx, binding)
	}); err == nil {
		t.Fatal("binding without a declaration succeeded, want FK failure")
	}

	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		if err := tx.RecordWorkUnitDeclaration(ctx, decl); err != nil {
			return err
		}
		return tx.RecordWorkUnitPRBinding(ctx, binding)
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitPRBinding(ctx, binding)
	}); err != nil {
		t.Fatalf("identical replay must converge: %v", err)
	}
	divergent := binding
	divergent.PRNumber = 451
	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitPRBinding(ctx, divergent)
	}); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("divergent replay = %v, want %v", err, store.ErrImmutableConflict)
	}

	var got domain.WorkUnitPRBinding
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetWorkUnitPRBinding(ctx, decl.ID)
		return err
	}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != binding {
		t.Fatalf("round trip = %+v, want %+v", got, binding)
	}
}

// TestPullMergeFactAppendOnMaterialChange: the first observation appends, an
// instant-only repeat does not, and a state change appends again, so the
// stored history is the resource's state timeline.
func TestPullMergeFactAppendOnMaterialChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openCaptureStore(t)
	_, _, merged, _ := captureFixture(t)

	open := merged
	open.State = domain.PullRequestOpen
	open.Merged = false
	open.MergeCommitSHA = ""
	open.ObservedAt = merged.ObservedAt.Add(-time.Hour)

	append_ := func(f domain.PullMergeFact) bool {
		var inserted bool
		if err := writeInternal(t, s, func(tx *store.InternalTx) error {
			var err error
			inserted, err = tx.AppendPullMergeFact(ctx, f)
			return err
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
		return inserted
	}

	if !append_(open) {
		t.Fatal("first observation must append")
	}
	repeat := open
	repeat.ObservedAt = open.ObservedAt.Add(15 * time.Minute)
	if append_(repeat) {
		t.Fatal("an instant-only repeat must not append")
	}
	if !append_(merged) {
		t.Fatal("a merged transition must append")
	}

	var timeline []domain.PullMergeFact
	var latest domain.PullMergeFact
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if timeline, err = tx.ListPullMergeFacts(ctx, 424242, 450); err != nil {
			return err
		}
		latest, err = tx.LatestPullMergeFact(ctx, 424242, 450)
		return err
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(timeline) != 2 {
		t.Fatalf("timeline length = %d, want 2", len(timeline))
	}
	if !latest.Merged || latest.MergeCommitSHA != "deadbeef" {
		t.Fatalf("latest = %+v, want the merged fact", latest)
	}

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.LatestPullMergeFact(ctx, 424242, 9999)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unobserved resource = %v, want %v", err, store.ErrNotFound)
	}
}

// TestIssueStateFactAppendOnMaterialChange mirrors the pull-fact rule for
// issues.
func TestIssueStateFactAppendOnMaterialChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openCaptureStore(t)
	_, _, _, closed := captureFixture(t)

	open := closed
	open.State = domain.IssueOpen
	open.ClosedByCommitSHA = ""
	open.ObservedAt = closed.ObservedAt.Add(-time.Hour)

	append_ := func(f domain.IssueStateFact) bool {
		var inserted bool
		if err := writeInternal(t, s, func(tx *store.InternalTx) error {
			var err error
			inserted, err = tx.AppendIssueStateFact(ctx, f)
			return err
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
		return inserted
	}

	if !append_(open) {
		t.Fatal("first observation must append")
	}
	repeat := open
	repeat.ObservedAt = open.ObservedAt.Add(15 * time.Minute)
	if append_(repeat) {
		t.Fatal("an instant-only repeat must not append")
	}
	if !append_(closed) {
		t.Fatal("a closure must append")
	}

	var timeline []domain.IssueStateFact
	var latest domain.IssueStateFact
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if timeline, err = tx.ListIssueStateFacts(ctx, 424242, 443); err != nil {
			return err
		}
		latest, err = tx.LatestIssueStateFact(ctx, 424242, 443)
		return err
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(timeline) != 2 {
		t.Fatalf("timeline length = %d, want 2", len(timeline))
	}
	if latest.State != domain.IssueClosed || latest.ClosedByCommitSHA != "deadbeef" {
		t.Fatalf("latest = %+v, want the closed fact", latest)
	}
}

// TestWorkUnitCompletionRoundTrip: write-once via the domain evaluator's
// output over the full recorded evidence chain; a replay of the same facts
// converges, a disagreeing completion claim conflicts, and the read re-gate
// refuses a done bit the store's own evidence does not derive.
func TestWorkUnitCompletionRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openCaptureStore(t)
	decl, binding, pull, issue := captureFixture(t)
	completion, ok := domain.EvaluateWorkUnitCompletion(decl, binding, pull, &issue)
	if !ok {
		t.Fatal("fixture did not evaluate as completed")
	}

	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitCompletion(ctx, completion)
	}); err == nil {
		t.Fatal("completion without a declaration succeeded, want FK failure")
	}

	// The evidence chain the read re-gate re-derives from: declaration,
	// binding, and the satisfying observations, as the capture pass records
	// them.
	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		if err := tx.RecordWorkUnitDeclaration(ctx, decl); err != nil {
			return err
		}
		if err := tx.RecordWorkUnitPRBinding(ctx, binding); err != nil {
			return err
		}
		if _, err := tx.AppendPullMergeFact(ctx, pull); err != nil {
			return err
		}
		if _, err := tx.AppendIssueStateFact(ctx, issue); err != nil {
			return err
		}
		return tx.RecordWorkUnitCompletion(ctx, completion)
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitCompletion(ctx, completion)
	}); err != nil {
		t.Fatalf("identical replay must converge: %v", err)
	}
	divergent := completion
	divergent.MergeCommitSHA = "0ther5ha"
	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitCompletion(ctx, divergent)
	}); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("divergent replay = %v, want %v", err, store.ErrImmutableConflict)
	}

	var got domain.WorkUnitCompletion
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetWorkUnitCompletion(ctx, decl.ID)
		return err
	}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UnitID != completion.UnitID || got.MergeCommitSHA != completion.MergeCommitSHA ||
		got.BoundIssue == nil || *got.BoundIssue != 443 {
		t.Fatalf("round trip = %+v, want %+v", got, completion)
	}

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetWorkUnitCompletion(ctx, "workunit-run-incomplete")
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("incomplete unit = %v, want %v", err, store.ErrNotFound)
	}
}

// rebaseProofFixture is a proven rebase of captureFixture's bound head onto
// the base putCleanReview records for it.
func rebaseProofFixture(binding domain.WorkUnitPRBinding) domain.WorkUnitRebaseProof {
	return domain.WorkUnitRebaseProof{
		UnitID: binding.UnitID, RepositoryID: binding.RepositoryID, PRNumber: binding.PRNumber,
		ReviewedBaseSHA: "facefeed", ReviewedHeadSHA: binding.HeadSHA, ProvenHeadSHA: "c0ffee00",
		ReviewedTreeSHA: "feedf00d", Proven: true,
		RecordedAt: binding.RecordedAt.Add(3 * time.Hour),
	}
}

// putCleanReview records a clean pass for run-1 over base and head.
func putCleanReview(t *testing.T, s *store.Store, round int, base, head string) {
	t.Helper()
	invocation := domain.InvocationID("review-run-1-" + strings.Repeat("r", round))
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: invocation, RunID: "run-1", Round: round,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, "run-1", invocation),
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   adjInstructionDigest,
		BaseSHA:             base, HeadSHA: head,
		CompletedAt:        time.Date(2026, 1, 2, 3, 4, 5+round, 0, time.UTC),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(context.Background(), record, nil)
	}); err != nil {
		t.Fatalf("put review round %d: %v", round, err)
	}
}

// TestWorkUnitRebaseProofRoundTrip is the write-once contract: a proof needs
// its unit's binding and pull request, an identical replay converges, and a
// divergent verdict for the same head is an immutable conflict.
func TestWorkUnitRebaseProofRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openCaptureStore(t)
	decl, binding, _, _ := captureFixture(t)
	proof := rebaseProofFixture(binding)
	put := func(p domain.WorkUnitRebaseProof) error {
		return writeInternal(t, s, func(tx *store.InternalTx) error {
			return tx.RecordWorkUnitRebaseProof(ctx, p)
		})
	}

	if err := put(proof); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("proof without a binding = %v, want %v", err, store.ErrNotFound)
	}
	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		if err := tx.RecordWorkUnitDeclaration(ctx, decl); err != nil {
			return err
		}
		return tx.RecordWorkUnitPRBinding(ctx, binding)
	}); err != nil {
		t.Fatal(err)
	}
	foreign := proof
	foreign.PRNumber = 451
	if err := put(foreign); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("proof for another pull request = %v, want %v", err, domain.ErrParentKeyMismatch)
	}
	if err := put(proof); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := put(proof); err != nil {
		t.Fatalf("identical replay must converge: %v", err)
	}
	refused := proof
	refused.Proven, refused.ReviewedTreeSHA, refused.Detail = false, "", "tree differs"
	if err := put(refused); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("divergent verdict = %v, want %v", err, store.ErrImmutableConflict)
	}
	other := refused
	other.ProvenHeadSHA = "0badc0de"
	if err := put(other); err != nil {
		t.Fatalf("refusal of another head: %v", err)
	}

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetWorkUnitRebaseProof(ctx, decl.ID, proof.ProvenHeadSHA)
		if err != nil {
			return err
		}
		if got != proof {
			t.Errorf("get = %+v, want %+v", got, proof)
		}
		list, err := tx.ListWorkUnitRebaseProofs(ctx, decl.ID)
		if err != nil {
			return err
		}
		if len(list) != 2 || list[0] != proof || list[1] != other {
			t.Errorf("list = %+v, want [%+v %+v]", list, proof, other)
		}
		_, err = tx.GetWorkUnitRebaseProof(ctx, decl.ID, "feedface")
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unrecorded head = %v, want %v", err, store.ErrNotFound)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestEffectiveWorkUnitPRBindingRegatesRebaseProof pins the read re-gate: a
// proof moves the head completion compares only while it proves the published
// head against the latest review's pair, and a decoded proven bit alone moves
// nothing.
func TestEffectiveWorkUnitPRBindingRegatesRebaseProof(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openCaptureStore(t)
	decl, binding, _, _ := captureFixture(t)
	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		if err := tx.RecordWorkUnitDeclaration(ctx, decl); err != nil {
			return err
		}
		return tx.RecordWorkUnitPRBinding(ctx, binding)
	}); err != nil {
		t.Fatal(err)
	}
	proof := rebaseProofFixture(binding)
	effectiveHead := func() string {
		t.Helper()
		var head string
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			effective, err := tx.EffectiveWorkUnitPRBinding(ctx, decl.ID)
			head = effective.HeadSHA
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return head
	}
	covers := func(effectiveHead, readyHead string) bool {
		t.Helper()
		effective := binding
		effective.HeadSHA = effectiveHead
		var ok bool
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			ok, err = tx.WorkUnitBindingCovers(ctx, effective, readyHead)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// A refusal never moves the head.
	refused := proof
	refused.ProvenHeadSHA = "0badc0de"
	refused.Proven, refused.ReviewedTreeSHA, refused.Detail = false, "", "tree differs"
	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		if err := tx.RecordWorkUnitRebaseProof(ctx, refused); err != nil {
			return err
		}
		return tx.RecordWorkUnitRebaseProof(ctx, proof)
	}); err != nil {
		t.Fatal(err)
	}
	// With no review record the proof has no pair to agree with.
	if got := effectiveHead(); got != binding.HeadSHA {
		t.Fatalf("no review: head = %q, want the published %q", got, binding.HeadSHA)
	}

	putCleanReview(t, s, 1, "facefeed", binding.HeadSHA)
	if got := effectiveHead(); got != proof.ProvenHeadSHA {
		t.Fatalf("matching review: head = %q, want the proven %q", got, proof.ProvenHeadSHA)
	}
	if !covers(proof.ProvenHeadSHA, binding.HeadSHA) {
		t.Fatal("the proven head must cover the ready binding's published head")
	}
	if !covers(binding.HeadSHA, binding.HeadSHA) {
		t.Fatal("equal heads must cover")
	}
	if covers(proof.ProvenHeadSHA, "feedface") {
		t.Fatal("the proven head covered a ready binding at another head")
	}
	if covers("0badc0de", binding.HeadSHA) {
		t.Fatal("a refused head covered the published head")
	}

	// A later review at another base stales the proof.
	putCleanReview(t, s, 2, "beefcafe", binding.HeadSHA)
	if got := effectiveHead(); got != binding.HeadSHA {
		t.Fatalf("later review: head = %q, want the published %q", got, binding.HeadSHA)
	}
	if covers(proof.ProvenHeadSHA, binding.HeadSHA) {
		t.Fatal("a stale proof still covered the published head")
	}
}
