package store_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func bindTestItemSubject(t *testing.T, s *store.Store, item *domain.AttentionItem) {
	t.Helper()
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error { return storetest.BindSubject(t.Context(), tx, item) }); err != nil {
		t.Fatal(err)
	}
}

func taskIssueSource() domain.SpecificationSource {
	return domain.SpecificationSource{
		Kind:         domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42},
	}
}

func TestTaskIntakeConcurrentReplayAndProjectIsolation(t *testing.T) {
	t.Parallel()
	for _, kind := range []domain.SpecificationSourceKind{domain.SpecificationSourceIssueSubject, domain.SpecificationSourceWorkItemArtifact} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			s := openStore(t, store.Options{})
			sources := []domain.SpecificationSource{taskIssueSource()}
			if kind == domain.SpecificationSourceWorkItemArtifact {
				sources = nil
				for _, id := range []domain.ArtifactID{"source-a", "source-b"} {
					artifact := domain.Artifact{
						ID: id, Type: domain.ArtifactKindSpecification, Digest: "sha256:same-source",
						Provenance: domain.Provenance{
							ProducerClass: domain.ProducerAgent, ProducerInvocationID: "intake",
							HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
						}, Metadata: runMeta(),
					}
					if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutArtifact(ctx, artifact) }); err != nil {
						t.Fatal(err)
					}
					sources = append(sources, domain.SpecificationSource{Kind: kind, WorkItemArtifactID: id})
				}
			}
			const count = 8
			ids := make(chan domain.TaskID, count)
			errs := make(chan error, count)
			var wg sync.WaitGroup
			for i := range count {
				source := sources[i%len(sources)]
				wg.Go(func() {
					var task domain.Task
					err := s.Write(ctx, func(tx *store.WriteTx) error {
						var err error
						task, err = tx.GetOrCreateTask(ctx, "project-a", source)
						return err
					})
					errs <- err
					ids <- task.ID
				})
			}
			wg.Wait()
			close(ids)
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			var first domain.TaskID
			for id := range ids {
				if id == "" || (first != "" && id != first) {
					t.Fatalf("duplicate intake IDs: %q and %q", first, id)
				}
				first = id
			}
			if err := s.Write(ctx, func(tx *store.WriteTx) error {
				replay, err := tx.GetOrCreateTask(ctx, "project-a", sources[len(sources)-1])
				if err != nil {
					return err
				}
				other, err := tx.GetOrCreateTask(ctx, "project-b", sources[0])
				if err != nil {
					return err
				}
				if replay.ID != first || other.ID == first || other.ID == "" {
					t.Fatalf("replay=%q other project=%q original=%q", replay.ID, other.ID, first)
				}
				all, err := tx.ListTasks(ctx)
				if err == nil && len(all) != 2 {
					t.Fatalf("task count=%d, want 2", len(all))
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTaskIntakeRollbackAndStoredName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	source := taskIssueSource()
	crash := errors.New("crash before commit")
	var abandoned domain.TaskID
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetOrCreateTask(ctx, "project", source)
		if err != nil {
			return err
		}
		abandoned = task.ID
		return crash
	}); !errors.Is(err, crash) {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		all, err := tx.ListTasks(ctx)
		if err != nil {
			return err
		}
		if len(all) != 0 {
			t.Fatalf("rollback retained %d tasks", len(all))
		}
		task, err := tx.GetOrCreateTask(ctx, "project", source)
		if err != nil {
			return err
		}
		if task.ID == abandoned || task.Name.Text != string(task.ID) || task.Name.Source != domain.DisplayNameSourceIdentifier {
			t.Fatalf("new task identity/name: %+v", task)
		}
		approved := domain.DisplayName{Text: "Improve task navigation", Source: domain.DisplayNameSourceSpecification}
		if err := tx.SetTaskName(ctx, task.ID, approved); err != nil {
			return err
		}
		if err := tx.SetTaskName(ctx, task.ID, domain.DisplayName{Text: "A later campaign title", Source: domain.DisplayNameSourceSpecification}); err != nil {
			return err
		}
		got, err := tx.GetTask(ctx, task.ID)
		if err == nil && got.Name != approved {
			t.Fatalf("approved name changed: %+v", got.Name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskNameAdvancesAttentionSnapshots(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedItem(t, s, &f)
	var before, after domain.AttentionItem
	var oldSnapshot, newSnapshot store.Snapshot
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		before, oldSnapshot, err = tx.GetAttentionItemSnapshot(ctx, f.item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	approved := domain.DisplayName{Text: "Approved task name", Source: domain.DisplayNameSourceSpecification}
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.SetTaskName(ctx, f.run.TaskID, approved) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		after, newSnapshot, err = tx.GetAttentionItemSnapshot(ctx, f.item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after.DisplayNames.Task != approved || before.DisplayNames.Task == approved {
		t.Fatalf("task name before=%+v after=%+v", before.DisplayNames.Task, after.DisplayNames.Task)
	}
	if newSnapshot.EntityVersion <= oldSnapshot.EntityVersion || newSnapshot.AsOfRevision <= oldSnapshot.AsOfRevision {
		t.Fatalf("changed name reused snapshot metadata: before=%+v after=%+v", oldSnapshot, newSnapshot)
	}
	if after.ItemVersion != before.ItemVersion || after.DecisionSurface != before.DecisionSurface {
		t.Fatal("task naming changed the decision binding")
	}
	// A replay or later campaign title cannot create a new presentation version.
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.SetTaskName(ctx, f.run.TaskID, domain.DisplayName{Text: "Later title", Source: domain.DisplayNameSourceSpecification})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, replay, err := tx.GetAttentionItemSnapshot(ctx, f.item.ID)
		if err == nil && replay != newSnapshot {
			t.Fatalf("unchanged name advanced metadata: before=%+v after=%+v", newSnapshot, replay)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskNamePreservesOperatorChoice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetOrCreateTask(ctx, "project", taskIssueSource())
		if err != nil {
			return err
		}
		operator := domain.DisplayName{Text: "My task", Source: domain.DisplayNameSourceOperator}
		if err := tx.SetTaskName(ctx, task.ID, operator); err != nil {
			return err
		}
		if err := tx.SetTaskName(ctx, task.ID, domain.DisplayName{Text: "Approved specification", Source: domain.DisplayNameSourceSpecification}); err != nil {
			return err
		}
		got, err := tx.GetTask(ctx, task.ID)
		if err == nil && got.Name != operator {
			t.Fatalf("operator name changed: %+v", got.Name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskMigrationReusesLegacySource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openPreRenameDatabase(t)
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		spec, err := tx.GetRun(ctx, legacySpecificationRun)
		if err != nil {
			return err
		}
		impl, err := tx.GetRun(ctx, legacyImplementationRun)
		if err != nil {
			return err
		}
		if spec.TaskID == "" || spec.TaskID != impl.TaskID {
			t.Fatalf("legacy task continuity: spec=%q implementation=%q", spec.TaskID, impl.TaskID)
		}
		task, err := tx.GetTask(ctx, spec.TaskID)
		if err != nil {
			return err
		}
		if task.Source == nil {
			t.Fatal("legacy source artifact was not recovered")
		}
		replay, err := tx.GetOrCreateTask(ctx, task.ProjectID, *task.Source)
		if err != nil {
			return err
		}
		if replay.ID != task.ID {
			t.Fatalf("later submit created %q, want migrated task %q", replay.ID, task.ID)
		}
		item, err := tx.GetAttentionItem(ctx, legacyApprovalItem)
		if err == nil && (item.Subject.TaskID == nil || *item.Subject.TaskID != task.ID) {
			t.Fatalf("legacy approval task: %+v", item.Subject)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskRetainsRetryAndLaterCampaignHistory(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openPreRenameDatabase(t)
	var parent, retry, later domain.Run
	var task, unrelated domain.Task
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		parent, err = tx.GetRun(ctx, legacyImplementationRun)
		if err != nil {
			return err
		}
		task, err = tx.GetTask(ctx, parent.TaskID)
		if err != nil {
			return err
		}
		attempt, err := tx.GetProductionAttempt(ctx, parent.CampaignID, 1)
		if err != nil {
			return err
		}
		retryID, err := engine.ProductionAttemptRunID(parent.CampaignID, 2)
		if err != nil {
			return err
		}
		retry = domain.Run{
			ID: retryID, ProjectID: parent.ProjectID, SpecDigest: parent.SpecDigest,
			PolicyDigest: parent.PolicyDigest, CampaignID: parent.CampaignID, AttemptNumber: 2,
			AttemptReason: "Retry after repair", ParentRunID: parent.ID, Stages: []domain.Stage{},
		}
		if err := tx.PutProductionAttempt(ctx, domain.ProductionAttempt{
			CampaignID: parent.CampaignID, AttemptNumber: 2, Kind: domain.ProductionAttemptRetry,
			Reason: retry.AttemptReason, ParentRunID: parent.ID, SourceDigest: attempt.SourceDigest,
			PublicationDigest: attempt.PublicationDigest, ApprovedSpecDigest: parent.SpecDigest,
			SpecificationRunID: attempt.SpecificationRunID, ImplementationRunID: retry.ID,
		}); err != nil {
			return err
		}
		unrelated, err = tx.GetOrCreateTask(ctx, parent.ProjectID, taskIssueSource())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	forged := retry
	forged.TaskID = unrelated.ID
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, forged) }); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("retry accepted another same-project task: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.AssignTask(ctx, &retry, nil); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, retry); err != nil {
			return err
		}
		approved := domain.DisplayName{Text: "Original approved task", Source: domain.DisplayNameSourceSpecification}
		if err := tx.SetTaskName(ctx, task.ID, approved); err != nil {
			return err
		}
		implementationID := domain.RunID("run-second-campaign")
		campaignID, err := engine.ProductionCampaignIDForImplementation(implementationID)
		if err != nil {
			return err
		}
		original, err := tx.GetRun(ctx, legacySpecificationRun)
		if err != nil {
			return err
		}
		later = domain.Run{
			ID: domain.SpecificationRunIDForImplementation(implementationID), ProjectID: parent.ProjectID,
			SpecDigest: original.SpecDigest, PolicyDigest: original.PolicyDigest, CampaignID: campaignID, AttemptNumber: 1, Stages: []domain.Stage{},
		}
		if err := tx.PutProductionAttempt(ctx, domain.ProductionAttempt{
			CampaignID: campaignID, AttemptNumber: 1,
			Kind: domain.ProductionAttemptInitial, SourceDigest: original.SpecDigest, PublicationDigest: "sha256:later-publication",
			SpecificationRunID: later.ID, ImplementationRunID: implementationID,
		}); err != nil {
			return err
		}
		if err := tx.AssignTask(ctx, &later, task.Source); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, later); err != nil {
			return err
		}
		if err := tx.SetTaskName(ctx, task.ID, domain.DisplayName{Text: "Later campaign title", Source: domain.DisplayNameSourceSpecification}); err != nil {
			return err
		}
		stored, err := tx.GetTask(ctx, task.ID)
		if err != nil {
			return err
		}
		if retry.TaskID != task.ID || later.TaskID != task.ID || stored.Name != approved || !slices.Equal(stored.CampaignIDs, []domain.CampaignID{parent.CampaignID, campaignID}) {
			t.Fatalf("task continuity: retry=%q later=%q task=%+v", retry.TaskID, later.TaskID, stored)
		}
		runs, err := tx.TaskRunIDs(ctx, task.ID)
		if err == nil && !slices.Equal(runs, []domain.RunID{legacySpecificationRun, parent.ID, retry.ID, later.ID}) {
			t.Fatalf("task run order = %v", runs)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
