package observe

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	fixtureRun        domain.RunID        = "run-follow"
	fixtureInvocation domain.InvocationID = "inv-1"
	// testInterval keeps the follow loop's waits negligible; the scenarios
	// are driven by the scripted reads, never by wall time.
	testInterval = time.Millisecond
)

var fixtureBase = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func at(seconds int) time.Time {
	return fixtureBase.Add(time.Duration(seconds) * time.Second)
}

func invocationMilestone(kind domain.RunMilestoneKind, seconds int) domain.RunMilestone {
	invocation := fixtureInvocation
	return domain.RunMilestone{
		RunID: fixtureRun, Kind: kind, InvocationID: &invocation, RecordedAt: at(seconds),
	}
}

func terminalMilestone(seconds int, terminal domain.ObservedInvocationStatus) domain.RunMilestone {
	m := invocationMilestone(domain.MilestoneTerminalRecorded, seconds)
	m.Terminal = &terminal
	return m
}

func outcomeMilestone(seconds int, outcome domain.ExecutionOutcomeStatus) domain.RunMilestone {
	m := invocationMilestone(domain.MilestoneExecutionOutcomeRecorded, seconds)
	m.Outcome = &outcome
	return m
}

func blockedMilestone(seconds int, reason domain.RunHoldReason) domain.RunMilestone {
	m := invocationMilestone(domain.MilestonePublicationBlocked, seconds)
	m.Reason = &reason
	return m
}

func invocationObservation(
	status domain.ObservedInvocationStatus, live bool, seconds int,
) domain.InvocationObservation {
	return domain.InvocationObservation{
		InvocationID: fixtureInvocation, RunID: fixtureRun,
		Status: status, Live: live, ObservedAt: at(seconds),
	}
}

func hold(reason domain.RunHoldReason, first, last int) *domain.RunHoldObservation {
	invocation := fixtureInvocation
	return &domain.RunHoldObservation{
		RunID: fixtureRun, InvocationID: &invocation, Reason: reason,
		FirstObservedAt: at(first), LastObservedAt: at(last),
	}
}

// snapshot builds one aggregate and asserts it is valid, so every scenario
// fixture doubles as a validation-positive case for the contract's shapes.
func snapshot(
	t *testing.T,
	milestones []domain.RunMilestone,
	held *domain.RunHoldObservation,
	observations ...domain.InvocationObservation,
) domain.RunObservation {
	t.Helper()
	o := domain.RunObservation{
		RunID: fixtureRun, Milestones: milestones, Hold: held, Invocations: observations,
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("fixture aggregate is invalid: %v", err)
	}
	return o
}

// scriptedObserver serves one snapshot per read and cancels the follow once
// the last one has been served, so a scenario whose run never concludes ends
// exactly as an operator's interrupt would.
type scriptedObserver struct {
	t         *testing.T
	snapshots []domain.RunObservation
	stop      context.CancelFunc
	err       error
	calls     int
	// runID is the run the scenario expects to be followed; zero means the
	// ordinary fixture run.
	runID domain.RunID
}

func (o *scriptedObserver) ObserveRun(
	_ context.Context, runID domain.RunID,
) (domain.RunObservation, error) {
	o.t.Helper()
	want := o.runID
	if want == "" {
		want = fixtureRun
	}
	if runID != want {
		o.t.Fatalf("followed run %q, want %q", runID, want)
	}
	if o.err != nil {
		return domain.RunObservation{}, o.err
	}
	index := min(o.calls, len(o.snapshots)-1)
	o.calls++
	if index == len(o.snapshots)-1 && o.stop != nil {
		o.stop()
	}
	return o.snapshots[index], nil
}

// stepClock reads the reader's clock once per follow iteration, so liveness
// and elapsed time are compared against the instant that scenario step
// describes rather than against wall time.
type stepClock struct {
	times []time.Time
	calls int
}

func (c *stepClock) now() time.Time {
	at := c.times[min(c.calls, len(c.times)-1)]
	c.calls++
	return at
}

type followResult struct {
	output string
	err    error
}

func runFollow(
	t *testing.T, snapshots []domain.RunObservation, clock []time.Time, cfg Config,
) followResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer := &scriptedObserver{t: t, snapshots: snapshots, stop: cancel}
	steps := &stepClock{times: clock}
	cfg.RunID, cfg.Interval, cfg.Now = fixtureRun, testInterval, steps.now
	var out bytes.Buffer
	err := Follow(ctx, observer, &out, cfg)
	return followResult{output: out.String(), err: err}
}

