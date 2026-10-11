package agentbaseline_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func claudeRecords() (domain.ClientEnrollment, domain.AuthIdentity) {
	return domain.ClientEnrollment{
			ID: "claude-a/claude_code", AuthIdentityID: "claude-a",
			HarnessClient: domain.HarnessClientClaudeCode, Route: agentbaseline.DefaultClaudeRoute,
			AuthMethod: domain.AuthMethodSetupToken, CredentialMode: domain.CredentialSubscriptionContained,
			RefreshStrategy: domain.RefreshExternal, SupportsReadOnlyAuthSnapshot: true,
			AccountBinding: "operator@example.com",
		}, domain.AuthIdentity{
			ID: "claude-a", Provider: "claude", AuthStoreMutationLease: true,
			MaxParallelExecutions: 1, Enabled: true, AccountBinding: "operator@example.com", CostOwner: "operator",
			Interim: domain.InterimClientFacts{AuthStoreVolume: "claude-cred", RefreshStrategy: domain.RefreshOnDemand},
		}
}

func codexRecords() (domain.ClientEnrollment, domain.AuthIdentity) {
	return domain.ClientEnrollment{
			ID: "codex-a/codex_cli", AuthIdentityID: "codex-a",
			HarnessClient: domain.HarnessClientCodexCLI, Route: agentbaseline.DefaultCodexRoute,
			AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
			RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
			AccountBinding: "acct-1",
		}, domain.AuthIdentity{
			ID: "codex-a", Provider: "openai", AuthStoreMutationLease: true,
			MaxParallelExecutions: 1, Enabled: true, AccountBinding: "acct-1", CostOwner: "operator",
			Interim: domain.InterimClientFacts{
				AuthStoreVolume: "/private/auth.json", RefreshStrategy: domain.RefreshOnDemand,
				SupportsReadOnlyAuthSnapshot: true,
			},
		}
}

func prompt(name string) agentbaseline.Prompt {
	return agentbaseline.Prompt{Name: name, Digest: domain.Digest(contentaddr.Sum([]byte(name)))}
}

func input(shadow bool) agentbaseline.TreeInput {
	claude, _ := claudeRecords()
	codex, _ := codexRecords()
	in := agentbaseline.TreeInput{
		ClaudeEnrollment: claude, CodexEnrollment: &codex, ReviewModel: "gpt-5.6-sol",
		TermsBasisDate: "2026-10-01", PricingRevision: "2026-10",
		OfferNotAfter: time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC),
		Prompts: map[domain.RoleName]agentbaseline.Prompt{
			domain.RoleSpecifier: prompt("specifier"), domain.RoleImplementer: prompt("implementer"),
			domain.RoleRemediator: prompt("remediator"), domain.RoleReviewer: prompt("review"),
		},
	}
	if shadow {
		in.Prompts[domain.RoleShadowReviewer] = prompt("shadow-review")
	}
	return in
}

// The tree adoption proposes survives its own round trip and resolves both
// baseline agents against the adopted enrollments.
func TestBaselineTreeRoundTripsAndResolves(t *testing.T) {
	tree := must(agentbaseline.Tree(input(true)))
	parsed := must(agenttree.Parse(must(agenttree.Render(tree))))
	if !reflect.DeepEqual(parsed, tree) {
		t.Fatalf("baseline tree changed across its round trip:\n%+v\n%+v", parsed, tree)
	}
	claudeEnrollment, claudeIdentity := claudeRecords()
	claude := must(parsed.ResolveAgent(agentbaseline.ClaudeAgentName, claudeEnrollment, claudeIdentity))
	codexEnrollment, codexIdentity := codexRecords()
	codex := must(parsed.ResolveAgent(agentbaseline.CodexReviewAgentName, codexEnrollment, codexIdentity))

	// "The baseline is honest": the Claude launch passes neither a model nor
	// an effort, and a Codex agent derives no selection at all.
	for _, agent := range []agenttree.ResolvedAgent{claude, codex} {
		selection := must(domain.DeriveAgentLaunchSelection(agent.Definition, agent.Adapter, agent.Offer))
		if selection != (domain.AgentLaunchSelection{}) {
			t.Fatalf("agent %s passes %+v, want nothing", agent.Definition.Name, selection)
		}
	}
	if codex.Offer.RouteModelID != "gpt-5.6-sol" {
		t.Fatalf("review offer model %q", codex.Offer.RouteModelID)
	}

	lineup := must(parsed.ResolveLineup())
	want := map[domain.RoleName]domain.Digest{
		domain.RoleSpecifier: claude.Definition.Digest, domain.RoleImplementer: claude.Definition.Digest,
		domain.RoleRemediator: claude.Definition.Digest, domain.RoleReviewer: codex.Definition.Digest,
		domain.RoleShadowReviewer: claude.Definition.Digest,
	}
	for role, digest := range want {
		line, ok := lineup.Line(role)
		if !ok || line.AgentDigest != digest {
			t.Fatalf("lineup line for %s = %+v, %v; want agent %s", role, line, ok, digest)
		}
		launch := must(agentbaseline.RoleLaunch(role))
		if parsed.Attended(line.AgentName, digest, launch.Digest) {
			t.Fatalf("role %s has no attended mark for its launch", role)
		}
	}
}

