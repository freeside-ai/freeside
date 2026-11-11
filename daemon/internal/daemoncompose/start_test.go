package daemoncompose

import (
	"context"
	"errors"
	"testing"
)

type recordingSessionCloser struct {
	calls int
	err   error
}

func (c *recordingSessionCloser) Close(context.Context) error {
	c.calls++
	return c.err
}

func TestStartupSessionCleanupTransfersOnlyAfterSuccess(t *testing.T) {
	t.Parallel()
	closeErr := errors.New("session close failed")
	failed := &recordingSessionCloser{err: closeErr}
	stopped := 0
	stop := func() { stopped++ }
	if err := closeStartupSessions(false, failed, stop); !errors.Is(err, closeErr) {
		t.Fatalf("failed-start cleanup = %v, want %v", err, closeErr)
	}
	if failed.calls != 1 {
		t.Fatalf("failed-start close calls = %d, want 1", failed.calls)
	}
	// A failed startup tears down a credential-bearing session while
	// signal.NotifyContext is still registered; restoring signal disposition
	// first is what keeps a second SIGTERM able to end a wedged lease cleanup.
	if stopped != 1 {
		t.Fatalf("failed-start signal restore calls = %d, want 1", stopped)
	}

	succeeded := &recordingSessionCloser{}
	if err := closeStartupSessions(true, succeeded, stop); err != nil {
		t.Fatalf("successful-start cleanup = %v", err)
	}
	if succeeded.calls != 0 {
		t.Fatalf("successful-start close calls = %d, want daemon ownership", succeeded.calls)
	}
	// A successful startup leaves signal handling in place: serve owns the
	// stop-before-close sequence for the running daemon's teardown.
	if stopped != 1 {
		t.Fatalf("successful-start signal restore calls = %d, want no additional restore", stopped)
	}

	// With no credential-bearing closer (the fake-driver lane), there is
	// nothing that can wedge, so signal disposition is left untouched.
	if err := closeStartupSessions(false, nil, stop); err != nil {
		t.Fatalf("failed-start with no closer = %v", err)
	}
	if stopped != 1 {
		t.Fatalf("no-closer signal restore calls = %d, want no restore", stopped)
	}
}
