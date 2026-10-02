package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const heldTestRun = domain.RunID("run-cap")

// heldRun seeds a run whose publication is held after its pull request was
// opened: an open publish_blocked item that carries the pull request and, when
// bound is set, the held binding the engine records next to it. The run has no
// ready item. boundIssue declares a bound-issue work unit for the run.
func heldRun(t *testing.T, st *store.Store, bound bool, boundIssue *int) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	runID := heldTestRun
	seedCaptureRun(t, st, runID)
	authority := seedReadyBindingAuthority(t, st, runID, "cafed00d")
	if boundIssue != nil {
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
			BoundIssue:          boundIssue,
		}, runID, "project-1", time.Date(2026, 2, 3, 3, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
				return err
			}
			return tx.RecordWorkUnitPRBinding(ctx, domain.WorkUnitPRBinding{
				UnitID: declaration.ID, Repo: "owner/repo", RepositoryID: 424242,
				PRNumber: 450, BaseRef: "main", HeadSHA: "cafed00d",
				RecordedAt: activeResourceTestTime.Add(-time.Minute),
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-held", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
		Reason: "publication held", RequestedDecision: []domain.Action{
			domain.ActionInspectTrustFailure, domain.ActionOpenPR,
		},
		PRHeadSHA: "cafed00d", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	if !bound {
		return item
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordHeldItemPRBinding(ctx, domain.HeldItemPRBinding{
			ItemID: item.ID, RunID: runID,
			ProducingInvocationID: authority.invocationID, PublicationIdentity: authority.identity,
			PublicationInvocationID: authority.publicationInvocationID,
			Repo:                    "owner/repo", RepositoryID: 424242,
			PRNumber: 450, BaseRef: "main", HeadSHA: item.PRHeadSHA,
			RecordedAt: activeResourceTestTime.Add(-time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func assertHoldUntouched(t *testing.T, st *store.Store, item domain.AttentionItem) {
	t.Helper()
	got := readActiveItem(t, st, item.ID)
	if got.Status != domain.StatusOpen || got.ItemVersion != item.ItemVersion {
		t.Fatalf("hold changed: status %s version %d", got.Status, got.ItemVersion)
	}
}

func TestActiveResourceRecordsHeldPullRequestEndAndLeavesHoldOpen(t *testing.T) {
	for _, tc := range []struct {
		name      string
		merged    bool
		wantPolls int
	}{
		// A merge is final, so one recorded merge ends the polling.
		{name: "merged", merged: true, wantPolls: 1},
		// A closed pull request can reopen, so it stays watched.
		{name: "closed unmerged", merged: false, wantPolls: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := schedTestStore(t)
			item := heldRun(t, st, true, nil)
			pullCalls := 0
			reconciler := activeResourceReconciler{
				store: st,
				pull: func(context.Context, string, int) (publish.PullObservation, error) {
					pullCalls++
					return exactPull("closed", tc.merged), nil
				},
				now: func() time.Time { return activeResourceTestTime },
			}
			for pass := range 3 {
				result, err := reconciler.Reconcile(ctx)
				if err != nil || len(result.failures) != 0 {
					t.Fatalf("pass %d Reconcile = %v, %v", pass, result.failures, err)
				}
				if result.operatorActive {
					t.Fatalf("pass %d: a held run reported operator activity", pass)
				}
			}
			assertHoldUntouched(t, st, item)
			facts := activePullFacts(t, st, 424242, 450)
			if len(facts) != 1 || facts[0].State != domain.PullRequestClosed || facts[0].Merged != tc.merged {
				t.Fatalf("facts = %+v", facts)
			}
			if pullCalls != tc.wantPolls {
				t.Fatalf("pull calls = %d, want %d", pullCalls, tc.wantPolls)
			}
			assertNoActiveCompletion(t, st, item)
		})
	}
}

func TestActiveResourceCompletesDeclaredHeldRun(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	boundIssue := 443
	item := heldRun(t, st, true, &boundIssue)
	pullCalls := 0
	issueClosed := false
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			pullCalls++
			return exactPull("closed", true), nil
		},
		issue: func(context.Context, string, int) (publish.IssueObservation, error) {
			if !issueClosed {
				return publish.IssueObservation{Number: 443, State: "open"}, nil
			}
			return publish.IssueObservation{
				Number: 443, State: "closed", ClosedByCommitSHA: "deadbeef",
			}, nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	pulls, issues, completion := readCaptureState(t, st)
	if len(pulls) != 1 || !pulls[0].Merged || len(issues) != 1 ||
		issues[0].State != domain.IssueOpen || completion != nil {
		t.Fatalf("pending capture = pulls %v issues %v completion %v", pulls, issues, completion)
	}

	issueClosed = true
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("issue-closure Reconcile = %v, %v", failures, err)
	}
	pulls, issues, completion = readCaptureState(t, st)
	if len(pulls) != 1 || len(issues) != 2 || completion == nil {
		t.Fatalf("capture = pulls %v issues %v completion %v", pulls, issues, completion)
	}
	assertHoldUntouched(t, st, item)

	// Completion is recorded, so nothing is left to observe.
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("settled Reconcile = %v, %v", failures, err)
	}
	if pullCalls != 4 { // two passes, each with a repository-identity recheck
		t.Fatalf("pull calls = %d, want 4", pullCalls)
	}
}

func TestActiveResourceSkipsHoldWithoutBinding(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := heldRun(t, st, false, nil)
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			t.Fatal("polled a hold whose binding is not recorded")
			return publish.PullObservation{}, nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	assertHoldUntouched(t, st, item)
}

func TestActiveResourceObservesHeldRunWithReadyItemOnce(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	held := heldRun(t, st, true, nil)
	runID := heldTestRun
	ready, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-ready-held", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "published and verified", RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionDismiss,
		},
		PRHeadSHA: "cafed00d", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, ready)
	}); err != nil {
		t.Fatal(err)
	}
	var heldBinding domain.HeldItemPRBinding
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		heldBinding, err = tx.GetHeldItemPRBinding(ctx, held.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	readyBinding := domain.ReadyItemPRBinding(heldBinding)
	readyBinding.ItemID = ready.ID
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordReadyItemPRBinding(ctx, readyBinding)
	}); err != nil {
		t.Fatal(err)
	}
	pullCalls := 0
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			pullCalls++
			return exactPull("open", false), nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	if pullCalls != 1 {
		t.Fatalf("pull calls = %d, want the ready item's single observation", pullCalls)
	}
	assertHoldUntouched(t, st, held)
}