func TestBaselineTreeWithoutShadowArmOrReviewAgent(t *testing.T) {
	tree := must(agentbaseline.Tree(input(false)))
	if _, ok := must(tree.ResolveLineup()).Line(domain.RoleShadowReviewer); ok {
		t.Fatal("shadow reviewer line without a shadow prompt")
	}

	// An unadoptable review identity leaves a Claude-only tree, and a
	// reviewer prompt with no review agent is refused, not dropped.
	in := input(false)
	in.CodexEnrollment = nil
	if _, err := agentbaseline.Tree(in); err == nil {
		t.Fatal("reviewer line built with no review agent")
	}
	delete(in.Prompts, domain.RoleReviewer)
	tree = must(agentbaseline.Tree(in))
	if len(tree.Agents) != 2 || tree.Agents[0].Name != agentbaseline.ClaudeCallAgentName ||
		tree.Agents[1].Name != agentbaseline.ClaudeAgentName {
		t.Fatalf("agents = %+v", tree.Agents)
	}
}

// The two call adapters are one launch that differs in the efforts it can
// send: v2 declares every effort the claude_code table can send, v1 declares
// harness_default alone, and nothing else about them differs. v1's digest is
// written out because committed trees hold it: a deployment adopted on v1
// keeps admitting only while this definition still produces that digest. v2
// writes its efforts out, so a table that gains an effort fails here: the
// answer is a new adapter under a new audit, not an edit to v2.
func TestCallAdapterV2AddsOnlyTheEffortsItCanSend(t *testing.T) {
	v1, v2 := must(agentbaseline.ClaudeCallAdapterV1()), must(agentbaseline.ClaudeCallAdapter())
	const v1Digest = "sha256:3a7d6167dd93ef441a2533456f754b5b4dd643f0a81543f9c232de73f9fa4938"
	if v1.Digest != v1Digest || v1.AdapterBuild != "claude_code_call_v1" {
		t.Fatalf("v1 call adapter = %s %s, want the adopted %s", v1.AdapterBuild, v1.Digest, v1Digest)
	}
	if !slices.Equal(v1.SendableEfforts, []domain.EffortLevel{domain.EffortHarnessDefault}) {
		t.Fatalf("v1 sendable efforts = %v", v1.SendableEfforts)
	}
	if !slices.Equal(v2.SendableEfforts, domain.ClaudeCodeSendableEfforts()) || len(v2.SendableEfforts) < 2 {
		t.Fatalf("v2 sendable efforts = %v, want %v", v2.SendableEfforts, domain.ClaudeCodeSendableEfforts())
	}
	if v2.AdapterBuild != "claude_code_call_v2" || v2.Digest == v1.Digest {
		t.Fatalf("v2 call adapter = %s %s", v2.AdapterBuild, v2.Digest)
	}
	same := v2
	same.AdapterBuild, same.SendableEfforts, same.Digest = v1.AdapterBuild, v1.SendableEfforts, v1.Digest
	if !reflect.DeepEqual(same, v1) {
		t.Fatalf("the call adapters differ beyond build and sendable efforts:\nv1 %+v\nv2 %+v", v1, v2)
	}
}

