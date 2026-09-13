package signet

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func taskTimelineFixture(t *testing.T) TaskTimeline {
	t.Helper()
	task, inputs := taskTimelineFixtureInputs()
	if err := task.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, input := range inputs {
		if err := input.run.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := input.observation.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	timeline, err := taskTimeline(task, inputs, 42, task.CreatedAt.Add(8*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return timeline
}

func taskTimelineFixtureInputs() (domain.Task, []taskTimelineInput) {
	at := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	task := domain.Task{
		ID: "task-1", ProjectID: "project-1", CreatedAt: at,
		Name:        domain.DisplayName{Text: "Improve navigation", Source: domain.DisplayNameSourceSpecification},
		CampaignIDs: []domain.CampaignID{"campaign-1", "campaign-2"},
	}
	inputs := []taskTimelineInput{}
	for _, seed := range []struct {
		run      domain.RunID
		campaign domain.CampaignID
		spec     domain.RunID
		impl     domain.RunID
		digest   domain.Digest
		number   int
		hour     int
	}{
		{"run-spec-1", "campaign-1", "run-spec-1", "run-impl-1", "sha256:spec-1", 1, 1},
		{"run-impl-1", "campaign-1", "run-spec-1", "run-impl-1", "sha256:spec-1", 1, 2},
		{"run-retry-1", "campaign-1", "run-spec-1", "run-retry-1", "sha256:spec-1", 2, 3},
		{"run-spec-2", "campaign-2", "run-spec-2", "run-impl-2", "sha256:spec-2", 1, 5},
		{"run-impl-2", "campaign-2", "run-spec-2", "run-impl-2", "sha256:spec-2", 1, 6},
	} {
		inv := domain.InvocationID("inv-" + seed.run)
		input := taskTimelineInput{
			run: domain.Run{
				ID: seed.run, TaskID: task.ID, ProjectID: task.ProjectID,
				CampaignID: seed.campaign, AttemptNumber: seed.number,
				SpecDigest: seed.digest, PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
			},
			attempt: &domain.ProductionAttempt{
				CampaignID: seed.campaign, AttemptNumber: seed.number,
				SpecificationRunID: seed.spec, ImplementationRunID: seed.impl, ApprovedSpecDigest: seed.digest,
			},
			observation: domain.RunObservation{
				RunID: seed.run, Milestones: []domain.RunMilestone{{
					RunID: seed.run, Kind: domain.MilestoneRunSubmitted, InvocationID: &inv,
					RecordedAt: at.Add(time.Duration(seed.hour) * time.Hour),
				}},
			},
		}
		if seed.run == seed.spec {
			input.facts.supersededBy = &seed.impl
		}
		if seed.run == "run-impl-1" {
			retryID := domain.RunID("run-retry-1")
			input.facts.supersededBy = &retryID
		}
		if seed.number == 2 {
			input.run.ParentRunID = "run-impl-1"
			input.run.AttemptReason = "Retry after the provider outage"
			input.prBinding = &domain.WorkUnitPRBinding{PRNumber: 73, RecordedAt: at.Add(210 * time.Minute)}
			input.facts.completion = &WorkUnitCompletionFacts{
				PRNumber: 73, MergeCommitSHA: strings.Repeat("a", 40), RecordedAt: at.Add(4 * time.Hour),
			}
			input.observation.Milestones = append(input.observation.Milestones,
				domain.RunMilestone{RunID: seed.run, Kind: domain.MilestonePublicationReady, InvocationID: &inv, RecordedAt: input.prBinding.RecordedAt},
				domain.RunMilestone{RunID: seed.run, Kind: domain.MilestoneWorkUnitCompleted, InvocationID: &inv, RecordedAt: input.facts.completion.RecordedAt})
		}
		inputs = append(inputs, input)
	}
	return task, inputs
}

func TestTaskTimelineOrdersHistoryAndEmitsApprovalOnce(t *testing.T) {
	timeline := taskTimelineFixture(t)
	if len(timeline.Sections) != 2 || *timeline.Sections[0].CampaignID != "campaign-2" {
		t.Fatalf("campaign order = %+v", timeline.Sections)
	}
	first := timeline.Sections[1]
	if len(first.Events) != 2 || first.Events[0].Kind != domain.TaskEventSpecificationApproved || first.Events[1].Kind != domain.TaskEventCampaignAllocated {
		t.Fatalf("campaign events = %+v", first.Events)
	}
	retry := first.Runs[0]
	if retry.RunID != "run-retry-1" || *retry.AttemptNumber != 2 || *retry.ParentRunID != "run-impl-1" || *retry.Role != domain.TaskRunImplementation {
		t.Fatalf("retry header = %+v", retry)
	}
	if len(retry.Events) != 2 || retry.Events[0].Kind != domain.TaskEventPRMerged || retry.Events[1].Kind != domain.TaskEventPROpened {
		t.Fatalf("PR events = %+v", retry.Events)
	}
	if retry.Milestones[0].Kind != domain.MilestoneWorkUnitCompleted || retry.Milestones[2].Kind != domain.MilestoneRunSubmitted {
		t.Fatalf("milestone order = %+v", retry.Milestones)
	}
	if *first.Runs[1].SupersededBy != retry.RunID || *first.Runs[2].SupersededBy != first.Runs[1].RunID {
		t.Fatalf("supersession = %+v", first.Runs)
	}
}

func TestTaskTimelineRejectsContradictoryMembershipAndMissingAllocation(t *testing.T) {
	for name, change := range map[string]func(*domain.Task, []taskTimelineInput){
		"foreign task":       func(_ *domain.Task, inputs []taskTimelineInput) { inputs[0].run.TaskID = "other-task" },
		"foreign project":    func(_ *domain.Task, inputs []taskTimelineInput) { inputs[0].run.ProjectID = "other-project" },
		"missing submission": func(_ *domain.Task, inputs []taskTimelineInput) { inputs[0].observation.Milestones = nil },
		"missing campaign":   func(task *domain.Task, _ []taskTimelineInput) { task.CampaignIDs = task.CampaignIDs[:1] },
		"unrecorded campaign": func(task *domain.Task, _ []taskTimelineInput) {
			task.CampaignIDs = append(task.CampaignIDs, "campaign-3")
		},
	} {
		t.Run(name, func(t *testing.T) {
			task, inputs := taskTimelineFixtureInputs()
			change(&task, inputs)
			if _, err := taskTimeline(task, inputs, 42, task.CreatedAt); !errors.Is(err, ErrRunObservationIntegrity) {
				t.Fatalf("error = %v, want ErrRunObservationIntegrity", err)
			}
		})
	}
}

func TestTaskTimelineLegacyAndEmptyHistory(t *testing.T) {
	task, _ := taskTimelineFixtureInputs()
	task.CampaignIDs = []domain.CampaignID{}
	empty, err := taskTimeline(task, nil, 42, task.CreatedAt)
	if err != nil || empty.Sections == nil || len(empty.Sections) != 0 || len(empty.Events) != 1 {
		t.Fatalf("empty history = %+v, %v", empty, err)
	}
	legacy := taskTimelineInput{
		run:         domain.Run{ID: "legacy", TaskID: task.ID, ProjectID: task.ProjectID},
		observation: domain.RunObservation{RunID: "legacy"},
	}
	got, err := taskTimeline(task, []taskTimelineInput{legacy}, 42, task.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	section := got.Sections[0]
	run := section.Runs[0]
	if section.CampaignID != nil || len(section.Events) != 0 || run.Role != nil || run.AttemptNumber != nil || run.Milestones == nil || run.Events == nil {
		t.Fatalf("legacy group = %+v", section)
	}
}
