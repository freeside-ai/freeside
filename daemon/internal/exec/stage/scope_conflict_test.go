package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
			reconcileClean(t, context.Background(), d)
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

// TestScopeConflictBesideOmittedReviewGuide pins the scope-conflict rewrite
// to the returned artifact list. The omitted review guide sorts just before
// the scope conflict and returns no artifact, so a rewrite indexed by
// manifest position would panic past the list's end (no summary) or
// overwrite the summary's slot, and the recovered list would then refuse
// the completed intent on restart.
func TestScopeConflictBesideOmittedReviewGuide(t *testing.T) {
	t.Parallel()
	for _, withSummary := range []bool{false, true} {
		name := "no summary"
		if withSummary {
			name = "summary"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			conflict := domain.ScopeConflict{Version: domain.ScopeConflictEncodingVersion, Paths: []string{"AGENTS.md"}, Decision: blockedDecisions()[0]}
			conflictBody, err := json.MarshalIndent(conflict, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{
				export.ScopeConflictEvidencePath: conflictBody,
				export.ReviewGuideEvidencePath: []byte(`{"stops":[{"title":"t","summary":"s","attention":"risk","excerpts":[]}],"questions":[],"notes":null` +
					strings.Repeat(" ", importer.MaxReviewGuideBytes) + "}"),
				"source.ts": []byte("const renamed = true;\n"),
			}
			sources := []export.EvidenceSource{{
				Label: export.ScopeConflictEvidenceLabel, MediaType: "application/json", Path: export.ScopeConflictEvidencePath,
				HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivityNormal, ProducerInvocationID: string(testInvoke),
			}, {
				Label: export.ReviewGuideEvidenceLabel, MediaType: "application/json", Path: export.ReviewGuideEvidencePath,
				HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivitySensitive, ProducerInvocationID: string(testInvoke),
			}}
			if withSummary {
				files[export.SummaryEvidencePath] = []byte("Renamed the helper.\n")
				sources = append(sources, export.EvidenceSource{
					Label: export.SummaryEvidenceLabel, MediaType: "text/markdown", Path: export.SummaryEvidencePath,
					HeadBinding: export.EvidenceHeadIndependent, SensitivityClass: export.EvidenceSensitivityNormal, ProducerInvocationID: string(testInvoke),
				})
			}
			descriptor, err := json.Marshal(export.EvidenceSourceManifest{Version: export.EvidenceSourceVersion, Sources: sources})
			if err != nil {
				t.Fatal(err)
			}
			files[export.EvidenceDescriptorPath] = descriptor
			workspace := t.TempDir()
			if err := os.MkdirAll(filepath.Join(workspace, export.EvidenceWorkspaceDir), 0o700); err != nil {
				t.Fatal(err)
			}
			for path, body := range files {
				if err := os.WriteFile(filepath.Join(workspace, filepath.FromSlash(path)), body, 0o600); err != nil {
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
			var want []domain.Digest
			var guide domain.Digest
			for _, entry := range evidence.Entries {
				if entry.Label == export.ReviewGuideEvidenceLabel {
					guide = domain.Digest(entry.Digest)
					continue
				}
				want = append(want, domain.Digest(entry.Digest))
			}
			repo, base := emptyBaseRepo(t)
			artifacts := newStubArtifacts()
			d := newTestDriver(t, &stubGate{}, newStubExports())
			d.artifacts = artifacts
			d.seeder = recoveryGitSeeder{repo: repo}
			d.imports.Policy.Allowlist = []string{"*.ts"}
			spec := testStartSpec()
			spec.Base.BaseSHA = base
			in := orphanWithSpec(t, d, phaseExported, &releasedExport{Dir: outDir, Manifest: manifest, Evidence: evidence, EvidencePresent: true, ObservedBaseSHA: base}, spec)
			reconcileClean(t, ctx, d)
			// A second pass re-checks the completed intent against the
			// durable export, as a restart does.
			reconcileClean(t, ctx, d)
			result, err := d.Collect(ctx, in.InvocationID)
			if err != nil || result.Status != exec.StatusCompleted {
				t.Fatalf("result = %+v, %v; want completed", result, err)
			}
			if !slices.Equal(result.Artifacts, want) {
				t.Fatalf("artifacts = %v, want the raw conflict and summary %v", result.Artifacts, want)
			}
			if _, stored := artifacts.blobs[guide]; !stored {
				t.Fatalf("omitted guide %s is not stored for replay", guide)
			}
			if !bytes.Equal(artifacts.blobs[result.Artifacts[0]], conflictBody) {
				t.Fatal("released conflict bytes were lost")
			}
		})
	}
}
