package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// ErrUnsupportedStart marks an admitted start this driver refuses to run:
// a containment class it cannot enforce, an identity it cannot resolve, or
// an input bundle it cannot render. It is a refusal, never a downgrade.
var ErrUnsupportedStart = errors.New("claude driver cannot run this start")

// ErrDriverClosed marks a start refused because daemon shutdown has begun.
// A closed driver never creates another external writer.
var ErrDriverClosed = errors.New("claude driver is closed")

// Config is the driver's composition. Every field is required: a nil port
// would turn a containment or durability guarantee into a silent no-op.
type Config struct {
	// Lifetime is the owning daemon's lifetime, not a reconcile-pass context.
	// Every external writer is canceled when it ends.
	Lifetime context.Context
	// Dir is the driver's private state directory.
	Dir string
	// SeedRoot is where daemon-owned base checkouts are materialized. It must
	// be the same root the ward Config declares, since the gate refuses a
	// seed source outside it.
	SeedRoot string
	// ExportRoot is the daemon-owned durable root the ward Config declares.
	// Returned and replayed export paths must be direct children of this
	// exact root before the driver permits any read or deletion.
	ExportRoot string
	Gate       Gate
	Seeder     Seeder
	Exports    ExportRecorder
	Outcomes   OutcomeRecorder
	Authority  AdmissionAuthority
	// Artifacts persists released evidence before the export directory is
	// removed; a result may name only what it has stored.
	Artifacts Artifacts
	Volumes   AuthStoreVolumes
	// PreJob is the production backend's lightweight fail-closed probe. It
	// runs after duplicate arbitration and before materialization or intent
	// persistence, so a refused probe starts no work and a duplicate replay
	// does not depend on current runtime health.
	PreJob func(context.Context, domain.InvocationID) error
	// Import carries the gauntlet policy the candidate is imported under:
	// the declared path allowlist. Authority widens it with the exact
	// admitted trust profile before every import.
	Import importer.Options
	// Preparation is the project image's workspace-hydration argv (the
	// image-owned freeside-project-prepare helper). When set, the launch
	// command runs it in the root phase so the implementer can execute the
	// admitted verification recipe over hydrated dependencies (#522). It is
	// composition config resolved from the immutable project_images record, not
	// per-attempt state, so recovery re-derives the same command; empty leaves
	// the attended launch command unchanged. New re-gates it to empty or the
	// fixed helper (projectimage.PreparationPath): the value reaches root argv,
	// so this exported constructor is a trust boundary that re-runs the policy
	// gate rather than trusting an arbitrary caller-supplied command (AGENTS.md
	// daemon conventions), the same gate composition and onboarding apply.
	Preparation []string
	// Now supplies the pinned instants a replayed pipeline reuses.
	Now func() time.Time
}

// Driver is the production Claude stage driver. It implements
// exec.MaterializedStageDriver; the daemon wraps it in
// exec.MaterializingStageDriver so digest verification always completes
// before an intent is committed.
type Driver struct {
	dir        string
	seedRoot   string
	exportRoot string
	seedMu     sync.Mutex
	seedFS     *os.Root
	gate       Gate
	seeder     Seeder
	exports    ExportRecorder
	outcomes   OutcomeRecorder
	authority  AdmissionAuthority
	artifacts  Artifacts
	volumes    AuthStoreVolumes
	preJob     func(context.Context, domain.InvocationID) error
	imports    importer.Options
	prepare    []string
	now        func() time.Time
	lifetime   context.Context

	// mu serializes state transitions per invocation. StartWithInputs owns
	// duplicate arbitration (exec.MaterializedStageDriver), and the pipeline
	// commits through the same lock so a concurrent Collect never observes a
	// half-written transition.
	mu         sync.Mutex
	running    map[domain.InvocationID]*session
	recovering map[domain.InvocationID]struct{}
	closed     bool
}

// session is one in-flight handoff in this process.
type session struct {
	cancel        context.CancelFunc
	done          chan struct{}
	pendingIntent *intent
	intentErr     error
	pendingResult *exec.StageResult
	commitErr     error
	// preJournalCancellation says pendingResult was authorized by a
	// confirmed absent ward journal even if the durable intent had already
	// advanced to phaseRunning.
	preJournalCancellation bool
}

