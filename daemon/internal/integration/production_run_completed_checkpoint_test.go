package integration_test

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func completedCheckpointHarness(t *testing.T) *productionPublicationHarness {
	t.Helper()
	p := newProductionPublicationHarnessWithBoundIssue(t, "", 79)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("publish original: %#v, %v", result, err)
	}
	return p
}

func assertCompletedSuccessorCheckpoint(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	var accepted domain.WorkUnitPRBinding
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		var err error
		accepted, err = tx.EffectiveWorkUnitPRBinding(p.ctx, p.declaration.ID)
		if err != nil {
			return err
		}
		original, err := tx.GetWorkUnitPRBinding(p.ctx, p.declaration.ID)
		if err != nil {
			return err
		}
		if accepted.HeadSHA == original.HeadSHA {
			t.Fatal("fixture did not publish a successor")
		}
		pull := domain.PullMergeFact{
			Repo: accepted.Repo, RepositoryID: accepted.RepositoryID, PRNumber: accepted.PRNumber,
			State: domain.PullRequestClosed, Merged: true, MergeCommitSHA: "successor-merge",
			BaseRef: accepted.BaseRef, HeadSHA: accepted.HeadSHA, ObservedAt: p.now,
		}
		issue := domain.IssueStateFact{
			Repo: accepted.Repo, RepositoryID: accepted.RepositoryID, IssueNumber: *p.declaration.BoundIssue,
			State: domain.IssueClosed, ClosedByCommitSHA: pull.MergeCommitSHA, ObservedAt: p.now,
		}
		if _, err := tx.AppendPullMergeFact(p.ctx, pull); err != nil {
			return err
		}
		if _, err := tx.AppendIssueStateFact(p.ctx, issue); err != nil {
			return err
		}
		completion, ok := domain.EvaluateWorkUnitCompletion(*p.declaration, accepted, pull, &issue)
		if !ok {
			t.Fatal("successor did not derive a completion")
		}
		return tx.RecordWorkUnitCompletion(p.ctx, completion)
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		checkpoint, err := readRealRunCheckpoint(p.ctx, tx, p.runID, true)
		if err != nil {
			return err
		}
		if checkpoint.State != "completed" || checkpoint.Completion == nil ||
			checkpoint.Binding.HeadSHA != accepted.HeadSHA || checkpoint.Completion.MergeCommitSHA != "successor-merge" {
			t.Fatalf("completion selected a predecessor publication: %#v", checkpoint)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertRealRunCheckpoint(t, p, false, "")
}

func TestRealRunCompletedCheckpointPreservesHistory(t *testing.T) {
	for _, action := range []domain.Action{"", domain.ActionDismiss, domain.ActionStop} {
		t.Run(string(action), func(t *testing.T) {
			p := completedCheckpointHarness(t)
			id := domain.ProductionReadyItemID(p.runID)
			if action != "" {
				id = decideCurrentReady(t, p, action)
			}
			before, err := p.attention.GetAttentionItem(p.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			recordOriginalCompletion(t, p)
			// A later issue reopen cannot erase the satisfying historical fact.
			if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
				binding, err := tx.GetWorkUnitPRBinding(p.ctx, p.declaration.ID)
				if err != nil {
					return err
				}
				_, err = tx.AppendIssueStateFact(p.ctx, domain.IssueStateFact{
					Repo: binding.Repo, RepositoryID: binding.RepositoryID, IssueNumber: 79,
					State: domain.IssueOpen, ObservedAt: p.now.Add(time.Minute),
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			ro, err := store.OpenReadOnly(p.ctx, p.dbPath, store.Options{
				ApprovedRecipes: map[domain.Digest]bool{p.image.RecipeDigest: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := ro.Close(); err != nil {
					t.Error(err)
				}
			}()
			if err := ro.Read(p.ctx, func(tx *store.ReadTx) error {
				checkpoint, err := readRealRunCheckpoint(p.ctx, tx, p.runID, true)
				if err != nil {
					return err
				}
				completion, err := tx.GetWorkUnitCompletion(p.ctx, p.declaration.ID)
				if err != nil {
					return err
				}
				if checkpoint.State != "completed" || checkpoint.Completion == nil ||
					!checkpoint.Completion.Equal(completion) || checkpoint.Binding.ItemID != id ||
					checkpoint.Binding.HeadSHA != before.Item.PRHeadSHA || checkpoint.Review != nil {
					t.Fatalf("completed history checkpoint: %#v", checkpoint)
				}
				if _, err := readRealRunCheckpoint(p.ctx, tx, p.runID, false); !errors.Is(err, store.ErrPublicationCompleted) {
					t.Fatalf("completed history gained publication acceptance: %v", err)
				}
				if err := tx.RequireIncompletePublication(p.ctx, p.runID); !errors.Is(err, store.ErrPublicationCompleted) {
					t.Fatalf("completed history gained continuation authority: %v", err)
				}
				after, err := tx.GetAttentionItem(p.ctx, id)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(before.Item, after) {
					t.Fatal("restoration verification changed the terminal card")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRealRunCompletedCheckpointRejectsForgedAuthority(t *testing.T) {
	for _, damage := range []struct {
		name    string
		queries []string
	}{
		{"unsupported-merge", []string{"UPDATE work_unit_completions SET body = json_set(body, '$.merge_commit_sha', 'forged')"}},
		{"wrong-bound-issue", []string{"UPDATE work_unit_completions SET body = json_set(body, '$.bound_issue', 80)"}},
		{"missing-merge-facts", []string{"DELETE FROM pull_merge_facts"}},
		{"different-published-pr", []string{
			"UPDATE work_unit_pr_bindings SET pr_number = pr_number + 1, body = json_set(body, '$.pr_number', pr_number + 1)",
			"UPDATE pull_merge_facts SET pr_number = pr_number + 1, body = json_set(body, '$.pr_number', pr_number + 1)",
			"UPDATE work_unit_completions SET body = json_set(body, '$.pr_number', json_extract(body, '$.pr_number') + 1)",
		}},
	} {
		t.Run(damage.name, func(t *testing.T) {
			p := completedCheckpointHarness(t)
			decideCurrentReady(t, p, domain.ActionDismiss)
			recordOriginalCompletion(t, p)
			assertRealRunCheckpoint(t, p, true, "completed")
			raw, err := sql.Open("sqlite", p.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := raw.Close(); err != nil {
					t.Error(err)
				}
			}()
			for _, query := range damage.queries {
				if _, err := raw.ExecContext(p.ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			if damage.name == "different-published-pr" {
				// The forged completion is internally consistent. Only its
				// independent link to the published ready resource rejects it.
				if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
					_, err := tx.GetWorkUnitCompletion(p.ctx, p.declaration.ID)
					return err
				}); err != nil {
					t.Fatalf("forge fixture did not reach publication binding gate: %v", err)
				}
			}
			assertRealRunCheckpoint(t, p, true, "")
		})
	}
}
