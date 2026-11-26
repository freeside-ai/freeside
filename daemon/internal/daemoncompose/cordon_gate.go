package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/cordonhost"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/exportscan"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// cordonLauncher is what cordonGate uses of *cordon.Backend, so a test can
// fake it, and what a host launcher's client offers in the backend's place,
// so an operation added here can't be left off the protocol.
type cordonLauncher interface {
	Launch(context.Context, cordon.LaunchRequest) (cordon.LaunchResult, error)
	Export(context.Context, cordon.ExportRequest) (cordon.ExportResult, error)
	Started(context.Context, string) (bool, error)
	RequestCancellation(context.Context, string) error
	Recover(context.Context, cordon.RecoverRequest) (cordon.RecoveryResult, error)
	AuthenticateReleasedExport(context.Context, string, string) error
	ReleaseFinished(context.Context, []string) error
	SendMessage(ctx context.Context, runID string, ordinal int64, commandID, text string) error
	Detach()
}

// cordonGate is the stage driver's cordon port over the composed backend.
// It hands each launch the setup token of the enrollment generation the
// launch's own admission bound, never a current or configured one, and
// requires the class's recorded proof under unattended operation.
//
// It also owns the shutdown rule the backend states: once Detach begins,
// no context cancellation reaches a Launch, because a canceled Launch
// cancels its run for good, and a daemon that exits leaves its runs to
// their supervisors for the next daemon to recover.
//
// Tier A: it reads the credential a cordoned writer runs with. What it
// re-checks: the admission names the cordoned backend and binds an
// enrollment generation, and the token read refuses any other generation.
type cordonGate struct {
	backend cordonLauncher
	// probe proves the launch's host before a job: the runner's pre-job
	// pass, which reruns the full suite when the host's configuration
	// changed and then runs the probe.
	probe func(context.Context) error
	// settle brings the launch's host's proof condition in line with its
	// newest record, after a launch that required proof failed: the probe
	// the launch runs again may have withdrawn the declaration.
	settle func(context.Context) error
	// admission reads the attempt's durable admission.
	admission func(context.Context, domain.InvocationID) (domain.ExecutionAdmission, error)
	// readToken is wardstore.CredentialReader.Read.
	readToken  func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error)
	unattended bool
	// scanRoot holds the private scratch directory a failure transcript is
	// scanned in before the driver keeps it.
	scanRoot  string
	logger    *slog.Logger
	detaching atomic.Bool
}

var (
	_ stage.CordonGate = (*cordonGate)(nil)
	_ cordonLauncher   = (*cordon.Backend)(nil)
	_ cordonLauncher   = (*cordonhost.Client)(nil)
)

// newCordonGate is the port over a composed cordon. It reads each
// admission from st and each token through readToken. Every launch runs on
// the daemon's host, the only one composed until #3625, so the probe and
// the proof condition are the daemon's host's.
func newCordonGate(
	c *ComposedCordon, st *store.Store,
	readToken func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error),
	unattended bool, scanRoot string, logger *slog.Logger,
) *cordonGate {
	return &cordonGate{
		backend: c.Backend,
		probe:   func(ctx context.Context) error { return c.proofs.PreJob(ctx, domain.DaemonHostID) },
		settle:  func(ctx context.Context) error { return c.proofs.Settle(ctx, domain.DaemonHostID) },
		admission: func(ctx context.Context, id domain.InvocationID) (domain.ExecutionAdmission, error) {
			var admission domain.ExecutionAdmission
			err := st.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				admission, err = tx.GetExecutionAdmission(ctx, id)
				return err
			})
			return admission, err
		},
		readToken: readToken, unattended: unattended, scanRoot: scanRoot, logger: logger,
	}
}

// cordonDriverLifetime is the stage driver's lifetime under a cordon: it
// ends when ctx does, but only once detach has returned, so the driver
// cancels no session while a cordoned Launch could still take the
// cancellation as its run's.
func cordonDriverLifetime(ctx context.Context, detach func()) context.Context {
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	context.AfterFunc(ctx, func() {
		detach()
		cancel()
	})
	return lifetime
}

// Detach stops every waiting Launch and Recover with ErrCordonDetached,
// leaving their runs alive, and refuses every later one.
func (g *cordonGate) Detach() {
	g.detaching.Store(true)
	g.backend.Detach()
}

// Close detaches, so a session group that closes the gate before the
// driver detaches before the driver cancels any session.
func (g *cordonGate) Close(context.Context) error {
	g.Detach()
	return nil
}

