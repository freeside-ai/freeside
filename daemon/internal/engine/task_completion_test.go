package engine

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const completionFixturePR = 127

var completionFixtureAt = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

type completionFixture struct {
	path     string
	store    *store.Store
	run      domain.Run
	dispute  domain.ItemID
	proposal domain.ItemID
	ready    domain.ItemID
}

// newCompletionFixture seeds one task episode in the given lifecycle with an
// open review dispute, effect proposal, and ready item. "restarted" admits a
// second episode after the first completed, and "recompleted" also completes
// it, leaving the first episode's items open as an upgrade would. A completion is
// seeded through real declaration, binding, and merge rows, because the sweep
// reads it through the store's completion re-gate.
func newCompletionFixture(t *testing.T, lifecycle string) completionFixture {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, path, store.Options{})
	runID := domain.RunID("completion-run")
	policy := completionPolicy(t, runID)
	run := domain.Run{ID: runID, ProjectID: "completion-project", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		var err error
		if run, err = tx.GetRun(ctx, run.ID); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		return tx.RecordTaskStart(ctx, run.ID, completionFixtureAt.Add(-time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	if lifecycle == "abandoned" {
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			_, err := tx.AbandonTask(ctx, run.TaskID, completionFixtureAt)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if lifecycle != "active" && lifecycle != "abandoned" {
		seedRunCompletion(t, st, run, policy, completionFixturePR, completionFixtureAt)
	}
	f := completionFixture{path: path, store: st, run: run, dispute: "completion-dispute", proposal: "completion-proposal", ready: "completion-ready"}
	subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID}
	inputs := []domain.AttentionItemInput{{
		ID: f.dispute, ProjectID: run.ProjectID, Subject: subject,
		Type: domain.AttentionReviewDispute, Priority: domain.PriorityHigh, Reason: "Reword the test comment.",
		RequestedDecision: []domain.Action{domain.ActionDiscuss}, ItemVersion: 1, Status: domain.StatusOpen,
		InterruptionClass: domain.InterruptionExceptional,
	}, {
		ID: f.proposal, ProjectID: run.ProjectID, Subject: subject,
		Type: domain.AttentionEffectProposal, Priority: domain.PriorityNormal, Reason: "File a follow-up.",
		RequestedDecision: []domain.Action{domain.ActionApprove}, ItemVersion: 1, Status: domain.StatusOpen,
		InterruptionClass: domain.InterruptionPlannedGate,
	}, {
		ID: f.ready, ProjectID: run.ProjectID, Subject: subject,
		Type: domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal, Reason: "Published.",
		RequestedDecision: []domain.Action{domain.ActionOpenPR}, ItemVersion: 1, Status: domain.StatusOpen,
		PRHeadSHA: "cafebabe", PRReference: &domain.PRReference{Repo: "owner/repo", Number: completionFixturePR},
		InterruptionClass: domain.InterruptionPlannedGate,
	}}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, input := range inputs {
			item, err := domain.NewAttentionItem(input, nil)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if lifecycle == "restarted" || lifecycle == "recompleted" {
		next := run
		next.ID = "completion-next-run"
		nextPolicy := completionPolicy(t, next.ID)
		next.PolicyDigest = nextPolicy.Digest
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			if err := tx.PutRun(ctx, next); err != nil {
				return err
			}
			if err := tx.PutResolvedPolicy(ctx, nextPolicy); err != nil {
				return err
			}
			return tx.AdmitTaskStart(ctx, next.ID, completionFixtureAt.Add(time.Hour), 2)
		}); err != nil {
			t.Fatal(err)
		}
		if lifecycle == "recompleted" {
			seedRunCompletion(t, st, next, nextPolicy, completionFixturePR+1, completionFixtureAt.Add(2*time.Hour))
		}
	}
	return f
}

func completionPolicy(t *testing.T, runID domain.RunID) domain.ResolvedPolicy {
	t.Helper()
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "paths", Value: "daemon/",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("cd", 32)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// seedRunCompletion records the run's work unit as completed by a merged
// pull request, then the task completion fact at the given time.
func seedRunCompletion(t *testing.T, st *store.Store, run domain.Run, policy domain.ResolvedPolicy, pr int, at time.Time) {
	t.Helper()
	ctx := t.Context()
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged,
		DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
	}, run.ID, run.ProjectID, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.WorkUnitPRBinding{
		UnitID: declaration.ID, Repo: "owner/repo", RepositoryID: 424242,
		PRNumber: pr, BaseRef: "refs/heads/main", HeadSHA: "cafebabe",
		RecordedAt: at.Add(-30 * time.Minute),
	}
	pull := domain.PullMergeFact{
		Repo: "owner/repo", RepositoryID: 424242, PRNumber: pr,
		State: domain.PullRequestClosed, Merged: true,
		MergeCommitSHA: "deadbeef", BaseRef: "refs/heads/main", HeadSHA: "cafebabe",
		ObservedAt: at,
	}
	completion, ok := domain.EvaluateWorkUnitCompletion(declaration, binding, pull, nil)
	if !ok {
		t.Fatal("completion fixture did not evaluate as completed")
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		if err := tx.RecordWorkUnitPRBinding(ctx, binding); err != nil {
			return err
		}
		if _, err := tx.AppendPullMergeFact(ctx, pull); err != nil {
			return err
		}
		return tx.RecordWorkUnitCompletion(ctx, completion)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordTaskCompletion(ctx, run.ID, declaration.ID, at)
	}); err != nil {
		t.Fatal(err)
	}
}

