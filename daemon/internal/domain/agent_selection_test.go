package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// selectionAgent is the shared resolution's agent with the fragments it pins.
func selectionAgent(t *testing.T) (domain.AgentResolutionInput, domain.AgentDefinition) {
	t.Helper()
	in := agentResolution(t)
	agent, err := domain.ResolveAgentDefinition(in)
	if err != nil {
		t.Fatal(err)
	}
	return in, agent
}

// selectionCatalog is a catalog for the shared enrollment that lists the
// shared offer's model with the Codex efforts.
func selectionCatalog(enrollmentID domain.ClientEnrollmentID) *domain.DiscoveredModelCatalog {
	return &domain.DiscoveredModelCatalog{
		EnrollmentID: enrollmentID, HarnessBuild: "codex-cli 0.29.0",
		ObservedAt: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		Models: []domain.DiscoveredModel{
			{ID: "gpt-5.6-sol", DisplayName: "Sol", EffortLevels: []string{"low", "medium", "high", "xhigh"}},
			{ID: "gpt-5.6-mini", DisplayName: "Mini", EffortLevels: []string{}},
		},
	}
}

func selectionInput(t *testing.T) domain.AgentSelectionInput {
	t.Helper()
	in := agentResolution(t)
	return domain.AgentSelectionInput{
		Names: domain.AgentSelectionNames{
			Enrollment: string(in.Enrollment.ID), Model: "gpt-5.6-sol", Effort: "xhigh",
		},
		Identity: in.Identity, Enrollment: in.Enrollment, Adapter: in.Adapter,
		Catalog: selectionCatalog(in.Enrollment.ID),
	}
}

