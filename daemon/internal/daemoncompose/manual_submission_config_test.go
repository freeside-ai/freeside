package daemoncompose

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// Upkeep keeps one image per repository, so it needs one registry set: the
// one the configured projects declare, none when none declares one, and a
// refusal to start when two disagree.
func TestImageUpkeepPolicyTakesTheProjectsOneRegistrySet(t *testing.T) {
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
			policy, err := imageUpkeepPolicy(test.projects)
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