func assertOutput(t *testing.T, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := range max(len(gotLines), len(wantLines)) {
		g, w := "", ""
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Fatalf("output line %d:\n got %q\nwant %q\n\nfull output:\n%s", i+1, g, w, got)
		}
	}
	t.Fatalf("output = %q, want %q", got, want)
}

// assertNoCompletionFraction pins the contract's refusal to invent progress:
// no percentage can appear because no model field carries one.
func assertNoCompletionFraction(t *testing.T, output string) {
	t.Helper()
	for _, invented := range []string{"%", "percent", "complete=", "progress"} {
		if strings.Contains(strings.ToLower(output), invented) {
			t.Errorf("output invents progress (%q):\n%s", invented, output)
		}
	}
}

// TestFollowNormalRunToPublishedOutcome is the normal-run scenario: the
// timeline is emitted as it grows, the status block reprints only when the
// observed state changes, and the follow ends on the run's final outcome. It
// also pins the settling read: publication converges before the terminal
// record lands, and the later milestone is emitted rather than truncated.
func TestFollowNormalRunToPublishedOutcome(t *testing.T) {
	submitted := invocationMilestone(domain.MilestoneRunSubmitted, 0)
	admitted := invocationMilestone(domain.MilestoneInvocationAdmitted, 1)
	started := invocationMilestone(domain.MilestoneInvocationStarted, 1)
	exported := invocationMilestone(domain.MilestoneExecutionExportRecorded, 12)
	published := invocationMilestone(domain.MilestonePublicationReady, 30)
	terminal := terminalMilestone(36, domain.ObservedStatusCompleted)

	got := runFollow(t, []domain.RunObservation{
		snapshot(t, []domain.RunMilestone{submitted}, nil,
			invocationObservation(domain.ObservedStatusPending, false, 0)),
		snapshot(t, []domain.RunMilestone{submitted, admitted, started}, nil,
			invocationObservation(domain.ObservedStatusRunning, true, 2)),
		snapshot(t, []domain.RunMilestone{submitted, admitted, started, exported}, nil,
			invocationObservation(domain.ObservedStatusRunning, true, 12)),
		snapshot(t, []domain.RunMilestone{submitted, admitted, started, exported, published}, nil,
			invocationObservation(domain.ObservedStatusCompleted, false, 30)),
		snapshot(t, []domain.RunMilestone{
			submitted, admitted, started, exported, published, terminal,
		}, nil, invocationObservation(domain.ObservedStatusCompleted, false, 30)),
		snapshot(t, []domain.RunMilestone{
			submitted, admitted, started, exported, published, terminal,
		}, nil, invocationObservation(domain.ObservedStatusCompleted, false, 30)),
	}, []time.Time{at(0), at(2), at(12), at(30), at(36), at(36)}, Config{})

	if got.err != nil {
		t.Fatalf("follow: %v", got.err)
	}
	assertOutput(t, got.output, `run run-follow
2026-08-01T12:00:00Z  run_submitted  invocation=inv-1
status  elapsed=0s  last-observed=2026-08-01T12:00:00Z
  hold  none
  invocation  inv-1  status=pending  liveness=observed_idle  observed-at=2026-08-01T12:00:00Z
2026-08-01T12:00:01Z  invocation_admitted  invocation=inv-1
2026-08-01T12:00:01Z  invocation_started  invocation=inv-1
status  elapsed=2s  last-observed=2026-08-01T12:00:02Z
  hold  none
  invocation  inv-1  status=running  liveness=observed_live  observed-at=2026-08-01T12:00:02Z
2026-08-01T12:00:12Z  execution_export_recorded  invocation=inv-1
status  elapsed=12s  last-observed=2026-08-01T12:00:12Z
  hold  none
  invocation  inv-1  status=running  liveness=observed_live  observed-at=2026-08-01T12:00:12Z
2026-08-01T12:00:30Z  publication_ready  invocation=inv-1
status  elapsed=30s  last-observed=2026-08-01T12:00:30Z
  hold  none
  invocation  inv-1  status=completed  liveness=terminal  observed-at=2026-08-01T12:00:30Z
2026-08-01T12:00:36Z  terminal_recorded  invocation=inv-1  terminal=completed
status  elapsed=36s  last-observed=2026-08-01T12:00:36Z
  hold  none
  invocation  inv-1  status=completed  liveness=terminal  observed-at=2026-08-01T12:00:30Z
outcome  published
`)
	assertNoCompletionFraction(t, got.output)
}

