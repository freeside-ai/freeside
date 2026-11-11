package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// closureReferenceFixture builds a workflow, task, binding, and checkpoint
// that compose an authored candidate over st, seeded by seedClosureChain. The
// imported account carries a valid v1 publication claim, so the candidate's
// metadata renders whether or not the authoring checkpoint is readable.
func closureReferenceFixture(
	st *store.Store, policy domain.ResolvedPolicy, taskID domain.TaskID,
) (*productionPublicationWorkflow, productionPublicationTask, productionBinding, productionVerificationCheckpoint) {
	now := func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }
	w := &productionPublicationWorkflow{
		store: st, attention: signet.NewService(st), now: now,
		holdRetryAfter: map[string]time.Time{},
	}
	publication := recipePublicationFixture()
	publication.Recipe = clientPublicationRecipeV2
	task := productionPublicationTask{
		ProjectID: closureStoreProject, RunID: policy.RunID, PublicationID: "pub-1",
		ProducingInvocationID: "inv-producer", HeadSHA: closureStoreHead,
		Publication: publication,
	}
	claim := publicMetadataFixture("inv-producer", "# Retry closure reads\n\nKeep a transient read retryable.\n")
	checkpoint := productionVerificationCheckpoint{Imported: importer.Result{
		CommitSHA: closureStoreHead, Claims: []domain.AgentClaim{claim},
	}}
	return w, task, closureStoreBinding(policy, taskID), checkpoint
}

// composeClosureReference runs composePublicationCandidate with no handoff
// check, the shape both callers share once a candidate's inputs are bound.
func composeClosureReference(
	ctx context.Context, w *productionPublicationWorkflow, task productionPublicationTask,
	binding productionBinding, checkpoint productionVerificationCheckpoint,
) (*productionTaskOutcome, error) {
	_, held, err := w.composePublicationCandidate(
		ctx, task, binding, checkpoint, nil, nil, exec.ReviewInstructionBinding{}, nil,
	)
	return held, err
}

// assertNoClosureReferenceHold checks that composition left no
// public-metadata hold behind: no publish_blocked item and no durable run
// hold. Under the test's lock a hold write would fail too, so the nil
// outcome, not this check, is what proves the retry.
func assertNoClosureReferenceHold(t *testing.T, st *store.Store, task productionPublicationTask) {
	t.Helper()
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetAttentionItemRecord(t.Context(), task.blockedItemID()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("publish_blocked item read = %v, want not found", err)
		}
		hold, held, err := tx.GetRunHold(t.Context(), task.RunID)
		if err != nil {
			return err
		}
		if held {
			t.Errorf("run hold = %+v, want none", hold)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestComposeRetriesTransientClosureCheckpointRead proves a busy store read of
// the closure checkpoint reaches the run loop as a retryable error with no
// hold, and that the next pass composes the stored closure reference (#3001).
func TestComposeRetriesTransientClosureCheckpointRead(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{BusyTimeout: time.Millisecond})
	policy, taskID := seedClosureRows(t, st, "run-closure-retry", &domain.SpecificationSource{
		Kind: domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{
			Repo: closureStoreRepo, RepositoryID: closureStoreRepoID, IssueNumber: closureStoreIssue,
		},
	})
	w, task, binding, checkpoint := closureReferenceFixture(st, policy, taskID)
	stored := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: task.RunID, PublicationID: task.PublicationID,
		HasProposal: true, InstanceID: "proposal-1", Origin: domain.ClosureFlagOriginPolicyDefault,
	}
	if err := w.putClosureCheckpoint(ctx, productionClosureCheckpointKey(task.RunID, task.PublicationID), stored); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	held, err := composeClosureReference(ctx, w, task, binding, checkpoint)
	if held != nil {
		t.Fatalf("locked closure read held the task: outcome %+v, error %v", *held, err)
	}
	if err == nil || !productionPublicationRetryableFailure(err) {
		t.Fatalf("locked closure read = %v, want a retryable failure", err)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	assertNoClosureReferenceHold(t, st, task)
	candidate, err := w.productionCandidate(ctx, task, binding, checkpoint, nil, nil, nil)
	if err != nil {
		t.Fatalf("compose after the lock released: %v", err)
	}
	if candidate.ClosureInstanceID != stored.InstanceID {
		t.Fatalf("candidate closure instance = %q, want %q", candidate.ClosureInstanceID, stored.InstanceID)
	}
}

// TestComposeHoldsMismatchedClosureCheckpoint proves a closure row that fails
// its identity re-check still fails closed into the public-metadata hold: it
// is a state contradiction, not an environmental fault, so it never retries.
func TestComposeHoldsMismatchedClosureCheckpoint(t *testing.T) {
	t.Parallel()
	for name, write := range map[string]func(*testing.T, productionPublicationTask) (string, []byte){
		"wrong kind": func(t *testing.T, task productionPublicationTask) (string, []byte) {
			return "not-a-closure-checkpoint", []byte(`{}`)
		},
		"wrong run": func(t *testing.T, task productionPublicationTask) (string, []byte) {
			payload, err := json.Marshal(productionClosureCheckpoint{
				Version: productionClosureCheckpointVersion, RunID: "run-other", PublicationID: task.PublicationID,
			})
			if err != nil {
				t.Fatal(err)
			}
			return productionClosureCheckpointKind, payload
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			st, policy, taskID := seedClosureChain(t, "run-closure-mismatch")
			w, task, binding, checkpoint := closureReferenceFixture(st, policy, taskID)
			kind, payload := write(t, task)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				_, _, err := tx.RecordInbox(ctx, productionClosureCheckpointKey(task.RunID, task.PublicationID), kind, payload)
				return err
			}); err != nil {
				t.Fatal(err)
			}

			held, err := composeClosureReference(ctx, w, task, binding, checkpoint)
			if err != nil || held == nil {
				t.Fatalf("mismatched closure row: outcome %v, error %v; want the public-metadata hold", held, err)
			}
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				item, err := tx.GetAttentionItemRecord(ctx, task.blockedItemID())
				if err != nil {
					return err
				}
				if item.Type != domain.AttentionPublishBlocked {
					t.Errorf("hold item type = %q, want %q", item.Type, domain.AttentionPublishBlocked)
				}
				hold, found, err := tx.GetRunHold(ctx, task.RunID)
				if err != nil {
					return err
				}
				if !found || hold.Reason != domain.HoldTrustBlocked {
					t.Errorf("run hold = %+v (found %t), want %q", hold, found, domain.HoldTrustBlocked)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
