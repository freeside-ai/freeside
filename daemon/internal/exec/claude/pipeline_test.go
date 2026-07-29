package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// writeEvidenceBlob stages one released evidence blob the way the exporter
// lays them out under the handoff directory.
func writeEvidenceBlob(t *testing.T, dir string, body []byte) domain.Digest {
	t.Helper()
	sum := sha256.Sum256(body)
	hexDigits := hex.EncodeToString(sum[:])
	blobDir := filepath.Join(dir, export.EvidenceBlobsDirname, "sha256")
	if err := os.MkdirAll(blobDir, 0o750); err != nil {
		t.Fatalf("stage evidence dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, hexDigits), body, 0o600); err != nil {
		t.Fatalf("stage evidence blob: %v", err)
	}
	return domain.Digest("sha256:" + hexDigits)
}

func TestTruncateSummaryPreservesUTF8WithinTheByteCap(t *testing.T) {
	t.Parallel()
	input := strings.Repeat("a", maxSummaryBytes-4) + "€-tail"
	got := truncateSummary(input)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated summary is not valid UTF-8: %q", got)
	}
	if len(got) > maxSummaryBytes {
		t.Fatalf("truncated summary is %d bytes, cap is %d", len(got), maxSummaryBytes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated summary %q has no ellipsis", got)
	}

	invalid := strings.Repeat("a", maxSummaryBytes) + string([]byte{0xff})
	if got := truncateSummary(invalid); !utf8.ValidString(got) || len(got) > maxSummaryBytes {
		t.Fatalf("normalized summary = %q (%d bytes), want valid UTF-8 within cap", got, len(got))
	}
}

// TestEvidenceIsPersistedBeforeTheExportIsRemoved is the regression for a
// result that named artifacts nothing could resolve: released evidence lives
// only under the gate's export directory, which the driver deletes, so the
// bytes must reach durable storage before the result names them. The agent
// transcript rides this same channel, so losing it loses the run's audit
// trail.
func TestEvidenceIsPersistedBeforeTheExportIsRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	artifacts := newStubArtifacts()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	d.artifacts = artifacts

	dir := t.TempDir()
	transcript := []byte(`{"type":"result","subtype":"success"}` + "\n")
	digest := writeEvidenceBlob(t, dir, transcript)
	out := exportOutcome{
		dir: dir, evidencePresent: true,
		evidence: export.EvidenceManifest{
			Version: export.EvidenceManifestVersion,
			Entries: []export.EvidenceEntry{{
				Label: "agent-transcript", MediaType: "application/jsonl",
				Size: int64(len(transcript)), Digest: export.Digest(digest),
			}},
		},
	}
	in := intent{InvocationID: testInvoke}

	digests, err := d.persistEvidence(ctx, in, out, nil)
	if err != nil {
		t.Fatalf("persistEvidence: %v", err)
	}
	if len(digests) != 1 || digests[0] != digest {
		t.Fatalf("returned digests = %v, want [%s]", digests, digest)
	}
	stored, ok := artifacts.blobs[digest]
	if !ok {
		t.Fatal("evidence blob was never stored; the result would name unresolvable content")
	}
	if string(stored) != string(transcript) {
		t.Fatalf("stored bytes = %q, want the released transcript", stored)
	}

	// Every digest the result names must now survive the export directory.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove export dir: %v", err)
	}
	if _, ok := artifacts.blobs[digest]; !ok {
		t.Fatal("evidence did not survive removal of the export directory")
	}
}

// TestEvidenceBlobMissingFromTheExportFailsTheStage: a named entry whose
// blob is absent must fail rather than yield a result naming content nothing
// can resolve.
func TestEvidenceBlobMissingFromTheExportFailsTheStage(t *testing.T) {
	t.Parallel()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	out := exportOutcome{
		dir: t.TempDir(), evidencePresent: true,
		evidence: export.EvidenceManifest{
			Version: export.EvidenceManifestVersion,
			Entries: []export.EvidenceEntry{{
				Label: "agent-transcript", MediaType: "application/jsonl", Size: 1,
				Digest: export.Digest("sha256:" + strings.Repeat("ab", 32)),
			}},
		},
	}
	if _, err := d.persistEvidence(context.Background(), intent{InvocationID: testInvoke}, out, nil); err == nil {
		t.Fatal("a missing evidence blob was accepted")
	}
}

