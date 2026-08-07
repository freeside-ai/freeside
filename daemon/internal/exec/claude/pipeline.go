package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// ErrRecoveryRetryable marks an operational recovery failure that preserved
// the durable preterminal intent. The daemon may start and let a later Inspect
// retry it; permanent reconstruction failures remain startup-fatal.
var ErrRecoveryRetryable = errors.New("claude driver recovery is retryable")

// errDefinitiveExportRejection marks a returned workspace that the trust
// boundary has conclusively rejected. Only this class may discard the
// released directory; every operational or ambiguous failure preserves the
// phaseExported replay state.
var errDefinitiveExportRejection = errors.New("released export was definitively rejected")

// errExportAuthorityConflict marks a durable ExecutionExport that disagrees
// with this invocation's released facts. It is neither retryable nor safe to
// terminalize: recording a failed outcome beside the existing export would
// create two contradictory authorities. Recovery preserves the evidence and
// fails loud for operator repair.
var errExportAuthorityConflict = errors.New("released export conflicts with durable authority")

// runPipeline drives one invocation from seeded workspace to committed
// result. Operational failures after a handoff has returned deliberately
// remain in phaseExported so Inspect can retry them without discarding work.
func (d *Driver) runPipeline(ctx context.Context, in intent) {
	log := d.logger.With("invocation", string(in.InvocationID), "run", in.RunID)
	log.Debug("pipeline started", "phase", string(in.Phase))
	result, err := d.handoffAndImport(ctx, in)
	if err == nil {
		// A failed terminal write is retained on the live session and retried
		// by Inspect/Collect; the asynchronous pipeline has no caller to
		// return it to, so this record is where the retained failure surfaces
		// before that retry rather than after it.
		d.commit(log, in.InvocationID, result)
		return
	}
	if errors.Is(err, errExportAuthorityConflict) {
		log.Error("released export conflicts with durable authority; preserved for repair",
			"error", err)
		return
	}
	if errors.Is(err, ErrRecoveryRetryable) {
		// The durable preterminal phase (or an in-process retry of its write)
		// owns this window. Inventing a failed result would discard a returned
		// export or skip recovery of a writer whose teardown is not yet proven.
		log.Debug("pipeline left the window to recovery", "error", err)
		return
	}
	// A failure here is this stage's outcome, not the daemon's: the engine
	// records it and raises the operator-visible execution_failure item. A
	// canceled context is reported as canceled so the two are distinguishable
	// in the durable record.
	status := exec.StatusFailed
	if ctx.Err() != nil && !errors.Is(err, ErrSeedRefused) {
		status = exec.StatusCanceled
	}
	if status == exec.StatusCanceled {
		// A stage the daemon's own shutdown canceled is a lifecycle event,
		// not a fault. Logging it at error would train the operator to skim
		// past the severity, which is the habit the rest of this costs
		// effort to avoid; the durable StatusCanceled remains the authority.
		log.Info("stage canceled", "error", err)
	} else {
		log.Error("stage failed", "error", err)
	}
	d.commit(log, in.InvocationID, exec.StageResult{
		InvocationID: in.InvocationID, Status: status,
		Summary: truncateSummary(err.Error()),
	})
}

// commit writes the terminal result and reports a write that failed. The
// failure is retained for Inspect/Collect either way; recording it is the
// difference between an operator seeing it now and inferring it later.
func (d *Driver) commit(log *slog.Logger, id domain.InvocationID, result exec.StageResult) {
	if err := d.commitResult(id, result); err != nil {
		log.Error("terminal result write failed; retained for retry",
			"status", string(result.Status), "error", err)
		return
	}
	log.Debug("terminal result committed", "status", string(result.Status))
}

// handoffAndImport seeds the exact base, runs the ward gate, imports the
// export into a candidate commit, and records the ExecutionExport before
// returning the collectable result.
func (d *Driver) handoffAndImport(ctx context.Context, in intent) (exec.StageResult, error) {
	if err := d.seedBase(ctx, in); err != nil {
		return exec.StageResult{}, err
	}
	hs, err := d.handoffSpec(ctx, in)
	if err != nil {
		// Start authenticated the same immutable spec before committing this
		// seeding intent. After the exact-base fetch, the only fallible state
		// lookup is the trusted auth-store binding. No journal or writer exists
		// yet, so cancellation or operational lookup failure must preserve the
		// rerunnable seeding phase rather than minting a terminal outcome.
		if !errors.Is(err, ErrUnsupportedStart) {
			return exec.StageResult{}, fmt.Errorf(
				"%w: rebuild pre-journal handoff: %w", ErrRecoveryRetryable, err)
		}
		return exec.StageResult{}, err
	}
	// The phase advances before the gate is called, because from here on a
	// journal record may exist and ward's own recovery is what may adopt it.
	if err := d.advance(&in, phaseRunning, nil); err != nil {
		return exec.StageResult{}, err
	}
	handoff, err := d.gate.Handoff(ctx, hs)
	if err != nil {
		return exec.StageResult{}, fmt.Errorf("ward handoff: %w", err)
	}
	out := exportOutcome{
		dir: handoff.ExportDir, manifest: handoff.Manifest,
		evidence: handoff.Evidence, evidencePresent: handoff.EvidencePresent,
		commitPlanPresent: handoff.CommitPlanPresent,
		observedBaseSHA:   handoff.Workspace.ObservedBaseSHA,
	}
	// Handoff has already closed its journal. Persist the complete return before
	// authentication, validation, or any other fallible work; phaseExported is
	// replay data, not trust, and every reconstruction re-authenticates it
	// before using or deleting the named directory.
	if err := d.advance(&in, phaseExported, releasedFrom(out)); err != nil {
		if retainErr := d.retainPendingIntent(in, err); retainErr != nil {
			return exec.StageResult{}, errors.Join(err, retainErr)
		}
		return exec.StageResult{}, fmt.Errorf("%w: persist returned handoff: %w",
			ErrRecoveryRetryable, err)
	}
	if err := d.authenticateReleasedExport(ctx, in, out.dir); err != nil {
		return exec.StageResult{}, classifyReleasedExportAuthentication(err)
	}
	return d.completeReleasedExport(ctx, in, out)
}

// advance moves one intent to its next phase durably. Every phase boundary
// that changes what recovery must do goes through here, so a crash lands on
// a record that names the window it is actually in.
func (d *Driver) advance(in *intent, next phase, released *releasedExport) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	in.Phase = next
	if released != nil {
		in.Export = released
	}
	if err := d.saveIntent(*in); err != nil {
		return fmt.Errorf("record %s phase: %w", next, err)
	}
	return nil
}

// exportOutcome is the gate's release, from either a fresh handoff or an
// adopted recovery: the two carry the same facts, and the rest of the
// pipeline must not care which produced them.
type exportOutcome struct {
	dir               string
	manifest          export.Manifest
	evidence          export.EvidenceManifest
	evidencePresent   bool
	commitPlanPresent bool
	observedBaseSHA   string
}

