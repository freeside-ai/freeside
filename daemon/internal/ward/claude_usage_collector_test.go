package ward_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// This file is the external test package because exec/claude, whose parser
// turns the pinned CLI's fixtures into captures, imports ward.

const claudeUsageEnrollment = domain.ClientEnrollmentID("enr-claude")

var (
	claudeUsageObservedAt = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	fiveHourLength        = 5 * time.Hour
	sevenDayLength        = 7 * 24 * time.Hour
)

func claudeUsageFixture(t *testing.T, name string) exec.ClaudeUsageCapture {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "exec", "claude", "testdata", name)) //nolint:gosec // fixed fixture names
	if err != nil {
		t.Fatal(err)
	}
	capture := claude.ExtractUsageCapture(body, claudeUsageObservedAt)
	if capture == nil {
		t.Fatalf("%s carries no usage capture", name)
	}
	return *capture
}

func claudeUsageRequest(generation int) ward.UsageCollectionRequest {
	return ward.UsageCollectionRequest{
		Enrollment: domain.ClientEnrollment{ID: claudeUsageEnrollment, HarnessClient: domain.HarnessClientClaudeCode},
		Generation: generation,
	}
}

// collectClaudeUsage records capture under generation 1 and returns the
// answer to a collection under the same enrollment and generation.
func collectClaudeUsage(t *testing.T, capture exec.ClaudeUsageCapture) (ward.UsageCollection, error) {
	t.Helper()
	collector := ward.NewClaudeUsageCollector()
	if !collector.Record(claudeUsageEnrollment, 1, capture) {
		t.Fatal("Record() = false for a valid first capture")
	}
	return collector.CollectUsage(context.Background(), claudeUsageRequest(1))
}

func reading(window string, length time.Duration, used *int, resetsAt *time.Time, status domain.WindowStatus) domain.WindowReading {
	return domain.WindowReading{
		Window: window, EnrollmentID: claudeUsageEnrollment, Generation: 1, Status: status,
		UsedPercent: used, ResetsAt: resetsAt, Length: &length, ObservedAt: claudeUsageObservedAt,
	}
}

// TestClaudeUsageCollectorPinnedCLIWindows pins the readings the pinned
// 2.1.282 CLI's captures produce: each known window with its length, share,
// and reset, stamped with the enrollment, generation, and capture time.
func TestClaudeUsageCollectorPinnedCLIWindows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture string
		want    []domain.WindowReading
	}{
		{"claude_usage_loopback_turn.jsonl", []domain.WindowReading{
			reading("five_hour", fiveHourLength, new(4), new(time.Date(2026, 11, 4, 16, 28, 26, 0, time.UTC)),
				domain.WindowStatusAllowed),
			// The event's reset wins over the answer's.
			reading("seven_day", sevenDayLength, new(57), new(time.Unix(1793892506, 0).UTC()),
				domain.WindowStatusAllowed),
		}},
		{"claude_usage_observed_turn_seeded.jsonl", []domain.WindowReading{
			reading("seven_day", sevenDayLength, new(57), new(time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)),
				domain.WindowStatusAllowed),
		}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			got, err := collectClaudeUsage(t, claudeUsageFixture(t, tc.fixture))
			if err != nil {
				t.Fatalf("CollectUsage() error = %v", err)
			}
			if got.Availability != domain.AvailabilitySupported || got.Plan != nil || got.ModelWindows != nil {
				t.Fatalf("CollectUsage() = %+v, want supported with no plan or model windows", got)
			}
			if !reflect.DeepEqual(got.Windows, tc.want) {
				t.Fatalf("windows = %#v, want %#v", got.Windows, tc.want)
			}
			for _, window := range got.Windows {
				if err := window.Validate(); err != nil {
					t.Fatalf("reading %s is invalid: %v", window.Window, err)
				}
			}
			if strings.Contains(got.Detail, "left out") || strings.Contains(got.Detail, "ignored") {
				t.Fatalf("detail = %q, want no unknown window or ignored event", got.Detail)
			}
		})
	}
}

