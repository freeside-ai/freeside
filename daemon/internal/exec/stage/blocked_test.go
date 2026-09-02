package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func blockedDecisions() []domain.Decision {
	return []domain.Decision{{
		Question:    "Which retention period applies to exported logs?",
		WhyBlocking: "The schema cannot be fixed without it.",
		Options: []domain.DecisionOption{
			{Label: "30 days", Tradeoffs: "Cheaper storage, shorter audit window."},
			{Label: "1 year", Tradeoffs: "Longer audit window, higher storage cost."},
		},
		Recommendation: "30 days",
	}}
}

// blockedWorkspace writes the launcher-declared blocked outcome into a
// workspace's reserved evidence subtree, plus any extra repo-channel files,
// and exports it the way the ward hands a released export to the driver.
func blockedWorkspace(t *testing.T, body []byte, extra map[string]string) (string, export.Manifest, export.EvidenceManifest) {
	t.Helper()
	workspace := t.TempDir()
	evidenceDir := filepath.Join(workspace, export.EvidenceWorkspaceDir)
	if err := os.MkdirAll(evidenceDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, filepath.FromSlash(export.BlockedEvidencePath)), body, 0o600); err != nil {
		t.Fatal(err)
	}
	descriptor, err := json.Marshal(export.EvidenceSourceManifest{
		Version: export.EvidenceSourceVersion,
		Sources: []export.EvidenceSource{{
			Label: export.BlockedEvidenceLabel, MediaType: "application/json",
			Path: export.BlockedEvidencePath, HeadBinding: export.EvidenceHeadIndependent,
			SensitivityClass:     export.EvidenceSensitivityNormal,
			ProducerInvocationID: string(testInvoke),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, filepath.FromSlash(export.EvidenceDescriptorPath)), descriptor, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range extra {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outDir, err := os.MkdirTemp("", "freeside-handoff-"+testRunIDFor(testInvoke)+"-out-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outDir) })
	manifest, err := export.Export(os.DirFS(workspace), outDir, export.Options{})
	if err != nil {
		t.Fatalf("export blocked workspace: %v", err)
	}
	evidenceBody, err := os.ReadFile(filepath.Join(outDir, export.EvidenceFilename)) //nolint:gosec // G304: test-owned export dir
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := export.DecodeEvidenceManifest(evidenceBody)
	if err != nil {
		t.Fatal(err)
	}
	return outDir, manifest, evidence
}

func emptyBaseRepo(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	repo := t.TempDir()
	if err := runRecoveryGit(ctx, repo, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := runRecoveryGit(ctx, repo, "commit", "-q", "--allow-empty", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	baseSHA, err := runRecoveryGitOutput(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return repo, baseSHA
}

// finishReleasedBlockedExport drives a released export through the driver's
// finish path (via exported-phase recovery, the same code the live pipeline
// runs) and returns the collected terminal.
func finishReleasedBlockedExport(
	t *testing.T, body []byte, extra map[string]string,
) (exec.StageResult, *stubExports, *stubArtifacts) {
	t.Helper()
	ctx := context.Background()
	repo, baseSHA := emptyBaseRepo(t)
	outDir, manifest, evidence := blockedWorkspace(t, body, extra)
	exports := newStubExports()
	artifacts := newStubArtifacts()
	d := newTestDriver(t, &stubGate{}, exports)
	d.artifacts = artifacts
	d.seeder = recoveryGitSeeder{repo: repo}
	spec := testStartSpec()
	spec.Base.BaseSHA = baseSHA
	_, planErr := os.Stat(filepath.Join(outDir, export.CommitPlanFilename))
	in := orphanWithSpec(t, d, phaseExported, &releasedExport{
		Dir: outDir, Manifest: manifest, Evidence: evidence, EvidencePresent: true,
		CommitPlanPresent: planErr == nil,
		ObservedBaseSHA:   baseSHA,
	}, spec)
	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile released blocked export: %v", err)
	}
	result, err := d.Collect(ctx, in.InvocationID)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return result, exports, artifacts
}

func TestBlockedExportBecomesBlockedTerminalWithoutCandidate(t *testing.T) {
	t.Parallel()
	for _, kind := range domain.AllBlockedKinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			body, err := domain.EncodeBlockedOutcome(domain.BlockedOutcome{
				Version: domain.BlockedOutcomeEncodingVersion, Kind: kind, Decisions: blockedDecisions(),
			})
			if err != nil {
				t.Fatal(err)
			}
			result, exports, artifacts := finishReleasedBlockedExport(t, body, nil)
			if result.Status != exec.StatusBlocked || result.HeadSHA != "" ||
				result.Summary != blockedDecisions()[0].Question || len(result.Artifacts) != 1 {
				t.Fatalf("blocked result = %#v", result)
			}
			if len(exports.records) != 0 {
				t.Fatalf("blocked export recorded %d ExecutionExports, want none", len(exports.records))
			}
			outcome, ok := exports.outcomes[testInvoke]
			if !ok || outcome.Status != domain.ExecutionOutcomeBlocked || outcome.Summary != result.Summary {
				t.Fatalf("blocked outcome = %#v, %t", outcome, ok)
			}
			stored, ok := artifacts.blobs[result.Artifacts[0]]
			if !ok {
				t.Fatal("blocked outcome bytes were not persisted")
			}
			decoded, err := domain.DecodeBlockedOutcome(stored)
			if err != nil || decoded.Kind != kind {
				t.Fatalf("persisted blocked outcome = %+v, %v", decoded, err)
			}
			claims := artifacts.claims[testInvoke]
			if len(claims) != 1 || claims[0].Label != export.BlockedEvidenceLabel ||
				claims[0].Digest != result.Artifacts[0] ||
				claims[0].Provenance.ProducerInvocationID != testInvoke {
				t.Fatalf("recorded claims = %+v, want one blocked claim from the invocation", claims)
			}
		})
	}
}

func TestBlockedExportCanonicalizesPersistedOutcome(t *testing.T) {
	t.Parallel()
	blocked := domain.BlockedOutcome{
		Version: domain.BlockedOutcomeEncodingVersion, Kind: domain.BlockedKindOwnerDecision,
		Decisions: blockedDecisions(),
	}
	raw, err := json.MarshalIndent(blocked, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := domain.EncodeBlockedOutcome(blocked)
	if err != nil {
		t.Fatal(err)
	}
	result, _, artifacts := finishReleasedBlockedExport(t, raw, nil)
	if got := artifacts.blobs[result.Artifacts[0]]; !bytes.Equal(got, canonical) {
		t.Fatalf("persisted blocked outcome = %q, want canonical %q", got, canonical)
	}
	claims := artifacts.claims[testInvoke]
	if len(claims) != 1 || claims[0].Digest != result.Artifacts[0] ||
		claims[0].Metadata.SizeBytes != int64(len(canonical)) {
		t.Fatalf("persisted blocked claim = %+v, want canonical digest and size", claims)
	}
}

func TestBlockedExportRejectsChangesCommitPlanAndMalformedOutcome(t *testing.T) {
	t.Parallel()
	valid, err := domain.EncodeBlockedOutcome(domain.BlockedOutcome{
		Version: domain.BlockedOutcomeEncodingVersion, Kind: domain.BlockedKindOwnerDecision,
		Decisions: blockedDecisions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		body  []byte
		extra map[string]string
	}{
		{"changed path", valid, map[string]string{"candidate.txt": "changed\n"}},
		{"commit plan present", valid, map[string]string{
			export.CommitPlanFilename: `{"version":"freeside.commit-plan/v1","groups":[]}`,
		}},
		{"unknown kind", []byte(`{"version":"freeside.blocked-outcome/v1","kind":"tired","decisions":[]}`), nil},
		{"credential-shaped decision", secretBlockedOutcome(t), nil},
		{"escaped credential-shaped decision", escapedSecretBlockedOutcome(t), nil},
		{"escaped JSON credential-shaped decision", escapedJSONSecretBlockedOutcome(t), nil},
		{"oversized outcome", oversizedBlockedOutcome(t), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result, exports, _ := finishReleasedBlockedExport(t, tc.body, tc.extra)
			if result.Status != exec.StatusFailed {
				t.Fatalf("result = %#v, want a failed stage", result)
			}
			if len(exports.records) != 0 {
				t.Fatal("rejected blocked export recorded an ExecutionExport")
			}
			if outcome := exports.outcomes[testInvoke]; outcome.Status != domain.ExecutionOutcomeFailed {
				t.Fatalf("outcome = %#v, want failed", outcome)
			}
		})
	}
}

// secretBlockedOutcome is a valid outcome whose tradeoffs carry a token; the
// stage must refuse it before the decisions reach a card.
func secretBlockedOutcome(t *testing.T) []byte {
	t.Helper()
	decisions := blockedDecisions()
	decisions[0].Options[0].Tradeoffs = "Use token ghp_" + strings.Repeat("A", 36)
	body, err := domain.EncodeBlockedOutcome(domain.BlockedOutcome{
		Version: domain.BlockedOutcomeEncodingVersion, Kind: domain.BlockedKindOwnerDecision,
		Decisions: decisions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func escapedSecretBlockedOutcome(t *testing.T) []byte {
	t.Helper()
	body := secretBlockedOutcome(t)
	escaped := bytes.Replace(body, []byte("ghp_"), []byte(`ghp\u005f`), 1)
	if bytes.Equal(escaped, body) {
		t.Fatal("secret fixture did not contain a GitHub token prefix")
	}
	return escaped
}

func escapedJSONSecretBlockedOutcome(t *testing.T) []byte {
	t.Helper()
	decisions := blockedDecisions()
	decisions[0].Options[0].Tradeoffs = `Use credential {"private_key_id":"` + strings.Repeat("a", 40) + `"}`
	body, err := domain.EncodeBlockedOutcome(domain.BlockedOutcome{
		Version: domain.BlockedOutcomeEncodingVersion, Kind: domain.BlockedKindOwnerDecision,
		Decisions: decisions,
	})
	if err != nil {
		t.Fatal(err)
	}
	escaped := bytes.ReplaceAll(body, []byte(`\"`), []byte(`\u0022`))
	if bytes.Equal(escaped, body) {
		t.Fatal("JSON secret fixture did not contain escaped quotes")
	}
	return escaped
}

// oversizedBlockedOutcome is syntactically an outcome whose bytes exceed the
// decoder's cap; the stage refuses it by declared size before reading it.
func oversizedBlockedOutcome(t *testing.T) []byte {
	t.Helper()
	filler := strings.Repeat(" ", int(domain.MaxBlockedOutcomeBytes)+1)
	return []byte(`{"version":"freeside.blocked-outcome/v1","kind":"owner_decision",` + filler + `"decisions":[]}`)
}

// TestRecoveredBlockedOutcomeIsAdopted: a crash after the blocked outcome
// commits but before the intent phase does converges on the durable record,
// even when the released directory is gone.
func TestRecoveredBlockedOutcomeIsAdopted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	spec := testStartSpec()
	blockedDigest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	wantUsage := []exec.UsageMeasurement{{
		Source: domain.UsageSourceAdapterTranscript,
		Kind:   domain.UsageMeasurementReportedUsage,
		Metric: "input_tokens", Unit: "tokens", Quantity: 12,
		Sequence: 1, ObservedAt: fixedNow,
	}}
	if err := d.artifacts.RecordClaims(ctx, testInvoke, []domain.AgentClaim{{
		Label: export.BlockedEvidenceLabel, Digest: blockedDigest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: testInvoke,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
	}}); err != nil {
		t.Fatal(err)
	}
	in := orphanWithSpec(t, d, phaseExported, &releasedExport{
		Dir:             filepath.Join(os.TempDir(), "freeside-handoff-"+testRunIDFor(testInvoke)+"-out-gone"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	}, spec)
	in.PendingUsage = wantUsage
	if err := d.saveIntent(in); err != nil {
		t.Fatal(err)
	}
	if err := exports.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
		InvocationID: testInvoke, AdmissionID: spec.AdmissionID,
		Status: domain.ExecutionOutcomeBlocked, Summary: "Which retention period applies?",
		RecordedAt: fixedNow,
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	result, err := d.Collect(ctx, testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Status != exec.StatusBlocked || result.Summary != "Which retention period applies?" ||
		len(result.Artifacts) != 1 || result.Artifacts[0] != blockedDigest ||
		!reflect.DeepEqual(result.Usage, wantUsage) {
		t.Fatalf("recovered result = %#v, want the recorded blocked outcome", result)
	}
	if !errors.Is(d.Reconcile(ctx), nil) {
		t.Fatal("second reconcile did not converge")
	}
}
