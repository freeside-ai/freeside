package publish

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// MintRecord is the per-mint audit row (issue #80 acceptance 3; plan
// §8's typed-observability discipline). It deliberately has no token
// field: like the store's device_credentials shape, the secret is
// unrepresentable in the audited value, so no audit read path can leak
// it. Requested and Granted both persist — an audit trail that shows
// only what was asked for would go silently stale if GitHub ever
// narrows a grant.
type MintRecord struct {
	MintedAt       time.Time   `json:"minted_at"`
	RegistrationID int64       `json:"registration_id"`
	InstallationID int64       `json:"installation_id"`
	RepositoryID   int64       `json:"repository_id"`
	Repo           string      `json:"repo"`
	Requested      Permissions `json:"requested"`
	Granted        Permissions `json:"granted"`
	ExpiresAt      time.Time   `json:"expires_at"`
}

// Recorder receives one record per successful mint. Minting fails when
// recording fails: an unauditable token must not circulate.
type Recorder interface {
	RecordMint(MintRecord) error
}

// InstallationMintOutcome is the closed audit vocabulary for how a minted
// installation token's returned grant was judged. A row is written for every
// token GitHub actually created, not only the clean ones, so this records why
// a given credential is or is not accounted for. The zero value "" is invalid
// by design.
type InstallationMintOutcome string

const (
	// InstallationMintValidated: the returned grant matched the request and
	// the expiry was valid; this is the credential the janitor then used.
	InstallationMintValidated InstallationMintOutcome = "validated"
	// InstallationMintGrantRejected: GitHub returned a token whose permission
	// scopes or repository selection differed from the request.
	InstallationMintGrantRejected InstallationMintOutcome = "grant_rejected"
	// InstallationMintExpiryRejected: the grant matched the request but its
	// expiry was missing, malformed, lapsed, or over-long.
	InstallationMintExpiryRejected InstallationMintOutcome = "expiry_rejected"
	// InstallationMintUndecodable: GitHub returned a token in a 201 whose body
	// could not be decoded, so nothing else about the grant is trustworthy.
	InstallationMintUndecodable InstallationMintOutcome = "undecodable"
)

// AllInstallationMintOutcomes is the single registration point for valid mint
// outcomes.
var AllInstallationMintOutcomes = []InstallationMintOutcome{
	InstallationMintValidated,
	InstallationMintGrantRejected,
	InstallationMintExpiryRejected,
	InstallationMintUndecodable,
}

func (o InstallationMintOutcome) valid() bool {
	switch o {
	case InstallationMintValidated,
		InstallationMintGrantRejected,
		InstallationMintExpiryRejected,
		InstallationMintUndecodable:
		return true
	default:
		return false
	}
}

// InstallationMintRecord is the per-mint audit row for an installation-scope
// grant-read token (issue #545): the installation janitor mints one every pass
// to enumerate an installation's repository grants. It is the installation
// sibling of MintRecord and carries no RepositoryID/Repo — the mint is
// installation-wide, with no single repository — and, like MintRecord, no
// token field: the secret is unrepresentable in the audited value.
//
// A record is written for every token GitHub minted, so a token whose revoke
// later fails is never left off the ledger. Outcome carries the verdict.
// Requested is always the fixed grant-read request; Granted persists only for
// a validated grant (the daemon does not vouch for a grant it rejected) and is
// otherwise the zero Permissions. ExpiresAt is nil when the expiry was not
// validated, so the audit never fabricates an instant that never held.
type InstallationMintRecord struct {
	MintedAt       time.Time               `json:"minted_at"`
	RegistrationID int64                   `json:"registration_id"`
	InstallationID int64                   `json:"installation_id"`
	Outcome        InstallationMintOutcome `json:"outcome"`
	Requested      Permissions             `json:"requested"`
	Granted        Permissions             `json:"granted"`
	ExpiresAt      *time.Time              `json:"expires_at,omitempty"`
}

// InstallationMintRecorder receives one record per successful installation-scope
// mint. It is the janitor's mint-audit port, distinct from the JanitorRecorder
// that commits the destructive-action barrier: the two record different events
// to different durable surfaces (this ledger is the SQLite audit table, the
// destructive barrier is the file journal). Recording fails the mint, so an
// unauditable installation token never circulates. StoreRecorder implements it,
// which is how the janitor's mint routes through the store-backed recorder.
type InstallationMintRecorder interface {
	RecordInstallationMint(InstallationMintRecord) error
}

