package domain

import (
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

// MaxNativeReviewTextBytes caps a native finding's inline text (message and raw
// body). Native review bodies are third-party content crossing into the
// durable store (plan §7, issue #497); the cap bounds a forged or runaway body
// while staying well beyond a real review comment, and Validate enforces it so
// an oversized row fails closed on reconstruction.
const MaxNativeReviewTextBytes = 1 << 16

// NativeReviewProvider names the origin of a native (forge-hosted) review
// signal. The zero value "" is invalid by design. This vocabulary is
// deliberately separate from the readiness-authority ReviewMode enum: native
// review is observation-only evidence and never satisfies the review
// requirement (plan §7), so it must never share the authority vocabulary.
type NativeReviewProvider string

// NativeReviewCodexGitHub is GitHub-native Codex review activity (AGENTS.md,
// Automated reviewer).
const NativeReviewCodexGitHub NativeReviewProvider = "codex_github"

// AllNativeReviewProviders is the single registration point for providers.
var AllNativeReviewProviders = []NativeReviewProvider{NativeReviewCodexGitHub}

func (p NativeReviewProvider) valid() bool {
	switch p {
	case NativeReviewCodexGitHub:
		return true
	default:
		return false
	}
}

// NativeReviewKind distinguishes a substantive native review carrying findings
// from a bare clean-pass signal. Codex's clean pass posts no review, only a
// reaction on the PR description (AGENTS.md, Automated reviewer), so the two
// arrive on different endpoints and normalize to different kinds. The zero
// value "" is invalid by design.
type NativeReviewKind string

const (
	// NativeReviewFindings is a native review that carries findings; it binds
	// to the head the review named (its commit_id).
	NativeReviewFindings NativeReviewKind = "findings_review"
	// NativeReviewCleanPass is a bare clean-pass signal (a reaction) that
	// carries no findings and binds to no commit of its own.
	NativeReviewCleanPass NativeReviewKind = "clean_pass_signal"
)

// AllNativeReviewKinds is the single registration point for kinds.
var AllNativeReviewKinds = []NativeReviewKind{NativeReviewFindings, NativeReviewCleanPass}

func (k NativeReviewKind) valid() bool {
	switch k {
	case NativeReviewFindings, NativeReviewCleanPass:
		return true
	default:
		return false
	}
}

// NativeReviewObservation is a durable, readiness-inert record of native
// (forge-hosted) review activity observed on a ready item's exact PR (plan
// §5.16, §7; issue #497), appended per identity on material change only, so
// the record history is the native signal's timeline rather than a polling
// log.
//
// It is best-effort extra evidence: it carries no trust bit, and no field on
// it can create, restore, or substitute readiness or suppress re-review, which
// stays gated on the exact Freeside-invoked ReviewRecord (plan §6). An edited
// or dismissed native review appends a new observation rather than rewriting
// the prior one.
type NativeReviewObservation struct {
	Repo         string               `json:"repo"`
	RepositoryID int64                `json:"repository_id"`
	PRNumber     int                  `json:"pr_number"`
	Provider     NativeReviewProvider `json:"provider"`
	Kind         NativeReviewKind     `json:"kind"`
	// NativeID is the forge's own identifier for the signal (a review id for a
	// findings_review, a reaction id for a clean_pass_signal); with the
	// repository, PR number, provider, and kind it forms the append identity.
	NativeID    int64  `json:"native_id"`
	AuthorLogin string `json:"author_login"`
	// ReviewCommitSHA is the head the native review bound to (its commit_id).
	// It is empty for a clean_pass_signal, which binds to no commit. A value
	// that differs from BindingHeadSHA is a stale-head review: it is recorded
	// with the divergence visible, never dropped.
	ReviewCommitSHA string `json:"review_commit_sha,omitempty"`
	// ReviewState is the native review's submitted state (a third-party token
	// such as COMMENTED, CHANGES_REQUESTED, or DISMISSED). It is required for a
	// findings_review (a submitted review always names a state) and empty for a
	// clean_pass_signal (a reaction has no review state). It takes part in
	// MaterialChangeFrom so a state-only transition (a dismissal that leaves the
	// inline comments unchanged) still appends a new observation rather than
	// coalescing, keeping the append timeline's edited-or-dismissed promise true.
	ReviewState string `json:"review_state,omitempty"`
	// BindingHeadSHA is the ready item's bound head at observation time,
	// recorded so a stale-head native review's divergence from the live
	// candidate is durable.
	BindingHeadSHA string    `json:"binding_head_sha"`
	SubmittedAt    time.Time `json:"submitted_at"`
	ObservedAt     time.Time `json:"observed_at"`
	// Findings are the normalized native findings, reusing Finding with source
	// equal to the provider. Empty for a clean_pass_signal.
	Findings []Finding `json:"findings,omitempty"`
}

// Validate reports whether the observation is well-formed, internally
// consistent, and safe to persist. As a trust boundary over third-party
// content (plan §7), it enforces valid UTF-8 and a size cap on every inline
// finding body so a forged or malformed row fails closed on reconstruction.
func (o NativeReviewObservation) Validate() error {
	if o.Repo == "" {
		return fmt.Errorf("native review observation repo: %w", ErrEmptyField)
	}
	if o.RepositoryID <= 0 {
		return fmt.Errorf("native review observation repository_id %d: %w", o.RepositoryID, ErrNonPositive)
	}
	if o.PRNumber <= 0 {
		return fmt.Errorf("native review observation pr_number %d: %w", o.PRNumber, ErrNonPositive)
	}
	if !o.Provider.valid() {
		return fmt.Errorf("native review observation provider %q: %w", o.Provider, ErrInvalidNativeReviewProvider)
	}
	if !o.Kind.valid() {
		return fmt.Errorf("native review observation kind %q: %w", o.Kind, ErrInvalidNativeReviewKind)
	}
	if o.NativeID <= 0 {
		return fmt.Errorf("native review observation native_id %d: %w", o.NativeID, ErrNonPositive)
	}
	if o.AuthorLogin == "" {
		return fmt.Errorf("native review observation author_login: %w", ErrEmptyField)
	}
	if o.BindingHeadSHA == "" {
		return fmt.Errorf("native review observation binding_head_sha: %w", ErrEmptyField)
	}
	// author_login, review_commit_sha, and review_state are third-party strings
	// that reach the store and take part in the material-change comparison, so
	// they carry the same UTF-8 and size trust boundary as the finding bodies
	// below: an invalid-UTF-8 value would fail the json.Marshal round-trip
	// (issue #180) and churn the append timeline forever.
	if err := nativeTextBounded(o.AuthorLogin); err != nil {
		return fmt.Errorf("native review observation author_login: %w", err)
	}
	if err := nativeTextBounded(o.ReviewCommitSHA); err != nil {
		return fmt.Errorf("native review observation review_commit_sha: %w", err)
	}
	if err := nativeTextBounded(o.ReviewState); err != nil {
		return fmt.Errorf("native review observation review_state: %w", err)
	}
	if err := o.validateKindConsistency(); err != nil {
		return err
	}
	if o.SubmittedAt.IsZero() {
		return fmt.Errorf("native review observation submitted_at: %w", ErrMissingTimestamp)
	}
	if o.SubmittedAt.Location() != time.UTC {
		return fmt.Errorf("native review observation submitted_at: %w", ErrTimestampNotUTC)
	}
	if o.ObservedAt.IsZero() {
		return fmt.Errorf("native review observation observed_at: %w", ErrMissingTimestamp)
	}
	if o.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("native review observation observed_at: %w", ErrTimestampNotUTC)
	}
	for _, f := range o.Findings {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("native review observation finding: %w", err)
		}
		if f.Source != string(o.Provider) {
			return fmt.Errorf("native review observation finding %s source %q: %w",
				f.ID, f.Source, ErrNativeReviewInconsistent)
		}
		// message, raw_text, and location are all third-party strings that reach
		// the store and take part in the material-change comparison (Finding.Validate
		// does not check location), so each carries the UTF-8 and size boundary.
		for _, tv := range []struct{ name, value string }{
			{"message", f.Message}, {"raw_text", f.RawText}, {"location", f.Location},
		} {
			if err := nativeTextBounded(tv.value); err != nil {
				return fmt.Errorf("native review observation finding %s %s: %w", f.ID, tv.name, err)
			}
		}
	}
	return nil
}

