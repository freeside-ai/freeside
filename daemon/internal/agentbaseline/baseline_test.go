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

// judgmentModel is the model the tests hand adoption for the call offer, and
// shadowReviewModel the one for the shadow reviewer's offer.
const (
	judgmentModel     = "claude-judgment-test"
	shadowReviewModel = "claude-shadow-review-test"
)

func input(shadow bool) agentbaseline.TreeInput {
	claude, _ := claudeRecords()
	codex, _ := codexRecords()
	in := agentbaseline.TreeInput{
		ClaudeEnrollment: claude, CodexEnrollment: &codex,
		ReviewModel: "gpt-5.6-sol", ReviewEffort: domain.EffortMax,
		TermsBasisDate: "2026-10-01", PricingRevision: "2026-10",
		OfferNotAfter: time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC),
		Prompts: map[domain.RoleName]agentbaseline.Prompt{
			domain.RoleSpecifier: prompt("specifier"), domain.RoleImplementer: prompt("implementer"),
			domain.RoleRemediator: prompt("remediator"), domain.RoleReviewer: prompt("review"),
		},
	}
	if shadow {
		in.Prompts[domain.RoleShadowReviewer] = prompt("shadow-review")
		in.ShadowReviewModel, in.ShadowReviewEffort = shadowReviewModel, domain.EffortHigh
	}
	return in
}

// The tree adoption proposes survives its own round trip and resolves its
// three ward agents against the adopted enrollments.
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

	shadow := must(parsed.ResolveAgent(agentbaseline.ClaudeShadowReviewAgentName, claudeEnrollment, claudeIdentity))

	// "The baseline is honest": the writer launch passes neither a model nor
	// an effort, and each review agent names the model and effort its launch
	// passes. Codex has no max, so the reviewer's max is sent as xhigh.
	for _, tc := range []struct {
		agent agenttree.ResolvedAgent
		want  domain.AgentLaunchSelection
	}{
		{claude, domain.AgentLaunchSelection{}},
		{codex, domain.AgentLaunchSelection{
			RouteModelID: "gpt-5.6-sol", RequestedEffort: domain.EffortMax, NativeEffort: "xhigh",
		}},
		{shadow, domain.AgentLaunchSelection{
			RouteModelID: shadowReviewModel, RequestedEffort: domain.EffortHigh, NativeEffort: "high",
		}},
	} {
		selection := must(domain.DeriveAgentLaunchSelection(tc.agent.Definition, tc.agent.Adapter, tc.agent.Offer))
		if selection != tc.want {
			t.Fatalf("agent %s passes %+v, want %+v", tc.agent.Definition.Name, selection, tc.want)
		}
	}
	if codex.Adapter.AdapterBuild != "codex_review_v2" ||
		!slices.Equal(codex.Offer.AllowedEfforts, []domain.EffortLevel{domain.EffortMax}) {
		t.Fatalf("review agent runs %s on an offer allowing %v", codex.Adapter.AdapterBuild, codex.Offer.AllowedEfforts)
	}
	// The shadow reviewer is the ward launch under the implementation
	// identity, on an offer of its own under the same route.
	if shadow.Adapter.Digest != claude.Adapter.Digest || shadow.Route.Digest != claude.Route.Digest ||
		shadow.Definition.EnrollmentID != claude.Definition.EnrollmentID ||
		shadow.Offer.IdentityStability != domain.IdentityPinned ||
		!slices.Equal(shadow.Offer.AllowedEfforts, []domain.EffortLevel{domain.EffortHigh}) {
		t.Fatalf("shadow reviewer agent = %+v on offer %+v", shadow.Definition, shadow.Offer)
	}

	lineup := must(parsed.ResolveLineup())
	want := map[domain.RoleName]domain.Digest{
		domain.RoleSpecifier: claude.Definition.Digest, domain.RoleImplementer: claude.Definition.Digest,
		domain.RoleRemediator: claude.Definition.Digest, domain.RoleReviewer: codex.Definition.Digest,
		domain.RoleShadowReviewer: shadow.Definition.Digest,
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

// The writer roles' agent is the one a deployment adopted before the review
// agents named a model and an effort: its digest is written out, and neither
// review input moves it.
func TestBaselineTreeKeepsTheWriterAgentDigest(t *testing.T) {
	const adopted = "sha256:80a6fc75affb198ed289cba5888f7a43f507ca252f32d6003b1987152152f711"
	for _, shadow := range []bool{false, true} {
		tree := must(agentbaseline.Tree(input(shadow)))
		if digest := must(tree.AgentDigest(agentbaseline.ClaudeAgentName)); digest != adopted {
			t.Fatalf("writer agent digest with shadow=%t is %s, want the adopted %s", shadow, digest, adopted)
		}
	}
}

// A review launch always passes a model and an effort, so the tree refuses a
// review agent that would name neither instead of writing a line the daemon
// then refuses.
func TestBaselineTreeRefusesAReviewAgentWithNothingToLaunchWith(t *testing.T) {
	cases := map[string]func(*agentbaseline.TreeInput){
		"no review effort":              func(in *agentbaseline.TreeInput) { in.ReviewEffort = "" },
		"review effort harness_default": func(in *agentbaseline.TreeInput) { in.ReviewEffort = domain.EffortHarnessDefault },
		"no shadow model":               func(in *agentbaseline.TreeInput) { in.ShadowReviewModel = "" },
		"shadow model is the native default": func(in *agentbaseline.TreeInput) {
			in.ShadowReviewModel = domain.ClaudeCodeNativeDefaultRouteModelID
		},
		"no shadow effort": func(in *agentbaseline.TreeInput) { in.ShadowReviewEffort = "" },
		"shadow effort harness_default": func(in *agentbaseline.TreeInput) {
			in.ShadowReviewEffort = domain.EffortHarnessDefault
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := input(true)
			mutate(&in)
			if tree, err := agentbaseline.Tree(in); err == nil {
				t.Fatalf("tree built: agents %+v", tree.Agents)
			}
		})
	}
	// Neither review input is needed while its agent is not in the tree.
	in := input(false)
	in.CodexEnrollment, in.ReviewEffort = nil, ""
	delete(in.Prompts, domain.RoleReviewer)
	if _, err := agentbaseline.Tree(in); err != nil {
		t.Fatalf("tree with no review agent: %v", err)
	}
}

