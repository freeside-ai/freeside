package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type testAuthStoreVolumes struct {
	volume string
	// holder, when set, receives the invocation the lookup named.
	holder *domain.InvocationID
}

func (v testAuthStoreVolumes) AuthStoreVolume(
	_ context.Context, _ domain.AuthIdentityID, holder domain.InvocationID,
) (string, error) {
	if v.holder != nil {
		*v.holder = holder
	}
	return v.volume, nil
}

func testProviderHandoffInput() stage.ProviderHandoffInput {
	id := domain.InvocationID("inv-provider-spec")
	return stage.ProviderHandoffInput{
		InvocationID: id,
		RunID:        RunIDFor(id),
		Spec: exec.StartSpec{
			Base: domain.BaseRevision{
				Repo: "freeside-ai/candidate", RepositoryID: 42,
				BaseRef: "refs/heads/main", BaseSHA: strings.Repeat("a", 40),
			},
			Workspace:      WorkspaceFor(id),
			CredentialMode: domain.CredentialSubscriptionContained,
			EgressProfile:  domain.EgressProviderOnly,
			AuthIdentityID: "auth-provider-spec",
			ImageRef: domain.ImageRef(
				"127.0.0.1:5014/freeside-agent-claude@sha256:" + strings.Repeat("ab", 32),
			),
		},
		Seed:   "/daemon/seeds/" + RunIDFor(id),
		Prompt: "do the work",
		Instructions: ward.VendorInstructions{
			Vendor: domain.AgentVendorClaude,
		},
	}
}

func TestHandoffSpecBindsContainmentAndInstructions(t *testing.T) {
	t.Parallel()
	const volume = "provider-owner-credentials"
	in := testProviderHandoffInput()
	var holder domain.InvocationID
	hs, err := (claudeProvider{volumes: testAuthStoreVolumes{volume: volume, holder: &holder}}).
		HandoffSpec(context.Background(), in)
	if err != nil {
		t.Fatalf("HandoffSpec: %v", err)
	}
	// The store resolves an agent-bound attempt's generation from this id.
	if holder != in.InvocationID || hs.AuthStoreLease.Holder != in.InvocationID {
		t.Errorf("volume looked up for %q, lease held by %q, want the invocation %q",
			holder, hs.AuthStoreLease.Holder, in.InvocationID)
	}
	if len(hs.Agent.CredentialMounts) != 1 {
		t.Fatalf("credential mounts = %#v, want exactly the leased one", hs.Agent.CredentialMounts)
	}
	mount := hs.Agent.CredentialMounts[0]
	if mount.Volume != volume || mount.Writable ||
		mount.Manifest != ward.CredentialManifestSetupToken {
		t.Errorf("credential mount = %#v, want the trusted token volume read-only", mount)
	}
	if mount.Target == "/root/.claude" || strings.HasPrefix(mount.Target, "/root/.claude/") {
		t.Errorf("credential mount target %q collides with the instruction mount", mount.Target)
	}
	if hs.Agent.LaunchState != ward.LaunchStateClaudeClean {
		t.Errorf("launch state = %q, want clean Claude state", hs.Agent.LaunchState)
	}
	command := strings.Join(hs.Agent.Command, " ")
	for _, required := range []string{
		"setpriv --reuid=" + agentUID,
		"--bounding-set=-all --no-new-privs",
		writerOutcomePath,
	} {
		if !strings.Contains(command, required) {
			t.Errorf("agent command omits privilege/outcome boundary %q", required)
		}
	}
	if hs.Agent.OutcomeMarkerPath != writerOutcomePath {
		t.Errorf("outcome marker = %q, want %q", hs.Agent.OutcomeMarkerPath, writerOutcomePath)
	}
	if hs.AuthStoreLease == nil || hs.AuthStoreLease.AuthIdentityID != in.Spec.AuthIdentityID ||
		hs.AuthStoreLease.Holder != in.InvocationID {
		t.Errorf("auth store lease = %#v, want the admitted identity and invocation", hs.AuthStoreLease)
	}
	wantEnv := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=" + workspaceDir,
	}
	if !reflect.DeepEqual(hs.Agent.Env, wantEnv) {
		t.Errorf("agent env = %#v, want %#v", hs.Agent.Env, wantEnv)
	}
}

// The adapter accepts provider_registry and carries only the profile: the
// registry set comes from the run's policy, which the stage driver reads.
func TestHandoffSpecAcceptsProviderRegistry(t *testing.T) {
	t.Parallel()
	in := testProviderHandoffInput()
	in.Spec.EgressProfile = domain.EgressProviderRegistry
	hs, err := (claudeProvider{volumes: testAuthStoreVolumes{volume: "provider-volume"}}).
		HandoffSpec(context.Background(), in)
	if err != nil {
		t.Fatalf("HandoffSpec: %v", err)
	}
	if hs.Agent.EgressProfile != domain.EgressProviderRegistry || len(hs.RegistryHosts) != 0 {
		t.Errorf("handoff = %s with registries %v, want provider_registry and no adapter-chosen set",
			hs.Agent.EgressProfile, hs.RegistryHosts)
	}
}

func TestHandoffSpecRefusesUnsupportedContainment(t *testing.T) {
	t.Parallel()
	provider := claudeProvider{volumes: testAuthStoreVolumes{volume: "provider-volume"}}
	tests := []struct {
		name string
		edit func(*exec.StartSpec)
	}{
		{"api key isolated", func(s *exec.StartSpec) { s.CredentialMode = domain.CredentialAPIKeyIsolated }},
		{"web-read egress", func(s *exec.StartSpec) { s.EgressProfile = domain.EgressProviderWebRead }},
		{"no auth identity", func(s *exec.StartSpec) { s.AuthIdentityID = "" }},
		{"foreign workspace", func(s *exec.StartSpec) { s.Workspace = "foreign-workspace" }},
	}
	// The CLI-safety rule refuses a comma and every control character; the
	// range ends are 0x00, 0x1f, and 0x7f.
	for _, delimiter := range []string{",", "\x00", "\t", "\n", "\r", "\x1f", "\x7f"} {
		tests = append(tests,
			struct {
				name string
				edit func(*exec.StartSpec)
			}{
				fmt.Sprintf("model with %q", delimiter),
				func(s *exec.StartSpec) { s.RouteModelID = "claude-opus-5-5" + delimiter + "x" },
			},
			struct {
				name string
				edit func(*exec.StartSpec)
			}{
				fmt.Sprintf("effort with %q", delimiter),
				func(s *exec.StartSpec) { s.NativeEffort = "high" + delimiter + "x" },
			})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := testProviderHandoffInput()
			tc.edit(&in.Spec)
			if _, err := provider.HandoffSpec(context.Background(), in); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("HandoffSpec error = %v, want ErrUnsupportedStart", err)
			}
		})
	}
}