// TestClaudeUsageCollectorStatus pins the status mapping: rejected is
// exhausted, allowed and allowed_warning are allowed, an unknown status is
// ignored and named, and a window named only by an event carries no share.
func TestClaudeUsageCollectorStatus(t *testing.T) {
	t.Parallel()
	answerReset := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	eventReset := time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		windows    []exec.ClaudeUsageWindow
		events     []exec.ClaudeRateLimitEvent
		want       []domain.WindowReading
		wantDetail string
	}{
		{
			name:    "rejected is exhausted and its reset wins",
			windows: []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: 100, ResetsAt: &answerReset}},
			events:  []exec.ClaudeRateLimitEvent{{Window: "seven_day", Status: "rejected", ResetsAt: &eventReset}},
			want: []domain.WindowReading{
				reading("seven_day", sevenDayLength, new(100), &eventReset, domain.WindowStatusExhausted),
			},
		},
		{
			name:    "allowed_warning is allowed and an event without a reset keeps the answer's",
			windows: []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: 91, ResetsAt: &answerReset}},
			events:  []exec.ClaudeRateLimitEvent{{Window: "seven_day", Status: "allowed_warning"}},
			want: []domain.WindowReading{
				reading("seven_day", sevenDayLength, new(91), &answerReset, domain.WindowStatusAllowed),
			},
		},
		{
			name:    "a window with no event is allowed",
			windows: []exec.ClaudeUsageWindow{{Window: "five_hour", UsedPercent: 12}},
			want: []domain.WindowReading{
				reading("five_hour", fiveHourLength, new(12), nil, domain.WindowStatusAllowed),
			},
		},
		{
			name:    "an unknown status is ignored and named",
			windows: []exec.ClaudeUsageWindow{{Window: "five_hour", UsedPercent: 30, ResetsAt: &answerReset}},
			events:  []exec.ClaudeRateLimitEvent{{Window: "five_hour", Status: "throttled", ResetsAt: &eventReset}},
			want: []domain.WindowReading{
				reading("five_hour", fiveHourLength, new(30), &answerReset, domain.WindowStatusAllowed),
			},
			wantDetail: "ignored rate-limit events five_hour status throttled",
		},
		{
			name:    "a window only an event names has status, reset, and length",
			windows: []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: 20, ResetsAt: &answerReset}},
			events:  []exec.ClaudeRateLimitEvent{{Window: "five_hour", Status: "rejected", ResetsAt: &eventReset}},
			want: []domain.WindowReading{
				reading("five_hour", fiveHourLength, nil, &eventReset, domain.WindowStatusExhausted),
				reading("seven_day", sevenDayLength, new(20), &answerReset, domain.WindowStatusAllowed),
			},
		},
		{
			name: "unknown windows are left out and named",
			windows: []exec.ClaudeUsageWindow{
				{Window: "seven_day", UsedPercent: 40},
				{Window: "seven_day_opus", UsedPercent: 70},
			},
			events: []exec.ClaudeRateLimitEvent{{Window: "overage", Status: "rejected"}},
			want: []domain.WindowReading{
				reading("seven_day", sevenDayLength, new(40), nil, domain.WindowStatusAllowed),
			},
			wantDetail: "left out unknown windows overage, seven_day_opus",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := collectClaudeUsage(t, exec.ClaudeUsageCapture{
				ObservedAt: claudeUsageObservedAt, Windows: tc.windows, Events: tc.events,
			})
			if err != nil {
				t.Fatalf("CollectUsage() error = %v", err)
			}
			if !reflect.DeepEqual(got.Windows, tc.want) {
				t.Fatalf("windows = %#v, want %#v", got.Windows, tc.want)
			}
			if tc.wantDetail != "" && !strings.Contains(got.Detail, tc.wantDetail) {
				t.Fatalf("detail = %q, want it to contain %q", got.Detail, tc.wantDetail)
			}
		})
	}
}

