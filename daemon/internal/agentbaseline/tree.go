package agentbaseline

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Prompt is the name and content digest a lineup line records for a role's
// prompt.
type Prompt struct {
	Name   string
	Digest domain.Digest
}

// Selection is the model and effort level a role's lineup line names. The
// line records the effort the role's harness sends for that level
// (domain.TranslateEffort), so a Codex max is recorded as xhigh.
type Selection struct {
	Model  string
	Effort domain.EffortLevel
}

// TreeInput is what adoption knows that the code does not: the enrollments
// the roles run under, the operator's dated terms attestation, the model and
// effort each writer, review, and judgment role launches with, and each
// role's prompt.
type TreeInput struct {
	// ClaudeEnrollment and CodexEnrollment are the adopted enrollments. Each
	// enrollment's route is the route fragment the tree carries for it.
	ClaudeEnrollment domain.ClientEnrollment
	// CodexEnrollment is nil when the review identity could not be adopted;
	// the tree then carries no Codex route or adapter and no reviewer line.
	CodexEnrollment *domain.ClientEnrollment
	// ClaudeReviewEnrollment is the review identity's claude_code enrollment
	// when the reviewer runs Claude instead (#397). At most one of it and
	// CodexEnrollment is set: the reviewer line names one enrollment.
	ClaudeReviewEnrollment *domain.ClientEnrollment
	// Writers is the model and effort each writer role launches with, keyed
	// by specifier, implementer, and remediator. Every writer role needs one
	// that names a model and an effort: the native-default id leaves the
	// role on whatever the pinned CLI defaults to, which the operator can
	// neither see in the tree nor pin.
	Writers map[domain.RoleName]Selection
	// SpecifierEscalated is the model and effort of the specifier's
	// escalated counterpart (plan §5.13, Specifier Triage), or nil for none.
	// When set, the lineup carries an escalated line on the specifier's
	// enrollment naming it. It must differ from the specifier's own
	// selection, or escalation would change nothing.
	SpecifierEscalated *Selection
	// SecondClaudeEnrollment is a second writer subscription's claude_code
	// enrollment (#2653), or nil for none. Its writer roles get no role
	// lines, since the routing policy reaches it through task lines; with
	// SpecifierEscalated it gets a second escalated line, keyed by its own
	// enrollment. It must be another identity's enrollment on the first
	// one's route.
	SecondClaudeEnrollment *domain.ClientEnrollment
	// ReviewModel is the reviewer's model id, and ReviewEffort the effort it
	// asks. A review launch always passes both, so a tree with a review
	// enrollment needs a model and an effort other than harness_default.
	ReviewModel  string
	ReviewEffort domain.EffortLevel
	// ShadowReviewModel and ShadowReviewEffort are the same two values for
	// the shadow reviewer, needed exactly when Prompts names
	// shadow_reviewer.
	ShadowReviewModel  string
	ShadowReviewEffort domain.EffortLevel
	// JudgmentModel is the exact model every baseline judgment line launches
	// with. Empty leaves the call adapter out of the tree, so no judgment
	// role can have a line. JudgmentEffort is the effort every judgment call
	// asks, needed exactly when JudgmentModel is set.
	JudgmentModel  string
	JudgmentEffort domain.EffortLevel
	// TermsBasisDate dates the operator's basis for both routes (YYYY-MM-DD).
	TermsBasisDate string
	// Prompts maps each role that gets a line to its prompt. A role absent
	// from the map gets no line: shadow_reviewer is present only while the
	// shadow arm is on, and publication_author only when the operator names
	// its prompt file.
	Prompts map[domain.RoleName]Prompt
}

// WriterRoles are the ward roles that run the Claude writer launches, in
// lineup order. Each takes its model and effort from TreeInput.Writers.
var WriterRoles = []domain.RoleName{domain.RoleSpecifier, domain.RoleImplementer, domain.RoleRemediator}

// JudgmentRoles returns the wardless roles with a built judgment site, in
// role order: the roles a baseline lineup can name on the call adapter. The
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