// finish imports the released export into a candidate commit and records the
// durable ExecutionExport before the result becomes collectable.
//
// Ordering is load-bearing: every blob and claim the export row implies lands
// first, then the row, then the collectable result. A crash may leave
// unreferenced content-addressed bytes, but can never leave a durable export
// asserting evidence that was not persisted.
func (d *Driver) finish(ctx context.Context, in intent, out exportOutcome) (exec.StageResult, error) {
	checkoutDir := filepath.Join(in.Seed+"-import", "checkout")
	if err := os.RemoveAll(filepath.Dir(checkoutDir)); err != nil {
		return exec.StageResult{}, fmt.Errorf("clear import checkout: %w", err)
	}
	if err := d.seeder.FetchBase(ctx,
		in.Spec.Base.Repo, in.Spec.Base.BaseRef, in.Spec.Base.BaseSHA, checkoutDir,
	); err != nil {
		return exec.StageResult{}, fmt.Errorf("fetch import base: %w", err)
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(checkoutDir)) }()

	opts, err := d.authority.ImportOptions(ctx, in.InvocationID, in.Spec, d.imports)
	if err != nil {
		return exec.StageResult{}, fmt.Errorf("derive import policy: %w", err)
	}
	opts.BaseSHA = in.Spec.Base.BaseSHA
	// Pinned at Start, so a replayed import after a crash reproduces the same
	// commit SHA and converges on the recorded export instead of minting a
	// second head for the same work.
	opts.CommitDate = in.CommitDate
	imported, err := importer.Import(ctx, out.dir, checkoutDir, opts)
	if err != nil {
		err = fmt.Errorf("gauntlet import: %w", err)
		if isDefinitiveImportRejection(err) {
			return exec.StageResult{}, fmt.Errorf("%w: %w",
				errDefinitiveExportRejection, err)
		}
		return exec.StageResult{}, err
	}
	if err := validateImportResult(imported); err != nil {
		return exec.StageResult{}, fmt.Errorf("%w: %w",
			errDefinitiveExportRejection, err)
	}

	manifestDigest, err := fileDigest(filepath.Join(out.dir, export.ManifestFilename))
	if err != nil {
		return exec.StageResult{}, err
	}
	var evidenceDigest *domain.Digest
	if out.evidencePresent {
		digest, err := fileDigest(filepath.Join(out.dir, export.EvidenceFilename))
		if err != nil {
			return exec.StageResult{}, err
		}
		evidenceDigest = &digest
	}
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID:           in.InvocationID,
		AdmissionID:            in.Spec.AdmissionID,
		ObservedBaseSHA:        out.observedBaseSHA,
		HeadSHA:                imported.CommitSHA,
		ManifestDigest:         manifestDigest,
		EvidenceManifestDigest: evidenceDigest,
		CommitPlanPresent:      out.commitPlanPresent,
		RecordedAt:             in.RecordedAt,
	})
	if err != nil {
		return exec.StageResult{}, err
	}
	// Every byte needed to reconstruct the exact candidate lands before the
	// immutable export row. A crash can leave unreferenced content-addressed
	// bytes, but never an ExecutionExport whose source material disappeared
	// with the released directory.
	artifacts, replay, err := d.persistReleasedMaterial(ctx, in, out, record, imported.Claims)
	if err != nil {
		return exec.StageResult{}, err
	}
	if err := d.recordExecutionReplay(in, replay); err != nil {
		return exec.StageResult{}, err
	}
	durableReplay := ExecutionReplay{
		InvocationID: in.InvocationID, ObservedBaseSHA: record.ObservedBaseSHA,
		HeadSHA: record.HeadSHA, Manifest: out.manifest,
		ManifestDigest: record.ManifestDigest, Evidence: out.evidence,
		EvidenceManifestDigest: cloneDigest(record.EvidenceManifestDigest),
		CommitPlanDigest:       cloneDigest(replay.CommitPlanDigest),
		ImportOptions:          opts,
	}
	if err := d.recordExport(ctx, record, durableReplay); err != nil {
		return exec.StageResult{}, err
	}
	return exec.StageResult{
		InvocationID: in.InvocationID, Status: exec.StatusCompleted,
		HeadSHA:   record.HeadSHA,
		Artifacts: artifacts,
		Summary: fmt.Sprintf("Imported candidate %s over base %s.",
			record.HeadSHA, record.ObservedBaseSHA),
	}, nil
}

// completeReleasedExport owns the cleanup decision after phaseExported. A
// successful import or a conclusive trust-boundary rejection consumes the
// directory. Operational and ambiguous failures retain it and advertise the
// retryable recovery class, including failures after a possibly successful
// ExecutionExport write.
func (d *Driver) completeReleasedExport(
	ctx context.Context, in intent, out exportOutcome,
) (exec.StageResult, error) {
	if err := validateReleasedExport(d.exportRoot, in, out); err != nil {
		return classifyExportCompletion(out.dir, exec.StageResult{}, err)
	}
	result, err := d.finish(ctx, in, out)
	return classifyExportCompletion(out.dir, result, err)
}

func classifyExportCompletion(
	dir string, result exec.StageResult, err error,
) (exec.StageResult, error) {
	if err == nil {
		removeReleasedExport(dir)
		return result, nil
	}
	if errors.Is(err, errExportAuthorityConflict) {
		return exec.StageResult{}, err
	}
	if errors.Is(err, errDefinitiveExportRejection) {
		removeReleasedExport(dir)
		return exec.StageResult{}, err
	}
	return exec.StageResult{}, fmt.Errorf("%w: finish released export: %w",
		ErrRecoveryRetryable, err)
}

// isDefinitiveImportRejection contains only candidate-controlled integrity
// failures. Read/process/configuration errors and unknown classes default to
// retryable because deleting a completed export on an ambiguous failure is
// irreversible. ErrMissingBlob is intentionally excluded because importer
// also wraps operating-system open failures in that sentinel.
func isDefinitiveImportRejection(err error) bool {
	return errors.Is(err, importer.ErrManifestInvalid) ||
		errors.Is(err, importer.ErrManifestTooLarge) ||
		errors.Is(err, importer.ErrEvidenceInvalid) ||
		errors.Is(err, importer.ErrEvidenceMediaMismatch) ||
		errors.Is(err, importer.ErrCommitPlanCollision) ||
		errors.Is(err, importer.ErrGitPathInjection) ||
		errors.Is(err, importer.ErrPathConflict) ||
		errors.Is(err, importer.ErrOrphanBlob) ||
		errors.Is(err, importer.ErrDigestMismatch) ||
		errors.Is(err, importer.ErrSizeMismatch) ||
		errors.Is(err, importer.ErrBlobTooLarge)
}

func validateImportResult(imported importer.Result) error {
	if len(imported.Findings) != 0 {
		return fmt.Errorf(
			"gauntlet containment reported %d publish-blocking findings",
			len(imported.Findings))
	}
	if imported.CommitSHA == "" {
		return fmt.Errorf("gauntlet containment withheld a candidate commit")
	}
	return nil
}

