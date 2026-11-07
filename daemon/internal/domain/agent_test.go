package domain_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func launchSpec(t *testing.T) domain.LaunchSpec {
	t.Helper()
	l := domain.LaunchSpec{
		EncodingVersion: domain.LaunchEncodingVersion,
		Stage:           domain.StageNameReview, Writer: false,
		OutputContract: "review-findings/v3", Severance: true,
		SessionMode: domain.SessionOneShot, AuxiliaryInference: domain.AuxiliaryForbidden,
	}
	digest, err := l.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	l.Digest = digest
	return l
}

func agentResolution(t *testing.T) domain.AgentResolutionInput {
	t.Helper()
	identity := authIdentity()
	identity.AccountBinding = "acct-1"
	identity.Enabled = true
	e := enrollment()
	return domain.AgentResolutionInput{
		Source: domain.AgentSource{
			Name: "sol-via-codex", Enrollment: "openai-chatgpt-A/codex",
			Route: "openai_chatgpt_codex", Adapter: "codex_proto_v1",
			Offer: "gpt-5.6-sol", Effort: domain.EffortMax,
		},
		Identity: identity, Enrollment: e,
		Route: routeFragment(t), Adapter: adapterFragment(t), Offer: offerFragment(t),
		OfferRoute: "openai_chatgpt_codex",
	}
}

func TestLaunchSpecValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.LaunchSpec)
		wantErr error
	}{
		{"valid", func(*domain.LaunchSpec) {}, nil},
		{"wrong encoding version", func(l *domain.LaunchSpec) { l.EncodingVersion = 9 }, domain.ErrAgentEncodingVersion},
		{"unknown stage", func(l *domain.LaunchSpec) { l.Stage = "implement" }, domain.ErrInvalidStageName},
		{"no output contract", func(l *domain.LaunchSpec) { l.OutputContract = "" }, domain.ErrEmptyField},
		{"unknown session mode", func(l *domain.LaunchSpec) { l.SessionMode = "eternal" }, domain.ErrInvalidSessionMode},
		{"unknown auxiliary policy", func(l *domain.LaunchSpec) { l.AuxiliaryInference = "vibes" }, domain.ErrInvalidAuxiliaryInference},
		{"digest of different content", func(l *domain.LaunchSpec) { l.Writer = true }, domain.ErrAgentDigestMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := launchSpec(t)
			tc.mutate(&l)
			if err := l.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("round-trip", func(t *testing.T) {
		l := launchSpec(t)
		body, err := l.Encode()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := domain.DecodeLaunchSpec(body)
		if err != nil || decoded.Digest != l.Digest {
			t.Fatalf("round-trip = %+v, %v", decoded, err)
		}
	})
}

// TestLaunchRequiredCapabilities pins the launch-to-capability derivation
// admission step 3 consumes: every session mode and auxiliary policy decides
// its stance here.
func TestLaunchRequiredCapabilities(t *testing.T) {
	base := []domain.LaunchCapability{
		domain.LaunchCapInstructionDelivery, domain.LaunchCapRouteStoreContract,
		domain.LaunchCapStructuredOutput,
	}
	cases := []struct {
		name   string
		mutate func(*domain.LaunchSpec)
		want   []domain.LaunchCapability
	}{
		{
			"read-only severed forbidden one-shot",
			func(*domain.LaunchSpec) {},
			append(slices.Clone(base), domain.LaunchCapReadTools,
				domain.LaunchCapContextSeverance, domain.LaunchCapAuxiliaryInferenceControl),
		},
		{
			"writer resumed observed",
			func(l *domain.LaunchSpec) {
				l.Writer = true
				l.Severance = false
				l.SessionMode = domain.SessionResumed
				l.AuxiliaryInference = domain.AuxiliaryObserved
			},
			append(slices.Clone(base), domain.LaunchCapMutationTools, domain.LaunchCapExactResume),
		},
		{
			"declared auxiliary requires control",
			func(l *domain.LaunchSpec) { l.AuxiliaryInference = domain.AuxiliaryDeclared },
			append(slices.Clone(base), domain.LaunchCapReadTools,
				domain.LaunchCapContextSeverance, domain.LaunchCapAuxiliaryInferenceControl),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := launchSpec(t)
			tc.mutate(&l)
			got := l.RequiredCapabilities()
			want := domain.NewLaunchCapabilitySet(tc.want...)
			if !slices.Equal(got, want) {
				t.Fatalf("RequiredCapabilities() = %v, want %v", got, want)
			}
		})
	}
}

