package observe

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func untilMilestone(kind domain.RunMilestoneKind, invocation domain.InvocationID) domain.RunMilestone {
	return domain.RunMilestone{RunID: followRun, Kind: kind, InvocationID: &invocation, RecordedAt: followAt(1)}
}

func mustParseUntil(t *testing.T, text string) Until {
	t.Helper()
	until, err := ParseUntil(text)
	if err != nil {
		t.Fatalf("ParseUntil(%q): %v", text, err)
	}
	if until.String() != text {
		t.Fatalf("ParseUntil(%q).String() = %q", text, until.String())
	}
	return until
}

// TestUntilConditionsHoldOnlyOnTheirCheckpoint pins each condition against a
// snapshot that holds it and the near misses that must not: the stage
// conditions read the run's stored engine spelling ("implement") through the
// canonical resolver, and a milestone on an invocation no admission names
// belongs to no stage.
func TestUntilConditionsHoldOnlyOnTheirCheckpoint(t *testing.T) {
	t.Parallel()
	const (
		spec domain.InvocationID = "inv-spec"
		impl domain.InvocationID = "inv-impl"
	)
	admissions := []observedb.Admission{
		{InvocationID: spec, Stage: string(domain.StageNameSpecification)},
		{InvocationID: impl, Stage: "implement"},
	}
	snapshot := func(milestones []domain.RunMilestone, items ...observedb.AttentionItem) observedb.Snapshot {
		return observedb.Snapshot{
			Observation:    domain.RunObservation{RunID: followRun, Milestones: milestones},
			Admissions:     admissions,
			AttentionItems: items,
		}
	}
	decision := []domain.Action{domain.ActionApprove}
	for _, tc := range []struct {
		name     string
		until    string
		snapshot observedb.Snapshot
		want     bool
	}{
		{
			name: "card requesting a decision", until: "card=spec_approval", want: true,
			snapshot: snapshot(nil, observedb.AttentionItem{Type: domain.AttentionSpecApproval, RequestedDecision: decision}),
		},
		{
			name: "card of another type", until: "card=spec_approval",
			snapshot: snapshot(nil, observedb.AttentionItem{Type: domain.AttentionBlocked, RequestedDecision: decision}),
		},
		{
			name: "card requesting nothing", until: "card=spec_approval",
			snapshot: snapshot(nil, observedb.AttentionItem{Type: domain.AttentionSpecApproval}),
		},
		{
			name: "implementation started", until: "stage-started=implementation", want: true,
			snapshot: snapshot([]domain.RunMilestone{untilMilestone(domain.MilestoneInvocationStarted, impl)}),
		},
		{
			name: "only specification started", until: "stage-started=implementation",
			snapshot: snapshot([]domain.RunMilestone{untilMilestone(domain.MilestoneInvocationStarted, spec)}),
		},
		{
			name: "started on an unadmitted invocation", until: "stage-started=specification",
			snapshot: snapshot([]domain.RunMilestone{untilMilestone(domain.MilestoneInvocationStarted, "inv-other")}),
		},
		{
			name: "implementation admitted but not started", until: "stage-started=implementation",
			snapshot: snapshot([]domain.RunMilestone{untilMilestone(domain.MilestoneInvocationAdmitted, impl)}),
		},
		{
			name: "implementation finished", until: "stage-finished=implementation", want: true,
			snapshot: snapshot([]domain.RunMilestone{
				untilMilestone(domain.MilestoneInvocationStarted, impl),
				untilMilestone(domain.MilestoneTerminalRecorded, impl),
			}),
		},
		{
			name: "implementation started not finished", until: "stage-finished=implementation",
			snapshot: snapshot([]domain.RunMilestone{untilMilestone(domain.MilestoneInvocationStarted, impl)}),
		},
		{
			name: "only specification finished", until: "stage-finished=implementation",
			snapshot: snapshot([]domain.RunMilestone{untilMilestone(domain.MilestoneTerminalRecorded, spec)}),
		},
		{
			name: "park ended by an admission", until: "park-ended", want: true,
			snapshot: snapshot([]domain.RunMilestone{untilMilestone(domain.MilestoneInvocationAdmitted, impl)}),
		},
		{
			name: "still parked", until: "park-ended",
			snapshot: snapshot([]domain.RunMilestone{
				{RunID: followRun, Kind: domain.MilestoneRunSubmitted, RecordedAt: followAt(0)},
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mustParseUntil(t, tc.until).Holds(tc.snapshot); got != tc.want {
				t.Fatalf("%s holds = %v, want %v", tc.until, got, tc.want)
			}
		})
	}
}

func TestParseUntilRefusesConditionsNoSnapshotCanAnswer(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"",
		"card",
		"card=",
		"card=not_a_type",
		// The snapshot leaves ready items out, so this could only time out.
		"card=ready_for_final_review",
		"stage-started=",
		"stage-started=implement",
		// Neither records an execution admission or invocation milestone.
		"stage-started=review",
		"stage-finished=verification",
		// A specification run's terminal records no terminal_recorded
		// milestone; its spec_approval card is the checkpoint.
		"stage-finished=specification",
		"park-ended=now",
		"outcome=published",
	} {
		if _, err := ParseUntil(text); !errors.Is(err, ErrUsage) {
			t.Errorf("ParseUntil(%q) err = %v, want %v", text, err, ErrUsage)
		}
	}
}

