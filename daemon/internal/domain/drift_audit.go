package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// DriftAuditEncodingVersion tags the canonical encoding; a change to the
	// field set or order is a new version, never a silent re-hash.
	DriftAuditEncodingVersion = 1
	// MaxDriftAuditBytes bounds a decoded artifact body. An audit carries at
	// most one reversal per finding of the run plus one explanation, so the
	// finding-adjudication bound is generous for it too.
	MaxDriftAuditBytes = MaxFindingAdjudicationBytes
)

// ParseDriftAuditRoute resolves a review.drift_audit_route policy value to its
// route. The enum predicate is unexported, so the store's policy decoder
// validates through this.
func ParseDriftAuditRoute(value string) (DriftAuditRoute, error) {
	route := DriftAuditRoute(value)
	if !route.valid() {
		return "", fmt.Errorf("drift audit route %q: %w", value, ErrInvalidDriftAuditRoute)
	}
	return route, nil
}

// DriftReversal is one piece of defensive work an over_hardened audit proposes
// to undo (plan §7 Review Drift). FindingID names the finding whose fix is
// reversed. Undo and Rationale are model prose: what to undo, and why the
// approved specification does not need it. Undo is advisory; it never names
// the remediation that made the fix, which the supersession record copies from
// the superseded disposition instead.
type DriftReversal struct {
	FindingID FindingID `json:"finding_id"`
	Undo      string    `json:"undo"`
	Rationale string    `json:"rationale"`
}

// Validate reports whether the reversal names a finding and explains itself.
// Whether the finding belongs to the run is the store's re-check; whether its
// fix may be reversed is the route gate's.
func (r DriftReversal) Validate() error {
	if r.FindingID == "" {
		return fmt.Errorf("drift reversal finding_id: %w", ErrEmptyID)
	}
	// json.Marshal rewrites invalid bytes to U+FFFD, so an unguarded field
	// would let the stored artifact differ from the submitted content while
	// still validating against the rewritten form, and two ids that differ
	// only in invalid bytes would share one digest.
	if !utf8.ValidString(string(r.FindingID)) {
		return fmt.Errorf("drift reversal finding_id %q: %w", r.FindingID, ErrDriftAuditInconsistent)
	}
	for _, field := range []struct{ label, text string }{
		{"undo", r.Undo}, {"rationale", r.Rationale},
	} {
		if strings.TrimSpace(field.text) == "" {
			return fmt.Errorf("drift reversal %q %s: %w", r.FindingID, field.label, ErrEmptyField)
		}
		if !utf8.ValidString(field.text) {
			return fmt.Errorf("drift reversal %q %s: %w", r.FindingID, field.label, ErrDriftAuditInconsistent)
		}
	}
	return nil
}

// DriftAudit is the immutable, digest-addressed artifact one drift audit
// produces (plan §7 Review Drift). It binds the run, the round, the round's
// base and candidate head, the approved specification digest, and the resolved
// policy digest, and carries the verdict, the auditor's self-assessed
// confidence, the reversal list, and an explanation. A round has at most one
// audit, so there is no revision chain. Digest is computed by the constructor,
// never caller-supplied.
//
// Confidence is required, as it is on a model-backed adjudication entry: the
// audit is always a model call. Reversals is never nil, so an empty list has
// one encoding ([]) and one digest.
type DriftAudit struct {
	EncodingVersion      int                    `json:"encoding_version"`
	RunID                RunID                  `json:"run_id"`
	Round                int                    `json:"round"`
	BaseSHA              string                 `json:"base_sha"`
	HeadSHA              string                 `json:"head_sha"`
	ApprovedSpecDigest   Digest                 `json:"approved_spec_digest"`
	ResolvedPolicyDigest Digest                 `json:"resolved_policy_digest"`
	Verdict              DriftVerdict           `json:"verdict"`
	Confidence           AdjudicationConfidence `json:"confidence"`
	Reversals            []DriftReversal        `json:"reversals"`
	Explanation          string                 `json:"explanation"`
	CreatedAt            time.Time              `json:"created_at"`
	Digest               Digest                 `json:"digest"`
}

