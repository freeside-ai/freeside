package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// errPublicationIntentCorrupt reports a pending outbox row whose payload
// does not name the invocation its idempotency key committed: fail-closed
// evidence of corruption or a foreign writer, never dispatchable. It
// mirrors signet's errInvocationIntentCorrupt (dispatch.go).
var errPublicationIntentCorrupt = errors.New("outbox intent payload disagrees with its idempotency key")

// errPublicationIntentDiverged reports a resolver that reconstructed a
// candidate not matching the committed intent — a different derived
// identity (content axis) or a different invocation (attempt axis).
// Either would let the re-converge escape the intent it recovers: a
// different identity creates resources the intent does not name, and a
// different invocation makes Publish record a second outbox row under the
// resolver's key while this row stays pending. The drain refuses before
// any effect and leaves the row pending.
var errPublicationIntentDiverged = errors.New("resolved candidate does not match the recorded intent")

// errPublicationOutcomeConflict reports that the outcome inbox key
// already holds a different record than this converged publication's
// outcome. Because the inbox is unique by idempotency key alone, a
// foreign or corrupt row under the key would otherwise let the finalize
// mark the intent dispatched with no valid outcome recorded; the drain
// refuses and leaves the row pending instead.
var errPublicationOutcomeConflict = errors.New("outcome inbox key holds a different record")

// CandidateResolver reconstructs the full publication candidate for a
// recorded intent. The outbox intent carries only the identity-relevant
// coordinates (identity, invocation, repo, base, head), not the evidence
// artifacts or PR prose a re-converge needs, so the drain asks the
// resolver for the rest. In production the Wave 2 engine implements it,
// reloading the candidate from durable workflow state; the kill-test
// harness stands in for the engine, holding the candidate across the
// simulated restart. This is the same boundary signet's dispatch draws
// with the empty StartSpec (dispatch.go): full request reconstruction is
// the engine's, not this recovery scan's.
//
// Resolve returns the candidate and the approved-recipe set the publish
// re-gate runs against, both as the engine currently holds them: a
// recipe un-approved since the intent committed must make Publish fail
// closed, not converge on stale eligibility.
type CandidateResolver interface {
	Resolve(ctx context.Context, intent Intent) (Candidate, map[domain.Digest]bool, error)
}

