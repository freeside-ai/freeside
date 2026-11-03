package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// PutReviewAdmission records the snapshot a review invocation was admitted
// under. The row is immutable: a caller resuming an invocation reads the
// first snapshot and never writes another, so a second, different snapshot
// for one invocation is a conflict, not an update. A snapshot that cites a
// task line is held to a recorded reviewer line of its run's task.
func (tx *WriteTx) PutReviewAdmission(ctx context.Context, a domain.ReviewAdmission) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if err := tx.gateReviewTaskLine(ctx, a); err != nil {
		return err
	}
	body, err := encode(a)
	if err != nil {
		return err
	}
	return tx.putImmutable(ctx, `INSERT INTO review_admissions
		(invocation_id, run_id, role, agent_digest, admitted_at, body_digest, body)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (invocation_id) DO NOTHING`,
		[]any{
			a.InvocationID, a.RunID, a.Role, a.AgentBinding.AgentDigest,
			formatTime(a.AdmittedAt), reviewBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM review_admissions WHERE invocation_id = ?`,
		[]any{a.InvocationID}, reviewBodyAuthority(body))
}

// GetReviewAdmission returns the snapshot an invocation was admitted under,
// or ErrNotFound for an invocation requested before admission was recorded.
// The decoded body is the authority: the copied columns and the body digest
// are checked against it, so a row edited in place does not read back, and a
// cited task line is re-gated as at write.
func (tx *ReadTx) GetReviewAdmission(ctx context.Context, id domain.InvocationID) (domain.ReviewAdmission, error) {
	var runID, role, agent, admitted, digest string
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT run_id, role, agent_digest, admitted_at, body_digest, body
		FROM review_admissions WHERE invocation_id = ?`, id).Scan(&runID, &role, &agent, &admitted, &digest, &body)
	if err != nil {
		return domain.ReviewAdmission{}, notFoundOr(err)
	}
	var a domain.ReviewAdmission
	if err := json.Unmarshal(body, &a); err != nil {
		return a, err
	}
	if err := a.Validate(); err != nil {
		return a, err
	}
	if digest != reviewBodyDigest(string(body)) || a.InvocationID != id || string(a.RunID) != runID ||
		string(a.Role) != role || string(a.AgentBinding.AgentDigest) != agent ||
		formatTime(a.AdmittedAt) != admitted {
		return a, fmt.Errorf("review admission %q: %w", id, errRowInconsistent)
	}
	if err := tx.gateReviewTaskLine(ctx, a); err != nil {
		return a, err
	}
	return a, nil
}

// ListRunReviewAdmissions returns every review snapshot recorded for a run,
// routed and shadow, in admission order (ties by invocation). Each row is
// read back through GetReviewAdmission, so a listed snapshot passes the same
// body, column, and task-line checks as a keyed read. The order is taken
// from the decoded times: the admitted_at column trims trailing zeros, so it
// does not sort lexically in time order.
func (tx *ReadTx) ListRunReviewAdmissions(ctx context.Context, runID domain.RunID) ([]domain.ReviewAdmission, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT invocation_id FROM review_admissions
		WHERE run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("list review admissions for run %q: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports any deferred-close failure
	var ids []domain.InvocationID
	for rows.Next() {
		var id domain.InvocationID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list review admissions for run %q: %w", runID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list review admissions for run %q: %w", runID, err)
	}
	admissions := make([]domain.ReviewAdmission, 0, len(ids))
	for _, id := range ids {
		admission, err := tx.GetReviewAdmission(ctx, id)
		if err != nil {
			return nil, err
		}
		admissions = append(admissions, admission)
	}
	slices.SortFunc(admissions, func(a, b domain.ReviewAdmission) int {
		return cmp.Or(a.AdmittedAt.Compare(b.AdmittedAt), cmp.Compare(a.InvocationID, b.InvocationID))
	})
	return admissions, nil
}

// gateReviewTaskLine holds a snapshot's cited task line to a recorded line of
// its run's task, as an execution admission's is, and to the role the
// snapshot admits: a writer's line chose no reviewer.
func (tx *ReadTx) gateReviewTaskLine(ctx context.Context, a domain.ReviewAdmission) error {
	line, err := tx.gateTaskLineSelection(ctx, a.InvocationID, a.RunID, a.AgentBinding)
	if err != nil {
		return err
	}
	if line.ID != "" && line.Role != a.Role {
		return fmt.Errorf("review admission %q cites task line %q for role %s: %w",
			a.InvocationID, line.ID, line.Role, domain.ErrAdmissionDerivationMismatch)
	}
	return nil
}
