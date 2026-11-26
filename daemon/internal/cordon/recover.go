package cordon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// Restart recovery (#3468). A launch's supervisor outlives the daemon, and
// everything recovery needs is in the run's directory, so a restarted
// daemon adopts a run by its ID alone. Each method here mirrors a stage.Gate
// method (Recover, HandoffStarted, RequestCancellation,
// ReleaseFinishedHandoffs) so the driver port can wrap it. Nothing here
// reads a path from a record: the run's directory follows from RunsRoot and
// the run ID, and a process is signaled or waited on only while its pid
// still has the start time its claim names.

var (
	// ErrNoRun means the run has no directory: it was never claimed, or it
	// was released.
	ErrNoRun = errors.New("cordon: no such run")
	// ErrDetached is a Launch or Recover that returned because Detach ran,
	// or that Detach refused. The run, if any, keeps running.
	ErrDetached = errors.New("cordon: the backend is detached")
)

// RecoveryOutcome is how Recover found a run once it ended.
type RecoveryOutcome string

const (
	// RecoveryExported means the run exited cleanly and is exported.
	RecoveryExported RecoveryOutcome = "exported"
	// RecoveryFailed means the agent exited nonzero or stalled.
	RecoveryFailed RecoveryOutcome = "failed"
	// RecoveryCanceled means the run's cancellation was requested.
	RecoveryCanceled RecoveryOutcome = "canceled"
	// RecoveryLost means no supervisor recorded the run's end, so recovery
	// ended it once the run-end proof passed.
	RecoveryLost RecoveryOutcome = "lost"
)

// AllRecoveryOutcomes lists every valid RecoveryOutcome; it is the single
// registration point.
var AllRecoveryOutcomes = []RecoveryOutcome{RecoveryExported, RecoveryFailed, RecoveryCanceled, RecoveryLost}

func (o RecoveryOutcome) valid() bool {
	switch o {
	case RecoveryExported, RecoveryFailed, RecoveryCanceled, RecoveryLost:
		return true
	default:
		return false
	}
}

// RecoverRequest names the run to recover and re-supplies its export, as
// the driver re-supplies a warded run's spec from its durable admission.
type RecoverRequest struct {
	RunID  string
	Export ExportRequest
}

// RecoveryResult is a recovered run's end.
type RecoveryResult struct {
	Outcome  RecoveryOutcome
	EndedBy  EndedBy
	ExitCode int
	// Reason says why a lost run is lost.
	Reason string
	// Export is the run's export, for RecoveryExported only.
	Export                     ExportResult
	TranscriptPath, StderrPath string
}

// Detach makes every waiting Launch and Recover return ErrDetached at once,
// signaling nothing, and every later one refuse: the daemon is exiting and
// leaves its runs to their supervisors, for the next daemon to recover. A
// daemon shutting down calls it before it cancels any Launch's context,
// since that cancellation cancels the run for good.
func (b *Backend) Detach() {
	if b.detached != nil {
		b.detachOnce.Do(func() { close(b.detached) })
	}
}

func (b *Backend) attached() error {
	select {
	case <-b.detached:
		return ErrDetached
	default:
		return nil
	}
}

// Started reports whether runID's directory exists: a launch claims it
// before it starts anything.
func (b *Backend) Started(ctx context.Context, runID string) (bool, error) {
	_, err := b.existingRun(ctx, runID)
	if errors.Is(err, ErrNoRun) {
		return false, nil
	}
	return err == nil, err
}

// existingRun returns runID's layout once its directory exists.
func (b *Backend) existingRun(ctx context.Context, runID string) (layout, error) {
	if err := ctx.Err(); err != nil {
		return layout{}, err
	}
	if !plainRunID(runID) {
		return layout{}, fmt.Errorf("cordon: run id %q must be one plain path component", runID)
	}
	l := layoutFor(b.cfg.RunsRoot, runID)
	if _, err := os.Lstat(l.dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return layout{}, fmt.Errorf("%w: %s", ErrNoRun, runID)
		}
		return layout{}, fmt.Errorf("cordon: inspect %s: %w", l.dir, err)
	}
	return l, nil
}

