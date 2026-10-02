package domain

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
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

// ExternalFindingProvenance says where a finding from outside Freeside came
// from (plan §5.19): who left it, on which thread, and on which published
// head. It records facts only. It has no authority and no admission bit:
// whether the reviewer may drive a round is the trust profile's answer at the
// moment of the read, so a stored finding can never carry a stale or forged
// "admitted".
type ExternalFindingProvenance struct {
	Class             FindingProvenanceClass `json:"class"`
	Forge             ExternalReviewForge    `json:"forge"`
	ReviewerAccountID int64                  `json:"reviewer_account_id"`
	ReviewerLogin     string                 `json:"reviewer_login"`
	// ThreadID is the forge's identifier for the review thread the finding
	// was left on.
	ThreadID string `json:"thread_id"`
	// RawSourceDigest is the content address of the finding's RawText, so the
	// text an authority was triggered by can be named without repeating it.
	RawSourceDigest Digest `json:"raw_source_digest"`
	// HeadSHA is the published head the finding was left on.
	HeadSHA string `json:"head_sha"`
}

// ExternalFindingSource is the Source label of every external finding from
// one forge. Source feeds Fingerprint, so the label is reserved in both
// directions: an external finding cannot claim a Freeside review source's
// identity, and no Freeside review source can claim this one.
func ExternalFindingSource(forge ExternalReviewForge) string {
	return "external_" + string(forge)
}

// externalFindingIDVersion tags the external finding ID derivation, as
// findingFingerprintVersion does for the fingerprint.
const externalFindingIDVersion = "extv1"

// externalFindingID derives the ID from who left the finding, where, and what
// it says, so a replayed ingest converges on one row and an edited comment is
// a new finding. The account is part of it so that someone else posting the
// same text in the same thread cannot occupy an admitted reviewer's row.
func externalFindingID(runID RunID, p ExternalFindingProvenance) FindingID {
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s",
		externalFindingIDVersion, runID, p.Forge, p.ReviewerAccountID, p.ThreadID, p.RawSourceDigest, p.HeadSHA)
	sum := sha256.Sum256([]byte(identity))
	return FindingID(fmt.Sprintf("external-%x", sum[:16]))
}

// ExternalFindingInput carries the caller-supplied fields of an external
// finding. It has no ID, Source, Class, or RawSourceDigest: all four are
// derived, so no input path can set them.
type ExternalFindingInput struct {
	RunID             RunID
	Forge             ExternalReviewForge
	ReviewerAccountID int64
	ReviewerLogin     string
	ThreadID          string
	HeadSHA           string
	Severity          FindingSeverity
	Location          *FindingLocation
	Message           string
	RawText           string
	// CreatedAt should be the forge's own timestamp for the comment: the
	// stored finding is immutable, so a replayed ingest converges only when
	// every field, this one included, is the same.
	CreatedAt time.Time
}

// NewExternalFinding builds a validated external finding (plan §5.19).
func NewExternalFinding(in ExternalFindingInput) (Finding, error) {
	provenance := ExternalFindingProvenance{
		Class:             FindingProvenanceExternalUntrusted,
		Forge:             in.Forge,
		ReviewerAccountID: in.ReviewerAccountID,
		ReviewerLogin:     in.ReviewerLogin,
		ThreadID:          in.ThreadID,
		RawSourceDigest:   Digest(contentaddr.Sum([]byte(in.RawText))),
		HeadSHA:           in.HeadSHA,
	}
	var location *FindingLocation
	if in.Location != nil {
		location = new(*in.Location)
	}
	f := Finding{
		ID:        externalFindingID(in.RunID, provenance),
		RunID:     in.RunID,
		Source:    ExternalFindingSource(in.Forge),
		Severity:  in.Severity,
		Location:  location,
		Message:   in.Message,
		RawText:   in.RawText,
		CreatedAt: in.CreatedAt,
		External:  &provenance,
	}
	if err := f.Validate(); err != nil {
		return Finding{}, err
	}
	return f, nil
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
	// External is set only on a finding from outside Freeside. It is omitted
	// when nil, not rendered as null: stored findings converge on
	// byte-identical bodies, so every finding stored before the field existed
	// must keep its bytes.
	External *ExternalFindingProvenance `json:"external,omitempty"`
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
	if f.External == nil {
		for _, forge := range AllExternalReviewForges {
			if f.Source == ExternalFindingSource(forge) {
				return fmt.Errorf("finding %s source %q without external provenance: %w",
					f.ID, f.Source, ErrExternalFindingInconsistent)
			}
		}
		return nil
	}
	return f.validateExternal()
}

