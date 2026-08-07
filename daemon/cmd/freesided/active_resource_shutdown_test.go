package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// idleActiveResourceReconciler is configured enough to complete a pass and
// has nothing to reconcile, so every pass is an idle one. The observers
// fail loudly rather than returning empty results: an empty store must
// reach no observer at all, and a silent stub would hide it if one day it
// did.
func idleActiveResourceReconciler(t *testing.T) activeResourceReconciler {
	t.Helper()
	return activeResourceReconciler{
		store: schedTestStore(t),
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			return publish.PullObservation{}, errors.New("no ready item should reach an observer")
		},
		now: func() time.Time { return time.Now().UTC() },
	}
}

// runIdlePasses drives the real loop for many ticks at level, then cancels
// and returns what it logged. It fails if the loop reports an error: a
// canceled daemon is stopping, not failing.
func runIdlePasses(t *testing.T, level string) []map[string]string {
	t.Helper()
	var out bytes.Buffer
	logger, err := newLogger(&out, level)
	if err != nil {
		t.Fatalf("newLogger(%q): %v", level, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	reconciler := idleActiveResourceReconciler(t)
	go func() { done <- reconciler.Run(ctx, time.Millisecond, logger) }()
	// Long enough for many ticks at a one-millisecond interval.
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run reported %v for a canceled loop; a clean stop must not "+
				"reach the daemon's fatal channel", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	return logRecords(t, out.String())
}

// TestCancellationStopsTheReconcilerCleanly pins the stop semantics the
// engine and scheduler loops already had. A pass interrupted mid-flight
// returns its store's "context canceled", and returning that made the
// daemon race a spurious failure into the channel it treats as fatal, so a
// plain SIGTERM could exit non-zero and read to a supervisor as a crash.
func TestCancellationStopsTheReconcilerCleanly(t *testing.T) {
	// runIdlePasses fails the test if Run reports an error; a store read
	// interrupted mid-pass is exactly what it cancels into.
	records := runIdlePasses(t, defaultLogLevel)
	if len(recordsWhere(records, "level", "ERROR")) != 0 {
		t.Fatalf("a clean stop logged at error severity: %v", records)
	}
}

// TestDefaultLevelEmitsNoPerPassRecords guards the flood: these loops tick
// forever, so a record per idle pass is a log an operator stops reading,
// which costs them the records that matter. It runs the real loop at both
// levels, so what it asserts is this loop's behaviour rather than slog's
// filtering.
func TestDefaultLevelEmitsNoPerPassRecords(t *testing.T) {
	quiet := runIdlePasses(t, defaultLogLevel)
	for _, record := range quiet {
		if strings.Contains(record["msg"], "pass complete") {
			t.Fatalf("the default level emitted a per-pass record: %v", record)
		}
	}
	// Start and stop are per-lifetime, not per-pass, so a handful is the
	// whole budget however long the loop ran.
	if len(quiet) > 4 {
		t.Fatalf("got %d records at the default level over many passes, want only lifetime records: %v",
			len(quiet), quiet)
	}

	verbose := runIdlePasses(t, "debug")
	if len(recordsWhere(verbose, "level", "DEBUG")) == 0 {
		t.Fatalf("debug suppressed the per-pass records it exists to enable: %v", verbose)
	}
}
