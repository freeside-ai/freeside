package inference

import "testing"

func TestFallbackAndAdjudicationCauseRegistration(t *testing.T) {
	seen := map[FallbackReason]bool{}
	for _, reason := range AllFallbackReasons {
		if !reason.valid() || seen[reason] {
			t.Fatalf("invalid or duplicate reason %q", reason)
		}
		seen[reason] = true
	}
	if FallbackReason("").valid() || FallbackReason("provider text").valid() {
		t.Fatal("open fallback reason")
	}
	for _, cause := range AllAdjudicationRefusals {
		if !cause.valid() {
			t.Fatalf("invalid refusal %q", cause)
		}
	}
	if AdjudicationRefusal("").valid() || AdjudicationRefusal("provider text").valid() {
		t.Fatal("open refusal cause")
	}
}