// validateExternal is the trust boundary over an external finding. Every
// string on it is third-party text, so each is held to the native-review
// UTF-8 and size limits, and every derived field is recomputed instead of
// read: the ID, the source label, and the raw-source digest of a decoded or
// caller-built finding must be what its content resolves to.
func (f Finding) validateExternal() error {
	p := *f.External
	if !p.Class.valid() {
		return fmt.Errorf("finding %s external class %q: %w", f.ID, p.Class, ErrInvalidFindingProvenanceClass)
	}
	if !p.Forge.valid() {
		return fmt.Errorf("finding %s external forge %q: %w", f.ID, p.Forge, ErrInvalidExternalReviewForge)
	}
	if p.ReviewerAccountID <= 0 {
		return fmt.Errorf("finding %s external reviewer_account_id %d: %w", f.ID, p.ReviewerAccountID, ErrNonPositive)
	}
	if err := validateExternalReviewerLogin(p.ReviewerLogin); err != nil {
		return fmt.Errorf("finding %s: %w", f.ID, err)
	}
	for _, required := range []struct{ name, value string }{
		{"thread_id", p.ThreadID}, {"head_sha", p.HeadSHA}, {"raw_text", f.RawText},
	} {
		if required.value == "" {
			return fmt.Errorf("finding %s external %s: %w", f.ID, required.name, ErrEmptyField)
		}
	}
	bounded := []struct{ name, value string }{
		{"reviewer_login", p.ReviewerLogin},
		{"thread_id", p.ThreadID},
		{"head_sha", p.HeadSHA},
		{"message", f.Message},
		{"raw_text", f.RawText},
	}
	if f.Location != nil {
		bounded = append(bounded, struct{ name, value string }{"location path", f.Location.Path})
	}
	for _, tv := range bounded {
		if err := nativeTextBounded(tv.value); err != nil {
			return fmt.Errorf("finding %s external %s: %w", f.ID, tv.name, err)
		}
	}
	if want := Digest(contentaddr.Sum([]byte(f.RawText))); p.RawSourceDigest != want {
		return fmt.Errorf("finding %s external raw_source_digest %q, raw text resolves to %q: %w",
			f.ID, p.RawSourceDigest, want, ErrExternalFindingInconsistent)
	}
	if want := ExternalFindingSource(p.Forge); f.Source != want {
		return fmt.Errorf("finding %s source %q, external forge requires %q: %w",
			f.ID, f.Source, want, ErrExternalFindingInconsistent)
	}
	if want := externalFindingID(f.RunID, p); f.ID != want {
		return fmt.Errorf("finding %s, external provenance resolves to %s: %w",
			f.ID, want, ErrExternalFindingInconsistent)
	}
	return nil
}

// findingsEqual compares two finding slices by value. It is the value-aware
// counterpart to slices.Equal: because Finding carries optional pointers (the
// location and the external provenance), a plain == (and thus slices.Equal)
// compares them by pointer identity, so a fresh re-derivation of an
// otherwise-identical finding would read as a change. Callers that coalesce
// re-derived findings (native review's MaterialChangeFrom) compare the
// pointed-to values instead.
func findingsEqual(a, b []Finding) bool {
	return slices.EqualFunc(a, b, func(x, y Finding) bool {
		if !pointeesEqual(x.Location, y.Location) || !pointeesEqual(x.External, y.External) {
			return false
		}
		x.Location, y.Location = nil, nil
		x.External, y.External = nil, nil
		return x == y
	})
}

