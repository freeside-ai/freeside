package agentbaseline

import (
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Prompt is the name and content digest a lineup line records for a role's
// prompt.
type Prompt struct {
	Name   string
	Digest domain.Digest
}

// TreeInput is what adoption knows that the code does not: the enrollments
// the two agents run under, the operator's dated attestations, the review
// model the review flags pin, and each role's prompt.
type TreeInput struct {
	// ClaudeEnrollment and CodexEnrollment are the adopted enrollments. An
	// agent's route name is its enrollment's route.
	ClaudeEnrollment domain.ClientEnrollment
	// CodexEnrollment is nil when the review identity could not be adopted;
	// the tree then carries no review agent and no reviewer line.
	CodexEnrollment *domain.ClientEnrollment
	// ReviewModel is the route model id of the Codex review offer.
	ReviewModel string
	// TermsBasisDate dates the operator's basis for both routes (YYYY-MM-DD).
	TermsBasisDate string
	// PricingRevision and OfferNotAfter are authored onto both offers.
	PricingRevision string
	OfferNotAfter   time.Time
	// Prompts maps each role that gets a line to its prompt. A role absent
	// from the map gets no line: shadow_reviewer is present only while the
	// shadow arm is on.
	Prompts map[domain.RoleName]Prompt
}

// wardRoleAgents fixes which baseline agent runs each ward role, in lineup
// order. The shadow reviewer is the Claude agent because the shadow arm runs
// under the implementation identity.
var wardRoleAgents = []struct {
	role  domain.RoleName
	codex bool
}{
	{domain.RoleSpecifier, false},
	{domain.RoleImplementer, false},
	{domain.RoleRemediator, false},
	{domain.RoleReviewer, true},
	{domain.RoleShadowReviewer, false},
}

// Tree builds the baseline tree: the fragments, the agents with their
// resolved enrollment ids, a lineup line for each role with a prompt, and
// the attended marks. A mark covers each launch the agent ran before the
// cutover, which is the operator's standing evidence that the pair ran
// attended; committing the patch is the operator making the mark.
func Tree(in TreeInput) (agenttree.Tree, error) {
	if in.ClaudeEnrollment.HarnessClient != domain.HarnessClientClaudeCode {
		return agenttree.Tree{}, errors.New("baseline tree: the Claude enrollment is not a claude_code enrollment")
	}
	claudeAdapter, err := ClaudeWardAdapter()
	if err != nil {
		return agenttree.Tree{}, err
	}
	claudeRoute, err := sealRoute(domain.RouteFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		ServiceOperator: "anthropic", Protocol: "anthropic_messages",
		InferenceAuthorities: []string{"api.anthropic.com:443"},
		BillingMode:          "subscription", FallbackPolicy: "fail_closed",
		TermsBasisDate: in.TermsBasisDate,
	})
	if err != nil {
		return agenttree.Tree{}, err
	}
	// Today's launch passes neither a model nor an effort, so the offer is
	// the native default at harness_default, and what model answers is
	// opaque to the daemon.
	claudeOffer, err := sealOffer(domain.OfferFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		RouteModelID:    domain.ClaudeCodeNativeDefaultRouteModelID, LineageGroup: "anthropic",
		IdentityStability: domain.IdentityOpaque,
		AllowedEfforts:    []domain.EffortLevel{domain.EffortHarnessDefault},
		PricingRevision:   in.PricingRevision, NotAfter: in.OfferNotAfter.UTC(),
	})
	if err != nil {
		return agenttree.Tree{}, err
	}
	tree := agenttree.Tree{
		Agents: []domain.AgentSource{{
			Name: ClaudeAgentName, Enrollment: string(in.ClaudeEnrollment.ID),
			Route: in.ClaudeEnrollment.Route, Adapter: ClaudeAdapterName,
			Offer: ClaudeOfferName, Effort: domain.EffortHarnessDefault,
		}},
		Routes:   []agenttree.Route{{Name: in.ClaudeEnrollment.Route, Fragment: claudeRoute}},
		Adapters: []agenttree.Adapter{{Name: ClaudeAdapterName, Fragment: claudeAdapter}},
		Offers: []agenttree.Offer{{
			Route: in.ClaudeEnrollment.Route, Name: ClaudeOfferName, Fragment: claudeOffer,
		}},
	}
	if in.CodexEnrollment != nil {
		if err := addCodexReview(&tree, in); err != nil {
			return agenttree.Tree{}, err
		}
	}
	digests := map[string]domain.Digest{}
	for _, agent := range tree.Agents {
		digest, err := tree.AgentDigest(agent.Name)
		if err != nil {
			return agenttree.Tree{}, err
		}
		digests[agent.Name] = digest
	}
	for _, entry := range wardRoleAgents {
		prompt, ok := in.Prompts[entry.role]
		if !ok {
			continue
		}
		agent := ClaudeAgentName
		if entry.codex {
			agent = CodexReviewAgentName
		}
		digest, ok := digests[agent]
		if !ok {
			return agenttree.Tree{}, fmt.Errorf("baseline tree: role %s has a prompt but agent %s was not adopted",
				entry.role, agent)
		}
		tree.Lineup = append(tree.Lineup, agenttree.LineupLine{
			Key: string(entry.role),
			Selection: domain.LineupSelection{
				AgentName: agent, AgentDigest: digest, PromptName: prompt.Name, PromptDigest: prompt.Digest,
			},
		})
		launch, err := RoleLaunch(entry.role)
		if err != nil {
			return agenttree.Tree{}, err
		}
		mark := agenttree.Mark{Agent: agent, AgentDigest: digest, LaunchDigest: launch.Digest}
		if !tree.Attended(agent, digest, launch.Digest) {
			continue // the implementer and remediator share one launch
		}
		tree.Marks = append(tree.Marks, mark)
	}
	// Tree slices are sorted by name, so the rendered files are stable.
	tree.Sort()
	if _, err := agenttree.Render(tree); err != nil {
		return agenttree.Tree{}, fmt.Errorf("baseline tree: %w", err)
	}
	return tree, nil
}