// Tree builds the baseline tree: the routes and adapters the adopted
// enrollments need, and a lineup line for each role with a prompt, in role
// order: the writer roles (the specifier's escalated lines right after its
// own), the review roles, then the judgment roles.
func Tree(in TreeInput) (agenttree.Tree, error) {
	if in.ClaudeEnrollment.HarnessClient != domain.HarnessClientClaudeCode {
		return agenttree.Tree{}, errors.New("baseline tree: the Claude enrollment is not a claude_code enrollment")
	}
	if err := checkSecondClaudeEnrollment(in); err != nil {
		return agenttree.Tree{}, err
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
	roles := map[domain.RoleName]domain.AgentSelectionNames{}
	if err := writerSelections(roles, in, in.ClaudeEnrollment); err != nil {
		return agenttree.Tree{}, err
	}
	// The second identity's writer selections are checked like the first's
	// but get no role line; only its escalated line is written.
	if in.SecondClaudeEnrollment != nil {
		if err := writerSelections(map[domain.RoleName]domain.AgentSelectionNames{}, in, *in.SecondClaudeEnrollment); err != nil {
			return agenttree.Tree{}, err
		}
	}
	var escalated []domain.AgentSelectionNames
	if in.SpecifierEscalated != nil {
		if _, ok := in.Prompts[domain.RoleSpecifier]; !ok {
			return agenttree.Tree{}, errors.New("baseline tree: an escalated specifier needs the specifier's prompt")
		}
		for _, enrollment := range []*domain.ClientEnrollment{&in.ClaudeEnrollment, in.SecondClaudeEnrollment} {
			if enrollment == nil {
				continue
			}
			names, err := escalatedSelection(in, *enrollment)
			if err != nil {
				return agenttree.Tree{}, err
			}
			escalated = append(escalated, names)
		}
	}
	switch {
	case in.CodexEnrollment != nil && in.ClaudeReviewEnrollment != nil:
		return agenttree.Tree{}, errors.New("baseline tree: the review identity has both a Codex and a Claude enrollment")
	case in.CodexEnrollment != nil:
		if err := addCodexReview(&tree, roles, in); err != nil {
			return agenttree.Tree{}, err
		}
	case in.ClaudeReviewEnrollment != nil:
		if err := addClaudeReview(&tree, roles, in, claudeRoute); err != nil {
			return agenttree.Tree{}, err
		}
	}
	if _, ok := in.Prompts[domain.RoleShadowReviewer]; ok {
		names, err := claudeSelection("shadow review", in.ClaudeEnrollment, in.ShadowReviewModel, in.ShadowReviewEffort)
		if err != nil {
			return agenttree.Tree{}, err
		}
		roles[domain.RoleShadowReviewer] = names
	}
	var judgment *domain.AgentSelectionNames
	if in.JudgmentModel != "" {
		names, err := addClaudeCall(&tree, in)
		if err != nil {
			return agenttree.Tree{}, err
		}
		judgment = &names
	}
	wardRoles := append(slices.Clone(WriterRoles), domain.RoleReviewer, domain.RoleShadowReviewer)
	for _, role := range wardRoles {
		prompt, ok := in.Prompts[role]
		if !ok {
			continue
		}
		names, ok := roles[role]
		if !ok {
			return agenttree.Tree{}, fmt.Errorf("baseline tree: role %s has a prompt but no review enrollment was adopted",
				role)
		}
		tree.Lineup = append(tree.Lineup, lineupLine(string(role), names, prompt))
		if role != domain.RoleSpecifier {
			continue
		}
		for _, names := range escalated {
			key, err := domain.LineupEscalatedKey(domain.RoleSpecifier, names.Enrollment)
			if err != nil {
				return agenttree.Tree{}, err
			}
			tree.Lineup = append(tree.Lineup, lineupLine(strings.TrimPrefix(key, domain.LineupRoleKeyPrefix), names, prompt))
		}
	}
	for _, role := range JudgmentRoles() {
		prompt, ok := in.Prompts[role]
		if !ok {
			continue
		}
		if judgment == nil {
			return agenttree.Tree{}, fmt.Errorf("baseline tree: role %s has a prompt but no judgment model was given", role)
		}
		tree.Lineup = append(tree.Lineup, lineupLine(string(role), *judgment, prompt))
	}
	// Tree slices are sorted by name, so the rendered files are stable.
	tree.Sort()
	if _, err := agenttree.Render(tree); err != nil {
		return agenttree.Tree{}, fmt.Errorf("baseline tree: %w", err)
	}
	return tree, nil
}

func lineupLine(key string, names domain.AgentSelectionNames, prompt Prompt) agenttree.LineupLine {
	return agenttree.LineupLine{
		Key:       key,
		Selection: domain.LineupSelection{Names: names, PromptName: prompt.Name, PromptDigest: prompt.Digest},
	}
}

// checkSecondClaudeEnrollment refuses a second writer enrollment that is not
// another identity's claude_code enrollment on the first one's route. One
// identity under both would be one usage pool under two names, and adoption
// gives every Claude enrollment one route.
func checkSecondClaudeEnrollment(in TreeInput) error {
	second := in.SecondClaudeEnrollment
	switch {
	case second == nil:
		return nil
	case second.HarnessClient != domain.HarnessClientClaudeCode:
		return errors.New("baseline tree: the second Claude enrollment is not a claude_code enrollment")
	case second.ID == in.ClaudeEnrollment.ID || second.AuthIdentityID == in.ClaudeEnrollment.AuthIdentityID:
		return fmt.Errorf("baseline tree: the second Claude enrollment %s is the first writer's identity", second.ID)
	case second.Route != in.ClaudeEnrollment.Route:
		return fmt.Errorf("baseline tree: the second Claude enrollment names route %q, not the writer's %q",
			second.Route, in.ClaudeEnrollment.Route)
	}
	return nil
}

// writerSelections records, for each writer role, the selection its line
// names on the enrollment. The ward launch passes the model and effort
// (#1618), so a selection that names neither is refused here rather than
// written as a line the daemon then runs on the CLI's default.
func writerSelections(
	roles map[domain.RoleName]domain.AgentSelectionNames, in TreeInput, enrollment domain.ClientEnrollment,
) error {
	for _, role := range WriterRoles {
		selection := in.Writers[role]
		names, err := claudeSelection("writer role "+string(role), enrollment, selection.Model, selection.Effort)
		if err != nil {
			return err
		}
		roles[role] = names
	}
	return nil
}

// escalatedSelection is the specifier's escalated counterpart on the
// enrollment. It shares the specifier's enrollment, as the pairing rule
// requires: escalation changes the model and effort, never the credential.
func escalatedSelection(in TreeInput, enrollment domain.ClientEnrollment) (domain.AgentSelectionNames, error) {
	selection := *in.SpecifierEscalated
	if selection == in.Writers[domain.RoleSpecifier] {
		return domain.AgentSelectionNames{}, fmt.Errorf(
			"baseline tree: escalated specifier %s at %s is the specifier's own selection",
			selection.Model, selection.Effort)
	}
	return claudeSelection("escalated specifier", enrollment, selection.Model, selection.Effort)
}

// claudeSelection is selectionNames on a claude_code enrollment.
func claudeSelection(
	role string, enrollment domain.ClientEnrollment, model string, effort domain.EffortLevel,
) (domain.AgentSelectionNames, error) {
	return selectionNames(role, enrollment, domain.HarnessClientClaudeCode, model, effort)
}

// selectionNames is the selection a line names for a role on the
// enrollment: the model as given and the effort the client sends for the
// level. A baseline launch always passes both, so the model must be one and
// not the native-default id, and the effort must be explicit.
func selectionNames(
	role string, enrollment domain.ClientEnrollment, client domain.HarnessClientKind,
	model string, effort domain.EffortLevel,
) (domain.AgentSelectionNames, error) {
	if model == "" || model == domain.ClaudeCodeNativeDefaultRouteModelID {
		return domain.AgentSelectionNames{}, fmt.Errorf("baseline tree: %s model %q names no model", role, model)
	}
	if err := explicitEffort(role, effort); err != nil {
		return domain.AgentSelectionNames{}, err
	}
	translated, err := domain.TranslateEffort(client, effort)
	if err != nil {
		return domain.AgentSelectionNames{}, fmt.Errorf("baseline tree: %s: %w", role, err)
	}
	return domain.AgentSelectionNames{
		Enrollment: string(enrollment.ID), Model: model, Effort: translated.Effective(),
	}, nil
}

// addClaudeCall adds the call adapter every judgment line runs on and
// returns the selection those lines name. The call shares the writer's
// enrollment: the same subscription answers a call through a launch with no
// tools. The operator's model is taken as exact, like the review model; the
// driver refuses an answer that reports another model.
func addClaudeCall(tree *agenttree.Tree, in TreeInput) (domain.AgentSelectionNames, error) {
	names, err := claudeSelection("judgment", in.ClaudeEnrollment, in.JudgmentModel, in.JudgmentEffort)
	if err != nil {
		return domain.AgentSelectionNames{}, err
	}
	adapter, err := ClaudeCallAdapter()
	if err != nil {
		return domain.AgentSelectionNames{}, err
	}
	tree.Adapters = append(tree.Adapters, agenttree.Adapter{Name: ClaudeCallAdapterName, Fragment: adapter})
	return names, nil
}

// addClaudeReview records the routed reviewer's selection when it runs
// Claude (#397), on the review identity's claude_code enrollment and the
// Claude ward adapter. A review enrollment on another route than the
// writer's gets that route; the route fragment is the same Anthropic
// subscription route.
func addClaudeReview(
	tree *agenttree.Tree, roles map[domain.RoleName]domain.AgentSelectionNames, in TreeInput,
	route domain.RouteFragment,
) error {
	enrollment := *in.ClaudeReviewEnrollment
	if enrollment.HarnessClient != domain.HarnessClientClaudeCode {
		return errors.New("baseline tree: the Claude review enrollment is not a claude_code enrollment")
	}
	names, err := claudeSelection("review", enrollment, in.ReviewModel, in.ReviewEffort)
	if err != nil {
		return err
	}
	if enrollment.Route != in.ClaudeEnrollment.Route {
		tree.Routes = append(tree.Routes, agenttree.Route{Name: enrollment.Route, Fragment: route})
	}
	roles[domain.RoleReviewer] = names
	return nil
}

// addCodexReview adds the Codex route and review adapter and records the
// reviewer's selection on the Codex enrollment.
func addCodexReview(tree *agenttree.Tree, roles map[domain.RoleName]domain.AgentSelectionNames, in TreeInput) error {
	enrollment := *in.CodexEnrollment
	if enrollment.HarnessClient != domain.HarnessClientCodexCLI {
		return errors.New("baseline tree: the Codex enrollment is not a codex_cli enrollment")
	}
	if enrollment.Route == in.ClaudeEnrollment.Route {
		return fmt.Errorf("baseline tree: both enrollments name route %q", enrollment.Route)
	}
	names, err := selectionNames("review", enrollment, domain.HarnessClientCodexCLI, in.ReviewModel, in.ReviewEffort)
	if err != nil {
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
	tree.Routes = append(tree.Routes, agenttree.Route{Name: enrollment.Route, Fragment: route})
	tree.Adapters = append(tree.Adapters, agenttree.Adapter{Name: CodexAdapterName, Fragment: adapter})
	roles[domain.RoleReviewer] = names
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

func sealRoute(route domain.RouteFragment) (domain.RouteFragment, error) {
	digest, err := route.ComputeDigest()
	if err != nil {
		return domain.RouteFragment{}, err
	}
	route.Digest = digest
	return route, route.Validate()
}
