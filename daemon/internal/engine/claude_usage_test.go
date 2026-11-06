package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// claudeUsageObservedAt is the fixture capture's export stamp: before the
// pinned loopback fixture's resets, so its weekly reset is ahead.
var claudeUsageObservedAt = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

func loopbackClaudeUsage(t *testing.T) *exec.ClaudeUsageCapture {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "exec", "claude", "testdata", "claude_usage_loopback_turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	capture := claude.ExtractUsageCapture(body, claudeUsageObservedAt)
	if capture == nil {
		t.Fatal("the loopback fixture carries no usage capture")
	}
	return capture
}

type capturedUsage struct {
	pool       domain.UsagePoolID
	enrollment domain.ClientEnrollmentID
	generation int
	capture    exec.ClaudeUsageCapture
}

// recordingClaudeUsageSink records every capture and answers err.
type recordingClaudeUsageSink struct {
	mu       sync.Mutex
	captured []capturedUsage
	err      error
}

func (s *recordingClaudeUsageSink) ClaudeUsageCaptured(
	_ context.Context, pool domain.UsagePoolID, enrollment domain.ClientEnrollmentID, generation int,
	capture exec.ClaudeUsageCapture,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = append(s.captured, capturedUsage{pool, enrollment, generation, capture})
	return s.err
}

// usageWarnings returns a logger over a buffer and a reader of the
// warnings it has logged so far.
func usageWarnings() (*slog.Logger, func() []string) {
	var logs bytes.Buffer
	return slog.New(slog.NewTextHandler(&logs, nil)), func() []string {
		var warnings []string
		for line := range strings.Lines(logs.String()) {
			if strings.Contains(line, "level=WARN") {
				warnings = append(warnings, line)
			}
		}
		return warnings
	}
}

// assertOneDropWarning requires exactly one warning, naming the invocation.
func assertOneDropWarning(t *testing.T, warnings []string, invocationID domain.InvocationID) {
	t.Helper()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "invocation="+string(invocationID)) {
		t.Fatalf("warnings = %q, want one naming invocation %s", warnings, invocationID)
	}
}

// TestTerminalReportsClaudeUsage covers the terminal-path hook: a capture
// reaches the sink under the admission's enrollment, generation, and pool;
// with no capture or no agent binding nothing does; and a sink error leaves
// the terminal as collected. Every lost capture logs a warning; a delivered
// one, or no capture at all, logs none.
func TestTerminalReportsClaudeUsage(t *testing.T) {
	t.Parallel()
	capture := loopbackClaudeUsage(t)
	for _, tc := range []struct {
		name    string
		cfg     usageFixtureConfig
		sinkErr error
		want    []capturedUsage
		warns   bool
	}{
		{
			name: "bound admission", cfg: usageFixtureConfig{bound: true, claude: true, claudeUsage: capture},
			want: []capturedUsage{{"pool-usage-1", "enrollment-usage-1", 1, *capture}},
		},
		{
			name: "sink error", cfg: usageFixtureConfig{bound: true, claude: true, claudeUsage: capture},
			sinkErr: errors.New("store unavailable"),
			want:    []capturedUsage{{"pool-usage-1", "enrollment-usage-1", 1, *capture}},
			warns:   true,
		},
		{name: "no capture", cfg: usageFixtureConfig{bound: true, claude: true}},
		{name: "unbound admission", cfg: usageFixtureConfig{claude: true, claudeUsage: capture}, warns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, run, attempt := usageEngineFixtureWith(t, tc.cfg)
			sink := &recordingClaudeUsageSink{err: tc.sinkErr}
			logger, warnings := usageWarnings()
			engine.claudeUsage, engine.logger = sink, logger
			result, ready, err := engine.collectTerminal(t.Context(), run.ID, attempt)
			if err != nil || !ready {
				t.Fatalf("collectTerminal = ready %t, err %v; want the terminal collected", ready, err)
			}
			if !reflect.DeepEqual(result.ClaudeUsage, tc.cfg.claudeUsage) {
				t.Fatalf("result ClaudeUsage = %+v, want the driver's %+v", result.ClaudeUsage, tc.cfg.claudeUsage)
			}
			if !reflect.DeepEqual(sink.captured, tc.want) {
				t.Fatalf("sink got %+v, want %+v", sink.captured, tc.want)
			}
			if tc.warns {
				assertOneDropWarning(t, warnings(), attempt.InvocationID)
			} else if got := warnings(); len(got) != 0 {
				t.Fatalf("warnings = %q, want none", got)
			}
		})
	}
}

// TestTerminalWithoutAClaudeUsageSinkDropsTheCapture covers an engine with no
// sink installed, such as every test engine and a daemon without the Claude
// collector: the capture is dropped with a warning naming the invocation,
// and the terminal is collected.
func TestTerminalWithoutAClaudeUsageSinkDropsTheCapture(t *testing.T) {
	t.Parallel()
	engine, run, attempt := usageEngineFixtureWith(t, usageFixtureConfig{
		bound: true, claude: true, claudeUsage: loopbackClaudeUsage(t),
	})
	logger, warnings := usageWarnings()
	engine.logger = logger
	if _, ready, err := engine.collectTerminal(t.Context(), run.ID, attempt); err != nil || !ready {
		t.Fatalf("collectTerminal = ready %t, err %v", ready, err)
	}
	assertOneDropWarning(t, warnings(), attempt.InvocationID)
}

