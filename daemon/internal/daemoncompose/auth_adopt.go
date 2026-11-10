package daemoncompose

import (
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// ValidReviewLaunchModel reports whether a review launch can pass the model
// as one command-line argument: the shape every launch value has, and the
// review launch's own rule. Adoption holds its flags to it and the daemon
// holds a review role's line to it (ReviewRoleAgent), so a tree adoption
// wrote is never refused for its model's shape.
func ValidReviewLaunchModel(model string) bool {
	return inference.ValidLaunchValue(model) &&
		ward.ValidateCodexReviewModelConfiguration(model, string(domain.EffortHigh)) == nil
}
