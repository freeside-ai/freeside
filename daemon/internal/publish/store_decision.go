package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// storePublicationDecision closes the local trust/intent TOCTOU seam: one
// SQLite transaction records the fresh live audit, resolves the explicitly
// active profile, evaluates drift, reconstructs the candidate authorization,
// and commits the publication intent. GitHub effects remain outside SQLite;
// no local transaction can be atomic with that external system.
type storePublicationDecision struct {
	store *store.Store
}

func (d *storePublicationDecision) revalidateOutcomeRepair(
	ctx context.Context, c Candidate, audit domain.WorkflowAudit,
) error {
	if c.DispositionHistory == nil {
		return nil
	}
	return d.store.Read(ctx, func(tx *store.ReadTx) error {
		profile, err := tx.LatestTrustProfile(ctx, c.Repo)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no current trust profile for %s: %w", c.Repo, ErrTrustProfileDrift)
		}
		if err != nil {
			return fmt.Errorf("read current trust profile: %w", err)
		}
		if err := validateTrustCandidate(c, profile, audit,
			func(digest domain.Digest) (domain.AutomationTrustProfile, bool, error) {
				superseded, err := tx.GetTrustProfile(ctx, digest)
				switch {
				case errors.Is(err, store.ErrNotFound):
					return domain.AutomationTrustProfile{}, false, nil
				case err != nil:
					return domain.AutomationTrustProfile{}, false, err
				case superseded.Repo != c.Repo:
					return domain.AutomationTrustProfile{}, false, nil
				}
				return superseded, true, nil
			},
			func(runID domain.RunID) (domain.ReviewConfigurationRecoveryTransition, bool, error) {
				transition, found, err := tx.LatestReviewConfigurationRecoveryTransition(ctx, runID)
				if errors.Is(err, domain.ErrReviewConfigRecoveryIneffective) {
					return domain.ReviewConfigurationRecoveryTransition{}, false, nil
				}
				if err != nil {
					return domain.ReviewConfigurationRecoveryTransition{}, false, err
				}
				return transition, found, nil
			}); err != nil {
			return err
		}
		if c.AuthorizationID == nil {
			return fmt.Errorf("candidate carries no authorization binding: %w", ErrUnauthorizedPublication)
		}
		auth, err := tx.GetCandidateAuthorization(ctx, *c.AuthorizationID)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no authorization recorded under %s: %w", *c.AuthorizationID, ErrUnauthorizedPublication)
		}
		if err != nil {
			return fmt.Errorf("read candidate authorization: %w", err)
		}
		if err := validateAuthorizationCandidate(c, auth); err != nil {
			return err
		}
		if err := validateCurrentDispositionHistory(ctx, tx, d.store, c, profile, auth); err != nil {
			return err
		}
		return validatePersistedReadinessProofs(ctx, tx, *c.DispositionHistory)
	})
}

