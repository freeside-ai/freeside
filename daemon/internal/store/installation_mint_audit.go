package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// InstallationMintAudit is one publish-lane installation-scope token mint
// (plan §5.9 audit, issue #545): the store-side row for publish's
// InstallationMintRecord, kept as flat typed fields so the store needs no
// knowledge of publish types. It is the installation-scope sibling of
// MintAudit: the janitor mints an installation-wide grant-read token to
// enumerate repository grants, which has no single repository, so this row
// carries no repo/repository_id. Like MintAudit it has no token field — the
// secret is unrepresentable in the audited value, so no audit read path can
// leak it.
//
// A row is written for every token GitHub actually minted, not only the
// validated ones, so a minted token whose revoke later fails is never left
// unrecorded. Outcome carries the post-mint validation verdict (a non-empty
// value supplied by publish). Requested scopes are always the fixed grant-read
// request; granted scopes persist only when the grant was validated and are
// otherwise empty. ExpiresAt is nil (stored NULL) when the mint's expiry was
// not validated, because an audit row must never fabricate an instant that
// never held.
//
// Mint audit is daemon-internal bookkeeping like the inbox/outbox queues:
// never exposed through synchronization, so rows carry no as_of_revision and
// the write method lives on InternalTx with a non-Put name (the #38
// invariant: every Put* is a synchronized write on WriteTx). Rows are
// insert-only with no idempotency key: two identical mints are two real
// events.
type InstallationMintAudit struct {
	ID                      int64
	MintedAt                time.Time
	RegistrationID          int64
	InstallationID          int64
	Outcome                 string
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
	// ExpiresAt is nil when the mint's expiry was not validated (a rejected or
	// undecodable grant); a validated mint always carries it.
	ExpiresAt *time.Time
}

const (
	recordInstallationMintAuditSQL = `
INSERT INTO publish_installation_mint_audits (
    minted_at, registration_id, installation_id, outcome,
    requested_contents, requested_pull_requests, requested_metadata,
    granted_contents, granted_pull_requests, granted_metadata,
    requested_actions, requested_administration, requested_environments,
    granted_actions, granted_administration, granted_environments,
    expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	listInstallationMintAuditsSQL = `
SELECT id, minted_at, registration_id, installation_id, outcome,
    requested_contents, requested_pull_requests, requested_metadata,
    granted_contents, granted_pull_requests, granted_metadata,
    requested_actions, requested_administration, requested_environments,
    granted_actions, granted_administration, granted_environments,
    expires_at
FROM publish_installation_mint_audits ORDER BY id`
)

// RecordInstallationMint appends one installation-scope mint to the audit
// ledger and returns the row with its assigned ID. Call it inside the
// transaction whose commit the mint's success depends on: a mint whose audit
// write fails must itself fail (#545's invariant, mirroring #80's for the
// worker mint), and the commit is the durability barrier.
func (tx *InternalTx) RecordInstallationMint(ctx context.Context, rec InstallationMintAudit) (InstallationMintAudit, error) {
	// The schema CHECKs mirror these, but failing here names the problem
	// instead of surfacing a constraint error. A zero mint time records an
	// event that cannot have happened; a present-but-zero expiry is the same
	// malformed row, while a nil expiry is the valid "not validated" case.
	if rec.RegistrationID <= 0 {
		return InstallationMintAudit{}, fmt.Errorf(
			"record installation mint audit: registration id %d is not positive", rec.RegistrationID)
	}
	if rec.InstallationID <= 0 {
		return InstallationMintAudit{}, fmt.Errorf(
			"record installation mint audit: installation id %d is not positive", rec.InstallationID)
	}
	if rec.Outcome == "" {
		return InstallationMintAudit{}, errors.New("record installation mint audit: empty outcome")
	}
	if rec.MintedAt.IsZero() {
		return InstallationMintAudit{}, errors.New("record installation mint audit: zero mint time")
	}
	if rec.ExpiresAt != nil && rec.ExpiresAt.IsZero() {
		return InstallationMintAudit{}, errors.New("record installation mint audit: present but zero expiry time")
	}
	rec.MintedAt = rec.MintedAt.UTC()
	var expiresAt any
	if rec.ExpiresAt != nil {
		utc := rec.ExpiresAt.UTC()
		rec.ExpiresAt = &utc
		expiresAt = formatTime(utc)
	}
	res, err := tx.tx.ExecContext(ctx, recordInstallationMintAuditSQL,
		formatTime(rec.MintedAt), rec.RegistrationID, rec.InstallationID, rec.Outcome,
		rec.RequestedContents, rec.RequestedPullRequests, rec.RequestedMetadata,
		rec.GrantedContents, rec.GrantedPullRequests, rec.GrantedMetadata,
		rec.RequestedActions, rec.RequestedAdministration, rec.RequestedEnvironments,
		rec.GrantedActions, rec.GrantedAdministration, rec.GrantedEnvironments,
		expiresAt)
	if err != nil {
		return InstallationMintAudit{}, fmt.Errorf("record installation mint audit: %w", err)
	}
	rec.ID, err = res.LastInsertId()
	if err != nil {
		return InstallationMintAudit{}, fmt.Errorf("record installation mint audit: %w", err)
	}
	return rec, nil
}

// ListInstallationMintAudits returns every recorded installation-scope mint in
// insertion order, for inspection surfaces and tests.
func (tx *ReadTx) ListInstallationMintAudits(ctx context.Context) ([]InstallationMintAudit, error) {
	rows, err := tx.tx.QueryContext(ctx, listInstallationMintAuditsSQL)
	if err != nil {
		return nil, fmt.Errorf("list installation mint audits: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var audits []InstallationMintAudit
	for rows.Next() {
		var (
			rec       InstallationMintAudit
			mintedAt  string
			expiresAt sql.NullString
		)
		if err := rows.Scan(&rec.ID, &mintedAt, &rec.RegistrationID, &rec.InstallationID, &rec.Outcome,
			&rec.RequestedContents, &rec.RequestedPullRequests, &rec.RequestedMetadata,
			&rec.GrantedContents, &rec.GrantedPullRequests, &rec.GrantedMetadata,
			&rec.RequestedActions, &rec.RequestedAdministration, &rec.RequestedEnvironments,
			&rec.GrantedActions, &rec.GrantedAdministration, &rec.GrantedEnvironments,
			&expiresAt); err != nil {
			return nil, fmt.Errorf("list installation mint audits: %w", err)
		}
		rec.MintedAt, err = parseTime(mintedAt)
		if err != nil {
			return nil, fmt.Errorf("list installation mint audits: stored minted_at invalid: %w", err)
		}
		if expiresAt.Valid {
			parsed, err := parseTime(expiresAt.String)
			if err != nil {
				return nil, fmt.Errorf("list installation mint audits: stored expires_at invalid: %w", err)
			}
			rec.ExpiresAt = &parsed
		}
		audits = append(audits, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list installation mint audits: %w", err)
	}
	return audits, nil
}