// DrainPendingPublications re-converges every committed-but-undispatched
// publication intent onto its one branch, PR, and recorded outcome: the
// recovery half of effectively-once publication (plan §5.9), the analog
// of signet's DispatchPendingInvocations. It is not a polling loop; it
// is the idempotent drain safe to call on startup and after any
// suspected loss, returning the count it finalized.
//
// Effectively-once composes from two layers. Publisher.Publish is
// idempotent: check-before-create finds any branch and PR a prior
// attempt created and converges instead of duplicating, and the ledger
// converges the re-recorded intent. The finalize — mark the outbox
// dispatched and record the outcome — rides one internal transaction, so
// the two commit together or not at all; a crash before it re-runs on
// the next drain and re-converges with no new external effect. The
// GitHub effect and the SQLite finalize cannot share a transaction; that
// gap is the after-effect-before-acceptance boundary, closed by the
// idempotent re-converge, not by a shared commit.
func DrainPendingPublications(ctx context.Context, s *store.Store, p *Publisher, resolve CandidateResolver) (int, error) {
	var pending []store.QueueEntry
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		entries, err := tx.ListPendingOutbox(ctx, IntentKindPublication)
		if err != nil {
			return err
		}
		pending = entries
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("drain publications: %w", err)
	}

	dispatched := 0
	for _, entry := range pending {
		intent, err := DecodeIntent(entry.Payload)
		if err != nil {
			return dispatched, fmt.Errorf("drain publications: intent %q payload: %w", entry.IdempotencyKey, err)
		}
		// The payload is opaque to the store, so the decoded intent is a
		// reconstruction boundary: re-check it against the row's own key
		// before acting (the store trust-boundary convention, dispatch.go).
		// A mismatch fails loud and leaves the row pending — publishing a
		// decoded foreign identity while marking the original dispatched
		// would both misfire a publication and orphan the real intent.
		key, err := IntentKey(intent.InvocationID, IntentKindPublication)
		if err != nil {
			return dispatched, fmt.Errorf("drain publications: intent %q: %w", entry.IdempotencyKey, err)
		}
		if key != entry.IdempotencyKey {
			return dispatched, fmt.Errorf("drain publications: intent %q payload names invocation %q: %w",
				entry.IdempotencyKey, intent.InvocationID, errPublicationIntentCorrupt)
		}

		cand, approved, err := resolve.Resolve(ctx, intent)
		if err != nil {
			return dispatched, fmt.Errorf("drain publications: resolve %q: %w", entry.IdempotencyKey, err)
		}

		// The resolver is trusted to reload the candidate, not to reload
		// the *right* one, so match the resolved candidate against the
		// committed intent on both axes BEFORE any external effect —
		// deriving first is what makes the refusal zero-effect.
		//
		// Attempt axis: the content identity excludes the invocation, so a
		// candidate carrying a different InvocationID would derive the same
		// identity yet make Publish record a second outbox row under the
		// resolver's key, leaving this intent's row to re-drive forever.
		if cand.InvocationID != intent.InvocationID {
			return dispatched, fmt.Errorf("drain publications: intent %q resolved to invocation %q: %w",
				entry.IdempotencyKey, cand.InvocationID, errPublicationIntentDiverged)
		}
		// Content axis: a different derived identity would create a branch
		// and PR the intent does not name while this row stayed pending.
		derived, err := deriveCandidateIdentity(cand)
		if err != nil {
			return dispatched, fmt.Errorf("drain publications: intent %q resolved candidate: %w", entry.IdempotencyKey, err)
		}
		if derived.Digest() != intent.Identity {
			return dispatched, fmt.Errorf("drain publications: intent %q resolved to identity %s: %w",
				entry.IdempotencyKey, derived.Digest(), errPublicationIntentDiverged)
		}

		result, err := p.Publish(ctx, cand, approved)
		if err != nil {
			return dispatched, fmt.Errorf("drain publications: publish %q: %w", entry.IdempotencyKey, err)
		}

		// Publish returned success, so every artifact passed the evidence
		// gate before any external effect (publisher.go): the recorded
		// eligibility is the gate's verdict, not an assumed one.
		outcome := Outcome{
			Identity:         result.Identity.Digest(),
			Repo:             intent.Repo,
			BaseRef:          intent.BaseRef,
			HeadSHA:          intent.SourceHeadSHA,
			Branch:           result.Branch,
			PRNumber:         result.PRNumber,
			EvidenceEligible: true,
		}
		payload, err := outcome.Encode()
		if err != nil {
			return dispatched, fmt.Errorf("drain publications: outcome %q: %w", entry.IdempotencyKey, err)
		}

		// One internal transaction records the outcome and marks the
		// intent dispatched together (#82 acceptance 3): both are
		// non-revision-bumping bookkeeping, and both are individually
		// idempotent, so a re-drive after this commit re-converges cleanly.
		// RecordInbox does not overwrite an existing key, so a returned
		// row is a reconstruction boundary: it must be this outcome
		// (byte-identical, since an outcome is deterministic per identity)
		// or the finalize is refusing a foreign/corrupt row — fail closed
		// and leave the intent pending rather than dispatch it with no
		// valid outcome recorded.
		outcomeKey := OutcomeKey(result.Identity)
		err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
			if err := tx.MarkOutboxDispatched(ctx, entry.IdempotencyKey); err != nil {
				return err
			}
			existing, _, err := tx.RecordInbox(ctx, outcomeKey, IntentKindOutcome, payload)
			if err != nil {
				return err
			}
			// The returned row is the durable outcome this transaction is
			// attesting to. The inbox is unique by key alone, so payload
			// equality is insufficient: a foreign kind can occupy the key
			// with the same bytes. Verify the complete identity/kind/payload
			// tuple whether this call inserted or converged.
			if existing.IdempotencyKey != outcomeKey || existing.Kind != IntentKindOutcome || !bytes.Equal(existing.Payload, payload) {
				return fmt.Errorf("outcome key %s: %w", outcomeKey, errPublicationOutcomeConflict)
			}
			return nil
		})
		if err != nil {
			return dispatched, fmt.Errorf("drain publications: finalize %q: %w", entry.IdempotencyKey, err)
		}
		dispatched++
	}
	return dispatched, nil
}

// deriveCandidateIdentity computes a candidate's publication identity
// from the same inputs Publisher.Publish derives from (publisher.go): the
// repository and base, the candidate head, the artifact digest set, and
// the recipe digest. It exists so the drain can reject a diverging
// resolved candidate before any external effect; the mirror is pinned by
// a test that asserts it agrees with a real Publish's Result.Identity.
func deriveCandidateIdentity(c Candidate) (Identity, error) {
	digests := make([]domain.Digest, len(c.Artifacts))
	for i, a := range c.Artifacts {
		digests[i] = a.Digest
	}
	return DeriveIdentity(IdentityInput{
		Repo:            c.Repo,
		BaseRef:         c.BaseRef,
		SourceHeadSHA:   c.HeadSHA,
		ArtifactDigests: digests,
		RecipeDigest:    c.RecipeDigest,
	})
}
