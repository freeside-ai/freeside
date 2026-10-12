package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// PutReviewAdmission records the snapshot a review invocation was admitted
// under. The row is immutable: a caller resuming an invocation reads the
// first snapshot and never writes another, so a second, different snapshot
// for one invocation is a conflict, not an update.
func (tx *WriteTx) PutReviewAdmission(ctx context.Context, a domain.ReviewAdmission) error {
	if err := a.Validate(); err != nil {
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
// are checked against it, so a row edited in place does not read back.
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
	return a, nil
}
