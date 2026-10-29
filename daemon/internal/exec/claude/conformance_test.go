package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const conformanceTestID domain.InvocationID = "conformance-test"

func conformanceTestTask() ConformanceTask {
	return ConformanceTask{ReadNonce: "read-challenge-123", InstructionNonce: "instruction-challenge-456"}
}

func conformanceTestTranscript(mcp string) []byte {
	return fmt.Appendf(nil, `{"type":"system","subtype":"init","session_id":%q,"mcp_servers":%s,"plugins":[]}`+"\n"+
		`{"type":"result","subtype":"success","session_id":%q,"is_error":false}`+"\n", sessionIDFor(conformanceTestID), mcp, sessionIDFor(conformanceTestID))
}

func writeConformanceTestBlob(t *testing.T, root, channel string, body []byte) export.Digest {
	t.Helper()
	sum := sha256.Sum256(body)
	hexDigest := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, channel, "sha256")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hexDigest), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return export.Digest("sha256:" + hexDigest)
}

func conformanceTestResult(t *testing.T, summary, output, transcript []byte) *ward.HandoffResult {
	t.Helper()
	root := t.TempDir()
	size := int64(len(output))
	digest := writeConformanceTestBlob(t, root, "blobs", output)
	result := &ward.HandoffResult{
		ExportDir: root, EvidencePresent: true,
		Manifest:  export.Manifest{Entries: []export.Entry{{Path: conformanceOutput, Kind: export.EntryRegular, Digest: &digest, Size: &size}}},
		AuthStore: ward.AuthStoreObservation{Leased: true, Shared: true, PostAttested: true, AuthIdentityID: "claude-test", Holder: conformanceTestID, PreDigest: "store-digest", PostDigest: "store-digest"},
		Egress:    ward.EgressObservation{Profile: domain.EgressProviderOnly, HostOnly: true, Allowlist: []string{"api.anthropic.com:443"}},
	}
	for _, source := range []struct {
		label, media string
		body         []byte
	}{
		{export.SummaryEvidenceLabel, "text/markdown", summary}, {"agent-transcript", "application/jsonl", transcript},
	} {
		result.Evidence.Entries = append(result.Evidence.Entries, export.EvidenceEntry{
			Label: source.label, MediaType: source.media, Size: int64(len(source.body)),
			Digest:     writeConformanceTestBlob(t, root, export.EvidenceBlobsDirname, source.body),
			Provenance: export.EvidenceProvenance{ProducerInvocationID: string(conformanceTestID)},
		})
	}
	return result
}

func TestJudgeConformanceIndependentObservations(t *testing.T) {
	adapter, err := agentbaseline.ClaudeWardAdapter()
	if err != nil {
		t.Fatal(err)
	}
	task := conformanceTestTask()
	full := []byte(task.ReadNonce + "\n" + task.InstructionNonce)
	for _, missing := range append(domain.LaunchCapabilitySet{""}, adapter.LaunchCapabilities...) {
		t.Run(string(missing), func(t *testing.T) {
			summary, output, transcript := full, full, conformanceTestTranscript("[]")
			switch missing {
			case domain.LaunchCapReadTools:
				summary = []byte(task.InstructionNonce)
			case domain.LaunchCapInstructionDelivery:
				summary = []byte(task.ReadNonce)
			case domain.LaunchCapMutationTools:
				output = []byte("unrelated change")
			case domain.LaunchCapContextSeverance:
				transcript = conformanceTestTranscript(`[{"name":"ambient"}]`)
			case "", domain.LaunchCapStructuredOutput, domain.LaunchCapRouteStoreContract:
				// These observations are changed on the result below.
			case domain.LaunchCapExactResume, domain.LaunchCapAuxiliaryInferenceControl:
				t.Fatal("unexpected capability in the baseline Claude adapter")
			}
			result := conformanceTestResult(t, summary, output, transcript)
			if missing == domain.LaunchCapStructuredOutput {
				result.EvidencePresent = false
			}
			if missing == domain.LaunchCapRouteStoreContract {
				result.AuthStore.PostAttested = false
			}
			got, err := JudgeConformance(result, task, conformanceTestID, []string{"api.anthropic.com:443"})
			if err != nil {
				t.Fatal(err)
			}
			want := slices.DeleteFunc(slices.Clone(adapter.LaunchCapabilities), func(c domain.LaunchCapability) bool { return c == missing })
			if !slices.Equal(got, want) {
				t.Fatalf("capabilities = %v, want %v", got, want)
			}
		})
	}
}

