package observe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/topicstore"
)

const (
	followRun        domain.RunID        = "run-follow-cli"
	followInvocation domain.InvocationID = "inv-follow-cli"
)

// followBase is fixed and safely in the past, so the seeded timeline renders
// the same instants on every run and a seeded observation is always older
// than any freshness window a test would pass.
var followBase = time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

func followAt(seconds int) time.Time {
	return followBase.Add(time.Duration(seconds) * time.Second)
}

// openFollowStore opens an empty store at a temporary path and returns both
// the handle for seeding and the path the command is given, so the test
// exercises the command's own open of the daemon's database.
func openFollowStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "freeside.db")
	st, _, err := topicstore.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st, path
}

func followMilestone(kind domain.RunMilestoneKind, seconds int) domain.RunMilestone {
	invocation := followInvocation
	return domain.RunMilestone{
		RunID: followRun, Kind: kind, InvocationID: &invocation, RecordedAt: followAt(seconds),
	}
}

func appendFollowMilestones(t *testing.T, st *store.Store, milestones ...domain.RunMilestone) {
	t.Helper()
	ctx := context.Background()
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, m := range milestones {
			if err := tx.AppendRunMilestone(ctx, m); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("append milestones: %v", err)
	}
}

func recordFollowObservation(
	t *testing.T, st *store.Store,
	status domain.ObservedInvocationStatus, live bool, observedAt time.Time,
) {
	t.Helper()
	ctx := context.Background()
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordInvocationObservation(ctx, domain.InvocationObservation{
			InvocationID: followInvocation, RunID: followRun,
			Status: status, Live: live, ObservedAt: observedAt,
		})
	}); err != nil {
		t.Fatalf("record observation: %v", err)
	}
}

func recordFollowHold(t *testing.T, st *store.Store, reason domain.RunHoldReason, seconds int) {
	t.Helper()
	ctx := context.Background()
	invocation := followInvocation
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordRunHold(ctx, domain.RunHoldObservation{
			RunID: followRun, InvocationID: &invocation, Reason: reason,
			FirstObservedAt: followAt(seconds), LastObservedAt: followAt(seconds),
		})
	}); err != nil {
		t.Fatalf("record hold: %v", err)
	}
}

func runFollowCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), args, &stdout, &stderr)
	if stderr.Len() > 0 {
		t.Logf("stderr: %s", stderr.String())
	}
	return stdout.String(), err
}