func (d *Driver) persistReleasedMaterial(
	ctx context.Context,
	in intent,
	out exportOutcome,
	record domain.ExecutionExport,
	claims []domain.AgentClaim,
) ([]domain.Digest, executionReplay, error) {
	if err := d.persistManifests(ctx, out, record); err != nil {
		return nil, executionReplay{}, err
	}
	if err := d.persistRepositoryBlobs(ctx, out); err != nil {
		return nil, executionReplay{}, err
	}
	// Evidence lands before the export record: the released blobs live only
	// under this directory, and a durable row is an assertion that every
	// object it implies can already be resolved.
	artifacts, err := d.persistEvidence(ctx, in, out, claims)
	if err != nil {
		return nil, executionReplay{}, err
	}
	planDigest, err := d.persistCommitPlan(ctx, out)
	if err != nil {
		return nil, executionReplay{}, err
	}
	return artifacts, executionReplay{CommitPlanDigest: planDigest}, nil
}

// persistRepositoryBlobs copies every regular repository blob named by the
// manifest into durable content-addressed storage. A successful importer
// cannot have accepted an omitted blob, but this boundary rejects one
// explicitly rather than depending on that earlier conclusion.
func (d *Driver) persistRepositoryBlobs(ctx context.Context, out exportOutcome) error {
	for _, entry := range out.manifest.Entries {
		if entry.Kind != export.EntryRegular {
			continue
		}
		if entry.BlobOmitted || entry.Digest == nil {
			return fmt.Errorf("persist repository blob for %q: manifest content is unavailable", entry.Path)
		}
		digest := domain.Digest(*entry.Digest)
		hexDigits, ok := contentaddr.Parse(string(digest))
		if !ok {
			return fmt.Errorf("persist repository blob for %q: digest %q is not canonical", entry.Path, digest)
		}
		body, err := readDigestedFile(filepath.Join(out.dir, "blobs", "sha256", hexDigits), digest)
		if err != nil {
			return fmt.Errorf("persist repository blob for %q: %w", entry.Path, err)
		}
		if err := d.artifacts.PutBlob(ctx, digest, body); err != nil {
			return fmt.Errorf("persist repository blob for %q: %w", entry.Path, err)
		}
	}
	return nil
}

func (d *Driver) persistCommitPlan(
	ctx context.Context, out exportOutcome,
) (*domain.Digest, error) {
	if !out.commitPlanPresent {
		return nil, nil
	}
	path := filepath.Join(out.dir, export.CommitPlanFilename)
	digest, err := fileDigest(path)
	if err != nil {
		return nil, err
	}
	body, err := readDigestedFile(path, digest)
	if err != nil {
		return nil, err
	}
	if err := d.artifacts.PutBlob(ctx, digest, body); err != nil {
		return nil, fmt.Errorf("persist commit plan: %w", err)
	}
	return &digest, nil
}

// recordExecutionReplay closes the durable-export/source-material crash
// window. It advances no lifecycle phase: it only enriches phaseExported with
// independently re-auditable replay data before the authoritative export row
// is allowed to commit.
func (d *Driver) recordExecutionReplay(in intent, replay executionReplay) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if in.Phase != phaseExported || in.Export == nil {
		return fmt.Errorf("record execution replay from phase %q: %w", in.Phase, ErrUnsupportedStart)
	}
	copyReplay := replay
	in.Export.Replay = &copyReplay
	if err := d.saveIntent(in); err != nil {
		return fmt.Errorf("record execution replay: %w", err)
	}
	return nil
}

// validateReleasedExport re-authenticates every field returned by the gate or
// decoded from private recovery state before the driver imports from or
// deletes the named directory.
func validateReleasedExport(exportRoot string, in intent, out exportOutcome) error {
	if err := validateReleasedExportPath(exportRoot, in, out.dir); err != nil {
		return fmt.Errorf("%w: %w", errDefinitiveExportRejection, err)
	}
	clean := filepath.Clean(out.dir)
	info, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("inspect released export: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: released export %q is not a plain directory: %w",
			errDefinitiveExportRejection, clean, ErrUnsupportedStart)
	}
	if out.observedBaseSHA != in.Spec.Base.BaseSHA {
		return fmt.Errorf("%w: released export observed base %q, admission names %q: %w",
			errDefinitiveExportRejection, out.observedBaseSHA,
			in.Spec.Base.BaseSHA, domain.ErrParentKeyMismatch)
	}
	manifest, err := out.manifest.Encode()
	if err != nil {
		return fmt.Errorf("%w: validate released manifest: %w",
			errDefinitiveExportRejection, err)
	}
	onDisk, err := os.ReadFile(filepath.Join(clean, export.ManifestFilename)) //nolint:gosec // run-owned export directory authenticated above
	if err != nil {
		return fmt.Errorf("read released %s: %w", export.ManifestFilename, err)
	}
	if !bytes.Equal(onDisk, manifest) {
		return fmt.Errorf("%w: released manifest disagrees with %s: %w",
			errDefinitiveExportRejection, export.ManifestFilename,
			domain.ErrParentKeyMismatch)
	}
	evidencePath := filepath.Join(clean, export.EvidenceFilename)
	if out.evidencePresent {
		evidence, err := out.evidence.Encode()
		if err != nil {
			return fmt.Errorf("%w: validate released evidence manifest: %w",
				errDefinitiveExportRejection, err)
		}
		onDisk, err := os.ReadFile(evidencePath) //nolint:gosec // run-owned export directory authenticated above
		if err != nil {
			return fmt.Errorf("read released %s: %w", export.EvidenceFilename, err)
		}
		if !bytes.Equal(onDisk, evidence) {
			return fmt.Errorf("%w: released evidence disagrees with %s: %w",
				errDefinitiveExportRejection, export.EvidenceFilename,
				domain.ErrParentKeyMismatch)
		}
	} else if _, err := os.Lstat(evidencePath); err == nil {
		return fmt.Errorf("%w: released export carries unreported %s: %w",
			errDefinitiveExportRejection, export.EvidenceFilename,
			domain.ErrParentKeyMismatch)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect released %s: %w", export.EvidenceFilename, err)
	}
	commitPlanPath := filepath.Join(clean, export.CommitPlanFilename)
	_, planErr := os.Lstat(commitPlanPath)
	if out.commitPlanPresent != (planErr == nil) {
		if planErr != nil && !errors.Is(planErr, os.ErrNotExist) {
			return fmt.Errorf("inspect released commit plan: %w", planErr)
		}
		return fmt.Errorf("%w: released commit-plan presence disagrees with disk: %w",
			errDefinitiveExportRejection, domain.ErrParentKeyMismatch)
	}
	if planErr != nil && !errors.Is(planErr, os.ErrNotExist) {
		return fmt.Errorf("inspect released commit plan: %w", planErr)
	}
	return nil
}