// RequestCancellation makes runID end canceled and never export. It writes
// the durable marker first and then sends SIGTERM to the run's supervisor,
// if its claim names one still alive with its start time, which ends the
// agent as a canceled Launch does. A supervisor that claims later finds the
// marker after its claim, so one of the two always sees the request.
func (b *Backend) RequestCancellation(ctx context.Context, runID string) error {
	l, err := b.existingRun(ctx, runID)
	if err != nil {
		return err
	}
	err = writeRecord(l.cancel, cancelMarker{Version: cancelVersion, RunID: runID})
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	claim, err := readClaim(l, runID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || claim.Supervisor == nil {
		return err
	}
	host, err := b.newHost()
	if err != nil {
		return err
	}
	alive, err := host.same(claim.Supervisor.hostProcess())
	if err != nil || !alive {
		return err
	}
	return host.terminate(claim.Supervisor.PID)
}

// Recover finds how runID ended, waiting for it when its supervisor is
// still running, and finishes what a restart interrupted:
//
//   - An exported run returns its export once AuthenticateReleasedExport
//     accepts it.
//   - A run no supervisor claimed is claimed for recovery, so a supervisor
//     that starts later starts nothing, and ends lost. Every supervisor
//     claims before it starts anything, so nothing of the run ever ran and
//     no proof is needed; the same holds for a run recovery claimed before.
//   - A run whose claimed supervisor is gone without an end record ends
//     lost once the run-end proof, with a fresh canary under the run's rule
//     file, passes. It returns a *BlockedError when the proof fails.
//   - A run with an end record ends as the record says, a cancellation
//     marker outranking it. A clean exit is exported: an export directory
//     with no export record is what an interrupted export left, and it is
//     removed first.
//
// An error commits nothing and Recover may be called again. Export's
// refusals pass through unchanged, so the caller can tell one that will
// repeat (a *ScanRefusalError, ErrWorktreeReplaced, ErrNotExportable) from
// a transient failure. ErrCanceled can pass through too, when a
// cancellation is requested between the end record's read and the export:
// it means the run ended canceled, not that its export was refused.
// Canceling ctx stops the wait and leaves the run running;
// RequestCancellation is what cancels a run.
func (b *Backend) Recover(ctx context.Context, req RecoverRequest) (RecoveryResult, error) {
	if err := b.attached(); err != nil {
		return RecoveryResult{}, err
	}
	if req.Export.RunID != req.RunID {
		return RecoveryResult{}, fmt.Errorf("cordon: recovering %s with %s's export", req.RunID, req.Export.RunID)
	}
	if err := req.Export.validate(); err != nil {
		return RecoveryResult{}, err
	}
	l, err := b.existingRun(ctx, req.RunID)
	if err != nil {
		return RecoveryResult{}, err
	}
	if _, err := os.Lstat(l.exportRecord); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return RecoveryResult{}, fmt.Errorf("cordon: inspect %s: %w", l.exportRecord, err)
		}
		return b.recoveredExport(ctx, l, req.RunID)
	}
	host, err := b.newHost()
	if err != nil {
		return RecoveryResult{}, err
	}
	for {
		end, err := readEndRecord(l, req.RunID)
		if err == nil {
			return b.recoverEnded(ctx, l, req, end, "")
		}
		if !errors.Is(err, ErrNoEndRecord) {
			return RecoveryResult{}, err
		}
		claim, err := readClaim(l, req.RunID)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// A launch claims ctl/ before its supervisor can start, but a
			// daemon that died mid-claim may have left none.
			if err := os.Mkdir(l.ctl, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return RecoveryResult{}, fmt.Errorf("cordon: create %s: %w", l.ctl, err)
			}
			won, err := claimRun(l, req.RunID, nil)
			if err != nil {
				return RecoveryResult{}, err
			}
			if !won {
				continue // a supervisor claimed it meanwhile
			}
			return b.endLost(ctx, l, req, nil, "no supervisor started it")
		case err != nil:
			return RecoveryResult{}, err
		case claim.Supervisor == nil:
			return b.endLost(ctx, l, req, nil, "no supervisor started it")
		}
		sup := claim.Supervisor.hostProcess()
		alive, err := host.same(sup)
		if err != nil {
			return RecoveryResult{}, err
		}
		if !alive {
			return b.endLost(ctx, l, req, host, "its supervisor ended before it recorded an end")
		}
		gone := func() (bool, error) {
			alive, err := host.same(sup)
			return !alive, err
		}
		if _, err := b.awaitSupervisor(ctx, req.RunID, nil, gone, false); err != nil {
			return RecoveryResult{}, err
		}
	}
}

// endLost records a lost end for a run no supervisor will record one for.
// With host set, it first proves the run ended: a fresh canary under the
// run's rule file reads as a member as the original did, so the proof finds
// and ends whatever of the run is still alive.
func (b *Backend) endLost(ctx context.Context, l layout, req RecoverRequest, host hostProcesses, reason string) (RecoveryResult, error) {
	if host != nil {
		target := proofTarget{tag: l.tag, control: l.control}
		c, err := startCanary(b.start, host, &target,
			processSpec{argv: confinedArgv(b.cfg, l, canaryProgram), dir: l.runRoot, env: launchEnv(b.cfg, l)}, b.cfg.EndProofTimeout)
		if err == nil {
			err = endLaunch(context.WithoutCancel(ctx), host, target, b.cfg.EndProofTimeout)
			c.close()
		}
		if err != nil {
			return RecoveryResult{}, &BlockedError{RunID: req.RunID, EndedBy: EndedByLost, Err: err}
		}
	}
	err := writeEndRecord(l, endRecord{RunID: req.RunID, EndedBy: EndedByLost, ExitCode: -1})
	switch {
	case errors.Is(err, fs.ErrExist):
		// The supervisor recorded its end between this run's last read and
		// its exit; that record is the run's end.
		reason = ""
	case err != nil:
		return RecoveryResult{}, err
	}
	end, err := readEndRecord(l, req.RunID)
	if err != nil {
		return RecoveryResult{}, err
	}
	return b.recoverEnded(ctx, l, req, end, reason)
}

