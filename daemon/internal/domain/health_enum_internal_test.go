package domain

import "testing"

// TestHealthAndRejectionEnumValidity exercises the validity predicate of the
// feature and command-refusal enums: every registered member is valid and the
// zero value is not. The HTTP boundary writes these values rather than
// decoding them, so this internal test is their valid() caller.
func TestHealthAndRejectionEnumValidity(t *testing.T) {
	for _, f := range AllDaemonFeatures {
		if !f.valid() {
			t.Errorf("feature %q not valid", f)
		}
	}
	if DaemonFeature("").valid() {
		t.Error("zero feature is valid")
	}
	for _, r := range AllCommandRequestRejectionReasons {
		if !r.valid() {
			t.Errorf("reason %q not valid", r)
		}
	}
	if CommandRequestRejectionReason("").valid() {
		t.Error("zero reason is valid")
	}
}