func pointeesEqual[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// FindingFingerprint is a deterministic cross-round semantic identity for a
// raw Finding: stable across the same-base, different-head remediation rounds
// of one work unit, and independent of the invocation and candidate head. It
// is the identity the §7 fixed-disposition absence proof keys on — a finding
// counts as fixed only when its fingerprint no longer appears in the
// remediation review — so it must not carry any field that legitimately
// changes between those rounds. It is a pure derivation, never stored and
// never source-supplied: both rounds always recompute under one derivation
// version, which is why no migration or persisted field is needed.
type FindingFingerprint string

// findingFingerprintVersion tags the derivation. It is embedded in the hashed
// input and rendered as the fingerprint's prefix, so a derivation change bumps
// the version and every fingerprint visibly changes rather than silently
// colliding across versions.
const findingFingerprintVersion = "fpv1"

// NormalizeFindingMessage collapses a finding message to its cross-round
// comparison form: leading and trailing whitespace trimmed and every internal
// whitespace run reduced to a single space, with no case folding. It is the
// single shared derivation for the whitespace-normalized message, used by
// Fingerprint (finding identity), the finding-adjudication proposal producer,
// and the store re-gate, so a message projected into an adjudication proposal
// and the message recomputed from the stored Finding cannot diverge on cosmetic
// reflowing alone.
func NormalizeFindingMessage(message string) string {
	return strings.Join(strings.Fields(message), " ")
}

// Fingerprint derives the finding's cross-round semantic identity over the
// immutable persisted fields that survive remediation: the review Source, the
// location Path, and the whitespace-normalized Message. Everything that
// legitimately differs across the remediation rounds of one unit is excluded
// by design:
//
//   - ID, RunID: the ID hashes the invocation and candidate head, and RunID is
//     per-run; both change for the required same-base, different-head review.
//   - Severity: a round may re-tag the same defect.
//   - Location line range: remediation edits shift the lines a finding points
//     at, so only the Path is identity-bearing.
//   - CreatedAt: per-emission.
//
// It fails closed with ErrUnfingerprintableFinding when the finding carries no
// location, an empty path, or a Message that is empty after normalization: a
// finding with no computable fingerprint can never satisfy the absence proof,
// so the safe direction is to refuse an identity rather than invent one. The
// Message is normalized by trimming and collapsing every internal whitespace
// run to a single space (no case folding), so cosmetic reflowing of the same
// explanation compares equal while a genuinely reworded finding does not.
func (f Finding) Fingerprint() (FindingFingerprint, error) {
	if f.Location == nil || f.Location.Path == "" {
		return "", fmt.Errorf("finding fingerprint: %w", ErrUnfingerprintableFinding)
	}
	message := NormalizeFindingMessage(f.Message)
	if message == "" {
		return "", fmt.Errorf("finding fingerprint: %w", ErrUnfingerprintableFinding)
	}
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%s",
		findingFingerprintVersion, f.Source, f.Location.Path, message)
	sum := sha256.Sum256([]byte(identity))
	return FindingFingerprint(fmt.Sprintf("%s-%x", findingFingerprintVersion, sum[:12])), nil
}

// FindingIdentityAbsent reports whether a prior finding's cross-round identity
// is absent from the current review's findings — the §7 fixed-disposition
// primitive: a finding is a candidate for `fixed` only when it is absent here.
// It fails closed (ErrUnfingerprintableFinding) when the prior finding or any
// current finding has no computable fingerprint: an unfingerprintable current
// finding could be the re-emission the proof must not miss, so absence cannot
// be asserted while any identity is undecidable. When every fingerprint is
// computable, it returns true iff no current fingerprint equals the prior's.
func FindingIdentityAbsent(prior Finding, current []Finding) (bool, error) {
	priorFP, err := prior.Fingerprint()
	if err != nil {
		return false, fmt.Errorf("prior finding: %w", err)
	}
	// Validate every current finding before deciding: an unfingerprintable
	// current finding fails the whole comparison closed regardless of where it
	// sits, so a match found before it can never mask it.
	found := false
	for i, c := range current {
		currentFP, err := c.Fingerprint()
		if err != nil {
			return false, fmt.Errorf("current finding %d: %w", i, err)
		}
		if currentFP == priorFP {
			found = true
		}
	}
	return !found, nil
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
