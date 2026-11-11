package daemoncompose

import (
	"fmt"
	"maps"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// imageUpkeepPolicy is the policy image upkeep bounds a dependency change
// with: the registry set the configured projects declare. The daemon serves
// one repository, and upkeep keeps one image for it, so projects that
// declare different registry sets leave no single answer, and the daemon
// refuses to start rather than pick one. No declaration is a policy with no
// registry set, under which upkeep refuses any lockfile change.
//
// The result is not a run's policy and is never persisted, so it carries
// only keys: the gate and the room sizes read nothing else, and
// NewResolvedPolicy refuses the empty key set a config without a
// declaration yields.
func imageUpkeepPolicy(projects map[domain.ProjectID]engine.ManualInitiator) (domain.ResolvedPolicy, error) {
	var keys []domain.PolicyKey
	for projectID, project := range projects {
		for _, key := range project.PolicyKeys {
			if key.Key != domain.RegistrySetPolicyKey {
				continue
			}
			if len(keys) != 0 && keys[0].Value != key.Value {
				return domain.ResolvedPolicy{}, fmt.Errorf(
					"manual submission config project %q declares a %s other than another project's; image upkeep keeps one image per repository and needs one",
					projectID, domain.RegistrySetPolicyKey)
			}
			keys = []domain.PolicyKey{key}
		}
	}
	return domain.ResolvedPolicy{Keys: keys}, nil
}

// RequireManualProjectsFit refuses a configured project whose launch sizes
// can never fit bound, so a policy no launch could ever run under refuses the
// start instead of raising an item at its first launch.
func RequireManualProjectsFit(projects map[domain.ProjectID]engine.ManualInitiator, bound ward.LaunchBound) error {
	for _, id := range slices.Sorted(maps.Keys(projects)) {
		if _, err := ward.ResolveLaunchSizes(projects[id].PolicyKeys, policySizeBound(bound)); err != nil {
			return fmt.Errorf("manual submission config project %q: %w", id, err)
		}
	}
	return nil
}
