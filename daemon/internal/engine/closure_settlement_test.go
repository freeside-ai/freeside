package engine

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const closureSettlementPR = 17

var closureSettlementObservedAt = closureSequenceAt.Add(time.Hour)

// bindPR records the work unit's PR binding, as the publisher does once the
// pull request exists.
func (f *closureSequenceFixture) bindPR(t *testing.T) {
	t.Helper()
	if err := f.st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitPRBinding(context.Background(), domain.WorkUnitPRBinding{
			UnitID: f.declaration.ID, Repo: closureStoreRepo, RepositoryID: closureStoreRepoID,
			PRNumber: closureSettlementPR, BaseRef: closureStoreBaseRef, HeadSHA: closureSequenceHead1,
			RecordedAt: closureSequenceAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// pullFact is the observers' pull request fact for the unit's PR.
func pullFact(state domain.PullRequestState, merged bool) domain.PullMergeFact {
	fact := domain.PullMergeFact{
		Repo: closureStoreRepo, RepositoryID: closureStoreRepoID, PRNumber: closureSettlementPR,
		State: state, Merged: merged, BaseRef: closureStoreBaseRef, HeadSHA: closureSequenceHead1,
		ObservedAt: closureSettlementObservedAt,
	}
	if merged {
		fact.MergeCommitSHA = closureSequenceHead2
	}
	return fact
}

// observe appends the observers' facts.
func (f *closureSequenceFixture) observe(t *testing.T, pull *domain.PullMergeFact, issue *domain.IssueStateFact) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if pull != nil {
			if _, err := tx.AppendPullMergeFact(ctx, *pull); err != nil {
				return err
			}
		}
		if issue != nil {
			if _, err := tx.AppendIssueStateFact(ctx, *issue); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *closureSequenceFixture) retireSettled(t *testing.T) []domain.ItemID {
	t.Helper()
	var retired []domain.ItemID
	if err := f.st.Write(context.Background(), func(tx *store.WriteTx) error {
		var err error
		retired, err = RetireSettledClosureProposals(context.Background(), tx, f.declaration)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return retired
}

func (f *closureSequenceFixture) card(t *testing.T, publication int) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := f.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(context.Background(), f.opened[f.ordered[publication]])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

// TestSettledClosureCardsRetire proves each settling fact retires the unit's
// open closure card with its own sentence, stamped at the fact's
// observed_at, with no task completion recorded, and that a second call
// retires nothing.
func TestSettledClosureCardsRetire(t *testing.T) {
	boundIssue := closureStoreIssue
	closedIssue := domain.IssueStateFact{
		Repo: closureStoreRepo, RepositoryID: closureStoreRepoID, IssueNumber: closureStoreIssue,
		State: domain.IssueClosed, ObservedAt: closureSettlementObservedAt,
	}
	merged := pullFact(domain.PullRequestClosed, true)
	closed := pullFact(domain.PullRequestClosed, false)
	open := pullFact(domain.PullRequestOpen, false)
	for _, tc := range []struct {
		name       string
		boundIssue *int
		pull       *domain.PullMergeFact
		issue      *domain.IssueStateFact
		reason     string
	}{
		{
			name: "merged", pull: &merged,
			reason: "Superseded because pull request #17 was merged; the merge settled whether it closes the source issue.",
		},
		{
			name: "closed unmerged", pull: &closed,
			reason: "Superseded because pull request #17 was closed without merging.",
		},
		{
			name: "bound issue closed", boundIssue: &boundIssue, pull: &open, issue: &closedIssue,
			reason: "Superseded because issue #7 is already closed.",
		},
		{
			name: "merged with bound issue closed", boundIssue: &boundIssue, pull: &merged, issue: &closedIssue,
			reason: "Superseded because pull request #17 was merged; the merge settled whether it closes the source issue.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedClosureSequenceBound(t, tc.boundIssue)
			f.publish(t, domain.ProductionPublicationInvocationID(f.policy.RunID), closureSequenceHead1, closureSequenceBase1)
			f.bindPR(t)
			f.observe(t, tc.pull, tc.issue)
			if got := f.retireSettled(t); len(got) != 1 || got[0] != f.opened[f.ordered[0]] {
				t.Fatalf("retired = %v, want the publication's card", got)
			}
			f.requireOpen(t, tc.name, nil)
			f.requireRetired(t, tc.name, 0, tc.reason)
			if at := f.card(t, 0).Conclusion.ConcludedAt; !at.Equal(closureSettlementObservedAt) {
				t.Fatalf("concluded_at = %s, want the fact's observed_at %s", at, closureSettlementObservedAt)
			}
			if got := f.retireSettled(t); len(got) != 0 {
				t.Fatalf("second call retired %v, want nothing", got)
			}
		})
	}
}

// TestUnsettledClosureCardsStayOpen proves a card whose unit has no PR
// binding, only an open pull request fact, or only a closed fact for its
// bound issue observed before the unit's PR existed (an earlier unit's, on an
// issue since reopened) keeps its state and version, and that a decided card
// stays decided after its merge is observed.
func TestUnsettledClosureCardsStayOpen(t *testing.T) {
	requireUntouched := func(t *testing.T, f *closureSequenceFixture, status domain.ItemStatus, version int) {
		t.Helper()
		if got := f.retireSettled(t); len(got) != 0 {
			t.Fatalf("retired %v, want nothing", got)
		}
		if item := f.card(t, 0); item.Status != status || item.ItemVersion != version {
			t.Fatalf("card = status %q version %d, want %q at version %d", item.Status, item.ItemVersion, status, version)
		}
	}
	t.Run("no binding", func(t *testing.T) {
		f := seedClosureSequence(t)
		f.publish(t, domain.ProductionPublicationInvocationID(f.policy.RunID), closureSequenceHead1, closureSequenceBase1)
		requireUntouched(t, f, domain.StatusOpen, 1)
	})
	t.Run("open pull request", func(t *testing.T) {
		f := seedClosureSequence(t)
		f.publish(t, domain.ProductionPublicationInvocationID(f.policy.RunID), closureSequenceHead1, closureSequenceBase1)
		f.bindPR(t)
		open := pullFact(domain.PullRequestOpen, false)
		f.observe(t, &open, nil)
		requireUntouched(t, f, domain.StatusOpen, 1)
	})
	t.Run("issue closed before the binding", func(t *testing.T) {
		boundIssue := closureStoreIssue
		f := seedClosureSequenceBound(t, &boundIssue)
		f.publish(t, domain.ProductionPublicationInvocationID(f.policy.RunID), closureSequenceHead1, closureSequenceBase1)
		f.bindPR(t)
		open := pullFact(domain.PullRequestOpen, false)
		f.observe(t, &open, &domain.IssueStateFact{
			Repo: closureStoreRepo, RepositoryID: closureStoreRepoID, IssueNumber: closureStoreIssue,
			State: domain.IssueClosed, ObservedAt: closureSequenceAt.Add(-time.Minute),
		})
		requireUntouched(t, f, domain.StatusOpen, 1)
	})
	t.Run("decided card", func(t *testing.T) {
		f := seedClosureSequence(t)
		f.publish(t, domain.ProductionPublicationInvocationID(f.policy.RunID), closureSequenceHead1, closureSequenceBase1)
		card := f.card(t, 0)
		if err := f.st.Write(context.Background(), func(tx *store.WriteTx) error {
			closed, err := card.ConcludeByCommand(domain.StatusDismissed, closureSequenceAt.Add(time.Minute), domain.ConclusionDecision{
				CommandID: "command-decline", Action: domain.ActionDecline, DeviceID: "device-1", DeviceDisplayName: "Phone",
			})
			if err != nil {
				return err
			}
			closed.ItemVersion = card.ItemVersion + 1
			return tx.PutAttentionItem(context.Background(), closed)
		}); err != nil {
			t.Fatal(err)
		}
		f.bindPR(t)
		merged := pullFact(domain.PullRequestClosed, true)
		f.observe(t, &merged, nil)
		requireUntouched(t, f, domain.StatusDismissed, card.ItemVersion+1)
	})
}

// TestReconcileRetiresSettledClosureCardsOnce proves the engine's reconcile
// pass runs the sweep once the merge is observed, and that a pass over a held
// card or an already retired one bumps no revision.
func TestReconcileRetiresSettledClosureCardsOnce(t *testing.T) {
	f := seedClosureSequence(t)
	f.publish(t, domain.ProductionPublicationInvocationID(f.policy.RunID), closureSequenceHead1, closureSequenceBase1)
	f.bindPR(t)
	e, err := New(f.st, signet.NewService(f.st), execfake.NewStageDriver())
	if err != nil {
		t.Fatal(err)
	}
	revision := func() int64 {
		var state store.ServerState
		if err := f.st.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			state, err = tx.ServerState(t.Context())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return state.Revision
	}
	requireNoOpPass := func(step string) {
		t.Helper()
		before := revision()
		if err := e.reconcileSettledClosureProposals(t.Context()); err != nil {
			t.Fatal(err)
		}
		if after := revision(); after != before {
			t.Fatalf("%s: revision moved from %d to %d on a no-op pass", step, before, after)
		}
	}
	requireNoOpPass("held card")
	if item := f.card(t, 0); item.Status != domain.StatusOpen {
		t.Fatalf("held card status = %q, want open", item.Status)
	}

	merged := pullFact(domain.PullRequestClosed, true)
	f.observe(t, &merged, nil)
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if item := f.card(t, 0); item.Status != domain.StatusSuperseded {
		t.Fatalf("card status after reconcile = %q, want superseded", item.Status)
	}
	requireNoOpPass("retired card")
}
