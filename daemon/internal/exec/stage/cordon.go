package stage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// This file holds the cordoned paths of the driver: a start whose
// exec.StartSpec.RunnerClass is cordoned_host_process runs as a host
// process under the cordon (CordonGate) instead of a ward handoff. The rest
// of the driver branches here at each point the two differ and shares
// everything after the release: import, the export record, the outcome,
// and failure evidence.
//
// The driver trusts the port as it trusts Gate, and checks what the
// provider returns against the intent before the port sees it.

// runnerClassOf reports whether spec runs cordoned. Empty is the ward's
// class and exactly cordoned_host_process is the cordon's. Any other value
// is refused: a journaled spec's class isn't validated on its own, so the
// driver never reads an unknown one as either.
func runnerClassOf(spec exec.StartSpec) (cordoned bool, err error) {
	if spec.RunnerClass == "" {
		return false, nil
	}
	if spec.RunnerClass == domain.BackendCordonedHostProcess {
		return true, nil
	}
	return false, fmt.Errorf("%w: runner class %q has no stage port", ErrUnsupportedStart, spec.RunnerClass)
}

// egressFitsClass reports whether an intent's egress profile is one its
// runner class can run under.
func egressFitsClass(cordoned bool, profile domain.EgressProfile) bool {
	if cordoned {
		return profile == domain.EgressHostOpen
	}
	return profile == domain.EgressProviderOnly || profile == domain.EgressProviderRegistry
}

// authenticateCordonExport authenticates a cordoned run's released export,
// whose path validateReleasedExportPath has already bound to this run.
// Unlike ward, which compares the path with a journal row that outlives the
// directory, the cordon hashes the directory's members against an export
// record in the run's directory. The driver removes the export once its
// outcome is durable and ReleaseFinished removes the run with its record,
// yet a recovered or committed intent keeps its Export and is re-gated on
// every load. So:
//
//   - A terminal intent's durable export record or outcome is its
//     authority, which the regate checks next, and nothing reads or removes
//     its export directory; it needs no composed cordon, so a later
//     configuration change leaves its history readable.
//   - Otherwise a cordon refusal stands unless the directory is gone, when
//     nothing is left to read, or a durable export record already exists,
//     which the regate binds to this export next, so a crash partway
//     through the removal can't strand an import that already finished.
func (d *Driver) authenticateCordonExport(ctx context.Context, in intent, dir string) error {
	if in.Phase == phaseCommitted || in.Phase == phaseLost {
		return nil
	}
	port, err := d.cordonPort()
	if err != nil {
		return err
	}
	err = port.AuthenticateReleasedExport(ctx, in.RunID, dir)
	if err == nil {
		return nil
	}
	if _, statErr := os.Lstat(dir); errors.Is(statErr, fs.ErrNotExist) {
		return nil
	}
	_, recorded, lookupErr := d.exports.LookupExecutionExportRecord(ctx, in.InvocationID)
	if lookupErr != nil {
		// Not a refusal: classifyReleasedExportAuthentication retries it.
		return fmt.Errorf("look up the export record of %s: %w", in.InvocationID, lookupErr)
	}
	if recorded {
		return nil
	}
	return err
}

// cordonPort is the composed cordon, refusing when none is.
func (d *Driver) cordonPort() (CordonGate, error) {
	if d.cordon == nil {
		return nil, fmt.Errorf("%w: no cordon is composed for a cordoned start", ErrUnsupportedStart)
	}
	return d.cordon, nil
}

