package daemoncompose

import (
	"context"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestImageRepairProjectIsTheFirstConfiguredProject(t *testing.T) {
	t.Parallel()
	if _, ok := ImageRepairProject(nil); ok {
		t.Fatal("no configured project chose one")
	}
	got, ok := ImageRepairProject(map[domain.ProjectID]engine.ManualInitiator{"project-b": {}, "project-a": {}})
	if !ok || got != "project-a" {
		t.Fatalf("project = %q %v, want project-a", got, ok)
	}
}

// Only a composition that selects each attempt's image submits maintenance
// tasks: without selection a task could not run at its refusal's tip.
func TestImageUpkeepSubmitsMaintenanceOnlyWithImageSelection(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
	t.Cleanup(func() { _ = st.Close() })
	cfg := ClaudeStartupConfig{Claude: &ClaudeDriverConfig{SeedRoot: t.TempDir(), Repo: "owner/repo", RepositoryID: 7}}
	for name, selection := range map[string]engine.ImageSelection{
		"attended": nil,
		"unattended": func(context.Context, domain.InvocationID, domain.BaseRevision, *domain.ImageUpkeepRefusal) (domain.ImageRef, error) {
			return "", nil
		},
	} {
		upkeeps, err := composeImageUpkeeps(t.Context(), st, signet.NewService(st), nil, cfg,
			&ClaudeComposition{selectImage: selection}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		upkeep := upkeeps[0].upkeep
		submits := upkeep.SubmitMaintenance != nil && upkeep.MaintenancePending != nil
		if submits != (selection != nil) {
			t.Fatalf("%s composition submits maintenance = %t", name, submits)
		}
	}
}

// baseCapableBuilder builds project images and agent bases, as the daemon's
// builder does. Composition only checks its shape; nothing calls it.
type baseCapableBuilder struct{}

func (baseCapableBuilder) Build(context.Context, projectimage.Request) (domain.ProjectImage, error) {
	return domain.ProjectImage{}, nil
}

func (baseCapableBuilder) BuildAgentBase(context.Context, projectimage.AgentBaseRequest) (projectimage.AgentBase, error) {
	return projectimage.AgentBase{}, nil
}

// Only the configured repository's upkeep keeps the agent base: the base is
// one daemon-wide pointer the agent_base setting may build from that
// repository alone, so another repository's upkeep gets no keeper.
func TestOnlyTheConfiguredRepositorysUpkeepKeepsTheAgentBase(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
	t.Cleanup(func() { _ = st.Close() })
	cfg := ClaudeStartupConfig{Claude: &ClaudeDriverConfig{Repo: "owner/a", RepositoryID: 1}}
	rebuild := &engine.ProjectImageRebuild{Builder: baseCapableBuilder{}}
	var doctorLock sync.Mutex
	for _, loop := range []repositoryLoop{
		{Repo: "owner/a", RepositoryID: 1, Project: "project-a"},
		{Repo: "owner/b", RepositoryID: 2, Projects: []domain.ProjectID{"project-b"}},
	} {
		upkeep := composeImageUpkeep(st, signet.NewService(st), nil, cfg, &ClaudeComposition{}, rebuild, loop,
			t.TempDir(), &doctorLock, &orphanUpkeepCards{store: st, cfg: cfg})
		keeps := upkeep.AgentBase != nil
		if keeps != (loop.RepositoryID == cfg.Claude.RepositoryID) {
			t.Fatalf("%s keeps the agent base = %t", loop.Repo, keeps)
		}
		if keeps && upkeep.AgentBase.Repository != loop.Repo {
			t.Fatalf("keeper repository = %q, want %q", upkeep.AgentBase.Repository, loop.Repo)
		}
	}
}
