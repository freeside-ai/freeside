package publish

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// IntentKindOutcome is the inbox kind under which a converged
// publication's outcome is recorded: the durable "one recorded outcome"
// the effectively-once matrix converges to (issue #82 acceptance 3).
const IntentKindOutcome = "publish.outcome"

// Outcome is the recorded result of one converged publication: the
// identity it converged to, the branch and PR that now carry it, the
// candidate head, and the evidence-eligibility state the publisher gated
// at publication time. It is written to the store inbox keyed by
// OutcomeKey, so a re-drive of the same publication converges on the one
// recorded row rather than recording a second.
//
// Every field is a pure function of the publication identity and the one
// PR that identity converges to, so an Outcome is deterministic per
// identity. That is deliberate: the attempt axis (which invocation
// published) lives on the outbox intent, not here, so two invocations
// publishing the same content (a §5.9 operator re-run or crash recovery)
// produce a byte-identical outcome and converge on the one row instead
// of conflicting on it.
type Outcome struct {
	Identity domain.Digest `json:"identity"`
	Repo     string        `json:"repo"`
	BaseRef  string        `json:"base_ref"`
	HeadSHA  string        `json:"head_sha"`
	Branch   string        `json:"branch"`
	PRNumber int           `json:"pr_number"`
	// EvidenceEligible records that every published artifact passed the
	// trusted evidence gate at publication time (plan §5.15 rule 2). The
	// publisher fails closed on any ineligible artifact before any
	// external effect, so a recorded outcome carries the gate's verdict,
	// never an assumed one.
	EvidenceEligible bool `json:"evidence_eligible"`
}

// Validate reports whether the outcome is well-formed. Like Intent it
// runs on both sides of the store boundary: before encoding, and on
// every decode, since a decoded inbox row is a reconstructed value and
// is not trusted to be well-formed.
func (o Outcome) Validate() error {
	if !validIdentityDigest(string(o.Identity)) {
		return fmt.Errorf("outcome identity %q is not a publication identity digest", o.Identity)
	}
	if o.Repo == "" {
		return errors.New("outcome: empty repository")
	}
	if o.BaseRef == "" {
		return errors.New("outcome: empty base ref")
	}
	if o.HeadSHA == "" {
		return errors.New("outcome: empty head sha")
	}
	if o.Branch == "" {
		return errors.New("outcome: empty branch")
	}
	wantBranch := (Identity{digest: o.Identity}).BranchName()
	if o.Branch != wantBranch {
		return fmt.Errorf("outcome branch %q does not match identity branch %q", o.Branch, wantBranch)
	}
	if o.PRNumber <= 0 {
		return fmt.Errorf("outcome: non-positive pr number %d", o.PRNumber)
	}
	if !o.EvidenceEligible {
		return errors.New("outcome: evidence is not eligible")
	}
	return nil
}

// Encode validates and serializes the outcome for the inbox payload.
func (o Outcome) Encode() ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("outcome: encode: %w", err)
	}
	return payload, nil
}

// DecodeOutcome deserializes and validates an inbox payload. Unknown
// fields and trailing data fail closed: an outcome this package cannot
// fully interpret must not be trusted as a recorded result.
func DecodeOutcome(payload []byte) (Outcome, error) {
	var o Outcome
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return Outcome{}, fmt.Errorf("outcome: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Outcome{}, errors.New("outcome: decode: trailing data after the outcome")
	}
	if err := o.Validate(); err != nil {
		return Outcome{}, err
	}
	return o, nil
}

// outcomeKeyPrefix namespaces the outcome's inbox idempotency key to
// this kind. The inbox enforces uniqueness by idempotency_key alone
// (not by kind), so a bare digest could collide with some other inbox
// kind that keys by a content digest; the prefix keeps the outcome key
// unambiguously the publish lane's.
const outcomeKeyPrefix = IntentKindOutcome + "/"

// OutcomeKey returns the inbox idempotency key for a publication's
// outcome: the kind prefix plus the full identity digest — the full
// digest, not the 16-hex branch prefix, so two identities sharing a
// branch-name prefix can never alias one outcome row (the same reason
// the PR marker carries the full digest).
func OutcomeKey(id Identity) string {
	return outcomeKeyPrefix + string(id.Digest())
}