// TestFollowHeldRunShowsTheClosedReason is the held-run scenario: the hold
// displays the contract's closed cause code with the span it has been
// observed over, and an invocation the daemon never looked at reads as
// never_observed. The refreshed hold instant is deliberately part of the
// displayed state: a run held before any invocation was observed has nothing
// else that advances, so without it the display would go silent and an
// operator could not tell a standing hold from a stopped daemon.
func TestFollowHeldRunShowsTheClosedReason(t *testing.T) {
	submitted := invocationMilestone(domain.MilestoneRunSubmitted, 0)
	got := runFollow(t, []domain.RunObservation{
		snapshot(t, []domain.RunMilestone{submitted}, hold(domain.HoldOperationStopped, 0, 0)),
		snapshot(t, []domain.RunMilestone{submitted}, hold(domain.HoldOperationStopped, 0, 10)),
	}, []time.Time{at(0), at(10)}, Config{})

	if got.err != nil {
		t.Fatalf("follow: %v", got.err)
	}
	assertOutput(t, got.output, `run run-follow
2026-08-01T12:00:00Z  run_submitted  invocation=inv-1
status  elapsed=0s  last-observed=2026-08-01T12:00:00Z
  hold  operation_stopped  invocation=inv-1  first-observed=2026-08-01T12:00:00Z  last-observed=2026-08-01T12:00:00Z
  invocation  inv-1  liveness=never_observed
status  elapsed=10s  last-observed=2026-08-01T12:00:10Z
  hold  operation_stopped  invocation=inv-1  first-observed=2026-08-01T12:00:00Z  last-observed=2026-08-01T12:00:10Z
  invocation  inv-1  liveness=never_observed
outcome  pending
`)
	assertNoCompletionFraction(t, got.output)
}

// TestFollowTerminalFailure is the terminal-failure scenario: a failed
// execution is a final outcome with no publication to wait for, and the
// recorded terminal class rides the outcome line.
func TestFollowTerminalFailure(t *testing.T) {
	submitted := invocationMilestone(domain.MilestoneRunSubmitted, 0)
	admitted := invocationMilestone(domain.MilestoneInvocationAdmitted, 1)
	started := invocationMilestone(domain.MilestoneInvocationStarted, 1)
	recorded := outcomeMilestone(40, domain.ExecutionOutcomeFailed)
	terminal := terminalMilestone(40, domain.ObservedStatusFailed)
	failed := []domain.RunMilestone{submitted, admitted, started, recorded, terminal}

	got := runFollow(t, []domain.RunObservation{
		snapshot(t, []domain.RunMilestone{submitted, admitted, started}, nil,
			invocationObservation(domain.ObservedStatusRunning, true, 5)),
		snapshot(t, failed, nil, invocationObservation(domain.ObservedStatusFailed, false, 40)),
		snapshot(t, failed, nil, invocationObservation(domain.ObservedStatusFailed, false, 40)),
	}, []time.Time{at(5), at(40), at(40)}, Config{})

	if got.err != nil {
		t.Fatalf("follow: %v", got.err)
	}
	assertOutput(t, got.output, `run run-follow
2026-08-01T12:00:00Z  run_submitted  invocation=inv-1
2026-08-01T12:00:01Z  invocation_admitted  invocation=inv-1
2026-08-01T12:00:01Z  invocation_started  invocation=inv-1
status  elapsed=5s  last-observed=2026-08-01T12:00:05Z
  hold  none
  invocation  inv-1  status=running  liveness=observed_live  observed-at=2026-08-01T12:00:05Z
2026-08-01T12:00:40Z  execution_outcome_recorded  invocation=inv-1  outcome=failed
2026-08-01T12:00:40Z  terminal_recorded  invocation=inv-1  terminal=failed
status  elapsed=40s  last-observed=2026-08-01T12:00:40Z
  hold  none
  invocation  inv-1  status=failed  liveness=terminal  observed-at=2026-08-01T12:00:40Z
outcome  failed  terminal=failed
`)
	assertNoCompletionFraction(t, got.output)
}