func TestResolveAgentDefinition(t *testing.T) {
	t.Run("resolves and digests the canonical body", func(t *testing.T) {
		in := agentResolution(t)
		agent, err := domain.ResolveAgentDefinition(in)
		if err != nil {
			t.Fatal(err)
		}
		if agent.EnrollmentID != in.Enrollment.ID || agent.Effort != domain.EffortMax {
			t.Fatalf("resolved agent = %+v", agent)
		}
		if agent.RouteDigest != in.Route.Digest || agent.AdapterDigest != in.Adapter.Digest ||
			agent.OfferDigest != in.Offer.Digest {
			t.Fatalf("resolved digests disagree with fragments: %+v", agent)
		}
		if err := agent.Validate(); err != nil {
			t.Fatal(err)
		}
		body, err := agent.Encode()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := domain.DecodeAgentDefinition(body)
		if err != nil || decoded.Digest != agent.Digest {
			t.Fatalf("round-trip = %+v, %v", decoded, err)
		}
		// A renamed agent is the same agent: the name is outside the digest.
		renamed := agent
		renamed.Name = "sol-prime"
		recomputed, err := renamed.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		if recomputed != agent.Digest {
			t.Fatal("rename changed the agent digest")
		}
	})

	joinCases := []struct {
		name   string
		mutate func(*domain.AgentResolutionInput)
	}{
		{"disabled identity", func(in *domain.AgentResolutionInput) { in.Identity.Enabled = false }},
		{"enrollment through another route", func(in *domain.AgentResolutionInput) { in.Enrollment.Route = "openai_platform" }},
		{"adapter for another client", func(in *domain.AgentResolutionInput) { in.Enrollment.HarnessClient = domain.HarnessClientClaudeCode }},
		{"offer under another route", func(in *domain.AgentResolutionInput) { in.OfferRoute = "openai_platform" }},
		{"effort the offer does not allow", func(in *domain.AgentResolutionInput) { in.Source.Effort = domain.EffortMedium }},
		{"effort the adapter cannot send", func(in *domain.AgentResolutionInput) {
			in.Offer.AllowedEfforts = []domain.EffortLevel{domain.EffortLow}
			digest, err := in.Offer.ComputeDigest()
			if err != nil {
				t.Fatal(err)
			}
			in.Offer.Digest = digest
			in.Source.Effort = domain.EffortLow
		}},
	}
	for _, tc := range joinCases {
		t.Run(tc.name, func(t *testing.T) {
			in := agentResolution(t)
			tc.mutate(&in)
			if _, err := domain.ResolveAgentDefinition(in); !errors.Is(err, domain.ErrAgentJoinInvalid) {
				t.Fatalf("ResolveAgentDefinition = %v, want %v", err, domain.ErrAgentJoinInvalid)
			}
		})
	}

	t.Run("credential for another account fails the join", func(t *testing.T) {
		in := agentResolution(t)
		in.Enrollment.AccountBinding = "acct-2"
		if _, err := domain.ResolveAgentDefinition(in); !errors.Is(err, domain.ErrAccountBindingMismatch) {
			t.Fatalf("ResolveAgentDefinition = %v, want %v", err, domain.ErrAccountBindingMismatch)
		}
	})

	t.Run("claude agent resolves", func(t *testing.T) {
		if _, err := domain.ResolveAgentDefinition(claudeAgentResolution(t)); err != nil {
			t.Fatal(err)
		}
	})

	// The pinned Claude build's table sends every valid level, so an authored
	// list is the only way a valid Claude effort can be unsendable today;
	// TestTranslateEffortRefusals covers the table's own refusal.
	t.Run("claude effort missing from the authored list", func(t *testing.T) {
		in := claudeAgentResolution(t)
		in.Adapter.SendableEfforts = []domain.EffortLevel{domain.EffortHigh}
		in.Adapter.Digest = mustComputeDigest(t, in.Adapter.ComputeDigest)
		if _, err := domain.ResolveAgentDefinition(in); !errors.Is(err, domain.ErrAgentJoinInvalid) {
			t.Fatalf("ResolveAgentDefinition = %v, want %v", err, domain.ErrAgentJoinInvalid)
		}
	})
}

