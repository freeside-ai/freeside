package stage

import (
	"bytes"
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

func TestProductionReplayBytesOutliveTheReleasedDirectory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	artifacts := newStubArtifacts()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	d.artifacts = artifacts
	dir := t.TempDir()
	body := []byte("candidate bytes\n")
	sum := sha256.Sum256(body)
	hexDigits := hex.EncodeToString(sum[:])
	digest := domain.Digest("sha256:" + hexDigits)
	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, hexDigits), body, 0o600); err != nil {
		t.Fatal(err)
	}
	mode := "100644"
	size := int64(len(body))
	exportDigest := export.Digest(digest)
	out := exportOutcome{
		dir: dir,
		manifest: export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{{
			Path: "README.md", Kind: export.EntryRegular,
			Mode: &mode, Size: &size, Digest: &exportDigest,
		}}},
		commitPlanPresent: true,
	}
	plan := []byte(`{"commits":[{"message":"Keep replay exact","paths":["README.md"]}]}`)
	if err := os.WriteFile(filepath.Join(dir, export.CommitPlanFilename), plan, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.persistRepositoryBlobs(ctx, out); err != nil {
		t.Fatalf("persistRepositoryBlobs: %v", err)
	}
	planDigest, err := d.persistCommitPlan(ctx, out)
	if err != nil {
		t.Fatalf("persistCommitPlan: %v", err)
	}
	if planDigest == nil {
		t.Fatal("commit plan digest was not retained")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if got := artifacts.blobs[digest]; !bytes.Equal(got, body) {
		t.Fatalf("stored repository blob = %q", got)
	}
	if got := artifacts.blobs[*planDigest]; !bytes.Equal(got, plan) {
		t.Fatalf("stored commit plan = %q", got)
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
	if _, _, err := d.persistReleasedMaterial(ctx, intent{InvocationID: testInvoke}, out, record, nil, true); err == nil {
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
		RunID:        testRunIDFor(testInvoke),
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
		if runID != testRunIDFor(testInvoke) || exportDir == "" {
			t.Fatalf("authenticate args = %q/%q", runID, exportDir)
		}
		return refused
	}}
	d := newTestDriver(t, gate, newStubExports())
	dir, err := os.MkdirTemp("", "freeside-handoff-"+testRunIDFor(testInvoke)+"-out-")
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
		RunID:        testRunIDFor(testInvoke),
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

// TestPartitionFindingsByProfile pins the dispatch: under the default
// publish-strict profile every finding is fatal, so a completed candidate can
// carry none; under the specification profile the workspace-debris classes are
// tolerated while a secret stays fatal.
func TestPartitionFindingsByProfile(t *testing.T) {
	t.Parallel()
	findings := []importer.Finding{
		{Kind: importer.FindingAllowlistViolation, Path: "dist/bundle.js"},
		{Kind: importer.FindingSizeViolation, Path: "dist/big.bin"},
		{Kind: importer.FindingSecret, Path: "config.yaml"},
	}
	// A nil profile is the default publish-strict behavior: every finding fatal.
	fatalStrict, tolStrict := partitionFindings(findings, nil)
	if len(fatalStrict) != 3 || len(tolStrict) != 0 {
		t.Fatalf("publish-strict partition = %d fatal, %d tolerated; want 3, 0",
			len(fatalStrict), len(tolStrict))
	}
	spec := importer.FindingProfileSpecification
	fatalSpec, tolSpec := partitionFindings(findings, &spec)
	if len(fatalSpec) != 1 || fatalSpec[0].Kind != importer.FindingSecret {
		t.Fatalf("specification fatal = %+v, want only the secret", fatalSpec)
	}
	if len(tolSpec) != 2 {
		t.Fatalf("specification tolerated = %d, want 2 debris findings", len(tolSpec))
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
	in := intent{RunID: testRunIDFor(testInvoke)}
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
			RunID:        testRunIDFor(testInvoke),
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

// rejectionIntent is the minimal intent validateImportFindings needs: the
// invocation, the admission id it records the rejection under, and the pinned
// instant that makes a replay converge.
func rejectionIntent() intent {
	return intent{InvocationID: testInvoke, Spec: testStartSpec(), RecordedAt: fixedNow}
}

func specProfile() *importer.FindingProfile {
	p := importer.FindingProfileSpecification
	return &p
}

// TestSpecificationProfileToleratesWorkspaceDebris is the acceptance case: an
// elaboration import whose only findings are ignored-file debris, with a valid
// candidate commit, concludes successfully and records no rejection.
func TestSpecificationProfileToleratesWorkspaceDebris(t *testing.T) {
	t.Parallel()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	imported := importer.Result{
		CommitSHA: strings.Repeat("c", 40),
		Findings: []importer.Finding{
			{Kind: importer.FindingAllowlistViolation, Path: "dist/bundle.js"},
			{Kind: importer.FindingSizeViolation, Path: "dist/big.bin"},
			{Kind: importer.FindingSecretScanSkipped, Path: "dist/big.bin"},
		},
	}
	if err := d.validateImportFindings(rejectionIntent(), imported, specProfile()); err != nil {
		t.Fatalf("specification profile rejected pure debris: %v", err)
	}
	if _, found, err := exports.LookupExportRejection(context.Background(), testInvoke); err != nil || found {
		t.Fatalf("tolerated debris recorded a rejection (found=%t, err=%v)", found, err)
	}
}

// TestValidateImportFindingsRejectsWithoutWriting proves the reframe: a fatal
// finding returns a definitive rejection carrying the diagnostic sample and a
// count-only summary, and persists nothing — the ExportRejection lands later,
// beside the committed outcome, so this boundary cannot fail on a store write.
func TestValidateImportFindingsRejectsWithoutWriting(t *testing.T) {
	t.Parallel()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	imported := importer.Result{
		CommitSHA: strings.Repeat("c", 40),
		Findings: []importer.Finding{
			{Kind: importer.FindingAllowlistViolation, Path: "dist/bundle.js"},
			{Kind: importer.FindingSecret, Path: "config.yaml", Rule: "aws_key", Line: 12},
		},
	}
	err := d.validateImportFindings(rejectionIntent(), imported, specProfile())
	if !errors.Is(err, errDefinitiveExportRejection) {
		t.Fatalf("secret under specification = %v, want definitive rejection", err)
	}
	// The summary flows to the client-visible AttentionItem.Reason, so it is
	// count-only: per-finding paths never reach the synced surface.
	if strings.Contains(err.Error(), "config.yaml") {
		t.Fatalf("rejection summary %q leaked a finding path onto the synced surface", err.Error())
	}
	// One fatal secret and one tolerated debris finding: the summary must name
	// the fatal count, not label the debris publish-blocking.
	if !strings.Contains(err.Error(), "1 publish-blocking of 2 findings") {
		t.Fatalf("rejection summary %q must report 1 blocking of 2 reported", err.Error())
	}
	// Nothing is persisted here — the write is deferred to the commit path.
	if _, found, _ := exports.LookupExportRejection(context.Background(), testInvoke); found {
		t.Fatal("validateImportFindings wrote the rejection before the outcome committed")
	}
	// The typed error carries the sample for that later write.
	var rej *definitiveRejection
	if !errors.As(err, &rej) {
		t.Fatal("rejection does not carry the diagnostic sample")
	}
	// Fatal-first ordering (C2): the secret leads the carried sample.
	if rej.total != 2 || len(rej.findings) != 2 || rej.findings[0].Path != "config.yaml" {
		t.Fatalf("carried rejection total=%d findings=%+v, want total 2 with the secret first",
			rej.total, rej.findings)
	}
}

// TestRecordRejectionDetailPersistsSample proves the deferred, best-effort
// write records the sample and true total, retrievable after the released
// directory is cleaned.
func TestRecordRejectionDetailPersistsSample(t *testing.T) {
	t.Parallel()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	rej := newDefinitiveRejection(
		[]importer.Finding{{Kind: importer.FindingSecret, Path: "config.yaml", Rule: "aws_key", Line: 12}},
		[]importer.Finding{{Kind: importer.FindingAllowlistViolation, Path: "dist/bundle.js"}},
	)
	d.recordRejectionDetail(context.Background(), rejectionIntent(), rej)
	rejection, found, err := exports.LookupExportRejection(context.Background(), testInvoke)
	if err != nil || !found {
		t.Fatalf("rejection not persisted (found=%t, err=%v)", found, err)
	}
	if rejection.TotalFindings != 2 || len(rejection.Findings) != 2 {
		t.Fatalf("persisted %+v, want 2 findings / total 2", rejection)
	}
	secret := rejection.Findings[0]
	if secret.Kind != "secret" || secret.Path != "config.yaml" || secret.Rule != "aws_key" || secret.Line != 12 {
		t.Fatalf("fatal detail not preserved: %+v", secret)
	}
}

// TestRecordRejectionDetailIsBestEffort proves a store failure on the
// diagnostic write is swallowed, never propagated: it cannot fail the terminal
// the outcome already recorded.
func TestRecordRejectionDetailIsBestEffort(t *testing.T) {
	t.Parallel()
	exports := newStubExports()
	exports.rejectErr = errors.New("rejection store unavailable")
	d := newTestDriver(t, &stubGate{}, exports)
	rej := newDefinitiveRejection([]importer.Finding{{Kind: importer.FindingSecret, Path: "config.yaml"}}, nil)
	d.recordRejectionDetail(context.Background(), rejectionIntent(), rej) // must not panic or block
	if _, found, _ := exports.LookupExportRejection(context.Background(), testInvoke); found {
		t.Fatal("a failed best-effort write must persist nothing")
	}
}

// TestPublishStrictRejectsAnyFinding pins the default profile: every finding is
// fatal, so a candidate carrying even ignored-file debris is definitively
// rejected, exactly as before the profile existed.
func TestPublishStrictRejectsAnyFinding(t *testing.T) {
	t.Parallel()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	imported := importer.Result{
		CommitSHA: strings.Repeat("c", 40),
		Findings:  []importer.Finding{{Kind: importer.FindingAllowlistViolation, Path: "outside-scope.txt"}},
	}
	if err := d.validateImportFindings(rejectionIntent(), imported, nil); !errors.Is(err, errDefinitiveExportRejection) {
		t.Fatalf("publish-strict finding = %v, want definitive rejection", err)
	}
}

// TestCommitRejectionWritesOutcomeThenDetail proves the terminal-commit path
// records the authoritative failed outcome (count-only summary) and the
// diagnostic rejection beside it.
func TestCommitRejectionWritesOutcomeThenDetail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	in := orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+testRunIDFor(testInvoke)+"-out-gone-commit"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})
	rej := newDefinitiveRejection([]importer.Finding{{Kind: importer.FindingSecret, Path: "config.yaml"}}, nil)
	if err := d.commitRejection(ctx, d.logger, in, exec.StatusFailed, rej); err != nil {
		t.Fatalf("commitRejection: %v", err)
	}

	outcome, ok := exports.outcomes[testInvoke]
	if !ok || outcome.Status != domain.ExecutionOutcomeFailed {
		t.Fatalf("outcome = %+v (present=%t), want failed", outcome, ok)
	}
	if strings.Contains(outcome.Summary, "config.yaml") ||
		!strings.Contains(outcome.Summary, "publish-blocking findings") {
		t.Fatalf("outcome summary %q must be count-only", outcome.Summary)
	}
	if _, found, _ := exports.LookupExportRejection(ctx, testInvoke); !found {
		t.Fatal("commitRejection did not record the diagnostic detail")
	}
}

// TestCommitRejectionOutcomeSurvivesDetailFailure proves the terminal is
// committed even when the best-effort diagnostic write fails: the outcome is
// the authority, the detail is expendable.
func TestCommitRejectionOutcomeSurvivesDetailFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	exports.rejectErr = errors.New("rejection store unavailable")
	d := newTestDriver(t, &stubGate{}, exports)
	in := orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+testRunIDFor(testInvoke)+"-out-gone-detailfail"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})
	rej := newDefinitiveRejection([]importer.Finding{{Kind: importer.FindingSecret, Path: "config.yaml"}}, nil)
	if err := d.commitRejection(ctx, d.logger, in, exec.StatusFailed, rej); err != nil {
		t.Fatalf("commitRejection: %v", err)
	}

	if outcome, ok := exports.outcomes[testInvoke]; !ok || outcome.Status != domain.ExecutionOutcomeFailed {
		t.Fatalf("outcome = %+v (present=%t), want failed despite the detail-write failure", outcome, ok)
	}
	if _, found, _ := exports.LookupExportRejection(ctx, testInvoke); found {
		t.Fatal("a failed best-effort detail write must persist nothing")
	}
}

