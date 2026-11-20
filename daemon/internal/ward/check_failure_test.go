package ward

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestEveryCheckHasADomainCheck: the domain mirror of Check lists every
// ward check, so no refusal records an outcome Validate refuses.
func TestEveryCheckHasADomainCheck(t *testing.T) {
	t.Parallel()
	for _, check := range AllChecks {
		record := domain.HandoffCheckFailure{Check: domain.WardCheck(check), Evidence: "x"}
		if err := record.Validate(); err != nil {
			t.Errorf("ward check %q has no domain check: %v", check, err)
		}
	}
	if len(AllChecks) != len(domain.AllWardChecks) {
		t.Errorf("ward has %d checks, domain mirrors %d", len(AllChecks), len(domain.AllWardChecks))
	}
}

// TestHandoffCheckFailureOfWithholdsTheError: a failw caller formats Err
// into Reason, and the record still never carries Err's text.
func TestHandoffCheckFailureOfWithholdsTheError(t *testing.T) {
	t.Parallel()
	cause := errors.New("stat /var/folders/private/handoff: no such file")
	failure := &ConformanceFailure{
		Backend: "apple", Check: CheckExportVerification,
		Reason: fmt.Sprintf("export verification failed: %v", cause), Err: cause,
		ContentEvidence: true,
	}
	record := HandoffCheckFailureOf(fmt.Errorf("handoff: %w", failure))
	if record == nil || record.Check != domain.WardCheckExportVerification || !record.ContentEvidence {
		t.Fatalf("record = %+v, want the export verification check with content evidence", record)
	}
	if strings.Contains(record.Evidence, "/var/folders") {
		t.Fatalf("evidence %q carries the error text", record.Evidence)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("record does not validate: %v", err)
	}
	if HandoffCheckFailureOf(errors.New("plain")) != nil {
		t.Fatal("an error with no ConformanceFailure produced a record")
	}
}