// handoffCommandCases are the launch shapes one HandoffSpec path produces:
// every launch protocol, each with and without the preparation command.
// redirect is where that protocol streams the transcript while the agent runs.
var handoffCommandCases = []struct {
	name     string
	delivery stage.PromptDelivery
	prepare  []string
	redirect string
}{
	{"argument", stage.PromptArgument, nil, transcriptPath},
	{"argument-prepare", stage.PromptArgument, []string{"/usr/local/bin/freeside-project-prepare"}, transcriptPath},
	{"prompt-file", stage.PromptFileV1, nil, transcriptPath},
	{"prompt-file-prepare", stage.PromptFileV1, []string{"/usr/local/bin/freeside-project-prepare"}, transcriptPath},
	{"file-v2", stage.PromptFileV2, nil, launchTranscriptPath},
	{"file-v2-prepare", stage.PromptFileV2, []string{"/usr/local/bin/freeside-project-prepare"}, launchTranscriptPath},
}

// deferredCommand is the launch script new launches run: the prompt file on
// stdin and the launcher's own files kept out of the workspace.
func deferredCommand(prepare []string) string {
	return agentCommandWithInput(
		"< "+shellQuote(ward.PromptFilePath), "session-1", "inv-1", prepare, "", "", launcherFilesDeferred)[2]
}

// TestHandoffSpecCommandGolden pins the whole launch command of a start that
// carries neither a model nor an effort. Recovery rebuilds a running launch's
// command from its stored intent, so that command must not drift by a byte
// across a daemon upgrade.
func TestHandoffSpecCommandGolden(t *testing.T) {
	t.Parallel()
	provider := claudeProvider{volumes: testAuthStoreVolumes{volume: "provider-volume"}}
	for _, tc := range handoffCommandCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := testProviderHandoffInput()
			in.PromptDelivery, in.Preparation = tc.delivery, tc.prepare
			hs, err := provider.HandoffSpec(context.Background(), in)
			if err != nil {
				t.Fatalf("HandoffSpec: %v", err)
			}
			got, err := json.MarshalIndent(hs.Agent.Command, "", "  ")
			if err != nil {
				t.Fatalf("marshal command: %v", err)
			}
			golden.Assert(t, "handoff-command-"+tc.name, append(got, '\n'))
		})
	}
}

// TestHandoffSpecPassesModelAndEffort covers the one launch path the
// specifier, implementer, and remediator share: each flag appears exactly
// when its StartSpec field is set, as an argument of the claude -p
// invocation, and changes nothing else in the command.
func TestHandoffSpecPassesModelAndEffort(t *testing.T) {
	t.Parallel()
	provider := claudeProvider{volumes: testAuthStoreVolumes{volume: "provider-volume"}}
	const model, effort = "claude-opus-5-5", "xhigh"
	selections := []struct {
		name, model, effort, flags string
	}{
		{"both", model, effort, "--model '" + model + "' --effort '" + effort + "' "},
		{"model only", model, "", "--model '" + model + "' "},
		{"effort only", "", effort, "--effort '" + effort + "' "},
	}
	for _, tc := range handoffCommandCases {
		for _, sel := range selections {
			t.Run(tc.name+"/"+sel.name, func(t *testing.T) {
				t.Parallel()
				redirect := "> " + shellQuote(tc.redirect)
				in := testProviderHandoffInput()
				in.PromptDelivery, in.Preparation = tc.delivery, tc.prepare
				bare, err := provider.HandoffSpec(context.Background(), in)
				if err != nil {
					t.Fatalf("HandoffSpec without a selection: %v", err)
				}
				in.Spec.RouteModelID, in.Spec.NativeEffort = sel.model, sel.effort
				hs, err := provider.HandoffSpec(context.Background(), in)
				if err != nil {
					t.Fatalf("HandoffSpec: %v", err)
				}
				if len(hs.Agent.Command) != 3 || !slices.Equal(hs.Agent.Command[:2], bare.Agent.Command[:2]) {
					t.Fatalf("command = %q, want the sh -c shape", hs.Agent.Command)
				}
				// The flags are the only difference from the bare command, and
				// they sit directly before the transcript redirect, so they are
				// arguments of the same claude -p invocation.
				script, bareScript := hs.Agent.Command[2], bare.Agent.Command[2]
				if n := strings.Count(bareScript, redirect); n != 1 {
					t.Fatalf("bare command has %d transcript redirects, want 1", n)
				}
				want := strings.Replace(bareScript, redirect, sel.flags+redirect, 1)
				if script != want {
					t.Errorf("command with %s:\n got %s\nwant %s", sel.name, script, want)
				}
				invocation, _, _ := strings.Cut(script, redirect)
				if _, args, ok := strings.Cut(invocation, " claude -p "); !ok || !strings.HasSuffix(args, sel.flags) {
					t.Errorf("flags %q are not arguments of the claude -p invocation", sel.flags)
				}
			})
		}
	}
}

// TestHandoffSpecShellQuotesModelAndEffort proves neither value can end its
// quoted word: an apostrophe is the only byte single quotes do not protect.
func TestHandoffSpecShellQuotesModelAndEffort(t *testing.T) {
	t.Parallel()
	in := testProviderHandoffInput()
	in.Spec.RouteModelID = `opus'; touch /pwned; '`
	in.Spec.NativeEffort = `$(id) high'`
	hs, err := (claudeProvider{volumes: testAuthStoreVolumes{volume: "provider-volume"}}).
		HandoffSpec(context.Background(), in)
	if err != nil {
		t.Fatalf("HandoffSpec: %v", err)
	}
	want := `--model 'opus'\''; touch /pwned; '\''' --effort '$(id) high'\''' > `
	if !strings.Contains(hs.Agent.Command[2], want) {
		t.Errorf("command omits the quoted selection %q:\n%s", want, hs.Agent.Command[2])
	}
}

func TestPromptLimitLeavesLinuxArgumentHeadroom(t *testing.T) {
	t.Parallel()
	// Apostrophes are shellQuote's worst case: each input byte expands to the
	// four-byte sequence that closes, escapes, and reopens the quoted word.
	command := agentCommand(
		strings.Repeat("'", maxPromptBytes),
		"00000000-0000-4000-8000-000000000000",
		"inv-headroom",
		nil, "", "",
	)[2]
	if len(command) >= linuxMaxArgumentBytes {
		t.Fatalf("max prompt produces %d-byte sh argument, Linux limit is %d",
			len(command), linuxMaxArgumentBytes)
	}
}

func TestPhase1ASpecifierPromptLeavesEnvelopeHeadroom(t *testing.T) {
	t.Parallel()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	promptPath := filepath.Join(filepath.Dir(sourceFile), "../../../../prompts/phase-1a/specifier.md")
	prompt, err := os.ReadFile(promptPath) //nolint:gosec // fixed repository test fixture
	if err != nil {
		t.Fatal(err)
	}
	const promptPackageBudget = 4 << 10
	if len(prompt) > promptPackageBudget {
		t.Fatalf("specifier prompt = %d bytes, want <= %d to preserve envelope headroom",
			len(prompt), promptPackageBudget)
	}
	if len(prompt) >= maxPromptBytes {
		t.Fatalf("specifier prompt consumes the %d-byte rendered prompt ceiling", maxPromptBytes)
	}
	if !bytes.HasPrefix(prompt, []byte(textPriorPromptPackageDirective)) {
		t.Fatal("specifier prompt does not enable authenticated prior-artifact rendering")
	}
}

