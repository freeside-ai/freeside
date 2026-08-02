package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Work-unit capture rows (migration 0026, plan §5.18): the explicit
// declarations and first-party observations the 1B.2 frontier projection
// derives from. The write methods live on InternalTx because the rows are
// not synchronized client state (the projection is 1B.2's surface), but
// unlike the 0024 observation projection they are authority for later
// derivation, so writes are write-once or append-on-material-change and
// reads run the full reconstruction gate, failing closed on anything the
// current vocabulary cannot express.

const (
	insertWorkUnitDeclarationSQL = `INSERT INTO work_unit_declarations
		(unit_id, run_id, project_id, body, declared_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`
	selectWorkUnitDeclarationBodySQL = `SELECT body
		FROM work_unit_declarations WHERE unit_id = ?`
	getWorkUnitDeclarationByRunSQL = `SELECT unit_id, run_id, project_id, body, declared_at
		FROM work_unit_declarations WHERE run_id = ?`
	getWorkUnitDeclarationSQL = `SELECT unit_id, run_id, project_id, body, declared_at
		FROM work_unit_declarations WHERE unit_id = ?`
	insertWorkUnitPRBindingSQL = `INSERT INTO work_unit_pr_bindings
		(unit_id, repository_id, pr_number, body, recorded_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`
	selectWorkUnitPRBindingBodySQL = `SELECT body
		FROM work_unit_pr_bindings WHERE unit_id = ?`
	getWorkUnitPRBindingSQL = `SELECT unit_id, repository_id, pr_number, body, recorded_at
		FROM work_unit_pr_bindings WHERE unit_id = ?`
	insertPullMergeFactSQL = `INSERT INTO pull_merge_facts
		(repository_id, pr_number, body, observed_at)
		VALUES (?, ?, ?, ?)`
	latestPullMergeFactSQL = `SELECT repository_id, pr_number, body, observed_at
		FROM pull_merge_facts WHERE repository_id = ? AND pr_number = ?
		ORDER BY id DESC LIMIT 1`
	listPullMergeFactsSQL = `SELECT repository_id, pr_number, body, observed_at
		FROM pull_merge_facts WHERE repository_id = ? AND pr_number = ?
		ORDER BY id`
	insertIssueStateFactSQL = `INSERT INTO issue_state_facts
		(repository_id, issue_number, body, observed_at)
		VALUES (?, ?, ?, ?)`
	latestIssueStateFactSQL = `SELECT repository_id, issue_number, body, observed_at
		FROM issue_state_facts WHERE repository_id = ? AND issue_number = ?
		ORDER BY id DESC LIMIT 1`
	listIssueStateFactsSQL = `SELECT repository_id, issue_number, body, observed_at
		FROM issue_state_facts WHERE repository_id = ? AND issue_number = ?
		ORDER BY id`
	insertWorkUnitCompletionSQL = `INSERT INTO work_unit_completions
		(unit_id, body, recorded_at)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`
	selectWorkUnitCompletionBodySQL = `SELECT body
		FROM work_unit_completions WHERE unit_id = ?`
	getWorkUnitCompletionSQL = `SELECT unit_id, body, recorded_at
		FROM work_unit_completions WHERE unit_id = ?`
)

// RecordWorkUnitDeclaration records one unit's declaration, write-once: a
// byte-identical replay converges, a differing re-declaration is
// ErrImmutableConflict (a changed declaration is a different unit, never an
// update).
func (tx *InternalTx) RecordWorkUnitDeclaration(ctx context.Context, d domain.WorkUnitDeclaration) error {
	body, err := encode(d)
	if err != nil {
		return fmt.Errorf("put work unit declaration: %w", err)
	}
	if err := tx.putImmutable(ctx, insertWorkUnitDeclarationSQL,
		[]any{d.ID, d.RunID, d.ProjectID, body, formatTime(d.DeclaredAt)},
		selectWorkUnitDeclarationBodySQL, []any{d.ID}, body); err != nil {
		return fmt.Errorf("put work unit declaration %s: %w", d.ID, err)
	}
	return nil
}

