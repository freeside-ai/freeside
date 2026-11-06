package agenttree

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ResolvedAgent is an agent with the fragments its lines name, as admission
// needs them: the definition carries the digests, the fragments carry the
// facts the later steps check, and Selection is the enrollment, model,
// effort, and adapter the agent reduces to (domain.ReduceAgentSelection),
// which stage admission holds the resolved selection to.
type ResolvedAgent struct {
	Definition domain.AgentDefinition
	Route      domain.RouteFragment
	Adapter    domain.AdapterFragment
	Offer      domain.OfferFragment
	Selection  domain.AgentSelection
}

// ResolveAgent is admission step 1 for one agent: the tree supplies the
// lines and fragments, the caller supplies the enrollment the "who" line
// names and that enrollment's identity as the store holds them now. Every
// join rule is domain.ResolveAgentDefinition's.
func (t Tree) ResolveAgent(
	name string, enrollment domain.ClientEnrollment, identity domain.AuthIdentity,
) (ResolvedAgent, error) {
	source, ok := t.Agent(name)
	if !ok {
		return ResolvedAgent{}, fmt.Errorf("agent %q: %w", name, ErrUnknownAgent)
	}
	closure, err := t.closure(source)
	if err != nil {
		return ResolvedAgent{}, err
	}
	if string(enrollment.ID) != source.Enrollment {
		return ResolvedAgent{}, fmt.Errorf("agent %s names enrollment %q, was given %q: %w",
			name, source.Enrollment, enrollment.ID, domain.ErrAgentJoinInvalid)
	}
	definition, err := domain.ResolveAgentDefinition(domain.AgentResolutionInput{
		Source: source, Identity: identity, Enrollment: enrollment,
		Route: closure.route, Adapter: closure.adapter, Offer: closure.offer,
		OfferRoute: source.Route,
	})
	if err != nil {
		return ResolvedAgent{}, err
	}
	selection, err := domain.ReduceAgentSelection(definition, closure.adapter, closure.offer)
	if err != nil {
		return ResolvedAgent{}, err
	}
	return ResolvedAgent{
		Definition: definition, Route: closure.route, Adapter: closure.adapter, Offer: closure.offer,
		Selection: selection,
	}, nil
}

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