func TestBaselineTreeWithoutShadowArmOrReviewAgent(t *testing.T) {
	tree := must(agentbaseline.Tree(input(false)))
	if _, ok := must(tree.ResolveLineup()).Line(domain.RoleShadowReviewer); ok {
		t.Fatal("shadow reviewer line without a shadow prompt")
	}
	if _, ok := tree.Agent(agentbaseline.ClaudeShadowReviewAgentName); ok {
		t.Fatal("shadow reviewer agent without a shadow prompt")
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
	if len(tree.Agents) != 1 || tree.Agents[0].Name != agentbaseline.ClaudeAgentName {
		t.Fatalf("agents = %+v", tree.Agents)
	}
}

// Without a judgment model the tree has nothing a judgment line could name:
// no call agent, no call offer, and no call adapter. A judgment prompt is
// then refused, not dropped, and so is the native-default id as the model,
// which would build a second offer that names no model.
func TestBaselineTreeWithoutAJudgmentModelHasNoCallAgent(t *testing.T) {
	tree := must(agentbaseline.Tree(input(true)))
	for _, agent := range tree.Agents {
		if agent.Name == agentbaseline.ClaudeCallAgentName {
			t.Fatalf("call agent without a judgment model: %+v", agent)
		}
	}
	for _, offer := range tree.Offers {
		if offer.Name == agentbaseline.ClaudeCallOfferName {
			t.Fatalf("call offer without a judgment model: %+v", offer)
		}
	}
	for _, adapter := range tree.Adapters {
		if adapter.Name == agentbaseline.ClaudeCallAdapterName {
			t.Fatalf("call adapter without a judgment model: %+v", adapter)
		}
	}

	in := input(false)
	in.Prompts[domain.RoleTaskNamer] = prompt("task_namer")
	if _, err := agentbaseline.Tree(in); err == nil {
		t.Fatal("judgment line built with no judgment model")
	}
	in.JudgmentModel = domain.ClaudeCodeNativeDefaultRouteModelID
	if _, err := agentbaseline.Tree(in); err == nil {
		t.Fatal("call offer built on the native-default id")
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

// The two review adapters are one launch that differs in the efforts it can
// send: v2 declares every effort the codex_cli table can send, v1 declares
// harness_default alone, and nothing else about them differs. v1's digest is
// written out because committed trees hold it, and its conformance record is
// held to this definition.
func TestReviewAdapterV2AddsOnlyTheEffortsItCanSend(t *testing.T) {
	v1, v2 := must(agentbaseline.CodexReviewAdapterV1()), must(agentbaseline.CodexReviewAdapter())
	const v1Digest = "sha256:cc1dd347863eafc67e095ddee3af95fb3141f606fae4d25d2e472ff2bd172f98"
	if v1.Digest != v1Digest || v1.AdapterBuild != "codex_review_v1" {
		t.Fatalf("v1 review adapter = %s %s, want the adopted %s", v1.AdapterBuild, v1.Digest, v1Digest)
	}
	if !slices.Equal(v1.SendableEfforts, []domain.EffortLevel{domain.EffortHarnessDefault}) {
		t.Fatalf("v1 sendable efforts = %v", v1.SendableEfforts)
	}
	if !slices.Equal(v2.SendableEfforts, domain.CodexCLISendableEfforts()) || len(v2.SendableEfforts) < 2 {
		t.Fatalf("v2 sendable efforts = %v, want %v", v2.SendableEfforts, domain.CodexCLISendableEfforts())
	}
	if v2.AdapterBuild != "codex_review_v2" || v2.Digest == v1.Digest {
		t.Fatalf("v2 review adapter = %s %s", v2.AdapterBuild, v2.Digest)
	}
	same := v2
	same.AdapterBuild, same.SendableEfforts, same.Digest = v1.AdapterBuild, v1.SendableEfforts, v1.Digest
	if !reflect.DeepEqual(same, v1) {
		t.Fatalf("the review adapters differ beyond build and sendable efforts:\nv1 %+v\nv2 %+v", v1, v2)
	}
}

// Every judgment role with a prompt gets a line on the call agent. That
// agent shares the ward agent's enrollment and route under a launch with no
// tools, and has its own offer, which names the model adoption was given:
// a call launch always passes a model, and the ward's offer names none.
func TestBaselineTreeNamesTheCallAgentForJudgmentRoles(t *testing.T) {
	in := input(false)
	in.JudgmentModel = judgmentModel
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
	if call.Route.Digest != ward.Route.Digest || call.Definition.EnrollmentID != ward.Definition.EnrollmentID {
		t.Fatal("the call agent does not share the ward agent's enrollment and route")
	}
	if call.Offer.RouteModelID != judgmentModel || call.Offer.IdentityStability != domain.IdentityPinned ||
		!slices.Equal(call.Offer.AllowedEfforts, []domain.EffortLevel{domain.EffortHarnessDefault}) ||
		call.Definition.Effort != domain.EffortHarnessDefault {
		t.Fatalf("call offer = %+v at effort %s", call.Offer, call.Definition.Effort)
	}
	if ward.Offer.RouteModelID != domain.ClaudeCodeNativeDefaultRouteModelID {
		t.Fatalf("ward offer model %q, want the native default", ward.Offer.RouteModelID)
	}
	// The call launch passes the offer's model and no effort; the ward launch
	// still passes neither.
	callLaunch := must(domain.DeriveAgentLaunchSelection(call.Definition, call.Adapter, call.Offer))
	if callLaunch != (domain.AgentLaunchSelection{RouteModelID: judgmentModel}) {
		t.Fatalf("call agent passes %+v, want the model alone", callLaunch)
	}
	wardLaunch := must(domain.DeriveAgentLaunchSelection(ward.Definition, ward.Adapter, ward.Offer))
	if wardLaunch != (domain.AgentLaunchSelection{}) {
		t.Fatalf("ward agent passes %+v, want nothing", wardLaunch)
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
	in.JudgmentModel = judgmentModel
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
// `forbidden` policy needs a control no adapter has, so its coverage fails
// closed until an adapter proves it (#1980). Both review adapters have a
// record, and neither can be admitted to a ward launch: capability coverage
// refuses them whatever effort they can send.
func TestConformanceRecordsCoverTheWriterLaunchesOnly(t *testing.T) {
	provedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	records := must(agentbaseline.ConformanceRecords(provedAt))
	claude := must(agentbaseline.ClaudeWardAdapter())
	codex := must(agentbaseline.CodexReviewAdapter())
	codexV1 := must(agentbaseline.CodexReviewAdapterV1())
	byDigest := map[domain.Digest]domain.AdapterConformance{}
	for _, record := range records {
		byDigest[record.AdapterDigest] = record
	}
	if len(byDigest) != 3 {
		t.Fatalf("records = %+v", records)
	}
	for _, stage := range []domain.StageName{domain.StageNameSpecification, domain.StageNameImplementation} {
		launch := must(agentbaseline.Launch(stage))
		if err := domain.ValidateAdapterLaunchCoverage(byDigest[claude.Digest], claude.Digest, launch); err != nil {
			t.Fatalf("%s launch on the Claude adapter: %v", stage, err)
		}
		// A review adapter has no mutation tools, so it cannot run a writer.
		for _, adapter := range []domain.AdapterFragment{codex, codexV1} {
			err := domain.ValidateAdapterLaunchCoverage(byDigest[adapter.Digest], adapter.Digest, launch)
			if !errors.Is(err, domain.ErrLaunchCapabilityUnproved) {
				t.Fatalf("%s launch on %s = %v", stage, adapter.AdapterBuild, err)
			}
		}
	}
	review := must(agentbaseline.Launch(domain.StageNameReview))
	for _, adapter := range []domain.AdapterFragment{claude, codex, codexV1} {
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

// claudeReviewRecords is a Claude review identity apart from the writer's,
// on a route of its own.
func claudeReviewRecords() (domain.ClientEnrollment, domain.AuthIdentity) {
	enrollment, identity := claudeRecords()
	enrollment.ID, enrollment.AuthIdentityID, enrollment.Route = "claude-r/claude_code", "claude-r", "anthropic_review"
	enrollment.AccountBinding = "reviewer@example.com"
	identity.ID, identity.AccountBinding = "claude-r", "reviewer@example.com"
	identity.Interim.AuthStoreVolume = "claude-review-cred"
	return enrollment, identity
}

// claudeReviewModel is the model the tests hand adoption for a Claude
// reviewer's offer.
const claudeReviewModel = "claude-review-test"

func claudeReviewInput() agentbaseline.TreeInput {
	in := input(true)
	review, _ := claudeReviewRecords()
	in.CodexEnrollment, in.ClaudeReviewEnrollment = nil, &review
	in.ReviewModel, in.ReviewEffort = claudeReviewModel, domain.EffortHigh
	return in
}

// A Claude reviewer (#397) is an agent on the review identity's claude_code
// enrollment and the Claude ward adapter, on an offer of its own, and the
// reviewer line names it with an attended mark. The writer agent and the
// shadow reviewer are unchanged, and the tree has no Codex agent.
func TestBaselineTreeRunsAClaudeReviewer(t *testing.T) {
	tree := must(agentbaseline.Tree(claudeReviewInput()))
	parsed := must(agenttree.Parse(must(agenttree.Render(tree))))
	if !reflect.DeepEqual(parsed, tree) {
		t.Fatalf("baseline tree changed across its round trip:\n%+v\n%+v", parsed, tree)
	}
	claudeEnrollment, claudeIdentity := claudeRecords()
	claude := must(parsed.ResolveAgent(agentbaseline.ClaudeAgentName, claudeEnrollment, claudeIdentity))
	reviewEnrollment, reviewIdentity := claudeReviewRecords()
	review := must(parsed.ResolveAgent(agentbaseline.ClaudeReviewAgentName, reviewEnrollment, reviewIdentity))
	if review.Adapter.Digest != claude.Adapter.Digest || review.Route.Digest != claude.Route.Digest ||
		review.Definition.EnrollmentID != reviewEnrollment.ID ||
		review.Adapter.Vendor != domain.AgentVendorClaude || review.Offer.IdentityStability != domain.IdentityPinned {
		t.Fatalf("Claude review agent = %+v on adapter %+v, offer %+v", review.Definition, review.Adapter, review.Offer)
	}
	selection := must(domain.DeriveAgentLaunchSelection(review.Definition, review.Adapter, review.Offer))
	if selection != (domain.AgentLaunchSelection{
		RouteModelID: claudeReviewModel, RequestedEffort: domain.EffortHigh, NativeEffort: "high",
	}) {
		t.Fatalf("Claude review agent passes %+v", selection)
	}
	for _, agent := range parsed.Agents {
		if agent.Name == agentbaseline.CodexReviewAgentName {
			t.Fatalf("a Claude reviewer tree carries the Codex review agent")
		}
	}
	line, ok := must(parsed.ResolveLineup()).Line(domain.RoleReviewer)
	launch := must(agentbaseline.RoleLaunch(domain.RoleReviewer))
	if !ok || line.AgentName != agentbaseline.ClaudeReviewAgentName || line.AgentDigest != review.Definition.Digest ||
		parsed.Attended(line.AgentName, line.AgentDigest, launch.Digest) {
		t.Fatalf("reviewer line = %+v, %v, or it has no attended mark", line, ok)
	}
	const adopted = "sha256:80a6fc75affb198ed289cba5888f7a43f507ca252f32d6003b1987152152f711"
	if digest := must(tree.AgentDigest(agentbaseline.ClaudeAgentName)); digest != adopted {
		t.Fatalf("writer agent digest is %s, want the adopted %s", digest, adopted)
	}

	// On the writer's own enrollment the review agent shares its route.
	in := claudeReviewInput()
	in.ClaudeReviewEnrollment = &claudeEnrollment
	shared := must(agentbaseline.Tree(in))
	if len(shared.Routes) != 1 {
		t.Fatalf("a Claude reviewer on the writer's enrollment added a route: %+v", shared.Routes)
	}
	must(shared.ResolveAgent(agentbaseline.ClaudeReviewAgentName, claudeEnrollment, claudeIdentity))
}

func TestBaselineTreeRefusesAClaudeReviewerItCannotLaunch(t *testing.T) {
	codex, _ := codexRecords()
	cases := map[string]func(*agentbaseline.TreeInput){
		"both review enrollments": func(in *agentbaseline.TreeInput) { in.CodexEnrollment = &codex },
		"a codex_cli enrollment":  func(in *agentbaseline.TreeInput) { in.ClaudeReviewEnrollment = &codex },
		"no review model":         func(in *agentbaseline.TreeInput) { in.ReviewModel = "" },
		"review model is the native default": func(in *agentbaseline.TreeInput) {
			in.ReviewModel = domain.ClaudeCodeNativeDefaultRouteModelID
		},
		"review effort harness_default": func(in *agentbaseline.TreeInput) { in.ReviewEffort = domain.EffortHarnessDefault },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := claudeReviewInput()
			mutate(&in)
			if tree, err := agentbaseline.Tree(in); err == nil {
				t.Fatalf("tree built: agents %+v", tree.Agents)
			}
		})
	}
}