// scanWorkUnitDeclaration reconstructs one declaration row (see the scanner
// doc for the shared gate sequence). Errors are returned unwrapped; callers
// add the entity/key context.
func (tx *ReadTx) scanWorkUnitDeclaration(sc scanner) (domain.WorkUnitDeclaration, error) {
	var (
		unitID, runID, projectID, declaredAt string
		body                                 []byte
	)
	if err := sc.Scan(&unitID, &runID, &projectID, &body, &declaredAt); err != nil {
		return domain.WorkUnitDeclaration{}, err
	}
	d, err := decode[domain.WorkUnitDeclaration](body)
	if err != nil {
		return domain.WorkUnitDeclaration{}, err
	}
	if d.ID != domain.WorkUnitID(unitID) || d.RunID != domain.RunID(runID) ||
		d.ProjectID != domain.ProjectID(projectID) || formatTime(d.DeclaredAt) != declaredAt {
		return domain.WorkUnitDeclaration{}, errRowInconsistent
	}
	return d, nil
}

// errDeclarationUnsupported is the declaration re-gate's fail-closed
// verdict: the stored declaration disagrees with the run and resolved
// policy it claims to describe.
var errDeclarationUnsupported = errors.New(
	"stored work-unit declaration is not supported by its run and resolved policy")

// GetWorkUnitDeclarationByRun returns the declaration captured with the
// given run, or ErrNotFound for an undeclared run (which is legitimate:
// capture records explicit declarations only).
func (tx *ReadTx) GetWorkUnitDeclarationByRun(ctx context.Context, runID domain.RunID) (domain.WorkUnitDeclaration, error) {
	d, err := tx.scanWorkUnitDeclaration(tx.tx.QueryRowContext(ctx, getWorkUnitDeclarationByRunSQL, runID))
	if err != nil {
		return domain.WorkUnitDeclaration{}, fmt.Errorf("get work unit declaration for run %s: %w", runID, notFoundOr(err))
	}
	if err := tx.regateWorkUnitDeclaration(ctx, d); err != nil {
		return domain.WorkUnitDeclaration{}, fmt.Errorf("get work unit declaration for run %s: %w", runID, err)
	}
	return d, nil
}

// GetWorkUnitDeclaration returns one unit's declaration by its id.
func (tx *ReadTx) GetWorkUnitDeclaration(ctx context.Context, unitID domain.WorkUnitID) (domain.WorkUnitDeclaration, error) {
	d, err := tx.scanWorkUnitDeclaration(tx.tx.QueryRowContext(ctx, getWorkUnitDeclarationSQL, unitID))
	if err != nil {
		return domain.WorkUnitDeclaration{}, fmt.Errorf("get work unit declaration %s: %w", unitID, notFoundOr(err))
	}
	if err := tx.regateWorkUnitDeclaration(ctx, d); err != nil {
		return domain.WorkUnitDeclaration{}, fmt.Errorf("get work unit declaration %s: %w", unitID, err)
	}
	return d, nil
}

// regateWorkUnitDeclaration re-derives the declaration's run-bound
// coordinates from the records it claims to describe (the store's
// reconstruction-gate convention): the project must be the run's, and the
// declared path scope must be exactly what the stored resolved policy's
// paths key derives — the same single definition intake used — so the
// scope the 1B.2 projection will reason over can never drift from the
// boundary the runner enforced. Fails closed on a missing run or policy:
// the foreign key and the intake transaction make that state unreachable
// by writes.
func (tx *ReadTx) regateWorkUnitDeclaration(ctx context.Context, d domain.WorkUnitDeclaration) error {
	run, err := tx.GetRun(ctx, d.RunID)
	if errors.Is(err, ErrNotFound) {
		return errDeclarationUnsupported
	}
	if err != nil {
		return err
	}
	if run.ProjectID != d.ProjectID {
		return errDeclarationUnsupported
	}
	policy, err := tx.GetResolvedPolicy(ctx, d.RunID)
	if errors.Is(err, ErrNotFound) {
		return errDeclarationUnsupported
	}
	if err != nil {
		return err
	}
	if !slices.Equal(d.DeclaredPaths, domain.CanonicalDeclaredPaths(policy)) {
		return errDeclarationUnsupported
	}
	return nil
}

// RecordWorkUnitPRBinding records the unit's exact PR binding, write-once from
// first-party publication facts; the publication workflow's convergent
// passes replay it byte-identically.
func (tx *InternalTx) RecordWorkUnitPRBinding(ctx context.Context, b domain.WorkUnitPRBinding) error {
	body, err := encode(b)
	if err != nil {
		return fmt.Errorf("put work unit pr binding: %w", err)
	}
	if err := tx.putImmutable(ctx, insertWorkUnitPRBindingSQL,
		[]any{b.UnitID, b.RepositoryID, b.PRNumber, body, formatTime(b.RecordedAt)},
		selectWorkUnitPRBindingBodySQL, []any{b.UnitID}, body); err != nil {
		return fmt.Errorf("put work unit pr binding %s: %w", b.UnitID, err)
	}
	return nil
}

