package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	t.Parallel()
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
		if !errors.Is(err, ErrDeclarationUnsupported) {
			t.Errorf("GetWorkUnitDeclarationByRun = %v, want %v", err, ErrDeclarationUnsupported)
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
		if !errors.Is(err, ErrCompletionUnsupported) {
			t.Errorf("GetWorkUnitCompletion = %v, want %v", err, ErrCompletionUnsupported)
		}
	})
}

// TestListWorkUnitCompletionsIsolatesUnsupportedRows: the start-up reconcile
// runs this list before the daemon is built, so one damaged row must be
// reported by id, never fail the list and keep the daemon from starting. A
// declaration the re-gate refuses and a completion body the store cannot
// reconstruct are both verdicts about one row, not infrastructure failures.
func TestListWorkUnitCompletionsIsolatesUnsupportedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := formatTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
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
	// The declaration claims a scope the resolved policy never derived, so
	// its read re-gate answers ErrDeclarationUnsupported rather than
	// ErrNotFound: the verdict that used to abort the whole list.
	declBody := `{"id":"workunit-run-1","run_id":"run-1","project_id":"proj-1",` +
		`"completion_criterion":"bound_pr_merged","declared_paths":["daemon/"],` +
		`"contract_serialized":false,"declared_at":"2026-01-02T03:04:05Z"}`
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO work_unit_declarations (unit_id, run_id, project_id, body, declared_at)
		 VALUES ('workunit-run-1', 'run-1', 'proj-1', ?, ?)`, declBody, at); err != nil {
		t.Fatalf("seed declaration: %v", err)
	}
	completionBody := `{"unit_id":"workunit-run-1","criterion":"bound_pr_merged",` +
		`"pr_number":450,"merge_commit_sha":"deadbeef",` +
		`"recorded_at":"2026-01-02T03:04:05Z"}`
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO work_unit_completions (unit_id, body, recorded_at)
		 VALUES ('workunit-run-1', ?, ?)`, completionBody, at); err != nil {
		t.Fatalf("seed completion: %v", err)
	}
	// A body the current vocabulary cannot express is a verdict about that
	// row too, and the row is still identified by its unit id column. Its
	// declaration is healthy so the refusal can only come from the body.
	policy2, err := domain.NewResolvedPolicy("run-2", []domain.PolicyKey{{
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
			ID: "run-2", ProjectID: "proj-1",
			SpecDigest: "sha256:spec", PolicyDigest: policy2.Digest,
		}); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, policy2)
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO work_unit_declarations (unit_id, run_id, project_id, body, declared_at)
		 VALUES ('workunit-run-2', 'run-2', 'proj-1', ?, ?)`,
		`{"id":"workunit-run-2","run_id":"run-2","project_id":"proj-1",`+
			`"completion_criterion":"bound_pr_merged","contract_serialized":false,`+
			`"declared_at":"2026-01-02T03:04:05Z"}`, at); err != nil {
		t.Fatalf("seed declaration: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO work_unit_completions (unit_id, body, recorded_at)
		 VALUES ('workunit-run-2', '{"unit_id":"workunit-run-2"}', ?)`, at); err != nil {
		t.Fatalf("seed damaged completion: %v", err)
	}
	var (
		supported   []domain.WorkUnitCompletion
		unsupported []domain.WorkUnitID
	)
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		supported, unsupported, err = tx.ListWorkUnitCompletions(ctx)
		return err
	}); err != nil {
		t.Fatalf("ListWorkUnitCompletions = %v, want the damaged rows isolated", err)
	}
	if len(supported) != 0 {
		t.Fatalf("supported = %+v, want none", supported)
	}
	want := []domain.WorkUnitID{"workunit-run-1", "workunit-run-2"}
	if !slices.Equal(unsupported, want) {
		t.Fatalf("unsupported = %v, want %v", unsupported, want)
	}
}

// TestIsRowVerdict pins the split every row-isolating caller depends on: a
// fail-closed verdict about one row may be logged and skipped, while a
// context, database, or transaction failure must propagate. Without the
// split a transient error reads as a refused row, and the start-up reconcile
// would leave a healthy completion unmirrored until the next restart.
func TestIsRowVerdict(t *testing.T) {
	t.Parallel()
	verdicts := map[string]error{
		"not found":               fmt.Errorf("get x: %w", ErrNotFound),
		"declaration unsupported": fmt.Errorf("get x: %w", ErrDeclarationUnsupported),
		"completion unsupported":  fmt.Errorf("get x: %w", ErrCompletionUnsupported),
		"row inconsistent":        fmt.Errorf("get x: %w", errRowInconsistent),
		"parent key mismatch":     fmt.Errorf("get x: %w", domain.ErrParentKeyMismatch),
	}
	for name, err := range verdicts {
		if !IsRowVerdict(err) {
			t.Errorf("IsRowVerdict(%s) = false, want true", name)
		}
	}
	infrastructure := map[string]error{
		"no error":          nil,
		"context canceled":  fmt.Errorf("observe run: %w", context.Canceled),
		"deadline exceeded": fmt.Errorf("observe run: %w", context.DeadlineExceeded),
		"database failure":  errors.New("database is locked"),
	}
	for name, err := range infrastructure {
		if IsRowVerdict(err) {
			t.Errorf("IsRowVerdict(%s) = true, want false", name)
		}
	}
}