func (d *storePublicationDecision) prepare(
	ctx context.Context,
	c Candidate,
	audit domain.WorkflowAudit,
	key string,
	payload []byte,
	claim *Reservation,
	producingInvocationID *domain.InvocationID,
) ([]byte, bool, error) {
	var (
		prior       []byte
		recorded    bool
		decisionErr error
	)
	err := d.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if _, err := tx.RecordWorkflowAudit(ctx, audit); err != nil {
			return fmt.Errorf("record fresh workflow audit: %w", err)
		}
		profile, err := tx.LatestTrustProfile(ctx, c.Repo)
		if errors.Is(err, store.ErrNotFound) {
			decisionErr = fmt.Errorf("no current trust profile for %s: %w", c.Repo, ErrTrustProfileDrift)
			return nil
		}
		if err != nil {
			return fmt.Errorf("read current trust profile: %w", err)
		}
		if err := validateTrustCandidate(c, profile, audit,
			func(digest domain.Digest) (domain.AutomationTrustProfile, bool, error) {
				superseded, err := tx.GetTrustProfile(ctx, digest)
				switch {
				case errors.Is(err, store.ErrNotFound):
					return domain.AutomationTrustProfile{}, false, nil
				case err != nil:
					return domain.AutomationTrustProfile{}, false, err
				case superseded.Repo != c.Repo:
					return domain.AutomationTrustProfile{}, false, nil
				}
				return superseded, true, nil
			},
			func(runID domain.RunID) (domain.ReviewConfigurationRecoveryTransition, bool, error) {
				transition, found, err := tx.LatestReviewConfigurationRecoveryTransition(ctx, runID)
				// The store's ineffective classification (a tampered, unbacked,
				// moved-on, or over-broad adoption) grants nothing: report
				// absence so the gate fails closed as ordinary drift.
				if errors.Is(err, domain.ErrReviewConfigRecoveryIneffective) {
					return domain.ReviewConfigurationRecoveryTransition{}, false, nil
				}
				if err != nil {
					return domain.ReviewConfigurationRecoveryTransition{}, false, err
				}
				return transition, found, nil
			}); err != nil {
			decisionErr = err
			return nil
		}
		if c.AuthorizationID == nil {
			decisionErr = fmt.Errorf("candidate carries no authorization binding: %w", ErrUnauthorizedPublication)
			return nil
		}
		auth, err := tx.GetCandidateAuthorization(ctx, *c.AuthorizationID)
		if errors.Is(err, store.ErrNotFound) {
			decisionErr = fmt.Errorf("no authorization recorded under %s: %w", *c.AuthorizationID, ErrUnauthorizedPublication)
			return nil
		}
		if err != nil {
			return fmt.Errorf("read candidate authorization: %w", err)
		}
		if err := validateAuthorizationCandidate(c, auth); err != nil {
			decisionErr = err
			return nil
		}
		validateReadinessProofs := c.DispositionHistory != nil
		if existing, err := tx.GetOutbox(ctx, key); err == nil {
			if existing.Kind == IntentKindPublication {
				intent, err := DecodeStoredIntent(existing)
				if err != nil {
					return fmt.Errorf("decode existing publication intent: %w", err)
				}
				// Only pre-disposition-history intents receive legacy
				// compatibility. A modern retry can still create forge state,
				// so its frozen readiness proofs must remain reconstructable.
				validateReadinessProofs = c.DispositionHistory != nil &&
					intent.FormatVersion == IntentFormatCurrent
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("read existing publication intent: %w", err)
		}
		if err := validateCurrentDispositionHistory(
			ctx, &tx.ReadTx, d.store, c, profile, auth,
		); err != nil {
			decisionErr = fmt.Errorf("validate disposition history at publication decision: %w", err)
			return nil
		}
		if validateReadinessProofs {
			if c.DispositionHistory == nil {
				decisionErr = errors.New("validate persisted readiness derivation: modern publication intent carries no disposition history")
				return nil
			}
			if err := validatePersistedReadinessProofs(ctx, &tx.ReadTx, *c.DispositionHistory); err != nil {
				decisionErr = fmt.Errorf("validate persisted readiness derivation: %w", err)
				return nil
			}
		}
		if producingInvocationID != nil {
			reservationState, err := validateExecutionCandidate(
				ctx, tx, c, claim, *producingInvocationID,
				profile.RepositoryID, auth.BaseSHA,
			)
			if err != nil {
				decisionErr = err
				return nil
			}
			// A newly settled execution publication must be fresh against the
			// exact target tip observed by this audit. An already-committed
			// intent may continue after a base advance: effects may have begun,
			// and recovery must converge them instead of stranding a branch.
			if reservationState == invocationReserved && audit.AuditedCommitSHA != auth.BaseSHA {
				decisionErr = fmt.Errorf(
					"fresh target %s is %s, execution was admitted at %s: %w",
					c.BaseRef, audit.AuditedCommitSHA, auth.BaseSHA, ErrTargetBaseAdvanced,
				)
				return nil
			}
		}
		stored, inserted, err := commitReservedIntent(ctx, tx, key, IntentKindPublication, payload, claim)
		if err != nil {
			// A refusal the publication itself earned (a quarantined intent,
			// an invocation another owner reserved) is a decision, not a
			// transaction failure: the fresh audit this transaction recorded
			// is a real observation and must still commit.
			if isPublicationRefusal(err) {
				decisionErr = err
				return nil
			}
			return fmt.Errorf("record intent: %w", err)
		}
		prior, recorded = stored, inserted
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if decisionErr != nil {
		return nil, false, decisionErr
	}
	return prior, recorded, nil
}
