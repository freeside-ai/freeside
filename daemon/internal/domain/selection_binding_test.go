package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// decodeFixture decodes a committed fixture strictly and checks that it
// encodes back to the same bytes, so a field rename or a dropped field shows
// up as a failure rather than as a silently different record.
func decodeFixture[T any](t *testing.T, name string) T {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixed test fixture names
	if err != nil {
		t.Fatal(err)
	}
	var value T
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	again, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(again), bytes.TrimSpace(body)) {
		t.Fatalf("%s: re-encodes to other bytes:\n%s", name, again)
	}
	return value
}

// TestAdmissionsRecordedBeforeSelectionsStillRead holds the bytes each kind
// of admission had before #2734 admitted a selection (the *_pre_selection
// fixtures are those goldens as they stood): each still decodes, validates,
// and, for an execution admission, keeps its id, and none reads as admitted
// under a selection.
func TestAdmissionsRecordedBeforeSelectionsStillRead(t *testing.T) {
	t.Parallel()
	execution := decodeFixture[domain.ExecutionAdmission](t, "execution_admission_task_line_pre_selection.golden")
	if err := execution.Validate(); err != nil {
		t.Fatalf("execution admission: %v", err)
	}
	if id, err := execution.ComputeID(); err != nil || id != execution.ID {
		t.Fatalf("execution admission id = %s, %v, want the recorded %s", id, err, execution.ID)
	}
	if execution.AgentBinding.SelectionAdmitted() {
		t.Fatal("an agent-admitted execution binding reads as selection-admitted")
	}
	review := decodeFixture[domain.ReviewAdmission](t, "review_admission_pre_selection.golden")
	if err := review.Validate(); err != nil {
		t.Fatalf("review admission: %v", err)
	}
	if review.AgentBinding.SelectionAdmitted() {
		t.Fatal("an agent-admitted review binding reads as selection-admitted")
	}
	wardless := decodeFixture[domain.WardlessAdmission](t, "wardless_admission_pre_selection.golden")
	if err := wardless.Validate(); err != nil {
		t.Fatalf("wardless admission: %v", err)
	}
}

// TestSelectionBindingRefusesAnAgentsFields refuses a binding admitted under
// a selection that carries a field only an agent's offer supplies: a pricing
// revision, or a Freeside effort level beside the native effort.
func TestSelectionBindingRefusesAnAgentsFields(t *testing.T) {
	t.Parallel()
	admitted := decodeFixture[domain.ExecutionAdmission](t, "execution_admission_task_line.golden")
	if !admitted.AgentBinding.SelectionAdmitted() {
		t.Fatal("the task line golden is not selection-admitted")
	}
	if err := admitted.Validate(); err != nil {
		t.Fatalf("the golden itself: %v", err)
	}
	for name, edit := range map[string]func(*domain.AdmissionAgentBinding){
		"a pricing revision": func(b *domain.AdmissionAgentBinding) { b.PricingRevision = "2026-10-01" },
		"a requested effort": func(b *domain.AdmissionAgentBinding) { b.RequestedEffort = domain.EffortHigh },
	} {
		binding := *admitted.AgentBinding
		edit(&binding)
		candidate := admitted
		candidate.AgentBinding = &binding
		if err := candidate.Validate(); !errors.Is(err, domain.ErrSelectionBindingInconsistent) {
			t.Errorf("%s: Validate() = %v, want %v", name, err, domain.ErrSelectionBindingInconsistent)
		}
	}
}