func TestFollowCommandCreatesAStoreThroughTheTopicKeyBoundary(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	_, err := runFollowCLI(t, "-db", dbPath, "-run", string(followRun), "-once")
	if !errors.Is(err, ErrNoObservedTimeline) {
		t.Fatalf("follow error = %v, want ErrNoObservedTimeline", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("stat follow-created store: %v", err)
	}
	info, err := os.Stat(dbPath + topicstore.KeySuffix)
	if err != nil {
		t.Fatalf("stat follow-created topic key: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != sha256.Size {
		t.Fatalf("follow-created topic key mode/size = %v/%d, want regular 0600/%d",
			info.Mode(), info.Size(), sha256.Size)
	}
}

func assertFollowContains(t *testing.T, output string, want ...string) {
	t.Helper()
	for _, line := range want {
		if !strings.Contains(output, line) {
			t.Errorf("output is missing %q:\n%s", line, output)
		}
	}
}

// TestFollowCommandFollowsANormalRunToItsOutcome is the CLI-level normal-run
// scenario: the command opens the daemon's own database, replays the observed
// timeline, and keeps following until the run's outcome is final, including
// milestones committed after the outcome's own transaction.
func TestFollowCommandFollowsANormalRunToItsOutcome(t *testing.T) {
	st, path := openFollowStore(t)
	appendFollowMilestones(t, st,
		followMilestone(domain.MilestoneRunSubmitted, 0),
		followMilestone(domain.MilestoneInvocationAdmitted, 1),
		followMilestone(domain.MilestoneInvocationStarted, 1))
	recordFollowObservation(t, st, domain.ObservedStatusRunning, true, followAt(2))

	// Advance the run while the follow is already reading, so the growing
	// timeline is what the command reports rather than a single snapshot.
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		appendFollowMilestones(t, st,
			followMilestone(domain.MilestoneExecutionExportRecorded, 20),
			followMilestone(domain.MilestonePublicationReady, 30))
		recordFollowObservation(t, st, domain.ObservedStatusCompleted, false, followAt(30))
		terminal := followMilestone(domain.MilestoneTerminalRecorded, 36)
		status := domain.ObservedStatusCompleted
		terminal.Terminal = &status
		appendFollowMilestones(t, st, terminal)
	}()

	output, err := runFollowCLI(t, "-db", path, "-run", string(followRun), "-interval", "5ms")
	<-completed
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	assertFollowContains(t, output,
		"run run-follow-cli",
		"2026-07-01T09:00:00Z  run_submitted  invocation=inv-follow-cli",
		"2026-07-01T09:00:01Z  invocation_admitted  invocation=inv-follow-cli",
		"2026-07-01T09:00:01Z  invocation_started  invocation=inv-follow-cli",
		"2026-07-01T09:00:20Z  execution_export_recorded  invocation=inv-follow-cli",
		"2026-07-01T09:00:30Z  publication_ready  invocation=inv-follow-cli",
		"2026-07-01T09:00:36Z  terminal_recorded  invocation=inv-follow-cli  terminal=completed",
		"outcome  published",
	)
	if !strings.HasSuffix(output, "outcome  published\n") {
		t.Errorf("follow did not end on the run's outcome:\n%s", output)
	}
}

// TestFollowCommandShowsAHeldRunsSafeReason is the CLI-level held-run
// scenario: the hold displays the contract's closed cause code, and nothing
// beside it, because no free-text reason exists to display.
func TestFollowCommandShowsAHeldRunsSafeReason(t *testing.T) {
	st, path := openFollowStore(t)
	appendFollowMilestones(t, st, followMilestone(domain.MilestoneRunSubmitted, 0))
	recordFollowHold(t, st, domain.HoldBlockingSystemHealth, 5)

	output, err := runFollowCLI(t, "-db", path, "-run", string(followRun), "-once")
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	assertFollowContains(t, output,
		"  hold  blocking_system_health  invocation=inv-follow-cli"+
			"  first-observed=2026-07-01T09:00:05Z  last-observed=2026-07-01T09:00:05Z",
		"outcome  pending",
	)
}

// TestFollowCommandReportsATerminalFailure is the CLI-level failure scenario:
// a failed execution ends the follow with the recorded terminal class, and
// observing a failed run is not itself a command failure.
func TestFollowCommandReportsATerminalFailure(t *testing.T) {
	st, path := openFollowStore(t)
	outcome := followMilestone(domain.MilestoneExecutionOutcomeRecorded, 40)
	recorded := domain.ExecutionOutcomeFailed
	outcome.Outcome = &recorded
	terminal := followMilestone(domain.MilestoneTerminalRecorded, 40)
	status := domain.ObservedStatusFailed
	terminal.Terminal = &status
	appendFollowMilestones(t, st,
		followMilestone(domain.MilestoneRunSubmitted, 0),
		followMilestone(domain.MilestoneInvocationStarted, 1),
		outcome, terminal)
	recordFollowObservation(t, st, domain.ObservedStatusFailed, false, followAt(40))

	output, err := runFollowCLI(t, "-db", path, "-run", string(followRun), "-interval", "5ms")
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	assertFollowContains(t, output,
		"2026-07-01T09:00:40Z  execution_outcome_recorded  invocation=inv-follow-cli  outcome=failed",
		"2026-07-01T09:00:40Z  terminal_recorded  invocation=inv-follow-cli  terminal=failed",
		"liveness=terminal",
		"outcome  failed  terminal=failed",
	)
}

// TestFollowCommandResumesAcrossReconnectAndDaemonRestart is the reconnect
// scenario: the timeline is durable, so a second command run replays every
// milestone the first one saw, and the observation that the stopped daemon
// left behind ages into an observation gap instead of a stale live verdict.
func TestFollowCommandResumesAcrossReconnectAndDaemonRestart(t *testing.T) {
	st, path := openFollowStore(t)
	appendFollowMilestones(t, st,
		followMilestone(domain.MilestoneRunSubmitted, 0),
		followMilestone(domain.MilestoneInvocationAdmitted, 1),
		followMilestone(domain.MilestoneInvocationStarted, 1))
	// A live observation the daemon wrote just before it stopped. The fixture
	// instant is in the past, so the command's own wall clock reads it
	// exactly as an operator reconnecting after a restart would.
	recordFollowObservation(t, st, domain.ObservedStatusRunning, true, followAt(2))

	first, err := runFollowCLI(t, "-db", path, "-run", string(followRun), "-once")
	if err != nil {
		t.Fatalf("first follow: %v", err)
	}
	second, err := runFollowCLI(t, "-db", path, "-run", string(followRun), "-once")
	if err != nil {
		t.Fatalf("reconnected follow: %v", err)
	}
	// elapsed= is rendered from the real clock as now-submitted truncated to
	// whole seconds, so the two invocations differ by a second whenever they
	// straddle a second boundary. Mask that one field: the assertion is that
	// the durable timeline replays identically across reconnect, not which
	// wall-clock second each invocation happened to land on.
	if maskElapsed(first) != maskElapsed(second) {
		t.Errorf("reconnect lost the observed timeline:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	assertFollowContains(t, second,
		"2026-07-01T09:00:00Z  run_submitted  invocation=inv-follow-cli",
		"2026-07-01T09:00:01Z  invocation_admitted  invocation=inv-follow-cli",
		"2026-07-01T09:00:01Z  invocation_started  invocation=inv-follow-cli",
		"status=running  liveness=observation_gap  observed-at=2026-07-01T09:00:02Z",
		"outcome  pending",
	)
	if strings.Contains(second, "liveness=observed_live") {
		t.Errorf("a stopped daemon's observation still reads as live:\n%s", second)
	}

	// A daemon that comes back and re-observes the runtime closes the gap, so
	// the reading above is the observation's age and not a display that can
	// never say live.
	recordFollowObservation(t, st, domain.ObservedStatusRunning, true, time.Now().UTC())
	live, err := runFollowCLI(t, "-db", path, "-run", string(followRun), "-once")
	if err != nil {
		t.Fatalf("resumed follow: %v", err)
	}
	assertFollowContains(t, live, "status=running  liveness=observed_live")
}

// elapsedValue matches the single clock-derived field in a follow output.
// Named to avoid colliding with the production elapsedField in follow.go.
var elapsedValue = regexp.MustCompile(`elapsed=\S+`)

// maskElapsed removes the one clock-derived value from a follow output so
// byte-for-byte comparisons assert timeline replay, not which wall-clock
// second the invocation happened to land on.
func maskElapsed(output string) string {
	return elapsedValue.ReplaceAllString(output, "elapsed=<masked>")
}

// TestFollowCommandShowsElapsedAndLastObservation pins the two quantities the
// contract does derive, and the absence of the one it refuses to invent. A
// concluded run's elapsed clock is frozen at its conclusion.
func TestFollowCommandShowsElapsedAndLastObservation(t *testing.T) {
	st, path := openFollowStore(t)
	terminal := followMilestone(domain.MilestoneTerminalRecorded, 40)
	status := domain.ObservedStatusCompleted
	terminal.Terminal = &status
	appendFollowMilestones(t, st,
		followMilestone(domain.MilestoneRunSubmitted, 0),
		followMilestone(domain.MilestoneInvocationStarted, 1),
		terminal,
		followMilestone(domain.MilestonePublicationReady, 45))

	output, err := runFollowCLI(t, "-db", path, "-run", string(followRun), "-once")
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	assertFollowContains(t, output,
		"status  elapsed=45s  last-observed=2026-07-01T09:00:45Z")
	for _, invented := range []string{"%", "percent", "progress"} {
		if strings.Contains(strings.ToLower(output), invented) {
			t.Errorf("output invents progress (%q):\n%s", invented, output)
		}
	}
}

// TestFollowCommandKeepsTheClockRunningUntilTheOutcomeIsFinal covers the run
// that completed its execution and is still waiting on import and
// publication. The model freezes elapsed at the terminal milestone, but the
// run is not over, so the display must keep counting rather than understate
// how long the operator has been waiting.
func TestFollowCommandKeepsTheClockRunningUntilTheOutcomeIsFinal(t *testing.T) {
	st, path := openFollowStore(t)
	terminal := followMilestone(domain.MilestoneTerminalRecorded, 40)
	status := domain.ObservedStatusCompleted
	terminal.Terminal = &status
	appendFollowMilestones(t, st,
		followMilestone(domain.MilestoneRunSubmitted, 0),
		followMilestone(domain.MilestoneInvocationStarted, 1),
		terminal)

	output, err := runFollowCLI(t, "-db", path, "-run", string(followRun), "-once")
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	assertFollowContains(t, output, "outcome  pending")
	if strings.Contains(output, "elapsed=40s") {
		t.Errorf("elapsed froze at the terminal milestone while the run is pending:\n%s", output)
	}
	// The fixture is dated well in the past, so a clock that is still running
	// reports far more than the run's 40 seconds of execution.
	if !strings.Contains(output, "elapsed=") || strings.Contains(output, "elapsed=unknown") {
		t.Errorf("pending run shows no elapsed clock:\n%s", output)
	}
}

func TestFollowCommandRefusesAnUnobservedRun(t *testing.T) {
	_, path := openFollowStore(t)
	output, err := runFollowCLI(t, "-db", path, "-run", "run-never-submitted", "-once")
	if !errors.Is(err, ErrNoObservedTimeline) {
		t.Fatalf("follow err = %v, want %v", err, ErrNoObservedTimeline)
	}
	if output != "" {
		t.Errorf("refused follow printed %q", output)
	}
}

func TestFollowCommandRejectsFlagMisuse(t *testing.T) {
	_, path := openFollowStore(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "no database", args: []string{"-run", string(followRun)}},
		{name: "no run", args: []string{"-db", path}},
		{
			name: "positional argument",
			args: []string{"-db", path, "-run", string(followRun), "extra"},
		},
		{
			name: "non-positive interval",
			args: []string{"-db", path, "-run", string(followRun), "-interval", "0s"},
		},
		{
			name: "non-positive freshness window",
			args: []string{"-db", path, "-run", string(followRun), "-freshness-window", "-1s"},
		},
		{name: "unknown flag", args: []string{"-db", path, "-run", string(followRun), "-tail"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := runFollowCLI(t, tc.args...)
			if err == nil {
				t.Fatalf("follow accepted %v, output:\n%s", tc.args, output)
			}
			// An invocation mistake is not an observation failure: the
			// caller has to be able to tell a typo from a run it could not
			// read, which is the whole point of the separate exit code.
			if !errors.Is(err, ErrUsage) {
				t.Errorf("follow %v err = %v, want %v", tc.args, err, ErrUsage)
			}
			if code := ExitCode(err); code != 2 {
				t.Errorf("follow %v exit = %d, want 2", tc.args, code)
			}
			if output != "" {
				t.Errorf("refused follow printed %q", output)
			}
		})
	}
}

// TestExitCodeSeparatesMisuseFromObservationFailure pins the exit contract the
// command documents: a caller has to be able to tell a bad invocation from a
// run it could not observe, and a successfully observed failure from both.
func TestExitCodeSeparatesMisuseFromObservationFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "observed", err: nil, want: 0},
		{name: "could not observe", err: ErrNoObservedTimeline, want: 1},
		{name: "wrapped read failure", err: errors.New("open store: disk is gone"), want: 1},
		{name: "invocation mistake", err: ErrUsage, want: 2},
		{
			name: "wrapped invocation mistake",
			err:  errors.Join(ErrUsage, errors.New("flag provided but not defined")),
			want: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
