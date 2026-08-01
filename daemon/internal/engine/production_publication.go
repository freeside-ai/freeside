package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	// KindProductionPublicationRequested is the durable queue kind that owns
	// exact replay through verification and execution-bound publication.
	KindProductionPublicationRequested   = "production_publication_requested"
	productionPublicationTaskVersion     = "freeside.production-publication/v1"
	productionVerificationVersion        = "freeside.production-verification/v1"
	productionVerificationCheckpointKind = "production_verification_checkpoint"
	defaultProductionRecipeReadTimeout   = 2 * time.Minute
	defaultProductionHoldRetryInterval   = 30 * time.Second
	productionBlockRecipeRevoked         = "Current trust no longer approves the admitted project-image recipe."
	productionBlockVerification          = "Verification or current policy findings blocked production publication."
	productionBlockTrust                 = "Current trust state definitively blocked publication."
	productionBlockBaseAdvanced          = "The target base advanced after admission; rerun and reverify against the current base."
	productionBlockExternal              = "Publication is durably held because the external service permanently refused the committed operation. Repair that external state to resume recovery."
)

var (
	errProductionCrashSeam = errors.New("injected production crash seam")
	errProductionRetryable = errors.New("production publication retryable failure")
)

// ProductionReplay is the immutable-admission-reconstructed, directory-free
// source account returned by the producing driver. The workflow still treats
// every field as replay data: it independently derives the recorded policy,
// materializes the named blobs, re-runs the hostile importer, and requires the
// resulting head to equal the immutable export. Current trust is re-gated at
// publication, after this exact historical reconstruction.
type ProductionReplay struct {
	InvocationID           domain.InvocationID     `json:"invocation_id"`
	ObservedBaseSHA        string                  `json:"observed_base_sha"`
	HeadSHA                string                  `json:"head_sha"`
	Manifest               export.Manifest         `json:"manifest"`
	ManifestDigest         domain.Digest           `json:"manifest_digest"`
	Evidence               export.EvidenceManifest `json:"evidence"`
	EvidenceManifestDigest *domain.Digest          `json:"evidence_manifest_digest,omitempty"`
	CommitPlanDigest       *domain.Digest          `json:"commit_plan_digest,omitempty"`
	ImportOptions          importer.Options        `json:"import_options"`
}

// ProductionPublicationConfig supplies the real boundaries required after a
// production execution export: exact-base transport, networkless verifier,
// store-backed execution publisher, durable blobs, and authenticated replay.
type ProductionPublicationConfig struct {
	WorkDir         string
	Transport       PublicationTransport
	Publisher       *publish.Publisher
	Artifacts       ArtifactStore
	ApprovedRecipes map[domain.Digest]bool
	NewRoom         func(domain.ProjectImage) (ProductionVerificationRoom, error)
	// HoldOnly composes the durable publication boundary without advancing its
	// queue. Attended startup uses it to recognize publication tasks admitted
	// by an earlier unattended process while preserving the attended ban on
	// automatic verification, push, and pull-request effects.
	HoldOnly             bool
	RecipeReadTimeout    time.Duration
	HoldRetryInterval    time.Duration
	Now                  func() time.Time
	AfterVerification    func() error
	AfterPublication     func() error
	AfterReady           func() error
	AfterBlocked         func() error
	AfterTerminal        func() error
	AfterTaskLockRelease func() error
}

// ProductionVerificationRoom executes verification and returns the exact
// recipe bytes embedded in its admitted project image. Implementations must
// extract without candidate mounts, credentials, or network and authenticate
// the bytes against ProjectImage.RecipeDigest.
type ProductionVerificationRoom interface {
	verify.Room
	ReadRecipe(context.Context) ([]byte, error)
}

// WithProductionPublication makes the real production lane complete only
// after clean verification, execution-bound publication, and ready attention.
func WithProductionPublication(cfg ProductionPublicationConfig) Option {
	return func(e *Engine) error {
		workflow, err := newProductionPublicationWorkflow(e.store, e.signet, cfg)
		if err != nil {
			return fmt.Errorf("configure production publication: %w", err)
		}
		e.productionPublication = workflow
		return nil
	}
}

type productionPublicationTask struct {
	Version               string                `json:"version"`
	RunID                 domain.RunID          `json:"run_id"`
	ProjectID             domain.ProjectID      `json:"project_id"`
	ProducingInvocationID domain.InvocationID   `json:"producing_invocation_id"`
	VerificationID        domain.InvocationID   `json:"verification_invocation_id"`
	PublicationID         domain.InvocationID   `json:"publication_invocation_id"`
	HeadSHA               string                `json:"head_sha"`
	Artifacts             []domain.Digest       `json:"artifacts,omitempty"`
	Replay                ProductionReplay      `json:"replay"`
	Publication           ProductionPublication `json:"publication"`
	Summary               string                `json:"summary,omitempty"`
}

type productionVerificationCheckpoint struct {
	Version       string                        `json:"version"`
	TaskKey       string                        `json:"task_key"`
	ProjectImage  domain.Digest                 `json:"project_image"`
	Imported      importer.Result               `json:"imported"`
	Authorization domain.CandidateAuthorization `json:"authorization"`
	Artifacts     []domain.Artifact             `json:"artifacts"`
}

type productionPublicationWorkflow struct {
	store                *store.Store
	attention            attentionService
	workDir              string
	transport            PublicationTransport
	publisher            *publish.Publisher
	artifacts            ArtifactStore
	approvedRecipes      map[domain.Digest]bool
	newRoom              func(domain.ProjectImage) (ProductionVerificationRoom, error)
	holdOnly             bool
	recipeReadTimeout    time.Duration
	holdRetryInterval    time.Duration
	now                  func() time.Time
	holdRetryAfter       map[domain.RunID]time.Time
	afterVerification    func() error
	afterPublication     func() error
	afterReady           func() error
	afterBlocked         func() error
	afterTerminal        func() error
	afterTaskLockRelease func() error
	reconcileMu          sync.Mutex
}

type productionPublicationResult struct {
	completed int
	accepted  int
	ready     int
	blocked   int
	lastPR    int
}

func newProductionPublicationWorkflow(
	st *store.Store,
	attention attentionService,
	cfg ProductionPublicationConfig,
) (*productionPublicationWorkflow, error) {
	if st == nil || attention == nil || cfg.Transport == nil || cfg.Publisher == nil ||
		cfg.Artifacts == nil || cfg.NewRoom == nil {
		return nil, errors.New("nil dependency")
	}
	if strings.TrimSpace(cfg.WorkDir) == "" {
		return nil, errors.New("empty work directory")
	}
	workDir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve work directory: %w", err)
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create work directory: %w", err)
	}
	if len(cfg.ApprovedRecipes) == 0 && !cfg.HoldOnly {
		return nil, errors.New("approved recipe set is empty")
	}
	if cfg.RecipeReadTimeout < 0 {
		return nil, errors.New("negative recipe-read timeout")
	}
	if cfg.RecipeReadTimeout == 0 {
		cfg.RecipeReadTimeout = defaultProductionRecipeReadTimeout
	}
	if cfg.HoldRetryInterval < 0 {
		return nil, errors.New("negative hold-retry interval")
	}
	if cfg.HoldRetryInterval == 0 {
		cfg.HoldRetryInterval = defaultProductionHoldRetryInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &productionPublicationWorkflow{
		store: st, attention: attention, workDir: workDir,
		transport: cfg.Transport, publisher: cfg.Publisher, artifacts: cfg.Artifacts,
		approvedRecipes: mapsClone(cfg.ApprovedRecipes),
		newRoom:         cfg.NewRoom, holdOnly: cfg.HoldOnly,
		recipeReadTimeout: cfg.RecipeReadTimeout,
		holdRetryInterval: cfg.HoldRetryInterval, now: cfg.Now,
		holdRetryAfter:    make(map[domain.RunID]time.Time),
		afterVerification: cfg.AfterVerification,
		afterPublication:  cfg.AfterPublication, afterReady: cfg.AfterReady,
		afterBlocked: cfg.AfterBlocked, afterTerminal: cfg.AfterTerminal,
		afterTaskLockRelease: cfg.AfterTaskLockRelease,
	}, nil
}

func mapsClone[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

const productionPublicationTaskKeyPrefix = "production-publication/"

func productionPublicationTaskKey(runID domain.RunID) string {
	return productionPublicationTaskKeyPrefix + string(runID)
}

// productionRunIDFromPublicationTaskKey inverts productionPublicationTaskKey,
// reporting false for a key this lane could not have filed. Like the marker's
// inversion it attributes a row by its key, never by the payload that has
// just failed to reconstruct.
func productionRunIDFromPublicationTaskKey(key string) (domain.RunID, bool) {
	runID, ok := strings.CutPrefix(key, productionPublicationTaskKeyPrefix)
	if !ok || runID == "" {
		return "", false
	}
	return domain.RunID(runID), true
}

func productionVerificationCheckpointKey(runID domain.RunID) string {
	return "production-verification/" + string(runID)
}

func productionReadyItemID(runID domain.RunID) domain.ItemID {
	return domain.ItemID("production-ready-" + string(runID))
}

func productionBlockedItemID(runID domain.RunID) domain.ItemID {
	return domain.ItemID("production-publish-blocked-" + string(runID))
}

// hasQueuedCompletion reports whether the durable publication task has
// already accepted ownership of this completed invocation. Once that task is
// stored, reconciliation must not depend on the producing driver's private
// state: SQLite plus the artifact closure is the supported recovery frontier.
func (w *productionPublicationWorkflow) hasQueuedCompletion(
	ctx context.Context,
	run domain.Run,
	invocationID domain.InvocationID,
) (bool, error) {
	var entry store.QueueEntry
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, productionPublicationTaskKey(run.ID))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		// No task row: either none was ever written, or an operator repaired
		// an unreadable one by removing it. Absence proves the hold a task
		// notice describes has ended, and the pending scan can no longer
		// reach that row to retire it.
		return false, releaseProductionQuarantine(
			ctx, w.store, w.attention, productionTaskQuarantinePrefix, run.ID)
	}
	if err != nil {
		return false, err
	}
	task, err := decodeProductionPublicationTask(entry)
	if err != nil {
		// The acceptance scan reaches this row too, so it needs the pending
		// scan's hold: an unreadable task is a durable completion this daemon
		// cannot read, which is a reason to leave the attempt alone, not to
		// re-collect it or end the pass.
		if holdErr := w.holdUnreadableTask(ctx, run, err); holdErr != nil {
			return false, holdErr
		}
		return true, nil
	}
	if task.RunID != run.ID || task.ProjectID != run.ProjectID ||
		task.ProducingInvocationID != invocationID {
		return false, fmt.Errorf("queued production completion disagrees with run: %w",
			domain.ErrParentKeyMismatch)
	}
	if entry.Dispatched() {
		return false, fmt.Errorf("production publication task dispatched without a terminal record: %w",
			domain.ErrImmutableTransition)
	}
	// The acceptance scan runs in every operating mode, while the pending
	// scan that also releases this notice returns early under a hold-only
	// publication lane. Retiring it here too is what keeps a repaired task
	// from leaving an open notice behind in attended_dev.
	return true, releaseProductionQuarantine(
		ctx, w.store, w.attention, productionTaskQuarantinePrefix, run.ID)
}