// TestFollowDistinguishesLivenessFromAnObservationGap is the daemon-restart
// scenario: nothing about the durable rows changes, but the last observation
// ages past the freshness window, so the display stops claiming the execution
// is observed and says so structurally.
func TestFollowDistinguishesLivenessFromAnObservationGap(t *testing.T) {
	submitted := invocationMilestone(domain.MilestoneRunSubmitted, 0)
	started := invocationMilestone(domain.MilestoneInvocationStarted, 1)
	running := snapshot(t, []domain.RunMilestone{submitted, started}, nil,
		invocationObservation(domain.ObservedStatusRunning, true, 5))

	got := runFollow(t, []domain.RunObservation{running, running},
		[]time.Time{at(6), at(90)}, Config{})

	if got.err != nil {
		t.Fatalf("follow: %v", got.err)
	}
	assertOutput(t, got.output, `run run-follow
2026-08-01T12:00:00Z  run_submitted  invocation=inv-1
2026-08-01T12:00:01Z  invocation_started  invocation=inv-1
status  elapsed=6s  last-observed=2026-08-01T12:00:05Z
  hold  none
  invocation  inv-1  status=running  liveness=observed_live  observed-at=2026-08-01T12:00:05Z
status  elapsed=1m30s  last-observed=2026-08-01T12:00:05Z
  hold  none
  invocation  inv-1  status=running  liveness=observation_gap  observed-at=2026-08-01T12:00:05Z
outcome  pending
`)
}

// TestFollowReconnectReplaysTheObservedTimeline is the reconnect scenario: a
// follow started after the run finished still shows every milestone the
// daemon durably observed, because the timeline is persisted rather than
// derived from a live join.
func TestFollowReconnectReplaysTheObservedTimeline(t *testing.T) {
	blocked := []domain.RunMilestone{
		invocationMilestone(domain.MilestoneRunSubmitted, 0),
		invocationMilestone(domain.MilestoneInvocationAdmitted, 1),
		invocationMilestone(domain.MilestoneInvocationStarted, 1),
		invocationMilestone(domain.MilestoneExecutionExportRecorded, 12),
		terminalMilestone(20, domain.ObservedStatusCompleted),
		blockedMilestone(30, domain.HoldVerificationFindings),
	}
	complete := snapshot(t, blocked, nil,
		invocationObservation(domain.ObservedStatusCompleted, false, 20))

	got := runFollow(t, []domain.RunObservation{complete},
		[]time.Time{at(600)}, Config{Once: true})

	if got.err != nil {
		t.Fatalf("follow: %v", got.err)
	}
	assertOutput(t, got.output, `run run-follow
2026-08-01T12:00:00Z  run_submitted  invocation=inv-1
2026-08-01T12:00:01Z  invocation_admitted  invocation=inv-1
2026-08-01T12:00:01Z  invocation_started  invocation=inv-1
2026-08-01T12:00:12Z  execution_export_recorded  invocation=inv-1
2026-08-01T12:00:20Z  terminal_recorded  invocation=inv-1  terminal=completed
2026-08-01T12:00:30Z  publication_blocked  invocation=inv-1  reason=verification_findings
status  elapsed=30s  last-observed=2026-08-01T12:00:30Z
  hold  none
  invocation  inv-1  status=completed  liveness=terminal  observed-at=2026-08-01T12:00:20Z
outcome  blocked  reason=verification_findings
`)
	assertNoCompletionFraction(t, got.output)
}

// TestFollowRefusesAnUnobservedRun keeps a mistyped run id from following
// nothing forever.
func TestFollowRefusesAnUnobservedRun(t *testing.T) {
	got := runFollow(t, []domain.RunObservation{{RunID: fixtureRun}},
		[]time.Time{at(0)}, Config{})
	if !errors.Is(got.err, ErrNoObservedTimeline) {
		t.Fatalf("follow err = %v, want %v", got.err, ErrNoObservedTimeline)
	}
	if got.output != "" {
		t.Errorf("refused follow printed %q", got.output)
	}
}

func TestFollowRequiresARunID(t *testing.T) {
	err := Follow(context.Background(), &scriptedObserver{t: t}, &bytes.Buffer{}, Config{})
	if !errors.Is(err, domain.ErrEmptyID) {
		t.Fatalf("follow err = %v, want %v", err, domain.ErrEmptyID)
	}
}

