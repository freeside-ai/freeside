package domain_test

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestTaskDisplayLifecycleAndWIP(t *testing.T) {
	start := startedFact(1, "spec", "campaign")
	abandon := abandonedFact(2, "spec")
	complete := completedFact(2, "implementation", "campaign")
	for _, tc := range []struct {
		name    string
		facts   []domain.TaskLifecycleFact
		state   domain.TaskCancellationState
		episode int
		run     *domain.RunLifecycle
		want    *domain.TaskLifecycle
		wip     bool
	}{
		{"queued", nil, "", 0, nil, nil, false},
		{"queued pending", nil, domain.TaskCancellationRequested, 0, nil, nil, false},
		{"queued failed", nil, domain.TaskCancellationFailed, 0, nil, nil, false},
		{"queued confirmed", nil, domain.TaskCancellationConfirmed, 0, nil, new(domain.TaskStopped), false},
		{"active", []domain.TaskLifecycleFact{start}, "", 0, new(domain.RunLifecycleActive), new(domain.TaskActive), true},
		{"retryable failure", []domain.TaskLifecycleFact{start}, domain.TaskCancellationFailed, 1, new(domain.RunLifecycleFinished), new(domain.TaskFinished), true},
		{"pending", []domain.TaskLifecycleFact{start}, domain.TaskCancellationRequested, 1, new(domain.RunLifecycleActive), new(domain.TaskActive), true},
		{"legacy abandonment", []domain.TaskLifecycleFact{start, abandon}, "", 0, new(domain.RunLifecycleActive), new(domain.TaskAbandoned), false},
		{"confirmed", []domain.TaskLifecycleFact{start, abandon}, domain.TaskCancellationConfirmed, 1, new(domain.RunLifecycleActive), new(domain.TaskStopped), false},
		{"historical confirmation without fact", []domain.TaskLifecycleFact{start}, domain.TaskCancellationConfirmed, 1, new(domain.RunLifecycleActive), new(domain.TaskStopped), false},
		{"completion then confirmation", []domain.TaskLifecycleFact{start, complete}, domain.TaskCancellationConfirmed, 1, new(domain.RunLifecycleActive), new(domain.TaskFinished), false},
		{"confirmation then late completion", []domain.TaskLifecycleFact{start, abandon, completedFact(3, "implementation", "campaign")}, domain.TaskCancellationConfirmed, 1, new(domain.RunLifecycleActive), new(domain.TaskFinished), false},
		{"older confirmation", []domain.TaskLifecycleFact{start, abandon, startedFact(3, "retry", "campaign")}, domain.TaskCancellationConfirmed, 1, new(domain.RunLifecycleActive), new(domain.TaskActive), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := domain.Task{LifecycleFacts: tc.facts}
			if tc.state != "" {
				task.Cancellation = &domain.TaskCancellation{State: tc.state, Target: domain.TaskCancellationTarget{EpisodeOrdinal: tc.episode}}
			}
			got := task.DisplayLifecycle(tc.run)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) || domain.TaskWIP(task) != tc.wip {
				t.Fatalf("lifecycle=%v want=%v WIP=%v want=%v", got, tc.want, domain.TaskWIP(task), tc.wip)
			}
		})
	}
}

func TestLateCompletionCannotReleaseNewSameCampaignEpisode(t *testing.T) {
	late := completedFact(4, "old-implementation", "campaign")
	late.EpisodeOrdinal = 1
	task := domain.Task{LifecycleFacts: []domain.TaskLifecycleFact{
		startedFact(1, "spec", "campaign"), abandonedFact(2, "spec"), startedFact(3, "retry", "campaign"), late,
	}}
	if !domain.TaskWIP(task) || *task.DisplayLifecycle(new(domain.RunLifecycleActive)) != domain.TaskActive {
		t.Fatal("late completion changed the newer episode")
	}
}