// authenticatesTerminal proves a completed terminal from the SQLite-backed
// publication task and immutable export. Once publication owns completion,
// reconciliation must not call back into private driver state merely to
// authenticate history it has already durably accepted.
func (w *productionPublicationWorkflow) authenticatesTerminal(
	ctx context.Context,
	run domain.Run,
	terminal productionTerminalRecord,
) (bool, error) {
	var (
		entry           store.QueueEntry
		admission       domain.ExecutionAdmission
		executionExport domain.ExecutionExport
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, productionPublicationTaskKey(run.ID))
		if err != nil {
			return err
		}
		admission, err = tx.GetExecutionAdmissionRecord(ctx, terminal.InvocationID)
		if err != nil {
			return err
		}
		executionExport, err = tx.GetExecutionExportRecord(ctx, terminal.InvocationID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		// Any of the three lookups may be the missing one, and only an absent
		// task row proves a task notice's hold has ended: a missing admission
		// or export record must not be read as a repair.
		return false, w.releaseTaskQuarantineIfRowAbsent(ctx, run)
	}
	if err != nil {
		return false, err
	}
	task, err := decodeProductionPublicationTask(entry)
	if err != nil {
		// The acceptance scan reaches this row too, so it needs the pending
		// scan's hold: an unreadable task is a durable completion this daemon
		// cannot read, which is a reason to leave the attempt alone, not to
		// re-collect it or end the pass.
		if holdErr := w.holdUnreadableTask(ctx, run, err); holdErr != nil {
			return false, holdErr
		}
		return true, nil
	}
	if run.ID != task.RunID || run.ProjectID != task.ProjectID ||
		admission.RunID != task.RunID || admission.StageID != productionStageID(task.RunID) ||
		executionExport.AdmissionID != admission.ID ||
		executionExport.InvocationID != task.ProducingInvocationID ||
		executionExport.HeadSHA != task.HeadSHA ||
		task.Replay.ObservedBaseSHA != admission.Base.BaseSHA ||
		task.Replay.ManifestDigest != executionExport.ManifestDigest ||
		!sameOptionalDigest(task.Replay.EvidenceManifestDigest, executionExport.EvidenceManifestDigest) ||
		(task.Replay.CommitPlanDigest != nil) != executionExport.CommitPlanPresent ||
		!task.Replay.ImportOptions.CommitDate.Equal(executionExport.RecordedAt) ||
		terminal.Status != exec.StatusCompleted || terminal.InvocationID != task.ProducingInvocationID ||
		terminal.RunID != task.RunID || terminal.StageID != productionStageID(task.RunID) ||
		terminal.HeadSHA != task.HeadSHA || !slices.Equal(terminal.Artifacts, task.Artifacts) ||
		terminal.Summary != task.Summary {
		return false, fmt.Errorf("production terminal disagrees with durable publication task: %w",
			domain.ErrParentKeyMismatch)
	}
	return true, releaseProductionQuarantine(
		ctx, w.store, w.attention, productionTaskQuarantinePrefix, run.ID)
}

// RecordExecutionExport selects the persistence boundary from the immutable
// admission that owns the export, not from the daemon's current mode. A
// recovered unattended attempt therefore cannot be downgraded to an
// export-only write after an attended-mode restart.
func RecordExecutionExport(
	ctx context.Context,
	st *store.Store,
	executionExport domain.ExecutionExport,
	replay ProductionReplay,
) error {
	if st == nil {
		return errors.New("record execution export: nil store")
	}
	var admission domain.ExecutionAdmission
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, executionExport.InvocationID)
		return err
	}); err != nil {
		return err
	}
	switch admission.OperatingMode {
	case domain.ModeAttendedDev:
		return st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.RecordExecutionExport(ctx, executionExport)
		})
	case domain.ModeUnattended:
		var run domain.Run
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			run, err = tx.GetRun(ctx, admission.RunID)
			return err
		}); err != nil {
			return err
		}
		request, err := (&productionPublicationWorkflow{store: st}).loadProductionRequest(ctx, run)
		if err != nil {
			return err
		}
		if request.Legacy {
			return st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				return tx.RecordExecutionExport(ctx, executionExport)
			})
		}
		return RecordProductionExecutionExport(ctx, st, executionExport, replay)
	}
	return fmt.Errorf("execution admission %q has invalid operating mode %q: %w",
		admission.InvocationID, admission.OperatingMode, domain.ErrInvalidOperatingMode)
}

// RecordProductionExecutionExport atomically commits an unattended export and
// the publication task that owns every byte needed to replay it. A crash can
// therefore expose neither row or both rows, never an ExecutionExport whose
// completion still depends on the Claude driver's private state directory.
func RecordProductionExecutionExport(
	ctx context.Context,
	st *store.Store,
	executionExport domain.ExecutionExport,
	replay ProductionReplay,
) error {
	if st == nil {
		return errors.New("record production execution export: nil store")
	}
	if replay.InvocationID != executionExport.InvocationID ||
		replay.ObservedBaseSHA != executionExport.ObservedBaseSHA ||
		replay.HeadSHA != executionExport.HeadSHA ||
		replay.ManifestDigest != executionExport.ManifestDigest ||
		!sameOptionalDigest(replay.EvidenceManifestDigest, executionExport.EvidenceManifestDigest) ||
		(replay.CommitPlanDigest != nil) != executionExport.CommitPlanPresent ||
		!replay.ImportOptions.CommitDate.Equal(executionExport.RecordedAt) {
		return fmt.Errorf("production replay disagrees with execution export: %w", domain.ErrParentKeyMismatch)
	}
	if err := validateProductionReplayRecord(replay); err != nil {
		return err
	}

	var (
		admission domain.ExecutionAdmission
		run       domain.Run
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, executionExport.InvocationID)
		if err != nil {
			return err
		}
		run, err = tx.GetRun(ctx, admission.RunID)
		return err
	}); err != nil {
		return err
	}
	w := &productionPublicationWorkflow{store: st}
	request, err := w.loadProductionRequest(ctx, run)
	if err != nil {
		return err
	}
	if request.Legacy {
		return fmt.Errorf("legacy production invocation %q has no publication authority: %w",
			executionExport.InvocationID, domain.ErrParentKeyMismatch)
	}
	artifacts := make([]domain.Digest, 0, len(replay.Evidence.Entries))
	for _, entry := range replay.Evidence.Entries {
		artifacts = append(artifacts, domain.Digest(entry.Digest))
	}
	task := productionPublicationTask{
		Version: productionPublicationTaskVersion, RunID: run.ID, ProjectID: run.ProjectID,
		ProducingInvocationID: executionExport.InvocationID,
		VerificationID:        productionVerificationInvocationID(run.ID),
		PublicationID:         productionPublicationInvocationID(run.ID),
		HeadSHA:               executionExport.HeadSHA, Artifacts: artifacts,
		Replay: replay, Publication: request.Publication,
		Summary: fmt.Sprintf("Imported candidate %s over base %s.",
			executionExport.HeadSHA, executionExport.ObservedBaseSHA),
	}
	if err := task.validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		currentAdmission, err := tx.GetExecutionAdmissionRecord(ctx, task.ProducingInvocationID)
		if err != nil {
			return err
		}
		if currentAdmission.ID != admission.ID || currentAdmission.RunID != task.RunID ||
			currentAdmission.StageID != productionStageID(task.RunID) {
			return fmt.Errorf("production publication admission disagrees with task: %w",
				domain.ErrParentKeyMismatch)
		}
		currentRun, err := tx.GetRun(ctx, task.RunID)
		if err != nil {
			return err
		}
		if currentRun.ProjectID != task.ProjectID {
			return fmt.Errorf("production publication run disagrees with task: %w",
				domain.ErrParentKeyMismatch)
		}
		resolvedPolicy, err := tx.GetResolvedPolicy(ctx, task.RunID)
		if err != nil {
			return err
		}
		if currentAdmission.TrustProfileDigest == nil {
			return fmt.Errorf("unattended production admission has no trust profile: %w",
				domain.ErrParentKeyMismatch)
		}
		profile, err := tx.GetTrustProfile(ctx, *currentAdmission.TrustProfileDigest)
		if err != nil {
			return err
		}
		if err := validateProductionReplayOptions(productionBinding{
			run: currentRun, admission: currentAdmission,
			resolvedPolicy: resolvedPolicy, profile: profile, replay: task.Replay,
		}, task.Publication); err != nil {
			return err
		}
		if err := tx.RecordExecutionExportRecord(ctx, executionExport); err != nil {
			return err
		}
		reservation, err := publish.NewReservation(task.PublicationID, task.RunID)
		if err != nil {
			return err
		}
		if err := publish.ClaimInvocation(ctx, tx, reservation); err != nil {
			return err
		}
		entry, _, err := tx.EnqueueOutbox(
			ctx, productionPublicationTaskKey(task.RunID), KindProductionPublicationRequested, payload,
		)
		if err != nil {
			return err
		}
		if entry.Kind != KindProductionPublicationRequested || !bytes.Equal(entry.Payload, payload) {
			return fmt.Errorf("production publication task disagrees with stored row: %w", domain.ErrImmutableTransition)
		}
		return nil
	})
}

// quarantineTaskRow holds the run named by a publication task row this daemon
// cannot reconstruct. The run comes from the row's key, so its project — which
// the notice needs — is read from the store rather than from the payload that
// just failed to decode. A run the store does not have is not quarantinable
// state and stays the caller's loud failure.
func (w *productionPublicationWorkflow) quarantineTaskRow(
	ctx context.Context, runID domain.RunID,
) (bool, error) {
	var run domain.Run
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, w.holdUnreadableTask(ctx, run, nil)
}

// releaseTaskQuarantineIfRowAbsent retires the task notice only when the task
// row itself is gone, which is how an operator repairs an unreadable one that
// the pending scan can no longer reach.
func (w *productionPublicationWorkflow) releaseTaskQuarantineIfRowAbsent(
	ctx context.Context, run domain.Run,
) error {
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetOutbox(ctx, productionPublicationTaskKey(run.ID))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return releaseProductionQuarantine(
			ctx, w.store, w.attention, productionTaskQuarantinePrefix, run.ID)
	}
	return err
}

