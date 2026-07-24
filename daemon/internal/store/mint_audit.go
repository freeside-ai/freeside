package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MintAudit is one publish-lane installation-token mint (plan §5.9 audit,
// issue #107): the store-side row for publish's MintRecord, kept as flat
// typed fields so the store needs no knowledge of publish types. It
// deliberately has no token field — the secret is unrepresentable in the
// audited value, so no audit read path can leak it. Requested and granted
// scopes both persist; an empty scope string means "not requested".
//
// Mint audit is daemon-internal bookkeeping like the inbox/outbox queues:
// never exposed through synchronization, so rows carry no as_of_revision
// and the write method lives on InternalTx with a non-Put name (the #38
// invariant: every Put* is a synchronized write on WriteTx). Rows are
// insert-only with no idempotency key: two identical mints are two real
// events.
type MintAudit struct {
	ID                      int64
	MintedAt                time.Time
	RegistrationID          int64
	InstallationID          int64
	RepositoryID            int64
	Repo                    string
	RequestedActions        string
	RequestedAdministration string
	RequestedContents       string
	RequestedEnvironments   string
	RequestedPullRequests   string
	RequestedMetadata       string
	GrantedActions          string
	GrantedAdministration   string
	GrantedContents         string
	GrantedEnvironments     string
	GrantedPullRequests     string
	GrantedMetadata         string
	ExpiresAt               time.Time
}

const (
	recordMintAuditSQL = `
INSERT INTO publish_mint_audits (
    minted_at, registration_id, installation_id, repository_id, repo,
    requested_contents, requested_pull_requests, requested_metadata,
    granted_contents, granted_pull_requests, granted_metadata,
    requested_actions, requested_administration, requested_environments,
    granted_actions, granted_administration, granted_environments,
    expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	listMintAuditsSQL = `
SELECT id, minted_at, registration_id, installation_id, repository_id, repo,
    requested_contents, requested_pull_requests, requested_metadata,
    granted_contents, granted_pull_requests, granted_metadata,
    requested_actions, requested_administration, requested_environments,
    granted_actions, granted_administration, granted_environments,
    expires_at
FROM publish_mint_audits ORDER BY id`
)

// RecordMintAudit appends one mint to the audit ledger and returns the row
// with its assigned ID. Call it inside the transaction whose commit the
// mint's success depends on: a mint whose audit write fails must itself
// fail (#80's invariant), and the commit is the durability barrier.
func (tx *InternalTx) RecordMintAudit(ctx context.Context, rec MintAudit) (MintAudit, error) {
	// The schema CHECKs mirror these, but failing here names the problem
	// instead of surfacing a constraint error. Zero times are rejected
	// because an audit row without a real mint or expiry instant records
	// an event that cannot have happened.
	if rec.Repo == "" {
		return MintAudit{}, errors.New("record mint audit: empty repo")
	}
	if rec.RegistrationID <= 0 {
		return MintAudit{}, fmt.Errorf("record mint audit %q: registration id %d is not positive",
			rec.Repo, rec.RegistrationID)
	}
	if rec.InstallationID <= 0 {
		return MintAudit{}, fmt.Errorf("record mint audit %q: installation id %d is not positive",
			rec.Repo, rec.InstallationID)
	}
	if rec.RepositoryID <= 0 {
		return MintAudit{}, fmt.Errorf("record mint audit %q: repository id %d is not positive",
			rec.Repo, rec.RepositoryID)
	}
	if rec.MintedAt.IsZero() || rec.ExpiresAt.IsZero() {
		return MintAudit{}, fmt.Errorf("record mint audit %q: zero mint or expiry time", rec.Repo)
	}
	rec.MintedAt = rec.MintedAt.UTC()
	rec.ExpiresAt = rec.ExpiresAt.UTC()
	res, err := tx.tx.ExecContext(ctx, recordMintAuditSQL,
		formatTime(rec.MintedAt), rec.RegistrationID, rec.InstallationID, rec.RepositoryID, rec.Repo,
		rec.RequestedContents, rec.RequestedPullRequests, rec.RequestedMetadata,
		rec.GrantedContents, rec.GrantedPullRequests, rec.GrantedMetadata,
		rec.RequestedActions, rec.RequestedAdministration, rec.RequestedEnvironments,
		rec.GrantedActions, rec.GrantedAdministration, rec.GrantedEnvironments,
		formatTime(rec.ExpiresAt))
	if err != nil {
		return MintAudit{}, fmt.Errorf("record mint audit %q: %w", rec.Repo, err)
	}
	rec.ID, err = res.LastInsertId()
	if err != nil {
		return MintAudit{}, fmt.Errorf("record mint audit %q: %w", rec.Repo, err)
	}
	return rec, nil
}

// ListMintAudits returns every recorded mint in insertion order, for
// inspection surfaces and tests.
func (tx *ReadTx) ListMintAudits(ctx context.Context) ([]MintAudit, error) {
	rows, err := tx.tx.QueryContext(ctx, listMintAuditsSQL)
	if err != nil {
		return nil, fmt.Errorf("list mint audits: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var audits []MintAudit
	for rows.Next() {
		var (
			rec       MintAudit
			mintedAt  string
			expiresAt string
		)
		if err := rows.Scan(&rec.ID, &mintedAt, &rec.RegistrationID, &rec.InstallationID, &rec.RepositoryID, &rec.Repo,
			&rec.RequestedContents, &rec.RequestedPullRequests, &rec.RequestedMetadata,
			&rec.GrantedContents, &rec.GrantedPullRequests, &rec.GrantedMetadata,
			&rec.RequestedActions, &rec.RequestedAdministration, &rec.RequestedEnvironments,
			&rec.GrantedActions, &rec.GrantedAdministration, &rec.GrantedEnvironments,
			&expiresAt); err != nil {
			return nil, fmt.Errorf("list mint audits: %w", err)
		}
		rec.MintedAt, err = time.Parse(time.RFC3339Nano, mintedAt)
		if err != nil {
			return nil, fmt.Errorf("list mint audits: stored minted_at invalid: %w", err)
		}
		rec.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
		if err != nil {
			return nil, fmt.Errorf("list mint audits: stored expires_at invalid: %w", err)
		}
		audits = append(audits, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mint audits: %w", err)
	}
	return audits, nil
}
