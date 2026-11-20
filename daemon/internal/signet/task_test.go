package signet_test

import (
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestBootstrapProjectsStoredTaskAndNewestRun(t *testing.T) {
	t.Parallel()
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
	if got.ID != task.ID || got.ProjectID != task.ProjectID || got.DisplayNames.Task.Text != "Durable task name" || got.Source == nil || got.Source.IssueSubject.IssueNumber != 42 || got.Source.WorkItemArtifactDigest != nil {
		t.Fatalf("stored task projection = %+v", got)
	}
	if !slices.Equal(got.RunIDs, []domain.RunID{"run-earlier", "run-current"}) || len(got.CampaignIDs) != 0 || got.LastActivityAt.Before(got.CreatedAt) {
		t.Fatalf("task history = %+v", got)
	}
	current := bootstrap.Runs[slices.IndexFunc(bootstrap.Runs, func(snapshot signet.RunSnapshot) bool { return snapshot.Run.ID == "run-current" })].Run
	if got.Lifecycle == nil || string(*got.Lifecycle) != string(current.Lifecycle) || got.CurrentPosition == nil || got.CurrentPosition.RunID != "run-current" || got.CurrentPosition.Stage == nil || *got.CurrentPosition.Stage != "implementation" {
		t.Fatalf("task current position = %+v, lifecycle=%v", got.CurrentPosition, got.Lifecycle)
	}
}

// Task.issue is the intake issue of an issue_subject task, the recorded
// submitted issue of a work_item_artifact task, and null for any other task.
func TestTaskProjectsItsIssue(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	ctx := t.Context()
	intake := domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42}
	submitted := domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 7}
	want := map[domain.TaskID]*domain.IssueSubjectRef{}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetOrCreateTask(ctx, "proj-1", domain.SpecificationSource{
			Kind: domain.SpecificationSourceIssueSubject, IssueSubject: &intake,
		})
		if err != nil {
			return err
		}
		want[task.ID] = &intake
		for _, id := range []domain.ArtifactID{"source-url", "source-text"} {
			artifact, err := domain.NewArtifact(domain.ArtifactInput{
				ID: id, Type: domain.ArtifactKindSpecification, Digest: domain.Digest("sha256:" + string(id)),
				Provenance: domain.Provenance{
					ProducerClass: domain.ProducerAgent, ProducerInvocationID: "submit",
					HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
				},
				Metadata: domain.EvidenceMetadata{
					MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: 1, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
					Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
				},
			}, map[domain.Digest]bool{})
			if err != nil {
				return err
			}
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
			task, err := tx.GetOrCreateTask(ctx, "proj-1", domain.SpecificationSource{
				Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: id,
			})
			if err != nil {
				return err
			}
			want[task.ID] = nil
			if id == "source-url" {
				if err := tx.SetTaskSubmittedIssue(ctx, task.ID, submitted); err != nil {
					return err
				}
				want[task.ID] = &submitted
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bootstrap.Tasks) != len(want) {
		t.Fatalf("tasks = %d, want %d", len(bootstrap.Tasks), len(want))
	}
	for _, snapshot := range bootstrap.Tasks {
		got, expected := snapshot.Task.Issue, want[snapshot.Task.ID]
		if (got == nil) != (expected == nil) || (got != nil && *got != *expected) {
			t.Errorf("task %s issue = %+v, want %+v", snapshot.Task.ID, got, expected)
		}
	}
}
