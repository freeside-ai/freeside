package stage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// errDefinitiveExportRejection marks a returned workspace that the trust
// boundary has conclusively rejected. Only this class may discard the
// released directory; every operational or ambiguous failure preserves the
// phaseExported replay state.
var errDefinitiveExportRejection = errors.New("released export was definitively rejected")

// definitiveRejection carries a rejected export's diagnostic finding sample
// from the import boundary to the terminal-commit path. The ExportRejection is
// recorded there, best-effort, only after the authoritative failed
// ExecutionOutcome commits (recordRejectionDetail) — never before the released
// directory is cleaned. Recovery therefore never observes a rejection without
// its outcome and needs no rejection-specific special-casing: a crash between
// the outcome and the detail write loses only the diagnostic, never
// correctness (issue #768). It wraps errDefinitiveExportRejection so the
// directory-cleanup classification is unchanged, and its message is the
// count-only summary the failed outcome carries (per-finding paths stay
// daemon-internal, off the client-visible AttentionItem.Reason).
type definitiveRejection struct {
	findings []domain.ExportRejectionFinding
	// fatal is the count of findings that actually blocked under the profile;
	// total is every reported finding. They differ only under the specification
	// profile, where tolerated debris is reported but not blocking, so the
	// summary must name the fatal count rather than labelling debris
	// publish-blocking.
	fatal int
	total int
}

func (r *definitiveRejection) Error() string {
	if r.fatal == r.total {
		return fmt.Sprintf("%s: gauntlet containment reported %d publish-blocking findings",
			errDefinitiveExportRejection.Error(), r.total)
	}
	return fmt.Sprintf("%s: gauntlet containment reported %d publish-blocking of %d findings",
		errDefinitiveExportRejection.Error(), r.fatal, r.total)
}

func (r *definitiveRejection) Unwrap() error { return errDefinitiveExportRejection }

// errExportAuthorityConflict marks a durable ExecutionExport that disagrees
// with this invocation's released facts. It is neither retryable nor safe to
// terminalize: recording a failed outcome beside the existing export would
// create two contradictory authorities. Recovery preserves the evidence and
// fails loud for operator repair.
var errExportAuthorityConflict = errors.New("released export conflicts with durable authority")