// TestCommitRejectionDetailSurvivesCanceledRunContext covers the shutdown race:
// Close/Cancel cancels the run context, runPipeline still commits a canceled
// outcome, and the diagnostic detail must still be recorded. A real store
// begins its write under the caller's context, so passing the canceled run
// context would drop the record on every shutdown; commitRejection writes it on
// a context detached from that cancellation.
func TestCommitRejectionDetailSurvivesCanceledRunContext(t *testing.T) {
	t.Parallel()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	in := orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+testRunIDFor(testInvoke)+"-out-gone-cancelctx"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})
	runCtx, cancel := context.WithCancel(context.Background())
	cancel() // the run context is already canceled (daemon shutdown)
	rej := newDefinitiveRejection([]importer.Finding{{Kind: importer.FindingSecret, Path: "config.yaml"}}, nil)

	if err := d.commitRejection(runCtx, d.logger, in, exec.StatusCanceled, rej); err != nil {
		t.Fatalf("commitRejection: %v", err)
	}

	if outcome, ok := exports.outcomes[testInvoke]; !ok || outcome.Status != domain.ExecutionOutcomeCanceled {
		t.Fatalf("outcome = %+v (present=%t), want canceled", outcome, ok)
	}
	if _, found, err := exports.LookupExportRejection(context.Background(), testInvoke); err != nil || !found {
		t.Fatalf("diagnostic detail lost under a canceled run context (found=%t, err=%v)", found, err)
	}
}