// validateReleasedExportRecord authenticates the durable, directory-free
// recovery path. The write-once export row binds the decoded release payload;
// only then may its evidence entries reconstruct a completed result.
func validateReleasedExportRecord(
	exportRoot string, in intent, out exportOutcome, record domain.ExecutionExport,
) ([]domain.Digest, error) {
	if err := validateReleasedExportPath(exportRoot, in, out.dir); err != nil {
		return nil, err
	}
	if record.InvocationID != in.InvocationID ||
		record.AdmissionID != in.Spec.AdmissionID ||
		record.ObservedBaseSHA != in.Spec.Base.BaseSHA ||
		out.observedBaseSHA != record.ObservedBaseSHA ||
		out.commitPlanPresent != record.CommitPlanPresent {
		return nil, fmt.Errorf("stored execution export disagrees with invocation %s: %w",
			in.InvocationID, domain.ErrParentKeyMismatch)
	}
	manifest, err := out.manifest.Encode()
	if err != nil {
		return nil, fmt.Errorf("validate recovered manifest: %w", err)
	}
	if digestBytes(manifest) != record.ManifestDigest {
		return nil, fmt.Errorf("recovered manifest disagrees with export record: %w",
			domain.ErrParentKeyMismatch)
	}
	if out.evidencePresent != (record.EvidenceManifestDigest != nil) {
		return nil, fmt.Errorf("recovered evidence presence disagrees with export record: %w",
			domain.ErrParentKeyMismatch)
	}
	if !out.evidencePresent {
		return nil, nil
	}
	evidence, err := out.evidence.Encode()
	if err != nil {
		return nil, fmt.Errorf("validate recovered evidence: %w", err)
	}
	if digestBytes(evidence) != *record.EvidenceManifestDigest {
		return nil, fmt.Errorf("recovered evidence disagrees with export record: %w",
			domain.ErrParentKeyMismatch)
	}
	artifacts := make([]domain.Digest, 0, len(out.evidence.Entries))
	for _, entry := range out.evidence.Entries {
		artifacts = append(artifacts, domain.Digest(entry.Digest))
	}
	return artifacts, nil
}

func digestBytes(body []byte) domain.Digest {
	sum := sha256.Sum256(body)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func validateReleasedExportPath(exportRoot string, in intent, dir string) error {
	clean := filepath.Clean(dir)
	wantPrefix := "freeside-handoff-" + in.RunID + "-out-"
	if clean != dir || filepath.Dir(clean) != exportRoot ||
		!strings.HasPrefix(filepath.Base(clean), wantPrefix) ||
		len(filepath.Base(clean)) == len(wantPrefix) {
		return fmt.Errorf("released export path %q is not owned by run %s: %w",
			dir, in.RunID, ErrUnsupportedStart)
	}
	return nil
}

func (d *Driver) authenticateReleasedExport(ctx context.Context, in intent, dir string) error {
	if err := validateReleasedExportPath(d.exportRoot, in, dir); err != nil {
		return err
	}
	if err := d.gate.AuthenticateReleasedExport(ctx, in.RunID, dir); err != nil {
		return fmt.Errorf("authenticate released export for %s: %w", in.InvocationID, err)
	}
	return nil
}

func classifyReleasedExportAuthentication(err error) error {
	if errors.Is(err, ErrUnsupportedStart) ||
		errors.Is(err, ward.ErrInvalidJournalRecord) {
		return err
	}
	return fmt.Errorf("%w: authenticate returned handoff: %w",
		ErrRecoveryRetryable, err)
}

func removeReleasedExport(dir string) {
	_ = os.RemoveAll(dir)
}

// persistManifests stores the manifest byte streams the export record names
// under their recorded digests, so both objects outlive the export
// directory the driver removes.
func (d *Driver) persistManifests(
	ctx context.Context, out exportOutcome, record domain.ExecutionExport,
) error {
	manifest, err := readDigestedFile(filepath.Join(out.dir, export.ManifestFilename), record.ManifestDigest)
	if err != nil {
		return err
	}
	if err := d.artifacts.PutBlob(ctx, record.ManifestDigest, manifest); err != nil {
		return fmt.Errorf("persist repo manifest: %w", err)
	}
	if record.EvidenceManifestDigest == nil {
		return nil
	}
	evidence, err := readDigestedFile(
		filepath.Join(out.dir, export.EvidenceFilename), *record.EvidenceManifestDigest)
	if err != nil {
		return err
	}
	if err := d.artifacts.PutBlob(ctx, *record.EvidenceManifestDigest, evidence); err != nil {
		return fmt.Errorf("persist evidence manifest: %w", err)
	}
	return nil
}

// readDigestedFile reads one released file and re-verifies it against the
// digest the record will name, so the stored object is the one the audit
// trail claims.
func readDigestedFile(path string, digest domain.Digest) ([]byte, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: gate-released export path this driver just received
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	sum := sha256.Sum256(body)
	if got := domain.Digest("sha256:" + hex.EncodeToString(sum[:])); got != digest {
		return nil, fmt.Errorf("%s hashes to %s, record names %s", filepath.Base(path), got, digest)
	}
	return body, nil
}

// persistEvidence copies each released evidence blob into durable storage
// and records the importer's agent claims, returning the digests the result
// may safely name. A claim whose blob is missing from the export fails the
// stage rather than being recorded unresolvable.
func (d *Driver) persistEvidence(
	ctx context.Context, in intent, out exportOutcome, claims []domain.AgentClaim,
) ([]domain.Digest, error) {
	if !out.evidencePresent {
		if len(claims) > 0 {
			return nil, fmt.Errorf("import returned %d claims with no evidence channel", len(claims))
		}
		return nil, nil
	}
	digests := make([]domain.Digest, 0, len(out.evidence.Entries))
	for _, entry := range out.evidence.Entries {
		digest := domain.Digest(entry.Digest)
		body, err := readEvidenceBlob(out.dir, digest)
		if err != nil {
			return nil, err
		}
		if err := d.artifacts.PutBlob(ctx, digest, body); err != nil {
			return nil, fmt.Errorf("persist evidence %s: %w", entry.Label, err)
		}
		digests = append(digests, digest)
	}
	if err := d.artifacts.RecordClaims(ctx, in.InvocationID, claims); err != nil {
		return nil, fmt.Errorf("record agent claims: %w", err)
	}
	return digests, nil
}

// readEvidenceBlob reads one released evidence blob and re-verifies its
// content address. The gate already verified the export, but this copy
// crosses into durable storage under that digest, so it is re-checked at the
// boundary rather than trusted across it.
func readEvidenceBlob(dir string, digest domain.Digest) ([]byte, error) {
	hexDigits, ok := contentaddr.Parse(string(digest))
	if !ok {
		return nil, fmt.Errorf("evidence digest %q is not canonical", digest)
	}
	path := filepath.Join(dir, export.EvidenceBlobsDirname, "sha256", hexDigits)
	body, err := os.ReadFile(path) //nolint:gosec // G304: gate-released export path addressed by verified digest
	if err != nil {
		return nil, fmt.Errorf("read evidence blob %s: %w", digest, err)
	}
	if got := sha256.Sum256(body); hex.EncodeToString(got[:]) != hexDigits {
		return nil, fmt.Errorf("evidence blob %s does not match its digest", digest)
	}
	return body, nil
}

// recordExport writes the write-once export row, converging on an identical
// stored record rather than failing a replay. A stored record that disagrees
// is a real conflict: two different heads claiming one invocation.
func (d *Driver) recordExport(
	ctx context.Context,
	record domain.ExecutionExport,
	replay ExecutionReplay,
) error {
	if err := d.exports.RecordExecutionExport(ctx, record, replay); err != nil {
		if errors.Is(err, domain.ErrImmutableTransition) ||
			errors.Is(err, domain.ErrParentKeyMismatch) {
			return fmt.Errorf("%w: %w", errExportAuthorityConflict, err)
		}
		return fmt.Errorf("record execution export and replay: %w", err)
	}
	return nil
}

// seedBase materializes the daemon-owned checkout the gate seeds from. The
// directory is cleared first so a replayed start after a crashed
// materialization is not wedged by a partial checkout.
func (d *Driver) seedBase(ctx context.Context, in intent) error {
	if err := os.MkdirAll(d.seedRoot, 0o700); err != nil {
		return fmt.Errorf("create seed root: %w", err)
	}
	if err := os.RemoveAll(in.Seed); err != nil {
		return fmt.Errorf("clear seed checkout: %w", err)
	}
	// The workspace seed needs the files, not just the objects: ward's
	// observer compares the raw worktree with HEAD and refuses a repository
	// whose tracked paths are all missing.
	if err := d.seeder.FetchBaseWorktree(ctx,
		in.Spec.Base.Repo, in.Spec.Base.BaseRef, in.Spec.Base.BaseSHA, in.Seed,
	); err != nil {
		if errors.Is(err, ErrSeedRefused) {
			return fmt.Errorf("fetch exact base: %w", err)
		}
		if errors.Is(err, ErrSeedRetryable) || ctx.Err() != nil {
			return fmt.Errorf("%w: fetch exact base: %w", ErrRecoveryRetryable, err)
		}
		return fmt.Errorf("fetch exact base: %w", err)
	}
	return nil
}

// cleanupTerminalSeed removes only daemon-derived names beneath the trusted
// seed root. The persisted Seed field is authenticated for reconstruction but
// is deliberately not deletion authority. A Root-scoped removal cannot follow
// a replaced child symlink outside seedRoot.
func (d *Driver) cleanupTerminalSeed(in intent) error {
	if !in.Phase.valid() {
		return fmt.Errorf("refuse seed cleanup for invalid phase %q", in.Phase)
	}
	switch in.Phase {
	case phaseCommitted, phaseLost:
	case phaseSeeding, phaseRunning, phaseExported:
		return fmt.Errorf("refuse seed cleanup for nonterminal invocation %s in phase %q",
			in.InvocationID, in.Phase)
	}
	d.seedMu.Lock()
	defer d.seedMu.Unlock()
	if d.seedFS == nil {
		return errors.New("terminal seed cleanup after driver close")
	}
	runID := RunIDFor(in.InvocationID)
	for _, name := range []string{runID, runID + "-import"} {
		if err := d.seedFS.RemoveAll(name); err != nil {
			return fmt.Errorf("%w: remove terminal seed %s: %w",
				ErrRecoveryRetryable, name, err)
		}
	}
	return nil
}

// commitResult makes one terminal result durable and collectable. The durable
// record is the only phase authority: callers may hold a copy from before a
// handoff or recovery advanced it, and writing that copy would erase the
// released export the completed result must authenticate against.
func (d *Driver) commitResult(id domain.InvocationID, result exec.StageResult) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.commitResultLocked(id, result)
	if err != nil {
		if sess := d.running[id]; sess != nil {
			pending := result
			pending.Artifacts = append([]domain.Digest(nil), result.Artifacts...)
			sess.pendingResult = &pending
			sess.commitErr = err
		}
	}
	return err
}

