package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

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
	identity := authIdentity()
	identity.AccountBinding = "acct-1"
	identity.Enabled = true
	e := enrollment()
	return domain.AgentSelectionInput{
		Names: domain.AgentSelectionNames{
			Enrollment: string(e.ID), Model: "gpt-5.6-sol", Effort: "xhigh",
		},
		Identity: identity, Enrollment: e, Adapter: adapterFragment(t),
		Catalog: selectionCatalog(e.ID),
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
		// An enrollment bound to another account would carry this identity's
		// credential onto it.
		{"a credential for another account", func(in *domain.AgentSelectionInput) {
			in.Enrollment.AccountBinding = "acct-2"
		}, domain.ErrAccountBindingMismatch},
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
		{"an allow list that omits the model", func(in *domain.AgentSelectionInput) {
			in.Models = &domain.RouteModelList{Mode: domain.ModelListAllow, Models: []string{"gpt-5.6-mini"}}
		}, domain.ErrAgentJoinInvalid},
		{"a deny list that names the model", func(in *domain.AgentSelectionInput) {
			in.Models = &domain.RouteModelList{Mode: domain.ModelListDeny, Models: []string{"gpt-5.6-sol"}}
		}, domain.ErrAgentJoinInvalid},
		{"a model list with no model", func(in *domain.AgentSelectionInput) {
			in.Models = &domain.RouteModelList{Mode: domain.ModelListAllow}
		}, domain.ErrInvalidRouteModelList},
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

// TestResolveAgentSelectionNamesTheUnservedPart: a refusal under the
// served-tuple rule matches its served sentinel beside ErrAgentJoinInvalid,
// keeping the text it always printed, and no other refusal matches either
// sentinel, so admission names a part only for these legs.
func TestResolveAgentSelectionNamesTheUnservedPart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*domain.AgentSelectionInput)
		part   error // nil: the refusal names no part
	}{
		{"a model the catalog lacks", func(in *domain.AgentSelectionInput) {
			in.Names.Model = "gpt-5.6-luna"
		}, domain.ErrDiscoveredModelUnserved},
		{"an effort the model does not report", func(in *domain.AgentSelectionInput) {
			in.Names.Effort = "max"
		}, domain.ErrDiscoveredEffortUnserved},
		{"an effort outside the native table under the bridge", func(in *domain.AgentSelectionInput) {
			in.Catalog, in.Names.Effort = nil, "vibes"
		}, domain.ErrDiscoveredEffortUnserved},
		{"the native-default id on codex", func(in *domain.AgentSelectionInput) {
			in.Names.Model, in.Names.Effort = domain.ClaudeCodeNativeDefaultRouteModelID, "harness_default"
		}, domain.ErrDiscoveredModelUnserved},
		{"a disabled identity", func(in *domain.AgentSelectionInput) {
			in.Identity.Enabled = false
		}, nil},
		{"a model list that does not admit the model", func(in *domain.AgentSelectionInput) {
			in.Models = &domain.RouteModelList{Mode: domain.ModelListDeny, Models: []string{"gpt-5.6-sol"}}
		}, nil},
		{"a model a launch would read as an option", func(in *domain.AgentSelectionInput) {
			in.Names.Model = "--yolo"
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := selectionInput(t)
			tc.mutate(&in)
			_, err := domain.ResolveAgentSelection(in)
			if !errors.Is(err, domain.ErrAgentJoinInvalid) {
				t.Fatalf("ResolveAgentSelection = %v, want ErrAgentJoinInvalid", err)
			}
			for _, sentinel := range []error{domain.ErrDiscoveredModelUnserved, domain.ErrDiscoveredEffortUnserved} {
				if got, want := errors.Is(err, sentinel), errors.Is(tc.part, sentinel); got != want {
					t.Errorf("errors.Is(%v, %v) = %t, want %t", err, sentinel, got, want)
				}
				if strings.Contains(err.Error(), sentinel.Error()) {
					t.Errorf("refusal text %q gained the sentinel's", err)
				}
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
		{"an allow list that names the model", func(in *domain.AgentSelectionInput) {
			in.Models = &domain.RouteModelList{Mode: domain.ModelListAllow, Models: []string{"gpt-5.6-mini", "gpt-5.6-sol"}}
		}, true},
		{"a deny list that omits the model", func(in *domain.AgentSelectionInput) {
			in.Models = &domain.RouteModelList{Mode: domain.ModelListDeny, Models: []string{"gpt-5.6-mini"}}
		}, true},
		// The list matches the id as the selection names it: denying the
		// alias does not deny the model it resolves to.
		{"a deny list naming an alias of the model", func(in *domain.AgentSelectionInput) {
			in.Catalog.Models[0].ID, in.Catalog.Models[0].ResolvedModel = "sol", new("gpt-5.6-sol")
			in.Models = &domain.RouteModelList{Mode: domain.ModelListDeny, Models: []string{"sol"}}
		}, true},
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

func TestRouteModelList(t *testing.T) {
	t.Parallel()
	allow := domain.RouteModelList{Mode: domain.ModelListAllow, Models: []string{"a", "b"}}
	deny := domain.RouteModelList{Mode: domain.ModelListDeny, Models: []string{"a"}}
	for _, tc := range []struct {
		list  domain.RouteModelList
		model string
		want  bool
	}{
		{allow, "a", true}, {allow, "c", false}, {deny, "a", false}, {deny, "c", true},
	} {
		if got := tc.list.Admits(tc.model); got != tc.want {
			t.Errorf("%s %v admits %q = %v, want %v", tc.list.Mode, tc.list.Models, tc.model, got, tc.want)
		}
	}
	for name, list := range map[string]domain.RouteModelList{
		"unknown mode": {Mode: "only", Models: []string{"a"}},
		"no model":     {Mode: domain.ModelListDeny},
		"duplicate":    {Mode: domain.ModelListAllow, Models: []string{"a", "a"}},
		"empty model":  {Mode: domain.ModelListAllow, Models: []string{""}},
		"option model": {Mode: domain.ModelListAllow, Models: []string{"-a"}},
		"spaced model": {Mode: domain.ModelListAllow, Models: []string{"a b"}},
	} {
		if err := list.Validate(); !errors.Is(err, domain.ErrInvalidRouteModelList) {
			t.Errorf("%s: Validate() = %v, want %v", name, err, domain.ErrInvalidRouteModelList)
		}
	}
	if err := allow.Validate(); err != nil {
		t.Errorf("allow list: %v", err)
	}
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

// A call adapter gives the harness no tools; SelectAdapterForCall takes
// the one for the client that sends the effort, and never a stage adapter.
func TestSelectAdapterForCall(t *testing.T) {
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
	stage := adapterFragment(t)
	stage.ClientKind, stage.Vendor = domain.HarnessClientClaudeCode, domain.AgentVendorClaude
	stage = seal(t, stage)
	call := stage
	call.AdapterBuild = "claude_code_call_v2@build-1"
	call.LaunchCapabilities = domain.NewLaunchCapabilitySet(
		domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput,
	)
	call.SendableEfforts = []domain.EffortLevel{domain.EffortHarnessDefault, domain.EffortHigh}
	call = seal(t, call)
	retired := call
	retired.AdapterBuild, retired.SendableEfforts = "claude_code_call_v1@build-1", []domain.EffortLevel{domain.EffortHarnessDefault}
	retired = seal(t, retired)

	got, err := domain.SelectAdapterForCall(
		[]domain.AdapterFragment{stage, retired, call}, domain.HarnessClientClaudeCode, "high")
	if err != nil || got.Digest != call.Digest {
		t.Fatalf("call at high = %v %v, want the adapter that sends it", got.AdapterBuild, err)
	}
	if _, err := domain.SelectAdapterForCall(
		[]domain.AdapterFragment{stage}, domain.HarnessClientClaudeCode, "high",
	); !errors.Is(err, domain.ErrNoAdapterForLaunch) {
		t.Fatalf("only a stage adapter = %v, want ErrNoAdapterForLaunch", err)
	}
	if _, err := domain.SelectAdapterForCall(
		[]domain.AdapterFragment{call}, domain.HarnessClientCodexCLI, "high",
	); !errors.Is(err, domain.ErrNoAdapterForLaunch) {
		t.Fatalf("another client = %v, want ErrNoAdapterForLaunch", err)
	}
	if _, err := domain.SelectAdapterForCall(
		[]domain.AdapterFragment{retired, call}, domain.HarnessClientClaudeCode, "harness_default",
	); !errors.Is(err, domain.ErrAmbiguousAdapterForLaunch) {
		t.Fatalf("two call adapters that send the effort = %v, want ErrAmbiguousAdapterForLaunch", err)
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

// TestSelectionServed: the router and the routing policy's setter keep the
// enrollments admission resolves. The native-default model passes a
// claude_code enrollment whatever its catalog lists, an enrollment with no
// catalog passes on a native effort, and otherwise the catalog decides.
func TestSelectionServed(t *testing.T) {
	t.Parallel()
	catalog := selectionCatalog("codex-a/codex_cli")
	for name, tc := range map[string]struct {
		client        domain.HarnessClientKind
		catalog       *domain.DiscoveredModelCatalog
		model, effort string
		served        bool
	}{
		"native default on claude_code": {
			domain.HarnessClientClaudeCode, catalog, domain.ClaudeCodeNativeDefaultRouteModelID, "high", true,
		},
		"native default on codex_cli": {
			domain.HarnessClientCodexCLI, catalog, domain.ClaudeCodeNativeDefaultRouteModelID, "high", false,
		},
		"native default without a native effort": {
			domain.HarnessClientClaudeCode, nil, domain.ClaudeCodeNativeDefaultRouteModelID, "turbo", false,
		},
		"no catalog, native effort":    {domain.HarnessClientCodexCLI, nil, "gpt-5.6-sol", "high", true},
		"no catalog, no native effort": {domain.HarnessClientCodexCLI, nil, "gpt-5.6-sol", "turbo", false},
		"catalog serves":               {domain.HarnessClientCodexCLI, catalog, "gpt-5.6-sol", "xhigh", true},
		"catalog lacks the model":      {domain.HarnessClientCodexCLI, catalog, "gpt-5.6-other", "high", false},
		"catalog lacks the effort":     {domain.HarnessClientCodexCLI, catalog, "gpt-5.6-mini", "high", false},
	} {
		if err := domain.SelectionServed(tc.client, tc.catalog, tc.model, tc.effort); (err == nil) != tc.served {
			t.Errorf("%s: SelectionServed = %v, want served %v", name, err, tc.served)
		}
	}
}