// TestExportOnlyRecordReportsNoClaudeUsage pins that an export-only arm (here
// attended) reports nothing when its export is recorded, even with a capture
// on the replay: its terminal is collected later and reports it then.
func TestExportOnlyRecordReportsNoClaudeUsage(t *testing.T) {
	t.Parallel()
	engine, _, attempt := usageEngineFixtureWith(t, usageFixtureConfig{bound: true, claude: true})
	ctx := t.Context()
	var admission domain.ExecutionAdmission
	if err := engine.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, attempt.InvocationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: attempt.InvocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest", RecordedAt: admission.AdmittedAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingClaudeUsageSink{}
	logger, warnings := usageWarnings()
	if err := RecordExecutionExport(ctx, engine.store, export,
		ProductionReplay{ClaudeUsage: loopbackClaudeUsage(t)}, sink, logger); err != nil {
		t.Fatal(err)
	}
	if len(sink.captured) != 0 {
		t.Fatalf("sink got %+v at export time, want nothing", sink.captured)
	}
	if got := warnings(); len(got) != 0 {
		t.Fatalf("warnings = %q, want none", got)
	}
}

// chainClaudeUsageSink is the daemon's sink shape over the real collector and
// coordinator: hold the capture, then start a collection only when it was
// kept.
type chainClaudeUsageSink struct {
	collector *ward.ClaudeUsageCollector
	coord     *usagecollect.Coordinator
}

func (s chainClaudeUsageSink) ClaudeUsageCaptured(
	ctx context.Context, pool domain.UsagePoolID, enrollment domain.ClientEnrollmentID, generation int,
	capture exec.ClaudeUsageCapture,
) error {
	if !s.collector.Record(enrollment, generation, capture) {
		return nil
	}
	return s.coord.CaptureArrived(ctx, pool, enrollment, generation)
}

// TestClaudeUsageReachesWeeklyRouting drives a capture parsed from the pinned
// 2.1.282 fixture through the terminal hook, the Claude collector, the
// coordinator, and the store, and checks that the router's weeklyReading
// then finds the pool's weekly reset.
func TestClaudeUsageReachesWeeklyRouting(t *testing.T) {
	t.Parallel()
	capture := loopbackClaudeUsage(t)
	engine, run, attempt := usageEngineFixtureWith(t, usageFixtureConfig{bound: true, claude: true, claudeUsage: capture})
	now := claudeUsageObservedAt.Add(time.Minute)
	collector := ward.NewClaudeUsageCollector()
	coord := usagecollect.New(engine.store, map[domain.HarnessClientKind]ward.UsageCollector{
		domain.HarnessClientClaudeCode: collector,
	}, usagecollect.Config{Now: func() time.Time { return now }})
	ctx, cancel := context.WithCancel(t.Context())
	coord.Start(ctx)
	t.Cleanup(func() {
		cancel()
		coord.Wait()
	})
	engine.claudeUsage, engine.logger = chainClaudeUsageSink{collector, coord}, slog.New(slog.DiscardHandler)

	const pool = domain.UsagePoolID("pool-usage-1")
	// Before any writer turn the collector's memory is empty: the attempt
	// fails and nothing reaches the record's windows.
	if report, err := coord.Refresh(ctx, pool); err != nil || report.Outcome != usagecollect.OutcomeFailed {
		t.Fatalf("refresh before the turn = %+v, %v; want failed", report, err)
	}
	now = now.Add(time.Minute)

	if _, ready, err := engine.collectTerminal(ctx, run.ID, attempt); err != nil || !ready {
		t.Fatalf("collectTerminal = ready %t, err %v", ready, err)
	}
	// Refresh joins the capture's collection, which the empty-memory backoff
	// did not hold, or reports the record it left.
	report, err := coord.Refresh(ctx, pool)
	if err != nil || report.Outcome != usagecollect.OutcomeCollected {
		t.Fatalf("refresh after the turn = %+v, %v; want collected", report, err)
	}

	var record domain.PoolUsageObservation
	if err := engine.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetPoolUsageObservation(ctx, pool)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(record.Windows) != 2 {
		t.Fatalf("windows = %+v, want five_hour and seven_day", record.Windows)
	}
	weekly, found := weeklyReading(&record, "", now)
	weeklyReset := time.Unix(1793892506, 0).UTC()
	if !found || weekly.Window != "seven_day" || !weekly.ResetsAt.Equal(weeklyReset) {
		t.Fatalf("weeklyReading = %+v (found %t), want seven_day resetting at %s", weekly, found, weeklyReset)
	}

	// A replayed terminal collection holds nothing newer, so it starts no
	// collection and leaves the record as it was.
	before := record
	if _, _, err := engine.collectTerminal(ctx, run.ID, attempt); err != nil {
		t.Fatal(err)
	}
	if err := engine.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetPoolUsageObservation(ctx, pool)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record, before) {
		t.Fatalf("record after a replayed terminal = %+v, want %+v", record, before)
	}
}
