package signet

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// TestRunTimelineHoldAndClockGoldens pins the hold history and clocks the
// run timeline serves (#3236) for a run never held, a run held twice, a run
// held now, and a finished run. Every instant is fixed, and each closed hold
// ends at a milestone or at the next cause, as the store ends them.
func TestRunTimelineHoldAndClockGoldens(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }
	runID := domain.RunID("run-3236")
	submit := domain.InvocationID("inv-submit")
	work := domain.InvocationID("inv-implement")
	publish := domain.ProductionPublicationInvocationID(runID)
	milestone := func(kind domain.RunMilestoneKind, invocation domain.InvocationID, minutes int) domain.RunMilestone {
		return domain.RunMilestone{RunID: runID, Kind: kind, InvocationID: &invocation, RecordedAt: at(minutes)}
	}
	span := func(reason domain.RunHoldReason, invocation *domain.InvocationID, first, last int, cleared *int) domain.RunHoldSpan {
		s := domain.RunHoldSpan{
			RunID: runID, InvocationID: invocation, Reason: reason,
			FirstObservedAt: at(first), LastObservedAt: at(last),
		}
		if cleared != nil {
			s.ClearedAt = new(at(*cleared))
		}
		return s
	}
	submitted := milestone(domain.MilestoneRunSubmitted, submit, 0)
	started := milestone(domain.MilestoneInvocationStarted, work, 10)
	exported := milestone(domain.MilestoneExecutionExportRecorded, work, 40)

	// Held for machine capacity before the invocation started, then stopped
	// by the operator while the started invocation was still working; the
	// export cleared the stop. The active time still counts the overlap.
	heldTwice := []domain.RunHoldSpan{
		span(domain.HoldMachineCapacity, nil, 2, 8, new(10)),
		span(domain.HoldOperationStopped, &work, 20, 30, new(40)),
	}
	// A run held now: its history ends with the open span, which is the
	// current hold's cause and instants.
	currentHold := &domain.RunHoldObservation{
		RunID: runID, InvocationID: &work, Reason: domain.HoldAttendedModeActive,
		FirstObservedAt: at(45), LastObservedAt: at(50),
	}
	heldNow := []domain.RunHoldSpan{
		span(domain.HoldMachineCapacity, nil, 2, 8, new(10)),
		span(currentHold.Reason, currentHold.InvocationID, 45, 50, nil),
	}
	// A run stopped while held: it finished at its newest milestone, and the
	// hold that outlasted it extends past the clocks' end.
	finishedMilestones := []domain.RunMilestone{
		submitted, started, exported,
		milestone(domain.MilestonePublicationReady, publish, 50),
	}
	finishedHolds := []domain.RunHoldSpan{span(domain.HoldOperationStopped, &work, 52, 56, new(58))}

	cases := []struct {
		name        string
		milestones  []domain.RunMilestone
		hold        *domain.RunHoldObservation
		holdHistory []domain.RunHoldSpan
		finished    bool
	}{
		{name: "run-timeline-never-held", milestones: []domain.RunMilestone{submitted, started, exported}},
		{
			name: "run-timeline-held-twice", milestones: []domain.RunMilestone{submitted, started, exported},
			holdHistory: heldTwice,
		},
		{
			name: "run-timeline-held-now", milestones: []domain.RunMilestone{submitted, started, exported},
			hold: currentHold, holdHistory: heldNow,
		},
		{
			name: "run-timeline-finished", milestones: finishedMilestones,
			holdHistory: finishedHolds, finished: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			observation := domain.RunObservation{
				RunID: runID, Milestones: tc.milestones, Hold: tc.hold,
				Invocations: []domain.InvocationObservation{},
			}
			if err := observation.Validate(); err != nil {
				t.Fatalf("fixture observation: %v", err)
			}
			for _, s := range tc.holdHistory {
				if err := s.Validate(); err != nil {
					t.Fatalf("fixture span: %v", err)
				}
			}
			facts := runProjectionFacts{holdHistory: tc.holdHistory, finished: tc.finished}
			timeline := runTimeline(observation, 7, at(60), facts, RunOutcomes{
				Posts: []RunOutcomePost{}, Findings: []RunOutcomeFinding{}, Complete: true,
			})
			got, err := json.MarshalIndent(timeline, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, tc.name, append(got, '\n'))
		})
	}
}
