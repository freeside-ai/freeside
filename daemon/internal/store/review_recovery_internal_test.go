package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestReviewRecoveryMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0032_")
	if got := rawVersion(t, db); got != 31 {
		t.Fatalf("prior schema version = %d, want 31", got)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := rawVersion(t, db); got != 39 {
		t.Fatalf("schema version = %d, want 39", got)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM review_recovery_transitions`).Scan(&count); err != nil {
		t.Fatalf("count recovery transitions: %v", err)
	}
	if count != 0 {
		t.Fatalf("new recovery log contains %d rows, want 0", count)
	}
}

func seedReviewRecovery(t *testing.T) (*Store, domain.ReviewRecoveryTransition) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 7, 1, 2, 3, 0, time.UTC)
	run := domain.Run{
		ID: "run-recovery", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	failure := domain.ReviewFailure{
		RunID: run.ID, InvocationID: "review-recovery-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureContradiction,
		Reason: "review contradicted its contract", ObservedAt: at,
	}
	failureBody, err := encode(failure)
	if err != nil {
		t.Fatalf("encode failure: %v", err)
	}
	digest := domain.Digest(reviewBodyDigest(failureBody))
	binding := domain.ReviewRecoveryBinding{
		RunID: failure.RunID, InvocationID: failure.InvocationID, Round: failure.Round,
		BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA, FailureDigest: digest,
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "review-recovery-item", ProjectID: run.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID,
		},
		Type: domain.AttentionReviewContradiction, Priority: domain.PriorityHigh,
		Reason:            "review contradicted its contract",
		RequestedDecision: []domain.Action{domain.ActionRecoverReview},
		PRHeadSHA:         failure.HeadSHA, ReviewRecoveryBinding: &binding,
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	commandID := "command-recover-review"
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: commandID, DeviceID: "device-1", ItemID: item.ID,
		ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		ArtifactDigests: item.ArtifactDigests, Action: domain.ActionRecoverReview,
	})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	transition := domain.ReviewRecoveryTransition{
		RunID: binding.RunID, InvocationID: binding.InvocationID, Round: binding.Round,
		BaseSHA: binding.BaseSHA, HeadSHA: binding.HeadSHA, FailureDigest: binding.FailureDigest,
		CommandID: &commandID, Reason: "operator authorized recovery", OccurredAt: at.Add(time.Minute),
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewFailure(ctx, failure); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		return tx.RecordReviewRecoveryTransition(ctx, transition)
	}); err != nil {
		t.Fatalf("seed recovery: %v", err)
	}
	return st, transition
}

func TestReviewRecoveryTransitionRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, want := seedReviewRecovery(t)
	var got domain.ReviewRecoveryTransition
	var found bool
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, found, err = tx.LatestReviewRecoveryTransition(ctx, want.RunID)
		return err
	}); err != nil {
		t.Fatalf("LatestReviewRecoveryTransition: %v", err)
	}
	if !found {
		t.Fatal("recovery transition not found")
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("round trip = %s, want %s", gotJSON, wantJSON)
	}
	pretty, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "review_recovery_transition", append(pretty, '\n'))

	var absent bool
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, found, err := tx.LatestReviewRecoveryTransition(ctx, "run-other")
		absent = !found
		return err
	}); err != nil || !absent {
		t.Fatalf("unrelated run lookup = absent %v, err %v", absent, err)
	}
}

func TestReviewRecoveryWriteRejectsEveryMismatchedBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, valid := seedReviewRecovery(t)
	for name, mutate := range map[string]func(*domain.ReviewRecoveryTransition){
		"run":        func(tr *domain.ReviewRecoveryTransition) { tr.RunID = "run-other" },
		"invocation": func(tr *domain.ReviewRecoveryTransition) { tr.InvocationID = "review-other" },
		"round":      func(tr *domain.ReviewRecoveryTransition) { tr.Round++ },
		"base":       func(tr *domain.ReviewRecoveryTransition) { tr.BaseSHA = "other-base" },
		"head":       func(tr *domain.ReviewRecoveryTransition) { tr.HeadSHA = "other-head" },
		"digest":     func(tr *domain.ReviewRecoveryTransition) { tr.FailureDigest = "sha256:other" },
	} {
		t.Run(name, func(t *testing.T) {
			transition := valid
			mutate(&transition)
			err := st.WriteInternal(ctx, func(tx *InternalTx) error {
				return tx.RecordReviewRecoveryTransition(ctx, transition)
			})
			if !errors.Is(err, domain.ErrReviewRecoveryBindingMismatch) {
				t.Fatalf("mismatched write = %v, want %v", err, domain.ErrReviewRecoveryBindingMismatch)
			}
		})
	}
}

func TestReviewRecoveryReadFailsClosedOnTamper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tamper := range map[string]func(*Store) error{
		"unbacked": func(st *Store) error {
			_, err := st.db.ExecContext(ctx,
				`UPDATE review_recovery_transitions SET command_id = NULL`)
			return err
		},
		"binding": func(st *Store) error {
			_, err := st.db.ExecContext(ctx,
				`UPDATE review_recovery_transitions SET head_sha = 'forged-head'`)
			return err
		},
		"failure body bytes": func(st *Store) error {
			var invocationID, body string
			if err := st.db.QueryRowContext(ctx,
				`SELECT invocation_id, body FROM review_failures`).Scan(&invocationID, &body); err != nil {
				return err
			}
			body += "\n"
			_, err := st.db.ExecContext(ctx,
				`UPDATE review_failures SET body = ?, body_digest = ? WHERE invocation_id = ?`,
				body, reviewBodyDigest(body), invocationID)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, transition := seedReviewRecovery(t)
			if err := tamper(st); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, _, err := tx.LatestReviewRecoveryTransition(ctx, transition.RunID)
				return err
			})
			want := domain.ErrReviewRecoveryBindingMismatch
			if name == "unbacked" {
				want = domain.ErrTransitionUnbacked
			}
			if !errors.Is(err, want) {
				t.Fatalf("tampered read = %v, want %v", err, want)
			}
		})
	}
}

func TestReviewRecoveryRequiresContradiction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, transition := seedReviewRecovery(t)
	var rewrittenBody string
	if err := st.db.QueryRowContext(ctx,
		`SELECT json_set(body, '$.class', 'quota') FROM review_failures WHERE invocation_id = ?`,
		transition.InvocationID).Scan(&rewrittenBody); err != nil {
		t.Fatalf("read rewritten failure: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_failures SET failure_class = 'quota',
		 body = json_set(body, '$.class', 'quota'), body_digest = ?
		 WHERE invocation_id = ?`,
		reviewBodyDigest(rewrittenBody), transition.InvocationID); err != nil {
		t.Fatalf("rewrite failure: %v", err)
	}
	err := st.Read(ctx, func(tx *ReadTx) error {
		_, _, err := tx.LatestReviewRecoveryTransition(ctx, transition.RunID)
		return err
	})
	if !errors.Is(err, domain.ErrReviewRecoveryBindingMismatch) {
		t.Fatalf("non-contradiction recovery = %v, want %v", err, domain.ErrReviewRecoveryBindingMismatch)
	}
}
