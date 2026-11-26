package daemoncompose

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// Upkeep keeps one image per repository, so it needs one registry set: the
// one the repository's projects declare, none when none declares one, and a
// refusal to start when two disagree.
func TestRepositoryUpkeepPolicyTakesTheProjectsOneRegistrySet(t *testing.T) {
	t.Parallel()
	registrySet := func(value string) domain.PolicyKey {
		return domain.PolicyKey{
			Key: domain.RegistrySetPolicyKey, Value: value,
			Provenance: domain.KeyProvenance{Source: "manual-submission-config"},
		}
	}
	other := domain.PolicyKey{Key: ward.PolicyWriterMemoryMiB, Value: "4096"}
	projects := func(keys ...[]domain.PolicyKey) map[domain.ProjectID]engine.ManualInitiator {
		out := map[domain.ProjectID]engine.ManualInitiator{}
		for index, projectKeys := range keys {
			out[domain.ProjectID(fmt.Sprintf("project-%d", index))] = engine.ManualInitiator{PolicyKeys: projectKeys}
		}
		return out
	}
	declared := registrySet(`["registry.npmjs.org"]`)
	for name, test := range map[string]struct {
		projects map[domain.ProjectID]engine.ManualInitiator
		want     []domain.PolicyKey
		fails    bool
	}{
		"no projects":              {projects: projects()},
		"no declaration":           {projects: projects([]domain.PolicyKey{other})},
		"one declaration":          {projects: projects([]domain.PolicyKey{other, declared}), want: []domain.PolicyKey{declared}},
		"two agreeing projects":    {projects: projects([]domain.PolicyKey{declared}, []domain.PolicyKey{declared}), want: []domain.PolicyKey{declared}},
		"two disagreeing projects": {projects: projects([]domain.PolicyKey{declared}, []domain.PolicyKey{registrySet(`["npm.example.test"]`)}), fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			policy, err := repositoryUpkeepPolicy(test.projects)
			if test.fails {
				if err == nil {
					t.Fatalf("policy = %+v, want a refusal", policy)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(policy.Keys, test.want) {
				t.Fatalf("policy = %+v, %v; want keys %+v", policy, err, test.want)
			}
		})
	}
}

// Each served repository gets one loop entry with its own projects and its
// own registry set, so projects on different repositories may declare
// different sets; projects on one repository still may not.
func TestRepositoryLoopsGroupProjectsByRepository(t *testing.T) {
	t.Parallel()
	registrySet := func(value string) []domain.PolicyKey {
		return []domain.PolicyKey{{
			Key: domain.RegistrySetPolicyKey, Value: value,
			Provenance: domain.KeyProvenance{Source: "manual-submission-config"},
		}}
	}
	npm, other := registrySet(`["registry.npmjs.org"]`), registrySet(`["npm.example.test"]`)
	repositories := []loopRepository{
		{Repo: "owner/a", RepositoryID: 1, BaseRef: "main", Project: "project-a"},
		{Repo: "owner/b", RepositoryID: 2, BaseRef: "trunk"},
		{Repo: "owner/a", RepositoryID: 1, BaseRef: "main"},
	}
	intake := []IntakeInitiator{
		{RepositoryID: 2, ProjectID: "intake-b"},
		{RepositoryID: 9, ProjectID: "intake-elsewhere"},
	}
	manual := []boundManualProject{
		{ID: "manual-b", RepositoryID: 2, Initiator: engine.ManualInitiator{PolicyKeys: other}},
		{ID: "manual-a", RepositoryID: 1, Initiator: engine.ManualInitiator{PolicyKeys: npm}},
		{ID: "manual-unbound", Initiator: engine.ManualInitiator{PolicyKeys: npm}},
		{ID: "manual-elsewhere", RepositoryID: 9, Initiator: engine.ManualInitiator{PolicyKeys: other}},
	}
	loops, err := repositoryLoops(repositories, intake, manual)
	if err != nil {
		t.Fatal(err)
	}
	if len(loops) != 2 {
		t.Fatalf("loops = %+v, want one per repository", loops)
	}
	a, b := loops[0], loops[1]
	if a.RepositoryID != 1 || a.Repo != "owner/a" || a.BaseRef != "main" || a.cardProject() != "project-a" ||
		!reflect.DeepEqual(a.Projects, []domain.ProjectID{"manual-a", "manual-unbound"}) ||
		!reflect.DeepEqual(a.Policy.Keys, npm) {
		t.Fatalf("repository 1 loop = %+v", a)
	}
	if project, ok := ImageRepairProject(a.Manual); !ok || project != "manual-a" {
		t.Fatalf("repository 1 repair project = %q %t", project, ok)
	}
	if b.RepositoryID != 2 || b.BaseRef != "trunk" || b.cardProject() != "intake-b" ||
		!reflect.DeepEqual(b.Projects, []domain.ProjectID{"intake-b", "manual-b"}) ||
		!reflect.DeepEqual(b.Policy.Keys, other) {
		t.Fatalf("repository 2 loop = %+v", b)
	}

	_, err = repositoryLoops(repositories[:1], nil, append(manual,
		boundManualProject{ID: "manual-a2", RepositoryID: 1, Initiator: engine.ManualInitiator{PolicyKeys: other}}))
	if err == nil || !strings.Contains(err.Error(), "owner/a") {
		t.Fatalf("one repository's projects with two registry sets: err = %v, want a refusal naming it", err)
	}
	_, err = repositoryLoops(append(repositories, loopRepository{Repo: "owner/a", RepositoryID: 1, BaseRef: "dev"}), nil, nil)
	if err == nil {
		t.Fatal("one repository served on two branches composed")
	}
	if loops, err := repositoryLoops([]loopRepository{{Repo: "owner/c", RepositoryID: 3, BaseRef: "main"}}, nil, nil); err != nil ||
		len(loops) != 1 || loops[0].cardProject() != SchedulerSystemProjectID {
		t.Fatalf("a loop with no project: %+v, %v; want the system project's card", loops, err)
	}
}
