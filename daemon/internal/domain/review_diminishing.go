package domain

import "fmt"

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
