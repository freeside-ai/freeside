package domain

import (
	"fmt"
	"time"
)

// Finding is a raw, immutable observation from a review source (plan §5.12).
// It has no mutators and no verdict field: the raw finding is never edited and
// is never itself marked fixed. Interpretation lives in Classification.
type Finding struct {
	ID        FindingID `json:"id"`
	RunID     RunID     `json:"run_id"`
	Source    string    `json:"source"`
	Location  string    `json:"location"`
	Message   string    `json:"message"`
	RawText   string    `json:"raw_text"`
	CreatedAt time.Time `json:"created_at"`
}

// Validate reports whether the finding is well-formed.
func (f Finding) Validate() error {
	if f.ID == "" {
		return fmt.Errorf("finding id: %w", ErrEmptyID)
	}
	if f.RunID == "" {
		return fmt.Errorf("finding %s run_id: %w", f.ID, ErrEmptyID)
	}
	if f.CreatedAt.IsZero() {
		return fmt.Errorf("finding %s created_at: %w", f.ID, ErrMissingTimestamp)
	}
	return nil
}

// Classification is a versioned annotation over a raw Finding (plan §5.12). It
// deliberately has no "fixed" verdict: the classifier can never declare a
// finding fixed, only annotate its materiality. A correction is a new version,
// produced by Annotate; the annotation is never mutated in place.
type Classification struct {
	FindingID   FindingID `json:"finding_id"`
	Version     int       `json:"version"`
	Materiality string    `json:"materiality"`
	Confidence  string    `json:"confidence"`
	Note        string    `json:"note"`
}

// Validate reports whether the classification is well-formed.
func (c Classification) Validate() error {
	if c.FindingID == "" {
		return fmt.Errorf("classification finding_id: %w", ErrEmptyID)
	}
	if c.Version < 1 {
		return fmt.Errorf("classification version %d: %w", c.Version, ErrNonPositive)
	}
	return nil
}

// Annotate returns the next version of a classification with revised
// materiality, confidence, and note. It returns a new value rather than
// mutating the receiver: corrections are new versions (plan §5.12).
func (c Classification) Annotate(materiality, confidence, note string) Classification {
	c.Version++
	c.Materiality = materiality
	c.Confidence = confidence
	c.Note = note
	return c
}
