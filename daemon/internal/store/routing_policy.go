package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	insertRoutingPolicySQL = `
INSERT INTO routing_policies (revision, source, set_by, set_at, lineup_revision, drain_first_account, body)
VALUES (?, ?, ?, ?, ?, ?, ?)`
)

// AppendRoutingPolicy records the next routing policy revision. The store
// assigns the revision here, so a change can only supersede the last one. It
// is on WriteTx because the policy is client-visible state from #1719 on, and
// such a change must ride a revision bump.
func (tx *WriteTx) AppendRoutingPolicy(
	ctx context.Context, in domain.RoutingPolicyInput, setAt time.Time,
) (domain.RoutingPolicy, error) {
	var last int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(revision), 0) FROM routing_policies`).Scan(&last); err != nil {
		return domain.RoutingPolicy{}, fmt.Errorf("append routing policy: %w", err)
	}
	policy, err := domain.NewRoutingPolicy(in, last+1, setAt)
	if err != nil {
		return domain.RoutingPolicy{}, fmt.Errorf("append routing policy: %w", err)
	}
	body, err := encode(policy)
	if err != nil {
		return domain.RoutingPolicy{}, fmt.Errorf("append routing policy: %w", err)
	}
	var drainFirst sql.NullString
	if policy.DrainFirstAccount != nil {
		drainFirst = sql.NullString{String: string(*policy.DrainFirstAccount), Valid: true}
	}
	if _, err := tx.tx.ExecContext(ctx, insertRoutingPolicySQL, policy.Revision, policy.Source, policy.SetBy,
		formatTime(policy.SetAt), policy.LineupRevision, drainFirst, body); err != nil {
		return domain.RoutingPolicy{}, fmt.Errorf("append routing policy: %w", err)
	}
	return policy, nil
}

// CurrentRoutingPolicy reconstructs the newest revision. ErrNotFound means
// none was ever set, so nothing routes and admission keeps the lineup.
func (tx *ReadTx) CurrentRoutingPolicy(ctx context.Context) (domain.RoutingPolicy, error) {
	policy, err := scanRoutingPolicy(tx.tx.QueryRowContext(ctx,
		`SELECT body FROM routing_policies ORDER BY revision DESC LIMIT 1`))
	if err != nil {
		return domain.RoutingPolicy{}, fmt.Errorf("current routing policy: %w", notFoundOr(err))
	}
	return policy, nil
}

// RoutingPolicyRevisions reconstructs every revision in order; one
// row that fails its decode fails the whole listing. None set is an empty listing.
func (tx *ReadTx) RoutingPolicyRevisions(ctx context.Context) ([]domain.RoutingPolicy, error) {
	policies, err := queryAll(ctx, tx.tx, scanRoutingPolicy,
		`SELECT body FROM routing_policies ORDER BY revision`)
	if err != nil {
		return nil, fmt.Errorf("list routing policies: %w", err)
	}
	if policies == nil {
		return []domain.RoutingPolicy{}, nil
	}
	return policies, nil
}

// scanRoutingPolicy decodes one row's body, which Validate checks on decode.
// The row is ordinary state (Tier B): admission reads a revision's body, never
// its columns, so the columns, which order and number the revisions, are not
// compared with it.
func scanRoutingPolicy(row scanner) (domain.RoutingPolicy, error) {
	var body []byte
	if err := row.Scan(&body); err != nil {
		return domain.RoutingPolicy{}, err
	}
	return decode[domain.RoutingPolicy](body)
}