// cordonLaunch builds the intent's launch and export request through the
// provider and holds them to the intent, as handoffSpec holds a ward
// handoff. Start, the pipeline, and recovery all build it, so every Recover
// declares the export the launch did. Every refusal wraps
// ErrUnsupportedStart.
func (d *Driver) cordonLaunch(ctx context.Context, in intent) (CordonLaunch, CordonExportRequest, error) {
	if _, err := d.cordonPort(); err != nil {
		return CordonLaunch{}, CordonExportRequest{}, err
	}
	provider, ok := d.provider.(CordonLaunchProvider)
	if !ok {
		return CordonLaunch{}, CordonExportRequest{}, fmt.Errorf(
			"%w: the provider can't build a cordoned launch", ErrUnsupportedStart)
	}
	// Stage-input attachments reach a warded writer through ward's input
	// volume; the cordon has no such delivery yet, so a start that names
	// any is refused rather than run without them.
	if len(InputFilesFor(in.Spec)) != 0 {
		return CordonLaunch{}, CordonExportRequest{}, fmt.Errorf(
			"%w: a cordoned start can't deliver stage-input attachments", ErrUnsupportedStart)
	}
	if len(in.Preparation) != 0 {
		return CordonLaunch{}, CordonExportRequest{}, fmt.Errorf(
			"%w: a cordoned start records no workspace preparation", ErrUnsupportedStart)
	}
	launch, req, err := provider.CordonLaunch(ctx, providerHandoffInputFrom(in))
	if err != nil {
		return CordonLaunch{}, CordonExportRequest{}, err
	}
	// Detach before checking, so a reference the provider keeps can't
	// change what was checked.
	launch.Prompt = slices.Clone(launch.Prompt)
	launch.Args = slices.Clone(launch.Args)
	req.Sources = slices.Clone(req.Sources)
	switch {
	case launch.InvocationID != in.InvocationID:
		return CordonLaunch{}, CordonExportRequest{}, fmt.Errorf(
			"%w: cordoned launch invocation differs from the durable input", ErrUnsupportedStart)
	case launch.RunID != in.RunID || req.RunID != in.RunID:
		return CordonLaunch{}, CordonExportRequest{}, fmt.Errorf(
			"%w: cordoned launch run ID differs from the durable input", ErrUnsupportedStart)
	case launch.Base != in.Spec.Base:
		return CordonLaunch{}, CordonExportRequest{}, fmt.Errorf(
			"%w: cordoned launch base differs from the durable input", ErrUnsupportedStart)
	case !bytes.Equal(launch.Prompt, []byte(in.Prompt)):
		return CordonLaunch{}, CordonExportRequest{}, fmt.Errorf(
			"%w: cordoned launch prompt differs from durable input", ErrUnsupportedStart)
	case slices.ContainsFunc(launch.Args, settingsArg):
		// The cordon fixes the CLI's settings and refuses a second set; a
		// refusal here leaves no intent instead of failing the launch.
		return CordonLaunch{}, CordonExportRequest{}, fmt.Errorf(
			"%w: cordoned launch arguments set the CLI's settings", ErrUnsupportedStart)
	case req.Transcript.ProducerInvocationID != string(in.InvocationID):
		return CordonLaunch{}, CordonExportRequest{}, fmt.Errorf(
			"%w: cordoned transcript names another producer", ErrUnsupportedStart)
	case slices.ContainsFunc(req.Sources, func(s export.EvidenceSource) bool {
		return s.ProducerInvocationID != string(in.InvocationID)
	}):
		return CordonLaunch{}, CordonExportRequest{}, fmt.Errorf(
			"%w: cordoned evidence source names another producer", ErrUnsupportedStart)
	}
	return launch, req, nil
}

func settingsArg(arg string) bool {
	return arg == "--settings" || strings.HasPrefix(arg, "--settings=")
}

// cordonHandoff is handoffAndImport for a cordoned intent. The cordon seeds
// its own worktree, so nothing is seeded here. The run gets no stall hook
// and no live transcript (non-goals of #3584).
func (d *Driver) cordonHandoff(ctx context.Context, in intent) (exec.StageResult, error) {
	launch, req, err := d.cordonLaunch(ctx, in)
	if err != nil {
		// Start built the same launch from the same intent before saving
		// it, so only a composition change since (no cordon composed now)
		// gets here. Nothing ran: a refusal fails the attempt, as a warded
		// handoff spec refusal does, and any other error leaves the
		// seeding phase rerunnable.
		if !errors.Is(err, ErrUnsupportedStart) {
			return exec.StageResult{}, fmt.Errorf("%w: rebuild cordoned launch: %w", ErrRecoveryRetryable, err)
		}
		return exec.StageResult{}, handoffSpecRefusal{err}
	}
	// From here on the run's directory may exist, and only the cordon's
	// recovery may say how it ended.
	if err := d.advance(&in, phaseRunning, nil); err != nil {
		return exec.StageResult{}, err
	}
	end, err := d.cordon.Launch(ctx, launch)
	if err != nil {
		return d.cordonLaunchFailed(ctx, in, err)
	}
	if !end.EndedBy.valid() {
		return exec.StageResult{}, fmt.Errorf("%w: cordon reported an invalid end %q", ErrRecoveryRetryable, end.EndedBy)
	}
	switch end.EndedBy {
	case CordonEndedByExited:
		if end.ExitCode != 0 {
			return d.cordonFailedResult(ctx, in, end)
		}
	case CordonEndedByStalled:
		return d.cordonFailedResult(ctx, in, end)
	case CordonEndedByCanceled:
		return d.cordonCanceledResult(in), nil
	case CordonEndedByLost:
		// Only recovery ends a run lost.
		return exec.StageResult{}, fmt.Errorf("%w: a cordoned launch reported a lost end", ErrRecoveryRetryable)
	}
	exp, err := d.cordon.Export(ctx, req)
	if err != nil {
		var refusal *CordonExportRefusal
		if errors.As(err, &refusal) {
			return d.cordonExportRefusedResult(in, refusal), nil
		}
		// The run has a clean, proven end, so recovery exports it again.
		return exec.StageResult{}, fmt.Errorf("%w: export cordoned run: %w", ErrRecoveryRetryable, err)
	}
	return d.importReleased(ctx, in, cordonOutcome(exp))
}