// holdUnreadableTask records one run's publication-task hold, keeping the
// cause with any store fault so the failure that triggered it is not dropped.
func (w *productionPublicationWorkflow) holdUnreadableTask(
	ctx context.Context, run domain.Run, cause error,
) error {
	if err := recordProductionQuarantine(
		ctx, w.store, w.attention, productionTaskQuarantinePrefix,
		run.ID, run.ProjectID, productionQuarantineUnreadableTask,
	); err != nil {
		return errors.Join(cause, err)
	}
	return nil
}

// quarantineTaskMarker reports whether this task's run is held out of the lane
// by its ownership marker. reconcileTask re-gates its own authority from the
// store and never reads the marker, so without this the publication lane would
// publish a run the dispatch and acceptance paths have already quarantined.
// A task whose marker reads again retires the marker's notice, the same
// release the ownership scan performs.
func (w *productionPublicationWorkflow) quarantineTaskMarker(
	ctx context.Context, task productionPublicationTask,
) (bool, error) {
	var entry store.QueueEntry
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(productionInvocationID(task.RunID)))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		// Marker presence is not this lane's gate; the task carries its own
		// authority. Leave the pre-existing behaviour of a missing row alone,
		// but retire the notice first: removing the bad row is one way an
		// operator repairs the marker, and this task is about to publish.
		return false, releaseProductionQuarantine(
			ctx, w.store, w.attention, productionMarkerQuarantinePrefix, task.RunID)
	}
	if err != nil {
		return false, err
	}
	if _, authErr := authenticateProductionMarker(entry, task.RunID); authErr != nil {
		// The project comes from the run row, not from the task payload: the
		// notice must not be filed under a project a decoded field claims.
		var run domain.Run
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			run, err = tx.GetRun(ctx, task.RunID)
			return err
		}); err != nil {
			return false, errors.Join(authErr, err)
		}
		quarantined, quarantineErr := quarantineProductionMarker(
			ctx, w.store, w.attention, run.ID, run.ProjectID, authErr)
		if quarantineErr != nil {
			return false, errors.Join(authErr, quarantineErr)
		}
		if quarantined {
			return true, nil
		}
		return false, authErr
	}
	if err := releaseProductionQuarantine(
		ctx, w.store, w.attention, productionMarkerQuarantinePrefix, task.RunID); err != nil {
		return false, err
	}
	return false, nil
}

func (w *productionPublicationWorkflow) loadProductionRequest(
	ctx context.Context,
	run domain.Run,
) (productionInvocationRequest, error) {
	var entry store.QueueEntry
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(productionInvocationID(run.ID)))
		return err
	})
	if err != nil {
		return productionInvocationRequest{}, fmt.Errorf("load production submission metadata: %w", err)
	}
	request, err := authenticateProductionMarker(entry, run.ID)
	if err != nil {
		return productionInvocationRequest{}, fmt.Errorf("authenticate production submission metadata: %w", err)
	}
	return request, nil
}

func (t productionPublicationTask) validate() error {
	if t.Version != productionPublicationTaskVersion || t.RunID == "" || t.ProjectID == "" ||
		t.ProducingInvocationID != productionInvocationID(t.RunID) ||
		t.VerificationID != productionVerificationInvocationID(t.RunID) ||
		t.PublicationID != productionPublicationInvocationID(t.RunID) ||
		!validCommitSHA(t.HeadSHA) {
		return fmt.Errorf("invalid production publication task: %w", domain.ErrParentKeyMismatch)
	}
	for _, artifact := range t.Artifacts {
		if !contentaddr.Valid(string(artifact)) {
			return fmt.Errorf("production publication task artifact %q is invalid", artifact)
		}
	}
	if t.Replay.InvocationID != t.ProducingInvocationID ||
		!validCommitSHA(t.Replay.ObservedBaseSHA) || t.Replay.HeadSHA != t.HeadSHA {
		return fmt.Errorf("production publication replay disagrees with task: %w", domain.ErrParentKeyMismatch)
	}
	if err := validateProductionReplayRecord(t.Replay); err != nil {
		return err
	}
	if err := t.Publication.Validate(); err != nil {
		return err
	}
	replayDigests := productionReplayDigests(t.Replay)
	if len(replayDigests) == 0 {
		return errors.New("production publication replay digests are absent")
	}
	for _, digest := range replayDigests {
		if !contentaddr.Valid(string(digest)) {
			return fmt.Errorf("production publication replay digest %q is invalid", digest)
		}
	}
	return nil
}