// PreJob runs the suite's probe when the attempt requires the class's
// proof, so a class whose proof doesn't stand holds the attempt instead of
// failing it. The launch runs it again under RequireProof, which closes
// the gap between the two.
func (g *cordonGate) PreJob(ctx context.Context, id domain.InvocationID) error {
	admission, err := g.admission(ctx, id)
	if err != nil {
		return fmt.Errorf("read the admission of %s: %w", id, err)
	}
	if !g.requiresProof(admission) {
		return nil
	}
	return g.probe(ctx)
}

// requiresProof reports whether a launch must run under the class's
// recorded proof: when the daemon runs unattended now, or the admission
// was granted unattended. Either alone requires it, so a mode change
// between admission and launch never drops the proof.
func (g *cordonGate) requiresProof(admission domain.ExecutionAdmission) bool {
	return g.unattended || admission.OperatingMode == domain.ModeUnattended
}

func (g *cordonGate) Launch(ctx context.Context, launch stage.CordonLaunch) (stage.CordonEnd, error) {
	if g.detaching.Load() {
		return stage.CordonEnd{}, stage.ErrCordonDetached
	}
	token, requireProof, err := g.launchCredential(ctx, launch.InvocationID)
	if err != nil {
		// A shutdown that began during the read ends it with nothing
		// started; detached leaves the intent for recovery instead of the
		// canceled outcome a canceled read would record.
		if g.detaching.Load() {
			return stage.CordonEnd{}, fmt.Errorf("%w: %w", stage.ErrCordonDetached, err)
		}
		return stage.CordonEnd{}, err
	}
	// The launch's context is canceled with ctx only until Detach begins.
	launchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() {
		if !g.detaching.Load() {
			cancel()
		}
	})
	defer stop()
	res, err := g.backend.Launch(launchCtx, cordon.LaunchRequest{
		RunID: launch.RunID, Repository: launch.Base.Repo, BaseRef: launch.Base.BaseRef, BaseSHA: launch.Base.BaseSHA,
		Prompt: launch.Prompt, Args: launch.Args, Token: token, RequireProof: requireProof,
	})
	if err != nil {
		if requireProof {
			// The launch's own error is what the driver acts on; a
			// condition left unsettled is repaired by the host's next pass.
			if settleErr := g.settle(ctx); settleErr != nil {
				g.logger.Warn("settle the host's proof condition after a failed launch", "error", settleErr)
			}
		}
		return stage.CordonEnd{}, cordonPortError(err)
	}
	return g.end(ctx, res.EndedBy, res.ExitCode, res.TranscriptPath, res.StderrPath)
}

// launchCredential reads the attempt's admission, and from it the setup
// token of the generation it bound and whether the launch requires the
// class's proof.
func (g *cordonGate) launchCredential(ctx context.Context, id domain.InvocationID) (ward.SetupToken, bool, error) {
	admission, err := g.admission(ctx, id)
	if err != nil {
		return ward.SetupToken{}, false, fmt.Errorf("read the admission of %s: %w", id, err)
	}
	if admission.Backend != string(domain.BackendCordonedHostProcess) {
		return ward.SetupToken{}, false, fmt.Errorf("admission %s names backend %q, not the cordon", id, admission.Backend)
	}
	binding := admission.AgentBinding
	if binding == nil {
		return ward.SetupToken{}, false, fmt.Errorf("admission %s binds no enrollment generation", id)
	}
	token, err := g.readToken(ctx, binding.EnrollmentID, binding.EnrollmentGeneration)
	if err != nil {
		return ward.SetupToken{}, false, fmt.Errorf("enrollment %s generation %d: %w",
			binding.EnrollmentID, binding.EnrollmentGeneration, err)
	}
	return token, g.requiresProof(admission), nil
}

func (g *cordonGate) Export(ctx context.Context, req stage.CordonExportRequest) (stage.CordonExport, error) {
	res, err := g.backend.Export(ctx, cordonExportRequest(req))
	if err != nil {
		return stage.CordonExport{}, cordonPortError(err)
	}
	return cordonExportResult(res), nil
}

func (g *cordonGate) Started(ctx context.Context, runID string) (bool, error) {
	return g.backend.Started(ctx, runID)
}

func (g *cordonGate) RequestCancellation(ctx context.Context, runID string) error {
	return cordonPortError(g.backend.RequestCancellation(ctx, runID))
}

