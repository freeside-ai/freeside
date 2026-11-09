package agenttree

import "github.com/freeside-ai/freeside/daemon/internal/domain"

// AdapterForLaunch is the tree's adapter for the client kind that sends the
// native effort and whose declared capabilities cover the stage launch, less
// the waived capabilities; every rule is domain.SelectAdapterForLaunch's.
func (t Tree) AdapterForLaunch(
	client domain.HarnessClientKind, launch domain.LaunchSpec, effort string, waived ...domain.LaunchCapability,
) (domain.AdapterFragment, error) {
	adapters := make([]domain.AdapterFragment, 0, len(t.Adapters))
	for _, adapter := range t.Adapters {
		adapters = append(adapters, adapter.Fragment)
	}
	return domain.SelectAdapterForLaunch(adapters, client, launch, effort, waived...)
}