func TestPhase1ASummaryPromptContracts(t *testing.T) {
	t.Parallel()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	promptDir := filepath.Join(filepath.Dir(sourceFile), "../../../../prompts/phase-1a")
	implementer, err := os.ReadFile(filepath.Join(promptDir, "implementer.md")) //nolint:gosec // fixed repository fixture
	if err != nil {
		t.Fatal(err)
	}
	specifier, err := os.ReadFile(filepath.Join(promptDir, "specifier.md")) //nolint:gosec // fixed repository fixture
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		export.SummaryEvidencePath,
		"State what changed and why, what you left undone or out of scope, and what remains uncertain.",
		"Assert a verifiable outcome only by naming the command, check, diff, or artifact it comes from",
		// The typed stop (#990): the blocked file, its version, every kind, and
		// the no-changes rule the importer enforces.
		export.BlockedEvidencePath,
		domain.BlockedOutcomeEncodingVersion,
		"leave no repository changes in the workspace, write no commit plan",
		"1 to 8 decisions, 2 to 6 options each, 4 KiB per text field",
	} {
		if !bytes.Contains(implementer, []byte(required)) {
			t.Errorf("implementer prompt omits %q", required)
		}
	}
	for _, kind := range domain.AllBlockedKinds {
		if kind == domain.BlockedKindCommitPlanCollision {
			if bytes.Contains(implementer, []byte("`"+string(kind)+"`")) {
				t.Errorf("implementer prompt advertises unusable blocked kind %q", kind)
			}
			continue
		}
		if !bytes.Contains(implementer, []byte("`"+string(kind)+"`")) {
			t.Errorf("implementer prompt omits blocked kind %q", kind)
		}
	}
	if bytes.Contains(implementer, []byte("report the exact blocker")) {
		t.Error("implementer prompt still carries the interim untyped stop protocol")
	}
	for _, required := range []string{
		// The summary's advisory shape (#1461): the writer's headings, the
		// word target, and every concern listed whatever the length.
		"Under `## Change`, the proposal and why", "80–120 words",
		"under `## Remaining concerns`, list every key question, open decision, uncertainty, and dissent",
		"drop none for length",
		"Never claim verification",
		// The needs_decision form (#990) and the limits the decoder pins.
		`{"decisions":[{"question"`,
		"8 decisions, 2 to 6 options each, 4 KiB per text field",
		"`recommendation` equals one option `label` exactly",
		"return `decisions` instead of a specification",
		// The sketch rule (#1329): a source that leaves outcome, scope, or
		// non-goals unresolved gets a clarification round before any research
		// or specification, so a later edit cannot drop it silently.
		"gets `decisions` before requesting research or a specification",
		// The first-turn/resume exit from the sketch round (#1329): the ask
		// fires only with no answering human_feedback, so the answered retry
		// proceeds to the specification instead of re-asking to the iteration
		// limit. Pinned so a later edit cannot silently drop the exit.
		"with no answering `human_feedback`",
	} {
		if !bytes.Contains(specifier, []byte(required)) {
			t.Errorf("specifier prompt omits %q", required)
		}
	}
	if bytes.Contains(specifier, []byte("open owner decision")) {
		t.Error("specifier prompt still lists owner decisions in the summary instead of returning them")
	}
	remediator, err := os.ReadFile(filepath.Join(promptDir, "remediator.md")) //nolint:gosec // fixed repository fixture
	if err != nil {
		t.Fatal(err)
	}
	// These assertions pin writing guidance, not a guarantee of model behavior.
	for name, prompt := range map[string][]byte{"implementer": implementer, "remediator": remediator} {
		for _, required := range []string{
			"`## Change`", "`## Remaining concerns`", "`## Details`",
			"80–120 words", "never omit a concern to meet that target",
			"uncertainty, unresolved questions, dissent, and unfinished obligations",
			"a flat list, one concern per item, most important first",
			"advisory writing guidance, not a required format",
			"retained artifacts and the full Result report",
		} {
			if !bytes.Contains(prompt, []byte(required)) {
				t.Errorf("%s prompt omits summary guidance %q", name, required)
			}
		}
		// A repository-required artifact is part of done (#1542), but the
		// commit-plan hard limits still override project conventions.
		for _, required := range []string{
			"An artifact the target repository's own instructions require for this kind of change",
			"is part of done, not scope widening.",
			"When its path is outside the allowed paths, use Required Work Outside Scope below instead of skipping it.",
			"The following are hard limits that override any project convention",
		} {
			if !bytes.Contains(prompt, []byte(required)) {
				t.Errorf("%s prompt omits repository-required artifact guidance %q", name, required)
			}
		}
	}
	if want := "When a finding reports a missing repository-required artifact, add the artifact rather than arguing it out of scope"; !bytes.Contains(remediator, []byte(want)) {
		t.Errorf("remediator prompt omits %q", want)
	}
	// The remediator plays the prior-artifact-rendering role opposite the
	// implementer, so the composition the driver enforces at startup validates.
	if err := ValidatePromptPackageRoles(implementer, remediator); err != nil {
		t.Errorf("ValidatePromptPackageRoles(implementer, remediator): %v", err)
	}
	// It carries the implementer's summary, commit-plan, and blocked-outcome
	// contract verbatim, so their shared required strings hold in the remediator
	// too. The blocked outcome is the modeled owner-decision interrupt for both
	// remediation and operator-feedback rounds (#1314); the pushback claim, not
	// the blocked outcome, is the separate channel for declining a finding.
	for _, required := range []string{
		export.SummaryEvidencePath,
		"State what changed and why, what you left undone or out of scope, and what remains uncertain.",
		"Assert a verifiable outcome only by naming the command, check, diff, or artifact it comes from",
		"Together the groups must exactly cover the final change set",
		export.BlockedEvidencePath,
		domain.BlockedOutcomeEncodingVersion,
		"leave no repository changes in the workspace, write no commit plan",
		"1 to 8 decisions, 2 to 6 options each, 4 KiB per text field",
	} {
		if !bytes.Contains(remediator, []byte(required)) {
			t.Errorf("remediator prompt omits %q", required)
		}
	}
	for _, kind := range domain.AllBlockedKinds {
		if kind == domain.BlockedKindCommitPlanCollision {
			if bytes.Contains(remediator, []byte("`"+string(kind)+"`")) {
				t.Errorf("remediator prompt advertises unusable blocked kind %q", kind)
			}
			continue
		}
		if !bytes.Contains(remediator, []byte("`"+string(kind)+"`")) {
			t.Errorf("remediator prompt omits blocked kind %q", kind)
		}
	}
	if summaryFixtureConforms("All tests pass.") {
		t.Fatal("bare verdict fixture passed the summary composition assertion")
	}
	if !summaryFixtureConforms("`go test ./...` passed in my run.") {
		t.Fatal("command-cited verification fixture failed the summary composition assertion")
	}
	if summaryFixtureConforms("All tests pass. `note`") {
		t.Fatal("unrelated inline code passed the summary composition assertion")
	}
}

func summaryFixtureConforms(summary string) bool {
	lower := strings.ToLower(summary)
	if !strings.Contains(lower, "all tests pass") {
		return true
	}
	for _, origin := range []string{"command", "check", "diff", "artifact", "verification run"} {
		if strings.Contains(lower, origin) {
			return true
		}
	}
	parts := strings.Split(summary, "`")
	for index := 1; index < len(parts); index += 2 {
		if len(strings.Fields(parts[index])) > 1 || strings.Contains(parts[index], "/") {
			return true
		}
	}
	return false
}

