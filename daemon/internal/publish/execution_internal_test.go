package publish

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestValidateExecutionReservationStateRejectsInvalidState(t *testing.T) {
	t.Parallel()
	claim, err := NewReservation("inv-0001", "run-0001")
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}

	err = validateExecutionReservationState(
		"", store.QueueEntry{}, claim, "inv-producing",
	)
	if err == nil {
		t.Fatal("invalid invocation state passed the execution reservation gate")
	}
	if want := unhandledInvocationState("").Error(); err.Error() != want {
		t.Fatalf("invalid invocation state error = %q, want %q", err, want)
	}
}
