package exec_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// TestInspectionValidate: every status is representable, and liveness is
// only representable beside a status that could still be executing — a
// terminal or gone inspection claiming liveness is malformed by
// construction (issue #394).
func TestInspectionValidate(t *testing.T) {
	for _, status := range exec.AllStatuses {
		if err := (exec.Inspection{Status: status}).Validate(); err != nil {
			t.Errorf("status %s without liveness: %v", status, err)
		}
		live := exec.Inspection{Status: status, Live: true}
		err := live.Validate()
		if status.Terminal() || status == exec.StatusGone {
			if !errors.Is(err, exec.ErrInvalidStatus) {
				t.Errorf("live %s: Validate() = %v, want %v", status, err, exec.ErrInvalidStatus)
			}
			continue
		}
		if err != nil {
			t.Errorf("live %s: Validate() = %v, want nil", status, err)
		}
	}
	if err := (exec.Inspection{}).Validate(); !errors.Is(err, exec.ErrInvalidStatus) {
		t.Errorf("zero inspection: Validate() = %v, want %v", exec.Inspection{}, exec.ErrInvalidStatus)
	}
}