func (d *Driver) retainPendingIntent(in intent, cause error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	sess := d.running[in.InvocationID]
	if sess == nil {
		return fmt.Errorf("invocation %s has no live session for returned handoff retry",
			in.InvocationID)
	}
	pending := in
	sess.pendingIntent = &pending
	sess.intentErr = cause
	return nil
}

func (d *Driver) retainRecoveredIntent(in intent, cause error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running[in.InvocationID] != nil {
		return fmt.Errorf("invocation %s already has a live session while retaining recovery",
			in.InvocationID)
	}
	done := make(chan struct{})
	close(done)
	pending := in
	d.running[in.InvocationID] = &session{
		cancel:        func() {},
		done:          done,
		pendingIntent: &pending,
		intentErr:     cause,
	}
	return nil
}

func (d *Driver) retryPendingIntentLocked(id domain.InvocationID) error {
	sess := d.running[id]
	if sess == nil || sess.pendingIntent == nil {
		return nil
	}
	if err := d.saveIntent(*sess.pendingIntent); err != nil {
		previous := sess.intentErr
		sess.intentErr = err
		return fmt.Errorf("%w: retry returned handoff persistence: %w",
			ErrRecoveryRetryable, errors.Join(previous, err))
	}
	sess.pendingIntent = nil
	sess.intentErr = nil
	select {
	case <-sess.done:
		if sess.pendingResult == nil {
			delete(d.running, id)
		}
	default:
	}
	return nil
}

// commitResultLocked makes the result durable while d.mu is held. committed
// is false without an error only for a non-completed result in the running
// handoff phase: only ward recovery may decide whether that credential-bearing
// writer was torn down.
func (d *Driver) commitResultLocked(
	id domain.InvocationID, result exec.StageResult,
) (committed bool, err error) {
	// Re-read: the pipeline may be racing a Reconcile that already committed
	// a terminal phase for this invocation, and its local intent copy may
	// predate a durable phase advance.
	in, err := d.loadIntentAdmission(context.Background(), id)
	if err != nil {
		return false, err
	}
	if in.Phase == phaseCommitted || in.Phase == phaseLost {
		_ = d.cleanupTerminalSeed(in)
		return true, nil
	}
	if result.Status != exec.StatusCompleted && in.Phase == phaseRunning {
		// Handoff errors do not prove teardown succeeded. The writer may still
		// hold the credential volume, so only ward recovery may turn this phase
		// into an explicit export or loss outcome.
		return false, nil
	}
	if result.Status != exec.StatusCompleted {
		if err := d.recordOutcome(context.Background(), in, result); err != nil {
			return false, err
		}
	}
	in.Phase = phaseCommitted
	in.Result = &result
	if err := d.saveIntent(in); err != nil {
		return false, err
	}
	_ = d.cleanupTerminalSeed(in)
	return true, nil
}

// retryPendingResultLocked gives a terminal intent write that failed after
// the pipeline ended a durable retry path. The session remains registered
// until this succeeds, so Inspect cannot misreport the invocation as gone and
// silently turn a local persistence failure into ward recovery.
func (d *Driver) retryPendingResultLocked(id domain.InvocationID) error {
	sess := d.running[id]
	if sess == nil || sess.pendingResult == nil {
		return nil
	}
	var committed bool
	var err error
	if sess.preJournalCancellation {
		committed, err = d.commitPreJournalCancellationLocked(id, *sess.pendingResult)
	} else {
		committed, err = d.commitResultLocked(id, *sess.pendingResult)
	}
	if err != nil {
		previous := sess.commitErr
		sess.commitErr = err
		return errors.Join(previous, err)
	}
	if !committed {
		return fmt.Errorf("pending terminal result is not safe to commit")
	}
	sess.pendingResult = nil
	sess.commitErr = nil
	sess.preJournalCancellation = false
	delete(d.running, id)
	return nil
}