// runPipeline drives one invocation from seeded workspace to committed
// result. Operational failures after a handoff has returned deliberately
// remain in an exported preterminal phase so Inspect can retry them without
// discarding work.
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
	var rej *definitiveRejection
	if errors.As(err, &rej) {
		// The live session retains a failed terminal write for Inspect/Collect,
		// so a returned error only needs logging here, matching d.commit.
		if commitErr := d.commitRejection(ctx, log, in, status, rej, result.Usage); commitErr != nil {
			log.Error("terminal result write failed; retained for retry",
				"status", string(status), "error", commitErr)
		}
		return
	}
	d.commit(log, in.InvocationID, exec.StageResult{
		InvocationID: in.InvocationID, Status: status,
		Summary: truncateSummary(err.Error()), Usage: result.Usage,
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

// rejectionDetailWriteTimeout bounds the best-effort diagnostic write so a
// hung store cannot delay daemon shutdown, while still giving the write a
// chance to land after the run context is canceled.
const rejectionDetailWriteTimeout = 15 * time.Second

// commitRejection commits the authoritative failed (or canceled) outcome for a
// definitive rejection, then records the diagnostic ExportRejection detail
// best-effort. The order is load-bearing: the outcome is the terminal
// authority, so it commits first; the detail write follows and never blocks or
// fails the terminal, which is what keeps recovery free of rejection-specific
// convergence special-casing (issue #768).
//
// It returns the terminal-write error so a caller without a retrying live
// session — recovery, which has already deleted the released directory — can
// propagate it and be retried, rather than dropping a failed write as success
// and having the next reconciliation record the rejected attempt as lost. The
// live-pipeline caller ignores the return: commitResult has already retained
// the pending result on its session for Inspect/Collect to retry.
//
// The detail write runs on a context detached from the run's cancellation, the
// same way commitResult writes the outcome under context.Background(). This is
// what makes it best-effort during the shutdown race: a canceled run still
// commits a canceled outcome, and passing that canceled context to the store
// would fail the diagnostic insert every time. It stays bounded so a stalled
// store cannot hold shutdown open on an expendable write.
func (d *Driver) commitRejection(
	ctx context.Context, log *slog.Logger, in intent, status exec.Status, rej *definitiveRejection,
	usage []exec.UsageMeasurement,
) error {
	if err := d.commitResult(in.InvocationID, exec.StageResult{
		InvocationID: in.InvocationID, Status: status,
		Summary: truncateSummary(rej.Error()), Usage: usage,
	}); err != nil {
		return err
	}
	log.Debug("terminal result committed", "status", string(status))
	detailCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), rejectionDetailWriteTimeout)
	defer cancel()
	d.recordRejectionDetail(detailCtx, in, rej)
	return nil
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
	if err := d.recordCurrentImportStart(ctx, in); err != nil {
		return exec.StageResult{}, fmt.Errorf("%w: record current-policy import authority: %w",
			ErrRecoveryRetryable, err)
	}
	// The gap after phaseExported became durable and before the marker above is
	// the admission-bound crash window. Once the marker lands, it, not this
	// private replay phase, makes every retry retain the current-policy gate the
	// live import is about to apply.
	if err := d.advance(&in, phaseImportPending, nil); err != nil {
		if retainErr := d.retainPendingIntent(in, err); retainErr != nil {
			return exec.StageResult{}, errors.Join(err, retainErr)
		}
		return exec.StageResult{}, fmt.Errorf("%w: record current-policy import: %w",
			ErrRecoveryRetryable, err)
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

type importOptionsProvider func(
	context.Context, domain.InvocationID, exec.StartSpec, importer.Options,
) (importer.Options, error)

// finish imports the released export into a candidate commit and records the
// durable ExecutionExport before the result becomes collectable.
//
// Ordering is load-bearing: every blob and claim the export row implies lands
// first, then the row, then the collectable result. A crash may leave
// unreferenced content-addressed bytes, but can never leave a durable export
// asserting evidence that was not persisted.
func (d *Driver) finish(
	ctx context.Context, in intent, out exportOutcome, importOptions importOptionsProvider,
	usage []exec.UsageMeasurement,
) (exec.StageResult, error) {
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

	opts, err := importOptions(ctx, in.InvocationID, in.Spec, d.imports)
	if err != nil {
		return exec.StageResult{}, fmt.Errorf("derive import policy: %w", err)
	}
	opts.BaseSHA = in.Spec.Base.BaseSHA
	// Pinned at Start, so a replayed import after a crash reproduces the same
	// commit SHA and converges on the recorded export instead of minting a
	// second head for the same work.
	opts.CommitDate = in.CommitDate
	// A declared blocked outcome turns the export into a typed stop: the
	// importer still audits both channels, but must find no change and no
	// commit plan, and builds no commit. An undecodable outcome is the
	// agent's defect, so it is a definitive rejection like malformed evidence.
	blocked, blockedPresent, err := releasedBlockedOutcome(out)
	if err != nil {
		return exec.StageResult{}, fmt.Errorf("%w: %w", errDefinitiveExportRejection, err)
	}
	opts.ExpectNoChanges = blockedPresent
	imported, err := importer.Import(ctx, out.dir, checkoutDir, opts)
	if err != nil {
		err = fmt.Errorf("gauntlet import: %w", err)
		if isDefinitiveImportRejection(err) {
			return exec.StageResult{}, fmt.Errorf("%w: %w",
				errDefinitiveExportRejection, err)
		}
		return exec.StageResult{}, err
	}
	if err := d.validateImportFindings(in, imported, opts.Policy.FindingProfile, !blockedPresent); err != nil {
		return exec.StageResult{}, err
	}
	if blockedPresent {
		return d.finishBlocked(ctx, in, out, imported, blocked, usage)
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
	// Every byte needed to reconstruct a publishable candidate lands before the
	// immutable export row. A crash can leave unreferenced content-addressed
	// bytes, but never an ExecutionExport whose publication source material
	// disappeared with the released directory. The specification profile is the
	// one exception: its commit is never published, so its repo-channel blobs
	// are deliberately not persisted (persistsRepositoryChannel).
	artifacts, replay, err := d.persistReleasedMaterial(
		ctx, in, out, record, imported.Claims, persistsRepositoryChannel(opts.Policy.FindingProfile))
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
		Usage:     usage,
		Summary: fmt.Sprintf("Imported candidate %s over base %s.",
			record.HeadSHA, record.ObservedBaseSHA),
	}, nil
}

// releasedBlockedOutcome finds and strictly decodes the launcher-declared
// blocked outcome in a released export's evidence channel. The blob is read
// from the released directory and re-verified against its digest.
func releasedBlockedOutcome(out exportOutcome) (domain.BlockedOutcome, bool, error) {
	if !out.evidencePresent {
		return domain.BlockedOutcome{}, false, nil
	}
	for _, entry := range out.evidence.Entries {
		if entry.Label != export.BlockedEvidenceLabel {
			continue
		}
		// This runs before the importer applies the evidence blob caps, so
		// bound the read here: the declared size is checked first and the
		// file is never read past the decoder's own limit.
		if entry.Size > int64(domain.MaxBlockedOutcomeBytes) {
			return domain.BlockedOutcome{}, false, fmt.Errorf(
				"blocked outcome: declared %d bytes exceeds %d", entry.Size, domain.MaxBlockedOutcomeBytes)
		}
		body, err := readBoundedEvidenceBlob(out.dir, domain.Digest(entry.Digest), int64(domain.MaxBlockedOutcomeBytes))
		if err != nil {
			return domain.BlockedOutcome{}, false, fmt.Errorf("blocked outcome: %w", err)
		}
		// The decisions are copied verbatim into item facts and synced to
		// clients, the way the specifier's decisions are; a credential-shaped
		// value fails the stage before any of that persists.
		if importer.ContainsSecret(body) {
			return domain.BlockedOutcome{}, false, errors.New("blocked outcome contains credential-shaped content")
		}
		blocked, err := domain.DecodeBlockedOutcome(body)
		if err != nil {
			return domain.BlockedOutcome{}, false, fmt.Errorf("blocked outcome: %w", err)
		}
		if blockedOutcomeContainsSecret(blocked) {
			return domain.BlockedOutcome{}, false, errors.New("blocked outcome contains credential-shaped content")
		}
		canonical, err := domain.EncodeBlockedOutcome(blocked)
		if err != nil {
			return domain.BlockedOutcome{}, false, fmt.Errorf("blocked outcome: %w", err)
		}
		if importer.ContainsSecret(canonical) {
			return domain.BlockedOutcome{}, false, errors.New("blocked outcome contains credential-shaped content")
		}
		return blocked, true, nil
	}
	return domain.BlockedOutcome{}, false, nil
}

// Scan decoded card text separately because JSON string escaping can hide
// credential delimiters from both the raw and canonical document scans.
func blockedOutcomeContainsSecret(blocked domain.BlockedOutcome) bool {
	for _, decision := range blocked.Decisions {
		if importer.ContainsSecret([]byte(decision.Question)) ||
			importer.ContainsSecret([]byte(decision.WhyBlocking)) ||
			importer.ContainsSecret([]byte(decision.Recommendation)) {
			return true
		}
		for _, option := range decision.Options {
			if importer.ContainsSecret([]byte(option.Label)) ||
				importer.ContainsSecret([]byte(option.Tradeoffs)) {
				return true
			}
		}
	}
	return false
}

// readBoundedEvidenceBlob reads one released evidence blob of at most limit
// bytes and re-verifies its content address, refusing a file that runs past
// the limit before hashing it.
func readBoundedEvidenceBlob(dir string, digest domain.Digest, limit int64) ([]byte, error) {
	hexDigits, ok := contentaddr.Parse(string(digest))
	if !ok {
		return nil, fmt.Errorf("evidence digest %q is not canonical", digest)
	}
	path := filepath.Join(dir, export.EvidenceBlobsDirname, "sha256", hexDigits)
	f, err := os.Open(path) //nolint:gosec // G304: gate-released export path addressed by verified digest
	if err != nil {
		return nil, fmt.Errorf("read evidence blob %s: %w", digest, err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read evidence blob %s: %w", digest, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("evidence blob %s exceeds %d bytes", digest, limit)
	}
	if got := sha256.Sum256(body); hex.EncodeToString(got[:]) != hexDigits {
		return nil, fmt.Errorf("evidence blob %s does not match its digest", digest)
	}
	return body, nil
}

// finishBlocked persists a blocked export's evidence and claims and returns
// the blocked terminal. The write-once blocked outcome is recorded by the
// ordinary terminal commit, like every other non-export terminal, so the
// committed result keeps the evidence digests and usage the outcome record
// does not carry; the persisted evidence lands before either. No
// ExecutionExport exists for the invocation, which is what keeps every
// publication path closed.
func (d *Driver) finishBlocked(
	ctx context.Context, in intent, out exportOutcome, imported importer.Result,
	blocked domain.BlockedOutcome, usage []exec.UsageMeasurement,
) (exec.StageResult, error) {
	body, err := domain.EncodeBlockedOutcome(blocked)
	if err != nil {
		return exec.StageResult{}, err
	}
	canonical := &evidenceNormalization{
		label:  export.BlockedEvidenceLabel,
		digest: domain.Digest(contentaddr.Sum(body)), body: body,
	}
	artifacts, err := d.persistEvidence(ctx, in, out, imported.Claims, canonical)
	if err != nil {
		return exec.StageResult{}, err
	}
	return exec.StageResult{
		InvocationID: in.InvocationID, Status: exec.StatusBlocked,
		Artifacts: artifacts, Usage: usage,
		Summary: exec.TruncateSummary(blocked.Decisions[0].Question),
	}, nil
}

func (d *Driver) extractUsage(
	in intent, out exportOutcome, observedAt time.Time,
) []exec.UsageMeasurement {
	if !out.evidencePresent {
		return nil
	}
	extractor, ok := d.provider.(UsageExtractor)
	if !ok {
		return nil
	}
	usage, err := extractor.ExtractUsage(
		filepath.Join(out.dir, export.EvidenceBlobsDirname), out.evidence, observedAt,
	)
	if err != nil {
		d.logger.Warn("usage extraction failed; continuing without measurements",
			"invocation", string(in.InvocationID), "error", err)
		return nil
	}
	return usage
}

// completeReleasedExport owns the cleanup decision after the current-policy
// import phase begins. A successful import or a conclusive trust-boundary
// rejection consumes the directory. Operational and ambiguous failures retain
// it and advertise the retryable recovery class, including failures after a
// possibly successful ExecutionExport write.
func (d *Driver) completeReleasedExport(
	ctx context.Context, in intent, out exportOutcome,
) (exec.StageResult, error) {
	return d.completeReleasedExportWithOptions(ctx, in, out, d.authority.ImportOptions)
}

// completeReleasedExportFromAdmission imports a tree that was already
// released before a restart under the immutable admission-bound policy. The
// current policy still gates work that can start or resume, but cannot strand
// this terminal-only recovery path after the agent and gate have stopped.
func (d *Driver) completeReleasedExportFromAdmission(
	ctx context.Context, in intent, out exportOutcome,
) (exec.StageResult, error) {
	return d.completeReleasedExportWithOptions(ctx, in, out, d.authority.ImportOptionsRecord)
}

func (d *Driver) recordCurrentImportStart(ctx context.Context, in intent) error {
	return d.importStarts.RecordCurrentImportStart(ctx, domain.CurrentImportStart{
		InvocationID: in.InvocationID,
		AdmissionID:  in.Spec.AdmissionID,
	})
}

func (d *Driver) currentImportStarted(ctx context.Context, in intent) (bool, error) {
	start, found, err := d.importStarts.LookupCurrentImportStart(ctx, in.InvocationID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if start.InvocationID != in.InvocationID || start.AdmissionID != in.Spec.AdmissionID {
		return false, fmt.Errorf("current import start disagrees with intent %s: %w",
			in.InvocationID, domain.ErrParentKeyMismatch)
	}
	return true, nil
}

func (d *Driver) completeReleasedExportWithOptions(
	ctx context.Context, in intent, out exportOutcome, importOptions importOptionsProvider,
) (exec.StageResult, error) {
	if err := validateReleasedExport(d.exportRoot, in, out); err != nil {
		return classifyExportCompletion(out.dir, exec.StageResult{}, err)
	}
	// The authenticated evidence is the usage source, so observe it before any
	// candidate validation can reject the returned workspace. The terminal
	// result persists this timestamp and makes later collection replay exact.
	usage := d.extractUsage(in, out, d.now().UTC())
	result, err := d.finish(ctx, in, out, importOptions, usage)
	if err != nil {
		result.Usage = usage
	}
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
		return result, err
	}
	if errors.Is(err, errDefinitiveExportRejection) {
		removeReleasedExport(dir)
		return result, err
	}
	return result, fmt.Errorf("%w: finish released export: %w",
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
		errors.Is(err, importer.ErrUnexpectedChanges) ||
		errors.Is(err, importer.ErrGitPathInjection) ||
		errors.Is(err, importer.ErrPathConflict) ||
		errors.Is(err, importer.ErrOrphanBlob) ||
		errors.Is(err, importer.ErrDigestMismatch) ||
		errors.Is(err, importer.ErrSizeMismatch) ||
		errors.Is(err, importer.ErrBlobTooLarge)
}

// validateImportFindings decides the released import's fate under the stage's
// finding profile. Under the default publish-strict profile every finding is
// fatal, exactly as before; the specification profile tolerates the
// workspace-debris classes because it publishes a typed specification, never
// workspace content. A fatal finding returns a definitiveRejection carrying the
// diagnostic sample; the ExportRejection itself is recorded later, after the
// failed outcome commits (see definitiveRejection), so this function performs
// no store write and cannot fail on one. expectCommit is false only for a
// blocked terminal, which by contract carries no candidate.
func (d *Driver) validateImportFindings(
	in intent, imported importer.Result, profile *importer.FindingProfile, expectCommit bool,
) error {
	fatal, tolerated := partitionFindings(imported.Findings, profile)
	if len(fatal) > 0 {
		return newDefinitiveRejection(fatal, tolerated)
	}
	if len(tolerated) > 0 {
		d.logger.Info("import tolerated non-fatal findings",
			"invocation", string(in.InvocationID), "profile", profileLabel(profile),
			"tolerated", len(tolerated))
	}
	if expectCommit && imported.CommitSHA == "" {
		return fmt.Errorf("%w: gauntlet containment withheld a candidate commit",
			errDefinitiveExportRejection)
	}
	return nil
}

// profileLabel names a possibly-absent finding profile for a log line; a nil
// profile is the default publish-strict behavior.
func profileLabel(profile *importer.FindingProfile) string {
	if profile == nil {
		return string(importer.FindingProfilePublishStrict)
	}
	return string(*profile)
}

// partitionFindings splits a result's findings into the definitively fatal and
// the tolerated under the given profile. Both slices preserve the importer's
// order.
func partitionFindings(
	findings []importer.Finding, profile *importer.FindingProfile,
) (fatal, tolerated []importer.Finding) {
	for _, f := range findings {
		if f.Fatal(profile) {
			fatal = append(fatal, f)
		} else {
			tolerated = append(tolerated, f)
		}
	}
	return fatal, tolerated
}

// maxPersistedRejectionFindings caps how many findings the durable rejection
// record retains. The findings are candidate-controlled (kinds and long paths
// an adversarial workspace can flood), and the record is permanent and copied
// into every backup checkpoint, so an uncapped body would let one rejected
// attempt bloat the control-plane database. The record keeps this many findings
// as a diagnostic sample plus the true total; the failed-outcome summary and
// the log line already carry the blocking and total counts.
const maxPersistedRejectionFindings = 100

// newDefinitiveRejection builds the rejection carrying a bounded, fatal-first
// diagnostic sample. Fatal findings lead so the true cause is never crowded out
// of the capped sample by tolerated debris — which a candidate-controlled
// workspace can flood — while the total spans every reported finding.
func newDefinitiveRejection(fatal, tolerated []importer.Finding) *definitiveRejection {
	total := len(fatal) + len(tolerated)
	sample := make([]importer.Finding, 0, min(total, maxPersistedRejectionFindings))
	for _, group := range [][]importer.Finding{fatal, tolerated} {
		for _, f := range group {
			if len(sample) == maxPersistedRejectionFindings {
				break
			}
			sample = append(sample, f)
		}
	}
	return &definitiveRejection{
		findings: domainRejectionFindings(sample), fatal: len(fatal), total: total,
	}
}

// recordRejectionDetail writes the diagnostic ExportRejection after the failed
// outcome is already durable. It is best-effort: the ExecutionOutcome is the
// authoritative terminal, and losing this row to a crash or a write error costs
// only the per-finding detail, never correctness. The record is keyed by the
// Start-pinned instant so a retried write converges. Per-finding paths reach
// the durable record and the error-level log line here, never the client-facing
// outcome summary (issue #768).
func (d *Driver) recordRejectionDetail(ctx context.Context, in intent, rej *definitiveRejection) {
	rejection, err := domain.NewExportRejection(domain.ExportRejectionInput{
		InvocationID:  in.InvocationID,
		AdmissionID:   in.Spec.AdmissionID,
		Findings:      rej.findings,
		TotalFindings: rej.total,
		RecordedAt:    in.RecordedAt,
	})
	if err != nil {
		d.logger.Error("build export rejection detail",
			"invocation", string(in.InvocationID), "error", err)
		return
	}
	if err := d.outcomes.RecordExportRejection(ctx, rejection); err != nil {
		d.logger.Error("record export rejection detail",
			"invocation", string(in.InvocationID), "error", err)
		return
	}
	d.logger.Error("released export definitively rejected by gauntlet containment",
		"invocation", string(in.InvocationID), "findings", rej.total,
		"detail", rejectionDetail(rejection.Findings))
}

// domainRejectionFindings lifts import findings into the durable record's flat
// shape. Path and PathHex are mutually exclusive in a manifest entry, so the
// lift preserves that exclusivity the domain Validate requires.
func domainRejectionFindings(findings []importer.Finding) []domain.ExportRejectionFinding {
	out := make([]domain.ExportRejectionFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, domain.ExportRejectionFinding{
			Kind: string(f.Kind), Path: f.Path, PathHex: f.PathHex,
			Rule: f.Rule, Line: f.Line, Detail: f.Detail,
		})
	}
	return out
}

// maxSummaryFindings bounds how many kind:path pairs the rejection summary
// folds in; the durable record holds all of them, and the summary column is
// capped anyway (truncateSummary).
const maxSummaryFindings = 5

// persistsRepositoryChannel reports whether this import's repo-channel blobs
// must enter durable storage. The specification profile publishes
// a typed result and never publishes the repo commit, so those blobs are
// vestigial: skipping them keeps unscanned tolerated content (a
// secret_scan_skipped blob most sharply) out of the daemon CAS, which
// publish-strict never admits because it rejects that finding outright. Every
// other profile may publish, so its repo blobs must be resolvable for the
// production replay that reconstructs the commit. This is safe only while no
// path builds a publication replay from a specification export: the specification
// arm of engine.RecordExecutionExport records the export export-only and mints
// no publication task, and backup artifact closure pulls repo entry blobs only
// through such a task (issue #768).
func persistsRepositoryChannel(profile *importer.FindingProfile) bool {
	return profile == nil || *profile != importer.FindingProfileSpecification
}

func (d *Driver) persistReleasedMaterial(
	ctx context.Context,
	in intent,
	out exportOutcome,
	record domain.ExecutionExport,
	claims []domain.AgentClaim,
	persistRepositoryChannel bool,
) ([]domain.Digest, executionReplay, error) {
	if err := d.persistManifests(ctx, out, record); err != nil {
		return nil, executionReplay{}, err
	}
	if persistRepositoryChannel {
		if err := d.persistRepositoryBlobs(ctx, out); err != nil {
			return nil, executionReplay{}, err
		}
	}
	// Evidence lands before the export record: the released blobs live only
	// under this directory, and a durable row is an assertion that every
	// object it implies can already be resolved.
	artifacts, err := d.persistEvidence(ctx, in, out, claims, nil)
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
// window. It advances no lifecycle phase: it only enriches an exported
// preterminal intent with independently re-auditable replay data before the
// authoritative export row is allowed to commit.
func (d *Driver) recordExecutionReplay(in intent, replay executionReplay) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if (in.Phase != phaseExported && in.Phase != phaseImportPending) || in.Export == nil {
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
	return domain.Digest(contentaddr.Format(sum[:]))
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
	if got := domain.Digest(contentaddr.Format(sum[:])); got != digest {
		return nil, fmt.Errorf("%s hashes to %s, record names %s", filepath.Base(path), got, digest)
	}
	return body, nil
}

type evidenceNormalization struct {
	label  string
	digest domain.Digest
	body   []byte
}

// persistEvidence copies each released evidence blob into durable storage
// and records the importer's agent claims, returning the digests the result
// may safely name. A claim whose blob is missing from the export fails the
// stage rather than being recorded unresolvable. normalized replaces one
// already-validated source with its canonical durable representation.
func (d *Driver) persistEvidence(
	ctx context.Context, in intent, out exportOutcome, claims []domain.AgentClaim,
	normalized *evidenceNormalization,
) ([]domain.Digest, error) {
	if !out.evidencePresent {
		if len(claims) > 0 {
			return nil, fmt.Errorf("import returned %d claims with no evidence channel", len(claims))
		}
		return nil, nil
	}
	if normalized != nil {
		entries := 0
		for _, entry := range out.evidence.Entries {
			if entry.Label == normalized.label {
				entries++
			}
		}
		if entries != 1 {
			return nil, fmt.Errorf("normalize evidence %q: found %d manifest entries",
				normalized.label, entries)
		}
		claims = append([]domain.AgentClaim(nil), claims...)
		normalizedClaims := 0
		for index := range claims {
			if claims[index].Label != normalized.label {
				continue
			}
			claims[index].Digest = normalized.digest
			claims[index].Metadata.SizeBytes = int64(len(normalized.body))
			normalizedClaims++
		}
		if normalizedClaims != 1 {
			return nil, fmt.Errorf("normalize evidence %q: found %d claims",
				normalized.label, normalizedClaims)
		}
	}
	digests := make([]domain.Digest, 0, len(out.evidence.Entries))
	for _, entry := range out.evidence.Entries {
		digest := domain.Digest(entry.Digest)
		body, err := readEvidenceBlob(out.dir, digest)
		if err != nil {
			return nil, err
		}
		if normalized != nil && entry.Label == normalized.label {
			digest = normalized.digest
			body = normalized.body
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

// errSeedCleanupAfterClose marks terminal seed cleanup that could not run
// because the driver's seed root is already closed. It is a benign shutdown
// race, not an undeletable checkout: an in-flight terminal Inspect/Collect
// reached cleanup after Close, so the seed is deferred to the next process.
// reportTerminalSeedCleanup suppresses it rather than raising a false failure.
var errSeedCleanupAfterClose = errors.New("terminal seed cleanup after driver close")

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
	case phaseSeeding, phaseRunning, phaseExported, phaseImportPending:
		return fmt.Errorf("refuse seed cleanup for nonterminal invocation %s in phase %q",
			in.InvocationID, in.Phase)
	}
	d.seedMu.Lock()
	defer d.seedMu.Unlock()
	if d.seedFS == nil {
		return errSeedCleanupAfterClose
	}
	runID, err := d.validatedProviderRunID(in.InvocationID)
	if err != nil {
		return err
	}
	if runID != in.RunID {
		return fmt.Errorf(
			"%w: refuse seed cleanup for invocation %s run %q, derivation gives %q",
			ErrUnsupportedStart, in.InvocationID, in.RunID, runID,
		)
	}
	for _, name := range []string{runID, runID + "-import"} {
		if err := d.seedFS.RemoveAll(name); err != nil {
			// Name the root-relative target, never filepath.Join(d.seedRoot,
			// name). The removal goes through the fd-pinned seedFS, but
			// d.seedRoot is a mutable pathname: were the seed root renamed and
			// replaced by a symlink, the joined path would resolve through it to
			// an unrelated outside checkout and misdirect operator remediation at
			// a tree cleanup never touched. The root-relative name cannot be
			// redirected.
			return fmt.Errorf("%w: remove terminal seed %s: %w",
				ErrRecoveryRetryable, name, err)
		}
	}
	return nil
}

// reportTerminalSeedCleanup runs best-effort terminal seed cleanup and reports
// a failure instead of discarding it. Terminal commits must not block on
// cleanup (the seed root would otherwise accumulate full checkouts with no
// operational signal), so the error is logged, never returned.
//
// Inspect and Collect are idempotent terminal reads the engine calls
// repeatedly, and each re-attempts cleanup; the retry is deliberate so a
// transient failure still converges, but the warning is deduplicated by the
// failing error's identity so one undeletable checkout cannot flood operator
// logs. Keying on the error rather than the invocation keeps each distinct
// unresolved checkout reported once: cleanup stops at the first undeletable
// name, so when an operator repairs it the sibling target's failure is a new
// error and is surfaced instead of being silently suppressed.
func (d *Driver) reportTerminalSeedCleanup(in intent) {
	err := d.cleanupTerminalSeed(in)
	if errors.Is(err, errSeedCleanupAfterClose) {
		// Benign shutdown race: cleanup could not run because the seed root is
		// already closed, so the seed is deferred to the next process rather
		// than leaked. Pre-close this error was discarded; do not raise a
		// false undeletable-checkout warning, and leave the dedup set alone.
		return
	}
	d.seedMu.Lock()
	defer d.seedMu.Unlock()
	if err == nil {
		delete(d.seedCleanupWarned, in.InvocationID)
		return
	}
	if d.seedCleanupWarned[in.InvocationID] == err.Error() {
		return
	}
	d.seedCleanupWarned[in.InvocationID] = err.Error()
	d.logger.Warn("terminal seed cleanup failed",
		"invocation", string(in.InvocationID), "run", in.RunID, "error", err)
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
		d.reportTerminalSeedCleanup(in)
		return true, nil
	}
	if result.Status != exec.StatusCompleted && in.Phase == phaseRunning {
		// Handoff errors do not prove teardown succeeded. The writer may still
		// hold the credential volume, so only ward recovery may turn this phase
		// into an explicit export or loss outcome.
		return false, nil
	}
	if result.Status != exec.StatusCompleted {
		if len(result.Usage) > 0 {
			in.PendingUsage = slices.Clone(result.Usage)
			if err := d.saveIntent(in); err != nil {
				return false, err
			}
		}
		if err := d.recordOutcome(context.Background(), in, result); err != nil {
			return false, err
		}
	}
	in.Phase = phaseCommitted
	in.Result = &result
	in.PendingUsage = nil
	if err := d.saveIntent(in); err != nil {
		return false, err
	}
	d.reportTerminalSeedCleanup(in)
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
		d.reportTerminalSeedCleanup(in)
		return nil
	case phaseRunning, phaseExported, phaseImportPending:
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
	d.reportTerminalSeedCleanup(in)
	return nil
}

func outcomeStatus(status exec.Status) (domain.ExecutionOutcomeStatus, error) {
	switch status {
	case exec.StatusFailed:
		return domain.ExecutionOutcomeFailed, nil
	case exec.StatusCanceled:
		return domain.ExecutionOutcomeCanceled, nil
	case exec.StatusBlocked:
		return domain.ExecutionOutcomeBlocked, nil
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
			d.reportTerminalSeedCleanup(in)
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
	case phaseExported, phaseImportPending:
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
		if err := d.recordCurrentImportStart(ctx, in); err != nil {
			return fmt.Errorf("%w: record recovered current-policy import authority: %w",
				ErrRecoveryRetryable, err)
		}
		if err := d.advance(&in, phaseImportPending, nil); err != nil {
			if retainErr := d.retainRecoveredIntent(in, err); retainErr != nil {
				return errors.Join(err, retainErr)
			}
			return fmt.Errorf("%w: record recovered current-policy import: %w",
				ErrRecoveryRetryable, err)
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
			// A re-derived definitive rejection routes through commitRejection,
			// the same terminal path as the live pipeline and recoverExported, so
			// its diagnostic detail is written beside the outcome rather than
			// dropped by a generic commitResult (the directory is already gone).
			var rej *definitiveRejection
			if errors.As(err, &rej) {
				if commitErr := d.commitRejection(ctx,
					d.logger.With("invocation", string(in.InvocationID), "run", in.RunID),
					in, status, rej, result.Usage); commitErr != nil {
					return fmt.Errorf("%w: commit recovery result: %w",
						ErrRecoveryRetryable, commitErr)
				}
				return nil
			}
			if commitErr := d.commitResult(in.InvocationID, exec.StageResult{
				InvocationID: in.InvocationID, Status: status,
				Summary: truncateSummary(err.Error()), Usage: result.Usage,
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
		summary := fmt.Sprintf("%s writer exited with status %d.", d.displayName, recovered.FailureStatus)
		if recovered.FailureStatus == d.provider.PrepareFailedStatus() {
			summary = fmt.Sprintf(
				"Workspace preparation failed before the agent started (status %d): "+
					"the project-image hydration helper exited nonzero.",
				d.provider.PrepareFailedStatus())
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
			Summary:      d.displayName + " invocation canceled by daemon request.",
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
	d.reportTerminalSeedCleanup(in)
	return nil
}

// fileDigest is the content address of one released manifest file.
func fileDigest(path string) (domain.Digest, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: gate-released export path this driver just received
	if err != nil {
		return "", fmt.Errorf("digest %s: %w", filepath.Base(path), err)
	}
	sum := sha256.Sum256(body)
	return domain.Digest(contentaddr.Format(sum[:])), nil
}

// maxSummaryBytes is exec.MaxSummaryBytes, the bound every terminal summary
// this driver commits is held to.
const maxSummaryBytes = exec.MaxSummaryBytes

// truncateSummary bounds a terminal summary; see exec.TruncateSummary.
func truncateSummary(s string) string {
	return exec.TruncateSummary(s)
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
// expendable source directory. Without a terminal record, the trusted import-
// start marker chooses current policy; its absence preserves admission-bound
// legacy and crash-only recovery. The private phase never chooses policy.
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
	// Adopt any already-written non-export terminal before deciding anything
	// else. The live pipeline (or a prior recovery pass) can durably record a
	// failed outcome — a definitive rejection commits one — or a canceled one
	// under daemon shutdown, before its intent phase commits. Converging on that
	// authoritative outcome here is what keeps the write-once record from making
	// recovery retry forever; re-importing a surviving directory instead could
	// derive a second, conflicting outcome.
	if outcome, outcomeFound, outcomeErr := d.outcomes.LookupExecutionOutcome(
		ctx, in.InvocationID,
	); outcomeErr != nil {
		return fmt.Errorf("%w: lookup execution outcome: %w", ErrRecoveryRetryable, outcomeErr)
	} else if outcomeFound {
		if err := d.adoptRecordedOutcome(ctx, in, outcome); err != nil {
			return err
		}
		removeReleasedExport(out.dir)
		return nil
	}
	// No terminal outcome is recorded. A surviving directory is replayed through
	// finish(); a gone directory means nothing durable was decided.
	if _, err := os.Stat(out.dir); err == nil {
		var result exec.StageResult
		var err error
		current, authorityErr := d.currentImportStarted(ctx, in)
		if authorityErr != nil {
			return fmt.Errorf("%w: authenticate current import start: %w",
				ErrRecoveryRetryable, authorityErr)
		}
		if current {
			result, err = d.completeReleasedExport(ctx, in, out)
		} else {
			result, err = d.completeReleasedExportFromAdmission(ctx, in, out)
		}
		if err != nil {
			// A re-derived definitive rejection is a normal terminal, not a
			// recovery failure. Commit it through the same terminal path as the
			// live pipeline — the clean count-only summary and the best-effort
			// diagnostic detail — rather than letting reconcileIntent's generic
			// catch-all record it as a "recovery failed:" outcome without detail.
			var rej *definitiveRejection
			if errors.As(err, &rej) {
				if commitErr := d.commitRejection(ctx,
					d.logger.With("invocation", string(in.InvocationID), "run", in.RunID),
					in, exec.StatusFailed, rej, result.Usage); commitErr != nil {
					// No live session retains a failed recovery write, so propagate
					// it as retryable instead of returning success and letting the
					// next pass record the rejected attempt as lost.
					return fmt.Errorf("%w: commit recovered rejection: %w",
						ErrRecoveryRetryable, commitErr)
				}
				return nil
			}
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
		// A read or reconstruction error says nothing about presence. Leave the
		// exported intent retryable rather than turning a transient failure into
		// permanent loss.
		return fmt.Errorf("%w: lookup execution export: %w",
			ErrRecoveryRetryable, lookupErr)
	}
	// Neither a durable export record, a terminal outcome, a rejection, nor the
	// released directory survived, so nothing was published and the attempt is
	// rerun-safe.
	return d.commitLost(ctx, in.InvocationID)
}

// adoptRecordedOutcome commits the intent phase to match a terminal outcome
// that was already durably written, so a crash between the outcome write and
// its phase commit converges on the authoritative record instead of
// resynthesizing (and conflicting with) it. A lost outcome routes through
// commitLost, which owns the phaseLost transition.
func (d *Driver) adoptRecordedOutcome(
	ctx context.Context, in intent, outcome domain.ExecutionOutcome,
) error {
	switch outcome.Status {
	case domain.ExecutionOutcomeLost:
		return d.commitLost(ctx, in.InvocationID)
	case domain.ExecutionOutcomeFailed, domain.ExecutionOutcomeCanceled, domain.ExecutionOutcomeBlocked:
		status := exec.StatusFailed
		switch outcome.Status {
		case domain.ExecutionOutcomeCanceled:
			status = exec.StatusCanceled
		case domain.ExecutionOutcomeBlocked:
			status = exec.StatusBlocked
		case domain.ExecutionOutcomeFailed, domain.ExecutionOutcomeLost:
		}
		if err := d.commitResult(in.InvocationID, exec.StageResult{
			InvocationID: in.InvocationID, Status: status, Summary: outcome.Summary,
		}); err != nil {
			return fmt.Errorf("%w: adopt recorded outcome: %w", ErrRecoveryRetryable, err)
		}
		return nil
	}
	return fmt.Errorf("%w: recorded outcome %q for %s has no stage status",
		errExportAuthorityConflict, outcome.Status, in.InvocationID)
}

// rejectionDetail renders the leading kind:path pairs of a rejection's
// diagnostic sample for the error-level log line, naming the invalid-path hex
// when a finding carries no canonical path. The paths stay in the log and the
// daemon-internal record, never the client-facing outcome summary.
func rejectionDetail(findings []domain.ExportRejectionFinding) string {
	var b strings.Builder
	for i, f := range findings {
		if i == maxSummaryFindings {
			fmt.Fprintf(&b, " (+%d more)", len(findings)-maxSummaryFindings)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		loc := f.Path
		if loc == "" {
			loc = "hex:" + f.PathHex
		}
		fmt.Fprintf(&b, "%s:%s", f.Kind, loc)
	}
	return b.String()
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