func (g *cordonGate) Recover(ctx context.Context, req stage.CordonExportRequest) (stage.CordonRecovery, error) {
	if g.detaching.Load() {
		return stage.CordonRecovery{}, stage.ErrCordonDetached
	}
	res, err := g.backend.Recover(ctx, cordon.RecoverRequest{RunID: req.RunID, Export: cordonExportRequest(req)})
	if err != nil {
		return stage.CordonRecovery{}, cordonPortError(err)
	}
	out := stage.CordonRecovery{Reason: res.Reason}
	switch res.Outcome {
	case cordon.RecoveryExported:
		out.Outcome, out.Export = stage.CordonRecoveryExported, cordonExportResult(res.Export)
		out.End = stage.CordonEnd{EndedBy: stage.CordonEndedByExited}
		return out, nil
	case cordon.RecoveryFailed:
		end, err := g.end(ctx, res.EndedBy, res.ExitCode, res.TranscriptPath, res.StderrPath)
		if err != nil {
			return stage.CordonRecovery{}, err
		}
		out.Outcome, out.End = stage.CordonRecoveryFailed, end
		return out, nil
	case cordon.RecoveryCanceled:
		out.Outcome = stage.CordonRecoveryCanceled
		out.End = stage.CordonEnd{EndedBy: stage.CordonEndedByCanceled, ExitCode: res.ExitCode}
		return out, nil
	case cordon.RecoveryLost:
		out.Outcome = stage.CordonRecoveryLost
		out.End = stage.CordonEnd{EndedBy: stage.CordonEndedByLost, ExitCode: res.ExitCode}
		return out, nil
	}
	return stage.CordonRecovery{}, fmt.Errorf("cordon recovery reported outcome %q", res.Outcome)
}

// SendMessage reaches the backend whether or not the gate is detaching: a
// message changes nothing about how the run ends, and the run outlives the
// daemon.
func (g *cordonGate) SendMessage(ctx context.Context, runID string, ordinal int64, commandID, text string) error {
	return cordonPortError(g.backend.SendMessage(ctx, runID, ordinal, commandID, text))
}

// AuthenticateReleasedExport refuses for good, as ward's journal refusal
// does, an export directory that isn't the one the cordon recorded.
func (g *cordonGate) AuthenticateReleasedExport(ctx context.Context, runID, dir string) error {
	err := g.backend.AuthenticateReleasedExport(ctx, runID, dir)
	if errors.Is(err, cordon.ErrExportUnauthenticated) {
		return fmt.Errorf("%w: %w", stage.ErrUnsupportedStart, err)
	}
	return err
}

// ReleaseFinished logs a failed release; the directory stays, the safe
// direction, and the next terminal pass releases it again.
func (g *cordonGate) ReleaseFinished(ctx context.Context, runIDs []string) {
	if err := g.backend.ReleaseFinished(ctx, runIDs); err != nil {
		g.logger.Warn("release finished cordoned runs", "runs", runIDs, "error", err)
	}
}

// end maps a launch's end, reading a failed run's diagnostic transcript.
func (g *cordonGate) end(
	ctx context.Context, endedBy cordon.EndedBy, exitCode int, transcriptPath, stderrPath string,
) (stage.CordonEnd, error) {
	out := stage.CordonEnd{ExitCode: exitCode}
	switch endedBy {
	case cordon.EndedByExited:
		out.EndedBy = stage.CordonEndedByExited
		if exitCode == 0 {
			return out, nil
		}
	case cordon.EndedByStalled:
		out.EndedBy = stage.CordonEndedByStalled
	case cordon.EndedByCanceled:
		out.EndedBy = stage.CordonEndedByCanceled
		return out, nil
	case cordon.EndedByLost:
		out.EndedBy = stage.CordonEndedByLost
		return out, nil
	}
	if out.EndedBy == "" {
		return stage.CordonEnd{}, fmt.Errorf("cordon reported an end %q", endedBy)
	}
	evidence, err := g.failureEvidence(ctx, transcriptPath, stderrPath)
	if err != nil {
		return stage.CordonEnd{}, err
	}
	out.Failure = evidence
	return out, nil
}

// failureEvidence is a failed run's transcript with its stderr appended,
// as the warded launcher appends a failed CLI's stderr. Like ward's capture,
// a transcript that is absent, too large, not UTF-8, or refused by the
// credential scan is unavailable rather than kept.
func (g *cordonGate) failureEvidence(ctx context.Context, transcriptPath, stderrPath string) (*ward.FailureEvidence, error) {
	unavailable := &ward.FailureEvidence{Unavailable: true}
	transcript, ok, err := readBounded(transcriptPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return unavailable, nil
	}
	stderr, ok, err := readBounded(stderrPath)
	if err != nil {
		return nil, err
	}
	body := transcript
	if ok {
		body = append(body, stderr...)
	}
	if len(body) > ward.MaxFailureTranscriptBytes || !utf8.Valid(body) {
		return unavailable, nil
	}
	refused, err := g.scanRefuses(ctx, body)
	if err != nil {
		return nil, err
	}
	if refused {
		return unavailable, nil
	}
	return &ward.FailureEvidence{Body: body, Digest: contentaddr.Sum(body)}, nil
}