// commitLost records a proven recovery loss from the current durable phase.
// Like commitResult, it accepts only identity from its caller: a recovery
// operation may have advanced the record after the caller took its snapshot,
// and an older value must never erase that progress.
func (d *Driver) commitLost(ctx context.Context, id domain.InvocationID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	in, err := d.loadIntentAdmission(ctx, id)
	if err != nil {
		return err
	}
	switch in.Phase {
	case phaseCommitted, phaseLost:
		_ = d.cleanupTerminalSeed(in)
		return nil
	case phaseRunning, phaseExported:
	case phaseSeeding:
		return fmt.Errorf("cannot record invocation %s lost before handoff: %w",
			id, exec.ErrInvalidStatus)
	}
	if err := d.recordLost(ctx, in); err != nil {
		return err
	}
	in.Phase = phaseLost
	if err := d.saveIntent(in); err != nil {
		return err
	}
	_ = d.cleanupTerminalSeed(in)
	return nil
}

func outcomeStatus(status exec.Status) (domain.ExecutionOutcomeStatus, error) {
	switch status {
	case exec.StatusFailed:
		return domain.ExecutionOutcomeFailed, nil
	case exec.StatusCanceled:
		return domain.ExecutionOutcomeCanceled, nil
	case exec.StatusCompleted, exec.StatusPending, exec.StatusRunning, exec.StatusGone:
		return "", fmt.Errorf("status %q is not a non-export terminal outcome", status)
	}
	return "", fmt.Errorf("status %q is not a non-export terminal outcome", status)
}

func (d *Driver) recordOutcome(
	ctx context.Context, in intent, result exec.StageResult,
) error {
	status, err := outcomeStatus(result.Status)
	if err != nil {
		return err
	}
	want := domain.ExecutionOutcome{
		InvocationID: in.InvocationID,
		AdmissionID:  in.Spec.AdmissionID,
		Status:       status,
		Summary:      result.Summary,
		RecordedAt:   d.now().UTC(),
	}
	return d.recordOrConvergeOutcome(ctx, want)
}

func (d *Driver) recordLost(ctx context.Context, in intent) error {
	return d.recordOrConvergeOutcome(ctx, domain.ExecutionOutcome{
		InvocationID: in.InvocationID,
		AdmissionID:  in.Spec.AdmissionID,
		Status:       domain.ExecutionOutcomeLost,
		RecordedAt:   d.now().UTC(),
	})
}

func (d *Driver) recordOrConvergeOutcome(
	ctx context.Context, want domain.ExecutionOutcome,
) error {
	stored, found, err := d.outcomes.LookupExecutionOutcome(ctx, want.InvocationID)
	if err != nil {
		return fmt.Errorf("lookup execution outcome: %w", err)
	}
	if found {
		if sameOutcome(stored, want) {
			return nil
		}
		return fmt.Errorf("execution outcome for %s disagrees with durable record: %w",
			want.InvocationID, domain.ErrImmutableTransition)
	}
	recordErr := d.outcomes.RecordExecutionOutcome(ctx, want)
	if recordErr == nil {
		return nil
	}
	stored, found, lookupErr := d.outcomes.LookupExecutionOutcome(ctx, want.InvocationID)
	if lookupErr == nil && found && sameOutcome(stored, want) {
		return nil
	}
	_, exportFound, exportErr := d.exports.LookupExecutionExport(ctx, want.InvocationID)
	if exportErr == nil && exportFound {
		return fmt.Errorf("%w: non-export outcome for %s conflicts with durable export: %w",
			errExportAuthorityConflict, want.InvocationID, recordErr)
	}
	return fmt.Errorf("record execution outcome for %s: %w",
		want.InvocationID, errors.Join(recordErr, lookupErr, exportErr))
}

func sameOutcome(a, b domain.ExecutionOutcome) bool {
	return a.InvocationID == b.InvocationID &&
		a.AdmissionID == b.AdmissionID &&
		a.Status == b.Status &&
		a.Summary == b.Summary
}

func (d *Driver) authenticateOutcome(
	ctx context.Context, in intent, result exec.StageResult,
) error {
	status, err := outcomeStatus(result.Status)
	if err != nil {
		return fmt.Errorf("%w: committed intent %s: %w",
			ErrUnsupportedStart, in.InvocationID, err)
	}
	stored, found, err := d.outcomes.LookupExecutionOutcomeRecord(ctx, in.InvocationID)
	if err != nil {
		return fmt.Errorf("authenticate outcome %s: %w", in.InvocationID, err)
	}
	want := domain.ExecutionOutcome{
		InvocationID: in.InvocationID,
		AdmissionID:  in.Spec.AdmissionID,
		Status:       status,
		Summary:      result.Summary,
	}
	if !found || !sameOutcome(stored, want) {
		return fmt.Errorf("%w: committed intent %s has no matching durable outcome",
			ErrUnsupportedStart, in.InvocationID)
	}
	return nil
}

func (d *Driver) authenticateLost(ctx context.Context, in intent) error {
	stored, found, err := d.outcomes.LookupExecutionOutcomeRecord(ctx, in.InvocationID)
	if err != nil {
		return fmt.Errorf("authenticate lost outcome %s: %w", in.InvocationID, err)
	}
	want := domain.ExecutionOutcome{
		InvocationID: in.InvocationID,
		AdmissionID:  in.Spec.AdmissionID,
		Status:       domain.ExecutionOutcomeLost,
	}
	if !found || !sameOutcome(stored, want) {
		return fmt.Errorf("%w: lost intent %s has no matching durable outcome",
			ErrUnsupportedStart, in.InvocationID)
	}
	return nil
}