// TestClaudeUsageCollectorNoKnownWindowFails pins that a capture yielding no
// reading fails, naming what it saw, so a full report never erases the pool's
// windows with an empty set.
func TestClaudeUsageCollectorNoKnownWindowFails(t *testing.T) {
	t.Parallel()
	_, err := collectClaudeUsage(t, exec.ClaudeUsageCapture{
		ObservedAt: claudeUsageObservedAt,
		Windows:    []exec.ClaudeUsageWindow{{Window: "seven_day_opus", UsedPercent: 70}},
		Events:     []exec.ClaudeRateLimitEvent{{Window: "five_hour", Status: "paused"}},
	})
	if err == nil {
		t.Fatal("CollectUsage() error = nil, want a failed attempt")
	}
	for _, want := range []string{"seven_day_opus", "five_hour status paused"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
}

// TestClaudeUsageCollectorDropsAnEventOnlyCapture pins that a capture with
// rate-limit events and no get_usage answer is never held: the pinned CLI's
// plain -p turn, which a specification stage runs, has exactly that shape. A
// full report from it would replace the pool's windows with the events'
// alone, so the answer held before stays the one collected.
func TestClaudeUsageCollectorDropsAnEventOnlyCapture(t *testing.T) {
	t.Parallel()
	collector := ward.NewClaudeUsageCollector()
	if !collector.Record(claudeUsageEnrollment, 1, claudeUsageFixture(t, "claude_usage_loopback_turn.jsonl")) {
		t.Fatal("Record(loopback) = false")
	}
	eventOnly := claudeUsageFixture(t, "claude_usage_observed_turn.jsonl")
	eventOnly.ObservedAt = claudeUsageObservedAt.Add(time.Hour)
	if len(eventOnly.Windows) != 0 || len(eventOnly.Events) == 0 {
		t.Fatalf("fixture capture = %+v, want events and no answer", eventOnly)
	}
	if collector.Record(claudeUsageEnrollment, 1, eventOnly) {
		t.Fatal("Record() kept a capture with no get_usage answer")
	}
	got, err := collector.CollectUsage(context.Background(), claudeUsageRequest(1))
	if err != nil {
		t.Fatalf("CollectUsage() error = %v", err)
	}
	if len(got.Windows) != 2 || got.Windows[1].Window != "seven_day" || got.Windows[1].UsedPercent == nil {
		t.Fatalf("windows = %+v, want the loopback answer's five_hour and seven_day", got.Windows)
	}
}

// TestClaudeUsageCollectorEnrollmentAndGeneration pins that the collector
// answers only from a capture held under the requested enrollment and
// generation, and that an empty memory says why it failed.
func TestClaudeUsageCollectorEnrollmentAndGeneration(t *testing.T) {
	t.Parallel()
	capture := claudeUsageFixture(t, "claude_usage_observed_turn_seeded.jsonl")
	collector := ward.NewClaudeUsageCollector()
	ctx := context.Background()

	_, err := collector.CollectUsage(ctx, claudeUsageRequest(1))
	if err == nil || !strings.Contains(err.Error(), "no writer turn has reported usage under enrollment enr-claude generation 1") {
		t.Fatalf("empty memory error = %v, want the no-writer-turn failure", err)
	}

	if !collector.Record(claudeUsageEnrollment, 2, capture) {
		t.Fatal("Record(generation 2) = false")
	}
	if _, err := collector.CollectUsage(ctx, claudeUsageRequest(1)); err == nil {
		t.Fatal("CollectUsage(generation 1) answered from a generation 2 capture")
	}
	other := claudeUsageRequest(2)
	other.Enrollment.ID = "enr-other"
	if _, err := collector.CollectUsage(ctx, other); err == nil {
		t.Fatal("CollectUsage(enr-other) answered from enr-claude's capture")
	}
	got, err := collector.CollectUsage(ctx, claudeUsageRequest(2))
	if err != nil {
		t.Fatalf("CollectUsage(generation 2) error = %v", err)
	}
	for _, window := range got.Windows {
		if window.EnrollmentID != claudeUsageEnrollment || window.Generation != 2 {
			t.Fatalf("reading %s stamped %s generation %d, want %s generation 2",
				window.Window, window.EnrollmentID, window.Generation, claudeUsageEnrollment)
		}
	}

	// A later turn under the replaced generation is never kept.
	later := capture
	later.ObservedAt = capture.ObservedAt.Add(time.Hour)
	if collector.Record(claudeUsageEnrollment, 1, later) {
		t.Fatal("Record(generation 1) kept a capture under a replaced generation")
	}
	if _, err := collector.CollectUsage(ctx, claudeUsageRequest(2)); err != nil {
		t.Fatalf("CollectUsage(generation 2) after a replaced capture error = %v", err)
	}
}

// TestClaudeUsageCollectorNewerWins pins that a capture no newer than the one
// held is dropped, so a replayed terminal collection can't roll a reading
// back, and that an invalid capture is never held.
func TestClaudeUsageCollectorNewerWins(t *testing.T) {
	t.Parallel()
	collector := ward.NewClaudeUsageCollector()
	held := exec.ClaudeUsageCapture{
		ObservedAt: claudeUsageObservedAt,
		Windows:    []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: 40}},
	}
	if !collector.Record(claudeUsageEnrollment, 1, held) {
		t.Fatal("Record() = false for the first capture")
	}
	for _, tc := range []struct {
		name    string
		at      time.Time
		used    int
		kept    bool
		wantUse int
	}{
		{"same instant", claudeUsageObservedAt, 10, false, 40},
		{"older", claudeUsageObservedAt.Add(-time.Minute), 10, false, 40},
		{"newer", claudeUsageObservedAt.Add(time.Minute), 45, true, 45},
	} {
		capture := exec.ClaudeUsageCapture{
			ObservedAt: tc.at,
			Windows:    []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: tc.used}},
		}
		if kept := collector.Record(claudeUsageEnrollment, 1, capture); kept != tc.kept {
			t.Fatalf("%s: Record() = %t, want %t", tc.name, kept, tc.kept)
		}
		got, err := collector.CollectUsage(context.Background(), claudeUsageRequest(1))
		if err != nil {
			t.Fatalf("%s: CollectUsage() error = %v", tc.name, err)
		}
		if used := *got.Windows[0].UsedPercent; used != tc.wantUse {
			t.Fatalf("%s: used_percent = %d, want %d", tc.name, used, tc.wantUse)
		}
	}

	for name, capture := range map[string]exec.ClaudeUsageCapture{
		"empty":          {ObservedAt: claudeUsageObservedAt.Add(time.Hour)},
		"local time":     {ObservedAt: claudeUsageObservedAt.Add(time.Hour).In(time.FixedZone("CET", 3600)), Windows: held.Windows},
		"share over 100": {ObservedAt: claudeUsageObservedAt.Add(time.Hour), Windows: []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: 101}}},
	} {
		if collector.Record(claudeUsageEnrollment, 1, capture) {
			t.Fatalf("Record(%s) kept an invalid capture", name)
		}
	}
	if collector.Record("", 1, held) || collector.Record(claudeUsageEnrollment, 0, held) {
		t.Fatal("Record() kept a capture with no enrollment or generation")
	}
}

