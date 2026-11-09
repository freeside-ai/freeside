package agentbaseline_test

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
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
		Enabled: true, AccountBinding: "operator@example.com", CostOwner: "operator",
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
		Enabled: true, AccountBinding: "acct-1", CostOwner: "operator",
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: "/private/auth.json", RefreshStrategy: domain.RefreshOnDemand,
			SupportsReadOnlyAuthSnapshot: true,
		},
	}
}

func prompt(name string) agentbaseline.Prompt {
	return agentbaseline.Prompt{Name: name, Digest: domain.Digest(contentaddr.Sum([]byte(name)))}
}

// judgmentModel is the model the tests hand adoption for the judgment lines,
// shadowReviewModel the one for the shadow reviewer's line, and the two
// writer models the ones the writer roles ask: the specifier its own, the
// implementer and remediator one between them.
const (
	judgmentModel     = "claude-judgment-test"
	shadowReviewModel = "claude-shadow-review-test"
	specifierModel    = "claude-specifier-test"
	implementerModel  = "claude-implementer-test"
)

// names is the selection a line names: the enrollment's id, the model, and
// the harness's own effort string.
func names(enrollment domain.ClientEnrollment, model, effort string) domain.AgentSelectionNames {
	return domain.AgentSelectionNames{Enrollment: string(enrollment.ID), Model: model, Effort: effort}
}

// assertLines fails unless each role's line names the wanted selection.
func assertLines(t *testing.T, tree agenttree.Tree, want map[domain.RoleName]domain.AgentSelectionNames) {
	t.Helper()
	lineup := must(tree.ResolveLineup())
	for role, names := range want {
		if line, ok := lineup.Line(role); !ok || line.Names != names {
			t.Fatalf("lineup line for %s = %+v, %v; want %+v", role, line, ok, names)
		}
	}
}

// resolveLine resolves a line's selection against the tree's adapter for the
// launch, as admission does, under the line's own enrollment records.
func resolveLine(
	t *testing.T, tree agenttree.Tree, names domain.AgentSelectionNames, launch domain.LaunchSpec,
	enrollment domain.ClientEnrollment, identity domain.AuthIdentity,
) domain.AgentSelection {
	t.Helper()
	adapter, err := tree.AdapterForLaunch(enrollment.HarnessClient, launch, names.Effort)
	if err != nil {
		t.Fatalf("adapter for %+v: %v", names, err)
	}
	selection, err := domain.ResolveAgentSelection(domain.AgentSelectionInput{
		Names: names, Identity: identity, Enrollment: enrollment, Adapter: adapter,
		Models: tree.RouteModels(enrollment.Route),
	})
	if err != nil {
		t.Fatalf("resolve %+v: %v", names, err)
	}
	return selection
}

// writers is the writer selection every test starts from.
func writers() map[domain.RoleName]agentbaseline.Selection {
	return map[domain.RoleName]agentbaseline.Selection{
		domain.RoleSpecifier:   {Model: specifierModel, Effort: domain.EffortHigh},
		domain.RoleImplementer: {Model: implementerModel, Effort: domain.EffortHigh},
		domain.RoleRemediator:  {Model: implementerModel, Effort: domain.EffortHigh},
	}
}