// The store proves a held binding whenever its item is read, so a binding it
// can no longer prove fails the pass at the item listing, before any hold is
// observed. Nothing is polled through it.
func TestActiveResourceRefusesUnprovableHeldBinding(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, dbPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	t.Cleanup(func() { _ = st.Close() })
	heldRun(t, st, true, nil)

	// The binding row survives, but the outcome it is proven against now
	// names another pull request.
	identity := heldTestIdentity(t)
	forged, err := (publish.Outcome{
		Identity: identity.Digest(), Repo: "owner/repo", BaseRef: "main", HeadSHA: "cafed00d",
		Branch: identity.BranchName(), PRNumber: 451, EvidenceEligible: true,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, updateErr := raw.ExecContext(ctx,
		`UPDATE inbox SET payload = ? WHERE kind = ?`, forged, publish.IntentKindOutcome)
	if closeErr := raw.Close(); updateErr != nil || closeErr != nil {
		t.Fatal(errors.Join(updateErr, closeErr))
	}

	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			t.Fatal("polled a pull request through an unprovable binding")
			return publish.PullObservation{}, nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if _, err := reconciler.Reconcile(ctx); err == nil {
		t.Fatal("an unprovable held binding went unreported")
	}
}

func heldTestIdentity(t *testing.T) publish.Identity {
	t.Helper()
	identity, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo: "owner/repo", BaseRef: "main", SourceHeadSHA: "cafed00d",
		ArtifactDigests: []domain.Digest{"sha256:ready-artifact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