func TestEvidenceFailureLeavesNoExecutionExport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	dir := t.TempDir()
	manifest := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}}
	manifestBody, err := manifest.Encode()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	evidence := export.EvidenceManifest{
		Version: export.EvidenceManifestVersion,
		Entries: []export.EvidenceEntry{{
			Label: "missing", MediaType: "text/plain", Size: 1,
			Digest: export.Digest("sha256:" + strings.Repeat("ab", 32)),
			Provenance: export.EvidenceProvenance{
				ProducerClass:        export.EvidenceProducerAgent,
				ProducerInvocationID: string(testInvoke),
				HeadBinding:          export.EvidenceHeadIndependent,
				SensitivityClass:     export.EvidenceSensitivityNormal,
			},
		}},
	}
	evidenceBody, err := evidence.Encode()
	if err != nil {
		t.Fatalf("encode evidence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), manifestBody, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, export.EvidenceFilename), evidenceBody, 0o600); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	manifestDigest := digestOf(manifestBody)
	evidenceDigest := digestOf(evidenceBody)
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID:           testInvoke,
		AdmissionID:            domain.Digest("sha256:" + strings.Repeat("44", 32)),
		ObservedBaseSHA:        testBase.BaseSHA,
		HeadSHA:                strings.Repeat("c", 40),
		ManifestDigest:         manifestDigest,
		EvidenceManifestDigest: &evidenceDigest,
		RecordedAt:             fixedNow,
	})
	if err != nil {
		t.Fatalf("new export: %v", err)
	}
	out := exportOutcome{dir: dir, manifest: manifest, evidence: evidence, evidencePresent: true}
	if _, err := d.persistReleasedExport(ctx, intent{InvocationID: testInvoke}, out, record, nil); err == nil {
		t.Fatal("missing evidence was accepted")
	}
	if len(exports.records) != 0 {
		t.Fatal("ExecutionExport was recorded before its evidence became durable")
	}
}

