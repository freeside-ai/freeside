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