var _ exec.MaterializedStageDriver = (*Driver)(nil)

// New constructs the driver and claims its state directory.
func New(cfg Config) (*Driver, error) {
	switch {
	case cfg.Lifetime == nil:
		return nil, errors.New("new claude driver: nil lifetime")
	case cfg.Dir == "":
		return nil, errors.New("new claude driver: state directory is required")
	case cfg.SeedRoot == "":
		return nil, errors.New("new claude driver: seed root is required")
	case !cleanAbsoluteRoot(cfg.ExportRoot):
		return nil, errors.New("new claude driver: clean absolute export root is required")
	case cfg.Gate == nil:
		return nil, errors.New("new claude driver: nil gate")
	case cfg.Seeder == nil:
		return nil, errors.New("new claude driver: nil seeder")
	case cfg.Exports == nil:
		return nil, errors.New("new claude driver: nil export recorder")
	case cfg.Outcomes == nil:
		return nil, errors.New("new claude driver: nil outcome recorder")
	case cfg.Authority == nil:
		return nil, errors.New("new claude driver: nil admission authority")
	case cfg.Artifacts == nil:
		return nil, errors.New("new claude driver: nil artifact store")
	case cfg.Volumes == nil:
		return nil, errors.New("new claude driver: nil auth store volumes")
	case cfg.Now == nil:
		return nil, errors.New("new claude driver: nil clock")
	case len(cfg.Preparation) > 0 &&
		!slices.Equal(cfg.Preparation, []string{projectimage.PreparationPath}):
		// The preparation argv reaches the root launch command; accept only the
		// fixed image-owned helper (or none) so an arbitrary caller-supplied
		// command cannot execute as root beside the credential mount.
		return nil, errors.New(
			"new claude driver: preparation must be empty or the fixed project-image helper")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("new claude driver: %w", err)
	}
	if err := os.MkdirAll(cfg.SeedRoot, 0o700); err != nil {
		return nil, fmt.Errorf("new claude driver: create seed root: %w", err)
	}
	seedFS, err := os.OpenRoot(cfg.SeedRoot)
	if err != nil {
		return nil, fmt.Errorf("new claude driver: open seed root: %w", err)
	}
	return &Driver{
		dir: cfg.Dir, seedRoot: cfg.SeedRoot, exportRoot: cfg.ExportRoot,
		gate: cfg.Gate, seeder: cfg.Seeder,
		seedFS:  seedFS,
		exports: cfg.Exports, outcomes: cfg.Outcomes, authority: cfg.Authority,
		artifacts: cfg.Artifacts, volumes: cfg.Volumes,
		preJob:  cfg.PreJob,
		imports: cfg.Import, prepare: slices.Clone(cfg.Preparation),
		now: cfg.Now, lifetime: cfg.Lifetime,
		running:    map[domain.InvocationID]*session{},
		recovering: map[domain.InvocationID]struct{}{},
	}, nil
}

func cleanAbsoluteRoot(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != string(filepath.Separator)
}

