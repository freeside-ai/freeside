package daemoncompose

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestUsageCollectorsForProduction pins the daemon's collector choice: with
// none injected it runs the Claude collector under the Claude harness and
// hands it back for the engine's sink; injected collectors run alone.
func TestUsageCollectorsForProduction(t *testing.T) {
	t.Parallel()
	collectors, claude := usageCollectorsFor(nil)
	if claude == nil || len(collectors) != 1 || collectors[domain.HarnessClientClaudeCode] != ward.UsageCollector(claude) {
		t.Fatalf("usageCollectorsFor(nil) = %v, %v; want the Claude collector alone", collectors, claude)
	}
	injected := map[domain.HarnessClientKind]ward.UsageCollector{domain.HarnessClientCodexCLI: &blockingUsageCollector{}}
	collectors, claude = usageCollectorsFor(injected)
	if claude != nil || !reflect.DeepEqual(collectors, injected) {
		t.Fatalf("usageCollectorsFor(injected) = %v, %v; want the injected collectors and no Claude collector", collectors, claude)
	}
}

// TestClaudeUsageSinkCollectsAKeptCapture covers the production sink over the
// real collector, coordinator, and store: a capture under the pool's
// collector lands in its record, and a capture the collector drops (here one
// with no get_usage answer) starts nothing and leaves the record.
func TestClaudeUsageSinkCollectsAKeptCapture(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	const pool = domain.UsagePoolID("pool-claude")
	now := time.Now().UTC()
	seedUsageEnrollment(t, st, "auth-claude", "enroll-claude", pool, domain.HarnessClientClaudeCode, now.Add(-time.Hour))
	collectors, claude := usageCollectorsFor(nil)
	usage := usagecollect.New(st, collectors, usagecollect.Config{})
	ctx, cancel := context.WithCancel(t.Context())
	usage.Start(ctx)
	t.Cleanup(func() {
		cancel()
		usage.Wait()
	})
	sink := claudeUsageSink{collector: claude, usage: usage}

	weeklyReset := now.Add(48 * time.Hour).Truncate(time.Second)
	capture := exec.ClaudeUsageCapture{
		ObservedAt: now.Add(-time.Minute).Truncate(time.Second),
		Windows:    []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: 57, ResetsAt: &weeklyReset}},
		Events:     []exec.ClaudeRateLimitEvent{},
	}
	if err := sink.ClaudeUsageCaptured(ctx, pool, "enroll-claude", 1, capture); err != nil {
		t.Fatalf("ClaudeUsageCaptured() error = %v", err)
	}
	report, err := usage.Refresh(ctx, pool)
	if err != nil || report.Outcome != usagecollect.OutcomeCollected {
		t.Fatalf("refresh = %+v, %v; want collected", report, err)
	}
	record := *report.Observation
	if len(record.Windows) != 1 || record.Windows[0].Window != "seven_day" ||
		record.Windows[0].Length == nil || *record.Windows[0].Length != 7*24*time.Hour {
		t.Fatalf("windows = %+v, want the weekly reading with its length", record.Windows)
	}

	eventOnly := exec.ClaudeUsageCapture{
		ObservedAt: capture.ObservedAt.Add(30 * time.Second),
		Windows:    []exec.ClaudeUsageWindow{},
		Events:     []exec.ClaudeRateLimitEvent{{Window: "five_hour", Status: "allowed"}},
	}
	if err := sink.ClaudeUsageCaptured(ctx, pool, "enroll-claude", 1, eventOnly); err != nil {
		t.Fatalf("ClaudeUsageCaptured(event only) error = %v", err)
	}
	if after := readPoolRecord(t, st, pool); !reflect.DeepEqual(after, record) {
		t.Fatalf("record after an event-only capture = %+v, want %+v", after, record)
	}
}

func readPoolRecord(t *testing.T, st *store.Store, pool domain.UsagePoolID) domain.PoolUsageObservation {
	t.Helper()
	var record domain.PoolUsageObservation
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetPoolUsageObservation(t.Context(), pool)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record
}
