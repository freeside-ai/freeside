package domain

import (
	"fmt"
	"time"
)

// EvidenceMetadata carries the §5.15 daemon-validated facts about an evidence
// or claim reference on the sync contract, so a client renders an explicit
// evidence state from typed fields alone rather than inferring it from a fetch
// outcome. The daemon already validates magic bytes, media type, and size
// (§5.15 rule 3); this type carries those facts through instead of dropping
// them. It is required on every Artifact (as run evidence) and every
// AgentClaim (as a claim), and its Source is pinned to the container by the
// enclosing type's Validate.
//
// Availability is a daemon read-time fact, recomputed by the sync projection
// from the blob store immediately before serialization (see
// signet.projectEvidenceAvailability), on the same re-gate pattern as
// publish_eligible. A persisted value is never trusted for the wire, so a
// producer stamps EvidenceAvailable at construction (its bytes are stored
// before the artifact is built) and the projection overwrites it per synced
// item. "Oversized" and "unsupported" are deliberately absent: they are client
// dispositions derived from SizeBytes and MediaType against a device-specific
// render cap, and over-cap content never passes the daemon's own validation
// gates, so the daemon carries no such disposition.
type EvidenceMetadata struct {
	MediaType    EvidenceMediaType    `json:"media_type"`
	SizeBytes    int64                `json:"size_bytes"`
	CreatedAt    time.Time            `json:"created_at"`
	Source       EvidenceSource       `json:"source"`
	Availability EvidenceAvailability `json:"availability"`
}

// Validate reports whether the metadata is well-formed: valid enums, a
// non-negative size, and a real UTC creation instant. It does not pin Source to
// a container; the enclosing Artifact or AgentClaim does that, because only the
// container knows which channel it is.
func (m EvidenceMetadata) Validate() error {
	if !m.MediaType.valid() {
		return fmt.Errorf("evidence metadata media_type %q: %w", m.MediaType, ErrInvalidEvidenceMediaType)
	}
	if m.SizeBytes < 0 {
		return fmt.Errorf("evidence metadata size_bytes %d: %w", m.SizeBytes, ErrNegativeSize)
	}
	if !m.Source.valid() {
		return fmt.Errorf("evidence metadata source %q: %w", m.Source, ErrInvalidEvidenceSource)
	}
	if !m.Availability.valid() {
		return fmt.Errorf("evidence metadata availability %q: %w", m.Availability, ErrInvalidEvidenceAvailability)
	}
	if m.CreatedAt.IsZero() {
		return fmt.Errorf("evidence metadata created_at: %w", ErrMissingTimestamp)
	}
	// The item body is a canonical persisted encoding whose re-put convergence
	// is a byte compare, so one instant must have one spelling; a non-UTC
	// created_at is the same moment in a different byte form (mirrors DecidedAt).
	if m.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("evidence metadata created_at: %w", ErrTimestampNotUTC)
	}
	return nil
}