func addCodexReview(tree *agenttree.Tree, in TreeInput) error {
	enrollment := *in.CodexEnrollment
	if enrollment.HarnessClient != domain.HarnessClientCodexCLI {
		return errors.New("baseline tree: the Codex enrollment is not a codex_cli enrollment")
	}
	if enrollment.Route == in.ClaudeEnrollment.Route {
		return fmt.Errorf("baseline tree: both enrollments name route %q", enrollment.Route)
	}
	adapter, err := CodexReviewAdapter()
	if err != nil {
		return err
	}
	route, err := sealRoute(domain.RouteFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		ServiceOperator: "openai", Protocol: "chatgpt_backend",
		InferenceAuthorities: []string{"chatgpt.com:443"},
		BillingMode:          "subscription", FallbackPolicy: "fail_closed",
		TermsBasisDate: in.TermsBasisDate,
	})
	if err != nil {
		return err
	}
	offer, err := sealOffer(domain.OfferFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		RouteModelID:    in.ReviewModel, LineageGroup: "openai",
		IdentityStability: domain.IdentityPinned,
		AllowedEfforts:    []domain.EffortLevel{domain.EffortHarnessDefault},
		PricingRevision:   in.PricingRevision, NotAfter: in.OfferNotAfter.UTC(),
	})
	if err != nil {
		return err
	}
	tree.Agents = append(tree.Agents, domain.AgentSource{
		Name: CodexReviewAgentName, Enrollment: string(enrollment.ID),
		Route: enrollment.Route, Adapter: CodexAdapterName,
		Offer: CodexOfferName, Effort: domain.EffortHarnessDefault,
	})
	tree.Routes = append(tree.Routes, agenttree.Route{Name: enrollment.Route, Fragment: route})
	tree.Adapters = append(tree.Adapters, agenttree.Adapter{Name: CodexAdapterName, Fragment: adapter})
	tree.Offers = append(tree.Offers, agenttree.Offer{Route: enrollment.Route, Name: CodexOfferName, Fragment: offer})
	return nil
}

func sealRoute(route domain.RouteFragment) (domain.RouteFragment, error) {
	digest, err := route.ComputeDigest()
	if err != nil {
		return domain.RouteFragment{}, err
	}
	route.Digest = digest
	return route, route.Validate()
}

func sealOffer(offer domain.OfferFragment) (domain.OfferFragment, error) {
	digest, err := offer.ComputeDigest()
	if err != nil {
		return domain.OfferFragment{}, err
	}
	offer.Digest = digest
	return offer, offer.Validate()
}