// scanWorkUnitPRBinding reconstructs one binding row.
func (tx *ReadTx) scanWorkUnitPRBinding(sc scanner) (domain.WorkUnitPRBinding, error) {
	var (
		unitID, recordedAt     string
		repositoryID, prNumber int64
		body                   []byte
	)
	if err := sc.Scan(&unitID, &repositoryID, &prNumber, &body, &recordedAt); err != nil {
		return domain.WorkUnitPRBinding{}, err
	}
	b, err := decode[domain.WorkUnitPRBinding](body)
	if err != nil {
		return domain.WorkUnitPRBinding{}, err
	}
	if b.UnitID != domain.WorkUnitID(unitID) || b.RepositoryID != repositoryID ||
		int64(b.PRNumber) != prNumber || formatTime(b.RecordedAt) != recordedAt {
		return domain.WorkUnitPRBinding{}, errRowInconsistent
	}
	return b, nil
}

// GetWorkUnitPRBinding returns the unit's recorded PR binding, or
// ErrNotFound before publication records one.
func (tx *ReadTx) GetWorkUnitPRBinding(ctx context.Context, unitID domain.WorkUnitID) (domain.WorkUnitPRBinding, error) {
	b, err := tx.scanWorkUnitPRBinding(tx.tx.QueryRowContext(ctx, getWorkUnitPRBindingSQL, unitID))
	if err != nil {
		return domain.WorkUnitPRBinding{}, fmt.Errorf("get work unit pr binding %s: %w", unitID, notFoundOr(err))
	}
	return b, nil
}

// AppendPullMergeFact appends one merge-state observation unless it repeats
// the latest recorded fact in everything but its instant (the domain's
// material-change rule), reporting whether a row was appended. The latest
// row is re-read inside this transaction, so concurrent capture passes
// serialize on SQLite's write lock rather than racing the comparison.
func (tx *InternalTx) AppendPullMergeFact(ctx context.Context, f domain.PullMergeFact) (bool, error) {
	body, err := encode(f)
	if err != nil {
		return false, fmt.Errorf("append pull merge fact: %w", err)
	}
	latest, err := tx.scanPullMergeFact(tx.tx.QueryRowContext(ctx, latestPullMergeFactSQL, f.RepositoryID, f.PRNumber))
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("append pull merge fact %s#%d: %w", f.Repo, f.PRNumber, err)
	default:
		if !f.MaterialChangeFrom(latest) {
			return false, nil
		}
	}
	if _, err := tx.tx.ExecContext(ctx, insertPullMergeFactSQL,
		f.RepositoryID, f.PRNumber, body, formatTime(f.ObservedAt)); err != nil {
		return false, fmt.Errorf("append pull merge fact %s#%d: %w", f.Repo, f.PRNumber, err)
	}
	return true, nil
}

// scanPullMergeFact reconstructs one merge-fact row.
func (tx *ReadTx) scanPullMergeFact(sc scanner) (domain.PullMergeFact, error) {
	var (
		repositoryID, prNumber int64
		observedAt             string
		body                   []byte
	)
	if err := sc.Scan(&repositoryID, &prNumber, &body, &observedAt); err != nil {
		return domain.PullMergeFact{}, err
	}
	f, err := decode[domain.PullMergeFact](body)
	if err != nil {
		return domain.PullMergeFact{}, err
	}
	if f.RepositoryID != repositoryID || int64(f.PRNumber) != prNumber ||
		formatTime(f.ObservedAt) != observedAt {
		return domain.PullMergeFact{}, errRowInconsistent
	}
	return f, nil
}

// LatestPullMergeFact returns the newest recorded merge state for one pull
// request, or ErrNotFound before any observation.
func (tx *ReadTx) LatestPullMergeFact(ctx context.Context, repositoryID int64, prNumber int) (domain.PullMergeFact, error) {
	f, err := tx.scanPullMergeFact(tx.tx.QueryRowContext(ctx, latestPullMergeFactSQL, repositoryID, prNumber))
	if err != nil {
		return domain.PullMergeFact{}, fmt.Errorf("latest pull merge fact %d#%d: %w", repositoryID, prNumber, notFoundOr(err))
	}
	return f, nil
}