func input(shadow bool) agentbaseline.TreeInput {
	claude, _ := claudeRecords()
	codex, _ := codexRecords()
	in := agentbaseline.TreeInput{
		ClaudeEnrollment: claude, CodexEnrollment: &codex, Writers: writers(),
		ReviewModel: "gpt-5.6-sol", ReviewEffort: domain.EffortMax, JudgmentEffort: domain.EffortMedium,
		TermsBasisDate: "2026-10-01",
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

// The tree adoption proposes survives its own round trip, and each line
// names the model and effort its launch passes and resolves under the
// adopted enrollments. It carries no attended mark file: plan revision 97
// removed the mark.
func TestBaselineTreeRoundTripsAndResolves(t *testing.T) {
	tree := must(agentbaseline.Tree(input(true)))
	files := must(agenttree.Render(tree))
	for path := range files {
		if strings.HasSuffix(path, ".attended") {
			t.Fatalf("baseline tree renders %s", path)
		}
	}
	parsed := must(agenttree.Parse(files))
	if !reflect.DeepEqual(parsed, tree) {
		t.Fatalf("baseline tree changed across its round trip:\n%+v\n%+v", parsed, tree)
	}
	claude, claudeIdentity := claudeRecords()
	codex, codexIdentity := codexRecords()
	// Codex has no max, so the reviewer's max is recorded as the xhigh the
	// review launch sends.
	assertLines(t, parsed, map[domain.RoleName]domain.AgentSelectionNames{
		domain.RoleSpecifier:      names(claude, specifierModel, "high"),
		domain.RoleImplementer:    names(claude, implementerModel, "high"),
		domain.RoleRemediator:     names(claude, implementerModel, "high"),
		domain.RoleReviewer:       names(codex, "gpt-5.6-sol", "xhigh"),
		domain.RoleShadowReviewer: names(claude, shadowReviewModel, "high"),
	})
	lineup := must(parsed.ResolveLineup())
	implementation := must(agentbaseline.Launch(domain.StageNameImplementation))
	for _, role := range []domain.RoleName{domain.RoleSpecifier, domain.RoleImplementer, domain.RoleShadowReviewer} {
		line, _ := lineup.Line(role)
		selection := resolveLine(t, parsed, line.Names, implementation, claude, claudeIdentity)
		if selection.AdapterDigest != must(agentbaseline.ClaudeWardAdapter()).Digest {
			t.Fatalf("%s line resolves on adapter %s", role, selection.AdapterDigest)
		}
	}
	// The review adapter drives Codex and sends the efforts its table maps.
	review, _ := lineup.Line(domain.RoleReviewer)
	adapter := must(parsed.AdapterForLaunch(domain.HarnessClientCodexCLI,
		must(agentbaseline.Launch(domain.StageNameReview)), review.Names.Effort,
		domain.LaunchCapAuxiliaryInferenceControl))
	if adapter.AdapterBuild != "codex_review_v2" {
		t.Fatalf("review line runs on %s", adapter.AdapterBuild)
	}
	must(domain.ResolveAgentSelection(domain.AgentSelectionInput{
		Names: review.Names, Identity: codexIdentity, Enrollment: codex, Adapter: adapter,
	}))
}

// renderedTree concatenates a rendered tree's files in path order, each
// under a header naming its path, so one golden pins every byte adoption
// writes.
func renderedTree(t *testing.T, in agentbaseline.TreeInput) []byte {
	t.Helper()
	files := must(agenttree.Render(must(agentbaseline.Tree(in))))
	var out strings.Builder
	for _, path := range slices.Sorted(maps.Keys(files)) {
		out.WriteString("=== " + path + "\n")
		out.Write(files[path])
	}
	return []byte(out.String())
}

// The baseline fixture renders the same bytes it rendered before escalated
// specifier lines existed: adoption without an escalated pair emits today's
// tree. The golden was recorded before that change.
func TestBaselineTreeRendersTheRecordedFiles(t *testing.T) {
	golden.Assert(t, "baseline_tree", renderedTree(t, input(true)))
}

const escalatedSpecifierModel = "claude-specifier-escalated-test"

// With an escalated specifier pair, adoption adds the escalated line, keyed
// by the specifier's enrollment, naming the escalated selection under the
// specifier's prompt. The rest of the tree is the one adoption emits without
// the pair.
func TestBaselineTreeEmitsTheEscalatedSpecifier(t *testing.T) {
	in := input(true)
	in.SpecifierEscalated = &agentbaseline.Selection{Model: escalatedSpecifierModel, Effort: domain.EffortMax}
	golden.Assert(t, "baseline_tree_escalated", renderedTree(t, in))

	tree := must(agenttree.Parse(must(agenttree.Render(must(agentbaseline.Tree(in))))))
	claudeEnrollment, claudeIdentity := claudeRecords()
	lineup := must(tree.ResolveLineup())
	line, _ := lineup.Line(domain.RoleSpecifier)
	counterpart, ok := lineup.EscalatedCounterpart(domain.RoleSpecifier, string(claudeEnrollment.ID))
	if !ok || counterpart.Names != names(claudeEnrollment, escalatedSpecifierModel, "max") ||
		counterpart.PromptDigest != line.PromptDigest ||
		line.Names != names(claudeEnrollment, specifierModel, "high") {
		t.Fatalf("escalated line = %+v, %t beside the specifier line %+v", counterpart, ok, line)
	}
	resolveLine(t, tree, counterpart.Names, must(agentbaseline.Launch(domain.StageNameSpecification)),
		claudeEnrollment, claudeIdentity)
}

func TestBaselineTreeRefusesAnEscalatedSpecifierThatChangesNothing(t *testing.T) {
	own := writers()[domain.RoleSpecifier]
	noPrompt := input(true)
	delete(noPrompt.Prompts, domain.RoleSpecifier)
	for name, tc := range map[string]struct {
		in        agentbaseline.TreeInput
		selection agentbaseline.Selection
	}{
		"the specifier's own pair": {input(true), own},
		"no model":                 {input(true), agentbaseline.Selection{Effort: domain.EffortMax}},
		"the native default":       {input(true), agentbaseline.Selection{Model: domain.ClaudeCodeNativeDefaultRouteModelID, Effort: domain.EffortMax}},
		"no effort":                {input(true), agentbaseline.Selection{Model: escalatedSpecifierModel, Effort: domain.EffortHarnessDefault}},
		"no specifier prompt":      {noPrompt, agentbaseline.Selection{Model: escalatedSpecifierModel, Effort: domain.EffortMax}},
	} {
		t.Run(name, func(t *testing.T) {
			in := tc.in
			in.SpecifierEscalated = &tc.selection
			if _, err := agentbaseline.Tree(in); err == nil {
				t.Fatal("Tree accepted the escalated specifier")
			}
		})
	}
}

// Each writer role's line names the role's own model and effort, all on the
// one Claude route and ward adapter: roles asking one pair name one
// selection, and a role asking another pair names its own.
func TestBaselineTreeNamesEachWriterRolesSelection(t *testing.T) {
	claude, _ := claudeRecords()
	in := input(false)
	in.Writers[domain.RoleRemediator] = agentbaseline.Selection{Model: implementerModel, Effort: domain.EffortMedium}
	tree := must(agentbaseline.Tree(in))
	assertLines(t, tree, map[domain.RoleName]domain.AgentSelectionNames{
		domain.RoleSpecifier:   names(claude, specifierModel, "high"),
		domain.RoleImplementer: names(claude, implementerModel, "high"),
		domain.RoleRemediator:  names(claude, implementerModel, "medium"),
	})
	var routes, adapters []string
	for _, route := range tree.Routes {
		routes = append(routes, route.Name)
	}
	for _, adapter := range tree.Adapters {
		adapters = append(adapters, adapter.Name)
	}
	if !slices.Equal(routes, []string{agentbaseline.DefaultClaudeRoute, agentbaseline.DefaultCodexRoute}) ||
		!slices.Equal(adapters, []string{agentbaseline.ClaudeAdapterName, agentbaseline.CodexAdapterName}) {
		t.Fatalf("routes %v, adapters %v", routes, adapters)
	}
}

// A writer launch passes the line's model and effort, so the tree refuses
// a writer selection that names neither instead of writing a line that runs
// on the CLI's default.
func TestBaselineTreeRefusesAWriterWithNothingToLaunchWith(t *testing.T) {
	for name, edit := range map[string]func(map[domain.RoleName]agentbaseline.Selection){
		"missing role": func(w map[domain.RoleName]agentbaseline.Selection) { delete(w, domain.RoleRemediator) },
		"empty model": func(w map[domain.RoleName]agentbaseline.Selection) {
			w[domain.RoleSpecifier] = agentbaseline.Selection{Effort: domain.EffortHigh}
		},
		"native default": func(w map[domain.RoleName]agentbaseline.Selection) {
			w[domain.RoleImplementer] = agentbaseline.Selection{
				Model: domain.ClaudeCodeNativeDefaultRouteModelID, Effort: domain.EffortHigh,
			}
		},
		"harness_default effort": func(w map[domain.RoleName]agentbaseline.Selection) {
			w[domain.RoleImplementer] = agentbaseline.Selection{Model: implementerModel, Effort: domain.EffortHarnessDefault}
		},
		"empty effort": func(w map[domain.RoleName]agentbaseline.Selection) {
			w[domain.RoleSpecifier] = agentbaseline.Selection{Model: specifierModel}
		},
		"unknown effort": func(w map[domain.RoleName]agentbaseline.Selection) {
			w[domain.RoleSpecifier] = agentbaseline.Selection{Model: specifierModel, Effort: "xhigh"}
		},
	} {
		in := input(false)
		edit(in.Writers)
		if _, err := agentbaseline.Tree(in); err == nil {
			t.Errorf("%s: tree built", name)
		}
	}
}

// A review launch always passes a model and an effort, so the tree refuses a
// review line that would name neither instead of writing a line the daemon
// then refuses.
func TestBaselineTreeRefusesAReviewLineWithNothingToLaunchWith(t *testing.T) {
	cases := map[string]func(*agentbaseline.TreeInput){
		"no review model":               func(in *agentbaseline.TreeInput) { in.ReviewModel = "" },
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
				t.Fatalf("tree built: lineup %+v", tree.Lineup)
			}
		})
	}
	// Neither review input is needed while its line is not in the tree.
	in := input(false)
	in.CodexEnrollment, in.ReviewModel, in.ReviewEffort = nil, "", ""
	delete(in.Prompts, domain.RoleReviewer)
	if _, err := agentbaseline.Tree(in); err != nil {
		t.Fatalf("tree with no review line: %v", err)
	}
}

