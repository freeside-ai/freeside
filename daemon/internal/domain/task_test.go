package domain_test

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestTaskValidation(t *testing.T) {
	t.Parallel()
	valid := domain.Task{
		ID: "task-1", ProjectID: "project-1",
		Name:      domain.DisplayName{Text: "Task one", Source: domain.DisplayNameSourceOperator},
		Source:    &domain.SpecificationSource{Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: "source-1"},
		CreatedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC), CampaignIDs: []domain.CampaignID{"campaign-1"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.Task){
		"missing ID":               func(task *domain.Task) { task.ID = "" },
		"missing project":          func(task *domain.Task) { task.ProjectID = "" },
		"missing creation":         func(task *domain.Task) { task.CreatedAt = time.Time{} },
		"nil campaigns":            func(task *domain.Task) { task.CampaignIDs = nil },
		"duplicate campaign":       func(task *domain.Task) { task.CampaignIDs = []domain.CampaignID{"campaign-1", "campaign-1"} },
		"project-only name source": func(task *domain.Task) { task.Name.Source = domain.DisplayNameSourceName },
		"invalid source union": func(task *domain.Task) {
			task.Source = &domain.SpecificationSource{Kind: domain.SpecificationSourceIssueSubject}
		},
	} {
		t.Run(name, func(t *testing.T) {
			task := valid
			mutate(&task)
			if err := task.Validate(); err == nil {
				t.Fatal("accepted invalid task")
			}
		})
	}
}

func campaign(id domain.CampaignID) *domain.CampaignID { return &id }
func binding(id domain.WorkUnitID) *domain.WorkUnitID  { return &id }

// wipFacts builds an ordinal-ordered log from kind/campaign/binding tuples so
// each case reads as the recorded history it represents.
func startedFact(ord int, run domain.RunID, camp domain.CampaignID) domain.TaskLifecycleFact {
	return domain.TaskLifecycleFact{Ordinal: ord, Kind: domain.TaskLifecycleStarted, RunID: run, CampaignID: campaign(camp), SourceID: "start:" + string(run) + ":" + string(camp), RecordedAt: time.Date(2026, 9, 12, 12, ord, 0, 0, time.UTC)}
}

func completedFact(ord int, run domain.RunID, camp domain.CampaignID) domain.TaskLifecycleFact {
	return domain.TaskLifecycleFact{Ordinal: ord, Kind: domain.TaskLifecycleCompleted, RunID: run, CampaignID: campaign(camp), BindingUnitID: binding(domain.WorkUnitIDForRun(run)), SourceID: "complete:" + string(run), RecordedAt: time.Date(2026, 9, 12, 12, ord, 0, 0, time.UTC)}
}

func abandonedFact(ord int, run domain.RunID) domain.TaskLifecycleFact {
	return domain.TaskLifecycleFact{Ordinal: ord, Kind: domain.TaskLifecycleAbandoned, RunID: run, SourceID: "abandon:" + string(run), RecordedAt: time.Date(2026, 9, 12, 12, ord, 0, 0, time.UTC)}
}

