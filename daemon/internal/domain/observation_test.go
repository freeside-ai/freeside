package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validMilestone(kind RunMilestoneKind) RunMilestone {
	inv := InvocationID("inv-1")
	m := RunMilestone{
		RunID: "run-1", Kind: kind, InvocationID: &inv,
		RecordedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	switch kind {
	case MilestoneTerminalRecorded:
		terminal := ObservedStatusCompleted
		m.Terminal = &terminal
	case MilestoneExecutionOutcomeRecorded:
		outcome := ExecutionOutcomeLost
		m.Outcome = &outcome
	case MilestonePublicationBlocked:
		reason := HoldTrustBlocked
		m.Reason = &reason
	case MilestoneRunSubmitted, MilestoneInvocationAdmitted,
		MilestoneInvocationStarted, MilestoneExecutionExportRecorded,
		MilestonePublicationReady:
	}
	return m
}

func TestRunOutcomeRegistrationAndConclusion(t *testing.T) {
	for _, outcome := range AllRunOutcomes {
		if !outcome.valid() {
			t.Errorf("registered run outcome %q is invalid", outcome)
		}
	}
	if RunOutcome("").valid() || RunOutcome("shipped").valid() {
		t.Error("an unregistered run outcome validates")
	}

	reason := HoldVerificationFindings
	conclusion := ConcludeRun(RunObservation{
		RunID: "run-1",
		Milestones: []RunMilestone{{
			RunID: "run-1", Kind: MilestonePublicationBlocked,
			InvocationID: ptr(InvocationID("inv-1")), Reason: &reason,
			RecordedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}},
	})
	if conclusion.Outcome != RunOutcomeBlocked || conclusion.Reason == nil ||
		*conclusion.Reason != reason || !conclusion.Final {
		t.Fatalf("ConcludeRun() = %+v, want final verification block", conclusion)
	}
	if err := conclusion.Validate(); err != nil {
		t.Fatalf("ConcludeRun().Validate() = %v", err)
	}
}

func TestConcludeRunClearsPriorTerminalWhenAnotherAttemptMakesProgress(t *testing.T) {
	failed := ObservedStatusFailed
	first := InvocationID("inv-1")
	second := InvocationID("inv-2")
	observation := RunObservation{
		RunID: "run-1",
		Milestones: []RunMilestone{
			{RunID: "run-1", Kind: MilestoneTerminalRecorded, InvocationID: &first, Terminal: &failed},
			{RunID: "run-1", Kind: MilestoneInvocationAdmitted, InvocationID: &second},
		},
	}
	if got := ConcludeRun(observation); got.Outcome != RunOutcomePending || got.Final {
		t.Fatalf("ConcludeRun() = %+v, want pending retry", got)
	}

	// A replay of the failed attempt is not a retry and must leave its
	// terminal conclusion intact.
	observation.Milestones[1].InvocationID = &first
	if got := ConcludeRun(observation); got.Outcome != RunOutcomeFailed || !got.Final {
		t.Fatalf("ConcludeRun() after same-attempt replay = %+v, want failed final", got)
	}
}

// TestConcludeRunEmptyHistoryIsUnobserved pins the pre-migration-0024 legacy
// run: a run with no observation history classifies as unobserved (non-final,
// pending-shaped detail), and either a first observed milestone or a
// nonterminal invocation observation flips it to pending. Only a run with
// neither a milestone nor a nonterminal invocation observation is unobserved.
func TestConcludeRunEmptyHistoryIsUnobserved(t *testing.T) {
	got := ConcludeRun(RunObservation{RunID: "run-1"})
	if got.Outcome != RunOutcomeUnobserved || got.Final ||
		got.Reason != nil || got.Terminal != nil {
		t.Fatalf("ConcludeRun(empty) = %+v, want non-final unobserved", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("ConcludeRun(empty).Validate() = %v", err)
	}

	// The boundary: a single observed milestone is enough to leave
	// unobserved for pending, preserving the 0024 no-backfill rule (an
	// in-flight run that gains its first milestone across the upgrade).
	boundary := ConcludeRun(RunObservation{
		RunID:      "run-1",
		Milestones: []RunMilestone{validMilestone(MilestoneRunSubmitted)},
	})
	if boundary.Outcome != RunOutcomePending || boundary.Final {
		t.Fatalf("ConcludeRun(one milestone) = %+v, want pending", boundary)
	}

	// A pre-0024 run still executing across the upgrade gains liveness
	// observations, not milestones: observeInvocation records invocation
	// observations, never milestones. A nonterminal (running) invocation
	// observation is in-flight evidence, so the run is pending, not
	// unobserved, even though its milestone history stays empty.
	live := ConcludeRun(RunObservation{
		RunID: "run-1",
		Invocations: []InvocationObservation{{
			InvocationID: "inv-1", RunID: "run-1",
			Status: ObservedStatusRunning, Live: true,
			ObservedAt: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
		}},
	})
	if live.Outcome != RunOutcomePending || live.Final {
		t.Fatalf("ConcludeRun(live invocation, no milestone) = %+v, want pending", live)
	}

	// A terminal-only invocation observation with no milestone is not
	// in-flight evidence, so the run stays unobserved rather than reading as
	// still running: the workflow records the terminal outcome as a
	// milestone, which is what refines it.
	terminalObs := ConcludeRun(RunObservation{
		RunID: "run-1",
		Invocations: []InvocationObservation{{
			InvocationID: "inv-1", RunID: "run-1",
			Status: ObservedStatusFailed, Live: false,
			ObservedAt: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
		}},
	})
	if terminalObs.Outcome != RunOutcomeUnobserved || terminalObs.Final {
		t.Fatalf("ConcludeRun(terminal invocation, no milestone) = %+v, want unobserved", terminalObs)
	}
}

// TestRunMilestoneValidatePerKind: every registered kind has a valid fixture
// (the golden discipline), and each kind-scoped detail field is required
// exactly where declared.
func TestRunMilestoneValidatePerKind(t *testing.T) {
	for _, kind := range AllRunMilestoneKinds {
		m := validMilestone(kind)
		if err := m.Validate(); err != nil {
			t.Errorf("kind %s: valid fixture rejected: %v", kind, err)
		}
	}

	terminal := ObservedStatusCompleted
	outcome := ExecutionOutcomeFailed
	reason := HoldRecipeRevoked
	mutations := []struct {
		name   string
		mutate func(*RunMilestone)
		want   error
	}{
		{"missing run id", func(m *RunMilestone) { m.RunID = "" }, ErrEmptyID},
		{"invalid kind", func(m *RunMilestone) { m.Kind = "sideways" }, ErrInvalidRunMilestoneKind},
		{"zero kind", func(m *RunMilestone) { m.Kind = "" }, ErrInvalidRunMilestoneKind},
		{"empty invocation id", func(m *RunMilestone) { empty := InvocationID(""); m.InvocationID = &empty }, ErrEmptyID},
		{"missing invocation id", func(m *RunMilestone) { m.InvocationID = nil }, ErrMilestoneDetailMismatch},
		{"zero recorded_at", func(m *RunMilestone) { m.RecordedAt = time.Time{} }, ErrMissingTimestamp},
		{"non-UTC recorded_at", func(m *RunMilestone) {
			m.RecordedAt = m.RecordedAt.In(time.FixedZone("PST", -8*3600))
		}, ErrTimestampNotUTC},
		{"terminal on submitted", func(m *RunMilestone) { m.Terminal = &terminal }, ErrMilestoneDetailMismatch},
		{"outcome on submitted", func(m *RunMilestone) { m.Outcome = &outcome }, ErrMilestoneDetailMismatch},
		{"reason on submitted", func(m *RunMilestone) { m.Reason = &reason }, ErrMilestoneDetailMismatch},
	}
	for _, tc := range mutations {
		m := validMilestone(MilestoneRunSubmitted)
		tc.mutate(&m)
		if err := m.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate() = %v, want %v", tc.name, err, tc.want)
		}
	}

	detail := []struct {
		name   string
		mutate func(*RunMilestone)
		want   error
	}{
		{"terminal without status", func(m *RunMilestone) { m.Terminal = nil }, ErrMilestoneDetailMismatch},
		{"terminal live status", func(m *RunMilestone) { running := ObservedStatusRunning; m.Terminal = &running }, ErrInvalidObservedStatus},
		{"terminal invalid status", func(m *RunMilestone) { bad := ObservedInvocationStatus("done"); m.Terminal = &bad }, ErrInvalidObservedStatus},
	}
	for _, tc := range detail {
		m := validMilestone(MilestoneTerminalRecorded)
		tc.mutate(&m)
		if err := m.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate() = %v, want %v", tc.name, err, tc.want)
		}
	}
	{
		m := validMilestone(MilestoneTerminalRecorded)
		gone := ObservedStatusGone
		m.Terminal = &gone
		if err := m.Validate(); err != nil {
			t.Errorf("terminal gone: Validate() = %v, want nil (a lost session is a recordable terminal class)", err)
		}
	}
	{
		m := validMilestone(MilestoneExecutionOutcomeRecorded)
		bad := ExecutionOutcomeStatus("crashed")
		m.Outcome = &bad
		if err := m.Validate(); !errors.Is(err, ErrInvalidExecOutcome) {
			t.Errorf("invalid outcome: Validate() = %v, want %v", err, ErrInvalidExecOutcome)
		}
	}
	{
		m := validMilestone(MilestonePublicationBlocked)
		bad := RunHoldReason("because")
		m.Reason = &bad
		if err := m.Validate(); !errors.Is(err, ErrInvalidRunHoldReason) {
			t.Errorf("invalid reason: Validate() = %v, want %v", err, ErrInvalidRunHoldReason)
		}
		m.Reason = nil
		if err := m.Validate(); !errors.Is(err, ErrMilestoneDetailMismatch) {
			t.Errorf("blocked without reason: Validate() = %v, want %v", err, ErrMilestoneDetailMismatch)
		}
	}
}

