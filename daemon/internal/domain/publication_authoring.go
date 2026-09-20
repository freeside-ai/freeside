package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// PublicationAuthoringEncodingVersion tags the canonical encoding; a change
	// bumps it so every digest visibly changes rather than silently colliding.
	// This type has a single shape and only version 1. A later encoding version
	// must keep decoding version 1: #1419 renders stored bytes for as long as a
	// publication PR is open, so version 1 can never be dropped the way
	// FindingAdjudication dropped its own.
	PublicationAuthoringEncodingVersion = 1

	// MaxPublicationAuthoringTitleBytes bounds the title.
	MaxPublicationAuthoringTitleBytes = 256
	// MaxPublicationAuthoringBodyBytes bounds the body prose. 32 KiB leaves room
	// under GitHub's 65,536-character PR body limit for the publisher's own
	// sections (#1419 owns the final fit check).
	MaxPublicationAuthoringBodyBytes = 32 << 10
	// MaxPublicationAuthoringReviewerNotesBytes bounds the optional reviewer notes.
	MaxPublicationAuthoringReviewerNotesBytes = 8 << 10
	// MaxPublicationAuthoringOutcomeSummaryBytes bounds the outcome summary.
	MaxPublicationAuthoringOutcomeSummaryBytes = 8 << 10
	// MaxPublicationEvidenceRefs bounds the evidence reference list.
	MaxPublicationEvidenceRefs = 64
	// MaxPublicationProducerFieldBytes bounds each of the producer label's site
	// and producer strings.
	MaxPublicationProducerFieldBytes = 256
	// MaxPublicationAuthoringBytes bounds a decoded artifact body. It caps the
	// whole encoded value, over and above the per-field bounds.
	MaxPublicationAuthoringBytes = 128 << 10
)

// PublicationEvidenceReference names one already-existing evidence artifact the
// prose links: its artifact ID plus that artifact's content digest. Both are
// re-resolved and re-gated at the store boundary; neither is trusted here.
type PublicationEvidenceReference struct {
	ArtifactID ArtifactID `json:"artifact_id"`
	Digest     Digest     `json:"digest"`
}

// PublicationProducer labels who produced the prose: the explain site, the
// producing agent/model identity, and the content address of the inputs the
// call ran over. The author runs as an inference call, not a ward invocation,
// so there is no InvocationID; the input digest lets #1419 tell whether a
// stored artifact still matches the run's current inputs after a base advance.
type PublicationProducer struct {
	Site        string `json:"site"`
	Producer    string `json:"producer"`
	InputDigest Digest `json:"input_digest"`
}

// PublicationAuthoring is the advisory, schema-validated, producer-labeled,
// digest-addressed record of the prose the `explain` site writes for a
// publication (plan §5.13, §9). It is deliberately not a domain.Artifact: it has
// its own type and its own store table, which keeps it out of evidence
// snapshots, the publish-eligibility computation, and the synced artifact list,
// so policy evaluation cannot reach it. It carries no trust bit and no
// publish_eligible field. Digest is computed by the constructor, never
// caller-supplied.
type PublicationAuthoring struct {
	EncodingVersion  int                            `json:"encoding_version"`
	RunID            RunID                          `json:"run_id"`
	Title            string                         `json:"title"`
	Body             string                         `json:"body"`
	ReviewerNotes    *string                        `json:"reviewer_notes"`
	EvidenceRefs     []PublicationEvidenceReference `json:"evidence_refs"`
	OutcomeSummary   string                         `json:"outcome_summary"`
	Producer         PublicationProducer            `json:"producer"`
	SensitivityClass SensitivityClass               `json:"sensitivity_class"`
	CreatedAt        time.Time                      `json:"created_at"`
	Digest           Digest                         `json:"digest"`
}

type canonicalPublicationAuthoring struct {
	EncodingVersion  int                            `json:"encoding_version"`
	RunID            RunID                          `json:"run_id"`
	Title            string                         `json:"title"`
	Body             string                         `json:"body"`
	ReviewerNotes    *string                        `json:"reviewer_notes"`
	EvidenceRefs     []PublicationEvidenceReference `json:"evidence_refs"`
	OutcomeSummary   string                         `json:"outcome_summary"`
	Producer         PublicationProducer            `json:"producer"`
	SensitivityClass SensitivityClass               `json:"sensitivity_class"`
	CreatedAt        time.Time                      `json:"created_at"`
}

// PublicationAuthoringInput carries the fields a caller supplies to the
// constructor. The digest and encoding version are set by the constructor, not
// the caller.
type PublicationAuthoringInput struct {
	RunID            RunID
	Title            string
	Body             string
	ReviewerNotes    *string
	EvidenceRefs     []PublicationEvidenceReference
	OutcomeSummary   string
	Producer         PublicationProducer
	SensitivityClass SensitivityClass
	CreatedAt        time.Time
}

