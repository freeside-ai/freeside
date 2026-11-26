package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// setWriterClass stands in for the project's writer_class setting: e
// answers every project with its static base's repository and class.
func setWriterClass(e *Engine, class *domain.RunnerBackendClass) {
	base := e.admission.environment.Base
	e.projectAuthoritySource = FixedProjectAuthority(ProjectAuthority{
		Repo: base.Repo, RepositoryID: base.RepositoryID, BaseRef: base.BaseRef, WriterClass: class,
	})
}

// projectsByID answers each project in projects with its authority, read at
// call time so a test can edit one between gates, and refuses any other.
func projectsByID(projects map[domain.ProjectID]ProjectAuthority) ProjectAuthoritySource {
	return func(_ context.Context, _ *store.ReadTx, id domain.ProjectID) (ProjectAuthority, error) {
		project, ok := projects[id]
		if !ok {
			return ProjectAuthority{}, fmt.Errorf("project %q: %w", id, ErrProjectSettingsRefused)
		}
		return project.clone(), nil
	}
}

// TestAdmissionReadsTheRunsProject: with two projects' settings present, an
// admission derives its base from the run's own project, records that
// project's settings revision with the instance's, and refuses a base that
// names any other repository, including one a repository edit leaves behind
// a run already admitted.
func TestAdmissionReadsTheRunsProject(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	warded := domain.BackendFreshVMReadOnlyVolumeHandoff
	widgets := ProjectAuthority{
		ProjectID: f.run.ProjectID, SettingsRevision: 7,
		Repo: "owner/repo", RepositoryID: 1, BaseRef: "refs/heads/main", WriterClass: &warded,
	}
	gadgets := ProjectAuthority{
		ProjectID: "project-gadgets", SettingsRevision: 3,
		Repo: "owner/gadgets", RepositoryID: 2, BaseRef: "refs/heads/trunk", WriterClass: &warded,
	}
	projects := map[domain.ProjectID]ProjectAuthority{widgets.ProjectID: widgets, gadgets.ProjectID: gadgets}
	f.engine.projectAuthoritySource = projectsByID(projects)
	f.engine.admission.environment.InstanceSettingsRevision = 5
	var derivedFor []domain.ProjectID
	derive := func(_ context.Context, _ domain.InvocationID, project ProjectAuthority) (string, domain.BaseRevision, error) {
		derivedFor = append(derivedFor, project.ProjectID)
		return "workspace-1", domain.BaseRevision{
			Repo: project.Repo, RepositoryID: project.RepositoryID, BaseRef: project.BaseRef, BaseSHA: "deadbeef",
		}, nil
	}
	f.engine.derive = derive
	gadgetsRun := seedRunPolicy(t, f.store, domain.Run{
		ID: "run-gadgets", ProjectID: gadgets.ProjectID, SpecDigest: agentTestDigest("9"),
	}, nil)

	for _, tc := range []struct {
		run     domain.Run
		project ProjectAuthority
	}{{f.run, widgets}, {gadgetsRun, gadgets}} {
		derivedFor = nil
		admission, err := f.admitRun(t, tc.run, agentTestAt)
		if err != nil {
			t.Fatalf("admit %s: %v", tc.run.ID, err)
		}
		if len(derivedFor) != 1 || derivedFor[0] != tc.project.ProjectID {
			t.Fatalf("admit %s derived for %v, want only %s", tc.run.ID, derivedFor, tc.project.ProjectID)
		}
		if admission.Base.Repo != tc.project.Repo || admission.Base.RepositoryID != tc.project.RepositoryID ||
			admission.Base.BaseRef != tc.project.BaseRef {
			t.Fatalf("admit %s at %+v, want %s's repository", tc.run.ID, admission.Base, tc.project.ProjectID)
		}
		want := domain.SettingsRevisions{Instance: 5, Project: tc.project.SettingsRevision}
		if admission.SettingsRevisions == nil || *admission.SettingsRevisions != want {
			t.Fatalf("admit %s recorded revisions %+v, want %+v", tc.run.ID, admission.SettingsRevisions, want)
		}
	}

	// A derivation that ignores the project, as one reading the composed
	// repository would, puts a gadgets run at widgets' repository.
	f.engine.derive = func(ctx context.Context, id domain.InvocationID, _ ProjectAuthority) (string, domain.BaseRevision, error) {
		return derive(ctx, id, widgets)
	}
	if _, err := f.admitRun(t, gadgetsRun, agentTestAt); !errors.Is(err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("gadgets run at widgets' repository: %v, want ErrRepositoryIdentityMismatch", err)
	}
	f.engine.derive = derive

	// A run admitted once keeps its recorded base: a repository edit holds
	// it at its next admission until the edit is undone.
	admission, _, err := f.admit(t)
	if err != nil {
		t.Fatal(err)
	}
	f.record(t, admission)
	// recordAttempt records the run's base with its first admission.
	f.binding.run.Base = &admission.Base
	edited := widgets
	edited.RepositoryID = 9
	projects[widgets.ProjectID] = edited
	if _, _, err := f.admit(t); !errors.Is(err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("admit after a repository edit: %v, want ErrRepositoryIdentityMismatch", err)
	}
	projects[widgets.ProjectID] = widgets
	if _, _, err := f.admit(t); err != nil {
		t.Fatalf("admit after the edit was undone: %v", err)
	}

	delete(projects, widgets.ProjectID)
	if _, _, err := f.admit(t); !errors.Is(err, ErrProjectSettingsRefused) {
		t.Fatalf("admit with no project settings: %v, want ErrProjectSettingsRefused", err)
	}
}