// cordonLaunchFailed decides what a Launch error leaves. A detached or
// unproven run is still the cordon's, and so is any run whose directory
// exists: recovery waits for its end, ending it lost once that end is
// proven if no supervisor records one. A launch refused before it claimed
// a directory started nothing, so it ends here; its error is logged in
// full, and the outcome carries a fixed summary because a launch error can
// name a path from the run.
func (d *Driver) cordonLaunchFailed(ctx context.Context, in intent, err error) (exec.StageResult, error) {
	if errors.Is(err, ErrCordonDetached) || errors.Is(err, ErrCordonEndUnproven) {
		return exec.StageResult{}, fmt.Errorf("%w: cordoned launch: %w", ErrRecoveryRetryable, err)
	}
	started, startedErr := d.cordon.Started(context.WithoutCancel(ctx), in.RunID)
	if startedErr != nil {
		return exec.StageResult{}, fmt.Errorf("%w: cordoned launch: %w", ErrRecoveryRetryable,
			errors.Join(err, startedErr))
	}
	d.logger.Error("cordoned launch failed",
		"invocation", string(in.InvocationID), "run", in.RunID, "started", started, "error", err)
	if started {
		return exec.StageResult{}, fmt.Errorf("%w: cordoned launch ended without an end; recovery owns the run",
			ErrRecoveryRetryable)
	}
	if ctx.Err() != nil {
		return d.cordonCanceledResult(in), nil
	}
	return exec.StageResult{
		InvocationID: in.InvocationID, Status: exec.StatusFailed,
		Summary: d.displayName + " cordoned launch was refused before the writer started.",
	}, nil
}

// cordonFailedResult is the failed result of a run that exited nonzero or
// stalled, with its diagnostic transcript kept as a warded writer's is.
func (d *Driver) cordonFailedResult(ctx context.Context, in intent, end CordonEnd) (exec.StageResult, error) {
	var summary string
	switch end.EndedBy {
	case CordonEndedByExited:
		switch {
		case end.ExitCode == -1:
			summary = d.displayName + " writer was ended by a signal."
		case end.ExitCode >= 1 && end.ExitCode <= 255:
			summary = fmt.Sprintf("%s writer exited with status %d.", d.displayName, end.ExitCode)
		default:
			return exec.StageResult{}, fmt.Errorf("%w: invalid cordoned exit code %d", ErrRecoveryRetryable, end.ExitCode)
		}
	case CordonEndedByStalled:
		summary = d.displayName + " writer stalled: it wrote nothing for the cordon's stall limit."
	case CordonEndedByCanceled, CordonEndedByLost:
		return exec.StageResult{}, fmt.Errorf("%w: a %s cordoned run is not a failed writer", ErrRecoveryRetryable, end.EndedBy)
	}
	if summary == "" {
		return exec.StageResult{}, fmt.Errorf("%w: invalid cordoned end %q", ErrRecoveryRetryable, end.EndedBy)
	}
	return d.retainFailureEvidence(ctx, in, exec.StageResult{
		InvocationID: in.InvocationID, Status: exec.StatusFailed, Summary: summary,
	}, end.Failure)
}

func (d *Driver) cordonCanceledResult(in intent) exec.StageResult {
	return exec.StageResult{
		InvocationID: in.InvocationID, Status: exec.StatusCanceled,
		Summary: d.displayName + " invocation canceled by daemon request.",
	}
}

// cordonExportRefusedResult fails a run whose export the cordon refused in
// a way that repeats. The summary carries only the screened reason.
func (d *Driver) cordonExportRefusedResult(in intent, refusal *CordonExportRefusal) exec.StageResult {
	d.logger.Error("cordoned export refused",
		"invocation", string(in.InvocationID), "run", in.RunID, "error", refusal.Err)
	return exec.StageResult{
		InvocationID: in.InvocationID, Status: exec.StatusFailed,
		Summary: truncateFailureSummary(d.displayName + " writer's export was refused: " + refusal.Reason),
	}
}

