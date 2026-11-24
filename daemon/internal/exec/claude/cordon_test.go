package claude

import (
	"bytes"
	"context"
	"errors"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func testCordonInput() stage.ProviderHandoffInput {
	in := testProviderHandoffInput()
	in.Spec.RunnerClass = domain.BackendCordonedHostProcess
	in.Spec.EgressProfile = domain.EgressHostOpen
	return in
}

func TestCordonLaunchArgs(t *testing.T) {
	t.Parallel()
	base := testCordonInput()
	session := sessionIDFor(base.InvocationID)
	cli := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions",
		"--safe-mode", "--session-id", session,
	}
	for _, tc := range []struct {
		name string
		edit func(*stage.ProviderHandoffInput)
		want []string
	}{
		{"plain", func(*stage.ProviderHandoffInput) {}, cli},
		{"instructions", func(in *stage.ProviderHandoffInput) {
			in.Instructions.Present, in.Instructions.Body = true, []byte("# Host\nBe terse.\n")
		}, append(slices.Clone(cli), "--append-system-prompt", "# Host\nBe terse.\n")},
		{"specification with model and effort", func(in *stage.ProviderHandoffInput) {
			in.Spec.RunID = "run-provider-spec"
			in.Spec.StageID = domain.SpecificationStageID(in.Spec.RunID)
			in.Spec.RouteModelID, in.Spec.NativeEffort = "claude-opus-5-5", "xhigh"
		}, append(slices.Clone(cli),
			"--json-schema", specify.OutputJSONSchema, "--model", "claude-opus-5-5", "--effort", "xhigh")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := testCordonInput()
			tc.edit(&in)
			launch, req, err := (claudeProvider{}).CordonLaunch(context.Background(), in)
			if err != nil {
				t.Fatalf("CordonLaunch: %v", err)
			}
			if !slices.Equal(launch.Args, tc.want) {
				t.Fatalf("args = %q\nwant %q", launch.Args, tc.want)
			}
			if launch.InvocationID != in.InvocationID || launch.RunID != in.RunID || req.RunID != in.RunID ||
				launch.Base != in.Spec.Base || !bytes.Equal(launch.Prompt, []byte(in.Prompt)) {
				t.Fatalf("launch = %+v, req run %q; want the input's identity, base, and prompt", launch, req.RunID)
			}
		})
	}
}

// TestCordonLaunchDeclaresTheWardedSources ties the cordoned export to the
// warded launcher: every source a cordoned run declares is one the warded
// command's descriptor carries, and the transcript sits where the cordon
// copies it.
func TestCordonLaunchDeclaresTheWardedSources(t *testing.T) {
	t.Parallel()
	in := testCordonInput()
	_, req, err := (claudeProvider{}).CordonLaunch(context.Background(), in)
	if err != nil {
		t.Fatalf("CordonLaunch: %v", err)
	}
	if path.Dir(req.Transcript.Path) != export.EvidenceWorkspaceDir ||
		req.Transcript.SensitivityClass != export.EvidenceSensitivitySensitive {
		t.Fatalf("transcript = %+v, want a sensitive source directly under %s", req.Transcript, export.EvidenceWorkspaceDir)
	}
	warded := testProviderHandoffInput()
	hs, err := (claudeProvider{volumes: testAuthStoreVolumes{volume: "provider-volume"}}).
		HandoffSpec(context.Background(), warded)
	if err != nil {
		t.Fatalf("HandoffSpec: %v", err)
	}
	command := strings.Join(hs.Agent.Command, " ")
	labels := []string{req.Transcript.Label}
	for _, s := range append([]export.EvidenceSource{req.Transcript}, req.Sources...) {
		// Both inputs share an invocation, so the fragments match byte for byte.
		if s.ProducerInvocationID != string(in.InvocationID) {
			t.Errorf("source %s names producer %q", s.Label, s.ProducerInvocationID)
		}
		if !strings.Contains(command, shellQuote(evidenceSourceFragment(s.Label, s))) {
			t.Errorf("source %s isn't one the warded launcher declares", s.Label)
		}
		labels = append(labels, s.Label)
	}
	for _, want := range []string{
		export.PublicationEvidenceLabel, export.SummaryEvidenceLabel, export.BlockedEvidenceLabel,
		export.ScopeConflictEvidenceLabel, export.ReviewGuideEvidenceLabel,
	} {
		if !slices.Contains(labels, want) {
			t.Errorf("cordoned sources %v omit %s", labels, want)
		}
	}
}

func TestCordonLaunchRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(*stage.ProviderHandoffInput)
	}{
		{"api key isolated", func(in *stage.ProviderHandoffInput) {
			in.Spec.CredentialMode = domain.CredentialAPIKeyIsolated
		}},
		{"ward egress", func(in *stage.ProviderHandoffInput) { in.Spec.EgressProfile = domain.EgressProviderOnly }},
		{"no auth identity", func(in *stage.ProviderHandoffInput) { in.Spec.AuthIdentityID = "" }},
		{"model delimiter", func(in *stage.ProviderHandoffInput) { in.Spec.RouteModelID = "claude,x" }},
		{"effort control character", func(in *stage.ProviderHandoffInput) { in.Spec.NativeEffort = "high\n" }},
		{"unknown delivery", func(in *stage.ProviderHandoffInput) { in.PromptDelivery = "file_v1" }},
		{"another vendor's instructions", func(in *stage.ProviderHandoffInput) {
			in.Instructions = instructions(domain.AgentVendorCodex, []byte("x"))
		}},
		{"instructions over the argument limit", func(in *stage.ProviderHandoffInput) {
			in.Instructions = instructions(domain.AgentVendorClaude, bytes.Repeat([]byte("x"), cordonInstructionArgBytes+1))
		}},
		{"instructions with a NUL", func(in *stage.ProviderHandoffInput) {
			in.Instructions = instructions(domain.AgentVendorClaude, []byte("a\x00b"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := testCordonInput()
			tc.edit(&in)
			if _, _, err := (claudeProvider{}).CordonLaunch(context.Background(), in); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("CordonLaunch = %v, want ErrUnsupportedStart", err)
			}
		})
	}
}

func instructions(vendor domain.AgentVendor, body []byte) ward.VendorInstructions {
	return ward.VendorInstructions{Vendor: vendor, Present: true, Body: body}
}
