package daemoncompose

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
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
		"unattended": func(context.Context, domain.InvocationID, *domain.ImageUpkeepRefusal) (domain.ImageRef, error) {
			return "", nil
		},
	} {
		upkeep, err := composeImageUpkeep(st, signet.NewService(st), nil, cfg,
			&ClaudeComposition{selectImage: selection}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		submits := upkeep.SubmitMaintenance != nil && upkeep.MaintenancePending != nil
		if submits != (selection != nil) {
			t.Fatalf("%s composition submits maintenance = %t", name, submits)
		}
	}
}
