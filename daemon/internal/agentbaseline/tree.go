package agentbaseline

import (
	"errors"
	"fmt"
	"slices"
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

// Selection is the model and effort a role's agent launches with: the route
// model id its offer names and the effort it asks.
type Selection struct {
	Model  string
	Effort domain.EffortLevel
}

// TreeInput is what adoption knows that the code does not: the enrollments
// the agents run under, the operator's dated attestations, the model and
// effort each writer, review, and judgment role launches with, and each
// role's prompt.
type TreeInput struct {
	// ClaudeEnrollment and CodexEnrollment are the adopted enrollments. An
	// agent's route name is its enrollment's route.
	ClaudeEnrollment domain.ClientEnrollment
	// CodexEnrollment is nil when the review identity could not be adopted;
	// the tree then carries no review agent and no reviewer line.
	CodexEnrollment *domain.ClientEnrollment
	// ClaudeReviewEnrollment is the review identity's claude_code enrollment
	// when the reviewer runs Claude instead (#397). At most one of it and
	// CodexEnrollment is set: the reviewer line names one agent.
	ClaudeReviewEnrollment *domain.ClientEnrollment
	// Writers is the model and effort each writer role's agent launches
	// with, keyed by specifier, implementer, and remediator. Every writer
	// role needs one that names a model and an effort: an offer on the
	// native-default id leaves the role on whatever the pinned CLI defaults
	// to, which the operator can neither see in the tree nor pin.
	Writers map[domain.RoleName]Selection
	// ReviewModel is the route model id of the review offer, and
	// ReviewEffort the effort the review agent asks. A review launch always
	// passes both, so a tree with a review enrollment needs an effort other
	// than harness_default.
	ReviewModel  string
	ReviewEffort domain.EffortLevel
	// ShadowReviewModel and ShadowReviewEffort are the same two values for
	// the shadow reviewer's agent, needed exactly when Prompts names
	// shadow_reviewer.
	ShadowReviewModel  string
	ShadowReviewEffort domain.EffortLevel
	// JudgmentModel is the route model id of the call offer: the exact model
	// every baseline judgment line launches with. Empty leaves the call agent,
	// its offer, and its adapter out of the tree, so no judgment role can have
	// a line. JudgmentEffort is the effort every judgment call asks, needed
	// exactly when JudgmentModel is set.
	JudgmentModel  string
	JudgmentEffort domain.EffortLevel
	// TermsBasisDate dates the operator's basis for both routes (YYYY-MM-DD).
	TermsBasisDate string
	// PricingRevision and OfferNotAfter are authored onto every offer.
	PricingRevision string
	OfferNotAfter   time.Time
	// Prompts maps each role that gets a line to its prompt. A role absent
	// from the map gets no line: shadow_reviewer is present only while the
	// shadow arm is on, and publication_author only when the operator names
	// its prompt file.
	Prompts map[domain.RoleName]Prompt
}

// WriterRoles are the ward roles that run the Claude writer launches, in
// lineup order. Each takes its model and effort from TreeInput.Writers.
var WriterRoles = []domain.RoleName{domain.RoleSpecifier, domain.RoleImplementer, domain.RoleRemediator}

// WriterAgentName is the name of the writer agent a role's selection
// creates. Writer roles that ask one model and effort share one agent, named
// for the first of them in WriterRoles order, so the tree carries one agent
// per distinct pair and the roles' lines point at it.
func WriterAgentName(role domain.RoleName) string {
	return "claude-code-" + string(role)
}

// reviewRoleAgents fixes which baseline agent runs each review role, in
// lineup order after the writer roles. The shadow reviewer has its own agent
// on the Claude enrollment: the shadow arm runs under the implementation
// identity, on an offer of its own. The reviewer's agent is the Codex one
// unless the tree's review enrollment is Claude (reviewerAgent).
var reviewRoleAgents = []struct {
	role  domain.RoleName
	agent string
}{
	{domain.RoleReviewer, CodexReviewAgentName},
	{domain.RoleShadowReviewer, ClaudeShadowReviewAgentName},
}

// JudgmentRoles returns the wardless roles with a built judgment site, in
// role order: the roles a baseline lineup can name on the call agent. The
// briefer has no site yet, so it has no line.
func JudgmentRoles() []domain.RoleName {
	var roles []domain.RoleName
	for _, role := range domain.AllRoleNames {
		if len(role.Sites()) > 0 {
			roles = append(roles, role)
		}
	}
	return roles
}

// Tree builds the baseline tree: the fragments, the agents with their
// resolved enrollment ids, a lineup line for each role with a prompt, and
// the attended marks. A mark is the operator's standing attestation that the
// agent and launch pair runs attended under their watch; committing the
// patch is the operator making the mark. The call agent gets no mark:
// attended evidence gates unattended ward launches, and a call is admitted
// by its launch proof.
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
	tree := agenttree.Tree{
		Routes:   []agenttree.Route{{Name: in.ClaudeEnrollment.Route, Fragment: claudeRoute}},
		Adapters: []agenttree.Adapter{{Name: ClaudeAdapterName, Fragment: claudeAdapter}},
	}
	wardRoleAgents, err := addClaudeWriters(&tree, in)
	if err != nil {
		return agenttree.Tree{}, err
	}
	for _, entry := range reviewRoleAgents {
		wardRoleAgents = append(wardRoleAgents, roleAgent{entry.role, entry.agent})
	}
	if in.JudgmentModel != "" {
		if err := addClaudeCall(&tree, in); err != nil {
			return agenttree.Tree{}, err
		}
	}
	switch {
	case in.CodexEnrollment != nil && in.ClaudeReviewEnrollment != nil:
		return agenttree.Tree{}, errors.New("baseline tree: the review identity has both a Codex and a Claude enrollment")
	case in.CodexEnrollment != nil:
		if err := addCodexReview(&tree, in); err != nil {
			return agenttree.Tree{}, err
		}
	case in.ClaudeReviewEnrollment != nil:
		if err := addClaudeReview(&tree, in, claudeRoute); err != nil {
			return agenttree.Tree{}, err
		}
	}
	if _, ok := in.Prompts[domain.RoleShadowReviewer]; ok {
		if err := addClaudeShadowReview(&tree, in); err != nil {
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
		agent := entry.agent
		if entry.role == domain.RoleReviewer {
			agent = reviewerAgent(in)
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
	for _, role := range JudgmentRoles() {
		prompt, ok := in.Prompts[role]
		if !ok {
			continue
		}
		digest, ok := digests[ClaudeCallAgentName]
		if !ok {
			return agenttree.Tree{}, fmt.Errorf(
				"baseline tree: role %s has a prompt but agent %s was not adopted: it needs a judgment model",
				role, ClaudeCallAgentName)
		}
		tree.Lineup = append(tree.Lineup, agenttree.LineupLine{
			Key: string(role),
			Selection: domain.LineupSelection{
				AgentName: ClaudeCallAgentName, AgentDigest: digest,
				PromptName: prompt.Name, PromptDigest: prompt.Digest,
			},
		})
	}
	// Tree slices are sorted by name, so the rendered files are stable.
	tree.Sort()
	if _, err := agenttree.Render(tree); err != nil {
		return agenttree.Tree{}, fmt.Errorf("baseline tree: %w", err)
	}
	return tree, nil
}

// roleAgent is one ward role and the name of the agent its lineup line names.
type roleAgent struct {
	role  domain.RoleName
	agent string
}

// addClaudeWriters adds one writer agent per distinct model and effort the
// writer roles ask, each on the Claude ward adapter and a pinned offer of its
// own, and returns which agent each writer role's line names. The ward
// launch passes the offer's model and the agent's effort (#1618), so a
// selection that names neither is refused here rather than written as a
// line the daemon then runs on the CLI's default.
func addClaudeWriters(tree *agenttree.Tree, in TreeInput) ([]roleAgent, error) {
	agents := map[Selection]string{}
	var lines []roleAgent
	for _, role := range WriterRoles {
		selection, ok := in.Writers[role]
		if !ok || selection.Model == "" || selection.Model == domain.ClaudeCodeNativeDefaultRouteModelID {
			return nil, fmt.Errorf("baseline tree: writer role %s model %q names no model", role, selection.Model)
		}
		if err := explicitEffort(string(role), selection.Effort); err != nil {
			return nil, err
		}
		if name, ok := agents[selection]; ok {
			lines = append(lines, roleAgent{role, name})
			continue
		}
		name := WriterAgentName(role)
		offer, err := sealOffer(domain.OfferFragment{
			EncodingVersion: domain.AgentFragmentEncodingVersion,
			RouteModelID:    selection.Model, LineageGroup: "anthropic",
			IdentityStability: domain.IdentityPinned,
			AllowedEfforts:    []domain.EffortLevel{selection.Effort},
			PricingRevision:   in.PricingRevision, NotAfter: in.OfferNotAfter.UTC(),
		})
		if err != nil {
			return nil, err
		}
		tree.Agents = append(tree.Agents, domain.AgentSource{
			Name: name, Enrollment: string(in.ClaudeEnrollment.ID),
			Route: in.ClaudeEnrollment.Route, Adapter: ClaudeAdapterName,
			Offer: name, Effort: selection.Effort,
		})
		tree.Offers = append(tree.Offers, agenttree.Offer{Route: in.ClaudeEnrollment.Route, Name: name, Fragment: offer})
		agents[selection] = name
		lines = append(lines, roleAgent{role, name})
	}
	return lines, nil
}

// addClaudeCall adds the call agent with the offer and adapter only it
// names. The call agent shares the writer agents' enrollment and route: the
// same subscription answers a call through a launch with no tools. Its offer
// is its own because every judgment line launches with one model and asks
// one effort, whatever the writer roles ask. The operator's model is taken
// as exact, like the review model, so the offer is pinned; the driver
// refuses an answer that reports another model.
func addClaudeCall(tree *agenttree.Tree, in TreeInput) error {
	if in.JudgmentModel == domain.ClaudeCodeNativeDefaultRouteModelID {
		return fmt.Errorf("baseline tree: judgment model %q is the native-default id, which names no model",
			in.JudgmentModel)
	}
	if err := explicitEffort("judgment", in.JudgmentEffort); err != nil {
		return err
	}
	adapter, err := ClaudeCallAdapter()
	if err != nil {
		return err
	}
	offer, err := sealOffer(domain.OfferFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		RouteModelID:    in.JudgmentModel, LineageGroup: "anthropic",
		IdentityStability: domain.IdentityPinned,
		AllowedEfforts:    []domain.EffortLevel{in.JudgmentEffort},
		PricingRevision:   in.PricingRevision, NotAfter: in.OfferNotAfter.UTC(),
	})
	if err != nil {
		return err
	}
	tree.Agents = append(tree.Agents, domain.AgentSource{
		Name: ClaudeCallAgentName, Enrollment: string(in.ClaudeEnrollment.ID),
		Route: in.ClaudeEnrollment.Route, Adapter: ClaudeCallAdapterName,
		Offer: ClaudeCallOfferName, Effort: in.JudgmentEffort,
	})
	tree.Adapters = append(tree.Adapters, agenttree.Adapter{Name: ClaudeCallAdapterName, Fragment: adapter})
	tree.Offers = append(tree.Offers, agenttree.Offer{
		Route: in.ClaudeEnrollment.Route, Name: ClaudeCallOfferName, Fragment: offer,
	})
	return nil
}

// addClaudeShadowReview adds the shadow reviewer's agent and the offer only
// it names. The agent shares the ward agent's enrollment, route, and adapter:
// the shadow review is a ward launch under the implementation identity. Its
// offer is its own because that launch always passes a model and an effort,
// and the native-default offer names neither.
func addClaudeShadowReview(tree *agenttree.Tree, in TreeInput) error {
	if in.ShadowReviewModel == "" || in.ShadowReviewModel == domain.ClaudeCodeNativeDefaultRouteModelID {
		return fmt.Errorf("baseline tree: shadow review model %q names no model", in.ShadowReviewModel)
	}
	if err := explicitEffort("shadow review", in.ShadowReviewEffort); err != nil {
		return err
	}
	offer, err := sealOffer(domain.OfferFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		RouteModelID:    in.ShadowReviewModel, LineageGroup: "anthropic",
		IdentityStability: domain.IdentityPinned,
		AllowedEfforts:    []domain.EffortLevel{in.ShadowReviewEffort},
		PricingRevision:   in.PricingRevision, NotAfter: in.OfferNotAfter.UTC(),
	})
	if err != nil {
		return err
	}
	tree.Agents = append(tree.Agents, domain.AgentSource{
		Name: ClaudeShadowReviewAgentName, Enrollment: string(in.ClaudeEnrollment.ID),
		Route: in.ClaudeEnrollment.Route, Adapter: ClaudeAdapterName,
		Offer: ClaudeShadowReviewOfferName, Effort: in.ShadowReviewEffort,
	})
	tree.Offers = append(tree.Offers, agenttree.Offer{
		Route: in.ClaudeEnrollment.Route, Name: ClaudeShadowReviewOfferName, Fragment: offer,
	})
	return nil
}

// reviewerAgent names the agent the reviewer line selects: the Claude review
// agent when the review identity is adopted on claude_code, else the Codex one.
func reviewerAgent(in TreeInput) string {
	if in.ClaudeReviewEnrollment != nil {
		return ClaudeReviewAgentName
	}
	return CodexReviewAgentName
}

// addClaudeReview adds the routed reviewer's agent when it runs Claude
// (#397): an agent on the review identity's claude_code enrollment and the
// Claude ward adapter, with an offer of its own that names the review model
// and effort, as the shadow reviewer's does. A review enrollment on another
// route than the writer's gets that route; the route fragment is the same
// Anthropic subscription route.
func addClaudeReview(tree *agenttree.Tree, in TreeInput, route domain.RouteFragment) error {
	enrollment := *in.ClaudeReviewEnrollment
	if enrollment.HarnessClient != domain.HarnessClientClaudeCode {
		return errors.New("baseline tree: the Claude review enrollment is not a claude_code enrollment")
	}
	if in.ReviewModel == "" || in.ReviewModel == domain.ClaudeCodeNativeDefaultRouteModelID {
		return fmt.Errorf("baseline tree: review model %q names no model", in.ReviewModel)
	}
	if err := explicitEffort("review", in.ReviewEffort); err != nil {
		return err
	}
	offer, err := sealOffer(domain.OfferFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		RouteModelID:    in.ReviewModel, LineageGroup: "anthropic",
		IdentityStability: domain.IdentityPinned,
		AllowedEfforts:    []domain.EffortLevel{in.ReviewEffort},
		PricingRevision:   in.PricingRevision, NotAfter: in.OfferNotAfter.UTC(),
	})
	if err != nil {
		return err
	}
	if enrollment.Route != in.ClaudeEnrollment.Route {
		tree.Routes = append(tree.Routes, agenttree.Route{Name: enrollment.Route, Fragment: route})
	}
	tree.Agents = append(tree.Agents, domain.AgentSource{
		Name: ClaudeReviewAgentName, Enrollment: string(enrollment.ID),
		Route: enrollment.Route, Adapter: ClaudeAdapterName,
		Offer: ClaudeReviewOfferName, Effort: in.ReviewEffort,
	})
	tree.Offers = append(tree.Offers, agenttree.Offer{
		Route: enrollment.Route, Name: ClaudeReviewOfferName, Fragment: offer,
	})
	return nil
}