// recoverEnded maps a run's end record to its outcome, exporting a clean
// exit.
func (b *Backend) recoverEnded(ctx context.Context, l layout, req RecoverRequest, end endRecord, reason string) (RecoveryResult, error) {
	res := RecoveryResult{EndedBy: end.EndedBy, ExitCode: end.ExitCode, TranscriptPath: l.transcript, StderrPath: l.stderr}
	switch end.EndedBy {
	case EndedByCanceled:
		res.Outcome = RecoveryCanceled
		return res, nil
	case EndedByLost:
		res.Outcome, res.Reason = RecoveryLost, reason
		if reason == "" {
			res.Reason = "no supervisor recorded its end"
		}
		return res, nil
	case EndedByStalled:
		res.Outcome = RecoveryFailed
		return res, nil
	case EndedByExited:
		if end.ExitCode != 0 {
			res.Outcome = RecoveryFailed
			return res, nil
		}
		// Export claims l.export by creating it, and records the export
		// last, so a directory with no record is an interrupted export's.
		if err := os.RemoveAll(l.export); err != nil {
			return RecoveryResult{}, fmt.Errorf("cordon: remove the interrupted export: %w", err)
		}
		exp, err := b.Export(ctx, req.Export)
		if err != nil {
			return RecoveryResult{}, err
		}
		res.Outcome, res.Export = RecoveryExported, exp
		return res, nil
	}
	return RecoveryResult{}, fmt.Errorf("cordon: end record for %s has an invalid ending %q", req.RunID, end.EndedBy)
}

// recoveredExport returns a run's recorded export once it authenticates,
// read again from the directory Export wrote.
func (b *Backend) recoveredExport(ctx context.Context, l layout, runID string) (RecoveryResult, error) {
	if err := b.AuthenticateReleasedExport(ctx, runID, l.export); err != nil {
		return RecoveryResult{}, err
	}
	end, err := readEndRecord(l, runID)
	if err != nil {
		return RecoveryResult{}, err
	}
	res := RecoveryResult{
		Outcome: RecoveryExported, EndedBy: end.EndedBy, ExitCode: end.ExitCode,
		TranscriptPath: l.transcript, StderrPath: l.stderr,
		Export: ExportResult{Dir: l.export, BaseSHA: end.BaseSHA},
	}
	manifest, err := readMember(l.export, export.ManifestFilename)
	if err != nil {
		return RecoveryResult{}, err
	}
	if err := strictjson.Decode(manifest, &res.Export.Manifest, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		return RecoveryResult{}, fmt.Errorf("cordon: decode the exported manifest: %w", err)
	}
	evidence, err := readMember(l.export, export.EvidenceFilename)
	if err != nil {
		return RecoveryResult{}, err
	}
	if evidence != nil {
		res.Export.EvidencePresent = true
		if res.Export.Evidence, err = export.DecodeEvidenceManifest(evidence); err != nil {
			return RecoveryResult{}, fmt.Errorf("cordon: decode the exported evidence: %w", err)
		}
	}
	plan, err := openMember(l.export, export.CommitPlanFilename)
	if err != nil {
		return RecoveryResult{}, err
	}
	if plan != nil {
		res.Export.CommitPlanPresent = true
		_ = plan.Close()
	}
	return res, nil
}

// ReleaseFinished removes the directory of each finished run: one with an
// end record whose claimed supervisor isn't alive. A live run, a run with no
// end record, and a run already gone are left alone; Recover gives every
// run that will never get an end record one, so a caller releasing such a
// run recovers it first; a bad run ID or a
// failed removal is reported and leaves the directory, the safe direction.
// The caller guarantees each run is terminal, as stage.Gate's
// ReleaseFinishedHandoffs requires of its caller. os.RemoveAll follows no
// symlink the agent left in the worktree.
func (b *Backend) ReleaseFinished(ctx context.Context, runIDs []string) error {
	host, err := b.newHost()
	if err != nil {
		return err
	}
	var errs []error
	for _, runID := range runIDs {
		l, err := b.existingRun(ctx, runID)
		if errors.Is(err, ErrNoRun) {
			continue
		}
		if err == nil {
			err = b.releaseIfFinished(l, runID, host)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *Backend) releaseIfFinished(l layout, runID string, host hostProcesses) error {
	if _, err := readEndRecord(l, runID); err != nil {
		if errors.Is(err, ErrNoEndRecord) {
			return nil
		}
		return err
	}
	claim, err := readClaim(l, runID)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case claim.Supervisor != nil:
		alive, err := host.same(claim.Supervisor.hostProcess())
		if err != nil || alive {
			return err
		}
	}
	if err := os.RemoveAll(l.dir); err != nil {
		return fmt.Errorf("cordon: release %s: %w", runID, err)
	}
	return nil
}