// canonicalDriftAudit is the digest preimage: every field but Digest, in the
// artifact's own order.
type canonicalDriftAudit struct {
	EncodingVersion      int                    `json:"encoding_version"`
	RunID                RunID                  `json:"run_id"`
	Round                int                    `json:"round"`
	BaseSHA              string                 `json:"base_sha"`
	HeadSHA              string                 `json:"head_sha"`
	ApprovedSpecDigest   Digest                 `json:"approved_spec_digest"`
	ResolvedPolicyDigest Digest                 `json:"resolved_policy_digest"`
	Verdict              DriftVerdict           `json:"verdict"`
	Confidence           AdjudicationConfidence `json:"confidence"`
	Reversals            []DriftReversal        `json:"reversals"`
	Explanation          string                 `json:"explanation"`
	CreatedAt            time.Time              `json:"created_at"`
}

// DriftAuditInput is the caller-supplied content of a drift audit. The
// encoding version and digest are omitted so no input path can set them.
type DriftAuditInput struct {
	RunID                RunID
	Round                int
	BaseSHA              string
	HeadSHA              string
	ApprovedSpecDigest   Digest
	ResolvedPolicyDigest Digest
	Verdict              DriftVerdict
	Confidence           AdjudicationConfidence
	Reversals            []DriftReversal
	Explanation          string
	CreatedAt            time.Time
}

// NewDriftAudit builds one round's drift audit. The constructor detaches the
// reversal list, sorts it by finding id, computes the content digest, and
// validates. CreatedAt must be a UTC instant.
func NewDriftAudit(input DriftAuditInput) (DriftAudit, error) {
	// A nil input list becomes the empty list here, which Validate requires.
	reversals := append([]DriftReversal{}, input.Reversals...)
	slices.SortStableFunc(reversals, func(a, b DriftReversal) int {
		return strings.Compare(string(a.FindingID), string(b.FindingID))
	})
	audit := DriftAudit{
		EncodingVersion:      DriftAuditEncodingVersion,
		RunID:                input.RunID,
		Round:                input.Round,
		BaseSHA:              input.BaseSHA,
		HeadSHA:              input.HeadSHA,
		ApprovedSpecDigest:   input.ApprovedSpecDigest,
		ResolvedPolicyDigest: input.ResolvedPolicyDigest,
		Verdict:              input.Verdict,
		Confidence:           input.Confidence,
		Reversals:            reversals,
		Explanation:          input.Explanation,
		CreatedAt:            input.CreatedAt,
	}
	digest, err := audit.ComputeDigest()
	if err != nil {
		return DriftAudit{}, err
	}
	audit.Digest = digest
	if err := audit.Validate(); err != nil {
		return DriftAudit{}, err
	}
	return audit, nil
}

