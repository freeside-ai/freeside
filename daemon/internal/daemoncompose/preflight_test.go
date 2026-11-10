package daemoncompose

import (
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// mustRoutedReviewProvider is the routed review provider a fixture reviewer
// line's harness selects.
func mustRoutedReviewProvider(client domain.HarnessClientKind) ward.RoutedReviewProvider {
	provider, ok := ward.LookupRoutedReviewProvider(client)
	if !ok {
		panic("no routed review provider for " + string(client))
	}
	return provider
}