func validateProductionReplayRecord(replay ProductionReplay) error {
	manifest, err := replay.Manifest.Encode()
	if err != nil || digestProductionBytes(manifest) != replay.ManifestDigest {
		return fmt.Errorf("production replay manifest disagrees with its role digest: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if replay.EvidenceManifestDigest == nil {
		if replay.Evidence.Version != "" || len(replay.Evidence.Entries) != 0 {
			return fmt.Errorf("production replay carries evidence without a role digest: %w",
				domain.ErrParentKeyMismatch)
		}
	} else {
		evidence, err := replay.Evidence.Encode()
		if err != nil || digestProductionBytes(evidence) != *replay.EvidenceManifestDigest {
			return fmt.Errorf("production replay evidence disagrees with its role digest: %w",
				errors.Join(err, domain.ErrParentKeyMismatch))
		}
	}
	if replay.CommitPlanDigest != nil && !contentaddr.Valid(string(*replay.CommitPlanDigest)) {
		return fmt.Errorf("production replay commit-plan digest %q is invalid", *replay.CommitPlanDigest)
	}
	if !validCommitSHA(replay.ImportOptions.BaseSHA) || replay.ImportOptions.CommitDate.IsZero() {
		return fmt.Errorf("production replay import coordinates are invalid: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}

func productionReplayDigests(replay ProductionReplay) []domain.Digest {
	set := map[domain.Digest]struct{}{replay.ManifestDigest: {}}
	if replay.EvidenceManifestDigest != nil {
		set[*replay.EvidenceManifestDigest] = struct{}{}
	}
	if replay.CommitPlanDigest != nil {
		set[*replay.CommitPlanDigest] = struct{}{}
	}
	for _, entry := range replay.Manifest.Entries {
		if entry.Kind == export.EntryRegular && entry.Digest != nil {
			set[domain.Digest(*entry.Digest)] = struct{}{}
		}
	}
	for _, entry := range replay.Evidence.Entries {
		set[domain.Digest(entry.Digest)] = struct{}{}
	}
	out := make([]domain.Digest, 0, len(set))
	for digest := range set {
		out = append(out, digest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func decodeProductionPublicationTask(entry store.QueueEntry) (productionPublicationTask, error) {
	if entry.Kind != KindProductionPublicationRequested {
		return productionPublicationTask{}, fmt.Errorf("task %q has kind %q: %w",
			entry.IdempotencyKey, entry.Kind, domain.ErrParentKeyMismatch)
	}
	decoder := json.NewDecoder(bytes.NewReader(entry.Payload))
	decoder.DisallowUnknownFields()
	var task productionPublicationTask
	if err := decoder.Decode(&task); err != nil {
		return productionPublicationTask{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return productionPublicationTask{}, errors.New("production publication task has trailing content")
	}
	if err := task.validate(); err != nil {
		return productionPublicationTask{}, err
	}
	if entry.IdempotencyKey != productionPublicationTaskKey(task.RunID) {
		return productionPublicationTask{}, fmt.Errorf("production publication task key disagrees with run: %w",
			domain.ErrParentKeyMismatch)
	}
	return task, nil
}

func (w *productionPublicationWorkflow) reconcile(ctx context.Context) (productionPublicationResult, error) {
	w.reconcileMu.Lock()
	defer w.reconcileMu.Unlock()
	if w.holdOnly {
		return productionPublicationResult{}, nil
	}
	var pending []store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, KindProductionPublicationRequested)
		return err
	}); err != nil {
		return productionPublicationResult{}, err
	}
	w.pruneHeldTaskRetries(pending)
	var result productionPublicationResult
	var joined error
	for _, entry := range pending {
		task, err := decodeProductionPublicationTask(entry)
		if err != nil {
			// A task row this daemon cannot reconstruct is the marker's
			// failure mode one row over: a newer daemon writes a newer task
			// too, so joining the error here would end Engine.Run on every
			// pass for as long as the row exists (#424). Quarantine the run
			// it names and leave the row pending for a daemon that can read
			// it. A row whose key this lane could not have filed names no
			// run, so it stays loud.
			if runID, ok := productionRunIDFromPublicationTaskKey(entry.IdempotencyKey); ok {
				quarantined, quarantineErr := w.quarantineTaskRow(ctx, runID)
				if quarantineErr != nil {
					joined = errors.Join(joined, fmt.Errorf(
						"task %q: %w", entry.IdempotencyKey, quarantineErr))
					continue
				}
				if quarantined {
					continue
				}
			}
			joined = errors.Join(joined, fmt.Errorf(
				"task %q cannot be reconstructed: %w: %w",
				entry.IdempotencyKey, err, domain.ErrParentKeyMismatch,
			))
			continue
		}
		// The task reads again, so its own hold has ended. It has to be
		// retired here, while the row is still pending: a task that completes
		// leaves the pending scan, and no later pass would reach its notice.
		if err := releaseProductionQuarantine(
			ctx, w.store, w.attention,
			productionTaskQuarantinePrefix, task.RunID,
		); err != nil {
			joined = errors.Join(joined, fmt.Errorf("task %q: %w", entry.IdempotencyKey, err))
			continue
		}
		// The publication lane holds a quarantined run too. reconcileTask
		// re-gates its own authority from the store and never reads the
		// marker, so without this check a run whose marker stopped
		// authenticating would still import, verify, and open a real pull
		// request while its notice claims it is held out of the lane.
		markerQuarantined, err := w.quarantineTaskMarker(ctx, task)
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("task %q: %w", entry.IdempotencyKey, err))
			continue
		}
		if markerQuarantined {
			continue
		}
		if retryAfter, held := w.holdRetryAfter[task.RunID]; held {
			if w.now().Before(retryAfter) {
				continue
			}
			delete(w.holdRetryAfter, task.RunID)
		}
		lockDir := filepath.Join(w.workDir, "task-locks")
		if err := os.MkdirAll(lockDir, 0o700); err != nil {
			w.deferHeldTask(task)
			continue
		}
		release, err := acquireFakePublicationLock(ctx, lockDir, entry.IdempotencyKey)
		if err != nil {
			w.deferHeldTask(task)
			continue
		}
		outcome, reconcileErr := w.reconcileTask(ctx, task)
		releaseErr := release()
		if w.afterTaskLockRelease != nil {
			releaseErr = errors.Join(releaseErr, w.afterTaskLockRelease())
		}
		if releaseErr != nil {
			// The task may already be durably complete. Preserve its outcome;
			// lock cleanup is environmental and cannot invalidate committed work.
			w.deferHeldTask(task)
		}
		if reconcileErr != nil {
			if productionPublicationRetryableFailure(reconcileErr) {
				w.deferHeldTask(task)
			} else {
				joined = errors.Join(joined, fmt.Errorf("task %q: %w", entry.IdempotencyKey, reconcileErr))
			}
			continue
		}
		result.completed += boolCount(outcome.completed)
		result.accepted += boolCount(outcome.accepted)
		result.ready += boolCount(outcome.ready)
		result.blocked += boolCount(outcome.blocked)
		if outcome.prNumber > 0 {
			result.lastPR = outcome.prNumber
		}
	}
	return result, joined
}

// productionPublicationStateContradiction is the durable-state error class
// that must continue to escape into Engine.Run. Untyped errors also fail loud:
// only errors positively identified as retryable are contained below.
func productionPublicationStateContradiction(err error) bool {
	return errors.Is(err, domain.ErrParentKeyMismatch) ||
		errors.Is(err, errProductionCrashSeam) ||
		errors.Is(err, domain.ErrImmutableTransition) ||
		errors.Is(err, domain.ErrInvalidOperatingMode) ||
		errors.Is(err, domain.ErrPathBoundaryMismatch) ||
		errors.Is(err, store.ErrNotFound) ||
		errors.Is(err, store.ErrImmutableConflict) ||
		errors.Is(err, store.ErrStaleWrite)
}

func productionPublicationRetryableError(err error) error {
	return errors.Join(err, errProductionRetryable)
}

// productionPublicationRetryableFailure recognizes failures at explicit
// environmental boundaries. Durable reconstruction errors are deliberately
// absent, so an untyped corruption cannot become an immortal pending task.
func productionPublicationRetryableFailure(err error) bool {
	if productionPublicationStateContradiction(err) {
		return false
	}
	var networkError net.Error
	var pathError *os.PathError
	var gitError *publish.TransportGitError
	return errors.Is(err, errProductionRetryable) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, publish.ErrGitHubAPI) ||
		errors.Is(err, publish.ErrJanitorInactive) ||
		errors.Is(err, publish.ErrInstallationGrantUntrusted) ||
		errors.As(err, &gitError) ||
		errors.As(err, &networkError) ||
		errors.As(err, &pathError) ||
		errors.Is(err, os.ErrPermission)
}

func productionPublicationPermanentExternalFailure(err error) bool {
	if errors.Is(err, publish.ErrRemoteMissingBase) ||
		errors.Is(err, publish.ErrNoInstallation) ||
		errors.Is(err, publish.ErrNoAppCredentials) ||
		errors.Is(err, publish.ErrNoAppRegistration) ||
		errors.Is(err, publish.ErrPendingAppAuthority) ||
		errors.Is(err, publish.ErrAmbiguousInstallation) ||
		errors.Is(err, publish.ErrInstallationResolution) ||
		errors.Is(err, publish.ErrGrantMismatch) {
		return true
	}
	var apiError *publish.APIError
	if errors.As(err, &apiError) {
		return apiError.Status >= http.StatusBadRequest &&
			apiError.Status < http.StatusInternalServerError &&
			apiError.Status != http.StatusRequestTimeout &&
			apiError.Status != http.StatusTooManyRequests &&
			apiError.Status != http.StatusForbidden
	}
	var gitError *publish.TransportGitError
	return errors.As(err, &gitError) && gitError.Refusal == publish.RefusalAuth
}

func (w *productionPublicationWorkflow) pruneHeldTaskRetries(pending []store.QueueEntry) {
	pendingKeys := make(map[string]struct{}, len(pending))
	for _, entry := range pending {
		pendingKeys[entry.IdempotencyKey] = struct{}{}
	}
	for runID := range w.holdRetryAfter {
		if _, found := pendingKeys[productionPublicationTaskKey(runID)]; !found {
			delete(w.holdRetryAfter, runID)
		}
	}
}

type productionTaskOutcome struct {
	completed bool
	accepted  bool
	ready     bool
	blocked   bool
	prNumber  int
}

type productionBinding struct {
	run            domain.Run
	admission      domain.ExecutionAdmission
	export         domain.ExecutionExport
	resolvedPolicy domain.ResolvedPolicy
	replay         ProductionReplay
	profile        domain.AutomationTrustProfile
	image          domain.ProjectImage
}

func (w *productionPublicationWorkflow) loadBinding(
	ctx context.Context, task productionPublicationTask,
) (productionBinding, error) {
	var binding productionBinding
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding.run, err = tx.GetRun(ctx, task.RunID)
		if err != nil {
			return err
		}
		binding.admission, err = tx.GetExecutionAdmissionRecord(ctx, task.ProducingInvocationID)
		if err != nil {
			return err
		}
		binding.export, err = tx.GetExecutionExportRecord(ctx, task.ProducingInvocationID)
		if err != nil {
			return err
		}
		binding.resolvedPolicy, err = tx.GetResolvedPolicy(ctx, task.RunID)
		if err != nil {
			return err
		}
		if binding.admission.TrustProfileDigest == nil {
			binding.profile, err = tx.LatestTrustProfile(ctx, binding.admission.Base.Repo)
		} else {
			binding.profile, err = tx.GetTrustProfile(ctx, *binding.admission.TrustProfileDigest)
		}
		if err != nil {
			return err
		}
		images, err := tx.ListProjectImages(ctx, binding.admission.Base.RepositoryID)
		if err != nil {
			return err
		}
		for _, image := range images {
			if image.ImageRef == binding.admission.ImageRef {
				if binding.image.ID != "" {
					return fmt.Errorf("multiple project-image records name the admitted image: %w",
						domain.ErrParentKeyMismatch)
				}
				binding.image = image
			}
		}
		if binding.image.ID == "" {
			return fmt.Errorf("admitted image has no project-image record: %w",
				domain.ErrParentKeyMismatch)
		}
		return nil
	})
	if err != nil {
		return productionBinding{}, fmt.Errorf("load production publication authority: %w", err)
	}
	binding.replay = task.Replay
	if binding.run.ID != task.RunID || binding.run.ProjectID != task.ProjectID ||
		binding.admission.RunID != task.RunID ||
		binding.admission.StageID != productionStageID(task.RunID) ||
		binding.export.AdmissionID != binding.admission.ID ||
		binding.export.InvocationID != task.ProducingInvocationID ||
		binding.export.HeadSHA != task.HeadSHA ||
		binding.replay.InvocationID != task.ProducingInvocationID ||
		binding.replay.HeadSHA != task.HeadSHA ||
		binding.replay.ObservedBaseSHA != binding.admission.Base.BaseSHA ||
		binding.replay.ManifestDigest != binding.export.ManifestDigest ||
		!sameOptionalDigest(binding.replay.EvidenceManifestDigest, binding.export.EvidenceManifestDigest) ||
		(binding.replay.CommitPlanDigest != nil) != binding.export.CommitPlanPresent ||
		!binding.replay.ImportOptions.CommitDate.Equal(binding.export.RecordedAt) ||
		(binding.admission.TrustProfileDigest != nil &&
			binding.profile.ProfileDigest != *binding.admission.TrustProfileDigest) ||
		binding.profile.Repo != binding.admission.Base.Repo ||
		binding.profile.RepositoryID != binding.admission.Base.RepositoryID ||
		binding.image.Repository != binding.admission.Base.Repo ||
		binding.image.RepositoryID != binding.admission.Base.RepositoryID ||
		binding.image.CommitSHA != binding.admission.Base.BaseSHA {
		return productionBinding{}, fmt.Errorf("production publication binding disagrees with durable authority: %w",
			domain.ErrParentKeyMismatch)
	}
	if err := validateProductionReplayOptions(binding, task.Publication); err != nil {
		return productionBinding{}, err
	}
	return binding, nil
}