// Every judgment role with a prompt gets a line on the call agent, and that
// agent is the ward agent with the call adapter in the adapter's place: the
// same enrollment, route, and offer under a launch with no tools.
func TestBaselineTreeNamesTheCallAgentForJudgmentRoles(t *testing.T) {
	in := input(false)
	roles := agentbaseline.JudgmentRoles()
	want := []domain.RoleName{
		domain.RoleDiagnostic, domain.RoleTaskNamer, domain.RolePublicationAuthor,
		domain.RoleFindingClassifier, domain.RoleFindingAdjudicator, domain.RoleDriftAuditor,
		domain.RoleAttentionDiscussion,
	}
	if !slices.Equal(roles, want) {
		t.Fatalf("judgment roles = %v, want %v", roles, want)
	}
	for _, role := range roles {
		if role != domain.RolePublicationAuthor {
			in.Prompts[role] = prompt(string(role))
		}
	}
	tree := must(agenttree.Parse(must(agenttree.Render(must(agentbaseline.Tree(in))))))
	enrollment, identity := claudeRecords()
	call := must(tree.ResolveAgent(agentbaseline.ClaudeCallAgentName, enrollment, identity))
	ward := must(tree.ResolveAgent(agentbaseline.ClaudeAgentName, enrollment, identity))
	adapter := must(agentbaseline.ClaudeCallAdapter())
	if call.Adapter.Digest != adapter.Digest || call.Adapter.HarnessBuild != agentbaseline.ClaudeCallHarnessBuild {
		t.Fatalf("call agent adapter = %+v", call.Adapter)
	}
	if call.Route.Digest != ward.Route.Digest || call.Offer.Digest != ward.Offer.Digest ||
		call.Definition.EnrollmentID != ward.Definition.EnrollmentID {
		t.Fatal("the call agent does not share the ward agent's enrollment, route, and offer")
	}
	if call.Definition.Digest == ward.Definition.Digest {
		t.Fatal("the call agent and the ward agent share a digest")
	}
	for _, capability := range []domain.LaunchCapability{
		domain.LaunchCapReadTools, domain.LaunchCapMutationTools, domain.LaunchCapRouteStoreContract,
	} {
		if adapter.LaunchCapabilities.Has(capability) {
			t.Fatalf("call adapter declares %s", capability)
		}
	}

	lineup := must(tree.ResolveLineup())
	for _, role := range roles {
		line, ok := lineup.Line(role)
		if role == domain.RolePublicationAuthor {
			// Its prompt is the operator's file; with none named, no line.
			if ok {
				t.Fatalf("publication author line without a prompt: %+v", line)
			}
			continue
		}
		if !ok || line.AgentName != agentbaseline.ClaudeCallAgentName ||
			line.AgentDigest != call.Definition.Digest || line.PromptName != string(role) {
			t.Fatalf("lineup line for %s = %+v, %v", role, line, ok)
		}
	}
	for _, mark := range tree.Marks {
		if mark.Agent == agentbaseline.ClaudeCallAgentName {
			t.Fatalf("call agent carries an attended mark: %+v", mark)
		}
	}
	// The ward adapters are the ones a conformance record proves.
	for _, proved := range must(agentbaseline.Adapters()) {
		if proved.Digest == adapter.Digest {
			t.Fatal("the call adapter is in the conformance set")
		}
	}
}

// rolesOff lists the roles the baseline gives no lineup line even with every
// optional input supplied, each with its reason. A role needs a line only
// while policy asks for its work (plan §5.4), so a role belongs here only
// while nothing can ask.
var rolesOff = map[domain.RoleName]string{
	domain.RoleBriefer: "no judgment site is built, so nothing asks for its work",
}

// With every optional input supplied, each role in the closed list has a
// lineup line keyed by its own name or a stated reason for being off, never
// both, and no line is keyed by anything else. A role added to AllRoleNames
// fails here until its author decides which.
func TestBaselineLineupCoversEveryRole(t *testing.T) {
	in := input(true)
	for _, role := range agentbaseline.JudgmentRoles() {
		in.Prompts[role] = prompt(string(role))
	}
	tree := must(agenttree.Parse(must(agenttree.Render(must(agentbaseline.Tree(in))))))

	keys := map[string]bool{}
	for _, line := range tree.Lineup {
		if !slices.Contains(domain.AllRoleNames, domain.RoleName(line.Key)) {
			// A stage name is the likeliest stray key, and no stage name is
			// a role name.
			t.Errorf("lineup key %q is not a role name", line.Key)
		}
		if keys[line.Key] {
			t.Errorf("lineup key %q appears twice", line.Key)
		}
		keys[line.Key] = true
	}
	for _, role := range domain.AllRoleNames {
		reason, off := rolesOff[role]
		switch {
		case keys[string(role)] && off:
			t.Errorf("role %s has a lineup line and is listed as off (%s)", role, reason)
		case !keys[string(role)] && !off:
			t.Errorf("role %s has no baseline lineup line and no reason in rolesOff", role)
		case off && reason == "":
			t.Errorf("role %s is listed as off with no reason", role)
		}
	}
	for role := range rolesOff {
		if !slices.Contains(domain.AllRoleNames, role) {
			t.Errorf("rolesOff names %q, which is not a role", role)
		}
	}
}