// TestRecoverExportedAdoptsWrittenFailedOutcome is the reframe's recovery
// contract: a failed outcome written before its phase commit (a definitive
// rejection commits one) is adopted on recovery, not resynthesized, so a second
// Reconcile converges instead of conflicting. Under the reframe a rejection
// never exists without its outcome, so recovery consults only the outcome.
func TestRecoverExportedAdoptsWrittenFailedOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)

	if err := exports.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
		InvocationID: testInvoke, AdmissionID: testStartSpec().AdmissionID,
		Status:     domain.ExecutionOutcomeFailed,
		Summary:    "released export was definitively rejected: gauntlet containment reported 3 publish-blocking findings",
		RecordedAt: fixedNow,
	}); err != nil {
		t.Fatalf("seed prior failed outcome: %v", err)
	}
	orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+testRunIDFor(testInvoke)+"-out-gone-adopt"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})

	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile must adopt the written outcome, got: %v", err)
	}
	if got := exports.outcomes[testInvoke].Status; got != domain.ExecutionOutcomeFailed {
		t.Fatalf("recovered outcome = %q, want failed (adopted)", got)
	}
	// Idempotent: a second pass converges on the same write-once outcome.
	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
}

// TestRecoverExportedHonorsCanceledOutcome covers the shutdown race: a canceled
// outcome written before its phase commit is adopted as canceled, not
// resynthesized as failed, or recordOrConvergeOutcome conflicts and the intent
// retries forever.
func TestRecoverExportedHonorsCanceledOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)

	if err := exports.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
		InvocationID: testInvoke, AdmissionID: testStartSpec().AdmissionID,
		Status:     domain.ExecutionOutcomeCanceled,
		Summary:    "Test invocation canceled by daemon request.",
		RecordedAt: fixedNow,
	}); err != nil {
		t.Fatalf("seed prior canceled outcome: %v", err)
	}
	orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+testRunIDFor(testInvoke)+"-out-gone-canceled"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})

	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile must adopt the canceled outcome, got: %v", err)
	}
	if got := exports.outcomes[testInvoke].Status; got != domain.ExecutionOutcomeCanceled {
		t.Fatalf("recovered outcome = %q, want canceled (adopted, not resynthesized as failed)", got)
	}
}