func validateProductionReplayOptions(
	binding productionBinding,
	publication ProductionPublication,
) error {
	if binding.resolvedPolicy.RunID != binding.run.ID ||
		binding.resolvedPolicy.Digest != binding.admission.PolicyDigest {
		return fmt.Errorf("production replay policy disagrees with admission: %w",
			domain.ErrParentKeyMismatch)
	}
	var allowedPaths []string
	for _, key := range binding.resolvedPolicy.Keys {
		if key.Key != "paths" {
			continue
		}
		for _, path := range strings.Split(key.Value, ",") {
			if trimmed := strings.TrimSpace(path); trimmed != "" {
				allowedPaths = append(allowedPaths, trimmed)
			}
		}
		break
	}
	if !explicitProductionPaths(allowedPaths) {
		return fmt.Errorf("production replay policy has no explicit path boundary: %w",
			domain.ErrPathBoundaryMismatch)
	}
	policy, err := (importer.Policy{Allowlist: allowedPaths}).WithProtectedPaths(binding.profile)
	if err != nil {
		return err
	}
	want := importer.Options{
		BaseSHA:     binding.admission.Base.BaseSHA,
		CommitDate:  binding.replay.ImportOptions.CommitDate,
		AuthorName:  publication.CommitAuthor.Name(),
		AuthorEmail: publication.CommitAuthor.Email(),
		Policy:      policy,
	}
	if binding.replay.ImportOptions.CommitDate.IsZero() ||
		!reflect.DeepEqual(binding.replay.ImportOptions, want) {
		return fmt.Errorf("production replay import options disagree with durable policy: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}

func explicitProductionPaths(patterns []string) bool {
	if len(patterns) == 0 || importer.ValidatePathPatterns(patterns) != nil {
		return false
	}
	for _, pattern := range patterns {
		first, _, _ := strings.Cut(pattern, "/")
		if first == "" || first == "." || first == ".." ||
			strings.ContainsAny(first, `*?[\`) {
			return false
		}
	}
	return true
}

// ProductionPublicationBackupPayloadDigests validates a durable production
// task and returns every blob its replay and original result require.
func ProductionPublicationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	task, err := decodeProductionPublicationTask(entry)
	if err != nil {
		return nil, err
	}
	return append(productionReplayDigests(task.Replay), task.Artifacts...), nil
}

func sameOptionalDigest(a, b *domain.Digest) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (w *productionPublicationWorkflow) reconcileTask(
	ctx context.Context, task productionPublicationTask,
) (productionTaskOutcome, error) {
	binding, err := w.loadBinding(ctx, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	scratch, err := os.MkdirTemp(w.workDir, ".production-publication-")
	if err != nil {
		return productionTaskOutcome{}, err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // run-owned scratch
	parentInfo, err := os.Stat(scratch)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	checkout, err := w.transport.FetchBase(
		ctx, binding.admission.Base.Repo, binding.admission.Base.BaseRef,
		binding.admission.Base.BaseSHA, filepath.Join(scratch, "checkout"),
	)
	if err != nil {
		if isDefinitiveTrustRefusal(err) {
			return w.holdBlockedTask(
				ctx, task, importer.Result{CommitSHA: task.HeadSHA}, productionBlockTrust,
			)
		}
		if productionPublicationPermanentExternalFailure(err) {
			return w.holdBlockedTask(
				ctx, task, importer.Result{CommitSHA: task.HeadSHA}, productionBlockExternal,
			)
		}
		if productionPublicationRetryableFailure(err) {
			err = productionPublicationRetryableError(err)
		}
		return productionTaskOutcome{}, fmt.Errorf("fetch exact publication base: %w", err)
	}
	checkoutDir, err := validatePublicationCheckoutBinding(
		checkout, binding.admission.Base.Repo, binding.admission.Base.BaseRef,
		binding.admission.Base.BaseSHA, filepath.Join(scratch, "checkout"), parentInfo,
	)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	handoffDir := filepath.Join(scratch, "handoff")
	if err := w.materializeReplay(binding.replay, handoffDir); err != nil {
		return productionTaskOutcome{}, err
	}
	options := binding.replay.ImportOptions
	if options.BaseSHA != binding.admission.Base.BaseSHA || options.CommitDate.IsZero() {
		return productionTaskOutcome{}, fmt.Errorf("replay import options lost base or commit-date binding: %w",
			domain.ErrParentKeyMismatch)
	}
	imported, err := importer.Import(ctx, handoffDir, checkoutDir, options)
	if err != nil {
		return productionTaskOutcome{}, fmt.Errorf("reconstruct execution export: %w", err)
	}
	if imported.CommitSHA != binding.export.HeadSHA || len(imported.Findings) != 0 {
		return productionTaskOutcome{}, fmt.Errorf("reconstructed execution export produced head %q with %d findings, want clean %q: %w",
			imported.CommitSHA, len(imported.Findings), binding.export.HeadSHA, domain.ErrParentKeyMismatch)
	}

	checkpoint, found, err := w.loadCheckpoint(ctx, task, binding)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	readyExists, err := w.hasReadyItemRecord(ctx, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if readyExists && !found {
		return productionTaskOutcome{}, fmt.Errorf(
			"ready item %q has no verification checkpoint: %w",
			productionReadyItemID(task.RunID), domain.ErrParentKeyMismatch,
		)
	}
	if found && !reflect.DeepEqual(checkpoint.Imported, imported) {
		return productionTaskOutcome{}, fmt.Errorf("verification checkpoint import account disagrees with reconstruction: %w",
			domain.ErrParentKeyMismatch)
	}
	blocked, err := w.recoverDefinitiveBlockedTask(
		ctx, task, binding, imported, checkpoint, found, readyExists,
	)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if blocked != nil {
		return *blocked, nil
	}
	if found {
		candidate := productionCandidate(task, binding, checkpoint)
		if readyExists {
			published, err := w.loadReadyPublicationOutcome(
				ctx, task, binding, checkpoint, candidate,
			)
			if err != nil {
				return productionTaskOutcome{}, err
			}
			return w.completePublishedTask(ctx, task, binding, checkpoint, published)
		}
		published, outcomeFound, err := w.loadPublicationOutcome(
			ctx, task, candidate, w.publisher.VerifyOutcome,
		)
		if err != nil {
			if isDurablePublicationConflict(err) {
				return w.holdBlockedTask(
					ctx, task, imported,
					"Publication is durably held because the external branch or pull request conflicts with the committed identity. Inspect and repair that external state to resume recovery.",
				)
			}
			if productionPublicationPermanentExternalFailure(err) {
				return w.holdBlockedTask(ctx, task, imported, productionBlockExternal)
			}
			return productionTaskOutcome{}, err
		}
		if outcomeFound {
			return w.completePublishedTask(ctx, task, binding, checkpoint, published)
		}
	}
	if !w.approvedRecipes[binding.image.RecipeDigest] {
		pendingIntent, err := w.hasPendingPublicationIntent(ctx, task.PublicationID)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if pendingIntent {
			return w.holdBlockedTask(
				ctx, task, imported,
				"Publication is durably held because current trust no longer approves the admitted project-image recipe. Restore that approval to recover the committed publication intent.",
			)
		}
		return w.completeBlockedTask(
			ctx, task, binding.run, imported, nil,
			productionBlockRecipeRevoked,
		)
	}
	if !found {
		checkpoint, err = w.verifyAndCheckpoint(ctx, task, binding, imported, checkoutDir)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if w.afterVerification != nil {
			if err := w.afterVerification(); err != nil {
				return productionTaskOutcome{}, fmt.Errorf("after production verification: %w",
					errors.Join(err, errProductionCrashSeam))
			}
		}
	}
	if !checkpoint.Authorization.AuthorizesPublication {
		return w.completeBlockedTask(
			ctx, task, binding.run, checkpoint.Imported, checkpoint.Artifacts,
			productionBlockVerification,
		)
	}

	candidate := productionCandidate(task, binding, checkpoint)
	if published, found, err := w.loadPublicationOutcome(
		ctx, task, candidate, w.publisher.VerifyOutcome,
	); err != nil {
		if isDurablePublicationConflict(err) {
			return w.holdBlockedTask(
				ctx, task, checkpoint.Imported,
				"Publication is durably held because the external branch or pull request conflicts with the committed identity. Inspect and repair that external state to resume recovery.",
			)
		}
		if productionPublicationPermanentExternalFailure(err) {
			return w.holdBlockedTask(ctx, task, checkpoint.Imported, productionBlockExternal)
		}
		return productionTaskOutcome{}, err
	} else if found {
		return w.completePublishedTask(ctx, task, binding, checkpoint, published)
	}
	published, err := w.publisher.PublishExecutionAfterGateAndFinalize(
		ctx,
		publish.ExecutionCandidate{Candidate: candidate, ProducingInvocationID: task.ProducingInvocationID},
		w.approvedRecipes,
		func(ctx context.Context, gated publish.GatedHead) error {
			_, err := w.transport.PushHead(ctx, checkout, gated)
			if err != nil && productionPublicationRetryableFailure(err) {
				return productionPublicationRetryableError(err)
			}
			return err
		},
	)
	if err != nil {
		if isDurablePublicationConflict(err) {
			return w.holdBlockedTask(
				ctx, task, checkpoint.Imported,
				"Publication is durably held because the external branch or pull request conflicts with the committed identity. Inspect and repair that external state to resume recovery.",
			)
		}
		if productionPublicationPermanentExternalFailure(err) {
			return w.holdBlockedTask(ctx, task, checkpoint.Imported, productionBlockExternal)
		}
		if isDefinitiveTrustRefusal(err) {
			pendingIntent, pendingErr := w.hasPendingPublicationIntent(ctx, task.PublicationID)
			if pendingErr != nil {
				return productionTaskOutcome{}, errors.Join(err, pendingErr)
			}
			if pendingIntent {
				return w.holdBlockedTask(
					ctx, task, checkpoint.Imported,
					"Publication is durably held because current trust definitively refused the committed publication intent. Repair the trust failure to resume recovery.",
				)
			}
			reason := productionBlockTrust
			if errors.Is(err, publish.ErrTargetBaseAdvanced) {
				reason = productionBlockBaseAdvanced
			}
			outcome, blockErr := w.completeBlockedTask(
				ctx, task, binding.run, checkpoint.Imported, checkpoint.Artifacts,
				reason,
			)
			if blockErr != nil {
				return productionTaskOutcome{}, errors.Join(err, blockErr)
			}
			return outcome, nil
		}
		if productionPublicationRetryableFailure(err) {
			return productionTaskOutcome{}, err
		}
		return productionTaskOutcome{}, fmt.Errorf("publish execution candidate: %w", err)
	}
	if w.afterPublication != nil {
		if err := w.afterPublication(); err != nil {
			return productionTaskOutcome{}, fmt.Errorf("after production publication: %w",
				errors.Join(err, errProductionCrashSeam))
		}
	}
	return w.completePublishedTask(ctx, task, binding, checkpoint, published)
}

func (w *productionPublicationWorkflow) completePublishedTask(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
) (productionTaskOutcome, error) {
	redactedCheckpoint := checkpoint
	if !w.approvedRecipes[binding.image.RecipeDigest] {
		// A ready item may have committed before or after recipe revocation. The
		// former legitimately retains evidence authenticated under the finalized
		// outcome; the latter must omit evidence that current policy rejects.
		// Reconstruct both exact states so either crash frontier can converge.
		redactedCheckpoint.Artifacts = nil
	}
	readyExists, err := w.hasCompatibleReadyItem(ctx, task, binding, checkpoint, published)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if !readyExists {
		ready, err := w.readyItem(task, redactedCheckpoint, published)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if err := w.attention.PutItem(ctx, ready); err != nil {
			return productionTaskOutcome{}, err
		}
	}
	if w.afterReady != nil {
		if err := w.afterReady(); err != nil {
			return productionTaskOutcome{}, fmt.Errorf("after production ready item: %w",
				errors.Join(err, errProductionCrashSeam))
		}
	}
	if err := w.supersedeBlockedHold(ctx, task); err != nil {
		return productionTaskOutcome{}, err
	}
	accepted, err := w.recordCompletedTerminal(ctx, binding.run, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if w.afterTerminal != nil {
		if err := w.afterTerminal(); err != nil {
			return productionTaskOutcome{}, fmt.Errorf("after production terminal: %w",
				errors.Join(err, errProductionCrashSeam))
		}
	}
	if err := w.finishTask(ctx, task); err != nil {
		return productionTaskOutcome{}, err
	}
	return productionTaskOutcome{
		completed: true, accepted: accepted, ready: true, prNumber: published.PRNumber,
	}, nil
}

func (w *productionPublicationWorkflow) hasCompatibleReadyItem(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
) (bool, error) {
	historicalRecipes := mapsClone(w.approvedRecipes)
	historicalRecipes[binding.image.RecipeDigest] = true
	expectedReady, err := w.readyItemWithRecipes(task, checkpoint, published, historicalRecipes)
	if err != nil {
		return false, err
	}
	var expectedRedacted domain.AttentionItem
	if !w.approvedRecipes[binding.image.RecipeDigest] {
		redactedCheckpoint := checkpoint
		redactedCheckpoint.Artifacts = nil
		expectedRedacted, err = w.readyItemWithRecipes(
			task, redactedCheckpoint, published, historicalRecipes,
		)
		if err != nil {
			return false, err
		}
	}
	var current domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		current, err = tx.GetAttentionItemRecord(ctx, expectedReady.ID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if compatibleTerminalItem(expectedReady, current) ||
		(expectedRedacted.ID != "" && compatibleTerminalItem(expectedRedacted, current)) {
		return true, nil
	}
	return false, fmt.Errorf("ready item %q disagrees with finalized publication: %w",
		expectedReady.ID, domain.ErrParentKeyMismatch)
}

func (w *productionPublicationWorkflow) holdBlockedTask(
	ctx context.Context,
	task productionPublicationTask,
	imported importer.Result,
	reason string,
) (productionTaskOutcome, error) {
	item, err := w.blockedHoldItem(task, imported, reason)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	var current domain.AttentionItem
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		current, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	})
	if err == nil {
		if current.ProjectID != item.ProjectID || current.Subject.Type != item.Subject.Type ||
			current.Subject.ID != item.Subject.ID || current.Subject.RunID == nil ||
			item.Subject.RunID == nil || *current.Subject.RunID != *item.Subject.RunID ||
			current.Type != item.Type || current.PRHeadSHA != item.PRHeadSHA ||
			current.Status != domain.StatusOpen {
			return productionTaskOutcome{}, fmt.Errorf(
				"production publication hold %q disagrees with task: %w",
				item.ID, domain.ErrParentKeyMismatch,
			)
		}
		if current.Reason == item.Reason &&
			slices.Equal(current.RequestedDecision, item.RequestedDecision) &&
			reflect.DeepEqual(current.AgentClaims, item.AgentClaims) &&
			reflect.DeepEqual(current.CommitPlanNotice, item.CommitPlanNotice) {
			w.deferHeldTask(task)
			return productionTaskOutcome{}, nil
		}
		item.ItemVersion = current.ItemVersion + 1
		item.Timing = current.Timing
		item.ConversationID = current.ConversationID
		item.ExpiresWhen = current.ExpiresWhen
		if err := w.attention.PutItem(ctx, item); err != nil {
			return productionTaskOutcome{}, err
		}
		w.deferHeldTask(task)
		return productionTaskOutcome{}, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return productionTaskOutcome{}, err
	}
	if err := w.attention.PutItem(ctx, item); err != nil {
		return productionTaskOutcome{}, err
	}
	w.deferHeldTask(task)
	return productionTaskOutcome{blocked: true}, nil
}

func (w *productionPublicationWorkflow) deferHeldTask(task productionPublicationTask) {
	w.holdRetryAfter[task.RunID] = w.now().Add(w.holdRetryInterval)
}

func (w *productionPublicationWorkflow) supersedeBlockedHold(
	ctx context.Context,
	task productionPublicationTask,
) error {
	var item domain.AttentionItem
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(ctx, productionBlockedItemID(task.RunID))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if item.ProjectID != task.ProjectID || item.Subject.Type != domain.SubjectRun ||
		item.Subject.ID != domain.SubjectID(task.RunID) || item.Subject.RunID == nil ||
		*item.Subject.RunID != task.RunID || item.Type != domain.AttentionPublishBlocked {
		return fmt.Errorf(
			"production publication hold %q disagrees with task: %w",
			item.ID, domain.ErrParentKeyMismatch,
		)
	}
	if item.Status != domain.StatusOpen {
		return nil
	}
	item.Status = domain.StatusSuperseded
	item.ItemVersion++
	return w.attention.PutItem(ctx, item)
}

// loadReadyPublicationOutcome authenticates the crash frontier after ready
// attention committed but before terminal/task cleanup. The ready item is the
// durable proof that live outcome verification already succeeded; without its
// exact finalized outcome, authorization, checkpoint, and task bindings this
// path fails closed rather than consulting mutable forge state.
func (w *productionPublicationWorkflow) loadReadyPublicationOutcome(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	candidate publish.Candidate,
) (publish.Result, error) {
	published, found, err := w.loadPublicationOutcome(ctx, task, candidate, func(
		ctx context.Context,
		_ publish.Candidate,
		identity publish.Identity,
		outcome publish.Outcome,
	) error {
		published := publish.Result{
			Identity: identity, Branch: outcome.Branch, PRNumber: outcome.PRNumber,
		}
		compatible, err := w.hasCompatibleReadyItem(ctx, task, binding, checkpoint, published)
		if err != nil {
			return err
		}
		if !compatible {
			return fmt.Errorf("ready item disappeared during finalized recovery: %w", store.ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return publish.Result{}, err
	}
	if !found {
		return publish.Result{}, fmt.Errorf(
			"ready item %q has no finalized publication outcome: %w",
			productionReadyItemID(task.RunID), domain.ErrParentKeyMismatch,
		)
	}
	return published, nil
}

func (w *productionPublicationWorkflow) hasReadyItemRecord(
	ctx context.Context, task productionPublicationTask,
) (bool, error) {
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItemRecord(ctx, productionReadyItemID(task.RunID))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (w *productionPublicationWorkflow) loadPublicationOutcome(
	ctx context.Context,
	task productionPublicationTask,
	candidate publish.Candidate,
	verify publish.OutcomeVerifier,
) (publish.Result, bool, error) {
	identity, err := fakePublicationIdentity(candidate)
	if err != nil {
		return publish.Result{}, false, err
	}
	intentKey, err := publish.IntentKey(candidate.InvocationID, publish.IntentKindPublication)
	if err != nil {
		return publish.Result{}, false, err
	}
	var entry store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, intentKey)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return publish.Result{}, false, nil
		}
		return publish.Result{}, false, err
	}
	if entry.IdempotencyKey != intentKey {
		return publish.Result{}, false, fmt.Errorf(
			"production publication intent read back the wrong key: %w", domain.ErrParentKeyMismatch,
		)
	}
	if entry.Kind == publish.IntentKindReservation {
		held, err := publish.DecodeReservation(entry.Payload)
		if err != nil {
			return publish.Result{}, false, fmt.Errorf(
				"decode durable production publication reservation: %w",
				errors.Join(err, domain.ErrParentKeyMismatch),
			)
		}
		if held.InvocationID != task.PublicationID || held.RunID != task.RunID {
			return publish.Result{}, false, fmt.Errorf(
				"production publication reservation disagrees with task: %w", domain.ErrParentKeyMismatch,
			)
		}
		return publish.Result{}, false, nil
	}
	if entry.Kind != publish.IntentKindPublication || !entry.Dispatched() {
		return publish.Result{}, false, nil
	}
	intent, err := publish.DecodeIntent(entry.Payload)
	if err != nil {
		return publish.Result{}, false, fmt.Errorf(
			"decode durable production publication intent: %w",
			errors.Join(err, domain.ErrParentKeyMismatch),
		)
	}
	if candidate.AuthorizationID == nil ||
		intent.InvocationID != task.PublicationID ||
		intent.Identity != identity.Digest() || intent.Repo != candidate.Repo ||
		intent.BaseRef != candidate.BaseRef || intent.SourceHeadSHA != task.HeadSHA ||
		intent.AuthorizationID != *candidate.AuthorizationID ||
		intent.ProducingInvocationID != task.ProducingInvocationID ||
		intent.ReservationRunID != task.RunID {
		return publish.Result{}, false, fmt.Errorf(
			"production publication intent disagrees with task: %w", domain.ErrParentKeyMismatch,
		)
	}
	if err := w.validatePublicationOutcomeAuthorization(ctx, candidate, intent.AuthorizationID); err != nil {
		return publish.Result{}, false, err
	}
	outcomeRecipes := mapsClone(w.approvedRecipes)
	if candidate.RecipeDigest != nil {
		// A dispatched outcome records effects authorized before current trust
		// changed. Reconstruct its historical identity under its frozen recipe;
		// the immutable authorization and live forge coordinates are still
		// revalidated below. Current approval continues to gate every new effect.
		outcomeRecipes[*candidate.RecipeDigest] = true
	}
	outcome, found, err := publish.LoadOutcome(
		ctx, w.store, candidate, outcomeRecipes, verify,
	)
	if err != nil {
		if productionPublicationRetryableFailure(err) || isDurablePublicationConflict(err) {
			return publish.Result{}, found, err
		}
		return publish.Result{}, found, fmt.Errorf(
			"reconstruct durable production publication outcome: %w",
			errors.Join(err, domain.ErrParentKeyMismatch),
		)
	}
	if !found {
		return publish.Result{}, found, err
	}
	return publish.Result{
		Identity: identity, Branch: outcome.Branch, PRNumber: outcome.PRNumber,
	}, true, nil
}

func isDurablePublicationConflict(err error) bool {
	return errors.Is(err, publish.ErrPublicationConflict) ||
		errors.Is(err, publish.ErrForeignResource)
}

func (w *productionPublicationWorkflow) validatePublicationOutcomeAuthorization(
	ctx context.Context,
	candidate publish.Candidate,
	authorizationID domain.Digest,
) error {
	if candidate.RecipeDigest == nil || candidate.TrustProfileDigest == nil {
		return errors.New("production publication outcome candidate lost its trust bindings")
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(candidate.Artifacts)
	if err != nil {
		return err
	}
	var authorization domain.CandidateAuthorization
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		authorization, err = tx.GetCandidateAuthorization(ctx, authorizationID)
		return err
	}); err != nil {
		return err
	}
	if err := authorization.Validate(); err != nil {
		return err
	}
	if authorization.ID != authorizationID || authorization.Repo != candidate.Repo ||
		authorization.HeadSHA != candidate.HeadSHA ||
		authorization.VerificationRecipeDigest != *candidate.RecipeDigest ||
		authorization.EvidenceSnapshotDigest != evidenceDigest ||
		authorization.TrustProfileDigest != *candidate.TrustProfileDigest ||
		!authorization.AuthorizesPublication {
		return fmt.Errorf("production publication authorization disagrees with outcome: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}

func (w *productionPublicationWorkflow) hasPendingPublicationIntent(
	ctx context.Context,
	invocationID domain.InvocationID,
) (bool, error) {
	key, err := publish.IntentKey(invocationID, publish.IntentKindPublication)
	if err != nil {
		return false, err
	}
	var pending []store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, publish.IntentKindPublication)
		return err
	}); err != nil {
		return false, err
	}
	return slices.ContainsFunc(pending, func(entry store.QueueEntry) bool {
		return entry.IdempotencyKey == key
	}), nil
}

func (w *productionPublicationWorkflow) materializeReplay(
	replay ProductionReplay, dir string,
) error {
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o700); err != nil {
		return err
	}
	manifest, err := replay.Manifest.Encode()
	if err != nil || digestProductionBytes(manifest) != replay.ManifestDigest {
		return fmt.Errorf("replay manifest disagrees with export: %w", errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), manifest, 0o600); err != nil {
		return err
	}
	for _, entry := range replay.Manifest.Entries {
		if entry.Kind != export.EntryRegular {
			continue
		}
		if entry.BlobOmitted || entry.Digest == nil {
			return fmt.Errorf("replay manifest entry %q has no durable blob", entry.Path)
		}
		if err := w.materializeBlob(domain.Digest(*entry.Digest), filepath.Join(dir, "blobs", "sha256")); err != nil {
			return fmt.Errorf("materialize replay blob for %q: %w", entry.Path, err)
		}
	}
	if replay.EvidenceManifestDigest != nil {
		evidence, err := replay.Evidence.Encode()
		if err != nil || digestProductionBytes(evidence) != *replay.EvidenceManifestDigest {
			return fmt.Errorf("replay evidence manifest disagrees with export: %w",
				errors.Join(err, domain.ErrParentKeyMismatch))
		}
		if err := os.WriteFile(filepath.Join(dir, export.EvidenceFilename), evidence, 0o600); err != nil {
			return err
		}
		evidenceDir := filepath.Join(dir, export.EvidenceBlobsDirname, "sha256")
		if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
			return err
		}
		for _, entry := range replay.Evidence.Entries {
			if err := w.materializeBlob(domain.Digest(entry.Digest), evidenceDir); err != nil {
				return fmt.Errorf("materialize replay evidence %q: %w", entry.Label, err)
			}
		}
	} else if len(replay.Evidence.Entries) != 0 {
		return fmt.Errorf("replay carries evidence without a manifest digest: %w", domain.ErrParentKeyMismatch)
	}
	if replay.CommitPlanDigest != nil {
		body, err := loadFakePublicationBlob(w.artifacts, *replay.CommitPlanDigest)
		if err != nil {
			return fmt.Errorf("load durable replay commit plan: %w",
				errors.Join(err, domain.ErrParentKeyMismatch))
		}
		if err := os.WriteFile(filepath.Join(dir, export.CommitPlanFilename), body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (w *productionPublicationWorkflow) materializeBlob(digest domain.Digest, dir string) error {
	hexDigits, ok := contentaddr.Parse(string(digest))
	if !ok {
		return fmt.Errorf("invalid blob digest %q", digest)
	}
	body, err := loadFakePublicationBlob(w.artifacts, digest)
	if err != nil {
		return fmt.Errorf("load durable replay blob %s: %w", digest,
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	return os.WriteFile(filepath.Join(dir, hexDigits), body, 0o600)
}

func digestProductionBytes(body []byte) domain.Digest {
	sum := sha256.Sum256(body)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func (w *productionPublicationWorkflow) verifyAndCheckpoint(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	imported importer.Result,
	checkoutDir string,
) (productionVerificationCheckpoint, error) {
	room, err := w.newRoom(binding.image)
	if err != nil {
		return productionVerificationCheckpoint{}, fmt.Errorf("construct networkless verification room: %w", err)
	}
	readCtx, cancelRead := context.WithTimeout(ctx, w.recipeReadTimeout)
	recipe, err := room.ReadRecipe(readCtx)
	cancelRead()
	if err != nil {
		return productionVerificationCheckpoint{}, fmt.Errorf(
			"load project-image verification recipe: %w",
			productionPublicationRetryableError(err),
		)
	}
	if len(recipe) > verify.DefaultMaxRecipeBytes {
		return productionVerificationCheckpoint{}, fmt.Errorf(
			"project-image verification recipe exceeds the %d-byte cap",
			verify.DefaultMaxRecipeBytes,
		)
	}
	if got := verify.RecipeDigest(recipe); got != binding.image.RecipeDigest {
		return productionVerificationCheckpoint{}, fmt.Errorf(
			"project-image verification recipe digest %s, want %s: %w",
			got, binding.image.RecipeDigest, domain.ErrParentKeyMismatch,
		)
	}
	verified, err := verify.Verify(ctx, checkoutDir, verify.Options{
		HeadSHA: imported.CommitSHA, BaseSHA: binding.admission.Base.BaseSHA,
		InvocationID: task.VerificationID, RecipeSource: verify.ConfigRecipe(recipe),
		RecipePath: verify.DefaultRecipePath, Room: room,
		ApprovedRecipes: w.approvedRecipes, Changes: imported.Changes,
		Policy: verify.Policy{ExtraVerificationControlPatterns: slices.Clone(
			binding.profile.ProtectedPaths.ExtraVerificationControlPatterns,
		)},
	})
	if err != nil {
		if !productionVerificationStateContradiction(err) {
			err = productionPublicationRetryableError(err)
		}
		return productionVerificationCheckpoint{}, fmt.Errorf("clean production verification: %w", err)
	}
	if verified.HeadSHA != imported.CommitSHA || verified.RecipeDigest != binding.image.RecipeDigest {
		return productionVerificationCheckpoint{}, fmt.Errorf("verification disagrees with project-image binding: %w",
			domain.ErrParentKeyMismatch)
	}
	artifacts := make([]domain.Artifact, len(verified.Evidence))
	for index, evidence := range verified.Evidence {
		artifacts[index] = evidence.Artifact
		if _, err := w.artifacts.Put(evidence.Artifact.Digest, bytes.NewReader(evidence.Content)); err != nil {
			return productionVerificationCheckpoint{}, err
		}
		if err := verifyFakePublicationBlob(w.artifacts, evidence.Artifact); err != nil {
			return productionVerificationCheckpoint{}, err
		}
	}
	importDigest, err := digestJSON(imported)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(artifacts)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	outcome := domain.VerificationFailed
	if verified.Outcome == verify.OutcomePassed {
		outcome = domain.VerificationPassed
	}
	authorization, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: binding.admission.Base.Repo, BaseSHA: binding.admission.Base.BaseSHA,
		HeadSHA: imported.CommitSHA, ImportResultDigest: importDigest,
		VerificationRecipeDigest: verified.RecipeDigest,
		EvidenceSnapshotDigest:   evidenceDigest, VerificationOutcome: outcome,
		Findings:           candidateFindings(imported.Findings, verified.Findings),
		TrustProfileDigest: binding.profile.ProfileDigest,
		InvocationID:       task.VerificationID, CreatedAt: binding.export.RecordedAt,
	})
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	checkpoint := productionVerificationCheckpoint{
		Version: productionVerificationVersion,
		TaskKey: productionPublicationTaskKey(task.RunID), ProjectImage: binding.image.ID,
		Imported: imported, Authorization: authorization, Artifacts: artifacts,
	}
	if err := w.persistCheckpoint(ctx, task, checkpoint); err != nil {
		return productionVerificationCheckpoint{}, err
	}
	return checkpoint, nil
}

func productionVerificationStateContradiction(err error) bool {
	return errors.Is(err, verify.ErrRecipeInvalid) ||
		errors.Is(err, verify.ErrRecipeUnreadable) ||
		errors.Is(err, verify.ErrGitPlumbing) ||
		errors.Is(err, verify.ErrUnsupportedRepo) ||
		errors.Is(err, verify.ErrHeadMismatch) ||
		errors.Is(err, verify.ErrBaseMismatch) ||
		errors.Is(err, verify.ErrWorkspaceMismatch) ||
		errors.Is(err, verify.ErrMalformedTree) ||
		errors.Is(err, verify.ErrInvalidOptions) ||
		errors.Is(err, verify.ErrSymlinkEntrypoint)
}

func (w *productionPublicationWorkflow) persistCheckpoint(
	ctx context.Context,
	task productionPublicationTask,
	checkpoint productionVerificationCheckpoint,
) error {
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		for _, artifact := range checkpoint.Artifacts {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		if err := tx.RecordCandidateAuthorization(ctx, checkpoint.Authorization); err != nil {
			return err
		}
		entry, _, err := tx.RecordInbox(
			ctx, productionVerificationCheckpointKey(task.RunID),
			productionVerificationCheckpointKind, payload,
		)
		if err != nil {
			return err
		}
		if entry.Kind != productionVerificationCheckpointKind || !bytes.Equal(entry.Payload, payload) {
			return fmt.Errorf("production verification checkpoint disagrees with stored row: %w",
				domain.ErrImmutableTransition)
		}
		return nil
	})
}

func (w *productionPublicationWorkflow) loadCheckpoint(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
) (productionVerificationCheckpoint, bool, error) {
	var entry store.QueueEntry
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetInbox(ctx, productionVerificationCheckpointKey(task.RunID))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return productionVerificationCheckpoint{}, false, nil
	}
	if err != nil {
		return productionVerificationCheckpoint{}, false, err
	}
	if entry.Kind != productionVerificationCheckpointKind {
		return productionVerificationCheckpoint{}, false, domain.ErrParentKeyMismatch
	}
	decoder := json.NewDecoder(bytes.NewReader(entry.Payload))
	decoder.DisallowUnknownFields()
	var checkpoint productionVerificationCheckpoint
	if err := decoder.Decode(&checkpoint); err != nil {
		return productionVerificationCheckpoint{}, false, fmt.Errorf(
			"decode durable production verification checkpoint: %w",
			errors.Join(err, domain.ErrParentKeyMismatch),
		)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return productionVerificationCheckpoint{}, false, fmt.Errorf(
			"production verification checkpoint has trailing content: %w",
			domain.ErrParentKeyMismatch,
		)
	}
	importDigest, err := digestJSON(checkpoint.Imported)
	if err != nil {
		return productionVerificationCheckpoint{}, false, err
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(checkpoint.Artifacts)
	if err != nil {
		return productionVerificationCheckpoint{}, false, err
	}
	authorization := checkpoint.Authorization
	if checkpoint.Version != productionVerificationVersion ||
		checkpoint.TaskKey != productionPublicationTaskKey(task.RunID) ||
		checkpoint.ProjectImage != binding.image.ID || authorization.Validate() != nil ||
		authorization.Repo != binding.admission.Base.Repo ||
		authorization.BaseSHA != binding.admission.Base.BaseSHA ||
		authorization.HeadSHA != task.HeadSHA ||
		authorization.ImportResultDigest != importDigest ||
		authorization.VerificationRecipeDigest != binding.image.RecipeDigest ||
		authorization.EvidenceSnapshotDigest != evidenceDigest ||
		authorization.TrustProfileDigest != binding.profile.ProfileDigest ||
		authorization.InvocationID != task.VerificationID {
		return productionVerificationCheckpoint{}, false, fmt.Errorf("production verification checkpoint disagrees with task: %w",
			domain.ErrParentKeyMismatch)
	}
	for _, artifact := range checkpoint.Artifacts {
		if err := verifyFakePublicationBlob(w.artifacts, artifact); err != nil {
			return productionVerificationCheckpoint{}, false, fmt.Errorf(
				"verify durable production checkpoint artifact: %w",
				errors.Join(err, domain.ErrParentKeyMismatch),
			)
		}
	}
	var stored domain.CandidateAuthorization
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetCandidateAuthorization(ctx, authorization.ID)
		return err
	}); err != nil {
		return productionVerificationCheckpoint{}, false, err
	}
	if !reflect.DeepEqual(stored, authorization) {
		return productionVerificationCheckpoint{}, false, fmt.Errorf("stored production authorization disagrees with checkpoint: %w",
			domain.ErrParentKeyMismatch)
	}
	return checkpoint, true, nil
}

func productionCandidate(
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
) publish.Candidate {
	recipe := binding.image.RecipeDigest
	authorization := checkpoint.Authorization.ID
	profile := binding.profile.ProfileDigest
	return publish.Candidate{
		Repo: binding.admission.Base.Repo, BaseRef: binding.admission.Base.BaseRef,
		HeadSHA: task.HeadSHA, Title: task.Publication.Title,
		Body:      task.Publication.Body,
		Artifacts: checkpoint.Artifacts, RecipeDigest: &recipe,
		InvocationID: task.PublicationID, RunID: task.RunID,
		AuthorizationID: &authorization, TrustProfileDigest: &profile,
	}
}

func (w *productionPublicationWorkflow) readyItem(
	task productionPublicationTask,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
) (domain.AttentionItem, error) {
	return w.readyItemWithRecipes(task, checkpoint, published, w.approvedRecipes)
}

func (w *productionPublicationWorkflow) readyItemWithRecipes(
	task productionPublicationTask,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
	approvedRecipes map[domain.Digest]bool,
) (domain.AttentionItem, error) {
	runID := task.RunID
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: productionReadyItemID(task.RunID), ProjectID: task.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: fmt.Sprintf("Published %s#%d and completed clean production verification.",
			checkpoint.Authorization.Repo, published.PRNumber),
		RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionMarkSeen, domain.ActionDismiss, domain.ActionStop,
		},
		EvidenceSnapshot: checkpoint.Artifacts, AgentClaims: checkpoint.Imported.Claims,
		PRHeadSHA:        checkpoint.Imported.CommitSHA,
		CommitPlanNotice: checkpoint.Imported.CommitPlanNotice,
		ItemVersion:      1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, approvedRecipes)
}

func (w *productionPublicationWorkflow) blockedItem(
	task productionPublicationTask,
	imported importer.Result,
	artifacts []domain.Artifact,
	reason string,
) (domain.AttentionItem, error) {
	return w.blockedItemWithRecipes(task, imported, artifacts, reason, w.approvedRecipes)
}

func (w *productionPublicationWorkflow) blockedItemWithRecipes(
	task productionPublicationTask,
	imported importer.Result,
	artifacts []domain.Artifact,
	reason string,
	approvedRecipes map[domain.Digest]bool,
) (domain.AttentionItem, error) {
	return w.newBlockedItem(
		task, imported, artifacts, reason,
		[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop},
		approvedRecipes,
	)
}

func (w *productionPublicationWorkflow) blockedHoldItem(
	task productionPublicationTask,
	imported importer.Result,
	reason string,
) (domain.AttentionItem, error) {
	return w.newBlockedItem(
		task, imported, nil, reason,
		[]domain.Action{domain.ActionInspectTrustFailure},
		w.approvedRecipes,
	)
}

func (w *productionPublicationWorkflow) newBlockedItem(
	task productionPublicationTask,
	imported importer.Result,
	artifacts []domain.Artifact,
	reason string,
	actions []domain.Action,
	approvedRecipes map[domain.Digest]bool,
) (domain.AttentionItem, error) {
	runID := task.RunID
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: productionBlockedItemID(task.RunID), ProjectID: task.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID},
		Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
		Reason:            reason,
		RequestedDecision: actions,
		EvidenceSnapshot:  artifacts, AgentClaims: imported.Claims,
		PRHeadSHA:        imported.CommitSHA,
		CommitPlanNotice: imported.CommitPlanNotice,
		ItemVersion:      1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, approvedRecipes)
}

func (w *productionPublicationWorkflow) recoverDefinitiveBlockedTask(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	imported importer.Result,
	checkpoint productionVerificationCheckpoint,
	checkpointFound bool,
	readyExists bool,
) (*productionTaskOutcome, error) {
	var current domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		current, err = tx.GetAttentionItemRecord(ctx, productionBlockedItemID(task.RunID))
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if slices.Equal(current.RequestedDecision, []domain.Action{domain.ActionInspectTrustFailure}) {
		return nil, nil
	}
	if !slices.Equal(current.RequestedDecision,
		[]domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop}) {
		return nil, fmt.Errorf("production blocked item %q has unexpected decisions: %w",
			current.ID, domain.ErrParentKeyMismatch)
	}
	if readyExists {
		return nil, fmt.Errorf("run %q has both ready and definitive blocked items: %w",
			task.RunID, domain.ErrParentKeyMismatch)
	}
	if !slices.Contains([]string{
		productionBlockRecipeRevoked,
		productionBlockVerification,
		productionBlockTrust,
		productionBlockBaseAdvanced,
	}, current.Reason) {
		return nil, fmt.Errorf("production blocked item %q has unexpected reason: %w",
			current.ID, domain.ErrParentKeyMismatch)
	}
	var artifacts []domain.Artifact
	switch current.Reason {
	case productionBlockRecipeRevoked:
		// Recipe approval is checked before verification on both the first
		// pass and recovery, so this definitive result never carries evidence.
	case productionBlockVerification:
		if !checkpointFound || checkpoint.Authorization.AuthorizesPublication {
			return nil, fmt.Errorf("production blocked item %q has an impossible verification reason: %w",
				current.ID, domain.ErrParentKeyMismatch)
		}
		artifacts = checkpoint.Artifacts
	case productionBlockTrust, productionBlockBaseAdvanced:
		if !checkpointFound || !checkpoint.Authorization.AuthorizesPublication {
			return nil, fmt.Errorf("production blocked item %q has an impossible publication reason: %w",
				current.ID, domain.ErrParentKeyMismatch)
		}
		artifacts = checkpoint.Artifacts
	default:
		// The known-reason gate above keeps this switch exhaustive if another
		// definitive reason is added without its own reconstruction contract.
		return nil, fmt.Errorf("production blocked item %q has no reason contract: %w",
			current.ID, domain.ErrParentKeyMismatch)
	}
	historicalRecipes := mapsClone(w.approvedRecipes)
	historicalRecipes[binding.image.RecipeDigest] = true
	expected, err := w.blockedItemWithRecipes(
		task, imported, artifacts, current.Reason, historicalRecipes,
	)
	if err != nil {
		return nil, err
	}
	if !compatibleTerminalItem(expected, current) {
		return nil, fmt.Errorf("production blocked item %q disagrees with task: %w",
			current.ID, domain.ErrParentKeyMismatch)
	}
	accepted, err := w.recordCompletedTerminal(ctx, binding.run, task)
	if err != nil {
		return nil, err
	}
	if err := w.finishTask(ctx, task); err != nil {
		return nil, err
	}
	return &productionTaskOutcome{completed: true, accepted: accepted, blocked: true}, nil
}

func (w *productionPublicationWorkflow) completeBlockedTask(
	ctx context.Context,
	task productionPublicationTask,
	run domain.Run,
	imported importer.Result,
	artifacts []domain.Artifact,
	reason string,
) (productionTaskOutcome, error) {
	item, err := w.blockedItem(task, imported, artifacts, reason)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.putTerminalItem(ctx, item); err != nil {
		return productionTaskOutcome{}, err
	}
	if w.afterBlocked != nil {
		if err := w.afterBlocked(); err != nil {
			return productionTaskOutcome{}, fmt.Errorf("after production blocked item: %w",
				errors.Join(err, errProductionCrashSeam))
		}
	}
	accepted, err := w.recordCompletedTerminal(ctx, run, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.finishTask(ctx, task); err != nil {
		return productionTaskOutcome{}, err
	}
	return productionTaskOutcome{completed: true, accepted: accepted, blocked: true}, nil
}

func (w *productionPublicationWorkflow) putTerminalItem(
	ctx context.Context, item domain.AttentionItem,
) error {
	err := w.attention.PutItem(ctx, item)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrStaleWrite) && !errors.Is(err, store.ErrImmutableConflict) {
		return err
	}
	var current domain.AttentionItem
	if readErr := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		current, err = tx.GetAttentionItemRecord(ctx, item.ID)
		return err
	}); readErr != nil {
		return errors.Join(err, readErr)
	}
	if current.Status == domain.StatusOpen && current.DecidedAt == nil &&
		slices.Equal(current.RequestedDecision, []domain.Action{domain.ActionInspectTrustFailure}) &&
		current.ProjectID == item.ProjectID && current.Type == item.Type &&
		reflect.DeepEqual(current.Subject, item.Subject) && current.PRHeadSHA == item.PRHeadSHA {
		// A repairable publication hold and the later definitive block share
		// one deterministic item identity. Advance the open hold instead of
		// attempting to overwrite its v1 body, preserving delivery-derived and
		// conversation metadata while the new body becomes the durable result.
		item.ItemVersion = current.ItemVersion + 1
		item.Timing = current.Timing
		item.ConversationID = current.ConversationID
		item.ExpiresWhen = current.ExpiresWhen
		return w.attention.PutItem(ctx, item)
	}
	if !compatibleTerminalItem(item, current) {
		return err
	}
	return nil
}

func (w *productionPublicationWorkflow) recordCompletedTerminal(
	ctx context.Context,
	run domain.Run,
	task productionPublicationTask,
) (bool, error) {
	return (&Engine{store: w.store}).recordProductionTerminalWithAuthority(ctx, run, productionTerminalRecord{
		InvocationID: task.ProducingInvocationID, RunID: task.RunID,
		StageID: productionStageID(task.RunID), Status: exec.StatusCompleted,
		HeadSHA: task.HeadSHA, Artifacts: slices.Clone(task.Artifacts), Summary: task.Summary,
	}, false)
}

func (w *productionPublicationWorkflow) finishTask(
	ctx context.Context, task productionPublicationTask,
) error {
	return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, productionPublicationTaskKey(task.RunID))
	})
}