// scriptedSnapshots returns its snapshots in order, repeating the last, and
// counts reads.
type scriptedSnapshots struct {
	snapshots []observedb.Snapshot
	err       error
	reads     int
}

func (s *scriptedSnapshots) ObserveSnapshot(context.Context, domain.RunID) (observedb.Snapshot, error) {
	s.reads++
	if s.err != nil {
		return observedb.Snapshot{}, s.err
	}
	return s.snapshots[min(s.reads, len(s.snapshots))-1], nil
}

func parkedSnapshot(milestones ...domain.RunMilestone) observedb.Snapshot {
	return observedb.Snapshot{Observation: domain.RunObservation{
		RunID: followRun,
		Milestones: append([]domain.RunMilestone{
			{RunID: followRun, Kind: domain.MilestoneRunSubmitted, RecordedAt: followAt(0)},
		}, milestones...),
	}}
}

func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return lines[len(lines)-1]
}

func TestWaitEndsWhenTheConditionHolds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		source := &scriptedSnapshots{snapshots: []observedb.Snapshot{
			parkedSnapshot(),
			parkedSnapshot(),
			parkedSnapshot(untilMilestone(domain.MilestoneInvocationAdmitted, followInvocation)),
		}}
		var out bytes.Buffer
		err := Wait(context.Background(), source, &out, WaitConfig{
			RunID: followRun, Until: mustParseUntil(t, "park-ended"), Timeout: time.Minute, Interval: time.Second,
		})
		if err != nil || ExitCode(err) != 0 {
			t.Fatalf("Wait = %v, want met", err)
		}
		if source.reads != 3 {
			t.Errorf("reads = %d, want 3", source.reads)
		}
		output := out.String()
		if got := lastLine(output); got != "until  park-ended  met" {
			t.Errorf("last line = %q", got)
		}
		// The timeline lines precede the until line, each milestone once.
		assertFollowContains(t, output, "run "+string(followRun), "run_submitted", "invocation_admitted")
		if strings.Count(output, "run_submitted") != 1 {
			t.Errorf("milestone reprinted:\n%s", output)
		}
	})
}

func TestWaitTimesOut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		source := &scriptedSnapshots{snapshots: []observedb.Snapshot{parkedSnapshot()}}
		var out bytes.Buffer
		started := time.Now()
		err := Wait(context.Background(), source, &out, WaitConfig{
			RunID: followRun, Until: mustParseUntil(t, "park-ended"),
			Timeout: 10 * time.Second, Interval: 3 * time.Second,
		})
		if !errors.Is(err, ErrUntilTimeout) || ExitCode(err) != 1 {
			t.Fatalf("Wait = %v (exit %d), want %v and exit 1", err, ExitCode(err), ErrUntilTimeout)
		}
		if elapsed := time.Since(started); elapsed != 10*time.Second {
			t.Errorf("waited %s, want the 10s timeout", elapsed)
		}
		// Reads at 0, 3, 6, and 9 seconds; the deadline lands before the
		// fifth.
		if source.reads != 4 {
			t.Errorf("reads = %d, want 4", source.reads)
		}
		if got := lastLine(out.String()); got != "until  park-ended  timed out" {
			t.Errorf("last line = %q", got)
		}
	})
}

// hungSnapshots answers no read until its context ends, as a stuck control
// socket would.
type hungSnapshots struct{}

func (hungSnapshots) ObserveSnapshot(ctx context.Context, _ domain.RunID) (observedb.Snapshot, error) {
	<-ctx.Done()
	return observedb.Snapshot{}, ctx.Err()
}

