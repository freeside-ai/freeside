package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// agentClaimsRecord is the persisted shape of an invocation's agent claims: the
// claim set plus the invocation it belongs to. The embedded InvocationID is the
// authentication key the readback cross-checks against the row's primary key, so
// a body-only tamper that rebinds the claims to a different invocation fails
// closed like every other reconstruction boundary (entities.go).
//
// It is package-private: a persistence format, not one of the contract shapes
// the goldens pin. Claims marshal through domain.AgentClaim's wire shape, which
// carries no publish_eligible field, so no decoded trust bit exists to honor.
type agentClaimsRecord struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	Claims       []domain.AgentClaim `json:"claims"`
}

// Validate is the encode/decode gate: it demands an invocation key, a non-empty
// claim set, and a well-formed claim at every position. AgentClaim.Validate
// pins the agent producer class and the text/digest binding, so a tampered
// producer_class or an unbound text claim is refused on read (the fail-closed
// reconstruction contract). An empty set is invalid: the driver early-returns
// before writing one (#381), so a persisted empty record has no writer and no
// consumer, and rejecting it keeps the tamper surface closed.
func (r agentClaimsRecord) Validate() error {
	if r.InvocationID == "" {
		return fmt.Errorf("agent claims invocation_id: %w", domain.ErrEmptyID)
	}
	if len(r.Claims) == 0 {
		return fmt.Errorf("agent claims %q: %w", r.InvocationID, domain.ErrEmptyField)
	}
	for i, claim := range r.Claims {
		if err := claim.Validate(); err != nil {
			return fmt.Errorf("agent claims %q claim %d: %w", r.InvocationID, i, err)
		}
	}
	return nil
}

const putAgentClaimsSQL = `
INSERT INTO agent_claims (invocation_id, entity_version, as_of_revision, body)
VALUES (?, 1, ?, ?)
ON CONFLICT (invocation_id) DO NOTHING`

// PutAgentClaims records an invocation's claim set write-once: a byte-identical
// replay converges on the existing row, and any differing set (label, digest,
// membership, text, order) surfaces as ErrImmutableConflict, since a claim
// set's canonical encoding is order-sensitive by identity (#381). The
// invocation_id foreign key requires the invocation row to exist first.
func (tx *WriteTx) PutAgentClaims(ctx context.Context, id domain.InvocationID, claims []domain.AgentClaim) error {
	body, err := encode(agentClaimsRecord{InvocationID: id, Claims: claims})
	if err != nil {
		return fmt.Errorf("put agent claims %q: %w", id, err)
	}
	if err := tx.putImmutable(ctx, putAgentClaimsSQL,
		[]any{id, tx.asOfRevision, body},
		`SELECT body FROM agent_claims WHERE invocation_id = ?`, []any{id}, body); err != nil {
		return fmt.Errorf("put agent claims %q: %w", id, err)
	}
	return nil
}

// GetAgentClaims reconstructs an invocation's claim set. decode re-validates the
// body (agentClaimsRecord.Validate is the fail-closed reconstruction gate), and
// the record's invocation_id is cross-checked against the queried key so a row
// whose body disagrees with its primary key is refused as inconsistent.
func (tx *ReadTx) GetAgentClaims(ctx context.Context, id domain.InvocationID) ([]domain.AgentClaim, error) {
	var body []byte
	err := tx.tx.QueryRowContext(ctx,
		`SELECT body FROM agent_claims WHERE invocation_id = ?`, id).Scan(&body)
	if err != nil {
		return nil, fmt.Errorf("get agent claims %q: %w", id, notFoundOr(err))
	}
	record, err := decode[agentClaimsRecord](body)
	if err != nil {
		return nil, fmt.Errorf("get agent claims %q: %w", id, err)
	}
	if record.InvocationID != id {
		return nil, fmt.Errorf("get agent claims %q: %w", id, errRowInconsistent)
	}
	return record.Claims, nil
}