func cordonOutcome(exp CordonExport) exportOutcome {
	return exportOutcome{
		dir: exp.Dir, manifest: exp.Manifest,
		evidence: exp.Evidence, evidencePresent: exp.EvidencePresent,
		commitPlanPresent: exp.CommitPlanPresent,
		observedBaseSHA:   exp.BaseSHA,
	}
}

// recoverCordoned is recoverRunning for a cordoned intent: the cordon's
// Recover waits for the run's end and exports a clean exit, and its outcome
// commits as ward recovery's does.
func (d *Driver) recoverCordoned(ctx context.Context, in intent) error {
	// The run may still be alive under its supervisor, so a missing port
	// waits for a daemon that composes one instead of ending the intent.
	_, req, err := d.cordonLaunch(ctx, in)
	if err != nil {
		return fmt.Errorf("%w: rebuild cordoned launch: %w", ErrRecoveryRetryable, err)
	}
	recovered, err := d.cordon.Recover(ctx, req)
	if err != nil {
		var refusal *CordonExportRefusal
		if errors.As(err, &refusal) {
			return d.commitRecoveredTerminal(in.InvocationID, d.cordonExportRefusedResult(in, refusal))
		}
		return fmt.Errorf("%w: cordon recovery: %w", ErrRecoveryRetryable, err)
	}
	if !recovered.Outcome.valid() {
		return fmt.Errorf("%w: cordon reported recovery outcome %q", ErrRecoveryRetryable, recovered.Outcome)
	}
	switch recovered.Outcome {
	case CordonRecoveryExported:
		return d.recoverReleased(ctx, in, cordonOutcome(recovered.Export))
	case CordonRecoveryFailed:
		result, err := d.cordonFailedResult(ctx, in, recovered.End)
		if err != nil {
			return err
		}
		return d.commitRecoveredTerminal(in.InvocationID, result)
	case CordonRecoveryCanceled:
		return d.commitRecoveredTerminal(in.InvocationID, d.cordonCanceledResult(in))
	case CordonRecoveryLost:
		// The reason is the cordon's own text, never the run's, but a lost
		// outcome carries no summary, as a ward loss doesn't.
		d.logger.Error("stage recovery lost the cordoned invocation",
			"invocation", string(in.InvocationID), "run", in.RunID, "cause", recovered.Reason)
		return d.commitLost(ctx, in.InvocationID, nil)
	}
	return fmt.Errorf("%w: cordon reported recovery outcome %q", ErrRecoveryRetryable, recovered.Outcome)
}

// handoffStarted reports whether the intent's gate holds its run: a ward
// journal, or a cordon run directory.
func (d *Driver) handoffStarted(ctx context.Context, in intent) (bool, error) {
	cordoned, err := runnerClassOf(in.Spec)
	if err != nil {
		return false, err
	}
	if !cordoned {
		return d.gate.HandoffStarted(ctx, in.RunID)
	}
	port, err := d.cordonPort()
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrRecoveryRetryable, err)
	}
	return port.Started(ctx, in.RunID)
}

// requestCancellation records the intent's cancellation with its gate.
func (d *Driver) requestCancellation(ctx context.Context, in intent) error {
	cordoned, err := runnerClassOf(in.Spec)
	if err != nil {
		return err
	}
	if !cordoned {
		return d.gate.RequestCancellation(ctx, in.RunID)
	}
	port, err := d.cordonPort()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecoveryRetryable, err)
	}
	return port.RequestCancellation(ctx, in.RunID)
}

// releaseFinished releases each terminal intent's run through its own
// gate. An intent whose class can't be read releases nothing, the safe
// direction.
func (d *Driver) releaseFinished(ctx context.Context, terminal []intent) {
	var warded, cordoned []string
	for _, in := range terminal {
		isCordoned, err := runnerClassOf(in.Spec)
		switch {
		case err != nil:
		case isCordoned:
			cordoned = append(cordoned, in.RunID)
		default:
			warded = append(warded, in.RunID)
		}
	}
	d.gate.ReleaseFinishedHandoffs(ctx, warded)
	if len(cordoned) > 0 && d.cordon != nil {
		d.cordon.ReleaseFinished(ctx, cordoned)
	}
}
