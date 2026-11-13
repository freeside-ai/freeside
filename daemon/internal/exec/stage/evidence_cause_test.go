package stage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// releasedEvidenceExport exports a workspace whose evidence channel carries
// one source the launcher declares, plus the extra workspace files.
func releasedEvidenceExport(
	t *testing.T, source export.EvidenceSource, body []byte, extra map[string]string,
) (string, export.Manifest, export.EvidenceManifest) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, export.EvidenceWorkspaceDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, filepath.FromSlash(source.Path)), body, 0o600); err != nil {
		t.Fatal(err)
	}
	descriptor, err := json.Marshal(export.EvidenceSourceManifest{
		Version: export.EvidenceSourceVersion, Sources: []export.EvidenceSource{source},
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
		t.Fatalf("export workspace: %v", err)
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

// TestImportRefusalCause pins which import refusals record their own cause:
// a refused evidence channel records invalid_evidence on the live pipeline,
// on recovery of a running handoff whose export ward returns, and on
// recovery of an already exported one; every other refusal still records
// unknown.
func TestImportRefusalCause(t *testing.T) {
	t.Parallel()
	guide := export.EvidenceSource{
		Label: export.ReviewGuideEvidenceLabel, MediaType: "application/json",
		Path: export.ReviewGuideEvidencePath, HeadBinding: export.EvidenceHeadIndependent,
		SensitivityClass: export.EvidenceSensitivitySensitive, ProducerInvocationID: string(testInvoke),
	}
	blocked := export.EvidenceSource{
		Label: export.BlockedEvidenceLabel, MediaType: "application/json",
		Path: export.BlockedEvidencePath, HeadBinding: export.EvidenceHeadIndependent,
		SensitivityClass: export.EvidenceSensitivityNormal, ProducerInvocationID: string(testInvoke),
	}
	blockedBody, err := domain.EncodeBlockedOutcome(domain.BlockedOutcome{
		Version: domain.BlockedOutcomeEncodingVersion, Kind: domain.BlockedKindOwnerDecision,
		Decisions: blockedDecisions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := map[string]string{"candidate.txt": "changed\n"}
	refusals := []struct {
		name   string
		source export.EvidenceSource
		body   []byte
		want   domain.FailureCause
	}{
		// The import refuses the guide's shape with ErrEvidenceInvalid.
		{"malformed review guide", guide, []byte(`{"version":"v1"}`), domain.FailureCauseInvalidEvidence},
		// The bytes are not the JSON the entry declares:
		// ErrEvidenceMediaMismatch.
		{"review guide that is not JSON", guide, []byte("not json"), domain.FailureCauseInvalidEvidence},
		// A blocked outcome beside a changed file: ErrUnexpectedChanges.
		{"blocked outcome with changes", blocked, blockedBody, domain.FailureCauseUnknown},
	}
	paths := map[string]func(t *testing.T, d *Driver, spec exec.StartSpec, released *releasedExport){
		"live": func(t *testing.T, d *Driver, spec exec.StartSpec, released *releasedExport) {
			d.gate = &stubGate{handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
				return &ward.HandoffResult{
					ExportDir: released.Dir, Manifest: released.Manifest,
					Evidence: released.Evidence, EvidencePresent: true,
					Workspace: ward.WorkspaceObservation{ObservedBaseSHA: released.ObservedBaseSHA},
				}, nil
			}}
			d.runPipeline(context.Background(), orphanWithSpec(t, d, phaseSeeding, nil, spec))
		},
		"running recovery": func(t *testing.T, d *Driver, spec exec.StartSpec, released *releasedExport) {
			d.gate = &stubGate{recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
				return &ward.RecoveryResult{
					Outcome: ward.RecoveryExported, ExportDir: released.Dir, Manifest: released.Manifest,
					Evidence: released.Evidence, EvidencePresent: true,
					Workspace: ward.WorkspaceObservation{ObservedBaseSHA: released.ObservedBaseSHA},
				}, nil
			}}
			orphanWithSpec(t, d, phaseRunning, nil, spec)
			reconcileClean(t, context.Background(), d)
		},
		"exported recovery": func(t *testing.T, d *Driver, spec exec.StartSpec, released *releasedExport) {
			orphanWithSpec(t, d, phaseExported, released, spec)
			reconcileClean(t, context.Background(), d)
		},
	}
	for _, refusal := range refusals {
		for pathName, drive := range paths {
			t.Run(refusal.name+"/"+pathName, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				repo, baseSHA := emptyBaseRepo(t)
				outDir, manifest, evidence := releasedEvidenceExport(t, refusal.source, refusal.body, changed)
				exports := newStubExports()
				d := newTestDriver(t, &stubGate{}, exports)
				d.artifacts = newStubArtifacts()
				d.seeder = recoveryGitSeeder{repo: repo}
				spec := testStartSpec()
				spec.Base.BaseSHA = baseSHA
				drive(t, d, spec, &releasedExport{
					Dir: outDir, Manifest: manifest, Evidence: evidence, EvidencePresent: true,
					ObservedBaseSHA: baseSHA,
				})
				result, err := d.Collect(ctx, testInvoke)
				if err != nil || result.Status != exec.StatusFailed {
					t.Fatalf("result = %+v, %v; want failed", result, err)
				}
				outcome, found, err := exports.LookupExecutionOutcome(ctx, testInvoke)
				if err != nil || !found || outcome.Cause != refusal.want {
					t.Fatalf("outcome = (%+v, %t, %v), want cause %q", outcome, found, err, refusal.want)
				}
				if refusal.want == domain.FailureCauseInvalidEvidence &&
					!strings.Contains(outcome.Summary, export.ReviewGuideEvidenceLabel) {
					t.Fatalf("summary %q does not name the refused entry", outcome.Summary)
				}
			})
		}
	}
}
