package signet

import "testing"

func TestDeviceTestNotificationOutcomeValidity(t *testing.T) {
	t.Parallel()
	for _, outcome := range AllDeviceTestNotificationOutcomes {
		if !outcome.valid() {
			t.Errorf("outcome %q: valid() = false, want true", outcome)
		}
	}
	if DeviceTestNotificationOutcome("").valid() {
		t.Error("zero outcome: valid() = true, want false")
	}
}