func TestValidatePromptPackageRoles(t *testing.T) {
	t.Parallel()
	directed := []byte(textPriorPromptPackageDirective + "specifier")
	if err := ValidatePromptPackageRoles([]byte("implementer"), directed); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePromptPackageRoles(directed, directed); err == nil {
		t.Fatal("implementation prompt package enabled prior artifacts")
	}
	if err := ValidatePromptPackageRoles([]byte("implementer"), []byte("specifier")); err == nil {
		t.Fatal("specification prompt package omitted prior-artifact directive")
	}
}

func TestRenderPromptRejectsNonUTF8Inputs(t *testing.T) {
	t.Parallel()
	for _, field := range []struct {
		name   string
		mutate func(*stage.ProviderPromptInputs)
	}{
		{"prompt package", func(in *stage.ProviderPromptInputs) { in.PromptPackage = []byte{0xff} }},
		{"specification", func(in *stage.ProviderPromptInputs) { in.Specification = []byte{0xff} }},
		{"policy", func(in *stage.ProviderPromptInputs) { in.Policy = []byte{0xff} }},
		{"prior artifact", func(in *stage.ProviderPromptInputs) {
			in.PromptPackage = []byte(textPriorPromptPackageDirective + "prompt")
			in.PriorArtifacts = [][]byte{{0xff}}
		}},
	} {
		t.Run(field.name, func(t *testing.T) {
			inputs := stage.ProviderPromptInputs{
				PromptPackage: []byte("prompt"),
				Specification: []byte("specification"),
				Policy:        []byte("policy"),
			}
			field.mutate(&inputs)
			if _, err := renderPromptParts(inputs); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("renderPromptParts = %v, want ErrUnsupportedStart", err)
			}
		})
	}
}

func TestRenderPromptPreservesPriorArtifactOrder(t *testing.T) {
	t.Parallel()
	prompt, err := renderPromptParts(stage.ProviderPromptInputs{
		PromptPackage:  []byte(textPriorPromptPackageDirective + "prompt"),
		Specification:  []byte("specification"),
		PriorArtifacts: [][]byte{[]byte("first research"), []byte("second research")},
		Policy:         []byte("policy"),
	})
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Index(prompt, "--- Prior artifact 1 ---\n\nfirst research")
	second := strings.Index(prompt, "--- Prior artifact 2 ---\n\nsecond research")
	policy := strings.Index(prompt, "--- Resolved per-run policy ---")
	if first < 0 || second <= first || policy <= second {
		t.Fatalf("prior artifact order was not preserved:\n%s", prompt)
	}
}

