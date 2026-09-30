package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// claudeAgentResolution is agentResolution's Claude counterpart: an
// enrollment and adapter on claude_code, the adapter declaring every effort
// the translation can send, and an offer passing an explicit model.
func claudeAgentResolution(t *testing.T) domain.AgentResolutionInput {
	t.Helper()
	in := agentResolution(t)
	in.Source = domain.AgentSource{
		Name: "opus-via-claude", Enrollment: "anthropic-A/claude",
		Route: "anthropic_claude_code", Adapter: "claude_code_v1",
		Offer: "claude-opus-5-5", Effort: domain.EffortMax,
	}
	in.Enrollment.HarnessClient = domain.HarnessClientClaudeCode
	in.Enrollment.Route = "anthropic_claude_code"
	in.OfferRoute = "anthropic_claude_code"
	in.Route = domain.RouteFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		ServiceOperator: "anthropic", Protocol: "anthropic_messages",
		InferenceAuthorities: []string{"api.anthropic.com"},
		BillingMode:          "subscription", FallbackPolicy: "fail_closed",
		TermsBasisDate: "2026-08-22",
	}
	in.Route.Digest = mustComputeDigest(t, in.Route.ComputeDigest)
	in.Adapter = domain.AdapterFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		AdapterBuild:    "claude_code_v1@build-1", HarnessBuild: "claude-code 2.1.220",
		ClientKind: domain.HarnessClientClaudeCode, Vendor: domain.AgentVendorClaude,
		LaunchCapabilities: adapterFragment(t).LaunchCapabilities,
		SendableEfforts:    domain.ClaudeCodeSendableEfforts(),
	}
	in.Adapter.Digest = mustComputeDigest(t, in.Adapter.ComputeDigest)
	in.Offer = domain.OfferFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		RouteModelID:    "claude-opus-5-5", LineageGroup: "anthropic",
		IdentityStability: domain.IdentityPinned,
		AllowedEfforts:    []domain.EffortLevel{domain.EffortHarnessDefault, domain.EffortHigh, domain.EffortMax},
		PricingRevision:   "2026-09",
		NotAfter:          time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	in.Offer.Digest = mustComputeDigest(t, in.Offer.ComputeDigest)
	return in
}