// interruptingObserver cancels the follow's own context and then fails the
// read with it, which is what an operator's interrupt looks like when it
// lands inside a read rather than between two.
type interruptingObserver struct {
	snapshot domain.RunObservation
	stop     context.CancelFunc
	calls    int
}

func (o *interruptingObserver) ObserveRun(
	_ context.Context, _ domain.RunID,
) (domain.RunObservation, error) {
	o.calls++
	if o.calls == 1 {
		return o.snapshot, nil
	}
	o.stop()
	return domain.RunObservation{}, context.Canceled
}

// TestFollowInterruptedDuringAReadEndsCleanly keeps the ordinary way to stop
// a follow from reporting a command failure, and holds that path to the same
// closing behaviour as an interrupt between reads: a status block dated to
// the interrupt, then the outcome. A blocking read can sit for a long time,
// so dating that block to the read before it would leave elapsed time and
// liveness stale by the interval plus however long the read was blocked.
func TestFollowInterruptedDuringAReadEndsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer := &interruptingObserver{
		snapshot: snapshot(t, []domain.RunMilestone{
			invocationMilestone(domain.MilestoneRunSubmitted, 0),
			invocationMilestone(domain.MilestoneInvocationStarted, 1),
		}, nil, invocationObservation(domain.ObservedStatusRunning, true, 2)),
		stop: cancel,
	}
	steps := &stepClock{times: []time.Time{at(3), at(600)}}
	var out bytes.Buffer
	err := Follow(ctx, observer, &out, Config{
		RunID: fixtureRun, Interval: testInterval, Now: steps.now,
	})
	if err != nil {
		t.Fatalf("interrupted follow: %v", err)
	}
	assertOutput(t, out.String(), `run run-follow
2026-08-01T12:00:00Z  run_submitted  invocation=inv-1
2026-08-01T12:00:01Z  invocation_started  invocation=inv-1
status  elapsed=3s  last-observed=2026-08-01T12:00:02Z
  hold  none
  invocation  inv-1  status=running  liveness=observed_live  observed-at=2026-08-01T12:00:02Z
status  elapsed=10m0s  last-observed=2026-08-01T12:00:02Z
  hold  none
  invocation  inv-1  status=running  liveness=observation_gap  observed-at=2026-08-01T12:00:02Z
outcome  pending
`)
}

// TestFollowPropagatesReadFailures keeps a failing read loud: the read
// surface fails closed on a row the vocabulary cannot express, and the
// command must not present that as an idle run.
func TestFollowPropagatesReadFailures(t *testing.T) {
	sentinel := errors.New("row names an unknown milestone kind")
	observer := &scriptedObserver{t: t, err: sentinel}
	err := Follow(context.Background(), observer, &bytes.Buffer{},
		Config{RunID: fixtureRun, Interval: testInterval})
	if !errors.Is(err, sentinel) {
		t.Fatalf("follow err = %v, want %v", err, sentinel)
	}
}

func TestConcludeClassifiesTheTimeline(t *testing.T) {
	verification := domain.HoldVerificationFindings
	canceled := domain.ObservedStatusCanceled
	gone := domain.ObservedStatusGone
	for _, tc := range []struct {
		name       string
		milestones []domain.RunMilestone
		want       Conclusion
	}{
		{
			name: "nothing terminal is pending",
			milestones: []domain.RunMilestone{
				invocationMilestone(domain.MilestoneInvocationStarted, 1),
			},
			want: Conclusion{Outcome: OutcomePending},
		},
		{
			name: "a completed execution still awaits publication",
			milestones: []domain.RunMilestone{
				terminalMilestone(10, domain.ObservedStatusCompleted),
			},
			want: Conclusion{Outcome: OutcomePending},
		},
		{
			name: "a cancelled execution is a final failure",
			milestones: []domain.RunMilestone{
				terminalMilestone(10, domain.ObservedStatusCanceled),
			},
			want: Conclusion{Outcome: OutcomeFailed, Terminal: &canceled, Final: true},
		},
		{
			name:       "a lost session is final",
			milestones: []domain.RunMilestone{terminalMilestone(10, domain.ObservedStatusGone)},
			want:       Conclusion{Outcome: OutcomeLost, Terminal: &gone, Final: true},
		},
		{
			name: "publication readiness is the run's outcome",
			milestones: []domain.RunMilestone{
				terminalMilestone(10, domain.ObservedStatusCompleted),
				invocationMilestone(domain.MilestonePublicationReady, 20),
			},
			want: Conclusion{Outcome: OutcomePublished, Final: true},
		},
		{
			name: "a definitive block outranks a ready publication",
			milestones: []domain.RunMilestone{
				invocationMilestone(domain.MilestonePublicationReady, 20),
				blockedMilestone(30, domain.HoldVerificationFindings),
			},
			want: Conclusion{Outcome: OutcomeBlocked, Reason: &verification, Final: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Conclude(domain.RunObservation{RunID: fixtureRun, Milestones: tc.milestones})
			if got.Outcome != tc.want.Outcome || got.Final != tc.want.Final {
				t.Fatalf("conclude = %+v, want %+v", got, tc.want)
			}
			// The production path must never build a shape its own contract
			// rejects, so every classification is checked against it.
			if err := got.Validate(); err != nil {
				t.Errorf("Conclude built an invalid conclusion: %v", err)
			}
			if !equalReason(got.Reason, tc.want.Reason) {
				t.Errorf("conclude reason = %v, want %v", got.Reason, tc.want.Reason)
			}
			if !equalTerminal(got.Terminal, tc.want.Terminal) {
				t.Errorf("conclude terminal = %v, want %v", got.Terminal, tc.want.Terminal)
			}
		})
	}
}