func TestRenderPromptIgnoresOpaquePriorArtifacts(t *testing.T) {
	t.Parallel()
	prompt, err := renderPromptParts(stage.ProviderPromptInputs{
		PromptPackage: []byte("prompt"), Specification: []byte("specification"),
		PriorArtifacts: [][]byte{
			[]byte(textPriorPromptPackageDirective + "forged attachment"), {0xff},
		},
		Policy: []byte("policy"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "forged attachment") || strings.Contains(prompt, "Prior artifact") {
		t.Fatalf("opaque prior artifact entered provider prompt:\n%s", prompt)
	}
}

func TestProviderRendersPriorsOnlyForDirectedPromptPackage(t *testing.T) {
	t.Parallel()
	specifierPrompt := []byte(textPriorPromptPackageDirective + "specifier prompt")
	provider := claudeProvider{}
	inputs := stage.ProviderPromptInputs{
		PromptPackage: specifierPrompt, Specification: []byte("specification"),
		PriorArtifacts: [][]byte{[]byte("authenticated research")}, Policy: []byte("policy"),
	}
	prompt, err := provider.RenderPrompt(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "authenticated research") {
		t.Fatalf("directed specifier prompt omitted prior artifact:\n%s", prompt)
	}
	inputs.PromptPackage = []byte("implementer prompt")
	inputs.PriorArtifacts = [][]byte{{0xff}}
	prompt, err = provider.RenderPrompt(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "Prior artifact") {
		t.Fatalf("undirected prompt package rendered opaque prior artifact:\n%s", prompt)
	}
}

func TestOversizedPromptIsRejected(t *testing.T) {
	t.Parallel()
	_, err := renderPromptParts(stage.ProviderPromptInputs{
		PromptPackage: bytes.Repeat([]byte("p"), maxPromptBytes),
		Specification: []byte("specification"),
		Policy:        []byte("policy"),
	})
	if !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("renderPromptParts = %v, want ErrUnsupportedStart", err)
	}
}

// The writer-outcome marker's integrity rests entirely on this command's
// filesystem topology, not on the nonce. An adversarial probe against the
// pinned image under Apple container confirmed both halves: pid 1's cmdline is
// readable at UID 1001, so the writer can always learn the nonce, while the
// forge itself fails at every step (writing into, removing, or renaming the
// root-owned control directory inside the sticky evidence directory;
// signalling pid 1; regaining privilege through a setuid copy). The nonce
// proves the marker is this run's, never that the writer did not author it.
//
// So this command string is a security control: an edit that reorders the
// chown sweep past the control directory's creation, lets others write to
// that directory, or writes into it before closing it, silently hands the
// writer the ability to report its own success. Ordering is asserted by
// position rather than by matching the whole script, so ordinary edits stay
// cheap. The command checked is the one new launches run; the golden files
// pin the two older ones byte for byte.
func TestAgentCommandKeepsTheOutcomeMarkerOutOfWriterReach(t *testing.T) {
	t.Parallel()
	script := deferredCommand(nil)
	evidenceDir := path.Dir(transcriptPath)
	controlDir := path.Dir(writerOutcomePath)

	if controlDir == evidenceDir {
		t.Fatalf("outcome marker sits directly in the agent-writable evidence directory %q", evidenceDir)
	}
	if path.Dir(controlDir) != evidenceDir {
		t.Fatalf("control directory %q is not inside the exported evidence directory %q",
			controlDir, evidenceDir)
	}
	// The in-flight transcript must sit on the container's own filesystem: a
	// path under a mount would either show it to the agent's tools or keep it
	// on a volume that outlives the launcher.
	for _, mount := range []string{
		workspaceDir, credentialMountTarget, ward.ClaudeConfigRootTarget,
		ward.PromptFileTarget, path.Dir(instructionBundlePath),
	} {
		if launchDir == mount || strings.HasPrefix(launchDir, mount+"/") || strings.HasPrefix(mount, launchDir+"/") {
			t.Errorf("launch directory %q overlaps the mount at %q", launchDir, mount)
		}
	}

	at := func(needle string) int {
		t.Helper()
		i := strings.Index(script, needle)
		if i < 0 {
			t.Fatalf("agent command omits %q:\n%s", needle, script)
		}
		return i
	}

	// Root owns both directories the writer must not control, and the sticky
	// bit is what stops an unprivileged writer unlinking or renaming a
	// root-owned entry out of a world-writable directory. While the writer
	// runs the control directory is empty and readable, never writable.
	stickyEvidence := at("mkdir -p '" + evidenceDir + "'; chown 0:0 '" + evidenceDir +
		"'; chmod 1777 '" + evidenceDir + "'")
	openControl := at("mkdir -p '" + controlDir + "'; chown 0:0 '" + controlDir +
		"'; chmod 0755 '" + controlDir + "'")
	stickyWorkspace := at("chown 0:0 '" + workspaceDir + "'; chmod 1777 '" + workspaceDir + "'")
	privateLaunch := at("rm -rf '" + launchDir + "'; mkdir '" + launchDir + "'; chown 0:0 '" + launchDir +
		"'; chmod 0700 '" + launchDir + "'")

	// The writer owns the repository it edits and nothing else.
	dropWorkspace := at("chown -hR " + agentUID + ":" + agentGID)
	drop := at("setpriv --reuid=" + agentUID + " --regid=" + agentGID)
	for _, flag := range []string{
		"--clear-groups", "--inh-caps=-all", "--ambient-caps=-all",
		"--bounding-set=-all", "--no-new-privs",
	} {
		if !strings.Contains(script, flag) {
			t.Errorf("privilege drop omits %q", flag)
		}
	}

	marker := at("> '" + writerOutcomePath + "'")
	dependencyCleanup := at("rm -rf -- '" + workspaceDir + "/node_modules'")
	if !strings.Contains(script, ward.WriterNoncePlaceholder) {
		t.Error("agent command carries no writer nonce placeholder for ward to substitute")
	}

	// Every mode must be in place before the writer exists, and the marker
	// must be written after it: a control directory created after the drop, or
	// a chown sweep that runs after it, would leave a window the writer owns.
	if stickyWorkspace > drop || stickyEvidence > drop ||
		openControl > drop || privateLaunch > drop || dropWorkspace > drop {
		t.Error("the workspace, evidence, control, and launch boundaries are not all established before the privilege drop")
	}
	if marker < drop {
		t.Error("the outcome marker is written before the writer runs")
	}
	if dependencyCleanup < drop || dependencyCleanup > marker {
		t.Error("the runtime dependency tree is not removed after the writer and before its outcome marker")
	}
	if openControl < stickyEvidence {
		t.Error("the control directory is created before its sticky parent, so its mode is not the one that survives")
	}

	// Before the writer exits the launcher puts none of its own files in the
	// workspace (#1929): the transcript streams to the launch directory and
	// nothing names the descriptor, the final transcript, or a control file.
	redirect := at("> '" + launchTranscriptPath + "' 2>&1")
	if redirect < drop {
		t.Error("the transcript redirect does not belong to the dropped invocation")
	}
	for _, launcherPath := range []string{transcriptDescriptorPath, transcriptPath, controlDir + "/"} {
		if i := strings.Index(script, launcherPath); i >= 0 && i < redirect {
			t.Errorf("the launcher names %q before the writer has exited", launcherPath)
		}
	}

	// Once the writer has exited, the launcher takes away its write access
	// to the evidence directory and closes the control directory in one step,
	// before anything is written into either. Both files are then renamed out
	// of the control directory onto their final paths before the marker says
	// the run is over.
	closeControl := at("chmod 0755 '" + evidenceDir + "'; chmod 0700 '" + controlDir + "'")
	if closeControl < redirect {
		t.Error("the evidence and control directories are closed before the writer has exited")
	}
	if firstWrite := at(controlDir + "/"); firstWrite < closeControl {
		t.Error("the launcher writes into the control directory before closing it")
	}
	for _, final := range []string{transcriptDescriptorPath, transcriptPath} {
		staged := controlDir + "/" + path.Base(final)
		move := at("rm -rf -- '" + final + "'; mv -T '" + staged + "' '" + final + "'")
		if move < closeControl || move > marker {
			t.Errorf("%q is not moved into place after the control directory closes and before the marker", final)
		}
	}

	for _, field := range []string{
		export.EvidenceSourceVersion, `"label":"agent-transcript"`,
		`"path":"` + transcriptEvidencePath + `"`,
		`"sensitivity_class":"sensitive"`,
		`"producer_invocation_id":"inv-1"`,
	} {
		if !strings.Contains(script, field) {
			t.Errorf("transcript descriptor omits %q", field)
		}
	}
	summaryGuard := at("if [ -f '" + export.SummaryEvidencePath + "' ] && [ ! -L '" +
		export.SummaryEvidencePath + "' ]")
	if summaryGuard < drop || summaryGuard > marker {
		t.Error("the summary descriptor is not declared after the writer and before its outcome marker")
	}
	for _, field := range []string{
		`"label":"` + export.SummaryEvidenceLabel + `"`,
		`"media_type":"text/markdown"`,
		`"path":"` + export.SummaryEvidencePath + `"`,
		`"head_binding":"head_independent"`,
		`"sensitivity_class":"sensitive"`,
		`"producer_invocation_id":"inv-1"`,
	} {
		if !strings.Contains(script[summaryGuard:], field) {
			t.Errorf("summary descriptor omits %q", field)
		}
	}
	blockedGuard := at("if [ -f '" + export.BlockedEvidencePath + "' ] && [ ! -L '" +
		export.BlockedEvidencePath + "' ]")
	if blockedGuard < summaryGuard || blockedGuard > marker {
		t.Error("the blocked descriptor is not declared after the writer and before its outcome marker")
	}
	for _, field := range []string{
		`"label":"` + export.BlockedEvidenceLabel + `"`,
		`"media_type":"application/json"`,
		`"path":"` + export.BlockedEvidencePath + `"`,
		`"head_binding":"head_independent"`,
		`"sensitivity_class":"normal"`,
		`"producer_invocation_id":"inv-1"`,
	} {
		if !strings.Contains(script[blockedGuard:], field) {
			t.Errorf("blocked descriptor omits %q", field)
		}
	}

	// The git exclude is written as root into a tree the writer will own, so
	// it must run while that tree is still only the daemon's seed.
	if exclude := at(">> '" + workspaceDir + "/.git/info/exclude'"); exclude > dropWorkspace {
		t.Error("the git exclude is written after the workspace is handed to the writer")
	}
}

// TestFixedSourceDescriptorComposes proves the shell-composed descriptor
// (prefix, comma-joined fragments, closing brackets) is exactly the encoded
// manifest for every present-source combination, so the post-writer shell
// cannot declare a shape the export helper rejects.
func TestFixedSourceDescriptorComposes(t *testing.T) {
	t.Parallel()
	transcript := export.EvidenceSource{
		Label: "agent-transcript", MediaType: "application/jsonl", Path: transcriptEvidencePath,
		HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivitySensitive,
		ProducerInvocationID: "inv-1",
	}
	summary := export.EvidenceSource{
		Label: export.SummaryEvidenceLabel, MediaType: "text/markdown", Path: export.SummaryEvidencePath,
		HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivitySensitive,
		ProducerInvocationID: "inv-1",
	}
	blocked := export.EvidenceSource{
		Label: export.BlockedEvidenceLabel, MediaType: "application/json", Path: export.BlockedEvidencePath,
		HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivityNormal,
		ProducerInvocationID: "inv-1",
	}
	publication := export.EvidenceSource{
		Label: export.PublicationEvidenceLabel, MediaType: "text/markdown", Path: export.PublicationEvidencePath,
		HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivityNormal,
		ProducerInvocationID: "inv-1",
	}
	for _, sources := range [][]export.EvidenceSource{
		{transcript, summary},
		{transcript, blocked},
		{transcript, summary, blocked},
		{transcript, publication},
		{transcript, publication, summary},
		{transcript, publication, summary, blocked},
	} {
		fragments := make([]string, 0, len(sources))
		for _, source := range sources {
			fragments = append(fragments, evidenceSourceFragment(source.Label, source))
		}
		composed := evidenceDescriptorPrefix() + strings.Join(fragments, ",") + "]}"
		want, err := json.Marshal(export.EvidenceSourceManifest{
			Version: export.EvidenceSourceVersion, Sources: sources,
		})
		if err != nil {
			t.Fatal(err)
		}
		if composed != string(want) {
			t.Fatalf("composed descriptor = %s, want %s", composed, want)
		}
		if _, err := export.DecodeEvidenceSourceManifest([]byte(composed)); err != nil {
			t.Fatalf("composed descriptor does not decode: %v", err)
		}
	}
}

func TestPublicMetadataLauncherAndPromptContract(t *testing.T) {
	script := deferredCommand(nil)
	guard := "if [ -f '" + export.PublicationEvidencePath + "' ] && [ ! -L '" + export.PublicationEvidencePath + "' ]"
	fragment := evidenceSourceFragment("publication", export.EvidenceSource{
		Label: export.PublicationEvidenceLabel, MediaType: "text/markdown", Path: export.PublicationEvidencePath,
		HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivityNormal,
		ProducerInvocationID: "inv-1",
	})
	if !strings.Contains(script, guard) || !strings.Contains(script, fragment) {
		t.Fatal("launcher lost fixed public provenance or optional safe-file guard")
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate source")
	}
	for _, name := range []string{"implementer.md", "remediator.md"} {
		body, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "../../../../prompts/phase-1a", name)) //nolint:gosec // fixed prompt fixtures
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{export.PublicationEvidencePath, "entire current change", "8 KiB", "256 UTF-8 bytes", "`# ` followed by the title you write", "Do not copy the private summary", "Agent-reported implementation (claim)"} {
			if !bytes.Contains(body, []byte(required)) {
				t.Errorf("%s omits %s", name, required)
			}
		}
		// Agents copied this placeholder verbatim as the PR title (#1464).
		if bytes.Contains(body, []byte("Outcome title")) {
			t.Errorf("%s shows a copyable placeholder title", name)
		}
	}
}

