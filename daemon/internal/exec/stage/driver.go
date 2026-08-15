package stage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
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

// Config is the driver's composition. Every field is required: a nil port
// would turn a containment or durability guarantee into a silent no-op.
type Config struct {
	// ErrorPrefix preserves provider-specific error text at this shared boundary.
	ErrorPrefix string
	// DisplayName is the provider name used in terminal summaries.
	DisplayName string
	// Provider renders the provider-specific prompt, handoff, and identities.
	Provider Provider
	// CredentialMount is the immutable capability topology for the provider's
	// sole identity-bound mount. Ward authenticates its dynamic volume.
	CredentialMount CredentialMountPolicy
	// ProviderConfigError preserves provider-specific construction validation
	// without widening the runtime port. Nil means the provider is configured.
	ProviderConfigError error
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
	ExportRoot   string
	Gate         Gate
	Seeder       Seeder
	Exports      ExportRecorder
	ImportStarts ImportStartRecorder
	Outcomes     OutcomeRecorder
	Authority    AdmissionAuthority
	// Artifacts persists released evidence before the export directory is
	// removed; a result may name only what it has stored.
	Artifacts Artifacts
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
	// Logger reports what the asynchronous pipeline does. The pipeline has
	// no caller to return an error to once a handoff is running, so without
	// it a retained terminal-write failure stays invisible until the next
	// Inspect. Nil discards the records.
	Logger *slog.Logger
}

// Driver is the provider-neutral durable stage driver. It implements
// exec.MaterializedStageDriver; the daemon wraps it in
// exec.MaterializingStageDriver so digest verification always completes
// before an intent is committed.
type Driver struct {
	errorPrefix string
	displayName string
	dir         string
	seedRoot    string
	exportRoot  string
	seedMu      sync.Mutex
	seedFS      *os.Root
	// seedCleanupWarned records, per invocation, the last terminal-seed-cleanup
	// failure already reported (by error identity), so a persistent removal
	// failure logs once rather than on every idempotent Inspect/Collect, yet a
	// change to a different failing target still warns: cleanup stops at the
	// first undeletable name, so once an operator repairs it the sibling
	// becomes the new blocker and must be reported. Guarded by seedMu; a full
	// success clears the entry.
	seedCleanupWarned map[domain.InvocationID]string
	gate              Gate
	seeder            Seeder
	exports           ExportRecorder
	importStarts      ImportStartRecorder
	outcomes          OutcomeRecorder
	authority         AdmissionAuthority
	artifacts         Artifacts
	provider          Provider
	credentialMount   CredentialMountPolicy
	preJob            func(context.Context, domain.InvocationID) error
	imports           importer.Options
	prepare           []string
	now               func() time.Time
	lifetime          context.Context
	logger            *slog.Logger

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

// providerRunIDPattern mirrors ward's HandoffSpec RunID contract because this
// driver must refuse an unsafe provider value before it can construct a ward
// request. Keep the two patterns byte-identical.
var providerRunIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validateRunID(runID string) error {
	if !providerRunIDPattern.MatchString(runID) {
		return fmt.Errorf(
			"%w: run ID %q does not match %s",
			ErrUnsupportedStart, runID, providerRunIDPattern,
		)
	}
	return nil
}

func (d *Driver) validatedProviderRunID(id domain.InvocationID) (string, error) {
	runID := d.provider.RunID(id)
	if err := validateRunID(runID); err != nil {
		return "", fmt.Errorf(
			"provider run ID for invocation %s: %w",
			id, err,
		)
	}
	return runID, nil
}

func (d *Driver) handoffSpec(ctx context.Context, in intent) (ward.HandoffSpec, error) {
	providerInput := providerHandoffInputFrom(in)
	hs, err := d.provider.HandoffSpec(ctx, providerInput)
	if err != nil {
		return ward.HandoffSpec{}, err
	}
	// Detach before checking so provider-retained references cannot change the
	// policy decision in the gap before ward freezes its own request.
	hs = detachProviderHandoffSpec(hs)
	// The provider chooses vendor-specific containment details, but it cannot
	// retarget the durable run, checkout, or admitted security bindings. Ward
	// validates the returned shape; only this boundary can compare it with the
	// authenticated intent.
	switch {
	case hs.RunID != providerInput.RunID:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff run ID differs from the durable input",
			ErrUnsupportedStart,
		)
	case providerInput.Spec.Workspace != ward.WorkspaceRef(hs.RunID):
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff workspace differs from the ward run identity",
			ErrUnsupportedStart,
		)
	case hs.Seed.Mode != ward.SeedBaseCheckout:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff seed mode differs from the durable input",
			ErrUnsupportedStart,
		)
	case hs.Seed.SourceDir != providerInput.Seed:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff seed source differs from the durable input",
			ErrUnsupportedStart,
		)
	case hs.Seed.Base != providerInput.Spec.Base:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff base differs from the durable input",
			ErrUnsupportedStart,
		)
	case hs.Agent.Image != string(providerInput.Spec.ImageRef):
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff image differs from the durable input",
			ErrUnsupportedStart,
		)
	case hs.Agent.EgressProfile != providerInput.Spec.EgressProfile:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff egress profile differs from the durable input",
			ErrUnsupportedStart,
		)
	case !vendorInstructionsEqual(hs.Agent.VendorInstructions, in.Instructions):
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff vendor instructions differ from the durable input",
			ErrUnsupportedStart,
		)
	case len(hs.Agent.CredentialMounts) != 1:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff must carry exactly one identity-bound credential mount",
			ErrUnsupportedStart,
		)
	case (providerInput.Spec.AuthIdentityID == "") != (hs.AuthStoreLease == nil):
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff auth-store lease presence differs from the durable input",
			ErrUnsupportedStart,
		)
	case hs.AuthStoreLease != nil &&
		hs.AuthStoreLease.AuthIdentityID != providerInput.Spec.AuthIdentityID:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff auth identity differs from the durable input",
			ErrUnsupportedStart,
		)
	case hs.AuthStoreLease != nil && hs.AuthStoreLease.Holder != providerInput.InvocationID:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff lease holder differs from the durable input",
			ErrUnsupportedStart,
		)
	}
	mount := hs.Agent.CredentialMounts[0]
	switch {
	case mount.Target != d.credentialMount.Target:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff credential target differs from the configured topology",
			ErrUnsupportedStart,
		)
	case mount.Manifest != d.credentialMount.Manifest:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff credential manifest differs from the configured topology",
			ErrUnsupportedStart,
		)
	case mount.Writable != d.credentialMount.Writable:
		return ward.HandoffSpec{}, fmt.Errorf(
			"%w: provider handoff credential writability differs from the configured topology",
			ErrUnsupportedStart,
		)
	}
	return hs, nil
}

