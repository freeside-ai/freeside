package ward

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckValid(t *testing.T) {
	for _, c := range AllChecks {
		if !c.valid() {
			t.Errorf("Check %q: valid() = false, want true", c)
		}
	}
	for _, c := range []Check{"", "credential-separation", "check_6"} {
		if c.valid() {
			t.Errorf("Check %q: valid() = true, want false", c)
		}
	}
}

func TestAllChecksDistinct(t *testing.T) {
	seen := make(map[Check]bool, len(AllChecks))
	for _, c := range AllChecks {
		if seen[c] {
			t.Errorf("check identifier %q listed twice", c)
		}
		seen[c] = true
	}
}

func TestConformanceFailureClassAndDetails(t *testing.T) {
	var err error = &ConformanceFailure{
		Backend: "some_backend",
		Check:   CheckWriterTermination,
		Reason:  "state running after stop deadline",
	}

	if !errors.Is(err, ErrConformance) {
		t.Fatal("errors.Is(err, ErrConformance) = false, want true")
	}
	var cf *ConformanceFailure
	if !errors.As(err, &cf) {
		t.Fatal("errors.As(err, *ConformanceFailure) = false, want true")
	}
	if cf.Check != CheckWriterTermination {
		t.Errorf("Check = %q, want %q", cf.Check, CheckWriterTermination)
	}
	for _, want := range []string{"some_backend", string(CheckWriterTermination), "stop deadline"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, missing %q", err.Error(), want)
		}
	}
}