// explicitEffort refuses the efforts a baseline launch must not run on:
// none, an effort the level set does not know, and harness_default, which
// sends none and leaves the pinned CLI's default in charge.
func explicitEffort(role string, effort domain.EffortLevel) error {
	if effort == "" || effort == domain.EffortHarnessDefault || !slices.Contains(domain.AllEffortLevels, effort) {
		return fmt.Errorf("baseline tree: %s effort %q names no effort for the launch to pass", role, effort)
	}
	return nil
}

func addCodexReview(tree *agenttree.Tree, in TreeInput) error {
	enrollment := *in.CodexEnrollment
	if enrollment.HarnessClient != domain.HarnessClientCodexCLI {
		return errors.New("baseline tree: the Codex enrollment is not a codex_cli enrollment")
	}
	if enrollment.Route == in.ClaudeEnrollment.Route {
		return fmt.Errorf("baseline tree: both enrollments name route %q", enrollment.Route)
	}
	if err := explicitEffort("review", in.ReviewEffort); err != nil {
		return err
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
		AllowedEfforts:    []domain.EffortLevel{in.ReviewEffort},
		PricingRevision:   in.PricingRevision, NotAfter: in.OfferNotAfter.UTC(),
	})
	if err != nil {
		return err
	}
	tree.Agents = append(tree.Agents, domain.AgentSource{
		Name: CodexReviewAgentName, Enrollment: string(enrollment.ID),
		Route: enrollment.Route, Adapter: CodexAdapterName,
		Offer: CodexOfferName, Effort: in.ReviewEffort,
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
