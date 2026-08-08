package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// CurrentTrust is a repository's recorded automation trust state (plan §5.5):
// the explicitly active trust profile and the latest persisted workflow
// audit. Either may be nil when none is recorded. Publication uses the active
// profile plus a separate fresh WorkflowAuditor observation; the persisted
// audit remains history and a compatibility seam for isolated tests.
type CurrentTrust struct {
	Profile *domain.AutomationTrustProfile
	Audit   *domain.WorkflowAudit
}

// TrustSource supplies the current automation trust state for a repository.
// Like IntentLedger it is a store-backed port, so the Publisher stays
// decoupled from the store and unit tests fake it: the drift gate re-reads
// through it on every Publish. WorkflowAuditor, not the recorded Audit field,
// supplies the publication gate's fresh observation; the aggregate shape is
// retained as a narrow adapter/test seam.
type TrustSource interface {
	CurrentTrust(ctx context.Context, repo string) (CurrentTrust, error)
}

// TrustProfileSource is the optional by-digest profile read a TrustSource may
// additionally support. The drift gate's adoption arm (issue #611) needs the
// superseded revision to re-derive a review-configuration-only supersession;
// a source without this capability simply keeps the strict equality gate, so
// the interface stays optional rather than widening every fake.
type TrustProfileSource interface {
	TrustProfile(ctx context.Context, repo string, digest domain.Digest) (domain.AutomationTrustProfile, bool, error)
}

// ReviewAdoptionSource is the optional command-backed adoption read a
// TrustSource may additionally support. The drift gate's adoption arm never
// takes Candidate.AdoptedTrustProfileDigest on its word: the run must carry
// an operator-authorized recovery transition, re-gated by its store on read,
// naming exactly the claimed supersession. A source without this capability
// keeps the strict equality gate, so a claim can never widen what a source
// built before this interface accepted.
type ReviewAdoptionSource interface {
	ReviewConfigurationAdoption(ctx context.Context, runID domain.RunID) (domain.ReviewConfigurationRecoveryTransition, bool, error)
}

// StoreTrustSource is the store-backed TrustSource, mirroring StoreLedger:
// it reads the explicitly activated trust profile and latest workflow audit
// for a repository in its own read transaction. They are selected by
// LatestTrustProfile and ListWorkflowAudits (ordered by insertion), so a §5.5
// drift-recovery re-approval or a fresh audit becomes current when recorded;
// the latest-only profile read is what keeps that true across an
// encoding-version bump, whose permanently stale pre-bump rows would abort
// a validating full-history read forever (#222).
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

// CurrentTrust returns the repository's active profile and latest audit.
// A repository with no recorded profile or audit yields a nil field rather
// than an error, so the read itself reports only real store failures.
// TrustProfile reconstructs one profile revision by its content digest,
// reporting absence separately and refusing a revision recorded for another
// repository.
func (t *StoreTrustSource) TrustProfile(
	ctx context.Context, repo string, digest domain.Digest,
) (domain.AutomationTrustProfile, bool, error) {
	var profile domain.AutomationTrustProfile
	err := t.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		profile, err = tx.GetTrustProfile(ctx, digest)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return domain.AutomationTrustProfile{}, false, nil
	case err != nil:
		return domain.AutomationTrustProfile{}, false, fmt.Errorf("trust profile %s: %w", digest, err)
	case profile.Repo != repo:
		return domain.AutomationTrustProfile{}, false, nil
	}
	return profile, true, nil
}

// ReviewConfigurationAdoption returns the run's newest command-backed
// recovery transition when the store's read re-gate accepts it. A determinate
// integrity or policy rejection (the store's ineffective classification)
// reports absence, matching the engine's treatment: an ineffective adoption
// grants nothing, and here that means the strict drift gate.
func (t *StoreTrustSource) ReviewConfigurationAdoption(
	ctx context.Context, runID domain.RunID,
) (domain.ReviewConfigurationRecoveryTransition, bool, error) {
	var (
		transition domain.ReviewConfigurationRecoveryTransition
		found      bool
	)
	err := t.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		transition, found, err = tx.LatestReviewConfigurationRecoveryTransition(ctx, runID)
		if errors.Is(err, domain.ErrReviewConfigRecoveryIneffective) {
			found = false
			return nil
		}
		return err
	})
	if err != nil || !found {
		return domain.ReviewConfigurationRecoveryTransition{}, false, err
	}
	return transition, true, nil
}

func (t *StoreTrustSource) CurrentTrust(ctx context.Context, repo string) (CurrentTrust, error) {
	var ct CurrentTrust
	err := t.store.Read(ctx, func(tx *store.ReadTx) error {
		profile, err := tx.LatestTrustProfile(ctx, repo)
		switch {
		case err == nil:
			ct.Profile = &profile
		case !errors.Is(err, store.ErrNotFound):
			return err
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
