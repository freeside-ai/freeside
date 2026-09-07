package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

func TestScopeConflictReleasedPipeline(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"valid pretty JSON", "blocked", "specification", "malformed", "in scope", "noncanonical", "edited conflict", "allowlist violation"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			conflict := domain.ScopeConflict{Version: domain.ScopeConflictEncodingVersion, Paths: []string{"AGENTS.md"}, Decision: blockedDecisions()[0]}
			if name == "noncanonical" {
				conflict.Paths[0] = "docs/../AGENTS.md"
			}
			body, err := json.MarshalIndent(conflict, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if name == "malformed" {
				body = []byte(`{"version":1}`)
			}
			workspace := t.TempDir()
			if err := os.MkdirAll(filepath.Join(workspace, export.EvidenceWorkspaceDir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, export.ScopeConflictEvidencePath), body, 0o600); err != nil {
				t.Fatal(err)
			}
			sources := []export.EvidenceSource{{
				Label: export.ScopeConflictEvidenceLabel, MediaType: "application/json", Path: export.ScopeConflictEvidencePath,
				HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivityNormal, ProducerInvocationID: string(testInvoke),
			}}
			if name == "blocked" {
				blocked, err := domain.EncodeBlockedOutcome(domain.BlockedOutcome{Version: domain.BlockedOutcomeEncodingVersion, Kind: domain.BlockedKindScopeExpansion, Decisions: blockedDecisions()})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(workspace, export.BlockedEvidencePath), blocked, 0o600); err != nil {
					t.Fatal(err)
				}
				sources = append(sources, export.EvidenceSource{
					Label: export.BlockedEvidenceLabel, MediaType: "application/json", Path: export.BlockedEvidencePath,
					HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivityNormal, ProducerInvocationID: string(testInvoke),
				})
			}
			descriptor, err := json.Marshal(export.EvidenceSourceManifest{Version: export.EvidenceSourceVersion, Sources: sources})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, export.EvidenceDescriptorPath), descriptor, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, "source.ts"), []byte("const renamed = true;\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if name == "edited conflict" || name == "allowlist violation" {
				if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("renamed helper\n"), 0o600); err != nil {
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
				t.Fatal(err)
			}
			evidenceBytes, err := os.ReadFile(filepath.Join(outDir, export.EvidenceFilename)) //nolint:gosec // test-owned handoff directory and fixed evidence filename
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := export.DecodeEvidenceManifest(evidenceBytes)
			if err != nil {
				t.Fatal(err)
			}
			repo, base := emptyBaseRepo(t)
			records := newStubExports()
			artifacts := newStubArtifacts()
			d := newTestDriver(t, &stubGate{}, records)
			d.artifacts = artifacts
			d.seeder = recoveryGitSeeder{repo: repo}
			d.imports.Policy.Allowlist = []string{"*.ts"}
			if name == "in scope" || name == "edited conflict" {
				d.imports.Policy.Allowlist = []string{"*.ts", "AGENTS.md"}
			}
			if name == "specification" {
				profile := importer.FindingProfileSpecification
				d.imports.Policy.FindingProfile = &profile
			}
			spec := testStartSpec()
			spec.Base.BaseSHA = base
			in := orphanWithSpec(t, d, phaseExported, &releasedExport{Dir: outDir, Manifest: manifest, Evidence: evidence, EvidencePresent: true, ObservedBaseSHA: base}, spec)
			if err := d.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			result, err := d.Collect(context.Background(), in.InvocationID)
			if err != nil {
				t.Fatal(err)
			}
			if name != "valid pretty JSON" {
				if result.Status != exec.StatusFailed || len(records.records) != 0 {
					t.Fatalf("invalid conflict accepted: %+v", result)
				}
				return
			}
			if result.Status != exec.StatusCompleted || result.HeadSHA == "" {
				t.Fatalf("valid conflict rejected: %+v", result)
			}
			canonical, err := domain.EncodeScopeConflict(conflict)
			if err != nil {
				t.Fatal(err)
			}
			claims := artifacts.claims[in.InvocationID]
			if len(claims) != 1 || !bytes.Equal(artifacts.blobs[claims[0].Digest], canonical) {
				t.Fatal("question claim is not canonical")
			}
			if len(result.Artifacts) != 1 || !bytes.Equal(artifacts.blobs[result.Artifacts[0]], body) {
				t.Fatal("released replay bytes were lost")
			}
		})
	}
}