func mustComputeDigest(t *testing.T, compute func() (domain.Digest, error)) domain.Digest {
	t.Helper()
	digest, err := compute()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestTranslateEffortClaudeCode(t *testing.T) {
	want := map[domain.EffortLevel]string{
		domain.EffortHarnessDefault: "",
		domain.EffortLow:            "low",
		domain.EffortMedium:         "medium",
		domain.EffortHigh:           "high",
		domain.EffortMax:            "max",
	}
	for _, effort := range domain.AllEffortLevels {
		t.Run(string(effort), func(t *testing.T) {
			native, ok := want[effort]
			if !ok {
				t.Fatalf("no expected native value for %q: extend this table with the translation", effort)
			}
			got, err := domain.TranslateEffort(domain.HarnessClientClaudeCode, effort)
			if err != nil {
				t.Fatal(err)
			}
			if got.Requested != effort || got.Native != native {
				t.Fatalf("TranslateEffort(%q) = %+v, want native %q", effort, got, native)
			}
		})
	}
	if got := domain.ClaudeCodeSendableEfforts(); !slices.Equal(got, domain.AllEffortLevels) {
		t.Fatalf("ClaudeCodeSendableEfforts() = %v, want every level", got)
	}
}

func TestTranslateEffortRefusals(t *testing.T) {
	cases := []struct {
		name    string
		client  domain.HarnessClientKind
		effort  domain.EffortLevel
		wantErr error
	}{
		{"codex has no table", domain.HarnessClientCodexCLI, domain.EffortHigh, domain.ErrEffortUntranslatable},
		{"claude level outside the table", domain.HarnessClientClaudeCode, "xhigh", domain.ErrEffortUntranslatable},
		{"unknown client", "pi", domain.EffortHigh, domain.ErrInvalidHarnessClientKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := domain.TranslateEffort(tc.client, tc.effort); !errors.Is(err, tc.wantErr) {
				t.Fatalf("TranslateEffort = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestEffortTranslationRendering(t *testing.T) {
	cases := []struct {
		name          string
		translation   domain.EffortTranslation
		wantString    string
		wantEffective string
		wantClamped   bool
	}{
		{"clamp", domain.EffortTranslation{Requested: domain.EffortMax, Native: "xhigh"}, "max → xhigh", "xhigh", true},
		{"unclamped", domain.EffortTranslation{Requested: domain.EffortHigh, Native: "high"}, "high", "high", false},
		{
			"harness default sends nothing",
			domain.EffortTranslation{Requested: domain.EffortHarnessDefault},
			"harness_default", "harness_default", false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := tc.translation
			if tr.String() != tc.wantString || tr.Effective() != tc.wantEffective || tr.Clamped() != tc.wantClamped {
				t.Fatalf("rendering = %q/%q/%v, want %q/%q/%v", tr.String(), tr.Effective(), tr.Clamped(),
					tc.wantString, tc.wantEffective, tc.wantClamped)
			}
		})
	}
}

func TestClaudeCodeLaunchModel(t *testing.T) {
	if got := domain.ClaudeCodeLaunchModel(domain.ClaudeCodeNativeDefaultRouteModelID); got != "" {
		t.Fatalf("native default passes model %q, want none", got)
	}
	if got := domain.ClaudeCodeLaunchModel("claude-opus-5-5"); got != "claude-opus-5-5" {
		t.Fatalf("explicit model passes %q, want claude-opus-5-5", got)
	}
}

func TestDeriveAgentLaunchSelection(t *testing.T) {
	resolve := func(t *testing.T, in domain.AgentResolutionInput) domain.AgentDefinition {
		t.Helper()
		agent, err := domain.ResolveAgentDefinition(in)
		if err != nil {
			t.Fatal(err)
		}
		return agent
	}

	t.Run("claude explicit model and effort", func(t *testing.T) {
		in := claudeAgentResolution(t)
		got, err := domain.DeriveAgentLaunchSelection(resolve(t, in), in.Adapter, in.Offer)
		want := domain.AgentLaunchSelection{
			RouteModelID: "claude-opus-5-5", RequestedEffort: domain.EffortMax, NativeEffort: "max",
		}
		if err != nil || got != want {
			t.Fatalf("DeriveAgentLaunchSelection = %+v, %v, want %+v", got, err, want)
		}
	})

	t.Run("claude native default at harness_default passes neither", func(t *testing.T) {
		in := claudeAgentResolution(t)
		in.Offer.RouteModelID = domain.ClaudeCodeNativeDefaultRouteModelID
		in.Offer.Digest = mustComputeDigest(t, in.Offer.ComputeDigest)
		in.Source.Effort = domain.EffortHarnessDefault
		got, err := domain.DeriveAgentLaunchSelection(resolve(t, in), in.Adapter, in.Offer)
		if err != nil || got != (domain.AgentLaunchSelection{}) {
			t.Fatalf("DeriveAgentLaunchSelection = %+v, %v, want empty", got, err)
		}
	})

	t.Run("codex derives nothing", func(t *testing.T) {
		in := agentResolution(t)
		got, err := domain.DeriveAgentLaunchSelection(resolve(t, in), in.Adapter, in.Offer)
		if err != nil || got != (domain.AgentLaunchSelection{}) {
			t.Fatalf("DeriveAgentLaunchSelection = %+v, %v, want empty", got, err)
		}
	})

	t.Run("offer the agent does not pin", func(t *testing.T) {
		in := claudeAgentResolution(t)
		agent := resolve(t, in)
		in.Offer.RouteModelID = "claude-fable-5-1"
		in.Offer.Digest = mustComputeDigest(t, in.Offer.ComputeDigest)
		if _, err := domain.DeriveAgentLaunchSelection(agent, in.Adapter, in.Offer); !errors.Is(err, domain.ErrAgentJoinInvalid) {
			t.Fatalf("DeriveAgentLaunchSelection = %v, want %v", err, domain.ErrAgentJoinInvalid)
		}
	})
}