func detachProviderHandoffSpec(hs ward.HandoffSpec) ward.HandoffSpec {
	hs.Agent.Command = slices.Clone(hs.Agent.Command)
	hs.Agent.Env = slices.Clone(hs.Agent.Env)
	hs.Agent.CredentialMounts = slices.Clone(hs.Agent.CredentialMounts)
	hs.Agent.VendorInstructions.Body = slices.Clone(hs.Agent.VendorInstructions.Body)
	hs.Agent.InstructionPolicy.Boundaries = slices.Clone(hs.Agent.InstructionPolicy.Boundaries)
	if hs.AuthStoreLease != nil {
		lease := *hs.AuthStoreLease
		hs.AuthStoreLease = &lease
	}
	return hs
}

func vendorInstructionsEqual(a, b ward.VendorInstructions) bool {
	return a.Vendor == b.Vendor && a.Delivery == b.Delivery &&
		a.Present == b.Present && a.Digest == b.Digest && slices.Equal(a.Body, b.Body)
}

// New constructs the driver and claims its state directory.
func New(cfg Config) (*Driver, error) {
	newError := func(message string) error {
		return errors.New("new " + cfg.ErrorPrefix + ": " + message)
	}
	switch {
	case cfg.ErrorPrefix == "":
		return nil, errors.New("new stage driver: error prefix is required")
	case cfg.DisplayName == "":
		return nil, newError("display name is required")
	case cfg.Provider == nil:
		return nil, newError("nil provider")
	case !cliSafeMountField(cfg.CredentialMount.Target):
		return nil, newError("credential mount target carries a CLI delimiter")
	case !cleanAbsoluteRoot(cfg.CredentialMount.Target):
		return nil, newError("credential mount target must be a clean absolute path")
	case !slices.Contains(ward.AllCredentialManifestPolicies, cfg.CredentialMount.Manifest):
		return nil, newError("credential mount manifest is invalid")
	case cfg.CredentialMount.Writable:
		return nil, newError("credential mount must be read-only under subscription_contained")
	case cfg.Lifetime == nil:
		return nil, newError("nil lifetime")
	case cfg.Dir == "":
		return nil, newError("state directory is required")
	case cfg.SeedRoot == "":
		return nil, newError("seed root is required")
	case !cleanAbsoluteRoot(cfg.ExportRoot):
		return nil, newError("clean absolute export root is required")
	case cfg.Gate == nil:
		return nil, newError("nil gate")
	case cfg.Seeder == nil:
		return nil, newError("nil seeder")
	case cfg.Exports == nil:
		return nil, newError("nil export recorder")
	case cfg.ImportStarts == nil:
		return nil, newError("nil import-start recorder")
	case cfg.Outcomes == nil:
		return nil, newError("nil outcome recorder")
	case cfg.Authority == nil:
		return nil, newError("nil admission authority")
	case cfg.Artifacts == nil:
		return nil, newError("nil artifact store")
	case cfg.ProviderConfigError != nil:
		return nil, fmt.Errorf("new %s: %w", cfg.ErrorPrefix, cfg.ProviderConfigError)
	case cfg.Now == nil:
		return nil, newError("nil clock")
	case len(cfg.Preparation) > 0 &&
		!slices.Equal(cfg.Preparation, []string{projectimage.PreparationPath}):
		// The preparation argv reaches the root launch command; accept only the
		// fixed image-owned helper (or none) so an arbitrary caller-supplied
		// command cannot execute as root beside the credential mount.
		return nil, newError(
			"preparation must be empty or the fixed project-image helper")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("new %s: %w", cfg.ErrorPrefix, err)
	}
	if err := os.MkdirAll(cfg.SeedRoot, 0o700); err != nil {
		return nil, fmt.Errorf("new %s: create seed root: %w", cfg.ErrorPrefix, err)
	}
	seedFS, err := os.OpenRoot(cfg.SeedRoot)
	if err != nil {
		return nil, fmt.Errorf("new %s: open seed root: %w", cfg.ErrorPrefix, err)
	}
	return &Driver{
		errorPrefix: cfg.ErrorPrefix, displayName: cfg.DisplayName,
		dir: cfg.Dir, seedRoot: cfg.SeedRoot, exportRoot: cfg.ExportRoot,
		gate: cfg.Gate, seeder: cfg.Seeder,
		seedFS:  seedFS,
		exports: cfg.Exports, importStarts: cfg.ImportStarts,
		outcomes: cfg.Outcomes, authority: cfg.Authority,
		artifacts: cfg.Artifacts, provider: cfg.Provider,
		credentialMount: cfg.CredentialMount,
		preJob:          cfg.PreJob,
		imports:         cfg.Import, prepare: slices.Clone(cfg.Preparation),
		now: cfg.Now, lifetime: cfg.Lifetime,
		logger: pipelineLogger(
			cfg.Logger, strings.ReplaceAll(cfg.ErrorPrefix, " ", "-"),
		),
		running:           map[domain.InvocationID]*session{},
		recovering:        map[domain.InvocationID]struct{}{},
		seedCleanupWarned: map[domain.InvocationID]string{},
	}, nil
}