func TestInvocationObservationValidate(t *testing.T) {
	valid := InvocationObservation{
		InvocationID: "inv-1", RunID: "run-1",
		Status: ObservedStatusRunning, Live: true,
		ObservedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid observation rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*InvocationObservation)
		want   error
	}{
		{"missing invocation id", func(o *InvocationObservation) { o.InvocationID = "" }, ErrEmptyID},
		{"missing run id", func(o *InvocationObservation) { o.RunID = "" }, ErrEmptyID},
		{"invalid status", func(o *InvocationObservation) { o.Status = "paused" }, ErrInvalidObservedStatus},
		{"zero status", func(o *InvocationObservation) { o.Status = "" }, ErrInvalidObservedStatus},
		{"zero observed_at", func(o *InvocationObservation) { o.ObservedAt = time.Time{} }, ErrMissingTimestamp},
		{"non-UTC observed_at", func(o *InvocationObservation) {
			o.ObservedAt = o.ObservedAt.In(time.FixedZone("PST", -8*3600))
		}, ErrTimestampNotUTC},
		{"live concluded", func(o *InvocationObservation) {
			o.Status = ObservedStatusCompleted
			o.Live = true
		}, ErrObservationInconsistent},
		{"live gone", func(o *InvocationObservation) {
			o.Status = ObservedStatusGone
			o.Live = true
		}, ErrObservationInconsistent},
	}
	for _, tc := range cases {
		o := valid
		tc.mutate(&o)
		if err := o.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate() = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestRunHoldObservationValidate(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	inv := InvocationID("inv-1")
	valid := RunHoldObservation{
		RunID: "run-1", InvocationID: &inv, Reason: HoldOperationStopped,
		FirstObservedAt: ts, LastObservedAt: ts.Add(time.Minute),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid hold rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*RunHoldObservation)
		want   error
	}{
		{"missing run id", func(h *RunHoldObservation) { h.RunID = "" }, ErrEmptyID},
		{"empty invocation id", func(h *RunHoldObservation) { empty := InvocationID(""); h.InvocationID = &empty }, ErrEmptyID},
		{"invalid reason", func(h *RunHoldObservation) { h.Reason = "vibes" }, ErrInvalidRunHoldReason},
		{"zero reason", func(h *RunHoldObservation) { h.Reason = "" }, ErrInvalidRunHoldReason},
		{"zero first", func(h *RunHoldObservation) { h.FirstObservedAt = time.Time{} }, ErrMissingTimestamp},
		{"non-UTC last", func(h *RunHoldObservation) {
			h.LastObservedAt = h.LastObservedAt.In(time.FixedZone("PST", -8*3600))
		}, ErrTimestampNotUTC},
		{"backwards span", func(h *RunHoldObservation) {
			h.LastObservedAt = h.FirstObservedAt.Add(-time.Second)
		}, ErrTimestampOutOfOrder},
	}
	for _, tc := range cases {
		h := valid
		tc.mutate(&h)
		if err := h.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate() = %v, want %v", tc.name, err, tc.want)
		}
	}
	// A hold that concerns the whole run carries no invocation binding.
	unscoped := valid
	unscoped.InvocationID = nil
	if err := unscoped.Validate(); err != nil {
		t.Errorf("unscoped hold rejected: %v", err)
	}
}