func TestTaskLifecycleFactKinds(t *testing.T) {
	t.Parallel()
	for _, k := range domain.AllTaskLifecycleFactKinds {
		f := domain.TaskLifecycleFact{Ordinal: 1, Kind: k, RunID: "r1", SourceID: "s1", RecordedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
		if k == domain.TaskLifecycleCompleted {
			f.BindingUnitID = binding("workunit-r1")
		}
		if err := f.Validate(); err != nil {
			t.Fatalf("kind %q: %v", k, err)
		}
	}
}

func TestTaskWIP(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		campaigns []domain.CampaignID
		facts     []domain.TaskLifecycleFact
		want      bool
	}{
		{"no facts", []domain.CampaignID{"c1"}, nil, false},
		{"started only", []domain.CampaignID{"c1"}, []domain.TaskLifecycleFact{startedFact(1, "r1", "c1")}, true},
		{"current-binding completion releases", []domain.CampaignID{"c1"}, []domain.TaskLifecycleFact{startedFact(1, "r1", "c1"), completedFact(2, "r1", "c1")}, false},
		{"abandonment releases", []domain.CampaignID{"c1"}, []domain.TaskLifecycleFact{startedFact(1, "r1", "c1"), abandonedFact(2, "r1")}, false},
		{"restart after completion restores slot", []domain.CampaignID{"c1"}, []domain.TaskLifecycleFact{startedFact(1, "r1", "c1"), completedFact(2, "r1", "c1"), startedFact(3, "r2", "c1")}, true},
		{"restart after abandonment restores slot", []domain.CampaignID{"c1"}, []domain.TaskLifecycleFact{startedFact(1, "r1", "c1"), abandonedFact(2, "r1"), startedFact(3, "r2", "c1")}, true},
		// A revision appends a new campaign; a late completion on the prior
		// campaign's binding is history and does not release the slot.
		{"superseded completion does not release", []domain.CampaignID{"c1", "c2"}, []domain.TaskLifecycleFact{startedFact(1, "r1", "c1"), startedFact(2, "r2", "c2"), completedFact(3, "r1", "c1")}, true},
		{"current completion after revision releases", []domain.CampaignID{"c1", "c2"}, []domain.TaskLifecycleFact{startedFact(1, "r1", "c1"), startedFact(2, "r2", "c2"), completedFact(3, "r2", "c2")}, false},
		// A reserved-but-unstarted proposal appends c2 to CampaignIDs but records
		// no start; currency is the newest start's campaign (c1), so the c1
		// completion still releases the slot (issue #1318 G2; docs/plan.md §5.11).
		{"reserved campaign does not shift currency", []domain.CampaignID{"c1", "c2"}, []domain.TaskLifecycleFact{startedFact(1, "r1", "c1"), completedFact(2, "r1", "c1")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := domain.Task{
				ID: "task-1", ProjectID: "project-1",
				Name:      domain.DisplayName{Text: "Task one", Source: domain.DisplayNameSourceOperator},
				CreatedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC), CampaignIDs: tc.campaigns, LifecycleFacts: tc.facts,
			}
			if err := task.Validate(); err != nil {
				t.Fatalf("invalid fixture: %v", err)
			}
			if got := domain.TaskWIP(task); got != tc.want {
				t.Fatalf("TaskWIP = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTaskLifecycleFactValidation(t *testing.T) {
	t.Parallel()
	base := domain.Task{
		ID: "task-1", ProjectID: "project-1",
		Name:      domain.DisplayName{Text: "Task one", Source: domain.DisplayNameSourceOperator},
		CreatedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC), CampaignIDs: []domain.CampaignID{"c1"},
	}
	for name, facts := range map[string][]domain.TaskLifecycleFact{
		"invalid kind":              {{Ordinal: 1, Kind: "spawned", RunID: "r1", SourceID: "s1", RecordedAt: base.CreatedAt}},
		"completed without binding": {{Ordinal: 1, Kind: domain.TaskLifecycleCompleted, RunID: "r1", SourceID: "s1", RecordedAt: base.CreatedAt}},
		"started with binding":      {startedWithBinding()},
		"missing run":               {{Ordinal: 1, Kind: domain.TaskLifecycleStarted, SourceID: "s1", RecordedAt: base.CreatedAt}},
		"non-utc timestamp":         {{Ordinal: 1, Kind: domain.TaskLifecycleStarted, RunID: "r1", SourceID: "s1", RecordedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.FixedZone("x", 3600))}},
		"non-contiguous ordinal":    {startedFact(2, "r1", "c1")},
		"duplicate source":          {startedFact(1, "r1", "c1"), {Ordinal: 2, Kind: domain.TaskLifecycleAbandoned, RunID: "r2", SourceID: "start:r1:c1", RecordedAt: base.CreatedAt}},
	} {
		t.Run(name, func(t *testing.T) {
			task := base
			task.LifecycleFacts = facts
			if err := task.Validate(); err == nil {
				t.Fatal("accepted invalid lifecycle facts")
			}
		})
	}
}

func startedWithBinding() domain.TaskLifecycleFact {
	f := startedFact(1, "r1", "c1")
	f.BindingUnitID = binding("workunit-r1")
	return f
}

func TestTaskSubjectValidation(t *testing.T) {
	t.Parallel()
	id := domain.TaskID("task-1")
	subject := domain.Subject{Type: domain.SubjectTask, ID: "task-1", TaskID: &id}
	if err := subject.Validate(); err != nil {
		t.Fatal(err)
	}
	other := domain.TaskID("task-2")
	for _, invalid := range []domain.Subject{
		{Type: domain.SubjectTask, ID: "task-1"},
		{Type: domain.SubjectTask, ID: "task-1", TaskID: &other},
		{Type: domain.SubjectTask, ID: "task-1", TaskID: &id, RunID: new(domain.RunID("run-1"))},
		{Type: domain.SubjectProject, ID: "project-1", TaskID: &id},
		{Type: domain.SubjectSystem, ID: "daemon", TaskID: &id},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("accepted invalid subject: %+v", invalid)
		}
	}
}