// StartWithInputs commits one durable intent and runs the handoff pipeline
// asynchronously. The intent lands before any runtime object exists, so a
// crash immediately after leaves recovery a record to adopt from; ward's own
// journal covers the window inside the handoff.
func (d *Driver) StartWithInputs(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec, load exec.StageInputLoader,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrDriverClosed
	}
	if _, err := d.loadIntent(ctx, id); err == nil {
		return exec.ErrDuplicateStart
	} else if !errors.Is(err, exec.ErrUnknownInvocation) {
		return err
	}
	if d.preJob != nil {
		if err := d.preJob(ctx, id); err != nil {
			return fmt.Errorf("pre-job probe: %w",
				errors.Join(exec.ErrPreJobRefused, err))
		}
	}
	if load == nil {
		return fmt.Errorf("%w: nil input loader", ErrUnsupportedStart)
	}
	inputs, err := load(ctx)
	if err != nil {
		// No intent is committed for a failed load, so a contender may still
		// become the winner (the MaterializedStageDriver contract).
		return err
	}
	instructions, err := ward.VendorInstructionsFromStageInputs(inputs)
	if err != nil {
		return err
	}
	materialized := durableInputsFrom(inputs)
	prompt, err := renderPromptParts(materialized)

	now := d.now().UTC()
	in := intent{
		InvocationID: id, RunID: RunIDFor(id), Phase: phaseSeeding, Spec: spec,
		Seed: filepath.Join(d.seedRoot, RunIDFor(id)), Prompt: prompt,
		Inputs: materialized, Instructions: instructions,
		// Capture the composition-derived hydration argv into the durable
		// record so recovery rebuilds the launch command from the intent, not
		// from a d.prepare that a later deploy or mode change may have altered.
		Preparation: slices.Clone(d.prepare),
		RecordedAt:  now, CommitDate: now,
	}
	if err != nil {
		if !errors.Is(err, ErrUnsupportedStart) {
			return err
		}
		in.Phase = phaseCommitted
		in.Result = &exec.StageResult{
			InvocationID: id, Status: exec.StatusFailed,
			Summary: truncateSummary(err.Error()),
		}
		if err := d.regate(ctx, in, true); err != nil {
			return err
		}
		if err := d.recordOutcome(ctx, in, *in.Result); err != nil {
			return err
		}
		return d.saveIntent(in)
	}
	if err := d.regate(ctx, in, true); err != nil {
		return err
	}
	// The handoff spec is built and rejected before the intent is durable:
	// a refusal must leave no record claiming an invocation this driver
	// cannot run.
	if _, err := d.handoffSpec(ctx, in); err != nil {
		return err
	}
	if err := d.saveIntent(in); err != nil {
		return err
	}

	// The pipeline outlives this reconcile call but not its owning daemon.
	// Close cancels and awaits this session before the store is closed.
	runCtx, cancel := context.WithCancel(d.lifetime)
	sess := &session{cancel: cancel, done: make(chan struct{})}
	d.running[id] = sess
	go func() {
		defer close(sess.done)
		d.runPipeline(runCtx, in)
		d.mu.Lock()
		if sess.pendingIntent == nil && sess.pendingResult == nil {
			delete(d.running, id)
		}
		d.mu.Unlock()
	}()
	return nil
}

// Close prevents new starts, cancels every in-process session, and waits for
// each pipeline to finish ward teardown. Durable preterminal intent remains
// recoverable if terminal persistence cannot complete during shutdown.
func (d *Driver) Close(ctx context.Context) error {
	type sessionRef struct {
		id    domain.InvocationID
		runID string
		phase phase
		sess  *session
	}
	d.mu.Lock()
	d.closed = true
	sessions := make([]sessionRef, 0, len(d.running))
	for id, sess := range d.running {
		sessions = append(sessions, sessionRef{id: id, sess: sess})
	}
	d.mu.Unlock()

	var closeErrs []error
	for i := range sessions {
		ref := &sessions[i]
		in, err := d.loadIntent(ctx, ref.id)
		if err != nil {
			closeErrs = append(closeErrs, fmt.Errorf(
				"close claude driver: load invocation %s: %w", ref.id, err,
			))
			continue
		}
		ref.runID = in.RunID
		ref.phase = in.Phase
	}
	for _, ref := range sessions {
		if ref.phase == phaseRunning {
			if err := d.gate.RequestCancellation(ctx, ref.runID); err != nil &&
				!errors.Is(err, ward.ErrJournalRecordNotFound) {
				closeErrs = append(closeErrs, fmt.Errorf(
					"close claude driver: cancellation intent for %s: %w",
					ref.runID, err,
				))
			}
		}
		// A failed durable amendment is a shutdown error, not permission to
		// leave this or any later credential-bearing process alive.
		ref.sess.cancel()
	}
	for _, ref := range sessions {
		select {
		case <-ref.sess.done:
		case <-ctx.Done():
			closeErrs = append(closeErrs, fmt.Errorf("close claude driver: %w", ctx.Err()))
			return errors.Join(closeErrs...)
		}
	}
	d.seedMu.Lock()
	defer d.seedMu.Unlock()
	if d.seedFS != nil {
		if err := d.seedFS.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close claude driver seed root: %w", err))
		}
		d.seedFS = nil
	}
	return errors.Join(closeErrs...)
}