// ListPullMergeFacts returns one pull request's recorded state timeline in
// append order.
func (tx *ReadTx) ListPullMergeFacts(ctx context.Context, repositoryID int64, prNumber int) ([]domain.PullMergeFact, error) {
	rows, err := tx.tx.QueryContext(ctx, listPullMergeFactsSQL, repositoryID, prNumber)
	if err != nil {
		return nil, fmt.Errorf("list pull merge facts %d#%d: %w", repositoryID, prNumber, err)
	}
	defer func() { _ = rows.Close() }()
	var facts []domain.PullMergeFact
	for rows.Next() {
		f, err := tx.scanPullMergeFact(rows)
		if err != nil {
			return nil, fmt.Errorf("list pull merge facts %d#%d: %w", repositoryID, prNumber, err)
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pull merge facts %d#%d: %w", repositoryID, prNumber, err)
	}
	return facts, nil
}

// AppendIssueStateFact appends one issue-state observation under the same
// material-change rule as AppendPullMergeFact.
func (tx *InternalTx) AppendIssueStateFact(ctx context.Context, f domain.IssueStateFact) (bool, error) {
	body, err := encode(f)
	if err != nil {
		return false, fmt.Errorf("append issue state fact: %w", err)
	}
	latest, err := tx.scanIssueStateFact(tx.tx.QueryRowContext(ctx, latestIssueStateFactSQL, f.RepositoryID, f.IssueNumber))
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("append issue state fact %s#%d: %w", f.Repo, f.IssueNumber, err)
	default:
		if !f.MaterialChangeFrom(latest) {
			return false, nil
		}
	}
	if _, err := tx.tx.ExecContext(ctx, insertIssueStateFactSQL,
		f.RepositoryID, f.IssueNumber, body, formatTime(f.ObservedAt)); err != nil {
		return false, fmt.Errorf("append issue state fact %s#%d: %w", f.Repo, f.IssueNumber, err)
	}
	return true, nil
}

// scanIssueStateFact reconstructs one issue-fact row.
func (tx *ReadTx) scanIssueStateFact(sc scanner) (domain.IssueStateFact, error) {
	var (
		repositoryID, issueNumber int64
		observedAt                string
		body                      []byte
	)
	if err := sc.Scan(&repositoryID, &issueNumber, &body, &observedAt); err != nil {
		return domain.IssueStateFact{}, err
	}
	f, err := decode[domain.IssueStateFact](body)
	if err != nil {
		return domain.IssueStateFact{}, err
	}
	if f.RepositoryID != repositoryID || int64(f.IssueNumber) != issueNumber ||
		formatTime(f.ObservedAt) != observedAt {
		return domain.IssueStateFact{}, errRowInconsistent
	}
	return f, nil
}

// LatestIssueStateFact returns the newest recorded state for one issue, or
// ErrNotFound before any observation.
func (tx *ReadTx) LatestIssueStateFact(ctx context.Context, repositoryID int64, issueNumber int) (domain.IssueStateFact, error) {
	f, err := tx.scanIssueStateFact(tx.tx.QueryRowContext(ctx, latestIssueStateFactSQL, repositoryID, issueNumber))
	if err != nil {
		return domain.IssueStateFact{}, fmt.Errorf("latest issue state fact %d#%d: %w", repositoryID, issueNumber, notFoundOr(err))
	}
	return f, nil
}