func TestRecoveryRefusesUnownedExportPathWithoutDeletingIt(t *testing.T) {
	t.Parallel()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	dir := t.TempDir()
	marker := filepath.Join(dir, "keep")
	if err := os.WriteFile(marker, []byte("owned by test"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	in := intent{
		InvocationID: testInvoke,
		RunID:        RunIDFor(testInvoke),
		Spec:         testStartSpec(),
		Export:       &releasedExport{Dir: dir},
	}
	if err := d.recoverExported(context.Background(), in); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("recover unowned path error = %v, want ErrUnsupportedStart", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("unowned path was modified: %v", err)
	}
}

func TestRecoveryRefusesSamePrefixExportNotIssuedByTheWard(t *testing.T) {
	t.Parallel()
	refused := errors.New("fixture: export directory not journal-bound")
	gate := &stubGate{authenticateFn: func(runID, exportDir string) error {
		if runID != RunIDFor(testInvoke) || exportDir == "" {
			t.Fatalf("authenticate args = %q/%q", runID, exportDir)
		}
		return refused
	}}
	d := newTestDriver(t, gate, newStubExports())
	dir, err := os.MkdirTemp("", "freeside-handoff-"+RunIDFor(testInvoke)+"-out-")
	if err != nil {
		t.Fatalf("create counterfeit export: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	marker := filepath.Join(dir, "keep")
	if err := os.WriteFile(marker, []byte("counterfeit"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	in := intent{
		InvocationID: testInvoke,
		RunID:        RunIDFor(testInvoke),
		Spec:         testStartSpec(),
		Export:       &releasedExport{Dir: dir},
	}
	if err := d.recoverExported(context.Background(), in); !errors.Is(err, refused) {
		t.Fatalf("recover counterfeit export = %v, want journal refusal", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("counterfeit export was modified: %v", err)
	}
}

func TestImportFindingsCannotBecomeACompletedCandidate(t *testing.T) {
	t.Parallel()
	result := importer.Result{
		CommitSHA: strings.Repeat("c", 40),
		Findings: []importer.Finding{{
			Kind: importer.FindingAllowlistViolation,
			Path: "outside-scope.txt",
		}},
	}
	if err := validateImportResult(result); err == nil {
		t.Fatal("a commit carrying a publish-blocking finding was accepted")
	}
}

func TestImportFailureClassificationDefaultsToRetryable(t *testing.T) {
	t.Parallel()
	definitive := []error{
		importer.ErrManifestInvalid,
		importer.ErrManifestTooLarge,
		importer.ErrEvidenceInvalid,
		importer.ErrEvidenceMediaMismatch,
		importer.ErrCommitPlanCollision,
		importer.ErrGitPathInjection,
		importer.ErrPathConflict,
		importer.ErrOrphanBlob,
		importer.ErrDigestMismatch,
		importer.ErrSizeMismatch,
		importer.ErrBlobTooLarge,
	}
	for _, err := range definitive {
		if !isDefinitiveImportRejection(fmt.Errorf("wrapped: %w", err)) {
			t.Errorf("%v was not classified as a definitive export rejection", err)
		}
	}
	operational := []error{
		importer.ErrManifestUnreadable,
		importer.ErrEvidenceUnreadable,
		importer.ErrCommitPlanUnreadable,
		importer.ErrHandoffUnreadable,
		importer.ErrMissingBlob,
		importer.ErrGitPlumbing,
		importer.ErrInvalidOptions,
		errors.New("ambiguous export-store failure"),
		context.Canceled,
	}
	for _, err := range operational {
		if isDefinitiveImportRejection(fmt.Errorf("wrapped: %w", err)) {
			t.Errorf("%v was classified as a definitive export rejection", err)
		}
	}
}

func TestAmbiguousExportCompletionPreservesReleasedDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(marker, []byte("released"), 0o600); err != nil {
		t.Fatalf("write released marker: %v", err)
	}
	ambiguous := errors.New("execution export write returned an ambiguous error")

	if _, err := classifyExportCompletion(dir, exec.StageResult{}, ambiguous); !errors.Is(err, ErrRecoveryRetryable) ||
		!errors.Is(err, ambiguous) {
		t.Fatalf("completion error = %v, want retryable ambiguous failure", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("released directory was not preserved: %v", err)
	}
}

func TestDefinitiveExportRejectionConsumesReleasedDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rejected := fmt.Errorf("%w: malformed manifest", errDefinitiveExportRejection)

	if _, err := classifyExportCompletion(dir, exec.StageResult{}, rejected); !errors.Is(err, rejected) {
		t.Fatalf("completion error = %v, want definitive rejection", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("definitively rejected directory still exists: %v", err)
	}
}

func TestDurableExportConflictPreservesReleasedDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(marker, []byte("released"), 0o600); err != nil {
		t.Fatalf("write released marker: %v", err)
	}
	conflict := fmt.Errorf("%w: stored head differs", errExportAuthorityConflict)

	if _, err := classifyExportCompletion(dir, exec.StageResult{}, conflict); !errors.Is(err, conflict) ||
		errors.Is(err, ErrRecoveryRetryable) {
		t.Fatalf("completion error = %v, want non-retryable authority conflict", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("conflicting released directory was not preserved: %v", err)
	}
}

func TestReleasedExportPathUsesConfiguredDurableRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	in := intent{RunID: RunIDFor(testInvoke)}
	valid := filepath.Join(root, "freeside-handoff-"+in.RunID+"-out-valid")
	if err := validateReleasedExportPath(root, in, valid); err != nil {
		t.Fatalf("configured durable export path: %v", err)
	}
	for _, path := range []string{
		filepath.Join(os.TempDir(), filepath.Base(valid)),
		filepath.Join(root, "freeside-handoff-"+in.RunID+"-out-"),
		filepath.Join(root, "freeside-handoff-foreign-out-valid"),
	} {
		if err := validateReleasedExportPath(root, in, path); err == nil {
			t.Errorf("accepted released export outside the exact run root: %q", path)
		}
	}
}

func TestReleasedExportBoundaryRejectsTamperedFields(t *testing.T) {
	t.Parallel()
	exportRoot := t.TempDir()
	makeExport := func(t *testing.T) (intent, exportOutcome) {
		t.Helper()
		in := intent{
			InvocationID: testInvoke,
			RunID:        RunIDFor(testInvoke),
			Spec:         testStartSpec(),
		}
		dir, err := os.MkdirTemp(exportRoot, "freeside-handoff-"+in.RunID+"-out-")
		if err != nil {
			t.Fatalf("create export: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		manifest := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}}
		body, err := manifest.Encode()
		if err != nil {
			t.Fatalf("encode manifest: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), body, 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		return in, exportOutcome{
			dir: dir, manifest: manifest,
			observedBaseSHA: in.Spec.Base.BaseSHA,
		}
	}
	tests := []struct {
		name   string
		mutate func(t *testing.T, out *exportOutcome)
	}{
		{"observed base", func(_ *testing.T, out *exportOutcome) {
			out.observedBaseSHA = strings.Repeat("b", 40)
		}},
		{"manifest value", func(_ *testing.T, out *exportOutcome) {
			out.manifest.Version = "foreign"
		}},
		{"manifest bytes", func(t *testing.T, out *exportOutcome) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(out.dir, export.ManifestFilename), []byte("{}"), 0o600); err != nil {
				t.Fatalf("tamper manifest: %v", err)
			}
		}},
		{"unreported evidence", func(t *testing.T, out *exportOutcome) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(out.dir, export.EvidenceFilename), []byte("{}"), 0o600); err != nil {
				t.Fatalf("stage hidden evidence: %v", err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in, out := makeExport(t)
			tc.mutate(t, &out)
			if err := validateReleasedExport(exportRoot, in, out); err == nil {
				t.Fatal("tampered gate return was accepted")
			}
		})
	}
}