// pipelineLogger normalizes the optional logger so the pipeline never has
// to check, and stamps the subsystem here rather than at each record.
func pipelineLogger(logger *slog.Logger, subsystem string) *slog.Logger {
	if logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return logger.With("subsystem", subsystem)
}

func cleanAbsoluteRoot(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != string(filepath.Separator)
}

func cliSafeMountField(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r == ',' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
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
	runID, err := d.validatedProviderRunID(id)
	if err != nil {
		return err
	}
	if _, err := d.loadIntentRegatedForRunID(ctx, id, runID, true); err == nil {
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
	prompt, err := d.provider.RenderPrompt(providerPromptInputsFrom(materialized))

	now := d.now().UTC()
	in := intent{
		InvocationID: id, RunID: runID, Phase: phaseSeeding, Spec: spec,
		Seed: filepath.Join(d.seedRoot, runID), Prompt: prompt,
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
				"close %s: load invocation %s: %w", d.errorPrefix, ref.id, err,
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
					"close %s: cancellation intent for %s: %w",
					d.errorPrefix, ref.runID, err,
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
			closeErrs = append(closeErrs, fmt.Errorf("close %s: %w", d.errorPrefix, ctx.Err()))
			return errors.Join(closeErrs...)
		}
	}
	d.seedMu.Lock()
	defer d.seedMu.Unlock()
	if d.seedFS != nil {
		if err := d.seedFS.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close %s seed root: %w", d.errorPrefix, err))
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
	case phaseSeeding, phaseRunning, phaseExported, phaseImportPending:
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
	case phaseSeeding, phaseRunning, phaseExported, phaseImportPending:
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
		d.reportTerminalSeedCleanup(in)
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
		Summary:      d.displayName + " invocation canceled by daemon request.",
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
	case phaseExported, phaseImportPending:
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
	d.reportTerminalSeedCleanup(in)
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
		d.reportTerminalSeedCleanup(in)
	}
	switch in.Phase {
	case phaseCommitted:
		return *in.Result, nil
	case phaseLost:
		return exec.StageResult{}, fmt.Errorf("invocation %s: %w", id, exec.ErrNoResult)
	case phaseSeeding, phaseRunning, phaseExported, phaseImportPending:
		return exec.StageResult{}, fmt.Errorf("invocation %s: %w", id, exec.ErrResultNotReady)
	}
	return exec.StageResult{}, fmt.Errorf("invocation %s: phase %q: %w", id, in.Phase, exec.ErrInvalidStatus)
}