// readBounded reads a regular file of at most MaxFailureTranscriptBytes,
// reporting false for one that is absent or larger.
func readBounded(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > ward.MaxFailureTranscriptBytes {
		return nil, false, nil
	}
	f, err := os.Open(path) //nolint:gosec // G304: a path the cordon derived from its runs root
	if err != nil {
		return nil, false, err
	}
	defer f.Close() //nolint:errcheck // read-only
	body, err := io.ReadAll(io.LimitReader(f, ward.MaxFailureTranscriptBytes+1))
	if err != nil {
		return nil, false, err
	}
	return body, len(body) <= ward.MaxFailureTranscriptBytes, nil
}

// scanRefuses runs the export credential scan over body alone. A scan that
// can't finish is an error whose details are withheld, since they may
// carry matched text.
func (g *cordonGate) scanRefuses(ctx context.Context, body []byte) (bool, error) {
	if err := os.MkdirAll(g.scanRoot, 0o700); err != nil {
		return false, fmt.Errorf("create the cordon's scan root: %w", err)
	}
	dir, err := os.MkdirTemp(g.scanRoot, "failure-scan-")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "transcript.txt"), body, 0o600); err != nil {
		return false, err
	}
	err = (exportscan.Scanner{}).Scan(ctx, dir)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, ward.ErrOutputScanRefused):
		return true, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	}
	return false, errors.New("failure transcript scan could not complete (details withheld)")
}

func cordonExportRequest(req stage.CordonExportRequest) cordon.ExportRequest {
	return cordon.ExportRequest{RunID: req.RunID, Transcript: req.Transcript, Sources: req.Sources}
}

func cordonExportResult(res cordon.ExportResult) stage.CordonExport {
	return stage.CordonExport{
		Dir: res.Dir, Manifest: res.Manifest, Evidence: res.Evidence,
		EvidencePresent: res.EvidencePresent, CommitPlanPresent: res.CommitPlanPresent, BaseSHA: res.BaseSHA,
	}
}

// cordonPortError maps the backend's errors to the stage port's. An export
// refusal that would repeat on every try becomes a CordonExportRefusal
// whose reason is a fixed sentence, or the scan's, which names a rule and
// an item, never the matched text.
func cordonPortError(err error) error {
	var scan *cordon.ScanRefusalError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, cordon.ErrDetached):
		return fmt.Errorf("%w: %w", stage.ErrCordonDetached, err)
	case errors.Is(err, cordon.ErrRunEndUnproven):
		return fmt.Errorf("%w: %w", stage.ErrCordonEndUnproven, err)
	case errors.Is(err, cordon.ErrNoRun):
		return fmt.Errorf("%w: %w", stage.ErrCordonNoRun, err)
	case errors.Is(err, cordon.ErrRunEnded):
		return fmt.Errorf("%w: %w", stage.ErrCordonRunEnded, err)
	case errors.Is(err, cordon.ErrInputClosed):
		return fmt.Errorf("%w: %w", stage.ErrCordonInputClosed, err)
	case errors.Is(err, cordon.ErrNotStreamed):
		return fmt.Errorf("%w: %w", stage.ErrCordonNotStreamed, err)
	case errors.Is(err, cordon.ErrNotTaken):
		return fmt.Errorf("%w: %w", stage.ErrCordonNotTaken, err)
	case errors.Is(err, cordon.ErrNotStarted):
		return fmt.Errorf("%w: %w", stage.ErrCordonNotStarted, err)
	case errors.As(err, &scan):
		return &stage.CordonExportRefusal{Reason: "the credential scan refused it: " + scan.Reason, Err: err}
	case errors.Is(err, cordon.ErrWorktreeReplaced):
		return &stage.CordonExportRefusal{Reason: "the writer replaced its worktree", Err: err}
	case errors.Is(err, export.ErrLinkedFile):
		return &stage.CordonExportRefusal{Reason: "the worktree holds a hard-linked file", Err: err}
	// ErrCanceled also matches ErrNotExportable, so it is mapped first: a
	// canceled run ends canceled, not refused.
	case errors.Is(err, cordon.ErrCanceled):
		return fmt.Errorf("%w: %w", stage.ErrCordonCanceled, err)
	case errors.Is(err, cordon.ErrNotExportable):
		return &stage.CordonExportRefusal{Reason: "the writer did not exit cleanly", Err: err}
	}
	return err
}