// nativeTextBounded reports whether a third-party string is safe to persist: it
// must be valid UTF-8 (or json.Marshal rewrites its bytes to U+FFFD and the
// stored body no longer round-trips, issue #180) and within the inline cap.
func nativeTextBounded(s string) error {
	if !utf8.ValidString(s) {
		return ErrNativeReviewTextNotUTF8
	}
	if len(s) > MaxNativeReviewTextBytes {
		return ErrNativeReviewTextTooLarge
	}
	return nil
}

// validateKindConsistency enforces the cross-field contract each kind carries:
// a findings_review names the head it bound to and the state it was submitted
// in, and a clean_pass_signal names no commit, no state, and no findings. The
// switch dispatches on the kind and omits default so a new kind must be handled
// here; the caller has already rejected the invalid zero value.
func (o NativeReviewObservation) validateKindConsistency() error {
	switch o.Kind {
	case NativeReviewFindings:
		if o.ReviewCommitSHA == "" {
			return fmt.Errorf("native review observation %s review_commit_sha: %w",
				o.Kind, ErrEmptyField)
		}
		if o.ReviewState == "" {
			return fmt.Errorf("native review observation %s review_state: %w",
				o.Kind, ErrEmptyField)
		}
	case NativeReviewCleanPass:
		if o.ReviewCommitSHA != "" {
			return fmt.Errorf("native review observation %s carries review_commit_sha %q: %w",
				o.Kind, o.ReviewCommitSHA, ErrNativeReviewInconsistent)
		}
		if o.ReviewState != "" {
			return fmt.Errorf("native review observation %s carries review_state %q: %w",
				o.Kind, o.ReviewState, ErrNativeReviewInconsistent)
		}
		if len(o.Findings) > 0 {
			return fmt.Errorf("native review observation %s carries %d findings: %w",
				o.Kind, len(o.Findings), ErrNativeReviewInconsistent)
		}
	}
	return nil
}

// MaterialChangeFrom reports whether this observation differs from the previous
// recorded observation for the same identity in anything but its instant: the
// append-on-material-change rule's single definition, so the store method stays
// mechanical. Findings are compared element-wise; the timestamp of observation
// is excluded so a re-poll of unchanged native state coalesces.
func (o NativeReviewObservation) MaterialChangeFrom(prev NativeReviewObservation) bool {
	if o.Repo != prev.Repo || o.RepositoryID != prev.RepositoryID ||
		o.PRNumber != prev.PRNumber || o.Provider != prev.Provider ||
		o.Kind != prev.Kind || o.NativeID != prev.NativeID ||
		o.AuthorLogin != prev.AuthorLogin || o.ReviewCommitSHA != prev.ReviewCommitSHA ||
		o.ReviewState != prev.ReviewState ||
		o.BindingHeadSHA != prev.BindingHeadSHA || !o.SubmittedAt.Equal(prev.SubmittedAt) {
		return true
	}
	return !slices.Equal(o.Findings, prev.Findings)
}