func TestJudgeConformanceRejectsMissingAndTamperedExports(t *testing.T) {
	task := conformanceTestTask()
	full := []byte(task.ReadNonce + task.InstructionNonce)
	for _, kind := range []string{"no export", "no transcript", "tampered blob", "invalid transcript", "empty challenge"} {
		t.Run(kind, func(t *testing.T) {
			result := conformanceTestResult(t, full, full, conformanceTestTranscript("[]"))
			challenge := task
			switch kind {
			case "no export":
				result.ExportDir = ""
			case "no transcript":
				result.Evidence.Entries = result.Evidence.Entries[:1]
			case "tampered blob":
				entry := result.Evidence.Entries[1]
				if err := os.WriteFile(filepath.Join(result.ExportDir, "evidence/sha256", strings.TrimPrefix(string(entry.Digest), "sha256:")), []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "invalid transcript":
				result = conformanceTestResult(t, full, full, []byte(`{"type":"system","type":"result"}`))
			case "empty challenge":
				challenge.ReadNonce = ""
			}
			if _, err := JudgeConformance(result, challenge, conformanceTestID, []string{"api.anthropic.com:443"}); err == nil {
				t.Fatal("unverifiable run passed")
			}
		})
	}
}

func TestJudgeConformanceRouteRequiresObservedAllowlistAndStore(t *testing.T) {
	task := conformanceTestTask()
	for _, mutate := range []func(*ward.HandoffResult){
		func(r *ward.HandoffResult) { r.Egress.Allowlist = append(r.Egress.Allowlist, "telemetry.invalid:443") },
		func(r *ward.HandoffResult) { r.AuthStore.PostDigest = "changed" },
		func(r *ward.HandoffResult) { r.AuthStore.Shared = false },
	} {
		result := conformanceTestResult(t, nil, nil, conformanceTestTranscript("[]"))
		mutate(result)
		got, err := JudgeConformance(result, task, conformanceTestID, []string{"api.anthropic.com:443"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Has(domain.LaunchCapRouteStoreContract) {
			t.Fatal("unobserved route/store contract passed")
		}
	}
}

func TestConformanceHandoffUsesWriterLaunch(t *testing.T) {
	in := testProviderHandoffInput()
	task, err := NewConformanceTask(t.Context(), t.TempDir(), in.Spec.Base)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := os.ReadFile(filepath.Join(task.Seed, ".git/freeside-repository-id"))
	if err != nil || string(stamp) != "42\n" {
		t.Fatalf("seed repository identity = %q, %v", stamp, err)
	}
	head, err := os.ReadFile(filepath.Join(task.Seed, ".git/HEAD"))
	if err != nil || string(head) != task.Base.BaseSHA+"\n" {
		t.Fatalf("seed is not detached at its declared base: %q, %v", head, err)
	}
	volumes := testAuthStoreVolumes{volume: "enrolled-volume"}
	hs, err := ConformanceHandoff(t.Context(), ConformanceInput{
		Task: task, InvocationID: in.InvocationID,
		Image: in.Spec.ImageRef, Identity: in.Spec.AuthIdentityID, RouteModelID: "claude-model", NativeEffort: "low", Volumes: volumes,
	})
	if err != nil {
		t.Fatal(err)
	}
	in.Spec.Base, in.Spec.RouteModelID, in.Spec.NativeEffort = task.Base, "claude-model", "low"
	in.Seed, in.Instructions, in.PromptDelivery = task.Seed, hs.Agent.VendorInstructions, launchDelivery
	// The command depends on the prompt file's path, never its bytes.
	in.Prompt = "same delivery, different user task"
	want, err := (claudeProvider{volumes: volumes}).HandoffSpec(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hs.Agent.Command, want.Agent.Command) || !reflect.DeepEqual(hs.Agent.CredentialMounts, want.Agent.CredentialMounts) ||
		hs.Agent.LaunchState != want.Agent.LaunchState || hs.Agent.OutcomeMarkerPath != want.Agent.OutcomeMarkerPath ||
		!reflect.DeepEqual(hs.Agent.InstructionPolicy, want.Agent.InstructionPolicy) || hs.Agent.PromptFile == nil ||
		!reflect.DeepEqual(hs.AuthStoreLease, want.AuthStoreLease) {
		t.Fatal("conformance diverges from production launch")
	}
	if hs.Class != ward.LaunchConformance || hs.Size != ward.DefaultLaunchSize(ward.LaunchConformance) {
		t.Fatal("conformance launch lacks its declared resource budget")
	}
}