// Inspect reports one invocation's inspection from durable state, so a
// restarted daemon answers the same way the process that started it would
// have. Liveness is the in-process pipeline observation: true only while
// this daemon process is actively driving the invocation, so a restarted
// daemon reports live=false until recovery re-observes the runtime — never
// a synthesized liveness bit (issue #394). Inspection reads no writer
// output: status derives from the durable intent and the driver's own
// pipeline state, per the §5.6 containment guarantee.
func (d *Driver) Inspect(ctx context.Context, id domain.InvocationID) (exec.Inspection, error) {
	in, live, err := d.inspectIntent(ctx, id)
	if err != nil {
		if errors.Is(err, ErrRecoveryRetryable) {
			return exec.Inspection{Status: exec.StatusRunning}, nil
		}
		return exec.Inspection{}, err
	}
	switch in.Phase {
	case phaseCommitted:
		return exec.Inspection{Status: in.Result.Status}, nil
	case phaseLost:
		return exec.Inspection{Status: exec.StatusGone}, nil
	case phaseSeeding, phaseRunning, phaseExported:
		if live {
			return exec.Inspection{Status: exec.StatusRunning, Live: true}, nil
		}
	}

	// A live Handoff can fail without proving its writer was reaped. The
	// pipeline then deliberately leaves phaseRunning for ward, but waiting for
	// the next daemon start to call Reconcile would make the current engine see
	// a gone invocation with no collectable result and stop. Adopt the orphan
	// here so every later reconcile pass can retry recovery safely.
	if err := d.reconcileIntent(ctx, in); err != nil {
		if errors.Is(err, ErrRecoveryRetryable) {
			return exec.Inspection{Status: exec.StatusRunning}, nil
		}
		return exec.Inspection{}, fmt.Errorf("reconcile invocation %s: %w", id, err)
	}

	in, live, err = d.inspectIntent(ctx, id)
	if err != nil {
		return exec.Inspection{}, err
	}
	switch in.Phase {
	case phaseCommitted:
		return exec.Inspection{Status: in.Result.Status}, nil
	case phaseLost:
		return exec.Inspection{Status: exec.StatusGone}, nil
	case phaseSeeding, phaseRunning, phaseExported:
		// Recovery can restart a pre-handoff pipeline asynchronously, or leave
		// a running handoff pending another ward observation. Both are live
		// workflow states, not proof that the invocation was lost; liveness is
		// whatever the recovery actually re-observed.
		return exec.Inspection{Status: exec.StatusRunning, Live: live}, nil
	}
	return exec.Inspection{}, fmt.Errorf("invocation %s: phase %q: %w", id, in.Phase, exec.ErrInvalidStatus)
}

func (d *Driver) inspectIntent(
	ctx context.Context, id domain.InvocationID,
) (intent, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.retryPendingIntentLocked(id); err != nil {
		return intent{}, false, err
	}
	if err := d.retryPendingResultLocked(id); err != nil {
		return intent{}, false, fmt.Errorf("invocation %s terminal commit: %w", id, err)
	}
	in, err := d.loadIntent(ctx, id)
	if err != nil {
		return intent{}, false, err
	}
	if in.Phase == phaseCommitted || in.Phase == phaseLost {
		_ = d.cleanupTerminalSeed(in)
	}
	_, live := d.running[id]
	return in, live, nil
}

// Stream returns an empty transcript for a known invocation. The writer's
// output leaves the VM only through the §5.6 evidence channel, which the
// gate releases after the writer is proven absent, so there is no live
// stream to follow: the gate's containment guarantee is exactly that nothing
// reads the writer while it runs.
func (d *Driver) Stream(ctx context.Context, id domain.InvocationID) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.loadIntent(ctx, id); err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader("")), nil
}