func TestAgentNameValidation(t *testing.T) {
	in := agentResolution(t)
	in.Source.Name = ""
	if err := in.Source.Validate(); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("empty AgentSource.Validate() = %v, want %v", err, domain.ErrEmptyField)
	}

	for _, name := range []string{"sol-via-codex", "a", "x_1-y"} {
		t.Run("valid source "+name, func(t *testing.T) {
			in := agentResolution(t)
			in.Source.Name = name
			if err := in.Source.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
	for _, name := range []string{
		"sol@codex", "Sol-via-codex", "sol-Via-codex", "sol.via", "sol/via", "sol\\via",
		"-sol", "sol-", "_sol", "sol_", "-", "_", "sol via", "sol\tvia", "sol\nvia",
		"sol:via", "søl",
	} {
		t.Run("invalid source "+name, func(t *testing.T) {
			in := agentResolution(t)
			in.Source.Name = name
			if err := in.Source.Validate(); !errors.Is(err, domain.ErrInvalidAgentName) {
				t.Fatalf("Validate() = %v, want %v", err, domain.ErrInvalidAgentName)
			}
		})
	}

	// Enumerate the byte vocabulary at both name positions. Bytes outside
	// ASCII are included, so every UTF-8 multibyte name is rejected too.
	for value := range 256 {
		char := byte(value)
		alphanumeric := ('a' <= char && char <= 'z') || ('0' <= char && char <= '9')
		for _, tc := range []struct {
			name  string
			valid bool
		}{
			{string([]byte{char}), alphanumeric},
			{string([]byte{'a', char, 'z'}), alphanumeric || char == '-' || char == '_'},
		} {
			in := agentResolution(t)
			in.Source.Name = tc.name
			err := in.Source.Validate()
			if tc.valid && err != nil {
				t.Fatalf("Validate(%q) = %v", tc.name, err)
			}
			if !tc.valid && !errors.Is(err, domain.ErrInvalidAgentName) {
				t.Fatalf("Validate(%q) = %v, want %v", tc.name, err, domain.ErrInvalidAgentName)
			}
		}
	}

	in = agentResolution(t)
	agent, err := domain.ResolveAgentDefinition(in)
	if err != nil {
		t.Fatal(err)
	}
	agent.Name = "sol@codex"
	if err := agent.Validate(); !errors.Is(err, domain.ErrInvalidAgentName) {
		t.Fatalf("AgentDefinition.Validate() = %v, want %v", err, domain.ErrInvalidAgentName)
	}
	body, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.DecodeAgentDefinition(body); !errors.Is(err, domain.ErrInvalidAgentName) {
		t.Fatalf("DecodeAgentDefinition() = %v, want %v", err, domain.ErrInvalidAgentName)
	}

	for _, tc := range []struct {
		name    string
		wantErr error
	}{
		{strings.Repeat("a", 246), nil},
		{strings.Repeat("a", 247), domain.ErrInvalidAgentName},
	} {
		t.Run(fmt.Sprintf("length %d", len(tc.name)), func(t *testing.T) {
			in := agentResolution(t)
			in.Source.Name = tc.name
			assertAgentNameError(t, "AgentSource.Validate", in.Source.Validate(), tc.wantErr)

			candidate := agent
			candidate.Name = tc.name
			assertAgentNameError(t, "AgentDefinition.Validate", candidate.Validate(), tc.wantErr)
			body, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			_, err = domain.DecodeAgentDefinition(body)
			assertAgentNameError(t, "DecodeAgentDefinition", err, tc.wantErr)

			// A lineup line's prompt name follows the agent name rule.
			_, err = domain.ParseLineupSelection(lineupSelection + "/" + tc.name + "@" + lineupPromptDigest)
			assertAgentNameError(t, "ParseLineupSelection prompt", err, tc.wantErr)
		})
	}
}

func assertAgentNameError(t *testing.T, operation string, got, want error) {
	t.Helper()
	if want == nil && got != nil {
		t.Fatalf("%s = %v", operation, got)
	}
	if want != nil && !errors.Is(got, want) {
		t.Fatalf("%s = %v, want %v", operation, got, want)
	}
}

// TestAgentBodyRejectsNames is the acceptance fixture: a canonical body that
// hashes names instead of resolved references fails validation.
func TestAgentBodyRejectsNames(t *testing.T) {
	in := agentResolution(t)
	agent, err := domain.ResolveAgentDefinition(in)
	if err != nil {
		t.Fatal(err)
	}
	named := agent
	named.RouteDigest = domain.Digest("openai_chatgpt_codex")
	digest, err := named.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	named.Digest = digest
	if err := named.Validate(); !errors.Is(err, domain.ErrAgentBodyUnresolved) {
		t.Fatalf("Validate() = %v, want %v", err, domain.ErrAgentBodyUnresolved)
	}
}

func TestValidateOfferCoversDeadline(t *testing.T) {
	offer := offerFragment(t)
	if err := domain.ValidateOfferCoversDeadline(offer, offer.NotAfter.Add(-time.Hour)); err != nil {
		t.Fatalf("covered deadline = %v", err)
	}
	if err := domain.ValidateOfferCoversDeadline(offer, offer.NotAfter.Add(time.Hour)); !errors.Is(err, domain.ErrOfferExpired) {
		t.Fatalf("expired offer = %v, want %v", err, domain.ErrOfferExpired)
	}
	if err := domain.ValidateOfferCoversDeadline(offer, time.Time{}); !errors.Is(err, domain.ErrMissingTimestamp) {
		t.Fatalf("zero deadline = %v, want %v", err, domain.ErrMissingTimestamp)
	}
}

const (
	lineupEnrollment   = "claude-a/claude_code"
	lineupSelection    = lineupEnrollment + "/claude-sonnet-4-5/high"
	lineupPromptDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	lineupValue        = lineupSelection + "/role-prompt@" + lineupPromptDigest
)

func lineupKey(key string) domain.PolicyKey {
	return domain.PolicyKey{
		Key: key, Value: lineupValue,
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:policy"},
	}
}

func TestParseLineupSelection(t *testing.T) {
	selection, err := domain.ParseLineupSelection(lineupValue)
	want := domain.LineupSelection{
		Names:      domain.AgentSelectionNames{Enrollment: lineupEnrollment, Model: "claude-sonnet-4-5", Effort: "high"},
		PromptName: "role-prompt", PromptDigest: lineupPromptDigest,
	}
	if err != nil || selection != want {
		t.Fatalf("ParseLineupSelection = %+v, %v", selection, err)
	}
	if got := selection.TaskLineSelection().Text(); got != lineupSelection {
		t.Fatalf("TaskLineSelection().Text() = %q, want %q", got, lineupSelection)
	}
	// A model id may carry an '@' of its own: only the last one starts the
	// prompt digest.
	pinned, err := domain.ParseLineupSelection(lineupEnrollment + "/claude-opus-4-1@20250805/max/role-prompt@" + lineupPromptDigest)
	if err != nil || pinned.Names.Model != "claude-opus-4-1@20250805" {
		t.Fatalf("model with an @ = %+v, %v", pinned, err)
	}
	prompt := "role-prompt@" + lineupPromptDigest
	if _, err := domain.ParseLineupSelection(lineupSelection + "/Role@" + lineupPromptDigest); !errors.Is(err, domain.ErrInvalidAgentName) {
		t.Fatalf("prompt name that fails the name rule = %v, want %v", err, domain.ErrInvalidAgentName)
	}
	for _, bad := range []string{
		"", "sol", prompt, "/" + prompt, lineupSelection, lineupSelection + "/",
		// The old agent-and-prompt value names an agent where the selection
		// belongs.
		"sol-via-codex@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/" + prompt,
		"claude-sonnet-4-5/high/" + prompt, "high/" + prompt,
		"/claude-sonnet-4-5/high/" + prompt, lineupEnrollment + "//high/" + prompt, lineupEnrollment + "/claude-sonnet-4-5//" + prompt,
		lineupSelection + "/role-prompt", lineupSelection + "/role-prompt@", lineupSelection + "/role-prompt@not-a-digest",
		lineupEnrollment + "/claude sonnet/high/" + prompt, lineupEnrollment + "/-claude/high/" + prompt,
	} {
		if _, err := domain.ParseLineupSelection(bad); !errors.Is(err, domain.ErrInvalidLineupKey) {
			t.Fatalf("ParseLineupSelection(%q) = %v, want %v", bad, err, domain.ErrInvalidLineupKey)
		}
	}
}

// TestLineupPolicyKeys is the lineup key contract's acceptance fixture: every
// role in the closed list is a key, and nothing else under the namespace is.
func TestLineupPolicyKeys(t *testing.T) {
	keys := []domain.PolicyKey{{Key: "driver", Value: "claude"}, {Key: "lineup.roles", Value: "not a lineup key"}}
	for _, role := range domain.AllRoleNames {
		key, err := domain.LineupRoleKey(role)
		if err != nil || key != "lineup.role."+string(role) {
			t.Fatalf("LineupRoleKey(%s) = %q, %v", role, key, err)
		}
		keys = append(keys, lineupKey(key))
	}
	lineup, err := domain.ResolveLineup(keys)
	if err != nil {
		t.Fatalf("ResolveLineup(every role) = %v", err)
	}
	if err := domain.ValidateLineupPolicyKeys(keys); err != nil {
		t.Fatalf("ValidateLineupPolicyKeys(every role) = %v", err)
	}
	for _, role := range domain.AllRoleNames {
		if line, bound := lineup.Line(role); !bound || line.Names.Enrollment != lineupEnrollment || line.PromptDigest != lineupPromptDigest {
			t.Fatalf("Line(%s) = %+v, %t", role, line, bound)
		}
	}
	if unbound := lineup.UnboundRoles(domain.AllRoleNames); len(unbound) != 0 {
		t.Fatalf("full lineup reports unbound roles %v", unbound)
	}
	for _, notRole := range []domain.RoleName{"", "implementation", "verification", "researcher"} {
		if _, err := domain.LineupRoleKey(notRole); !errors.Is(err, domain.ErrInvalidRoleName) {
			t.Fatalf("LineupRoleKey(%q) = %v, want %v", notRole, err, domain.ErrInvalidRoleName)
		}
	}
	missingPrompt := lineupKey("lineup.role.reviewer")
	missingPrompt.Value = lineupSelection
	missingAgent := lineupKey("lineup.role.reviewer")
	missingAgent.Value = "/role-prompt@" + lineupPromptDigest
	cases := []struct {
		name    string
		keys    []domain.PolicyKey
		wantErr error
	}{
		{"unknown role", []domain.PolicyKey{lineupKey("lineup.role.deployer")}, domain.ErrInvalidRoleName},
		{"empty role", []domain.PolicyKey{lineupKey("lineup.role.")}, domain.ErrInvalidRoleName},
		// Stage names are not role names: the specification, implementation,
		// and review stages each hold roles under other names.
		{"stage specification", []domain.PolicyKey{lineupKey("lineup.role.specification")}, domain.ErrInvalidRoleName},
		{"stage implementation", []domain.PolicyKey{lineupKey("lineup.role.implementation")}, domain.ErrInvalidRoleName},
		{"stage review", []domain.PolicyKey{lineupKey("lineup.role.review")}, domain.ErrInvalidRoleName},
		{"legacy stage spelling", []domain.PolicyKey{lineupKey("lineup.role.implement")}, domain.ErrInvalidRoleName},
		// Verification is an engine job and never a role (plan §5.13).
		{"verification", []domain.PolicyKey{lineupKey("lineup.role.verification")}, domain.ErrInvalidRoleName},
		// Plan revision 76's role joins through #1656.
		{"researcher before #1656", []domain.PolicyKey{lineupKey("lineup.role.researcher")}, domain.ErrInvalidRoleName},
		{"line missing its prompt", []domain.PolicyKey{missingPrompt}, domain.ErrInvalidLineupKey},
		{"line missing its agent", []domain.PolicyKey{missingAgent}, domain.ErrInvalidLineupKey},
		{
			"duplicate line",
			[]domain.PolicyKey{lineupKey("lineup.role.reviewer"), lineupKey("lineup.role.reviewer")},
			domain.ErrDuplicate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := domain.ValidateLineupPolicyKeys(tc.keys); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateLineupPolicyKeys = %v, want %v", err, tc.wantErr)
			}
			if _, err := domain.ResolveLineup(tc.keys); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ResolveLineup = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestLineupHasNoFallback proves a role is bound only by its own primary
// line, and that a role policy does not ask work from is never missing.
func TestLineupHasNoFallback(t *testing.T) {
	lineup, err := domain.ResolveLineup([]domain.PolicyKey{
		lineupKey("lineup.role.implementer"),
		lineupKey("lineup.role.reviewer"),
		lineupKey("lineup.role.task_namer.shadow.candidate"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, bound := lineup.Line(domain.RoleRemediator); bound {
		t.Fatal("remediator borrowed a line it does not have")
	}
	if _, bound := lineup.Line(domain.RoleTaskNamer); bound {
		t.Fatal("a shadow line bound its role with no primary line")
	}
	if shadow, present := lineup.ShadowLine(domain.RoleTaskNamer); !present || shadow.Name != "candidate" {
		t.Fatalf("ShadowLine(task_namer) = %+v, %t", shadow, present)
	}
	asked := []domain.RoleName{domain.RoleImplementer, domain.RoleRemediator, domain.RoleReviewer, domain.RoleTaskNamer}
	want := []domain.RoleName{domain.RoleRemediator, domain.RoleTaskNamer}
	if unbound := lineup.UnboundRoles(asked); !slices.Equal(unbound, want) {
		t.Fatalf("UnboundRoles = %v, want %v", unbound, want)
	}
	// The shadow reviewer at a zero rate and the drift auditor with its key
	// unset are off: no line, and nothing reported missing.
	if unbound := lineup.UnboundRoles([]domain.RoleName{domain.RoleImplementer, domain.RoleReviewer}); len(unbound) != 0 {
		t.Fatalf("roles outside the asked set reported missing: %v", unbound)
	}
	var empty domain.Lineup
	if unbound := empty.UnboundRoles([]domain.RoleName{domain.RoleReviewer}); !slices.Equal(unbound, []domain.RoleName{domain.RoleReviewer}) {
		t.Fatalf("zero lineup UnboundRoles = %v", unbound)
	}
}

func TestLineupShadowLines(t *testing.T) {
	key, err := domain.LineupShadowKey(domain.RoleTaskNamer, "candidate")
	if err != nil || key != "lineup.role.task_namer.shadow.candidate" {
		t.Fatalf("LineupShadowKey = %q, %v", key, err)
	}
	if _, err := domain.LineupShadowKey(domain.RoleReviewer, "candidate"); !errors.Is(err, domain.ErrLineupShadowOnWardRole) {
		t.Fatalf("shadow key on a ward role = %v", err)
	}
	if _, err := domain.LineupShadowKey("deployer", "candidate"); !errors.Is(err, domain.ErrInvalidRoleName) {
		t.Fatalf("shadow key on an unknown role = %v", err)
	}
	if _, err := domain.LineupShadowKey(domain.RoleTaskNamer, "a.b"); !errors.Is(err, domain.ErrInvalidAgentName) {
		t.Fatalf("shadow key with a dotted name = %v", err)
	}
	for _, role := range domain.AllRoleNames {
		if role.LaunchShape() != domain.LaunchShapeWardless {
			continue
		}
		primary, _ := domain.LineupRoleKey(role)
		shadowKey, err := domain.LineupShadowKey(role, "candidate")
		if err != nil {
			t.Fatalf("LineupShadowKey(%s) = %v", role, err)
		}
		lineup, err := domain.ResolveLineup([]domain.PolicyKey{lineupKey(primary), lineupKey(shadowKey)})
		if err != nil {
			t.Fatalf("%s with one shadow line = %v", role, err)
		}
		if _, bound := lineup.Line(role); !bound {
			t.Fatalf("%s unbound beside its shadow line", role)
		}
		if shadow, present := lineup.ShadowLine(role); !present || shadow.Name != "candidate" || shadow.Selection.PromptDigest != lineupPromptDigest {
			t.Fatalf("ShadowLine(%s) = %+v, %t", role, shadow, present)
		}
	}
	badValue := lineupKey("lineup.role.task_namer.shadow.candidate")
	badValue.Value = lineupSelection
	cases := []struct {
		name    string
		keys    []domain.PolicyKey
		wantErr error
	}{
		{
			"second shadow line",
			[]domain.PolicyKey{
				lineupKey("lineup.role.task_namer"),
				lineupKey("lineup.role.task_namer.shadow.candidate"),
				lineupKey("lineup.role.task_namer.shadow.other"),
			},
			domain.ErrLineupShadowLimit,
		},
		{
			"same shadow line twice",
			[]domain.PolicyKey{
				lineupKey("lineup.role.task_namer.shadow.candidate"),
				lineupKey("lineup.role.task_namer.shadow.candidate"),
			},
			domain.ErrLineupShadowLimit,
		},
		{"shadow on a ward role", []domain.PolicyKey{lineupKey("lineup.role.reviewer.shadow.candidate")}, domain.ErrLineupShadowOnWardRole},
		{"shadow under an unknown role", []domain.PolicyKey{lineupKey("lineup.role.deployer.shadow.candidate")}, domain.ErrInvalidRoleName},
		{"shadow under a stage name", []domain.PolicyKey{lineupKey("lineup.role.review.shadow.candidate")}, domain.ErrInvalidRoleName},
		{"shadow with no name", []domain.PolicyKey{lineupKey("lineup.role.task_namer.shadow")}, domain.ErrInvalidLineupKey},
		{"shadow with an empty name", []domain.PolicyKey{lineupKey("lineup.role.task_namer.shadow.")}, domain.ErrInvalidLineupKey},
		{"shadow with a dotted name", []domain.PolicyKey{lineupKey("lineup.role.task_namer.shadow.a.b")}, domain.ErrInvalidLineupKey},
		{"unknown key segment", []domain.PolicyKey{lineupKey("lineup.role.task_namer.fallback.candidate")}, domain.ErrInvalidLineupKey},
		{"trailing dot", []domain.PolicyKey{lineupKey("lineup.role.task_namer.")}, domain.ErrInvalidLineupKey},
		{"shadow missing its prompt", []domain.PolicyKey{badValue}, domain.ErrInvalidLineupKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := domain.ResolveLineup(tc.keys); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ResolveLineup = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

const escalatedPromptDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

// escalatedKey is the specifier's escalated line for the enrollment, keyed
// by keyEnrollment and escalating to model at effort on valueEnrollment, with
// the prompt lineupValue's primary line carries.
func escalatedKey(keyEnrollment, valueEnrollment, model, effort string) domain.PolicyKey {
	key := lineupKey("lineup.role.specifier.escalated." + keyEnrollment)
	key.Value = valueEnrollment + "/" + model + "/" + effort + "/role-prompt@" + lineupPromptDigest
	return key
}

func TestLineupEscalatedLines(t *testing.T) {
	key, err := domain.LineupEscalatedKey(domain.RoleSpecifier, lineupEnrollment)
	if err != nil || key != "lineup.role.specifier.escalated."+lineupEnrollment {
		t.Fatalf("LineupEscalatedKey = %q, %v", key, err)
	}
	if _, err := domain.LineupEscalatedKey(domain.RoleImplementer, lineupEnrollment); !errors.Is(err, domain.ErrLineupEscalatedRole) {
		t.Fatalf("escalated key on the implementer = %v", err)
	}
	for _, bad := range []string{"", "a b", "-a"} {
		if _, err := domain.LineupEscalatedKey(domain.RoleSpecifier, bad); !errors.Is(err, domain.ErrInvalidLineupKey) {
			t.Fatalf("escalated key for enrollment %q = %v", bad, err)
		}
	}

	opus := escalatedKey(lineupEnrollment, lineupEnrollment, "claude-opus-4-5", "max")
	lineup, err := domain.ResolveLineup([]domain.PolicyKey{lineupKey("lineup.role.specifier"), opus})
	if err != nil {
		t.Fatalf("specifier with one escalated line = %v", err)
	}
	counterpart, present := lineup.EscalatedCounterpart(domain.RoleSpecifier, lineupEnrollment)
	want := domain.AgentSelectionNames{Enrollment: lineupEnrollment, Model: "claude-opus-4-5", Effort: "max"}
	if !present || counterpart.Names != want {
		t.Fatalf("EscalatedCounterpart = %+v, %t", counterpart, present)
	}
	if _, present := lineup.EscalatedCounterpart(domain.RoleSpecifier, "claude-b/claude_code"); present {
		t.Fatal("another enrollment has an escalated line")
	}
	if line, _ := lineup.Line(domain.RoleSpecifier); line.Names.Model != "claude-sonnet-4-5" {
		t.Fatalf("an escalated line replaced the specifier's line: %+v", line)
	}
	// A second enrollment escalates on its own line, to the same model the
	// first does: the key, not the value's model, tells them apart.
	second := escalatedKey("claude-b/claude_code", "claude-b/claude_code", "claude-opus-4-5", "max")
	if _, err := domain.ResolveLineup([]domain.PolicyKey{lineupKey("lineup.role.specifier"), opus, second}); err != nil {
		t.Fatalf("two enrollments' escalated lines = %v", err)
	}
	// An effort change alone is an escalation.
	if _, err := domain.ResolveLineup([]domain.PolicyKey{
		lineupKey("lineup.role.specifier"), escalatedKey(lineupEnrollment, lineupEnrollment, "claude-sonnet-4-5", "max"),
	}); err != nil {
		t.Fatalf("effort-only escalation = %v", err)
	}

	otherPrompt := opus
	otherPrompt.Value = lineupEnrollment + "/claude-opus-4-5/max/role-prompt@" + escalatedPromptDigest
	renamedPrompt := opus
	renamedPrompt.Value = lineupEnrollment + "/claude-opus-4-5/max/other-prompt@" + lineupPromptDigest
	cases := []struct {
		name    string
		keys    []domain.PolicyKey
		wantErr error
	}{
		{
			"escalated on the implementer",
			[]domain.PolicyKey{lineupKey("lineup.role.implementer"), lineupKey("lineup.role.implementer.escalated." + lineupEnrollment)},
			domain.ErrLineupEscalatedRole,
		},
		{
			"escalated on a wardless role",
			[]domain.PolicyKey{lineupKey("lineup.role.task_namer"), lineupKey("lineup.role.task_namer.escalated." + lineupEnrollment)},
			domain.ErrLineupEscalatedRole,
		},
		{"prompt digest differs", []domain.PolicyKey{lineupKey("lineup.role.specifier"), otherPrompt}, domain.ErrLineupEscalatedPrompt},
		{"prompt name differs", []domain.PolicyKey{lineupKey("lineup.role.specifier"), renamedPrompt}, domain.ErrLineupEscalatedPrompt},
		{"no specifier line", []domain.PolicyKey{opus}, domain.ErrLineupEscalatedPrompt},
		{
			"second line for one enrollment",
			[]domain.PolicyKey{
				lineupKey("lineup.role.specifier"), opus,
				escalatedKey(lineupEnrollment, lineupEnrollment, "claude-opus-4-5", "high"),
			},
			domain.ErrDuplicate,
		},
		// Escalation changes the model and effort, never the credential.
		{
			"key names another enrollment than its value",
			[]domain.PolicyKey{lineupKey("lineup.role.specifier"), escalatedKey(lineupEnrollment, "claude-b/claude_code", "claude-opus-4-5", "max")},
			domain.ErrLineupEscalatedPair,
		},
		{
			"escalates to the specifier's own line",
			[]domain.PolicyKey{lineupKey("lineup.role.specifier"), escalatedKey(lineupEnrollment, lineupEnrollment, "claude-sonnet-4-5", "high")},
			domain.ErrLineupEscalatedPair,
		},
		{"escalated with no enrollment", []domain.PolicyKey{lineupKey("lineup.role.specifier.escalated")}, domain.ErrInvalidLineupKey},
		{"escalated with an empty enrollment", []domain.PolicyKey{lineupKey("lineup.role.specifier.escalated.")}, domain.ErrInvalidLineupKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := domain.ResolveLineup(tc.keys); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ResolveLineup = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestCanonicalStageRole is the exhaustive stage-role resolver fixture: every
// canonical name maps to itself, the engine's persisted legacy spelling maps
// to its canonical member, and anything else fails closed.
func TestCanonicalStageRole(t *testing.T) {
	for _, canonical := range domain.AllStageNames {
		got, err := domain.CanonicalStageRole(string(canonical))
		if err != nil || got != canonical {
			t.Fatalf("CanonicalStageRole(%q) = %q, %v", canonical, got, err)
		}
	}
	got, err := domain.CanonicalStageRole("implement")
	if err != nil || got != domain.StageNameImplementation {
		t.Fatalf("CanonicalStageRole(implement) = %q, %v", got, err)
	}
	if _, err := domain.CanonicalStageRole("deploy"); !errors.Is(err, domain.ErrUnknownStageRole) {
		t.Fatalf("CanonicalStageRole(deploy) = %v, want %v", err, domain.ErrUnknownStageRole)
	}
}

// TestComputeTreatmentDigest pins the grouping: commercial facts (billing,
// terms) stay out, behavioural facts (model, requested and effective effort)
// land in.
func TestComputeTreatmentDigest(t *testing.T) {
	route := routeFragment(t)
	launch := launchSpec(t)
	adapter := adapterFragment(t)
	digest := func(route domain.RouteFragment, model, requested, effective string) domain.Digest {
		t.Helper()
		got, err := domain.ComputeTreatmentDigest(route, adapter.Digest, launch.Digest, model, requested, effective)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := digest(route, "gpt-5.6", "max", "xhigh")
	if !strings.HasPrefix(string(base), "sha256:") {
		t.Fatalf("treatment digest %q", base)
	}

	rebilled := route
	rebilled.BillingMode = "api_metered"
	rebilled.TermsBasisDate = "2026-09-01"
	routeDigest, err := rebilled.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	rebilled.Digest = routeDigest
	if digest(rebilled, "gpt-5.6", "max", "xhigh") != base {
		t.Fatal("commercial-only changes moved the treatment digest")
	}
	for name, moved := range map[string]domain.Digest{
		"model":            digest(route, "gpt-5.7", "max", "xhigh"),
		"requested effort": digest(route, "gpt-5.6", "xhigh", "xhigh"),
		"effective effort": digest(route, "gpt-5.6", "max", "high"),
	} {
		if moved == base {
			t.Fatalf("%s change did not move the treatment digest", name)
		}
	}
	if _, err := domain.ComputeTreatmentDigest(route, adapter.Digest, launch.Digest, "", "max", "xhigh"); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("treatment without a model = %v, want %v", err, domain.ErrEmptyField)
	}
}