// NewPublicationAuthoring builds a validated, digest-addressed authoring
// artifact. A nil evidence-reference list is normalized to an empty one so the
// digest does not depend on how the caller built the slice, and so an empty list
// encodes as `[]` rather than `null`. createdAt must be a UTC instant. The
// constructor computes the digest and never accepts one from the caller.
func NewPublicationAuthoring(in PublicationAuthoringInput) (PublicationAuthoring, error) {
	// Clone the mutable inputs (the reviewer-notes pointer and the reference
	// slice) so a caller that reuses its input after construction cannot mutate
	// the digest-addressed artifact out from under its computed digest. The
	// reference element is a value type, so a shallow slice clone fully isolates
	// it. A nil reference list normalizes to empty, so the digest does not depend
	// on how the caller built the slice and an empty list encodes as `[]`.
	refs := slices.Clone(in.EvidenceRefs)
	if refs == nil {
		refs = []PublicationEvidenceReference{}
	}
	artifact := PublicationAuthoring{
		EncodingVersion:  PublicationAuthoringEncodingVersion,
		RunID:            in.RunID,
		Title:            in.Title,
		Body:             in.Body,
		ReviewerNotes:    clonePtr(in.ReviewerNotes),
		EvidenceRefs:     refs,
		OutcomeSummary:   in.OutcomeSummary,
		Producer:         in.Producer,
		SensitivityClass: in.SensitivityClass,
		CreatedAt:        in.CreatedAt,
	}
	digest, err := artifact.ComputeDigest()
	if err != nil {
		return PublicationAuthoring{}, err
	}
	artifact.Digest = digest
	if err := artifact.Validate(); err != nil {
		return PublicationAuthoring{}, err
	}
	return artifact, nil
}

// Validate is the structural and content-address backstop for a reconstructed
// or hand-built artifact, including a value that bypassed the constructor.
func (a PublicationAuthoring) Validate() error {
	if a.EncodingVersion != PublicationAuthoringEncodingVersion {
		return fmt.Errorf("publication authoring encoding_version %d: %w", a.EncodingVersion, ErrPublicationAuthoringInconsistent)
	}
	if a.RunID == "" {
		return fmt.Errorf("publication authoring run_id: %w", ErrEmptyID)
	}
	// Every free-text field is UTF-8-validated before it can be hashed or
	// persisted: json.Marshal silently rewrites invalid bytes to U+FFFD, so an
	// unguarded field would let the stored, digest-addressed artifact differ
	// from the content the caller submitted while still validating.
	for _, f := range []struct {
		name  string
		value string
		max   int
	}{
		{"title", a.Title, MaxPublicationAuthoringTitleBytes},
		{"body", a.Body, MaxPublicationAuthoringBodyBytes},
		{"outcome_summary", a.OutcomeSummary, MaxPublicationAuthoringOutcomeSummaryBytes},
	} {
		if f.value == "" {
			return fmt.Errorf("publication authoring %s: %w", f.name, ErrEmptyField)
		}
		if !utf8.ValidString(f.value) {
			return fmt.Errorf("publication authoring %s: %w", f.name, ErrPublicationAuthoringInconsistent)
		}
		if len(f.value) > f.max {
			return fmt.Errorf("publication authoring %s exceeds %d bytes: %w", f.name, f.max, ErrPublicationAuthoringInconsistent)
		}
	}
	if a.ReviewerNotes != nil {
		if !utf8.ValidString(*a.ReviewerNotes) {
			return fmt.Errorf("publication authoring reviewer_notes: %w", ErrPublicationAuthoringInconsistent)
		}
		if len(*a.ReviewerNotes) > MaxPublicationAuthoringReviewerNotesBytes {
			return fmt.Errorf("publication authoring reviewer_notes exceeds %d bytes: %w",
				MaxPublicationAuthoringReviewerNotesBytes, ErrPublicationAuthoringInconsistent)
		}
	}
	if err := a.validateEvidenceRefs(); err != nil {
		return err
	}
	if err := a.validateProducer(); err != nil {
		return err
	}
	if !a.SensitivityClass.valid() {
		return fmt.Errorf("publication authoring sensitivity_class %q: %w", a.SensitivityClass, ErrInvalidSensitivityClass)
	}
	if a.CreatedAt.IsZero() {
		return fmt.Errorf("publication authoring created_at: %w", ErrMissingTimestamp)
	}
	if a.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("publication authoring created_at: %w", ErrTimestampNotUTC)
	}
	if !contentaddr.Valid(string(a.Digest)) {
		return fmt.Errorf("publication authoring digest %q: %w", a.Digest, ErrPublicationAuthoringDigestMismatch)
	}
	computed, err := a.ComputeDigest()
	if err != nil {
		return err
	}
	if a.Digest != computed {
		return fmt.Errorf("publication authoring digest %q, content resolves to %q: %w",
			a.Digest, computed, ErrPublicationAuthoringDigestMismatch)
	}
	return nil
}