func (f completionFixture) items(t *testing.T) map[domain.ItemID]domain.AttentionItem {
	t.Helper()
	items := map[domain.ItemID]domain.AttentionItem{}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		for _, id := range []domain.ItemID{f.dispute, f.proposal, f.ready} {
			item, err := tx.GetAttentionItem(t.Context(), id)
			if err != nil {
				return err
			}
			items[id] = item
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return items
}

func (f completionFixture) retire(t *testing.T) []domain.ItemID {
	t.Helper()
	var ids []domain.ItemID
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		ids, err = RetireCompletedTaskItems(t.Context(), tx, f.run.TaskID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestRetireCompletedTaskItemsSupersedesTheEpisodeDispute(t *testing.T) {
	f := newCompletionFixture(t, "completed")
	before := f.items(t)
	if ids := f.retire(t); !slices.Equal(ids, []domain.ItemID{f.dispute}) {
		t.Fatalf("retired = %v, want the dispute only", ids)
	}
	after := f.items(t)
	dispute := after[f.dispute]
	if dispute.Status != domain.StatusSuperseded || dispute.Conclusion == nil {
		t.Fatalf("dispute status = %q, conclusion = %+v", dispute.Status, dispute.Conclusion)
	}
	wantReason := "Superseded when the task completed: pull request #127 was merged. " +
		"Any review findings it lists were never routed; they stay on this card and in the task record."
	if dispute.Conclusion.Cause != domain.ConcludedBySupersession || dispute.Conclusion.Reason != wantReason ||
		!dispute.Conclusion.ConcludedAt.Equal(completionFixtureAt) || dispute.Conclusion.SuccessorItemID != nil || dispute.Conclusion.Decision != nil {
		t.Fatalf("dispute conclusion = %+v", *dispute.Conclusion)
	}
	if dispute.DecidedAt != nil {
		t.Fatalf("dispute decided_at = %v, want none for a daemon close", dispute.DecidedAt)
	}
	for _, id := range []domain.ItemID{f.proposal, f.ready} {
		if !reflect.DeepEqual(before[id], after[id]) {
			t.Fatalf("exempt item %s changed", id)
		}
	}
	if ids := f.retire(t); len(ids) != 0 {
		t.Fatalf("second retirement = %v, want none", ids)
	}
	if !reflect.DeepEqual(after, f.items(t)) {
		t.Fatal("second retirement changed items")
	}
}

func TestRetireCompletedTaskItemsLeavesUnfinishedEpisodes(t *testing.T) {
	for _, lifecycle := range []string{"active", "abandoned", "restarted", "recompleted"} {
		t.Run(lifecycle, func(t *testing.T) {
			f := newCompletionFixture(t, lifecycle)
			before := f.items(t)
			if ids := f.retire(t); len(ids) != 0 {
				t.Fatalf("retired = %v, want none", ids)
			}
			if !reflect.DeepEqual(before, f.items(t)) {
				t.Fatal("items changed")
			}
		})
	}
}

func TestCompletedEpisodeAgreesWithDisplayLifecycle(t *testing.T) {
	first, second := domain.CampaignID("campaign-1"), domain.CampaignID("campaign-2")
	fact := func(ordinal int, kind domain.TaskLifecycleFactKind, campaign *domain.CampaignID, episode int) domain.TaskLifecycleFact {
		return domain.TaskLifecycleFact{Ordinal: ordinal, Kind: kind, CampaignID: campaign, EpisodeOrdinal: episode}
	}
	started := fact(1, domain.TaskLifecycleStarted, &first, 0)
	for _, tc := range []struct {
		name     string
		facts    []domain.TaskLifecycleFact
		finished bool
	}{
		{"unstarted", nil, false},
		{"active", []domain.TaskLifecycleFact{started}, false},
		{"completed", []domain.TaskLifecycleFact{started, fact(2, domain.TaskLifecycleCompleted, &first, 1)}, true},
		{"legacy episode", []domain.TaskLifecycleFact{started, fact(2, domain.TaskLifecycleCompleted, &first, 0)}, true},
		{"other campaign", []domain.TaskLifecycleFact{started, fact(2, domain.TaskLifecycleCompleted, &second, 1)}, false},
		{"no campaign", []domain.TaskLifecycleFact{started, fact(2, domain.TaskLifecycleCompleted, nil, 1)}, false},
		{"other episode", []domain.TaskLifecycleFact{started, fact(2, domain.TaskLifecycleCompleted, &first, 3)}, false},
		{"abandoned", []domain.TaskLifecycleFact{started, fact(2, domain.TaskLifecycleAbandoned, nil, 0)}, false},
		{"restarted", []domain.TaskLifecycleFact{
			started, fact(2, domain.TaskLifecycleCompleted, &first, 1), fact(3, domain.TaskLifecycleStarted, &first, 0),
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := domain.Task{LifecycleFacts: tc.facts}
			_, completion := completedEpisode(task)
			display := task.DisplayLifecycle(nil)
			finished := display != nil && *display == domain.TaskFinished
			if (completion != nil) != tc.finished || finished != tc.finished {
				t.Fatalf("completion = %+v, display = %v, want finished %t", completion, display, tc.finished)
			}
		})
	}
}

func TestCompletionSupersedesAllButTheExemptTypes(t *testing.T) {
	var exempt []domain.AttentionType
	for _, kind := range domain.AllAttentionTypes {
		if !completionSupersedes(kind) {
			exempt = append(exempt, kind)
		}
	}
	want := []domain.AttentionType{domain.AttentionReadyForFinalReview, domain.AttentionTaskProposal, domain.AttentionEffectProposal}
	slices.Sort(exempt)
	slices.Sort(want)
	if !slices.Equal(exempt, want) {
		t.Fatalf("exempt = %v, want %v", exempt, want)
	}
	if completionSupersedes("") {
		t.Fatal("the invalid zero type is superseded")
	}
}

func TestReconcileRetiresCompletedTaskItemsOnce(t *testing.T) {
	f := newCompletionFixture(t, "completed")
	e, err := New(f.store, signet.NewService(f.store), execfake.NewStageDriver())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if status := f.items(t)[f.dispute].Status; status != domain.StatusSuperseded {
		t.Fatalf("dispute status after reconcile = %q", status)
	}
	revision := func() int64 {
		var state store.ServerState
		if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			state, err = tx.ServerState(t.Context())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return state.Revision
	}
	first := revision()
	if err := e.reconcileCompletedTaskItems(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second := revision(); second != first {
		t.Fatalf("revision moved from %d to %d on a no-op pass", first, second)
	}
}

func TestReconcileCompletedTaskItemsContinuesPastABadTask(t *testing.T) {
	f := newCompletionFixture(t, "completed")
	ctx := t.Context()
	other := domain.Run{ID: "completion-other-run", ProjectID: f.run.ProjectID, SpecDigest: "sha256:spec"}
	otherPolicy := completionPolicy(t, other.ID)
	other.PolicyDigest = otherPolicy.Digest
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, other); err != nil {
			return err
		}
		var err error
		if other, err = tx.GetRun(ctx, other.ID); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, otherPolicy); err != nil {
			return err
		}
		return tx.RecordTaskStart(ctx, other.ID, completionFixtureAt.Add(-time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	seedRunCompletion(t, f.store, other, otherPolicy, completionFixturePR+1, completionFixtureAt)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "completion-other-dispute", ProjectID: other.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(other.ID), RunID: &other.ID},
		Type:    domain.AttentionReviewDispute, Priority: domain.PriorityHigh, Reason: "Reword the test comment.",
		RequestedDecision: []domain.Action{domain.ActionDiscuss}, ItemVersion: 1, Status: domain.StatusOpen,
		InterruptionClass: domain.InterruptionExceptional,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatal(err)
	}
	// The first task's dispute keeps its candidate columns but loses its
	// body, so that task's item read fails.
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `UPDATE attention_items SET body = '{' WHERE id = ?`, f.dispute); err != nil {
		t.Fatal(err)
	}
	e, err := New(f.store, signet.NewService(f.store), execfake.NewStageDriver())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.reconcileCompletedTaskItems(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, item.ID)
		if err == nil && got.Status != domain.StatusSuperseded {
			t.Fatalf("other task's dispute status = %q", got.Status)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
