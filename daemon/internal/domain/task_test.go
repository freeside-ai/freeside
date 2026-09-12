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
