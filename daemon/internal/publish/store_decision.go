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

func (d *storePublicationDecision) prepare(ctx context.Context, c Candidate, audit domain.WorkflowAudit, key string, payload []byte) ([]byte, bool, error) {
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
		if err := validateTrustCandidate(c, profile, audit); err != nil {
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
		entry, inserted, err := tx.EnqueueOutbox(ctx, key, IntentKindPublication, payload)
		if err != nil {
			return fmt.Errorf("record intent: %w", err)
		}
		if entry.IdempotencyKey != key || entry.Kind != IntentKindPublication {
			return fmt.Errorf("record intent: key %q holds kind %q", entry.IdempotencyKey, entry.Kind)
		}
		prior, recorded = entry.Payload, inserted
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