// Validate is the structural and content-address backstop for a constructed or
// reconstructed artifact. The reversal list's cardinality is part of the
// verdict: over_hardened requires a nonempty list, converged and stuck an empty
// one. Reversals are strictly ascending by finding id, which makes them
// distinct. The digest must match the canonical content.
func (a DriftAudit) Validate() error {
	if a.EncodingVersion != DriftAuditEncodingVersion {
		return fmt.Errorf("drift audit encoding_version %d: %w", a.EncodingVersion, ErrDriftAuditInconsistent)
	}
	if a.RunID == "" {
		return fmt.Errorf("drift audit run_id: %w", ErrEmptyID)
	}
	if a.Round < 1 {
		return fmt.Errorf("drift audit round %d: %w", a.Round, ErrNonPositive)
	}
	if a.BaseSHA == "" || a.HeadSHA == "" {
		return fmt.Errorf("drift audit commits %q..%q: %w", a.BaseSHA, a.HeadSHA, ErrEmptyField)
	}
	// The identifying strings get the reversal's invalid-UTF-8 guard for the
	// same reason: the digest is taken over the marshaled form.
	for _, field := range []struct{ label, text string }{
		{"run_id", string(a.RunID)}, {"base_sha", a.BaseSHA}, {"head_sha", a.HeadSHA},
	} {
		if !utf8.ValidString(field.text) {
			return fmt.Errorf("drift audit %s %q: %w", field.label, field.text, ErrDriftAuditInconsistent)
		}
	}
	if !contentaddr.Valid(string(a.ApprovedSpecDigest)) {
		return fmt.Errorf("drift audit approved_spec_digest %q: %w", a.ApprovedSpecDigest, ErrDriftAuditInconsistent)
	}
	if !contentaddr.Valid(string(a.ResolvedPolicyDigest)) {
		return fmt.Errorf("drift audit resolved_policy_digest %q: %w", a.ResolvedPolicyDigest, ErrDriftAuditInconsistent)
	}
	if !a.Verdict.valid() {
		return fmt.Errorf("drift audit verdict %q: %w", a.Verdict, ErrInvalidDriftVerdict)
	}
	if !a.Confidence.valid() {
		return fmt.Errorf("drift audit confidence %q: %w", a.Confidence, ErrInvalidAdjudicationConfidence)
	}
	if a.Reversals == nil {
		return fmt.Errorf("drift audit reversals absent: %w", ErrDriftAuditInconsistent)
	}
	// The switch dispatches on the verdict, so it omits default; Verdict is
	// already known valid.
	switch a.Verdict {
	case DriftVerdictOverHardened:
		if len(a.Reversals) == 0 {
			return fmt.Errorf("drift audit verdict %q without reversals: %w", a.Verdict, ErrDriftAuditInconsistent)
		}
	case DriftVerdictConverged, DriftVerdictStuck:
		if len(a.Reversals) != 0 {
			return fmt.Errorf("drift audit verdict %q with %d reversals: %w",
				a.Verdict, len(a.Reversals), ErrDriftAuditInconsistent)
		}
	}
	for i, reversal := range a.Reversals {
		if err := reversal.Validate(); err != nil {
			return err
		}
		if i > 0 && reversal.FindingID <= a.Reversals[i-1].FindingID {
			return fmt.Errorf("drift audit reversal %q order: %w", reversal.FindingID, ErrFindingsNotCanonical)
		}
	}
	if strings.TrimSpace(a.Explanation) == "" {
		return fmt.Errorf("drift audit explanation: %w", ErrEmptyField)
	}
	if !utf8.ValidString(a.Explanation) {
		return fmt.Errorf("drift audit explanation: %w", ErrDriftAuditInconsistent)
	}
	if a.CreatedAt.IsZero() {
		return fmt.Errorf("drift audit created_at: %w", ErrMissingTimestamp)
	}
	if a.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("drift audit created_at: %w", ErrTimestampNotUTC)
	}
	if !contentaddr.Valid(string(a.Digest)) {
		return fmt.Errorf("drift audit digest %q: %w", a.Digest, ErrDriftAuditDigestMismatch)
	}
	computed, err := a.ComputeDigest()
	if err != nil {
		return err
	}
	if a.Digest != computed {
		return fmt.Errorf("drift audit digest %q, content resolves to %q: %w",
			a.Digest, computed, ErrDriftAuditDigestMismatch)
	}
	return nil
}

// ComputeDigest hashes the canonical encoding, which excludes only the Digest
// field. Struct field order is part of the contract and is pinned by a golden.
func (a DriftAudit) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(canonicalDriftAudit{
		EncodingVersion:      a.EncodingVersion,
		RunID:                a.RunID,
		Round:                a.Round,
		BaseSHA:              a.BaseSHA,
		HeadSHA:              a.HeadSHA,
		ApprovedSpecDigest:   a.ApprovedSpecDigest,
		ResolvedPolicyDigest: a.ResolvedPolicyDigest,
		Verdict:              a.Verdict,
		Confidence:           a.Confidence,
		Reversals:            a.Reversals,
		Explanation:          a.Explanation,
		CreatedAt:            a.CreatedAt,
	})
	if err != nil {
		return "", fmt.Errorf("drift audit canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Encode emits the validated canonical persisted form.
func (a DriftAudit) Encode() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("drift audit encode: %w", err)
	}
	return body, nil
}

// DecodeDriftAudit rejects oversized, unknown-field, invalid-UTF8, and
// trailing-data payloads before revalidating the content address.
func DecodeDriftAudit(body []byte) (DriftAudit, error) {
	var audit DriftAudit
	if err := strictjson.Decode(body, &audit, strictjson.RejectInvalidUTF8, MaxDriftAuditBytes); err != nil {
		return DriftAudit{}, fmt.Errorf("drift audit decode: %w", err)
	}
	if err := audit.Validate(); err != nil {
		return DriftAudit{}, err
	}
	return audit, nil
}
