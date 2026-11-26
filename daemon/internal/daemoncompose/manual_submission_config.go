package daemoncompose

import (
	"fmt"
	"maps"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

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
