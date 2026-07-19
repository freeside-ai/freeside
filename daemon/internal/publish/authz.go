package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// AuthorizationSource resolves the daemon-authored candidate authorization
// (#172, plan §5.6) the publication gate consumes. Like TrustSource it is a
// store-backed port, so the Publisher stays decoupled from the store and unit
// tests fake it: the gate re-reads through it on every Publish so an
// authorization recorded (or a candidate re-authorized) since the last attempt
// is observed at the decision point. A missing record is reported as
// found=false, not as an error, since absence is the gate's fail-closed case;
// err carries only real store failures.
type AuthorizationSource interface {
	Authorization(ctx context.Context, id domain.Digest) (domain.CandidateAuthorization, bool, error)
}

// StoreAuthorizationSource is the store-backed AuthorizationSource, mirroring
// StoreTrustSource: it reconstructs one authorization by its content id in its
// own read transaction. store.GetCandidateAuthorization re-runs Validate on
// decode — recomputing the id and the authorizes_publication bit from the
// body's own bound facts — so a tampered row never reaches the gate as a
// trusted value.
type StoreAuthorizationSource struct {
	store *store.Store
}

// NewStoreAuthorizationSource wires the source to an open store; a nil store
// fails closed at construction rather than at the first read.
func NewStoreAuthorizationSource(s *store.Store) (*StoreAuthorizationSource, error) {
	if s == nil {
		return nil, errors.New("authorization source: nil store")
	}
	return &StoreAuthorizationSource{store: s}, nil
}

// Authorization returns the authorization recorded under id. A row absent from
// the store yields found=false rather than an error: absence is where the gate
// fails closed, so the read itself reports only real store failures.
func (a *StoreAuthorizationSource) Authorization(ctx context.Context, id domain.Digest) (domain.CandidateAuthorization, bool, error) {
	var auth domain.CandidateAuthorization
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetCandidateAuthorization(ctx, id)
		if err != nil {
			return err
		}
		auth = got
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.CandidateAuthorization{}, false, nil
	}
	if err != nil {
		return domain.CandidateAuthorization{}, false, fmt.Errorf("authorization %q: %w", id, err)
	}
	return auth, true, nil
}

// StoreAuthorizationSource must satisfy the port it backs.
var _ AuthorizationSource = (*StoreAuthorizationSource)(nil)