// Reconcile gives every intent this process did not start its first adoption
// attempt after a daemon restart. A retryable operational failure preserves
// the preterminal phase for the engine's later Inspect passes; permanent
// reconstruction errors still fail startup.
func (d *Driver) Reconcile(ctx context.Context) error {
	d.mu.Lock()
	intents, err := d.listIntents(ctx)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	for _, in := range intents {
		if in.Phase == phaseCommitted || in.Phase == phaseLost {
			_ = d.cleanupTerminalSeed(in)
			continue
		}
		d.mu.Lock()
		_, live := d.running[in.InvocationID]
		d.mu.Unlock()
		if live {
			continue
		}
		if err := d.reconcileIntent(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) reconcileIntent(ctx context.Context, in intent) error {
	d.mu.Lock()
	if _, live := d.running[in.InvocationID]; live {
		d.mu.Unlock()
		return nil
	}
	if _, alreadyRecovering := d.recovering[in.InvocationID]; alreadyRecovering {
		d.mu.Unlock()
		return nil
	}
	d.recovering[in.InvocationID] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.recovering, in.InvocationID)
		d.mu.Unlock()
	}()

	if err := d.recoverIntent(ctx, in); err != nil {
		if errors.Is(err, errExportAuthorityConflict) {
			return err
		}
		if errors.Is(err, ErrRecoveryRetryable) {
			return err
		}
		// One unrecoverable invocation must not wedge the daemon: an error here
		// would leave the same intent to fail every later pass. Record the
		// stage failure when the durable phase proves that is safe. A running
		// handoff refuses this transition until ward proves export or loss.
		if commitErr := d.commitResult(in.InvocationID, exec.StageResult{
			InvocationID: in.InvocationID, Status: exec.StatusFailed,
			Summary: truncateSummary("recovery failed: " + err.Error()),
		}); commitErr != nil {
			return fmt.Errorf("%w: commit recovery failure: %w",
				ErrRecoveryRetryable, commitErr)
		}
	}
	return nil
}

// recoverIntent asks the gate what became of one orphaned handoff and
// commits the corresponding terminal phase. The spec is rebuilt from the
// durable intent, never from current policy: re-deriving would silently
// retarget an in-flight invocation whose configuration has moved.
func (d *Driver) recoverIntent(ctx context.Context, in intent) error {
	switch in.Phase {
	case phaseSeeding:
		// The gate was never called, so no ward object and no journal record
		// exist and nothing external happened. Re-running the pipeline is
		// both safe and better than losing the work item: the intent already
		// pins every value a replay must reproduce.
		return d.resume(in)
	case phaseExported:
		// The gate closed its journal when Handoff returned and refuses to
		// recover a closed record, so this window is the driver's to finish.
		return d.recoverExported(ctx, in)
	case phaseRunning:
		// Handoff can refuse before its atomic journal/lease open (for example,
		// while another holder owns the identity lease) after the driver has
		// already persisted phaseRunning. Confirmed journal absence proves no
		// ward object or writer existed, so return to the rerunnable phase.
		// Any opened record remains exclusively ward recovery's authority.
		started, err := d.gate.HandoffStarted(ctx, in.RunID)
		if err != nil {
			return fmt.Errorf("%w: inspect running handoff authority: %w",
				ErrRecoveryRetryable, err)
		}
		if !started {
			if err := d.advance(&in, phaseSeeding, nil); err != nil {
				return fmt.Errorf("%w: return pre-journal refusal to seeding: %w",
					ErrRecoveryRetryable, err)
			}
			return d.resume(in)
		}
	case phaseCommitted, phaseLost:
		return nil
	}
	hs, err := d.handoffSpec(ctx, in)
	if err != nil {
		return fmt.Errorf("%w: rebuild running handoff: %w", ErrRecoveryRetryable, err)
	}
	recovered, err := d.gate.Recover(ctx, in.RunID, hs)
	if err != nil {
		return fmt.Errorf("%w: ward recovery: %w", ErrRecoveryRetryable, err)
	}
	switch recovered.Outcome {
	case ward.RecoveryExported:
		out := exportOutcome{
			dir: recovered.ExportDir, manifest: recovered.Manifest,
			evidence: recovered.Evidence, evidencePresent: recovered.EvidencePresent,
			commitPlanPresent: recovered.CommitPlanPresent,
			observedBaseSHA:   recovered.Workspace.ObservedBaseSHA,
		}
		// Recover has now closed the same journal as a successful Handoff.
		// Persist its complete return as untrusted replay data before any
		// authentication or validation can fail. If the write itself fails,
		// retain the exact return in a completed synthetic session so Inspect
		// can retry it without asking ward to recover a closed record again.
		if err := d.advance(&in, phaseExported, releasedFrom(out)); err != nil {
			if retainErr := d.retainRecoveredIntent(in, err); retainErr != nil {
				return errors.Join(err, retainErr)
			}
			return fmt.Errorf("%w: persist recovered handoff: %w",
				ErrRecoveryRetryable, err)
		}
		if err := d.authenticateReleasedExport(ctx, in, out.dir); err != nil {
			return classifyReleasedExportAuthentication(err)
		}
		result, err := d.completeReleasedExport(ctx, in, out)
		if err != nil {
			if errors.Is(err, errExportAuthorityConflict) {
				return err
			}
			if errors.Is(err, ErrRecoveryRetryable) {
				return err
			}
			// A daemon killed mid-recovery reports canceled, not failed: the
			// run is recoverable, and recording it as a failure would be a
			// durable verdict about the work rather than about the shutdown.
			status := exec.StatusFailed
			if ctx.Err() != nil {
				status = exec.StatusCanceled
			}
			if commitErr := d.commitResult(in.InvocationID, exec.StageResult{
				InvocationID: in.InvocationID, Status: status,
				Summary: truncateSummary(err.Error()),
			}); commitErr != nil {
				return fmt.Errorf("%w: commit recovery result: %w",
					ErrRecoveryRetryable, commitErr)
			}
			return nil
		}
		if err := d.commitResult(in.InvocationID, result); err != nil {
			return fmt.Errorf("%w: commit recovered result: %w",
				ErrRecoveryRetryable, err)
		}
		return nil
	case ward.RecoveryLoss:
		// Every runtime object was proven absent with nothing released, so
		// the invocation ends with no result: Collect reports ErrNoResult and
		// the production lane records a gone terminal with its
		// execution_failure item (the deterministic per-run invocation id
		// means the engine does not mint a fresh attempt).
		return d.commitLost(ctx, in.InvocationID)
	case ward.RecoveryFailed:
		// The pre-agent preparation sentinel is indistinguishable from an agent
		// failure under the bare status line, so name it: the workspace-hydration
		// helper ran and exited nonzero before the agent started (e.g. its
		// manifest guard exit 42), which is an environment fault the operator
		// triages differently from an agent exit.
		summary := fmt.Sprintf("Claude writer exited with status %d.", recovered.FailureStatus)
		if recovered.FailureStatus == writerOutcomePrepareFailed {
			summary = fmt.Sprintf(
				"Workspace preparation failed before the agent started (status %d): "+
					"the project-image hydration helper exited nonzero.",
				writerOutcomePrepareFailed)
		}
		return d.commitRecoveredTerminal(in.InvocationID, exec.StageResult{
			InvocationID: in.InvocationID,
			Status:       exec.StatusFailed,
			Summary:      truncateSummary(summary),
		})
	case ward.RecoveryCanceled:
		return d.commitRecoveredTerminal(in.InvocationID, exec.StageResult{
			InvocationID: in.InvocationID,
			Status:       exec.StatusCanceled,
			Summary:      "Claude invocation canceled by daemon request.",
		})
	}
	return fmt.Errorf("%w: gate reported recovery outcome %q",
		ErrRecoveryRetryable, recovered.Outcome)
}

