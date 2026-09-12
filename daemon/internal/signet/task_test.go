package signet_test

import (
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestBootstrapProjectsStoredTaskAndNewestRun(t *testing.T) {
	f := newRunFixture(t)
	ctx := t.Context()
	var task domain.Task
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		task, err = tx.GetOrCreateTask(ctx, "proj-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42},
		})
		if err != nil {
			return err
		}
		for _, id := range []domain.RunID{"run-earlier", "run-current"} {
			if err := tx.PutRun(ctx, domain.Run{
				ID: id, TaskID: task.ID, ProjectID: task.ProjectID,
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
				Stages: []domain.Stage{{ID: domain.StageID("stage-" + id), RunID: id, Name: "implementation", Attempts: []domain.Attempt{}}},
			}); err != nil {
				return err
			}
		}
		return tx.SetTaskName(ctx, task.ID, domain.DisplayName{Text: "Durable task name", Source: domain.DisplayNameSourceOperator})
	}); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bootstrap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(bootstrap.Tasks))
	}
	snapshot := bootstrap.Tasks[0]
	got := snapshot.Task
	if snapshot.AsOfRevision != bootstrap.Revision || snapshot.EntityVersion != bootstrap.Revision {
		t.Fatalf("task revision = %+v, bootstrap=%d", snapshot, bootstrap.Revision)
	}
	if got.ID != task.ID || got.ProjectID != task.ProjectID || got.DisplayNames.Task.Text != "Durable task name" || got.Source == nil || got.Source.IssueSubject.IssueNumber != 42 {
		t.Fatalf("stored task projection = %+v", got)
	}
	if !slices.Equal(got.RunIDs, []domain.RunID{"run-earlier", "run-current"}) || len(got.CampaignIDs) != 0 || got.LastActivityAt.Before(got.CreatedAt) {
		t.Fatalf("task history = %+v", got)
	}
	current := bootstrap.Runs[slices.IndexFunc(bootstrap.Runs, func(snapshot signet.RunSnapshot) bool { return snapshot.Run.ID == "run-current" })].Run
	if got.Lifecycle == nil || *got.Lifecycle != current.Lifecycle || got.CurrentPosition == nil || got.CurrentPosition.RunID != "run-current" || got.CurrentPosition.Stage == nil || *got.CurrentPosition.Stage != "implementation" {
		t.Fatalf("task current position = %+v, lifecycle=%v", got.CurrentPosition, got.Lifecycle)
	}
}
