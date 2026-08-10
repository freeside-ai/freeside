package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
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
	go func() { done <- reconciler.Run(ctx, time.Millisecond, time.Millisecond, logger) }()
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

func TestActiveResourceRunUsesOperatorActiveCadence(t *testing.T) {
	st := schedTestStore(t)
	item := capturedRunWithCriterion(
		t, st, "run-active-cadence", "item-active-cadence",
		domain.CompletionBoundPRMerged, nil,
	)
	var merged atomic.Bool
	observed := make(chan struct{}, 32)
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			select {
			case observed <- struct{}{}:
			default:
			}
			if merged.Load() {
				return exactPull("closed", true), nil
			}
			return exactPull("open", false), nil
		},
		now: func() time.Time { return time.Now().UTC() },
	}

	var out bytes.Buffer
	logger, err := newLogger(&out, "debug")
	if err != nil {
		t.Fatal(err)
	}
	const (
		defaultInterval  = 300 * time.Millisecond
		operatorInterval = 10 * time.Millisecond
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- reconciler.Run(ctx, defaultInterval, operatorInterval, logger)
	}()
	defer func() {
		cancel()
		if done == nil {
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("Run did not stop after cancellation")
		}
	}()

	// The startup pass engages the tight cadence. Three observations arrive
	// well inside the background interval, while unchanged state coalesces to
	// one durable fact and leaves the item version untouched.
	for pass := 0; pass < 3; pass++ {
		select {
		case <-observed:
		case <-time.After(defaultInterval / 2):
			t.Fatalf("operator-active pass %d did not arrive before the idle cadence", pass+1)
		}
	}
	if got := readActiveItem(t, st, item.ID); got.ItemVersion != item.ItemVersion {
		t.Fatalf("unchanged active passes changed item version to %d, want %d",
			got.ItemVersion, item.ItemVersion)
	}
	if facts := activePullFacts(t, st, 424242, 450); len(facts) != 1 {
		t.Fatalf("unchanged active passes recorded %d pull facts, want 1: %+v", len(facts), facts)
	}
	assertNoActiveCompletion(t, st, item)

	mergedAt := time.Now()
	merged.Store(true)
	deadline := mergedAt.Add(defaultInterval / 2)
	for {
		if got := readActiveItem(t, st, item.ID); got.Status == domain.StatusResolved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("merged ready item was not concluded at operator speed")
		}
		time.Sleep(time.Millisecond)
	}
	if elapsed := time.Since(mergedAt); elapsed >= defaultInterval/2 {
		t.Fatalf("completion took %s, want well inside the %s idle interval", elapsed, defaultInterval)
	}

	// Once the concluding pass releases the regime, several former tight-cadence
	// windows add neither another pass's facts nor another completion.
	time.Sleep(3 * operatorInterval)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	done = nil

	if facts := activePullFacts(t, st, 424242, 450); len(facts) != 2 || !facts[1].Merged {
		t.Fatalf("completion pull facts = %+v, want one open and one merged", facts)
	}
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		completion, err := tx.GetWorkUnitCompletion(
			t.Context(), domain.WorkUnitIDForRun(*item.Subject.RunID),
		)
		if err != nil {
			return err
		}
		if completion.MergeCommitSHA != "deadbeef" {
			t.Fatalf("completion = %+v", completion)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	records := logRecords(t, out.String())
	if engaged := recordsWhere(records, "msg", "operator-active cadence engaged"); len(engaged) != 1 {
		t.Fatalf("engaged records = %v, want exactly one", engaged)
	}
	if released := recordsWhere(records, "msg", "operator-active cadence released"); len(released) != 1 {
		t.Fatalf("released records = %v, want exactly one", released)
	}
	passes := recordsWhere(records, "msg", "active resource pass complete")
	if len(passes) < 4 {
		t.Fatalf("pass records = %v, want startup, unchanged, and completion passes", passes)
	}
	for _, record := range passes {
		if record["level"] != "DEBUG" {
			t.Fatalf("per-pass record escaped debug level: %v", record)
		}
	}
}

func TestActiveResourceRunWakesForNewReadyItem(t *testing.T) {
	st := schedTestStore(t)
	observed := make(chan struct{}, 1)
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			select {
			case observed <- struct{}{}:
			default:
			}
			return exactPull("open", false), nil
		},
		now: func() time.Time { return time.Now().UTC() },
	}

	const (
		defaultInterval  = 300 * time.Millisecond
		operatorInterval = 10 * time.Millisecond
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- reconciler.Run(ctx, defaultInterval, operatorInterval, slog.New(slog.DiscardHandler))
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("Run did not stop after cancellation")
		}
	}()

	// Let the startup pass enter its idle wait before committing the item. The
	// observation must arrive well before that idle timer would fire.
	time.Sleep(20 * time.Millisecond)
	capturedRunWithCriterion(t, st, "run-wake-ready", "item-wake-ready", domain.CompletionBoundPRMerged, nil)
	select {
	case <-observed:
	case <-time.After(defaultInterval / 2):
		t.Fatal("new ready item waited for the idle cadence")
	}
}