func TestAgentSelectionDigest(t *testing.T) {
	t.Parallel()
	selection, err := domain.ResolveAgentSelection(selectionInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := selection.Validate(); err != nil {
		t.Fatalf("resolved selection = %v", err)
	}
	// Two selections that differ only in effort are two selections.
	in := selectionInput(t)
	in.Names.Effort = "high"
	high, err := domain.ResolveAgentSelection(in)
	if err != nil {
		t.Fatal(err)
	}
	if high.Digest == selection.Digest {
		t.Fatalf("effort high and xhigh hash alike: %s", high.Digest)
	}
	tampered := selection
	tampered.Effort = "high"
	if err := tampered.Validate(); !errors.Is(err, domain.ErrAgentDigestMismatch) {
		t.Fatalf("effort edited under the digest = %v, want ErrAgentDigestMismatch", err)
	}
	unversioned := selection
	unversioned.EncodingVersion = 2
	if err := unversioned.Validate(); !errors.Is(err, domain.ErrAgentEncodingVersion) {
		t.Fatalf("encoding version 2 = %v, want ErrAgentEncodingVersion", err)
	}
}

func TestReduceAgentSelection(t *testing.T) {
	t.Parallel()
	in, agent := selectionAgent(t)
	offer := in.Offer
	reduced, err := domain.ReduceAgentSelection(agent, in.Adapter, offer)
	if err != nil {
		t.Fatal(err)
	}
	// Codex has no max: the agent's max reduces to the xhigh it sends.
	if reduced.EnrollmentID != in.Enrollment.ID || reduced.ModelID != offer.RouteModelID ||
		reduced.Effort != "xhigh" || reduced.AdapterDigest != in.Adapter.Digest {
		t.Fatalf("reduced = %+v", reduced)
	}
	resolved, err := domain.ResolveAgentSelection(selectionInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != reduced {
		t.Fatalf("resolved %+v, the agent reduces to %+v", resolved, reduced)
	}
	other := adapterFragment(t)
	other.AdapterBuild = "codex_proto_v2@build-1"
	if other.Digest, err = other.ComputeDigest(); err != nil {
		t.Fatal(err)
	}
	if _, err := domain.ReduceAgentSelection(agent, other, offer); !errors.Is(err, domain.ErrAgentJoinInvalid) {
		t.Fatalf("an adapter the agent does not pin = %v, want ErrAgentJoinInvalid", err)
	}
}

func TestResolveAgentSelectionRefusals(t *testing.T) {
	t.Parallel()
	claudeAdapter := adapterFragment(t)
	claudeAdapter.ClientKind, claudeAdapter.Vendor = domain.HarnessClientClaudeCode, domain.AgentVendorClaude
	digest, err := claudeAdapter.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	claudeAdapter.Digest = digest
	cases := []struct {
		name   string
		mutate func(*domain.AgentSelectionInput)
		want   error
	}{
		{"a model the catalog lacks", func(in *domain.AgentSelectionInput) {
			in.Names.Model = "gpt-5.6-luna"
		}, domain.ErrAgentJoinInvalid},
		{"an effort the model does not report", func(in *domain.AgentSelectionInput) {
			in.Names.Effort = "max"
		}, domain.ErrAgentJoinInvalid},
		{"an effort on a model that reports none", func(in *domain.AgentSelectionInput) {
			in.Names.Model, in.Names.Effort = "gpt-5.6-mini", "low"
		}, domain.ErrAgentJoinInvalid},
		{"a disabled identity", func(in *domain.AgentSelectionInput) {
			in.Identity.Enabled = false
		}, domain.ErrAgentJoinInvalid},
		{"another enrollment", func(in *domain.AgentSelectionInput) {
			in.Names.Enrollment = "enroll-2"
		}, domain.ErrAgentJoinInvalid},
		{"an adapter for another client kind", func(in *domain.AgentSelectionInput) {
			in.Adapter = claudeAdapter
		}, domain.ErrAgentJoinInvalid},
		{"a model a launch would read as an option", func(in *domain.AgentSelectionInput) {
			in.Names.Model = "--yolo"
		}, domain.ErrAgentJoinInvalid},
		{"an effort with whitespace", func(in *domain.AgentSelectionInput) {
			in.Names.Effort = "x high"
		}, domain.ErrAgentJoinInvalid},
		{"another enrollment's catalog", func(in *domain.AgentSelectionInput) {
			in.Catalog = selectionCatalog("enroll-2")
		}, domain.ErrAgentJoinInvalid},
		{"the native-default id on codex", func(in *domain.AgentSelectionInput) {
			in.Names.Model, in.Names.Effort = domain.ClaudeCodeNativeDefaultRouteModelID, "harness_default"
		}, domain.ErrAgentJoinInvalid},
		{"no effort", func(in *domain.AgentSelectionInput) {
			in.Names.Effort = ""
		}, domain.ErrEmptyField},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := selectionInput(t)
			tc.mutate(&in)
			if _, err := domain.ResolveAgentSelection(in); !errors.Is(err, tc.want) {
				t.Fatalf("ResolveAgentSelection = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestResolveAgentSelectionAdmits(t *testing.T) {
	t.Parallel()
	claude := func(t *testing.T) domain.AgentSelectionInput {
		in := selectionInput(t)
		in.Enrollment.HarnessClient = domain.HarnessClientClaudeCode
		in.Adapter.ClientKind, in.Adapter.Vendor = domain.HarnessClientClaudeCode, domain.AgentVendorClaude
		digest, err := in.Adapter.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		in.Adapter.Digest = digest
		in.Catalog = nil
		return in
	}
	cases := []struct {
		name   string
		mutate func(*domain.AgentSelectionInput)
		admits bool
	}{
		{"harness_default on a catalog model", func(in *domain.AgentSelectionInput) {
			in.Names.Effort = "harness_default"
		}, true},
		{"harness_default on a model with no efforts", func(in *domain.AgentSelectionInput) {
			in.Names.Model, in.Names.Effort = "gpt-5.6-mini", "harness_default"
		}, true},
		// The bridge: no catalog takes the model as named and an effort the
		// client's table produces.
		{"bridge: an unlisted model", func(in *domain.AgentSelectionInput) {
			in.Catalog, in.Names.Model = nil, "gpt-5.6-luna"
		}, true},
		{"bridge: a Codex native effort", func(in *domain.AgentSelectionInput) {
			in.Catalog = nil
		}, true},
		{"bridge: an effort Codex has no native value for", func(in *domain.AgentSelectionInput) {
			in.Catalog, in.Names.Effort = nil, "max"
		}, false},
		// A full model id finds the alias entry that resolves to it, as the
		// Claude CLI lists its current models.
		{"a full id an alias resolves to", func(in *domain.AgentSelectionInput) {
			in.Catalog.Models[0].ID, in.Catalog.Models[0].ResolvedModel = "sol", new("gpt-5.6-sol")
		}, true},
		{"a full id an alias resolves to, at an effort the alias lacks", func(in *domain.AgentSelectionInput) {
			in.Catalog.Models[0].ID, in.Catalog.Models[0].ResolvedModel = "sol", new("gpt-5.6-sol")
			in.Catalog.Models[0].EffortLevels = []string{"low"}
		}, false},
		// An entry with the exact id wins over an alias resolving to it, so
		// the alias's efforts don't widen the exact entry's.
		{"an exact id shadows an alias resolving to it", func(in *domain.AgentSelectionInput) {
			alias := in.Catalog.Models[0]
			alias.ID, alias.ResolvedModel = "sol", new("gpt-5.6-sol")
			in.Catalog.Models[0].EffortLevels = []string{"low"}
			in.Catalog.Models = append(in.Catalog.Models, alias)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := selectionInput(t)
			tc.mutate(&in)
			_, err := domain.ResolveAgentSelection(in)
			if tc.admits != (err == nil) {
				t.Fatalf("ResolveAgentSelection = %v, admits want %v", err, tc.admits)
			}
		})
	}
	t.Run("the native-default id on claude_code", func(t *testing.T) {
		t.Parallel()
		in := claude(t)
		in.Names.Model, in.Names.Effort = domain.ClaudeCodeNativeDefaultRouteModelID, "harness_default"
		selection, err := domain.ResolveAgentSelection(in)
		if err != nil {
			t.Fatal(err)
		}
		if model, effort := selection.LaunchValues(domain.HarnessClientClaudeCode); model != "" || effort != "" {
			t.Fatalf("native default passes model %q effort %q, want neither", model, effort)
		}
		// A recorded catalog does not list the reserved id; it still admits.
		in.Catalog = selectionCatalog(in.Enrollment.ID)
		if _, err := domain.ResolveAgentSelection(in); err != nil {
			t.Fatalf("native default under a catalog = %v", err)
		}
		in.Names.Effort = "xhigh"
		if _, err := domain.ResolveAgentSelection(in); !errors.Is(err, domain.ErrAgentJoinInvalid) {
			t.Fatalf("native default at an effort claude_code has no native value for = %v", err)
		}
	})
}

func TestSelectAdapterForLaunch(t *testing.T) {
	t.Parallel()
	seal := func(t *testing.T, f domain.AdapterFragment) domain.AdapterFragment {
		t.Helper()
		digest, err := f.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		f.Digest = digest
		return f
	}
	codexWriter := adapterFragment(t)
	codexReader := codexWriter
	codexReader.AdapterBuild = "codex_review_v1@build-2"
	codexReader.LaunchCapabilities = domain.NewLaunchCapabilitySet(
		domain.LaunchCapReadTools, domain.LaunchCapInstructionDelivery,
		domain.LaunchCapStructuredOutput, domain.LaunchCapContextSeverance, domain.LaunchCapRouteStoreContract,
	)
	codexReader = seal(t, codexReader)
	claude := codexWriter
	claude.ClientKind, claude.Vendor = domain.HarnessClientClaudeCode, domain.AgentVendorClaude
	claude = seal(t, claude)

	review := launchSpec(t) // read-only, forbids auxiliary inference
	writer := launchSpec(t)
	writer.Stage, writer.Writer, writer.AuxiliaryInference = domain.StageNameImplementation, true, domain.AuxiliaryObserved
	writer = sealedLaunch(t, writer)

	got, err := domain.SelectAdapterForLaunch(
		[]domain.AdapterFragment{claude, codexWriter}, domain.HarnessClientCodexCLI, writer, "high")
	if err != nil || got.Digest != codexWriter.Digest {
		t.Fatalf("writer launch on codex = %v %v, want the writer adapter", got.AdapterBuild, err)
	}
	if _, err := domain.SelectAdapterForLaunch(
		[]domain.AdapterFragment{claude, codexReader}, domain.HarnessClientCodexCLI, writer, "high",
	); !errors.Is(err, domain.ErrNoAdapterForLaunch) {
		t.Fatalf("writer launch with only a read-only codex adapter = %v, want ErrNoAdapterForLaunch", err)
	}
	if _, err := domain.SelectAdapterForLaunch(
		[]domain.AdapterFragment{codexWriter}, domain.HarnessClientCodexCLI, review, "high",
	); !errors.Is(err, domain.ErrNoAdapterForLaunch) {
		t.Fatalf("review launch, nothing waived = %v, want ErrNoAdapterForLaunch", err)
	}
	if _, err := domain.SelectAdapterForLaunch(
		[]domain.AdapterFragment{claude, codexWriter, codexReader}, domain.HarnessClientCodexCLI, review, "high",
		domain.LaunchCapAuxiliaryInferenceControl,
	); !errors.Is(err, domain.ErrAmbiguousAdapterForLaunch) {
		t.Fatalf("two codex adapters covering the review launch = %v, want ErrAmbiguousAdapterForLaunch", err)
	}
	// A retired adapter that sends only harness_default beside its successor
	// does not make the choice ambiguous for an effort only one sends.
	retired := codexReader
	retired.AdapterBuild, retired.SendableEfforts = "codex_review_v0@build-1", []domain.EffortLevel{domain.EffortHarnessDefault}
	retired = seal(t, retired)
	got, err = domain.SelectAdapterForLaunch(
		[]domain.AdapterFragment{retired, codexReader}, domain.HarnessClientCodexCLI, review, "high",
		domain.LaunchCapAuxiliaryInferenceControl)
	if err != nil || got.Digest != codexReader.Digest {
		t.Fatalf("review launch beside a retired adapter = %v %v, want the adapter that sends high", got.AdapterBuild, err)
	}
	if _, err := domain.SelectAdapterForLaunch(
		[]domain.AdapterFragment{retired}, domain.HarnessClientCodexCLI, review, "high",
		domain.LaunchCapAuxiliaryInferenceControl,
	); !errors.Is(err, domain.ErrNoAdapterForLaunch) {
		t.Fatalf("an effort no adapter sends = %v, want ErrNoAdapterForLaunch", err)
	}
	got, err = domain.SelectAdapterForLaunch(
		[]domain.AdapterFragment{claude, codexReader}, domain.HarnessClientCodexCLI, review, "high",
		domain.LaunchCapAuxiliaryInferenceControl)
	if err != nil || got.Digest != codexReader.Digest {
		t.Fatalf("review launch on codex = %v %v, want the read-only adapter", got.AdapterBuild, err)
	}
}

func sealedLaunch(t *testing.T, l domain.LaunchSpec) domain.LaunchSpec {
	t.Helper()
	digest, err := l.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	l.Digest = digest
	return l
}

func TestLineageGroupOf(t *testing.T) {
	for _, tc := range []struct {
		client domain.HarnessClientKind
		model  string
		want   string
	}{
		{domain.HarnessClientClaudeCode, "claude-opus-4-5", domain.LineageGroupAnthropic},
		{domain.HarnessClientClaudeCode, domain.ClaudeCodeNativeDefaultRouteModelID, domain.LineageGroupAnthropic},
		{domain.HarnessClientCodexCLI, "gpt-5.6-sol", domain.LineageGroupOpenAI},
		{domain.HarnessClientCodexCLI, "codex-mini-latest", domain.LineageGroupOpenAI},
		// An alias or another vendor's id stays unknown, never a guess.
		{domain.HarnessClientClaudeCode, "opus", ""},
		{domain.HarnessClientClaudeCode, "gpt-5.6-sol", ""},
		{domain.HarnessClientCodexCLI, "claude-opus-4-5", ""},
		{"", "claude-opus-4-5", ""},
	} {
		if got := domain.LineageGroupOf(tc.client, tc.model); got != tc.want {
			t.Errorf("LineageGroupOf(%q, %q) = %q, want %q", tc.client, tc.model, got, tc.want)
		}
	}
	if relation := domain.CompareLineage(domain.LineageGroupOf(domain.HarnessClientClaudeCode, "opus"), "anthropic"); relation != domain.LineageUnknown {
		t.Errorf("an unknown derived group compares %q, want %q", relation, domain.LineageUnknown)
	}
}
