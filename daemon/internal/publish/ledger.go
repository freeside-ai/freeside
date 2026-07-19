package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// IntentKindPublication is the outbox kind under which candidate
// publication intents are recorded (and later scanned for recovery).
const IntentKindPublication = "publish.publication"

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
type IntentLedger interface {
	Record(ctx context.Context, key, kind string, payload []byte) (prior []byte, recorded bool, err error)
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
type Intent struct {
	Identity        domain.Digest       `json:"identity"`
	InvocationID    domain.InvocationID `json:"invocation_id"`
	Repo            string              `json:"repo"`
	BaseRef         string              `json:"base_ref"`
	SourceHeadSHA   string              `json:"source_head_sha"`
	AuthorizationID domain.Digest       `json:"authorization_id"`
}

// Validate reports whether the intent is well-formed. It runs on both
// sides of the ledger boundary: before encoding, and on every decode,
// since a decoded outbox row is a reconstructed value and is not
// trusted to be well-formed.
func (i Intent) Validate() error {
	if !validIdentityDigest(string(i.Identity)) {
		return fmt.Errorf("intent identity %q is not a publication identity digest", i.Identity)
	}
	if i.InvocationID == "" {
		return errors.New("intent: empty invocation id")
	}
	if i.Repo == "" {
		return errors.New("intent: empty repository")
	}
	if i.BaseRef == "" {
		return errors.New("intent: empty base ref")
	}
	if i.SourceHeadSHA == "" {
		return errors.New("intent: empty source head sha")
	}
	// The authorization id is a sha256 content address (validIdentityDigest
	// checks the same sha256 form). A malformed one cannot name the record
	// the publication committed under, so the drain must not act on it.
	if !validIdentityDigest(string(i.AuthorizationID)) {
		return fmt.Errorf("intent authorization_id %q is not a digest", i.AuthorizationID)
	}
	return nil
}

// Encode validates and serializes the intent for the ledger payload.
func (i Intent) Encode() ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(i)
	if err != nil {
		return nil, fmt.Errorf("intent: encode: %w", err)
	}
	return payload, nil
}

// DecodeIntent deserializes and validates a ledger payload. Unknown
// fields and trailing data fail closed: an intent this package cannot
// fully interpret must not drive convergence decisions.
func DecodeIntent(payload []byte) (Intent, error) {
	var i Intent
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&i); err != nil {
		return Intent{}, fmt.Errorf("intent: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Intent{}, errors.New("intent: decode: trailing data after the intent")
	}
	if err := i.Validate(); err != nil {
		return Intent{}, err
	}
	return i, nil
}

// IntentKey returns the idempotency key for one invocation's effect of
// one kind. The invocation ID is the attempt axis (§5.9): retries of
// the same invocation collide on this key and converge on the original
// row, while a new invocation records a new intent whose payload
// carries the same content-derived identity. Empty components error
// rather than composing a key that could collide across invocations.
func IntentKey(invocationID domain.InvocationID, kind string) (string, error) {
	if invocationID == "" {
		return "", errors.New("intent key: empty invocation id")
	}
	if kind == "" {
		return "", errors.New("intent key: empty kind")
	}
	return "publish/" + string(invocationID) + "/" + kind, nil
}
