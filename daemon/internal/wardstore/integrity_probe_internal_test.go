package wardstore

import "testing"

// TestIntegritySkipReasonRegistration: every registered reason is valid and
// the zero value is not, so AllIntegritySkipReasons stays the one
// registration point.
func TestIntegritySkipReasonRegistration(t *testing.T) {
	for _, reason := range AllIntegritySkipReasons {
		if !reason.valid() {
			t.Errorf("registered skip reason %q is not valid", reason)
		}
	}
	if IntegritySkipReason("").valid() || IntegritySkipReason("unregistered").valid() {
		t.Error("an unregistered skip reason is valid")
	}
}
