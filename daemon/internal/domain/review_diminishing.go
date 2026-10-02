package domain

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ParseReviewDiminishingCause resolves a stored cause value. The enum
// predicate is unexported, so the store's Reason binding validates through
// this.
func ParseReviewDiminishingCause(value string) (ReviewDiminishingCause, error) {
	cause := ReviewDiminishingCause(value)
	if !cause.valid() {
		return "", fmt.Errorf("review diminishing cause %q: %w", value, ErrInvalidReviewDiminishingCause)
	}
	return cause, nil
}

// ReviewDiminishingFacts is the typed card payload of a
// review_diminishing_returns item (plan §7 Review Drift): why the review loop
// stopped and, when a drift audit stopped it, what the audit said. The same
// cause is bound in the item's Reason; the store re-checks that the two agree.
//
// DriftAudit is present exactly when Cause is drift_audit and renders an
// explicit null otherwise.
type ReviewDiminishingFacts struct {
	Cause      ReviewDiminishingCause `json:"cause"`
	DriftAudit *DriftAuditFacts       `json:"drift_audit"`
}

// DriftAuditFacts projects the stored DriftAudit that parked the run onto the
// card. AuditDigest names that artifact; the store re-checks every other
// field against it, so a client never has to trust the copy. All three
// verdicts are representable: which of them can park is the route gate's
// decision, not this shape's.
//
// SimplificationOnContinue says whether continue_under_policy will run the
// simplification round the audit proposed. It is true only when the reversal
// list passed the route gate, which only an over_hardened verdict can.
// Reversals is never nil, so an empty list has one encoding ([]).
type DriftAuditFacts struct {
	AuditDigest              Digest                 `json:"audit_digest"`
	Verdict                  DriftVerdict           `json:"verdict"`
	Confidence               AdjudicationConfidence `json:"confidence"`
	Explanation              string                 `json:"explanation"`
	Reversals                []DriftReversal        `json:"reversals"`
	SimplificationOnContinue bool                   `json:"simplification_on_continue"`
}

// Validate checks the facts' own consistency. Whether the cause matches the
// item's Reason and the drift facts match the stored audit is the store's
// re-check.
func (f ReviewDiminishingFacts) Validate() error {
	if !f.Cause.valid() {
		return fmt.Errorf("review diminishing cause %q: %w", f.Cause, ErrInvalidReviewDiminishingCause)
	}
	if (f.Cause == ReviewDiminishingDriftAudit) != (f.DriftAudit != nil) {
		return fmt.Errorf("review diminishing cause %q drift facts present=%t: %w",
			f.Cause, f.DriftAudit != nil, ErrCardFactInconsistent)
	}
	if f.DriftAudit != nil {
		return f.DriftAudit.Validate()
	}
	return nil
}

// Validate applies the stored artifact's own rules to its projection: the
// reversal list's cardinality is part of the verdict, and the list is strictly
// ascending by finding id.
func (f DriftAuditFacts) Validate() error {
	if !contentaddr.Valid(string(f.AuditDigest)) {
		return fmt.Errorf("drift facts audit_digest %q: %w", f.AuditDigest, ErrCardFactInconsistent)
	}
	if !f.Verdict.valid() {
		return fmt.Errorf("drift facts verdict %q: %w", f.Verdict, ErrInvalidDriftVerdict)
	}
	if !f.Confidence.valid() {
		return fmt.Errorf("drift facts confidence %q: %w", f.Confidence, ErrInvalidAdjudicationConfidence)
	}
	if strings.TrimSpace(f.Explanation) == "" {
		return fmt.Errorf("drift facts explanation: %w", ErrEmptyField)
	}
	if !utf8.ValidString(f.Explanation) {
		return fmt.Errorf("drift facts explanation: %w", ErrCardFactInconsistent)
	}
	if f.Reversals == nil {
		return fmt.Errorf("drift facts reversals absent: %w", ErrCardFactInconsistent)
	}
	// The switch dispatches on the verdict, so it omits default; Verdict is
	// already known valid.
	switch f.Verdict {
	case DriftVerdictOverHardened:
		if len(f.Reversals) == 0 {
			return fmt.Errorf("drift facts verdict %q without reversals: %w", f.Verdict, ErrCardFactInconsistent)
		}
	case DriftVerdictConverged, DriftVerdictStuck:
		if len(f.Reversals) != 0 {
			return fmt.Errorf("drift facts verdict %q with %d reversals: %w",
				f.Verdict, len(f.Reversals), ErrCardFactInconsistent)
		}
		if f.SimplificationOnContinue {
			return fmt.Errorf("drift facts verdict %q promises a simplification round: %w",
				f.Verdict, ErrCardFactInconsistent)
		}
	}
	for i, reversal := range f.Reversals {
		if err := reversal.Validate(); err != nil {
			return err
		}
		if i > 0 && reversal.FindingID <= f.Reversals[i-1].FindingID {
			return fmt.Errorf("drift facts reversal %q order: %w", reversal.FindingID, ErrFindingsNotCanonical)
		}
	}
	return nil
}

func cloneReviewDiminishingFacts(in *ReviewDiminishingFacts) *ReviewDiminishingFacts {
	if in == nil {
		return nil
	}
	out := *in
	if in.DriftAudit != nil {
		audit := *in.DriftAudit
		audit.Reversals = slices.Clone(in.DriftAudit.Reversals)
		out.DriftAudit = &audit
	}
	return &out
}