func equalReason(got, want *domain.RunHoldReason) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

func equalTerminal(got, want *domain.ObservedInvocationStatus) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

// TestFollowEscapesHostileIdentifiers is the display-integrity case. Run and
// invocation ids are the only rendered values a caller chooses
// (domain.Run.Validate requires only non-empty), so a durable id holding a
// newline or an ANSI escape must not be able to forge a line in this display
// or drive the operator's terminal.
func TestFollowEscapesHostileIdentifiers(t *testing.T) {
	const (
		hostileRun        domain.RunID        = "run-x\noutcome  published"
		hostileInvocation domain.InvocationID = "inv-\x1b[31mred\x1b[0m"
	)
	invocation := hostileInvocation
	milestone := domain.RunMilestone{
		RunID: hostileRun, Kind: domain.MilestoneRunSubmitted,
		InvocationID: &invocation, RecordedAt: at(0),
	}
	observation := domain.RunObservation{
		RunID: hostileRun, Milestones: []domain.RunMilestone{milestone},
		Hold: &domain.RunHoldObservation{
			RunID: hostileRun, InvocationID: &invocation,
			Reason:          domain.HoldOperationStopped,
			FirstObservedAt: at(0), LastObservedAt: at(0),
		},
	}
	if err := observation.Validate(); err != nil {
		t.Fatalf("hostile ids are rejected by the model, so this case cannot arise: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	steps := &stepClock{times: []time.Time{at(0)}}
	var out bytes.Buffer
	err := Follow(ctx, &scriptedObserver{
		t: t, snapshots: []domain.RunObservation{observation}, runID: hostileRun,
	}, &out, Config{RunID: hostileRun, Interval: testInterval, Once: true, Now: steps.now})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}

	got := out.String()
	if strings.ContainsAny(got, "\x1b\r") {
		t.Errorf("terminal control bytes reached the display:\n%q", got)
	}
	// The escaped id keeps its payload on one line, which is the point: the
	// bytes may still read as "outcome  published" inside a quoted id, but no
	// line of the display can be manufactured from them. The real outcome is
	// the last line, so any other line claiming one is a forgery.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	for i, line := range lines {
		if i < len(lines)-1 && strings.HasPrefix(line, "outcome  ") {
			t.Errorf("a hostile id forged an outcome line at %d:\n%q", i+1, got)
		}
		if line == "" {
			t.Errorf("a hostile id split the display across a blank line:\n%q", got)
		}
	}
	if lines[len(lines)-1] != "outcome  pending" {
		t.Errorf("the real outcome line is not last:\n%q", got)
	}
	if !strings.Contains(got, `run "run-x\noutcome  published"`) {
		t.Errorf("the run id was not quoted:\n%q", got)
	}
	if !strings.Contains(got, `invocation="inv-\x1b[31mred\x1b[0m"`) {
		t.Errorf("the invocation id was not quoted:\n%q", got)
	}
}