// The implementer hydration (#522) is a security-relevant ordering: it must
// run as root before the ownership sweep so the hydrated tree is dropped to
// the agent with everything else, a nonzero exit must skip the agent behind a
// distinct pre-agent sentinel, and the tree must be removed before the outcome
// marker so the git-blind export never sees it.
func TestAgentCommandHydratesBeforeTheOwnershipDrop(t *testing.T) {
	t.Parallel()
	prepare := []string{"/usr/local/bin/freeside-project-prepare"}
	script := deferredCommand(prepare)

	at := func(needle string) int {
		t.Helper()
		i := strings.Index(script, needle)
		if i < 0 {
			t.Fatalf("agent command omits %q:\n%s", needle, script)
		}
		return i
	}

	// The preparation runs in the workspace with the verifier's HOME and
	// LC_ALL, so the implementer resolves the same lockfile-pinned toolchain.
	hydrate := at("( cd '" + workspaceDir + "' && HOME='" + prepareHome +
		"' LC_ALL=C '/usr/local/bin/freeside-project-prepare' )")
	prepareStatus := at("prepare_status=$?")
	sweep := at("chown -hR " + agentUID + ":" + agentGID)
	drop := at("setpriv --reuid=" + agentUID)
	marker := at("> '" + writerOutcomePath + "'")
	cleanup := at("rm -rf -- '" + workspaceDir + "/node_modules'")

	if hydrate > sweep {
		t.Error("hydration runs after the ownership sweep, so the hydrated tree is not dropped to the agent")
	}
	// Hydration runs project code as root. The git exclude must already be
	// written, while the tree is still only the daemon's seed.
	if exclude := at(">> '" + workspaceDir + "/.git/info/exclude'"); exclude > hydrate {
		t.Error("the git exclude is written after project code has run in the workspace")
	}
	if prepareStatus > sweep {
		t.Error("the preparation exit status is captured after the ownership sweep")
	}

	// A nonzero preparation exit writes the distinct sentinel and gates the
	// agent behind the elif, so the agent never launches on a failed hydrate.
	guard := at("if [ \"$prepare_status\" -ne 0 ]; then status=" +
		strconv.Itoa(writerOutcomePrepareFailed) + "; elif [ -s '" + credentialTokenPath + "' ]")
	if guard > drop {
		t.Error("the preparation-failure guard is not established before the agent launch")
	}
	if cleanup < drop || cleanup > marker {
		t.Error("the hydrated tree is not removed after the writer and before its outcome marker")
	}
	if writerOutcomePrepareFailed == 86 {
		t.Error("the preparation sentinel collides with the token-missing status")
	}
}

// The attended launch command carries no hydration when no preparation is
// configured, so the 1A.0 conversation-turn path is byte-for-byte unchanged.
func TestAgentCommandWithoutPreparationIsUnchanged(t *testing.T) {
	t.Parallel()
	base := strings.Join(agentCommand("do the work", "session-1", "inv-1", nil, "", ""), " ")
	empty := strings.Join(agentCommand("do the work", "session-1", "inv-1", []string{}, "", ""), " ")
	if base != empty {
		t.Fatal("nil and empty preparation produce different launch commands")
	}
	for _, marker := range []string{"prepare_status", prepareHome, "LC_ALL=C"} {
		if strings.Contains(base, marker) {
			t.Errorf("launch command carries hydration text %q without a preparation command", marker)
		}
	}
	if !strings.Contains(base, "status=86; if [ -s '"+credentialTokenPath+"' ]") {
		t.Error("the token guard is not the plain single-branch form without a preparation command")
	}
}

func TestRuntimeDependencyCleanupDoesNotFollowReplacementSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := filepath.Join(workspace, "node_modules")
	if err := os.Symlink(outside, dependencies); err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command( //nolint:gosec // G204: fixed shell snippet with a test-owned temp path
		"sh", "-c", `rm -rf -- "$1"`, "sh", dependencies,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dependency cleanup: %v: %s", err, output)
	}
	if _, err := os.Lstat(dependencies); !os.IsNotExist(err) {
		t.Fatalf("replacement symlink survived cleanup: %v", err)
	}
	body, err := os.ReadFile(sentinel) //nolint:gosec // G304: test-owned path under t.TempDir
	if err != nil || string(body) != "keep" {
		t.Fatalf("cleanup followed replacement symlink: body=%q err=%v", body, err)
	}
}

// launchRig is a relocated copy of the writer container's filesystem: the
// generated launch script runs against it with every absolute path moved
// under one temporary root, and stand-ins on PATH for the three commands an
// unprivileged test cannot run.
type launchRig struct {
	root, workspace, bin, probe, sentinel string
}

const launchRigNonce = "nonce-1"