// The declared sets cover the two writer launches and nothing more: review's
// `forbidden` policy needs a control neither adapter has, so its coverage
// fails closed until an adapter proves it (#898).
func TestConformanceRecordsCoverTheWriterLaunchesOnly(t *testing.T) {
	provedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	records := must(agentbaseline.ConformanceRecords(provedAt))
	claude := must(agentbaseline.ClaudeWardAdapter())
	codex := must(agentbaseline.CodexReviewAdapter())
	byDigest := map[domain.Digest]domain.AdapterConformance{}
	for _, record := range records {
		byDigest[record.AdapterDigest] = record
	}
	if len(byDigest) != 2 {
		t.Fatalf("records = %+v", records)
	}
	for _, stage := range []domain.StageName{domain.StageNameSpecification, domain.StageNameImplementation} {
		launch := must(agentbaseline.Launch(stage))
		if err := domain.ValidateAdapterLaunchCoverage(byDigest[claude.Digest], claude.Digest, launch); err != nil {
			t.Fatalf("%s launch on the Claude adapter: %v", stage, err)
		}
		// The review adapter has no mutation tools, so it cannot run a writer.
		err := domain.ValidateAdapterLaunchCoverage(byDigest[codex.Digest], codex.Digest, launch)
		if !errors.Is(err, domain.ErrLaunchCapabilityUnproved) {
			t.Fatalf("%s launch on the review adapter = %v", stage, err)
		}
	}
	review := must(agentbaseline.Launch(domain.StageNameReview))
	for _, adapter := range []domain.AdapterFragment{claude, codex} {
		err := domain.ValidateAdapterLaunchCoverage(byDigest[adapter.Digest], adapter.Digest, review)
		if !errors.Is(err, domain.ErrLaunchCapabilityUnproved) {
			t.Fatalf("review launch on %s = %v, want %v", adapter.AdapterBuild, err, domain.ErrLaunchCapabilityUnproved)
		}
		missing := domain.MissingLaunchCapabilities(adapter.LaunchCapabilities, review.RequiredCapabilities())
		if !slices.Equal(missing, []domain.LaunchCapability{domain.LaunchCapAuxiliaryInferenceControl}) {
			t.Fatalf("review launch on %s misses %v, want only auxiliary-inference control", adapter.AdapterBuild, missing)
		}
	}
	if _, err := agentbaseline.Launch(domain.StageNameVerification); !errors.Is(err, domain.ErrInvalidStageName) {
		t.Fatalf("verification launch = %v", err)
	}
}

// The adapter fragments name the harness builds the agent images install. An
// image bump that leaves these behind would keep admitting under a record
// for a build that no longer runs.
func TestHarnessBuildsMatchTheImagePins(t *testing.T) {
	for _, tc := range []struct{ image, arg, prefix, build string }{
		{"agent-claude", "CLAUDE_CODE_VERSION", "claude-code ", agentbaseline.ClaudeCodeHarnessBuild},
		{"agent-codex", "CODEX_VERSION", "codex-cli ", agentbaseline.CodexHarnessBuild},
	} {
		path := filepath.Join("..", "..", "..", "images", tc.image, "Containerfile")
		body, err := os.ReadFile(path) //nolint:gosec // G304: fixed repository path
		if err != nil {
			t.Fatal(err)
		}
		match := regexp.MustCompile(`(?m)^ARG ` + tc.arg + `=(\S+)$`).FindSubmatch(body)
		if match == nil {
			t.Fatalf("%s pins no %s", path, tc.arg)
		}
		if want := tc.prefix + string(match[1]); tc.build != want {
			t.Fatalf("harness build %q, image pins %q", tc.build, want)
		}
	}
}
