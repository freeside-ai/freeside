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
// RecoveryCandidate carries every input needed to repeat the complete
// publication effect after an intent-only crash. PublishHead must
// idempotently transport this exact candidate head before Publisher asks the
// forge to converge the pull request.
type RecoveryCandidate struct {
	Candidate       Candidate
	ApprovedRecipes map[domain.Digest]bool
	PublishHead     func(context.Context, IdentityInput) error
}

// Resolve returns the candidate, current approved-recipe set, and the
// idempotent head transport reconstructed by the engine. A recipe un-approved
// since the intent committed must make Publish fail closed, and a missing
// transport must not leave a locally re-authored head unrecoverable.
type CandidateResolver interface {
	Resolve(ctx context.Context, intent Intent) (RecoveryCandidate, error)
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
	return drainPendingPublications(ctx, s, p, resolve, "")
}

// DrainPublicationIntent re-converges only the publication intent committed
// by invocationID. A workflow reconciling one durable task uses this scoped
// form so another task's pending intent cannot require a process-local
// candidate the active task did not reconstruct.
func DrainPublicationIntent(
	ctx context.Context,
	s *store.Store,
	p *Publisher,
	resolve CandidateResolver,
	invocationID domain.InvocationID,
) (int, error) {
	key, err := IntentKey(invocationID, IntentKindPublication)
	if err != nil {
		return 0, fmt.Errorf("drain publication: %w", err)
	}
	return drainPendingPublications(ctx, s, p, resolve, key)
}

func finalizePublicationResult(
	ctx context.Context,
	s *store.Store,
	candidate Candidate,
	result Result,
) error {
	identity, err := deriveCandidateIdentity(candidate)
	if err != nil {
		return fmt.Errorf("derive candidate identity: %w", err)
	}
	expected, err := intentForCandidate(candidate, identity)
	if err != nil {
		return err
	}
	key, err := IntentKey(candidate.InvocationID, IntentKindPublication)
	if err != nil {
		return err
	}
	return finalizePublicationEntry(ctx, s, key, expected, result)
}

func drainPendingPublications(
	ctx context.Context,
	s *store.Store,
	p *Publisher,
	resolve CandidateResolver,
	targetKey string,
) (int, error) {
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
		if targetKey != "" && entry.IdempotencyKey != targetKey {
			continue
		}
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

		recovered, err := resolve.Resolve(ctx, intent)
		if err != nil {
			return dispatched, fmt.Errorf("drain publications: resolve %q: %w", entry.IdempotencyKey, err)
		}
		cand := recovered.Candidate

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
		// Authorization axis (#168): the identity excludes the authorization
		// binding, so a resolver reconstructing the same head under a
		// different current authorization would derive the same identity yet
		// publish under a decision the intent never committed. Reproduce the
		// committed authorization or refuse — Publish's gate re-checks the
		// record is still authorizing, but recovery must not retarget which
		// record that is.
		if cand.AuthorizationID == nil || *cand.AuthorizationID != intent.AuthorizationID {
			return dispatched, fmt.Errorf("drain publications: intent %q committed authorization %s, resolved candidate carries %s: %w",
				entry.IdempotencyKey, intent.AuthorizationID, derefDigest(cand.AuthorizationID), errPublicationIntentDiverged)
		}
		if recovered.PublishHead == nil {
			return dispatched, fmt.Errorf(
				"drain publications: resolve %q returned no head transport",
				entry.IdempotencyKey,
			)
		}

		result, err := p.PublishAfterGate(
			ctx, cand, recovered.ApprovedRecipes, recovered.PublishHead,
		)
		if err != nil {
			return dispatched, fmt.Errorf("drain publications: publish %q: %w", entry.IdempotencyKey, err)
		}

		if err := finalizePublicationEntry(
			ctx, s, entry.IdempotencyKey, intent, result,
		); err != nil {
			return dispatched, fmt.Errorf("drain publications: finalize %q: %w", entry.IdempotencyKey, err)
		}
		dispatched++
	}
	return dispatched, nil
}

func finalizePublicationEntry(
	ctx context.Context,
	s *store.Store,
	key string,
	intent Intent,
	result Result,
) error {
	if result.Identity.Digest() != intent.Identity {
		return fmt.Errorf("publication intent %q disagrees with returned result: %w",
			key, errPublicationIntentDiverged)
	}
	// Publish returned success, so every artifact passed the evidence gate
	// before any external effect (publisher.go): the recorded eligibility is
	// that gate's verdict, not an assumed one.
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
		return fmt.Errorf("outcome %q: %w", key, err)
	}

	// One internal transaction records the outcome and marks the intent
	// dispatched together. A returned inbox row is a reconstruction boundary:
	// verify its complete identity, kind, and payload before accepting it.
	outcomeKey := OutcomeKey(result.Identity)
	return s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		entry, err := tx.GetOutbox(ctx, key)
		if err != nil {
			return fmt.Errorf("read publication intent %q: %w", key, err)
		}
		if entry.IdempotencyKey != key || entry.Kind != IntentKindPublication {
			return fmt.Errorf("publication intent %q has kind %q: %w",
				key, entry.Kind, errPublicationIntentCorrupt)
		}
		if entry.Quarantined() {
			return fmt.Errorf("publication intent %q is quarantined: %w",
				key, errPublicationIntentDiverged)
		}
		committed, err := DecodeIntent(entry.Payload)
		if err != nil {
			return fmt.Errorf("publication intent %q payload: %w", key, err)
		}
		if committed != intent {
			return fmt.Errorf("publication intent %q changed before finalization: %w",
				key, errPublicationIntentDiverged)
		}
		if err := tx.MarkOutboxDispatched(ctx, key); err != nil {
			return err
		}
		existing, _, err := tx.RecordInbox(ctx, outcomeKey, IntentKindOutcome, payload)
		if err != nil {
			return err
		}
		if existing.IdempotencyKey != outcomeKey ||
			existing.Kind != IntentKindOutcome ||
			!bytes.Equal(existing.Payload, payload) {
			return fmt.Errorf("outcome key %s: %w", outcomeKey, errPublicationOutcomeConflict)
		}
		return nil
	})
}

// derefDigest renders an optional digest for a divergence message: the
// value, or "none" when the resolved candidate carried no authorization.
func derefDigest(d *domain.Digest) domain.Digest {
	if d == nil {
		return "none"
	}
	return *d
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