// commitRecoveredTerminal is the phaseRunning counterpart to commitLost:
// ward has already closed its journal after proving teardown, so the ordinary
// live-handoff guard must not suppress the canonical failed outcome.
func (d *Driver) commitRecoveredTerminal(
	id domain.InvocationID, result exec.StageResult,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	in, err := d.loadIntentAdmission(context.Background(), id)
	if err != nil {
		return err
	}
	if in.Phase == phaseCommitted {
		return nil
	}
	if in.Phase != phaseRunning ||
		(result.Status != exec.StatusFailed && result.Status != exec.StatusCanceled) {
		return fmt.Errorf("recovered terminal from phase %q with status %q: %w",
			in.Phase, result.Status, exec.ErrInvalidStatus)
	}
	if err := d.recordOutcome(context.Background(), in, result); err != nil {
		return err
	}
	in.Phase = phaseCommitted
	in.Result = &result
	if err := d.saveIntent(in); err != nil {
		return err
	}
	_ = d.cleanupTerminalSeed(in)
	return nil
}

// fileDigest is the content address of one released manifest file.
func fileDigest(path string) (domain.Digest, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: gate-released export path this driver just received
	if err != nil {
		return "", fmt.Errorf("digest %s: %w", filepath.Base(path), err)
	}
	sum := sha256.Sum256(body)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}

// maxSummaryBytes bounds the human-readable outcome the engine carries into
// an attention item.
const maxSummaryBytes = 512

func truncateSummary(s string) string {
	// Error strings can carry data from filesystem or process boundaries.
	// Normalize first so the durable JSON body and extracted summary column
	// cannot disagree about replacement of malformed byte sequences.
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= maxSummaryBytes {
		return s
	}
	const suffix = "…"
	cut := maxSummaryBytes - len(suffix)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// resume restarts a pipeline for an intent whose external effects had not
// begun, under the same in-process bookkeeping a fresh Start uses.
func (d *Driver) resume(in intent) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return fmt.Errorf("%w: %w", ErrRecoveryRetryable, ErrDriverClosed)
	}
	runCtx, cancel := context.WithCancel(d.lifetime)
	sess := &session{cancel: cancel, done: make(chan struct{})}
	d.running[in.InvocationID] = sess
	d.mu.Unlock()
	go func() {
		defer close(sess.done)
		d.runPipeline(runCtx, in)
		d.mu.Lock()
		if sess.pendingIntent == nil && sess.pendingResult == nil {
			delete(d.running, in.InvocationID)
		}
		d.mu.Unlock()
	}()
	return nil
}

// recoverExported finishes an invocation whose export the gate had already
// released. A durable export record is terminal authority and outranks its
// expendable source directory; without a record, the directory is replayed.
func (d *Driver) recoverExported(ctx context.Context, in intent) error {
	out := in.Export.outcome()
	if err := d.authenticateReleasedExport(ctx, in, out.dir); err != nil {
		return classifyReleasedExportAuthentication(err)
	}
	record, found, lookupErr := d.exports.LookupExecutionExportRecord(ctx, in.InvocationID)
	if lookupErr == nil && found {
		if err := d.commitRecordedExport(in, out, record); err != nil {
			return err
		}
		removeReleasedExport(out.dir)
		return nil
	}
	if _, err := os.Stat(out.dir); err == nil {
		result, err := d.completeReleasedExport(ctx, in, out)
		if err != nil {
			return err
		}
		if err := d.commitResult(in.InvocationID, result); err != nil {
			return fmt.Errorf("%w: commit recovered export result: %w",
				ErrRecoveryRetryable, err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: inspect released export: %w",
			ErrRecoveryRetryable, err)
	}
	if lookupErr != nil {
		// A read or reconstruction error says nothing about presence. Leave
		// the exported intent retryable rather than turning a transient
		// failure into permanent loss.
		return fmt.Errorf("%w: lookup execution export: %w",
			ErrRecoveryRetryable, lookupErr)
	}
	if !found {
		// Neither the released directory nor a durable record survived, so
		// nothing was published and the attempt is rerun-safe.
		return d.commitLost(ctx, in.InvocationID)
	}
	return d.commitRecordedExport(in, out, record)
}

// LoadExecutionReplay returns the authenticated durable source description
// for a completed export. It reconstructs the import policy from the immutable
// admission record; the caller must still independently authenticate those
// options, import the durable bytes, compare the produced head with HeadSHA,
// and apply current publication authority before any external effect.
func (d *Driver) LoadExecutionReplay(
	ctx context.Context, id domain.InvocationID,
) (ExecutionReplay, error) {
	in, err := d.loadIntentAdmission(ctx, id)
	if err != nil {
		return ExecutionReplay{}, err
	}
	if in.Phase != phaseCommitted || in.Result == nil ||
		in.Result.Status != exec.StatusCompleted || in.Export == nil ||
		in.Export.Replay == nil {
		return ExecutionReplay{}, fmt.Errorf("invocation %s has no completed execution replay: %w",
			id, ErrUnsupportedStart)
	}
	record, found, err := d.exports.LookupExecutionExportRecord(ctx, id)
	if err != nil {
		return ExecutionReplay{}, fmt.Errorf("load execution replay export: %w", err)
	}
	if !found || in.Result.HeadSHA != record.HeadSHA {
		return ExecutionReplay{}, fmt.Errorf("execution replay for %s disagrees with durable export: %w",
			id, domain.ErrParentKeyMismatch)
	}
	options, err := d.authority.ImportOptionsRecord(ctx, id, in.Spec, d.imports)
	if err != nil {
		return ExecutionReplay{}, fmt.Errorf("load execution replay import policy: %w", err)
	}
	options.BaseSHA = in.Spec.Base.BaseSHA
	options.CommitDate = in.CommitDate
	return ExecutionReplay{
		InvocationID: id, ObservedBaseSHA: record.ObservedBaseSHA,
		HeadSHA: record.HeadSHA, Manifest: in.Export.Manifest,
		ManifestDigest: record.ManifestDigest, Evidence: in.Export.Evidence,
		EvidenceManifestDigest: cloneDigest(record.EvidenceManifestDigest),
		CommitPlanDigest:       cloneDigest(in.Export.Replay.CommitPlanDigest),
		ImportOptions:          options,
	}, nil
}

func cloneDigest(digest *domain.Digest) *domain.Digest {
	if digest == nil {
		return nil
	}
	copyDigest := *digest
	return &copyDigest
}

func (d *Driver) commitRecordedExport(
	in intent, out exportOutcome, record domain.ExecutionExport,
) error {
	artifacts, err := validateReleasedExportRecord(d.exportRoot, in, out, record)
	if err != nil {
		return fmt.Errorf("%w: %w", errExportAuthorityConflict, err)
	}
	if err := d.commitResult(in.InvocationID, exec.StageResult{
		InvocationID: in.InvocationID, Status: exec.StatusCompleted,
		HeadSHA:   record.HeadSHA,
		Artifacts: artifacts,
		Summary: fmt.Sprintf("Recovered candidate %s over base %s.",
			record.HeadSHA, record.ObservedBaseSHA),
	}); err != nil {
		return fmt.Errorf("%w: commit recovered export record: %w",
			ErrRecoveryRetryable, err)
	}
	return nil
}