func TestDefinitivePublicationBlockReason(t *testing.T) {
	t.Parallel()
	for reason, want := range map[string]RunHoldReason{
		PublicationBlockRecipeRevoked: HoldRecipeRevoked,
		PublicationBlockVerification:  HoldVerificationFindings,
		PublicationBlockTrust:         HoldTrustBlocked,
		PublicationBlockBaseAdvanced:  HoldBaseAdvanced,
	} {
		got, ok := DefinitivePublicationBlockReason(reason)
		if !ok || got != want {
			t.Errorf("DefinitivePublicationBlockReason(%q) = %q, %v; want %q, true",
				reason, got, ok, want)
		}
	}
	if got, ok := DefinitivePublicationBlockReason(
		"Publication is durably held while a transient retry remains possible.",
	); ok || got != "" {
		t.Errorf("transient reason = %q, %v; want empty, false", got, ok)
	}
}

func TestRunObservationValidate(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	valid := RunObservation{
		RunID:      "run-1",
		Milestones: []RunMilestone{validMilestone(MilestoneRunSubmitted)},
		Invocations: []InvocationObservation{{
			InvocationID: "inv-1", RunID: "run-1",
			Status: ObservedStatusPending, ObservedAt: ts,
		}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid aggregate rejected: %v", err)
	}
	{
		o := valid
		o.Milestones = []RunMilestone{validMilestone(MilestoneRunSubmitted)}
		o.Milestones[0].RunID = "run-2"
		if err := o.Validate(); !errors.Is(err, ErrParentKeyMismatch) {
			t.Errorf("foreign milestone: Validate() = %v, want %v", err, ErrParentKeyMismatch)
		}
	}
	{
		o := valid
		inv := InvocationID("inv-1")
		o.Hold = &RunHoldObservation{
			RunID: "run-2", InvocationID: &inv, Reason: HoldOperationStopped,
			FirstObservedAt: ts, LastObservedAt: ts,
		}
		if err := o.Validate(); !errors.Is(err, ErrParentKeyMismatch) {
			t.Errorf("foreign hold: Validate() = %v, want %v", err, ErrParentKeyMismatch)
		}
	}
	{
		o := valid
		o.Invocations = []InvocationObservation{{
			InvocationID: "inv-1", RunID: "run-2",
			Status: ObservedStatusPending, ObservedAt: ts,
		}}
		if err := o.Validate(); !errors.Is(err, ErrParentKeyMismatch) {
			t.Errorf("foreign observation: Validate() = %v, want %v", err, ErrParentKeyMismatch)
		}
	}
}

func TestDeriveInvocationLiveness(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	asOf := ts.Add(10 * time.Second)
	window := DefaultObservationFreshnessWindow
	obs := func(status ObservedInvocationStatus, live bool, at time.Time) *InvocationObservation {
		return &InvocationObservation{
			InvocationID: "inv-1", RunID: "run-1",
			Status: status, Live: live, ObservedAt: at,
		}
	}
	cases := []struct {
		name string
		obs  *InvocationObservation
		want InvocationLiveness
	}{
		{"never observed", nil, LivenessUnobserved},
		{"fresh live running", obs(ObservedStatusRunning, true, ts), LivenessLive},
		{"fresh idle pending", obs(ObservedStatusPending, false, ts), LivenessIdle},
		{"fresh gone", obs(ObservedStatusGone, false, ts), LivenessIdle},
		{"stale running", obs(ObservedStatusRunning, true, asOf.Add(-window-time.Second)), LivenessGap},
		{"stale pending", obs(ObservedStatusPending, false, asOf.Add(-window-time.Second)), LivenessGap},
		// An observation dated after asOf means the clocks disagree (a
		// step-back, a restore); "currently observed" cannot stand on it.
		{"future live running", obs(ObservedStatusRunning, true, asOf.Add(time.Minute)), LivenessGap},
		{"future pending", obs(ObservedStatusPending, false, asOf.Add(time.Hour)), LivenessGap},
		{"completed", obs(ObservedStatusCompleted, false, ts), LivenessTerminal},
		{"stale failed stays terminal", obs(ObservedStatusFailed, false, asOf.Add(-time.Hour)), LivenessTerminal},
	}
	for _, tc := range cases {
		if got := DeriveInvocationLiveness(tc.obs, asOf, window); got != tc.want {
			t.Errorf("%s: DeriveInvocationLiveness = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestRunObservationDerivations(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	inv := InvocationID("inv-1")
	submitted := RunMilestone{
		RunID: "run-1", Kind: MilestoneRunSubmitted, InvocationID: &inv, RecordedAt: ts,
	}
	started := RunMilestone{
		RunID: "run-1", Kind: MilestoneInvocationStarted, InvocationID: &inv,
		RecordedAt: ts.Add(time.Minute),
	}
	terminalStatus := ObservedStatusFailed
	terminal := RunMilestone{
		RunID: "run-1", Kind: MilestoneTerminalRecorded, InvocationID: &inv,
		Terminal: &terminalStatus, RecordedAt: ts.Add(10 * time.Minute),
	}

	open := RunObservation{RunID: "run-1", Milestones: []RunMilestone{submitted, started}}
	asOf := ts.Add(5 * time.Minute)
	if elapsed, ok := open.Elapsed(asOf); !ok || elapsed != 5*time.Minute {
		t.Errorf("open Elapsed = %v, %v; want 5m, true", elapsed, ok)
	}
	if last, ok := open.LastObservedAt(); !ok || !last.Equal(started.RecordedAt) {
		t.Errorf("open LastObservedAt = %v, %v; want %v, true", last, ok, started.RecordedAt)
	}

	concluded := RunObservation{RunID: "run-1", Milestones: []RunMilestone{submitted, started, terminal}}
	// Elapsed freezes at conclusion: a later asOf must not keep growing it.
	if elapsed, ok := concluded.Elapsed(ts.Add(time.Hour)); !ok || elapsed != 10*time.Minute {
		t.Errorf("concluded Elapsed = %v, %v; want 10m, true", elapsed, ok)
	}
	if at, ok := concluded.ConcludedAt(); !ok || !at.Equal(terminal.RecordedAt) {
		t.Errorf("ConcludedAt = %v, %v; want %v, true", at, ok, terminal.RecordedAt)
	}

	// Backwards endpoints (a clock stepped back between the instants) are
	// not an elapsed clock: both the not-yet-concluded and the concluded
	// shape refuse rather than reporting a negative span.
	if _, ok := open.Elapsed(ts.Add(-time.Minute)); ok {
		t.Error("Elapsed reported ok with asOf before submission")
	}
	rolledBack := RunObservation{RunID: "run-1", Milestones: []RunMilestone{
		submitted, {
			RunID: "run-1", Kind: MilestoneTerminalRecorded, InvocationID: &inv,
			Terminal: &terminalStatus, RecordedAt: ts.Add(-time.Minute),
		},
	}}
	if _, ok := rolledBack.Elapsed(asOf); ok {
		t.Error("Elapsed reported ok with a conclusion before submission")
	}

	empty := RunObservation{RunID: "run-1"}
	if _, ok := empty.Elapsed(asOf); ok {
		t.Error("empty Elapsed reported ok before submission was observed")
	}
	if _, ok := empty.SubmittedAt(); ok {
		t.Error("empty SubmittedAt reported ok")
	}
	if _, ok := empty.LastObservedAt(); ok {
		t.Error("empty LastObservedAt reported ok")
	}

	// An invocation observation newer than every milestone is the last
	// observation; the hold's span participates too.
	withObs := open
	withObs.Invocations = []InvocationObservation{{
		InvocationID: "inv-1", RunID: "run-1",
		Status: ObservedStatusRunning, Live: true, ObservedAt: ts.Add(4 * time.Minute),
	}}
	if last, ok := withObs.LastObservedAt(); !ok || !last.Equal(ts.Add(4*time.Minute)) {
		t.Errorf("LastObservedAt with observation = %v, %v; want %v, true", last, ok, ts.Add(4*time.Minute))
	}
}

// TestObservationModelHasNoCompletionFraction pins the issue-#394 acceptance
// that no percentage-complete field exists in the model: the serialized
// contract shapes carry no key that reads as a completion fraction. Guarding
// the wire shape (not field names in Go) catches a rename that keeps the
// JSON key as well as a new field.
func TestObservationModelHasNoCompletionFraction(t *testing.T) {
	inv := InvocationID("inv-1")
	reason := HoldTrustBlocked
	values := []any{
		validMilestone(MilestoneRunSubmitted),
		InvocationObservation{
			InvocationID: "inv-1", RunID: "run-1",
			Status: ObservedStatusRunning, Live: true,
			ObservedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		RunHoldObservation{
			RunID: "run-1", InvocationID: &inv, Reason: reason,
			FirstObservedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			LastObservedAt:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		RunObservation{RunID: "run-1"},
	}
	for _, v := range values {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %T: %v", v, err)
		}
		lowered := strings.ToLower(string(encoded))
		for _, banned := range []string{"percent", "progress", "fraction", "eta"} {
			if strings.Contains(lowered, banned) {
				t.Errorf("%T serializes a completion-fraction key (%q): %s", v, banned, encoded)
			}
		}
	}
}