// TestWaitTimesOutDuringAHungRead holds the timeout to its promise that a
// wait always ends: a read in flight at the deadline ends with it.
func TestWaitTimesOutDuringAHungRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var out bytes.Buffer
		started := time.Now()
		err := Wait(context.Background(), hungSnapshots{}, &out, WaitConfig{
			RunID: followRun, Until: mustParseUntil(t, "park-ended"), Timeout: 10 * time.Second,
		})
		if !errors.Is(err, ErrUntilTimeout) || ExitCode(err) != 1 {
			t.Fatalf("Wait = %v (exit %d), want %v and exit 1", err, ExitCode(err), ErrUntilTimeout)
		}
		if elapsed := time.Since(started); elapsed != 10*time.Second {
			t.Errorf("waited %s, want the 10s timeout", elapsed)
		}
		if got := lastLine(out.String()); got != "until  park-ended  timed out" {
			t.Errorf("last line = %q", got)
		}
	})
}

func TestWaitInterruptIsNotAMetCondition(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		source := &scriptedSnapshots{snapshots: []observedb.Snapshot{parkedSnapshot()}}
		var out bytes.Buffer
		go func() {
			time.Sleep(5 * time.Second)
			cancel()
		}()
		err := Wait(ctx, source, &out, WaitConfig{
			RunID: followRun, Until: mustParseUntil(t, "park-ended"), Timeout: time.Minute, Interval: time.Second,
		})
		if !errors.Is(err, context.Canceled) || ExitCode(err) != 1 {
			t.Fatalf("Wait = %v (exit %d), want canceled and exit 1", err, ExitCode(err))
		}
		if got := lastLine(out.String()); got != "until  park-ended  interrupted" {
			t.Errorf("last line = %q", got)
		}
	})
}

func TestWaitFailsOnAReadFailure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		readErr := errors.New("snapshot unavailable")
		var out bytes.Buffer
		err := Wait(context.Background(), &scriptedSnapshots{err: readErr}, &out, WaitConfig{
			RunID: followRun, Until: mustParseUntil(t, "park-ended"), Timeout: time.Minute,
		})
		if !errors.Is(err, readErr) || ExitCode(err) != 1 {
			t.Fatalf("Wait = %v (exit %d), want the read failure and exit 1", err, ExitCode(err))
		}
	})
}

// TestFollowCommandWaitsOnTheStoredImplementationStage runs -until against a
// real store, whose production run names its stage "implement": the wait
// has to resolve that spelling to answer stage-started=implementation.
func TestFollowCommandWaitsOnTheStoredImplementationStage(t *testing.T) {
	t.Parallel()
	st, path, admission, _ := openSeededFollowStore(t)
	ctx := context.Background()
	var stageName string
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, admission.RunID)
		if err != nil {
			return err
		}
		for _, stage := range run.Stages {
			if stage.ID == admission.StageID {
				stageName = stage.Name
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if canonical, err := domain.CanonicalStageRole(stageName); err != nil ||
		canonical != domain.StageNameImplementation {
		t.Fatalf("seeded stage %q resolves to %q, %v; want implementation", stageName, canonical, err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: admission.RunID, Kind: domain.MilestoneInvocationStarted,
			InvocationID: &admission.InvocationID, RecordedAt: admission.AdmittedAt.Add(time.Second),
		})
	}); err != nil {
		t.Fatalf("append invocation_started: %v", err)
	}

	output, err := runFollowCLI(t, "-db", path, "-run", string(admission.RunID),
		"-until", "stage-started=implementation", "-timeout", "1m")
	if err != nil {
		t.Fatalf("follow -until: %v\n%s", err, output)
	}
	if got := lastLine(output); got != "until  stage-started=implementation  met" {
		t.Fatalf("last line = %q\n%s", got, output)
	}
	assertFollowContains(t, output, "invocation_started  invocation="+string(admission.InvocationID))

	// -until takes -approved-recipe, as -snapshot does.
	output, err = runFollowCLI(t, "-db", path, "-run", string(admission.RunID),
		"-until", "stage-started=implementation", "-timeout", "1m",
		"-approved-recipe", "sha256:"+strings.Repeat("0", 64))
	if err != nil || lastLine(output) != "until  stage-started=implementation  met" {
		t.Fatalf("follow -until -approved-recipe: %v\n%s", err, output)
	}
}