// validateEvidenceRefs enforces the bounded, duplicate-free, non-nil-and-
// complete reference list. An empty list is valid (a publication may cite no
// evidence artifact); a nil list is not, because the constructor normalizes nil
// to empty and a nil here signals a value that bypassed it inconsistently with
// its digest.
func (a PublicationAuthoring) validateEvidenceRefs() error {
	if a.EvidenceRefs == nil {
		return fmt.Errorf("publication authoring evidence_refs: %w", ErrPublicationAuthoringInconsistent)
	}
	if len(a.EvidenceRefs) > MaxPublicationEvidenceRefs {
		return fmt.Errorf("publication authoring evidence_refs %d exceeds %d: %w",
			len(a.EvidenceRefs), MaxPublicationEvidenceRefs, ErrPublicationAuthoringInconsistent)
	}
	seen := make(map[ArtifactID]struct{}, len(a.EvidenceRefs))
	for _, ref := range a.EvidenceRefs {
		if ref.ArtifactID == "" {
			return fmt.Errorf("publication authoring evidence reference id: %w", ErrEmptyID)
		}
		// The reference names an existing artifact's own digest, whose spelling
		// is whatever that artifact carries, so only non-emptiness is checked
		// here; the store gate resolves the artifact and requires an exact match.
		if ref.Digest == "" {
			return fmt.Errorf("publication authoring evidence reference %q digest: %w", ref.ArtifactID, ErrEmptyField)
		}
		if _, dup := seen[ref.ArtifactID]; dup {
			return fmt.Errorf("publication authoring evidence reference %q: %w", ref.ArtifactID, ErrDuplicate)
		}
		seen[ref.ArtifactID] = struct{}{}
	}
	return nil
}

// validateProducer enforces the label shape: non-empty, bounded, valid-UTF-8
// site and producer strings, and a content-addressed input digest.
func (a PublicationAuthoring) validateProducer() error {
	for _, f := range []struct {
		name  string
		value string
	}{
		{"producer.site", a.Producer.Site},
		{"producer.producer", a.Producer.Producer},
	} {
		if f.value == "" {
			return fmt.Errorf("publication authoring %s: %w", f.name, ErrEmptyField)
		}
		if !utf8.ValidString(f.value) {
			return fmt.Errorf("publication authoring %s: %w", f.name, ErrPublicationAuthoringInconsistent)
		}
		if len(f.value) > MaxPublicationProducerFieldBytes {
			return fmt.Errorf("publication authoring %s exceeds %d bytes: %w",
				f.name, MaxPublicationProducerFieldBytes, ErrPublicationAuthoringInconsistent)
		}
	}
	if !contentaddr.Valid(string(a.Producer.InputDigest)) {
		return fmt.Errorf("publication authoring producer.input_digest %q: %w",
			a.Producer.InputDigest, ErrPublicationAuthoringInconsistent)
	}
	return nil
}

func (a PublicationAuthoring) canonical() canonicalPublicationAuthoring {
	return canonicalPublicationAuthoring{
		EncodingVersion:  a.EncodingVersion,
		RunID:            a.RunID,
		Title:            a.Title,
		Body:             a.Body,
		ReviewerNotes:    a.ReviewerNotes,
		EvidenceRefs:     a.EvidenceRefs,
		OutcomeSummary:   a.OutcomeSummary,
		Producer:         a.Producer,
		SensitivityClass: a.SensitivityClass,
		CreatedAt:        a.CreatedAt,
	}
}

// ComputeDigest hashes the canonical encoding, which is every field except the
// digest. Struct field order is part of the contract and is pinned by a golden.
func (a PublicationAuthoring) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(a.canonical())
	if err != nil {
		return "", fmt.Errorf("publication authoring canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Encode emits the validated canonical persisted form. It caps the encoded
// size at MaxPublicationAuthoringBytes, the same limit DecodePublicationAuthoring
// enforces: json.Marshal escapes control characters (a valid-UTF-8 control byte
// becomes a six-byte \uXXXX), so a field within its raw byte bound can still
// expand past the decode cap. Enforcing the cap here, symmetric with decode,
// prevents persisting an immutable row that no later read can reconstruct.
func (a PublicationAuthoring) Encode() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("publication authoring encode: %w", err)
	}
	if len(body) > MaxPublicationAuthoringBytes {
		return nil, fmt.Errorf("publication authoring encoded size %d exceeds %d bytes: %w",
			len(body), MaxPublicationAuthoringBytes, ErrPublicationAuthoringInconsistent)
	}
	return body, nil
}

// DecodePublicationAuthoring rejects oversized, unknown-field, invalid-UTF8, and
// trailing-data payloads before revalidating the content address.
func DecodePublicationAuthoring(body []byte) (PublicationAuthoring, error) {
	var artifact PublicationAuthoring
	if err := strictjson.Decode(body, &artifact, strictjson.RejectInvalidUTF8, MaxPublicationAuthoringBytes); err != nil {
		return PublicationAuthoring{}, fmt.Errorf("publication authoring decode: %w", err)
	}
	if err := artifact.Validate(); err != nil {
		return PublicationAuthoring{}, err
	}
	return artifact, nil
}