// Cancel stops an in-flight handoff. The gate's own teardown reaps the
// runtime objects; the terminal result is committed by the pipeline as it
// unwinds. Cancelling a terminal invocation is a no-op.
func (d *Driver) Cancel(ctx context.Context, id domain.InvocationID) error {
	d.mu.Lock()
	in, err := d.loadIntentAdmission(ctx, id)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	if in.Phase == phaseCommitted || in.Phase == phaseLost {
		d.mu.Unlock()
		return nil
	}
	sess, live := d.running[id]
	d.mu.Unlock()
	if !live {
		// An orphaned intent has no pipeline to stop; Reconcile adopts or
		// loses it against the gate's journal, which is the only honest way
		// to end a handoff this process did not start.
		return nil
	}
	cancelErr := d.gate.RequestCancellation(ctx, in.RunID)
	preJournal := errors.Is(cancelErr, ward.ErrJournalRecordNotFound)
	if cancelErr != nil && !preJournal {
		return fmt.Errorf("invocation %s cancellation intent: %w", id, cancelErr)
	}
	sess.cancel()
	<-sess.done
	if preJournal {
		if err := d.commitPreJournalCancellation(id, sess); err != nil {
			return fmt.Errorf("invocation %s pre-journal cancellation: %w", id, err)
		}
	}
	return nil
}

// commitPreJournalCancellation makes a successful user cancellation terminal
// when no ward journal ever acquired authority. Close deliberately does not
// call this: daemon shutdown preserves a seed that the next process may
// resume, while Cancel is an explicit instruction not to resume this work.
func (d *Driver) commitPreJournalCancellation(
	id domain.InvocationID,
	sess *session,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := exec.StageResult{
		InvocationID: id,
		Status:       exec.StatusCanceled,
		Summary:      "Claude invocation canceled by daemon request.",
	}
	committed, err := d.commitPreJournalCancellationLocked(id, result)
	if err != nil {
		pending := result
		sess.pendingResult = &pending
		sess.commitErr = err
		sess.preJournalCancellation = true
		d.running[id] = sess
	}
	if !committed && err == nil {
		return nil
	}
	return err
}

func (d *Driver) commitPreJournalCancellationLocked(
	id domain.InvocationID,
	result exec.StageResult,
) (bool, error) {
	in, err := d.loadIntentAdmission(context.Background(), id)
	if err != nil {
		return false, err
	}
	switch in.Phase {
	case phaseCommitted, phaseLost:
		return true, nil
	case phaseExported:
		// A returned export outraced the cancellation request. Its durable
		// evidence, rather than an earlier journal miss, owns disposition.
		return false, nil
	case phaseRunning:
		started, err := d.gate.HandoffStarted(context.Background(), in.RunID)
		if err != nil {
			return false, fmt.Errorf("confirm absent handoff journal: %w", err)
		}
		if started {
			// Ward owns a handoff once its journal exists. Inspect/Recover
			// will converge its durable cancellation outcome.
			return false, nil
		}
	case phaseSeeding:
	}
	if err := d.recordOutcome(context.Background(), in, result); err != nil {
		return false, err
	}
	in.Phase = phaseCommitted
	in.Result = &result
	if err := d.saveIntent(in); err != nil {
		return false, err
	}
	_ = d.cleanupTerminalSeed(in)
	return true, nil
}

// Collect returns the committed terminal result.
func (d *Driver) Collect(ctx context.Context, id domain.InvocationID) (exec.StageResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.retryPendingResultLocked(id); err != nil {
		return exec.StageResult{}, fmt.Errorf("invocation %s terminal commit: %w", id, err)
	}
	in, err := d.loadIntent(ctx, id)
	if err != nil {
		return exec.StageResult{}, err
	}
	if in.Phase == phaseCommitted || in.Phase == phaseLost {
		_ = d.cleanupTerminalSeed(in)
	}
	switch in.Phase {
	case phaseCommitted:
		return *in.Result, nil
	case phaseLost:
		return exec.StageResult{}, fmt.Errorf("invocation %s: %w", id, exec.ErrNoResult)
	case phaseSeeding, phaseRunning, phaseExported:
		return exec.StageResult{}, fmt.Errorf("invocation %s: %w", id, exec.ErrResultNotReady)
	}
	return exec.StageResult{}, fmt.Errorf("invocation %s: phase %q: %w", id, in.Phase, exec.ErrInvalidStatus)
}