// TestNewDefinitiveRejectionPrioritizesFatalAndBounds proves two things: an
// adversarial flood of tolerated debris cannot bloat the permanent row (the
// sample is capped at maxPersistedRejectionFindings, with the true total kept
// separately), and the one fatal cause is never crowded out of that capped
// sample by the debris — it leads (C2).
func TestNewDefinitiveRejectionPrioritizesFatalAndBounds(t *testing.T) {
	t.Parallel()
	tolerated := make([]importer.Finding, maxPersistedRejectionFindings+50)
	for i := range tolerated {
		tolerated[i] = importer.Finding{Kind: importer.FindingAllowlistViolation, Path: fmt.Sprintf("dist/f%d.js", i)}
	}
	fatal := []importer.Finding{{Kind: importer.FindingSecret, Path: "config.yaml", Rule: "aws_key", Line: 12}}

	rej := newDefinitiveRejection(fatal, tolerated)
	if rej.total != len(fatal)+len(tolerated) {
		t.Fatalf("total = %d, want %d", rej.total, len(fatal)+len(tolerated))
	}
	if len(rej.findings) != maxPersistedRejectionFindings {
		t.Fatalf("sample size = %d, want cap %d", len(rej.findings), maxPersistedRejectionFindings)
	}
	if rej.findings[0].Kind != "secret" || rej.findings[0].Path != "config.yaml" {
		t.Fatalf("sample[0] = %+v, want the fatal cause first, not crowded out by debris", rej.findings[0])
	}
}

