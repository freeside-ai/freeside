package domain

import (
	"fmt"
	"slices"
	"time"
)

// FindingLocation is a machine-actionable source location for a review
// finding: a path plus an inclusive, 1-based line range. StartLine ==
// EndLine == 0 with a path is the whole-file location (a file-level comment
// carrying no line). A nil *FindingLocation on a Finding is a review-level
// observation with no path (a native review-level comment); the §7 surface
// derivation fails closed on a nil location downstream.
type FindingLocation struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// Validate reports whether a present location is well-formed: a non-empty
// path, and either the whole-file marker (0,0) or a positive, non-inverted
// line range. A partial range (one endpoint zero, the other set), a
// non-positive endpoint, and an inverted range (start > end) are rejected.
func (l FindingLocation) Validate() error {
	if l.Path == "" {
		return fmt.Errorf("finding location path: %w", ErrEmptyField)
	}
	if l.StartLine == 0 && l.EndLine == 0 {
		return nil // whole-file location
	}
	if l.StartLine < 1 || l.EndLine < 1 {
		return fmt.Errorf("finding location range [%d,%d]: %w", l.StartLine, l.EndLine, ErrNonPositive)
	}
	if l.StartLine > l.EndLine {
		return fmt.Errorf("finding location range [%d,%d]: %w", l.StartLine, l.EndLine, ErrInvertedRange)
	}
	return nil
}

// String renders the canonical textual location and is the single shared
// derivation for every textual consumer (disposition history, annotation
// input, finding identity): "path" for the whole-file location, "path:line"
// for a single-line range, and "path:start-end" otherwise. It is defined on
// the value receiver; callers holding a nil pointer decide their own rendering
// (a nil location has no text).
func (l FindingLocation) String() string {
	if l.StartLine == 0 && l.EndLine == 0 {
		return l.Path
	}
	if l.StartLine == l.EndLine {
		return fmt.Sprintf("%s:%d", l.Path, l.StartLine)
	}
	return fmt.Sprintf("%s:%d-%d", l.Path, l.StartLine, l.EndLine)
}

// Finding is a raw, immutable observation from a review source (plan §5.12).
// It has no mutators and no verdict field: the raw finding is never edited and
// is never itself marked fixed. Interpretation lives in Classification.
type Finding struct {
	ID        FindingID        `json:"id"`
	RunID     RunID            `json:"run_id"`
	Source    string           `json:"source"`
	Severity  FindingSeverity  `json:"severity,omitempty"`
	Location  *FindingLocation `json:"location"`
	Message   string           `json:"message"`
	RawText   string           `json:"raw_text"`
	CreatedAt time.Time        `json:"created_at"`
}

// Validate reports whether the finding is well-formed. Severity is optional at
// the domain level (the native ingest observes third-party comments with no
// priority badge), but a non-empty severity must be a valid member; a present
// location must be well-formed.
func (f Finding) Validate() error {
	if f.ID == "" {
		return fmt.Errorf("finding id: %w", ErrEmptyID)
	}
	if f.RunID == "" {
		return fmt.Errorf("finding %s run_id: %w", f.ID, ErrEmptyID)
	}
	if f.Severity != "" && !f.Severity.valid() {
		return fmt.Errorf("finding %s severity %q: %w", f.ID, f.Severity, ErrInvalidFindingSeverity)
	}
	if f.Location != nil {
		if err := f.Location.Validate(); err != nil {
			return fmt.Errorf("finding %s: %w", f.ID, err)
		}
	}
	if f.CreatedAt.IsZero() {
		return fmt.Errorf("finding %s created_at: %w", f.ID, ErrMissingTimestamp)
	}
	return nil
}

// findingsEqual compares two finding slices by value. It is the value-aware
// counterpart to slices.Equal: because Finding now carries an optional
// *FindingLocation, a plain == (and thus slices.Equal) compares the location by
// pointer identity, so a fresh re-derivation of an otherwise-identical finding
// would read as a change. Callers that coalesce re-derived findings (native
// review's MaterialChangeFrom) compare the pointed-to location value instead.
func findingsEqual(a, b []Finding) bool {
	return slices.EqualFunc(a, b, func(x, y Finding) bool {
		switch {
		case x.Location == nil && y.Location == nil:
		case x.Location == nil || y.Location == nil:
			return false
		case *x.Location != *y.Location:
			return false
		}
		x.Location, y.Location = nil, nil
		return x == y
	})
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