func TestBaselineTreeWithoutShadowArmOrReviewAgent(t *testing.T) {
	tree := must(agentbaseline.Tree(input(false)))
	if _, ok := must(tree.ResolveLineup()).Line(domain.RoleShadowReviewer); ok {
		t.Fatal("shadow reviewer line without a shadow prompt")
	}

	// An unadoptable review identity leaves a Claude-only tree, and a
	// reviewer prompt with no review enrollment is refused, not dropped.
	in := input(false)
	in.CodexEnrollment = nil
	if _, err := agentbaseline.Tree(in); err == nil {
		t.Fatal("reviewer line built with no review enrollment")
	}
	delete(in.Prompts, domain.RoleReviewer)
	tree = must(agentbaseline.Tree(in))
	if len(tree.Routes) != 1 || len(tree.Adapters) != 1 || tree.Adapters[0].Name != agentbaseline.ClaudeAdapterName {
		t.Fatalf("routes %+v, adapters %+v, want the writer's alone", tree.Routes, tree.Adapters)
	}
}

// Without a judgment model the tree has nothing a judgment line could name:
// no call adapter. A judgment prompt is then refused, not dropped, and so is
// the native-default id as the model, which a call launch cannot pass.
func TestBaselineTreeWithoutAJudgmentModelHasNoCallAdapter(t *testing.T) {
	tree := must(agentbaseline.Tree(input(true)))
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
		t.Fatal("judgment line built on the native-default id")
	}
	// A call asks one effort; harness_default would leave it to the CLI.
	in.JudgmentModel, in.JudgmentEffort = judgmentModel, domain.EffortHarnessDefault
	if _, err := agentbaseline.Tree(in); err == nil {
		t.Fatal("judgment line built asking harness_default")
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
	const v1Digest = "sha256:d00b13ede2cb339da5cfc4828f7ecdf10978adda2c0079c6c1fb533aaef726a1"
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

// Every judgment role with a prompt gets a line naming the judgment model on
// the writer's enrollment, and the tree carries the call adapter those lines
// run on: a launch with no tools under the same subscription and route.
func TestBaselineTreeNamesTheJudgmentSelectionForJudgmentRoles(t *testing.T) {
	in := input(false)
	in.JudgmentModel = judgmentModel
	roles := agentbaseline.JudgmentRoles()
	want := []domain.RoleName{
		domain.RoleDiagnostic, domain.RoleTaskNamer, domain.RolePublicationAuthor,
		domain.RoleFindingClassifier, domain.RoleFindingAdjudicator, domain.RoleDriftAuditor,
		domain.RoleAttentionDiscussion, domain.RoleTriager,
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
	enrollment, _ := claudeRecords()
	adapter := must(agentbaseline.ClaudeCallAdapter())
	var carried bool
	for _, a := range tree.Adapters {
		carried = carried || (a.Name == agentbaseline.ClaudeCallAdapterName && a.Fragment.Digest == adapter.Digest)
	}
	if !carried || adapter.HarnessBuild != agentbaseline.ClaudeCallHarnessBuild {
		t.Fatalf("call adapter %+v carried %t", adapter, carried)
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
		if !ok || line.Names != names(enrollment, judgmentModel, "medium") || line.PromptName != string(role) {
			t.Fatalf("lineup line for %s = %+v, %v", role, line, ok)
		}
	}
	// The writer lines keep their own models beside the judgment lines.
	assertLines(t, tree, map[domain.RoleName]domain.AgentSelectionNames{
		domain.RoleSpecifier: names(enrollment, specifierModel, "high"),
	})
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
// closed until an adapter proves it (#1980).
func TestDeclaredCapabilitiesCoverTheWriterLaunchesOnly(t *testing.T) {
	provedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	claude := must(agentbaseline.ClaudeWardAdapter())
	codex := must(agentbaseline.CodexReviewAdapter())
	codexV1 := must(agentbaseline.CodexReviewAdapterV1())
	byDigest := map[domain.Digest]domain.AdapterConformance{}
	for _, adapter := range []domain.AdapterFragment{claude, codex, codexV1} {
		byDigest[adapter.Digest] = must(domain.NewAdapterConformance(domain.AdapterConformanceInput{
			AdapterDigest: adapter.Digest, Outcome: domain.ConformancePassed,
			ProvedCapabilities: adapter.LaunchCapabilities, ProvedAt: provedAt,
		}))
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
// reviewer's line.
const claudeReviewModel = "claude-review-test"

func claudeReviewInput() agentbaseline.TreeInput {
	in := input(true)
	review, _ := claudeReviewRecords()
	in.CodexEnrollment, in.ClaudeReviewEnrollment = nil, &review
	in.ReviewModel, in.ReviewEffort = claudeReviewModel, domain.EffortHigh
	return in
}

// A Claude reviewer (#397) runs on the review identity's claude_code
// enrollment and the Claude ward adapter, under a route of its own when the
// enrollment names one, and the reviewer line names it. The writer lines are
// unchanged, and the tree has no Codex route or adapter.
func TestBaselineTreeRunsAClaudeReviewer(t *testing.T) {
	tree := must(agentbaseline.Tree(claudeReviewInput()))
	parsed := must(agenttree.Parse(must(agenttree.Render(tree))))
	if !reflect.DeepEqual(parsed, tree) {
		t.Fatalf("baseline tree changed across its round trip:\n%+v\n%+v", parsed, tree)
	}
	claudeEnrollment, _ := claudeRecords()
	reviewEnrollment, _ := claudeReviewRecords()
	assertLines(t, parsed, map[domain.RoleName]domain.AgentSelectionNames{
		domain.RoleReviewer: names(reviewEnrollment, claudeReviewModel, "high"),
	})
	writerRoute, _ := parsed.Route(claudeEnrollment.Route)
	reviewRoute, ok := parsed.Route(reviewEnrollment.Route)
	if !ok || reviewRoute.Digest != writerRoute.Digest {
		t.Fatalf("review route %s = %+v, %t", reviewEnrollment.Route, reviewRoute, ok)
	}
	for _, adapter := range parsed.Adapters {
		if adapter.Name == agentbaseline.CodexAdapterName {
			t.Fatal("a Claude reviewer tree carries the Codex review adapter")
		}
	}
	if _, ok := parsed.Route(agentbaseline.DefaultCodexRoute); ok {
		t.Fatal("a Claude reviewer tree carries the Codex route")
	}
	codexLineup := must(must(agentbaseline.Tree(input(true))).ResolveLineup())
	lineup := must(parsed.ResolveLineup())
	for _, role := range agentbaseline.WriterRoles {
		got, _ := lineup.Line(role)
		want, _ := codexLineup.Line(role)
		if got != want {
			t.Fatalf("%s line under a Claude reviewer = %+v, want the Codex reviewer tree's %+v", role, got, want)
		}
	}

	// On the writer's own enrollment the reviewer shares its route.
	in := claudeReviewInput()
	in.ClaudeReviewEnrollment = &claudeEnrollment
	shared := must(agentbaseline.Tree(in))
	if len(shared.Routes) != 1 {
		t.Fatalf("a Claude reviewer on the writer's enrollment added a route: %+v", shared.Routes)
	}
	assertLines(t, shared, map[domain.RoleName]domain.AgentSelectionNames{
		domain.RoleReviewer: names(claudeEnrollment, claudeReviewModel, "high"),
	})
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
				t.Fatalf("tree built: lineup %+v", tree.Lineup)
			}
		})
	}
}

// secondClaudeRecords is a second writer subscription: another identity on
// its own volume and account, enrolled on the writer's route.
func secondClaudeRecords() (domain.ClientEnrollment, domain.AuthIdentity) {
	enrollment, identity := claudeRecords()
	enrollment.ID, enrollment.AuthIdentityID = "claude-b/claude_code", "claude-b"
	enrollment.AccountBinding = "second@example.com"
	identity.ID, identity.AccountBinding = "claude-b", "second@example.com"
	identity.Interim.AuthStoreVolume = "claude-cred-second"
	return enrollment, identity
}

func secondWriterInput(escalated bool) agentbaseline.TreeInput {
	in := input(true)
	second, _ := secondClaudeRecords()
	in.SecondClaudeEnrollment = &second
	if escalated {
		in.SpecifierEscalated = &agentbaseline.Selection{Model: escalatedSpecifierModel, Effort: domain.EffortMax}
	}
	return in
}

// renderedFiles is a rendered tree's files by path.
func renderedFiles(in agentbaseline.TreeInput) agenttree.Files {
	return must(agenttree.Render(must(agentbaseline.Tree(in))))
}

// A second writer subscription gets no role line: the routing policy reaches
// it through task lines that name its enrollment. So without an escalated
// specifier the tree is the one-writer tree byte for byte, and a tree adopted
// before the second writer keeps its lines, and the routing revisions and
// approvals that name them stand. With an escalated specifier the second
// enrollment gets the one escalated line keyed by it, and the lineup only
// gains that line.
func TestBaselineTreeAddsTheSecondWriterBesideTheFirst(t *testing.T) {
	for name, tc := range map[string]struct {
		escalated bool
		golden    string
	}{
		"plain":     {false, "baseline_tree_second_writer"},
		"escalated": {true, "baseline_tree_second_writer_escalated"},
	} {
		t.Run(name, func(t *testing.T) {
			in := secondWriterInput(tc.escalated)
			golden.Assert(t, tc.golden, renderedTree(t, in))

			one := in
			one.SecondClaudeEnrollment = nil
			first, both := renderedFiles(one), renderedFiles(in)
			if len(first) != len(both) {
				t.Fatalf("the second writer changed the tree's files: %d, %d", len(first), len(both))
			}
			for path, body := range first {
				if path == "lineup" && tc.escalated {
					for _, line := range strings.SplitAfter(string(body), "\n") {
						if line != "" && !strings.Contains(string(both[path]), line) {
							t.Fatalf("lineup lost the one-writer tree's line %q", line)
						}
					}
					continue
				}
				if string(both[path]) != string(body) {
					t.Fatalf("%s differs from the one-writer tree's:\n%s\n%s", path, both[path], body)
				}
			}

			tree := must(agenttree.Parse(both))
			secondEnrollment, secondIdentity := secondClaudeRecords()
			lineup := must(tree.ResolveLineup())
			counterpart, ok := lineup.EscalatedCounterpart(domain.RoleSpecifier, string(secondEnrollment.ID))
			if ok != tc.escalated {
				t.Fatalf("second specifier's escalated counterpart = %+v, %t", counterpart, ok)
			}
			if tc.escalated {
				line, _ := lineup.Line(domain.RoleSpecifier)
				if counterpart.Names != names(secondEnrollment, escalatedSpecifierModel, "max") ||
					counterpart.PromptDigest != line.PromptDigest {
					t.Fatalf("second escalated line = %+v beside the specifier line %+v", counterpart, line)
				}
				// It resolves only under its own identity's records.
				resolveLine(t, tree, counterpart.Names, must(agentbaseline.Launch(domain.StageNameSpecification)),
					secondEnrollment, secondIdentity)
			}
			for _, role := range agentbaseline.WriterRoles {
				if line, _ := lineup.Line(role); line.Names.Enrollment == string(secondEnrollment.ID) {
					t.Fatalf("role %s line names the second writer's enrollment: %+v", role, line)
				}
			}
		})
	}
}

func TestBaselineTreeRefusesASecondWriterThatIsNotASecondSubscription(t *testing.T) {
	first, _ := claudeRecords()
	codex, _ := codexRecords()
	for name, edit := range map[string]func(*domain.ClientEnrollment){
		"the first enrollment": func(e *domain.ClientEnrollment) { *e = first },
		"the first identity":   func(e *domain.ClientEnrollment) { e.AuthIdentityID = first.AuthIdentityID },
		"a codex enrollment":   func(e *domain.ClientEnrollment) { *e = codex },
		"another route":        func(e *domain.ClientEnrollment) { e.Route = "anthropic_other" },
	} {
		t.Run(name, func(t *testing.T) {
			in := secondWriterInput(false)
			second := *in.SecondClaudeEnrollment
			edit(&second)
			in.SecondClaudeEnrollment = &second
			if _, err := agentbaseline.Tree(in); err == nil {
				t.Fatal("Tree accepted the second enrollment")
			}
		})
	}
}