func TestPersistsRepositoryChannel(t *testing.T) {
	t.Parallel()
	strict := importer.FindingProfilePublishStrict
	spec := importer.FindingProfileSpecification
	if !persistsRepositoryChannel(nil) {
		t.Error("nil (default publish-strict) must persist the repo channel")
	}
	if !persistsRepositoryChannel(&strict) {
		t.Error("publish-strict must persist the repo channel")
	}
	if persistsRepositoryChannel(&spec) {
		t.Error("specification (elaboration) must skip the repo channel")
	}
}

// TestElaborationSkipsRepositoryBlobPersistence proves the security fix: under
// the elaboration profile the repo-channel blobs (which may hold unscanned
// tolerated content) never enter the durable CAS, while the manifest bytes
// still do. Under a publishing profile the same blobs are persisted for replay.
func TestElaborationSkipsRepositoryBlobPersistence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	body := []byte("oversized unscanned debris\n")
	sum := sha256.Sum256(body)
	hexDigits := hex.EncodeToString(sum[:])
	entryDigest := domain.Digest("sha256:" + hexDigits)
	mode := "0644"
	size := int64(len(body))
	exportDigest := export.Digest(entryDigest)
	manifest := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{{
		Path: "dist/bundle.js", Kind: export.EntryRegular,
		Mode: &mode, Size: &size, Digest: &exportDigest,
	}}}
	manifestBody, err := manifest.Encode()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: testInvoke, AdmissionID: testStartSpec().AdmissionID,
		ObservedBaseSHA: testBase.BaseSHA, HeadSHA: strings.Repeat("c", 40),
		ManifestDigest: digestOf(manifestBody), RecordedAt: fixedNow,
	})
	if err != nil {
		t.Fatalf("new execution export: %v", err)
	}

	setup := func() (*Driver, *stubArtifacts, exportOutcome) {
		artifacts := newStubArtifacts()
		d := newTestDriver(t, &stubGate{}, newStubExports())
		d.artifacts = artifacts
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), manifestBody, 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		blobDir := filepath.Join(dir, "blobs", "sha256")
		if err := os.MkdirAll(blobDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(blobDir, hexDigits), body, 0o600); err != nil {
			t.Fatal(err)
		}
		return d, artifacts, exportOutcome{dir: dir, manifest: manifest}
	}

	// Elaboration: repo blob skipped, manifest still stored.
	d, artifacts, out := setup()
	if _, _, err := d.persistReleasedMaterial(ctx, intent{InvocationID: testInvoke}, out, record, nil, false); err != nil {
		t.Fatalf("persistReleasedMaterial(skip): %v", err)
	}
	if _, ok := artifacts.blobs[entryDigest]; ok {
		t.Fatal("elaboration persisted a repo-channel blob into the CAS")
	}
	if _, ok := artifacts.blobs[record.ManifestDigest]; !ok {
		t.Fatal("manifest bytes were not persisted")
	}

	// Publishing profile: repo blob persisted for replay.
	d, artifacts, out = setup()
	if _, _, err := d.persistReleasedMaterial(ctx, intent{InvocationID: testInvoke}, out, record, nil, true); err != nil {
		t.Fatalf("persistReleasedMaterial(persist): %v", err)
	}
	if _, ok := artifacts.blobs[entryDigest]; !ok {
		t.Fatal("publishing profile did not persist the repo-channel blob")
	}
}
