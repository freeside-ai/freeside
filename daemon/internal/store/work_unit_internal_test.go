package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestWorkUnitReadsFailClosedOnTamper: capture rows are authority for the
// 1B.2 projection, so a row the current vocabulary cannot express, or whose
// body disagrees with its key columns, is a read error, never a silently
// different fact. The raw inserts bypass the write boundary the way
// tampering or a future schema would.
func TestWorkUnitReadsFailClosedOnTamper(t *testing.T) {
	ctx := context.Background()
	at := formatTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	seed := func(t *testing.T) *Store {
		t.Helper()
		s, err := Open(ctx, t.TempDir()+"/capture.db", Options{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		policy, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{{
			Key: "driver", Value: "claude",
			Provenance: domain.KeyProvenance{
				Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("cd", 32)),
			},
		}})
		if err != nil {
			t.Fatalf("seed policy: %v", err)
		}
		if err := s.Write(ctx, func(tx *WriteTx) error {
			if err := tx.PutRun(ctx, domain.Run{
				ID: "run-1", ProjectID: "proj-1",
				SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
			}); err != nil {
				return err
			}
			return tx.PutResolvedPolicy(ctx, policy)
		}); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		return s
	}

	t.Run("declaration body disagrees with its key columns", func(t *testing.T) {
		s := seed(t)
		body := `{"id":"workunit-run-1","run_id":"run-1","project_id":"proj-OTHER",` +
			`"completion_criterion":"bound_pr_merged","contract_serialized":false,` +
			`"declared_at":"2026-01-02T03:04:05Z"}`
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO work_unit_declarations (unit_id, run_id, project_id, body, declared_at)
			 VALUES ('workunit-run-1', 'run-1', 'proj-1', ?, ?)`, body, at); err != nil {
			t.Fatalf("seed tampered declaration: %v", err)
		}
		err := s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetWorkUnitDeclarationByRun(ctx, "run-1")
			return err
		})
		if !errors.Is(err, errRowInconsistent) {
			t.Errorf("GetWorkUnitDeclarationByRun = %v, want %v", err, errRowInconsistent)
		}
	})

	t.Run("declaration body outside the current vocabulary", func(t *testing.T) {
		s := seed(t)
		body := `{"id":"workunit-run-1","run_id":"run-1","project_id":"proj-1",` +
			`"completion_criterion":"made_up_criterion","contract_serialized":false,` +
			`"declared_at":"2026-01-02T03:04:05Z"}`
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO work_unit_declarations (unit_id, run_id, project_id, body, declared_at)
			 VALUES ('workunit-run-1', 'run-1', 'proj-1', ?, ?)`, body, at); err != nil {
			t.Fatalf("seed tampered declaration: %v", err)
		}
		err := s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetWorkUnitDeclarationByRun(ctx, "run-1")
			return err
		})
		if !errors.Is(err, domain.ErrInvalidCompletionCriterion) {
			t.Errorf("GetWorkUnitDeclarationByRun = %v, want %v", err, domain.ErrInvalidCompletionCriterion)
		}
	})

	t.Run("merge fact body disagrees with its resource columns", func(t *testing.T) {
		s := seed(t)
		body := `{"repo":"owner/repo","repository_id":999,"pr_number":450,` +
			`"state":"open","merged":false,"base_ref":"main","head_sha":"cafebabe",` +
			`"observed_at":"2026-01-02T03:04:05Z"}`
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO pull_merge_facts (repository_id, pr_number, body, observed_at)
			 VALUES (424242, 450, ?, ?)`, body, at); err != nil {
			t.Fatalf("seed tampered fact: %v", err)
		}
		err := s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.LatestPullMergeFact(ctx, 424242, 450)
			return err
		})
		if !errors.Is(err, errRowInconsistent) {
			t.Errorf("LatestPullMergeFact = %v, want %v", err, errRowInconsistent)
		}
	})

	t.Run("completion smuggling an issue past its criterion", func(t *testing.T) {
		s := seed(t)
		declBody := `{"id":"workunit-run-1","run_id":"run-1","project_id":"proj-1",` +
			`"completion_criterion":"bound_pr_merged","contract_serialized":false,` +
			`"declared_at":"2026-01-02T03:04:05Z"}`
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO work_unit_declarations (unit_id, run_id, project_id, body, declared_at)
			 VALUES ('workunit-run-1', 'run-1', 'proj-1', ?, ?)`, declBody, at); err != nil {
			t.Fatalf("seed declaration: %v", err)
		}
		body := `{"unit_id":"workunit-run-1","criterion":"bound_pr_merged",` +
			`"pr_number":450,"merge_commit_sha":"deadbeef","bound_issue":443,` +
			`"recorded_at":"2026-01-02T03:04:05Z"}`
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO work_unit_completions (unit_id, body, recorded_at)
			 VALUES ('workunit-run-1', ?, ?)`, body, at); err != nil {
			t.Fatalf("seed tampered completion: %v", err)
		}
		err := s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetWorkUnitCompletion(ctx, "workunit-run-1")
			return err
		})
		if !errors.Is(err, domain.ErrCompletionInconsistent) {
			t.Errorf("GetWorkUnitCompletion = %v, want %v", err, domain.ErrCompletionInconsistent)
		}
	})

	t.Run("declaration whose scope disagrees with the resolved policy", func(t *testing.T) {
		s := seed(t)
		// The seeded policy declares no paths key, so a declaration
		// claiming a daemon/ scope was never derivable from it: the read
		// re-gate refuses it rather than letting the projection reason
		// over a boundary the runner never enforced.
		body := `{"id":"workunit-run-1","run_id":"run-1","project_id":"proj-1",` +
			`"completion_criterion":"bound_pr_merged","declared_paths":["daemon/"],` +
			`"contract_serialized":false,"declared_at":"2026-01-02T03:04:05Z"}`
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO work_unit_declarations (unit_id, run_id, project_id, body, declared_at)
			 VALUES ('workunit-run-1', 'run-1', 'proj-1', ?, ?)`, body, at); err != nil {
			t.Fatalf("seed tampered declaration: %v", err)
		}
		err := s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetWorkUnitDeclarationByRun(ctx, "run-1")
			return err
		})
		if !errors.Is(err, errDeclarationUnsupported) {
			t.Errorf("GetWorkUnitDeclarationByRun = %v, want %v", err, errDeclarationUnsupported)
		}
	})

	t.Run("internally valid completion with no supporting facts", func(t *testing.T) {
		s := seed(t)
		declBody := `{"id":"workunit-run-1","run_id":"run-1","project_id":"proj-1",` +
			`"completion_criterion":"bound_pr_merged","contract_serialized":false,` +
			`"declared_at":"2026-01-02T03:04:05Z"}`
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO work_unit_declarations (unit_id, run_id, project_id, body, declared_at)
			 VALUES ('workunit-run-1', 'run-1', 'proj-1', ?, ?)`, declBody, at); err != nil {
			t.Fatalf("seed declaration: %v", err)
		}
		bindingBody := `{"unit_id":"workunit-run-1","repo":"owner/repo",` +
			`"repository_id":424242,"pr_number":450,"base_ref":"main",` +
			`"head_sha":"cafebabe","recorded_at":"2026-01-02T03:04:05Z"}`
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO work_unit_pr_bindings (unit_id, repository_id, pr_number, body, recorded_at)
			 VALUES ('workunit-run-1', 424242, 450, ?, ?)`, bindingBody, at); err != nil {
			t.Fatalf("seed binding: %v", err)
		}
		// A done bit that passes shape validation and matches its key
		// columns, with no recorded observation deriving it: the injected
		// claim the read re-gate exists to refuse.
		body := `{"unit_id":"workunit-run-1","criterion":"bound_pr_merged",` +
			`"pr_number":450,"merge_commit_sha":"deadbeef",` +
			`"recorded_at":"2026-01-02T03:04:05Z"}`
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO work_unit_completions (unit_id, body, recorded_at)
			 VALUES ('workunit-run-1', ?, ?)`, body, at); err != nil {
			t.Fatalf("seed forged completion: %v", err)
		}
		err := s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetWorkUnitCompletion(ctx, "workunit-run-1")
			return err
		})
		if !errors.Is(err, errCompletionUnsupported) {
			t.Errorf("GetWorkUnitCompletion = %v, want %v", err, errCompletionUnsupported)
		}
	})
}
