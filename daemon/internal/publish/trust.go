package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// CurrentTrust is a repository's current automation trust state at the
// publication decision point (plan §5.5): the latest recorded trust profile
// and the latest workflow audit. Either may be nil when none is recorded;
// the drift gate fails closed on a nil field, since a publication that
// cannot prove its bound profile is current and un-drifted must not proceed.
type CurrentTrust struct {
	Profile *domain.AutomationTrustProfile
	Audit   *domain.WorkflowAudit
}

// TrustSource supplies the current automation trust state for a repository.
// Like IntentLedger it is a store-backed port, so the Publisher stays
// decoupled from the store and unit tests fake it: the drift gate re-reads
// through it on every Publish, so a profile revised or an audit recorded
// since the candidate was authorized is observed at the decision point, not
// cached from authorization time (plan §5.5, "drift fails closed").
type TrustSource interface {
	CurrentTrust(ctx context.Context, repo string) (CurrentTrust, error)
}

// StoreTrustSource is the store-backed TrustSource, mirroring StoreLedger:
// it reads the latest recorded trust profile and workflow audit for a
// repository in its own read transaction. The "current" profile and audit
// are the last recorded revision of each (ListTrustProfiles and
// ListWorkflowAudits order by insertion), so a §5.5 drift-recovery
// re-approval or a fresh audit becomes current the moment it is recorded.
type StoreTrustSource struct {
	store *store.Store
}

// NewStoreTrustSource wires the trust source to an open store; a nil store
// fails closed at construction rather than at the first read.
func NewStoreTrustSource(s *store.Store) (*StoreTrustSource, error) {
	if s == nil {
		return nil, errors.New("trust source: nil store")
	}
	return &StoreTrustSource{store: s}, nil
}

// CurrentTrust returns the repository's latest recorded profile and audit.
// A repository with no recorded profile or audit yields a nil field rather
// than an error; the drift gate is where absence fails closed, so the read
// itself reports only real store failures.
func (t *StoreTrustSource) CurrentTrust(ctx context.Context, repo string) (CurrentTrust, error) {
	var ct CurrentTrust
	err := t.store.Read(ctx, func(tx *store.ReadTx) error {
		profiles, err := tx.ListTrustProfiles(ctx, repo)
		if err != nil {
			return err
		}
		if n := len(profiles); n > 0 {
			p := profiles[n-1].Profile
			ct.Profile = &p
		}
		audits, err := tx.ListWorkflowAudits(ctx, repo)
		if err != nil {
			return err
		}
		if n := len(audits); n > 0 {
			a := audits[n-1].Audit
			ct.Audit = &a
		}
		return nil
	})
	if err != nil {
		return CurrentTrust{}, fmt.Errorf("current trust for %s: %w", repo, err)
	}
	return ct, nil
}