// StoreRecorder lands each mint on the store-owned SQLite audit
// surface (plan §5.9; issue #107). The enclosing transaction's commit
// is the durability barrier the retired 1A JSONL substrate provided
// with fsync: a
// record that fails to commit fails the mint, so an unauditable token
// never circulates.
type StoreRecorder struct {
	store *store.Store
}

// NewStoreRecorder wires the recorder to an open store; a nil store
// fails closed at construction rather than at the first mint.
func NewStoreRecorder(s *store.Store) (*StoreRecorder, error) {
	if s == nil {
		return nil, errors.New("audit: nil store")
	}
	return &StoreRecorder{store: s}, nil
}

// RecordMint commits the record in its own internal transaction (audit
// is daemon bookkeeping, invisible to client sync). It deliberately
// runs under context.Background() rather than a caller context: the
// Recorder interface carries no context by design, and a request-scoped
// cancellation mid-commit would fail mints on a deadline that has
// nothing to do with audit durability; the local SQLite write either
// commits or the mint fails.
func (r *StoreRecorder) RecordMint(rec MintRecord) error {
	ctx := context.Background()
	err := r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordMintAudit(ctx, store.MintAudit{
			MintedAt:                rec.MintedAt,
			RegistrationID:          rec.RegistrationID,
			InstallationID:          rec.InstallationID,
			RepositoryID:            rec.RepositoryID,
			Repo:                    rec.Repo,
			RequestedActions:        rec.Requested.Actions,
			RequestedAdministration: rec.Requested.Administration,
			RequestedContents:       rec.Requested.Contents,
			RequestedEnvironments:   rec.Requested.Environments,
			RequestedPullRequests:   rec.Requested.PullRequests,
			RequestedMetadata:       rec.Requested.Metadata,
			GrantedActions:          rec.Granted.Actions,
			GrantedAdministration:   rec.Granted.Administration,
			GrantedContents:         rec.Granted.Contents,
			GrantedEnvironments:     rec.Granted.Environments,
			GrantedPullRequests:     rec.Granted.PullRequests,
			GrantedMetadata:         rec.Granted.Metadata,
			ExpiresAt:               rec.ExpiresAt,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("audit: record mint: %w", err)
	}
	return nil
}

// RecordInstallationMint commits the installation-scope mint record in its own
// internal transaction, on the same terms as RecordMint: audit is daemon
// bookkeeping invisible to sync, the commit is the durability barrier, and a
// record that fails to commit fails the mint so an unauditable installation
// token never circulates. It runs under context.Background() for the same
// reason RecordMint does.
func (r *StoreRecorder) RecordInstallationMint(rec InstallationMintRecord) error {
	// Fail closed on an unclassified mint: the store column is a plain string,
	// so the enum's validity is enforced here, at the boundary that owns it.
	if !rec.Outcome.valid() {
		return fmt.Errorf("audit: record installation mint: invalid outcome %q", rec.Outcome)
	}
	ctx := context.Background()
	err := r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordInstallationMint(ctx, store.InstallationMintAudit{
			MintedAt:                rec.MintedAt,
			RegistrationID:          rec.RegistrationID,
			InstallationID:          rec.InstallationID,
			Outcome:                 string(rec.Outcome),
			RequestedActions:        rec.Requested.Actions,
			RequestedAdministration: rec.Requested.Administration,
			RequestedContents:       rec.Requested.Contents,
			RequestedEnvironments:   rec.Requested.Environments,
			RequestedPullRequests:   rec.Requested.PullRequests,
			RequestedMetadata:       rec.Requested.Metadata,
			GrantedActions:          rec.Granted.Actions,
			GrantedAdministration:   rec.Granted.Administration,
			GrantedContents:         rec.Granted.Contents,
			GrantedEnvironments:     rec.Granted.Environments,
			GrantedPullRequests:     rec.Granted.PullRequests,
			GrantedMetadata:         rec.Granted.Metadata,
			ExpiresAt:               rec.ExpiresAt,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("audit: record installation mint: %w", err)
	}
	return nil
}
