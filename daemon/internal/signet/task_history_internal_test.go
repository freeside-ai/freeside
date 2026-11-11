package signet

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestTaskHistoryIncludesRecordedWorkflowAndStableSourceBindings(t *testing.T) {
	t.Parallel()
	task, inputs := taskTimelineFixtureInputs()
	at := task.CreatedAt
	run := inputs[1].run.ID
	task.LifecycleFacts = []domain.TaskLifecycleFact{{Kind: domain.TaskLifecycleStarted, RunID: run, RecordedAt: at.Add(time.Minute)}}
	requested, completed := at.Add(130*time.Minute), at.Add(140*time.Minute)
	outcome := domain.ReviewClean
	inputs[1].facts.review = &RunReviewFacts{Rounds: []RunReviewRound{{Round: 1, InvocationID: "review-1", HeadSHA: "review-head", BaseSHA: "review-base", RequestedAt: &requested, CompletedAt: &completed, Outcome: &outcome}}}
	inputs[1].readiness = []domain.AttentionItem{{ID: "ready-1", CreatedAt: &completed, Readiness: &domain.ReadinessSummary{Class: domain.ReadinessReadyClean}, ReadinessDetail: &domain.ReadinessDetail{CandidateHead: "ready-head", Base: domain.ReadinessBoundBase{BaseSHA: "ready-base"}}}}
	got, err := taskTimeline(task, nil, nil, inputs, 42, at.Add(8*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[domain.TaskEventKind]int{}
	for i, e := range got.Events {
		counts[e.Kind]++
		if i > 0 && e.RecordedAt.After(got.Events[i-1].RecordedAt) {
			t.Fatal("not newest first")
		}
		if e.Review != nil && (*e.RunID != run || e.Review.InvocationID != "review-1" || e.Review.HeadSHA != "review-head" || e.Review.BaseSHA != "review-base") {
			t.Fatalf("review binding: %+v", e)
		}
		if e.Verification != nil && (e.Verification.ItemID != "ready-1" || e.Verification.HeadSHA != "ready-head") {
			t.Fatalf("verification binding: %+v", e)
		}
	}
	for kind, want := range map[domain.TaskEventKind]int{domain.TaskEventCreated: 1, domain.TaskEventStarted: 1, domain.TaskEventCampaignAllocated: 2, domain.TaskEventSpecificationApproved: 2, domain.TaskEventReviewRequested: 1, domain.TaskEventReviewCompleted: 1, domain.TaskEventVerificationRecorded: 1, domain.TaskEventPROpened: 1, domain.TaskEventPRMerged: 1} {
		if counts[kind] != want {
			t.Errorf("%s: got %d want %d", kind, counts[kind], want)
		}
	}
	again, err := taskTimeline(task, nil, nil, inputs, 42, at.Add(8*time.Hour))
	if err != nil || !reflect.DeepEqual(got.Events, again.Events) {
		t.Fatal("read changed recorded events", err)
	}
	// Source enumeration changes cannot reorder coincident workflow events.
	reversed := slices.Clone(inputs)
	slices.Reverse(reversed)
	events, err := taskHistoryEvents(task, nil, nil, reversed, got.Sections)
	if err != nil || !reflect.DeepEqual(events, got.Events) {
		t.Fatal("source order changed history", err)
	}
	inputs[1].readiness[0].CreatedAt = nil
	legacy, err := taskTimeline(task, nil, nil, inputs, 42, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range legacy.Events {
		if e.Kind == domain.TaskEventVerificationRecorded {
			t.Fatal("invented legacy verification timestamp")
		}
	}
}

func TestTaskHistoryLifecycleAndQueuedStopping(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for _, state := range domain.AllTaskCancellationStates {
		t.Run(string(state), func(t *testing.T) {
			task := domain.Task{CreatedAt: at, Cancellation: &domain.TaskCancellation{RequestedAt: at.Add(time.Minute), State: state}}
			want := domain.TaskEventStopRequested
			switch state {
			case domain.TaskCancellationRequested:
			case domain.TaskCancellationConfirmed:
				want = domain.TaskEventStopped
			case domain.TaskCancellationFailed:
				want = domain.TaskEventStopFailed
			}
			if state != domain.TaskCancellationRequested {
				task.Cancellation.Acknowledgement = &domain.TaskCancellationAcknowledgement{State: state, RecordedAt: at.Add(2 * time.Minute)}
			}
			events, err := taskHistoryEvents(task, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if events[0].Kind != want || events[0].RunID != nil {
				t.Fatalf("queued stop: %+v", events)
			}
			for _, e := range events {
				if state != domain.TaskCancellationConfirmed && e.Kind == domain.TaskEventStopped {
					t.Fatal("request or failure claimed stopping")
				}
			}
		})
	}
	task := domain.Task{CreatedAt: at, LifecycleFacts: []domain.TaskLifecycleFact{{Kind: domain.TaskLifecycleStarted, RunID: "old", RecordedAt: at.Add(time.Minute)}, {Kind: domain.TaskLifecycleCompleted, RunID: "old", RecordedAt: at.Add(2 * time.Minute)}, {Kind: domain.TaskLifecycleStarted, RunID: "new", RecordedAt: at.Add(3 * time.Minute)}, {Kind: domain.TaskLifecycleAbandoned, RunID: "new", RecordedAt: at.Add(4 * time.Minute)}}}
	events, err := taskHistoryEvents(task, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 || events[0].Kind != domain.TaskEventAbandoned || *events[0].RunID != "new" || events[2].Kind != domain.TaskEventCompleted || *events[2].RunID != "old" {
		t.Fatalf("episode history: %+v", events)
	}
}

// TestTaskHistoryOrdersTaskStartBelowItsSubmission: a submission records the
// task start at its own instant (#3039), and the same-instant tie-break puts
// the start below it, so the history reads created, started, submitted.
func TestTaskHistoryOrdersTaskStartBelowItsSubmission(t *testing.T) {
	t.Parallel()
	task, inputs := taskTimelineFixtureInputs()
	specification := inputs[0]
	submittedAt := specification.observation.Milestones[0].RecordedAt
	task.LifecycleFacts = []domain.TaskLifecycleFact{{Kind: domain.TaskLifecycleStarted, RunID: specification.run.ID, RecordedAt: submittedAt}}
	events, err := taskHistoryEvents(task, nil, nil, []taskTimelineInput{specification}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]domain.TaskEventKind, 0, len(events))
	for _, e := range events {
		got = append(got, e.Kind)
	}
	if want := []domain.TaskEventKind{domain.TaskEventRunMilestone, domain.TaskEventStarted, domain.TaskEventCreated}; !slices.Equal(got, want) {
		t.Fatalf("newest first = %v, want %v", got, want)
	}
}

// TestTaskHistoryOrdersTiedRouterLinesByRole: the router sets every line at
// one instant, so tied lines read in workflow role order, never by line ID,
// whatever order the store returns them in.
func TestTaskHistoryOrdersTiedRouterLinesByRole(t *testing.T) {
	t.Parallel()
	task, _ := taskTimelineFixtureInputs()
	routed := taskTimelineFixtureLines(t, task)[0]
	// Each ID sorts in a different place than its role, so a line-ID
	// tie-break would read implementer, specifier, remediator.
	var lines []domain.TaskLine
	for _, line := range []struct {
		role domain.RoleName
		id   string
	}{
		{domain.RoleSpecifier, "b"},
		{domain.RoleImplementer, "a"},
		{domain.RoleRemediator, "c"},
	} {
		next := routed
		next.Role = line.role
		next.ID = domain.Digest("sha256:" + strings.Repeat(line.id, 64))
		lines = append(lines, next)
	}
	want := []domain.RoleName{domain.RoleRemediator, domain.RoleImplementer, domain.RoleSpecifier}
	for _, order := range []string{"store", "reversed"} {
		input := slices.Clone(lines)
		if order == "reversed" {
			slices.Reverse(input)
		}
		events, err := taskHistoryEvents(task, input, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var got []domain.RoleName
		for _, e := range events {
			if e.Line != nil {
				got = append(got, e.Line.Role)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s order: newest first = %v, want %v", order, got, want)
		}
	}
}
