package engine

import "testing"

// TestDispatchHoldKeepsTheFirstLaunchUntilItCloses: while the hold is open a
// pass starts nothing and records no attempt; the first pass after it closes
// starts the pending intent (#3447).
func TestDispatchHoldKeepsTheFirstLaunchUntilItCloses(t *testing.T) {
	t.Parallel()
	h := newDrainDispatchFixture(t, nil)
	hold := make(chan struct{})
	if err := WithDispatchHold(hold)(h.engine); err != nil {
		t.Fatal(err)
	}
	h.fixture.submit(t)
	for pass := range 2 {
		if started := h.dispatch(t); started != 0 {
			t.Fatalf("pass %d started %d while the hold was open", pass, started)
		}
	}
	if h.attemptRecorded(t) {
		t.Fatal("a held dispatch recorded an attempt")
	}
	close(hold)
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d on the first pass after the hold closed, want 1", started)
	}
}