// TestIdentifierLeavesOrdinaryIdsAlone keeps the escaping from taxing the
// normal case: an ordinary id renders verbatim, unquoted.
func TestIdentifierLeavesOrdinaryIdsAlone(t *testing.T) {
	for _, id := range []string{
		"run-walking-skeleton",
		"run-3f9a2c11",
		"inv-prod-1",
		"run-café",
	} {
		if got := identifier(id); got != id {
			t.Errorf("identifier(%q) = %q, want it unchanged", id, got)
		}
	}
	for _, id := range []string{"", "run one", `run"two`, "run\nthree", "run\x00four"} {
		if got := identifier(id); got == id {
			t.Errorf("identifier(%q) rendered verbatim; it needs quoting", id)
		}
	}
}

// TestFollowResamplesTheClockOnInterrupt covers the long-interval interrupt:
// the wait the follow just left can be an entire interval, so the closing
// status block must be dated to the interrupt rather than to the read before
// it. Dating it to the stale instant would understate elapsed time and,
// because the block would compare equal to the one already on screen, print
// no closing status at all.
func TestFollowResamplesTheClockOnInterrupt(t *testing.T) {
	running := snapshot(t, []domain.RunMilestone{
		invocationMilestone(domain.MilestoneRunSubmitted, 0),
		invocationMilestone(domain.MilestoneInvocationStarted, 1),
	}, nil, invocationObservation(domain.ObservedStatusRunning, true, 2))

	got := runFollow(t, []domain.RunObservation{running},
		[]time.Time{at(3), at(600)}, Config{Interval: time.Hour})

	if got.err != nil {
		t.Fatalf("follow: %v", got.err)
	}
	assertOutput(t, got.output, `run run-follow
2026-08-01T12:00:00Z  run_submitted  invocation=inv-1
2026-08-01T12:00:01Z  invocation_started  invocation=inv-1
status  elapsed=3s  last-observed=2026-08-01T12:00:02Z
  hold  none
  invocation  inv-1  status=running  liveness=observed_live  observed-at=2026-08-01T12:00:02Z
status  elapsed=10m0s  last-observed=2026-08-01T12:00:02Z
  hold  none
  invocation  inv-1  status=running  liveness=observation_gap  observed-at=2026-08-01T12:00:02Z
outcome  pending
`)
}

// TestConclusionValidatesItsOutcomeContract pins the enum's registration and
// the outcome-scoped detail fields, so a new outcome has to declare both.
func TestConclusionValidatesItsOutcomeContract(t *testing.T) {
	if got, want := AllOutcomes, domain.AllRunOutcomes; !slices.Equal(got, want) {
		t.Fatalf("AllOutcomes = %v, want shared domain registration %v", got, want)
	}

	reason := domain.HoldVerificationFindings
	terminal := domain.ObservedStatusFailed
	bogusReason := domain.RunHoldReason("because")
	for _, tc := range []struct {
		name       string
		conclusion Conclusion
		wantErr    error
	}{
		{name: "pending", conclusion: Conclusion{Outcome: OutcomePending}},
		{name: "published", conclusion: Conclusion{Outcome: OutcomePublished, Final: true}},
		{
			name:       "blocked",
			conclusion: Conclusion{Outcome: OutcomeBlocked, Reason: &reason, Final: true},
		},
		{
			name:       "failed",
			conclusion: Conclusion{Outcome: OutcomeFailed, Terminal: &terminal, Final: true},
		},
		{
			name:       "unregistered outcome",
			conclusion: Conclusion{Outcome: "shipped", Final: true},
			wantErr:    ErrInvalidOutcome,
		},
		{
			name:       "blocked with no reason",
			conclusion: Conclusion{Outcome: OutcomeBlocked, Final: true},
			wantErr:    ErrOutcomeDetailMismatch,
		},
		{
			name:       "failed with no terminal",
			conclusion: Conclusion{Outcome: OutcomeFailed, Final: true},
			wantErr:    ErrOutcomeDetailMismatch,
		},
		{
			name:       "pending claiming to be final",
			conclusion: Conclusion{Outcome: OutcomePending, Final: true},
			wantErr:    ErrOutcomeDetailMismatch,
		},
		{
			name:       "published carrying a reason",
			conclusion: Conclusion{Outcome: OutcomePublished, Reason: &reason, Final: true},
			wantErr:    ErrOutcomeDetailMismatch,
		},
		{
			name:       "blocked with a reason outside the vocabulary",
			conclusion: Conclusion{Outcome: OutcomeBlocked, Reason: &bogusReason, Final: true},
			wantErr:    domain.ErrInvalidRunHoldReason,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.conclusion.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
