package publish

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// IntentKindPublication is the outbox kind under which candidate
// publication intents are recorded (and later scanned for recovery).
const (
	IntentKindPublication = publicationrecord.IntentKindPublication
	IntentFormatLegacy    = publicationrecord.IntentFormatLegacy
	IntentFormatCurrent   = publicationrecord.IntentFormatCurrent
)

// IntentLedger is the publish-owned port onto the store's outbox
// ledger (plan §5.9): a publication effect commits its intent through
// Record, keyed by idempotency key, before anything is dispatched to
// GitHub. It mirrors the store's EnqueueOutbox so the Wave 2 wiring is
// a thin adapter; the port exists because that call rides the Write
// transaction committing the decision the effect belongs to (§5.14),
// and transaction composition belongs to the engine, not this package.
//
// Record returns the payload durably held under key: the given one
// when this call inserted it (recorded true), or the pre-existing row's
// payload when a prior attempt already committed the key (recorded
// false), so a retry converges on the original intent instead of
// re-recording.
//
// claim is the caller's proof that it holds the reservation occupying
// the key (reservation.go), or nil when the invocation was never
// reserved. An implementation that cannot see the reservation namespace
// cannot honour a claim, so it must refuse a non-nil one rather than
// writing past a reservation it did not check.
type IntentLedger interface {
	Record(ctx context.Context, key, kind string, payload []byte, claim *Reservation) (prior []byte, recorded bool, err error)
}

// Intent is the recorded payload of one publication effect: the
// derived identity (content axis) bound to the invocation that is
// publishing it (attempt axis), plus the coordinates reconciliation
// needs to find the branch and PR again without re-deriving anything.
//
// AuthorizationID pins the daemon-authored authorization the publication
// committed under (#168). Recovery must reproduce the committed decision,
// not silently retarget to whatever authorization is current at drain time:
// the identity excludes the authorization binding, so the invocation and
// identity divergence checks alone would not catch a resolver reconstructing
// the same head under a different authorization. The drain fails closed when
// the resolved candidate's AuthorizationID differs from this one.
//
// ProducingInvocationID and ReservationRunID are present together only for the
// execution-bound production path. Persisting both makes the stronger
// reservation/admission/export gate recoverable: the drain must reproduce the
// exact source invocation and reserving run before any effect. Legacy and
// attended-fake intents omit both and retain their existing recovery contract.
type Intent = publicationrecord.Intent

func intentForCandidate(
	c Candidate,
	identity Identity,
	producingInvocationID domain.InvocationID,
) (Intent, error) {
	if c.AuthorizationID == nil {
		return Intent{}, fmt.Errorf("candidate carries no authorization binding: %w", ErrUnauthorizedPublication)
	}
	intent := Intent{
		FormatVersion:         publicationrecord.IntentFormatCurrent,
		Identity:              identity.Digest(),
		InvocationID:          c.InvocationID,
		Repo:                  c.Repo,
		BaseRef:               c.BaseRef,
		SourceHeadSHA:         c.HeadSHA,
		AuthorizationID:       *c.AuthorizationID,
		ProducingInvocationID: producingInvocationID,
	}
	if c.DispositionHistory != nil {
		digest, err := c.DispositionHistory.digest()
		if err != nil {
			return Intent{}, fmt.Errorf("digest disposition history: %w", err)
		}
		intent.DispositionHistoryDigest = digest
	}
	if producingInvocationID != "" {
		intent.ReservationRunID = c.RunID
	}
	if err := intent.Validate(); err != nil {
		return Intent{}, err
	}
	return intent, nil
}

// ValidateIntentDispositionHistory binds recovery to the exact publisher-owned
// section frozen by the durable intent. Only authenticated legacy-format
// intents may omit the digest for a candidate that now carries disposition
// history; a current-format intent cannot gain that compatibility by losing
// one field.
func ValidateIntentDispositionHistory(intent Intent, c Candidate) error {
	if c.DispositionHistory == nil {
		if intent.DispositionHistoryDigest != "" {
			return fmt.Errorf("intent has disposition history but candidate does not: %w", ErrPublicationConflict)
		}
		return nil
	}
	digest, err := c.DispositionHistory.digest()
	if err != nil {
		return err
	}
	if intent.FormatVersion == IntentFormatCurrent && intent.DispositionHistoryDigest != digest {
		return fmt.Errorf("intent disposition history changed: %w", ErrPublicationConflict)
	}
	return nil
}

func intentsCompatible(committed, proposed Intent) bool {
	if committed == proposed {
		return true
	}
	if committed.FormatVersion == publicationrecord.IntentFormatLegacy &&
		proposed.FormatVersion == publicationrecord.IntentFormatCurrent {
		proposed.FormatVersion = publicationrecord.IntentFormatLegacy
		if committed.DispositionHistoryDigest == "" {
			proposed.DispositionHistoryDigest = ""
		}
		return committed == proposed
	}
	return false
}

// DecodeIntent deserializes and validates a ledger payload. Unknown
// fields and trailing data fail closed: an intent this package cannot
// fully interpret must not drive convergence decisions.
func DecodeIntent(payload []byte) (Intent, error) {
	return publicationrecord.DecodeIntent(payload)
}

// DecodeStoredIntent authenticates the payload contract against the
// store-owned outbox format marker. The payload's own version cannot grant
// legacy compatibility by itself.
func DecodeStoredIntent(entry store.QueueEntry) (Intent, error) {
	intent, err := DecodeIntent(entry.Payload)
	if err != nil {
		return Intent{}, err
	}
	if intent.FormatVersion != entry.PayloadVersion {
		return Intent{}, fmt.Errorf(
			"intent payload format %d disagrees with outbox format %d: %w",
			intent.FormatVersion, entry.PayloadVersion, domain.ErrParentKeyMismatch,
		)
	}
	return intent, nil
}

// IntentKey returns the idempotency key for one invocation's effect of
// one kind. The invocation ID is the attempt axis (§5.9): retries of
// the same invocation collide on this key and converge on the original
// row, while a new invocation records a new intent whose payload
// carries the same content-derived identity. Empty components error
// rather than composing a key that could collide across invocations.
func IntentKey(invocationID domain.InvocationID, kind string) (string, error) {
	return publicationrecord.IntentKey(invocationID, kind)
}
