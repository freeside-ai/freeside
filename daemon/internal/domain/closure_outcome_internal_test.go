package domain

import "testing"

// TestClosureOutcomeEnumValidity exercises the validity predicate of each
// outcome enum: every registered member is valid and the zero value is not.
// ClosureOutcomeFor produces these values rather than decoding them, so this
// internal test is their valid() caller, keeping the predicates honest.
func TestClosureOutcomeEnumValidity(t *testing.T) {
	for _, r := range AllClosureReferences {
		if !r.valid() {
			t.Errorf("reference %q not valid", r)
		}
	}
	if (ClosureReference("")).valid() {
		t.Error("zero reference is valid")
	}
	for _, h := range AllClosureHolds {
		if !h.valid() {
			t.Errorf("hold %q not valid", h)
		}
	}
	if (ClosureHold("")).valid() {
		t.Error("zero hold is valid")
	}
	for _, i := range AllClosureItems {
		if !i.valid() {
			t.Errorf("item %q not valid", i)
		}
	}
	if (ClosureItem("")).valid() {
		t.Error("zero item is valid")
	}
}
