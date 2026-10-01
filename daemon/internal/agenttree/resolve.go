package agenttree

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ResolvedAgent is an agent with the fragments its lines name, as admission
// needs them: the definition carries the digests, the fragments carry the
// facts the later steps check.
type ResolvedAgent struct {
	Definition domain.AgentDefinition
	Route      domain.RouteFragment
	Adapter    domain.AdapterFragment
	Offer      domain.OfferFragment
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
	return ResolvedAgent{
		Definition: definition, Route: closure.route, Adapter: closure.adapter, Offer: closure.offer,
	}, nil
}