// ListIssueStateFacts returns one issue's recorded state timeline in append
// order.
func (tx *ReadTx) ListIssueStateFacts(ctx context.Context, repositoryID int64, issueNumber int) ([]domain.IssueStateFact, error) {
	rows, err := tx.tx.QueryContext(ctx, listIssueStateFactsSQL, repositoryID, issueNumber)
	if err != nil {
		return nil, fmt.Errorf("list issue state facts %d#%d: %w", repositoryID, issueNumber, err)
	}
	defer func() { _ = rows.Close() }()
	var facts []domain.IssueStateFact
	for rows.Next() {
		f, err := tx.scanIssueStateFact(rows)
		if err != nil {
			return nil, fmt.Errorf("list issue state facts %d#%d: %w", repositoryID, issueNumber, err)
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list issue state facts %d#%d: %w", repositoryID, issueNumber, err)
	}
	return facts, nil
}

// RecordWorkUnitCompletion records the unit-done fact, write-once: the domain
// evaluator derives the completion's instant from the satisfying
// observations, so a replay of the same facts converges byte-identically
// while any disagreeing completion claim is a conflict.
func (tx *InternalTx) RecordWorkUnitCompletion(ctx context.Context, c domain.WorkUnitCompletion) error {
	body, err := encode(c)
	if err != nil {
		return fmt.Errorf("put work unit completion: %w", err)
	}
	if err := tx.putImmutable(ctx, insertWorkUnitCompletionSQL,
		[]any{c.UnitID, body, formatTime(c.RecordedAt)},
		selectWorkUnitCompletionBodySQL, []any{c.UnitID}, body); err != nil {
		return fmt.Errorf("put work unit completion %s: %w", c.UnitID, err)
	}
	return nil
}

// scanWorkUnitCompletion reconstructs one completion row.
func (tx *ReadTx) scanWorkUnitCompletion(sc scanner) (domain.WorkUnitCompletion, error) {
	var (
		unitID, recordedAt string
		body               []byte
	)
	if err := sc.Scan(&unitID, &body, &recordedAt); err != nil {
		return domain.WorkUnitCompletion{}, err
	}
	c, err := decode[domain.WorkUnitCompletion](body)
	if err != nil {
		return domain.WorkUnitCompletion{}, err
	}
	if c.UnitID != domain.WorkUnitID(unitID) || formatTime(c.RecordedAt) != recordedAt {
		return domain.WorkUnitCompletion{}, errRowInconsistent
	}
	return c, nil
}

// errCompletionUnsupported is the completion re-gate's fail-closed verdict:
// the stored done bit is not derivable from the store's own evidence.
var errCompletionUnsupported = errors.New(
	"stored completion is not supported by the recorded declaration, binding, and facts")

// GetWorkUnitCompletion returns the unit's completion record, or
// ErrNotFound while the unit is incomplete. The stored row is a done bit,
// and a decoded trust bit is never trusted (the store's reconstruction-gate
// convention): the read re-runs the trusted evaluator over the stored
// declaration, binding, and fact timelines, and fails closed unless some
// recorded observation derives exactly the stored completion. Evaluating
// against the timelines rather than only the latest facts keeps a
// legitimately recorded completion readable after later observations (an
// issue reopened post-merge) move the latest state past the satisfying one.
func (tx *ReadTx) GetWorkUnitCompletion(ctx context.Context, unitID domain.WorkUnitID) (domain.WorkUnitCompletion, error) {
	c, err := tx.scanWorkUnitCompletion(tx.tx.QueryRowContext(ctx, getWorkUnitCompletionSQL, unitID))
	if err != nil {
		return domain.WorkUnitCompletion{}, fmt.Errorf("get work unit completion %s: %w", unitID, notFoundOr(err))
	}
	supported, err := tx.completionSupported(ctx, c)
	if err != nil {
		return domain.WorkUnitCompletion{}, fmt.Errorf("get work unit completion %s: %w", unitID, err)
	}
	if !supported {
		return domain.WorkUnitCompletion{}, fmt.Errorf("get work unit completion %s: %w", unitID, errCompletionUnsupported)
	}
	return c, nil
}

// completionSupported re-derives the completion from stored inputs. A
// missing declaration or binding is unsupported, not an error: the foreign
// keys make that state unreachable by writes, so reaching it is exactly the
// corruption the re-gate exists to refuse.
func (tx *ReadTx) completionSupported(ctx context.Context, c domain.WorkUnitCompletion) (bool, error) {
	declaration, err := tx.GetWorkUnitDeclaration(ctx, c.UnitID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	binding, err := tx.GetWorkUnitPRBinding(ctx, c.UnitID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pulls, err := tx.ListPullMergeFacts(ctx, binding.RepositoryID, binding.PRNumber)
	if err != nil {
		return false, err
	}
	var issues []domain.IssueStateFact
	if declaration.BoundIssue != nil {
		issues, err = tx.ListIssueStateFacts(ctx, binding.RepositoryID, *declaration.BoundIssue)
		if err != nil {
			return false, err
		}
	}
	for _, pull := range pulls {
		if derived, ok := domain.EvaluateWorkUnitCompletion(declaration, binding, pull, nil); ok && derived.Equal(c) {
			return true, nil
		}
		for i := range issues {
			if derived, ok := domain.EvaluateWorkUnitCompletion(declaration, binding, pull, &issues[i]); ok && derived.Equal(c) {
				return true, nil
			}
		}
	}
	return false, nil
}
