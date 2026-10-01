package store

import (
	"slices"
	"testing"
)

func TestReviewDiminishingCauseEnumRegistration(t *testing.T) {
	causes := []ReviewDiminishingCause{
		ReviewDiminishingLowValue,
		ReviewDiminishingFixedRecurrence,
		ReviewDiminishingFinalFindings,
		ReviewDiminishingGrowthWithoutBlockers,
	}
	if !slices.Equal(AllReviewDiminishingCauses, causes) {
		t.Fatalf("causes = %v, want %v", AllReviewDiminishingCauses, causes)
	}
	for _, cause := range AllReviewDiminishingCauses {
		if !cause.valid() {
			t.Errorf("registered cause %q is invalid", cause)
		}
	}
	if ReviewDiminishingCause("").valid() {
		t.Error("empty cause is valid")
	}
}