// The stand-in agent records what a tool walking the repository root meets,
// then leaves the entries a hostile writer would: a symlink where the
// launcher's descriptor goes and a directory where its transcript goes.
const launchRigAgent = `#!/bin/sh
set -eu
find . -mindepth 1 | LC_ALL=C sort > "$FREESIDE_RIG_PROBE/entries"
find . -mindepth 1 \( ! -perm -004 -o \( -type d ! -perm -001 \) \) > "$FREESIDE_RIG_PROBE/unreadable"
printf 'summary\n' > .freeside-evidence/summary.md
ln -s "$FREESIDE_RIG_SENTINEL" .freeside-evidence/evidence.json
mkdir .freeside-evidence/agent-transcript.jsonl
printf 'planted\n' > .freeside-evidence/agent-transcript.jsonl/inner
printf 'stand-in transcript\n'
`

func newLaunchRig(t *testing.T) launchRig {
	t.Helper()
	root := t.TempDir()
	rig := launchRig{
		root: root, workspace: filepath.Join(root, "workspace"), bin: filepath.Join(root, "bin"),
		probe: filepath.Join(root, "probe"), sentinel: filepath.Join(root, "outside", "sentinel"),
	}
	files := []struct {
		path, body string
		mode       os.FileMode
	}{
		// The seed is world-readable, as a checkout is, so every unreadable
		// entry the walk finds is one the launcher added.
		{filepath.Join(rig.workspace, "README.md"), "seed\n", 0o644},
		{filepath.Join(rig.workspace, "src", "main.js"), "seed\n", 0o644},
		{filepath.Join(rig.workspace, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644},
		{filepath.Join(root, credentialTokenPath), "token\n", 0o600},
		{filepath.Join(root, ward.PromptFilePath), "prompt\n", 0o600},
		{rig.sentinel, "keep\n", 0o600},
		{filepath.Join(rig.bin, "chown"), "#!/bin/sh\nexit 0\n", 0o755},
		{filepath.Join(rig.bin, "setpriv"), "#!/bin/sh\nwhile [ \"${1#--}\" != \"$1\" ]; do shift; done\nexec \"$@\"\n", 0o755},
		{filepath.Join(rig.bin, "claude"), launchRigAgent, 0o755},
		{
			filepath.Join(rig.bin, "prepare"),
			"#!/bin/sh\nmkdir -p node_modules/dep && printf 'dep\\n' > node_modules/dep/index.js\n", 0o755,
		},
	}
	for _, dir := range []string{
		rig.probe, filepath.Join(root, "root"),
		filepath.Join(root, ward.ClaudeContinuityTarget), filepath.Join(root, ward.ClaudeSessionScratchTarget),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range files {
		dirMode := os.FileMode(0o750)
		if strings.HasPrefix(file.path, rig.workspace) {
			dirMode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(file.path), dirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file.path, []byte(file.body), file.mode); err != nil {
			t.Fatal(err)
		}
		// WriteFile and MkdirAll apply the umask; the walk reads exact bits.
		if err := os.Chmod(file.path, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{rig.workspace, filepath.Join(rig.workspace, "src"), filepath.Join(rig.workspace, ".git")} {
		if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: test fixture modes are the subject
			t.Fatal(err)
		}
	}
	return rig
}

// script is the real launch command, relocated under the rig's root. Every
// absolute path the command names is single-quoted, so a path the relocation
// missed is found here and not by a write outside the rig.
func (rig launchRig) script(t *testing.T) string {
	t.Helper()
	script := deferredCommand([]string{filepath.Join(rig.bin, "prepare")})
	script = strings.NewReplacer(
		"'"+workspaceDir, "'"+rig.workspace,
		"'/var/lib/freeside", "'"+filepath.Join(rig.root, "var/lib/freeside"),
		"'"+prepareHome, "'"+filepath.Join(rig.root, prepareHome),
		"'"+path.Dir(instructionBundlePath), "'"+filepath.Join(rig.root, path.Dir(instructionBundlePath)),
		"chmod 0711 /root;", "chmod 0711 "+shellQuote(filepath.Join(rig.root, "root"))+";",
		ward.WriterNoncePlaceholder, launchRigNonce,
	).Replace(script)
	gitPatterns := []string{"'/" + export.EvidenceWorkspaceDir + "/'", "'/" + export.CommitPlanFilename + "'"}
	for rest := script; ; {
		i := strings.Index(rest, "'/")
		if i < 0 {
			break
		}
		rest = rest[i:]
		if !strings.HasPrefix(rest, "'"+rig.root+"/") &&
			!slices.ContainsFunc(gitPatterns, func(p string) bool { return strings.HasPrefix(rest, p) }) {
			t.Fatalf("launch command names an absolute path outside the rig: %.80s", rest)
		}
		rest = rest[2:]
	}
	return script
}

func (rig launchRig) run(t *testing.T, pathPrefix ...string) ([]byte, error) {
	t.Helper()
	// The writer container runs the launcher under umask 022. The command's
	// own files do not depend on it; the seed and hydration stand-ins do.
	cmd := osexec.Command("sh", "-c", "umask 022; "+rig.script(t)) //nolint:gosec // G204: the generated launch command is the subject
	cmd.Dir = rig.root
	cmd.Env = append(os.Environ(),
		"PATH="+strings.Join(append(pathPrefix, rig.bin, os.Getenv("PATH")), string(os.PathListSeparator)),
		"FREESIDE_RIG_PROBE="+rig.probe, "FREESIDE_RIG_SENTINEL="+rig.sentinel)
	return cmd.CombinedOutput()
}

func (rig launchRig) read(t *testing.T, elem ...string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(elem...)) //nolint:gosec // G304: test-owned path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// requireRenameOnlyMove skips where mv cannot refuse a directory destination.
// The launch command needs GNU mv -T, which the pinned agent image and every
// Linux CI runner have, and macOS does not.
func requireRenameOnlyMove(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	if err := os.WriteFile(from, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := osexec.Command("mv", "-T", from, to).Run(); err != nil { //nolint:gosec // G204: fixed argv over test-owned paths
		if runtime.GOOS == "linux" {
			t.Fatalf("mv -T failed on Linux, where the launch command depends on it: %v", err)
		}
		t.Skipf("mv -T is unavailable on %s: %v", runtime.GOOS, err)
	}
}

// TestLaunchCommandLeavesTheWorkspaceWalkable runs the generated launch
// command, not a copy of it, and walks the workspace from the point where the
// agent runs (#1929). A lint that walks the repository root failed there on an
// unreadable control directory and on the launcher's one-line descriptor,
// neither of which the verifier's workspace contains.
//
// The test user owns every file, so it reads mode bits where the real writer
// would get an access error, and a stand-in chown means it cannot see
// ownership. TestAgentCommandKeepsTheOutcomeMarkerOutOfWriterReach holds the
// ownership and ordering half of the control.
func TestLaunchCommandLeavesTheWorkspaceWalkable(t *testing.T) {
	t.Parallel()
	requireRenameOnlyMove(t)
	rig := newLaunchRig(t)
	if output, err := rig.run(t); err != nil {
		t.Fatalf("launch command: %v: %s", err, output)
	}

	// While the agent ran, the launcher had added two empty directories and
	// the git exclude to the seed and its hydrated dependencies: no file of
	// its own, and nothing the agent's user could not read or enter.
	if unreadable := rig.read(t, rig.probe, "unreadable"); unreadable != "" {
		t.Errorf("the agent's walk met entries its user cannot read or enter:\n%s", unreadable)
	}
	wantEntries := []string{
		"./.freeside-evidence", "./.freeside-evidence/.control",
		"./.git", "./.git/HEAD", "./.git/info", "./.git/info/exclude",
		"./README.md",
		"./node_modules", "./node_modules/dep", "./node_modules/dep/index.js",
		"./src", "./src/main.js",
	}
	if got := strings.Fields(rig.read(t, rig.probe, "entries")); !slices.Equal(got, wantEntries) {
		t.Errorf("workspace while the agent ran:\n got %q\nwant %q", got, wantEntries)
	}
	exclude := rig.read(t, rig.workspace, ".git", "info", "exclude")
	if exclude != "\n/"+export.EvidenceWorkspaceDir+"/\n/"+export.CommitPlanFilename+"\n" {
		t.Errorf("git exclude = %q", exclude)
	}

	// Afterwards the launcher's two files hold its own content, whatever the
	// agent left at their paths, and the symlink's target is untouched.
	if sentinel := rig.read(t, rig.sentinel); sentinel != "keep\n" {
		t.Errorf("the launcher wrote through the agent's symlink: sentinel = %q", sentinel)
	}
	evidence := filepath.Join(rig.workspace, export.EvidenceWorkspaceDir)
	for _, name := range []string{path.Base(transcriptDescriptorPath), path.Base(transcriptPath)} {
		info, err := os.Lstat(filepath.Join(evidence, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
			t.Fatalf("%s is not the launcher's regular 0644 file: %v, %v", name, info, err)
		}
	}
	if transcript := rig.read(t, evidence, path.Base(transcriptPath)); transcript != "stand-in transcript\n" {
		t.Errorf("transcript = %q", transcript)
	}
	manifest, err := export.DecodeEvidenceSourceManifest([]byte(rig.read(t, evidence, path.Base(transcriptDescriptorPath))))
	if err != nil {
		t.Fatalf("descriptor does not decode: %v", err)
	}
	wantSources := []export.EvidenceSource{
		{
			Label: "agent-transcript", MediaType: "application/jsonl", Path: transcriptEvidencePath,
			HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivitySensitive,
			ProducerInvocationID: "inv-1",
		},
		{
			Label: export.SummaryEvidenceLabel, MediaType: "text/markdown", Path: export.SummaryEvidencePath,
			HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivitySensitive,
			ProducerInvocationID: "inv-1",
		},
	}
	if !slices.Equal(manifest.Sources, wantSources) {
		t.Errorf("descriptor sources = %+v, want %+v", manifest.Sources, wantSources)
	}

	// Both directories are closed to the agent, the control directory holds
	// only the marker, and the hydrated tree is gone before the export walk
	// could see it.
	if info, err := os.Stat(evidence); err != nil || info.Mode() != os.ModeDir|0o755 {
		t.Errorf("evidence directory is not a plain 0755 directory after the launcher: %v, %v", info, err)
	}
	control := filepath.Join(evidence, path.Base(path.Dir(writerOutcomePath)))
	if info, err := os.Stat(control); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("control directory is not 0700 after the launcher: %v, %v", info, err)
	}
	if entries, err := os.ReadDir(control); err != nil || len(entries) != 1 {
		t.Errorf("control directory holds %v, want only the marker: %v", entries, err)
	}
	if marker := rig.read(t, control, path.Base(writerOutcomePath)); marker != launchRigNonce+" 0\n" {
		t.Errorf("outcome marker = %q", marker)
	}
	if _, err := os.Lstat(filepath.Join(rig.workspace, "node_modules")); !os.IsNotExist(err) {
		t.Errorf("hydrated dependencies survived the launcher: %v", err)
	}
}

// The rename is what keeps the launcher's two files its own, whether or not
// the removal before it worked. With that removal defeated, as if the entries
// the agent left had reappeared, the symlink at the descriptor path is
// replaced and never followed, and the directory at the transcript path fails
// the move. A launcher that cannot move a file into place must not report an
// outcome: ward reads a missing marker as a run that never finished, where a
// marker beside the agent's own entry would hand the agent the evidence
// channel's provenance.
func TestLaunchCommandRenamesOverWhatTheAgentLeft(t *testing.T) {
	t.Parallel()
	requireRenameOnlyMove(t)
	rig := newLaunchRig(t)
	rm, err := osexec.LookPath("rm")
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(rig.workspace, export.EvidenceWorkspaceDir)
	descriptor := filepath.Join(evidence, path.Base(transcriptDescriptorPath))
	keeping := filepath.Join(rig.root, "keeping")
	if err := os.MkdirAll(keeping, 0o750); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\nfor arg; do case \"$arg\" in " +
		shellQuote(descriptor) + "|" + shellQuote(filepath.Join(evidence, path.Base(transcriptPath))) +
		") exit 0 ;; esac; done\nexec " + shellQuote(rm) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(keeping, "rm"), []byte(stub), 0o700); err != nil { //nolint:gosec // G306: an executable stand-in
		t.Fatal(err)
	}
	output, err := rig.run(t, keeping)
	if err == nil || !strings.Contains(string(output), path.Base(transcriptPath)) {
		t.Fatalf("launch command did not fail on the directory at the transcript path: %v: %s", err, output)
	}
	if _, err := os.Lstat(filepath.Join(rig.root, writerOutcomePath)); !os.IsNotExist(err) {
		t.Errorf("the launcher wrote an outcome marker after a failed move: %v", err)
	}
	if sentinel := rig.read(t, rig.sentinel); sentinel != "keep\n" {
		t.Errorf("the launcher wrote through the agent's symlink: sentinel = %q", sentinel)
	}
	if info, err := os.Lstat(descriptor); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the descriptor did not replace the agent's symlink: %v, %v", info, err)
	}
	if _, err := export.DecodeEvidenceSourceManifest([]byte(rig.read(t, descriptor))); err != nil {
		t.Errorf("descriptor does not decode: %v", err)
	}
}

// An agent that never launched still gets its outcome reported: the launcher
// has no transcript to place, writes the descriptor, and records the status
// that names the missing credential.
func TestLaunchCommandReportsAnAgentThatNeverLaunched(t *testing.T) {
	t.Parallel()
	requireRenameOnlyMove(t)
	rig := newLaunchRig(t)
	if err := os.Remove(filepath.Join(rig.root, credentialTokenPath)); err != nil {
		t.Fatal(err)
	}
	output, err := rig.run(t)
	var exit *osexec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 86 {
		t.Fatalf("launch command: %v, want exit status 86: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(rig.probe, "entries")); !os.IsNotExist(err) {
		t.Fatalf("the agent ran without a credential: %v", err)
	}
	evidence := filepath.Join(rig.workspace, export.EvidenceWorkspaceDir)
	if marker := rig.read(t, rig.root, writerOutcomePath); marker != launchRigNonce+" 86\n" {
		t.Errorf("outcome marker = %q", marker)
	}
	if _, err := export.DecodeEvidenceSourceManifest(
		[]byte(rig.read(t, evidence, path.Base(transcriptDescriptorPath))),
	); err != nil {
		t.Errorf("descriptor does not decode: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(evidence, path.Base(transcriptPath))); !os.IsNotExist(err) {
		t.Errorf("a transcript was placed for an agent that never ran: %v", err)
	}
}