// claudeUsageCollectorImports is the complete set the collector's source may
// import. It reads memory only: no net, os/exec, or wardstore import, and of
// internal/exec it names only the capture's types (claudeUsageCollectorExecNames).
var claudeUsageCollectorImports = map[string]bool{
	"context": true, "fmt": true, "maps": true, "slices": true, "strings": true, "sync": true, "time": true,
	"github.com/freeside-ai/freeside/daemon/internal/domain": true,
	"github.com/freeside-ai/freeside/daemon/internal/exec":   true,
}

var claudeUsageCollectorExecNames = map[string]bool{
	"ClaudeUsageCapture": true,
}

// TestClaudeUsageCollectorMakesNoRequest pins the collector's source to the
// imports and exec names above, so it can't open a connection, start a
// process, or reach a credential. Widening either is a deliberate change to
// the allowlists, not a passing test.
func TestClaudeUsageCollectorMakesNoRequest(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "claude_usage_collector.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Name != nil || !claudeUsageCollectorImports[imported] {
			t.Errorf("claude_usage_collector.go imports %s outside the memory-only boundary", spec.Path.Value)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "exec" && !claudeUsageCollectorExecNames[selector.Sel.Name] {
			t.Errorf("claude_usage_collector.go names exec.%s outside the capture types", selector.Sel.Name)
		}
		return true
	})
}
