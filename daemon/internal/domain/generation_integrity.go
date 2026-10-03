package domain

import (
	"fmt"
	"time"
)

// CredentialIntegrityFinding is what the credential-integrity probe observed
// wrong with one enrollment generation's stored bytes (plan §5.4 admission
// rule 4, issue #1624). The vocabulary is closed: the probe classifies, it
// never describes, so a mark has no free text to leak credential content
// through. Widening it is a kind:contract change.
type CredentialIntegrityFinding string

const (
	CredentialIntegrityTruncation CredentialIntegrityFinding = "truncation"
	CredentialIntegrityCorruption CredentialIntegrityFinding = "corruption"
)

// AllCredentialIntegrityFindings lists every valid finding; it is the single
// registration point and drives table-driven tests.
var AllCredentialIntegrityFindings = []CredentialIntegrityFinding{
	CredentialIntegrityTruncation, CredentialIntegrityCorruption,
}

func (f CredentialIntegrityFinding) valid() bool {
	switch f {
	case CredentialIntegrityTruncation, CredentialIntegrityCorruption:
		return true
	default:
		return false
	}
}

// GenerationIntegrityMark records that the credential-integrity probe found
// one enrollment generation's stored credential damaged. A marked generation
// is not credentialed (§5.4 admission rule 4), so nothing new is admitted
// against it; re-enrollment appends an unmarked successor, which is how the
// refusal clears.
//
// The mark is its own record, deliberately not a field on
// EnrollmentGeneration or on an admission: generations are immutable, and a
// carried "unmarked" bit would be a decoded trust claim. Each boundary that
// decides whether work may start reads the stored marks instead. It holds no
// credential bytes and no digest of the observed bytes.
type GenerationIntegrityMark struct {
	EnrollmentID ClientEnrollmentID `json:"enrollment_id"`
	// Ordinal names a persisted generation, so it is at least 1; the
	// not-yet-persisted zero EnrollmentGeneration admits has no meaning here.
	Ordinal    int                        `json:"ordinal"`
	Finding    CredentialIntegrityFinding `json:"finding"`
	ObservedAt time.Time                  `json:"observed_at"`
}

// Validate reports whether the mark is well-formed. Whether the generation
// it names exists is the store's fact, checked where the mark is recorded
// and again where it is read back.
func (m GenerationIntegrityMark) Validate() error {
	if m.EnrollmentID == "" {
		return fmt.Errorf("generation integrity mark enrollment_id: %w", ErrEmptyID)
	}
	if m.Ordinal < 1 {
		return fmt.Errorf("generation integrity mark %s ordinal %d: %w", m.EnrollmentID, m.Ordinal, ErrNonPositive)
	}
	if !m.Finding.valid() {
		return fmt.Errorf("generation integrity mark %s/%d finding %q: %w",
			m.EnrollmentID, m.Ordinal, m.Finding, ErrInvalidCredentialIntegrityFinding)
	}
	if m.ObservedAt.IsZero() {
		return fmt.Errorf("generation integrity mark %s/%d observed_at: %w",
			m.EnrollmentID, m.Ordinal, ErrMissingTimestamp)
	}
	if m.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("generation integrity mark %s/%d observed_at: %w",
			m.EnrollmentID, m.Ordinal, ErrTimestampNotUTC)
	}
	return nil
}

// GenerationIntegrityMarkedError is the refusal for work that would start on
// a marked generation. It is the concrete form of
// ErrGenerationIntegrityMarked, so a caller can match the class (errors.Is
// against the sentinel) and report the exact generation and finding
// (errors.As).
type GenerationIntegrityMarkedError struct {
	Mark GenerationIntegrityMark
}

func (e *GenerationIntegrityMarkedError) Error() string {
	return fmt.Sprintf("enrollment %s generation %d is marked %s: %v",
		e.Mark.EnrollmentID, e.Mark.Ordinal, e.Mark.Finding, ErrGenerationIntegrityMarked)
}

// Is reports the error as an ErrGenerationIntegrityMarked so an admission
// caller can match every finding class with a single sentinel.
func (e *GenerationIntegrityMarkedError) Is(target error) bool {
	return target == ErrGenerationIntegrityMarked
}
