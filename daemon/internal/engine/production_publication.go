package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	// KindProductionPublicationRequested is the durable queue kind that owns
	// exact replay through verification and execution-bound publication.
	KindProductionPublicationRequested   = "production_publication_requested"
	productionPublicationTaskVersion     = "freeside.production-publication/v1"
	productionVerificationVersionV1      = "freeside.production-verification/v1"
	productionVerificationVersion        = "freeside.production-verification/v2"
	productionVerificationCheckpointKind = "production_verification_checkpoint"
	productionPublicationSupersedingKind = "production_publication_superseding"
	defaultProductionRecipeReadTimeout   = 2 * time.Minute
	defaultProductionHoldRetryInterval   = 30 * time.Second
	productionBlockRecipeRevoked         = domain.PublicationBlockRecipeRevoked
	productionBlockRepairRecipeRevoked   = "Publication is durably held because current trust no longer approves the verification recipe required to repair the pull request. Restore that approval before repairing external state."
	productionBlockRepairTrustRefused    = "Publication is durably held because the current trust profile or candidate authorization no longer permits repairing the pull request. Restore that authority before repairing external state."
	productionBlockVerification          = domain.PublicationBlockVerification
	productionBlockTrust                 = domain.PublicationBlockTrust
	productionBlockBaseAdvanced          = domain.PublicationBlockBaseAdvanced
	productionBlockExternal              = "Publication is durably held because the external service permanently refused the committed operation. Repair that external state to resume recovery."
)

var (
	errProductionCrashSeam       = errors.New("injected production crash seam")
	errProductionRetryable       = errors.New("production publication retryable failure")
	errRemediationSourceIdentity = errors.New("remediation source tree identity is unavailable")
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
	ReviewSource    exec.ReviewSource
	// RemediationPromptPackageDigest selects the trusted prompt package for
	// implementation-role invocations created by finding adjudication.
	RemediationPromptPackageDigest domain.Digest
	// ShadowReviewSource is optional. When present, every other shadow field
	// is required and the source remains observation-only: it cannot satisfy
	// routed review, advance rounds, or authorize publication.
	ShadowReviewSource              exec.ReviewSource
	ShadowReviewConfigurationDigest domain.Digest
	ShadowReviewCostOwner           string
	ShadowReviewDefaultRate         float64
	ShadowReviewFailure             func(domain.RunID, int, domain.ReviewFailureClass, error)

	// ReviewRecovery is the cleanup-only startup seam shared by both modes. It
	// must not carry authority to start a new review.
	ReviewRecovery            func(context.Context) error
	ReviewConfigurationDigest domain.Digest
	ReviewHostInstructions    ReviewHostInstructions
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
	TransitionHook       DurableTransitionHook
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
	Version               string              `json:"version"`
	RunID                 domain.RunID        `json:"run_id"`
	ProjectID             domain.ProjectID    `json:"project_id"`
	ProducingInvocationID domain.InvocationID `json:"producing_invocation_id"`
	// LegacyRemediationNoop preserves v1 task decoding and byte-identical
	// recorded replay. It is never consulted for change classification.
	LegacyRemediationNoop bool                  `json:"remediation_noop,omitempty"`
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
	HeadSHA       string                        `json:"head_sha"`
	ProjectImage  domain.Digest                 `json:"project_image"`
	Imported      importer.Result               `json:"imported"`
	Authorization domain.CandidateAuthorization `json:"authorization"`
	Artifacts     []domain.Artifact             `json:"artifacts"`
}

type productionPublicationWorkflow struct {
	store                           *store.Store
	attention                       attentionService
	workDir                         string
	transport                       PublicationTransport
	publisher                       *publish.Publisher
	artifacts                       ArtifactStore
	approvedRecipes                 map[domain.Digest]bool
	newRoom                         func(domain.ProjectImage) (ProductionVerificationRoom, error)
	reviewSource                    exec.ReviewSource
	remediationPromptPackage        domain.Digest
	shadowReviewSource              exec.ReviewSource
	findingAdjudicator              findingAdjudicator
	signet                          *signet.Service
	inference                       *inference.Client
	reviewRecovery                  func(context.Context) error
	reviewRecoveryPending           bool
	reviewConfigurationDigest       domain.Digest
	shadowReviewConfigurationDigest domain.Digest
	shadowReviewCostOwner           string
	shadowReviewDefaultRate         float64
	shadowReviewFailure             func(domain.RunID, int, domain.ReviewFailureClass, error)
	reviewHostInstructions          ReviewHostInstructions
	holdOnly                        bool
	recipeReadTimeout               time.Duration
	holdRetryInterval               time.Duration
	now                             func() time.Time
	holdRetryAfter                  map[domain.RunID]time.Time
	reviewRetryAfter                map[domain.RunID]time.Time
	// holdPace bounds this workflow's per-pass hold projection writes: the
	// hold-only composition's observations, and the active composition's
	// clear when it accepts a queued task (issue #394). Process state only,
	// never authority.
	holdPace             observationPace
	afterVerification    func() error
	afterPublication     func() error
	afterReady           func() error
	afterBlocked         func() error
	afterTerminal        func() error
	afterTaskLockRelease func() error
	transitionHook       DurableTransitionHook
	reconcileMu          sync.Mutex
}

func (w *productionPublicationWorkflow) attentionCreatedAt() time.Time {
	if w.now == nil {
		return time.Now().UTC()
	}
	return w.now().UTC()
}

type productionPublicationResult struct {
	completed     int
	accepted      int
	readyClean    int
	readyDegraded int
	blocked       int
	lastPR        int
}

func newProductionPublicationWorkflow(
	st *store.Store,
	attention attentionService,
	cfg ProductionPublicationConfig,
) (*productionPublicationWorkflow, error) {
	shadowEnabled := cfg.ShadowReviewSource != nil
	if st == nil || attention == nil || cfg.Transport == nil || cfg.Publisher == nil ||
		cfg.Artifacts == nil || cfg.NewRoom == nil ||
		cfg.ReviewRecovery == nil ||
		(!cfg.HoldOnly && (cfg.ReviewSource == nil ||
			!contentaddr.Valid(string(cfg.ReviewConfigurationDigest)) ||
			cfg.ReviewHostInstructions.validate() != nil)) {
		return nil, errors.New("nil dependency")
	}
	if shadowEnabled {
		if !contentaddr.Valid(string(cfg.ShadowReviewConfigurationDigest)) ||
			strings.TrimSpace(cfg.ShadowReviewCostOwner) == "" ||
			math.IsNaN(cfg.ShadowReviewDefaultRate) || math.IsInf(cfg.ShadowReviewDefaultRate, 0) ||
			cfg.ShadowReviewDefaultRate < 0 || cfg.ShadowReviewDefaultRate > 1 ||
			cfg.ShadowReviewFailure == nil {
			return nil, errors.New("invalid shadow review configuration")
		}
	} else if cfg.ShadowReviewConfigurationDigest != "" || cfg.ShadowReviewCostOwner != "" ||
		cfg.ShadowReviewDefaultRate != 0 || cfg.ShadowReviewFailure != nil {
		return nil, errors.New("partial shadow review configuration")
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
		newRoom:         cfg.NewRoom, reviewSource: cfg.ReviewSource,
		remediationPromptPackage: cfg.RemediationPromptPackageDigest,
		shadowReviewSource:       cfg.ShadowReviewSource,
		reviewRecovery:           cfg.ReviewRecovery, reviewRecoveryPending: true,
		reviewConfigurationDigest:       cfg.ReviewConfigurationDigest,
		shadowReviewConfigurationDigest: cfg.ShadowReviewConfigurationDigest,
		shadowReviewCostOwner:           cfg.ShadowReviewCostOwner,
		shadowReviewDefaultRate:         cfg.ShadowReviewDefaultRate,
		shadowReviewFailure:             cfg.ShadowReviewFailure,
		reviewHostInstructions: ReviewHostInstructions{
			Present: cfg.ReviewHostInstructions.Present,
			Digest:  cfg.ReviewHostInstructions.Digest,
			Body:    bytes.Clone(cfg.ReviewHostInstructions.Body),
		},
		holdOnly:          cfg.HoldOnly,
		recipeReadTimeout: cfg.RecipeReadTimeout,
		holdRetryInterval: cfg.HoldRetryInterval, now: cfg.Now,
		holdRetryAfter:    make(map[domain.RunID]time.Time),
		reviewRetryAfter:  make(map[domain.RunID]time.Time),
		afterVerification: cfg.AfterVerification,
		afterPublication:  cfg.AfterPublication, afterReady: cfg.AfterReady,
		afterBlocked: cfg.AfterBlocked, afterTerminal: cfg.AfterTerminal,
		afterTaskLockRelease: cfg.AfterTaskLockRelease,
		transitionHook:       cfg.TransitionHook,
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

func productionVerificationCheckpointKey(runID domain.RunID, headSHA string) string {
	return "production-verification/" + string(runID) + "/" + headSHA
}

func productionReadyItemID(runID domain.RunID) domain.ItemID {
	return domain.ProductionReadyItemID(runID)
}

func productionBlockedItemID(runID domain.RunID) domain.ItemID {
	return domain.ProductionBlockedItemID(runID)
}

func productionReviewItemID(runID domain.RunID, round int) domain.ItemID {
	return domain.ItemID(fmt.Sprintf("production-review-%s-%d", runID, round))
}

func productionReviewHardLimitItemID(
	runID domain.RunID, hardLimit int, recoveredContradiction bool,
) domain.ItemID {
	if recoveredContradiction {
		// This namespace cannot collide with productionReviewItemID for any
		// legal RunID: their fixed prefixes diverge before the run coordinate.
		return domain.ItemID(fmt.Sprintf(
			"production-recovered-review-exhaustion-%s-%d", runID, hardLimit))
	}
	// Preserve the pre-recovery identity for every existing exhaustion path,
	// so an upgrade after its item write converges on the same durable row.
	return productionReviewItemID(runID, hardLimit)
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
	if task.RunID != run.ID || task.ProjectID != run.ProjectID {
		return false, fmt.Errorf("queued production completion disagrees with run: %w",
			domain.ErrParentKeyMismatch)
	}
	if task.ProducingInvocationID != invocationID {
		currentRound, currentIsRemediation := remediationRoundForInvocation(
			run.ID, task.ProducingInvocationID)
		priorRound, priorIsRemediation := remediationRoundForInvocation(run.ID, invocationID)
		priorIsInitial := invocationID == productionInvocationID(run.ID)
		if currentIsRemediation && (priorIsInitial || priorIsRemediation && priorRound < currentRound) {
			var (
				admission domain.ExecutionAdmission
				exported  domain.ExecutionExport
			)
			if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				admission, err = tx.GetExecutionAdmissionRecord(ctx, invocationID)
				if err != nil {
					return err
				}
				exported, err = tx.GetExecutionExportRecord(ctx, invocationID)
				return err
			}); err != nil {
				return false, err
			}
			stage, ok := productionStageForInvocation(run, invocationID)
			if !ok || admission.RunID != run.ID || admission.StageID != stage.ID ||
				exported.InvocationID != invocationID || exported.ObservedBaseSHA != task.Replay.ObservedBaseSHA {
				return false, domain.ErrParentKeyMismatch
			}
			return true, nil
		}
		round, remediation := remediationRoundForInvocation(run.ID, invocationID)
		if !remediation {
			return false, fmt.Errorf("queued production completion disagrees with run: %w",
				domain.ErrParentKeyMismatch)
		}
		var requestEntry store.QueueEntry
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			requestEntry, err = tx.GetOutbox(ctx, string(invocationID))
			return err
		}); err != nil {
			return false, err
		}
		request, err := decodeRemediationRequest(requestEntry)
		if err != nil || !requestEntry.Dispatched() || request.Round != round ||
			task.HeadSHA != request.HeadSHA {
			return false, errors.Join(err, domain.ErrParentKeyMismatch)
		}
		if priorRound, prior := remediationRoundForInvocation(run.ID, task.ProducingInvocationID); task.ProducingInvocationID != productionInvocationID(run.ID) && (!prior || priorRound >= round) {
			return false, domain.ErrImmutableTransition
		}
		return false, nil
	}
	if entry.Dispatched() {
		// The caller read the inbox in an earlier transaction, and the lane
		// commits the terminal and dispatches the task in two of its own. Since
		// the lane runs on its own loop (issue #425), this scan can read the
		// inbox before the terminal commits and the outbox after the dispatch,
		// which is a publication completing beside it, not a contradiction. The
		// terminal write happens-before the dispatch, so re-reading the inbox
		// here settles it: still absent proves the real violation.
		recorded, err := w.hasRecordedTerminal(ctx, invocationID)
		if err != nil {
			return false, err
		}
		if !recorded {
			return false, fmt.Errorf("production publication task dispatched without a terminal record: %w",
				domain.ErrImmutableTransition)
		}
	}
	// The acceptance scan runs in every operating mode, while the pending
	// scan that also releases this notice returns early under a hold-only
	// publication lane. Retiring it here too is what keeps a repaired task
	// from leaving an open notice behind in attended_dev.
	return true, releaseProductionQuarantine(
		ctx, w.store, w.attention, productionTaskQuarantinePrefix, run.ID)
}

// hasRecordedTerminal reports whether this invocation's terminal row exists.
// Presence is the whole question: whether the row authenticates against the
// durable task is authenticatesTerminal's, on the pass that reads it as the
// recorded terminal.
func (w *productionPublicationWorkflow) hasRecordedTerminal(
	ctx context.Context, invocationID domain.InvocationID,
) (bool, error) {
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetInbox(ctx, string(invocationID))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
	if err := validateProductionPublicationCompletion(run, task, terminal); err != nil {
		return false, err
	}
	stage, stageFound := productionStageForInvocation(run, task.ProducingInvocationID)
	if admission.RunID != task.RunID || !stageFound || admission.StageID != stage.ID ||
		executionExport.AdmissionID != admission.ID ||
		executionExport.InvocationID != task.ProducingInvocationID ||
		executionExport.HeadSHA != task.HeadSHA ||
		task.Replay.ObservedBaseSHA != admission.Base.BaseSHA ||
		task.Replay.ManifestDigest != executionExport.ManifestDigest ||
		!sameOptionalDigest(task.Replay.EvidenceManifestDigest, executionExport.EvidenceManifestDigest) ||
		(task.Replay.CommitPlanDigest != nil) != executionExport.CommitPlanPresent ||
		!task.Replay.ImportOptions.CommitDate.Equal(executionExport.RecordedAt) {
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
		return st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.RecordExecutionExport(ctx, executionExport)
		})
	case domain.ModeUnattended:
		// An unattended elaboration run produces an export the same way, but its
		// output is a typed specification published outside the repo channel, and
		// it mints no publication task: it is a separate run whose ownership
		// marker is inv-elaborate-<run>-<iter>, not the inv-implement-<run> marker
		// loadProductionRequest authenticates below, so that call would fail
		// before any legacy fallback. Record it export-only, mirroring the
		// attended branch. Without this arm a debris-tolerant elaboration import
		// succeeds and then retries the export-record step forever (issue #768).
		if IsElaborationInvocationIdentity(
			executionExport.InvocationID, admission.RunID, admission.StageID,
		) {
			return st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.RecordExecutionExport(ctx, executionExport)
			})
		}
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
			return st.Write(ctx, func(tx *store.WriteTx) error {
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
		admission       domain.ExecutionAdmission
		run             domain.Run
		publication     ProductionPublication
		remediation     *remediationInvocationRequest
		previousTask    *productionPublicationTask
		alreadyReplaced bool
		legacyNoop      bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, executionExport.InvocationID)
		if err != nil {
			return err
		}
		run, err = tx.GetRun(ctx, admission.RunID)
		if err != nil {
			return err
		}
		if round, ok := remediationRoundForInvocation(run.ID, executionExport.InvocationID); ok {
			entry, err := tx.GetOutbox(ctx, string(executionExport.InvocationID))
			if err != nil {
				return err
			}
			verified, err := authenticateRemediationInvocationTransition(
				ctx, tx, entry, run.ID, admission.StageID,
			)
			if err != nil || verified.request.Round != round {
				return errors.Join(err, domain.ErrParentKeyMismatch)
			}
			request := verified.request
			latestReview, err := tx.LatestReviewRecord(ctx, run.ID)
			if err != nil || latestReview.InvocationID != request.ReviewInvocationID ||
				latestReview.Round != request.Round || latestReview.Outcome != domain.ReviewFindings {
				return errors.Join(err, domain.ErrParentKeyMismatch)
			}
			remediation = &request
			taskEntry, err := tx.GetOutbox(ctx, productionPublicationTaskKey(run.ID))
			if err != nil {
				return err
			}
			current, err := decodeProductionPublicationTask(taskEntry)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current.Publication, verified.publication) {
				return domain.ErrParentKeyMismatch
			}
			publication = verified.publication
			switch current.ProducingInvocationID {
			case executionExport.InvocationID:
				alreadyReplaced = true
				legacyNoop = current.LegacyRemediationNoop
			case productionInvocationID(run.ID):
				previous := current
				previousTask = &previous
			default:
				if priorRound, ok := remediationRoundForInvocation(run.ID, current.ProducingInvocationID); !ok || priorRound >= round {
					return domain.ErrImmutableTransition
				}
				previous := current
				previousTask = &previous
			}
			return nil
		}
		entry, err := tx.GetOutbox(ctx, string(productionInvocationID(run.ID)))
		if err != nil {
			return err
		}
		request, err := authenticateProductionMarker(entry, run.ID)
		if err != nil {
			return err
		}
		if request.Legacy {
			return fmt.Errorf("legacy production invocation %q has no publication authority: %w",
				executionExport.InvocationID, domain.ErrParentKeyMismatch)
		}
		publication = request.Publication
		return nil
	}); err != nil {
		return err
	}
	if remediation != nil {
		if admission.StageID != remediation.StageID ||
			remediation.BaseSHA != replay.ObservedBaseSHA {
			return fmt.Errorf("remediation export disagrees with its request: %w", domain.ErrParentKeyMismatch)
		}
		if previousTask != nil && (previousTask.HeadSHA != remediation.HeadSHA ||
			previousTask.Replay.ObservedBaseSHA != remediation.BaseSHA) {
			return fmt.Errorf("remediation export does not supersede the current candidate: %w",
				domain.ErrParentKeyMismatch)
		}
	} else if admission.StageID != productionStageID(run.ID) ||
		executionExport.InvocationID != productionInvocationID(run.ID) {
		return fmt.Errorf("production export disagrees with its initial stage: %w", domain.ErrParentKeyMismatch)
	}
	artifacts := make([]domain.Digest, 0, len(replay.Evidence.Entries))
	for _, entry := range replay.Evidence.Entries {
		artifacts = append(artifacts, domain.Digest(entry.Digest))
	}
	task := productionPublicationTask{
		Version: productionPublicationTaskVersion, RunID: run.ID, ProjectID: run.ProjectID,
		ProducingInvocationID: executionExport.InvocationID,
		LegacyRemediationNoop: legacyNoop,
		VerificationID: productionVerificationInvocationIDForProducer(
			run.ID, executionExport.InvocationID),
		PublicationID: productionPublicationInvocationID(run.ID),
		HeadSHA:       executionExport.HeadSHA, Artifacts: artifacts,
		Replay: replay, Publication: publication,
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
	w := &productionPublicationWorkflow{store: st}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		currentAdmission, err := tx.GetExecutionAdmissionRecord(ctx, task.ProducingInvocationID)
		if err != nil {
			return err
		}
		currentRun, err := tx.GetRun(ctx, task.RunID)
		if err != nil {
			return err
		}
		expectedStage, stageFound := productionStageForInvocation(currentRun, task.ProducingInvocationID)
		if currentAdmission.ID != admission.ID || currentAdmission.RunID != task.RunID ||
			!stageFound || currentAdmission.StageID != expectedStage.ID {
			return fmt.Errorf("production publication admission disagrees with task: %w",
				domain.ErrParentKeyMismatch)
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
		key := productionPublicationTaskKey(task.RunID)
		if remediation == nil {
			entry, _, err := tx.EnqueueOutbox(
				ctx, key, KindProductionPublicationRequested, payload)
			if err != nil {
				return err
			}
			if entry.Kind != KindProductionPublicationRequested || !bytes.Equal(entry.Payload, payload) {
				return fmt.Errorf("production publication task disagrees with stored row: %w",
					domain.ErrImmutableTransition)
			}
			return nil
		}
		requestEntry, err := tx.GetOutbox(ctx, string(remediation.InvocationID))
		if err != nil {
			return err
		}
		currentRequest, err := decodeRemediationRequest(requestEntry)
		if err != nil || !reflect.DeepEqual(currentRequest, *remediation) || !requestEntry.Dispatched() {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		latestReview, err := tx.LatestReviewRecord(ctx, task.RunID)
		if err != nil || latestReview.InvocationID != remediation.ReviewInvocationID ||
			latestReview.Round != remediation.Round || latestReview.Outcome != domain.ReviewFindings {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		currentEntry, err := tx.GetOutbox(ctx, key)
		if err != nil {
			return err
		}
		if alreadyReplaced {
			if currentEntry.Kind != KindProductionPublicationRequested ||
				!bytes.Equal(currentEntry.Payload, payload) {
				return domain.ErrImmutableTransition
			}
			return nil
		}
		if previousTask == nil || currentEntry.Dispatched() {
			return domain.ErrImmutableTransition
		}
		previousPayload, err := json.Marshal(previousTask)
		if err != nil || !bytes.Equal(currentEntry.Payload, previousPayload) {
			return errors.Join(err, domain.ErrImmutableTransition)
		}
		if _, promoted, err := tx.PromoteOutbox(
			ctx, key, KindProductionPublicationRequested, productionPublicationSupersedingKind,
			previousPayload, previousPayload,
		); err != nil || !promoted {
			return errors.Join(err, domain.ErrImmutableTransition)
		}
		entry, promoted, err := tx.PromoteOutbox(
			ctx, key, productionPublicationSupersedingKind, KindProductionPublicationRequested,
			previousPayload, payload,
		)
		if err != nil || !promoted || entry.Kind != KindProductionPublicationRequested ||
			!bytes.Equal(entry.Payload, payload) {
			return errors.Join(err, domain.ErrImmutableTransition)
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

// quarantineTaskMarkers reports whether this task's run is held out of the
// lane by either ownership marker in its production transition. The active
// remediation transition comes from the run's durable stages, not from the
// producer currently carried by the publication task: before remediation
// export that task deliberately still names the prior producer. A task whose
// complete marker chain reads again retires each marker's notice.
func (w *productionPublicationWorkflow) quarantineTaskMarkers(
	ctx context.Context, task productionPublicationTask,
) (bool, error) {
	var (
		run        domain.Run
		transition authenticatedProductionRunTransition
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		transition, err = authenticateProductionRunTransition(ctx, tx, task.RunID)
		run = transition.run
		if err != nil || transition.remediation == nil {
			return err
		}
		verified := transition.remediation
		if verified.request.BaseSHA != task.Replay.ObservedBaseSHA ||
			!reflect.DeepEqual(verified.publication, task.Publication) {
			return classifyRemediationMarkerError(domain.ErrParentKeyMismatch)
		}
		if task.ProducingInvocationID == verified.request.InvocationID {
			return nil
		}
		producerRound, priorRemediation := remediationRoundForInvocation(
			task.RunID, task.ProducingInvocationID)
		priorProducer := task.ProducingInvocationID == productionInvocationID(task.RunID) ||
			priorRemediation && producerRound < verified.request.Round
		if !priorProducer || task.HeadSHA != verified.request.HeadSHA {
			return classifyRemediationMarkerError(domain.ErrParentKeyMismatch)
		}
		return nil
	})
	if err == nil {
		err = authenticateProductionRunInput(w.artifacts, transition)
	}
	if _, markerFailure := productionQuarantineReason(err); markerFailure {
		quarantined, quarantineErr := quarantineProductionMarker(
			ctx, w.store, w.attention, run.ID, run.ProjectID, err)
		if quarantineErr != nil {
			return false, errors.Join(err, quarantineErr)
		}
		if quarantined {
			return true, nil
		}
		return false, err
	}
	if errors.Is(err, errRemediationMarkerUnreadable) {
		if quarantineErr := recordProductionQuarantine(
			ctx, w.store, w.attention, remediationMarkerQuarantinePrefix,
			run.ID, run.ProjectID, remediationQuarantineUnreadable,
		); quarantineErr != nil {
			return false, errors.Join(err, quarantineErr)
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if err := releaseProductionQuarantine(
		ctx, w.store, w.attention, productionMarkerQuarantinePrefix, task.RunID); err != nil {
		return false, err
	}
	if err := releaseProductionQuarantine(
		ctx, w.store, w.attention, remediationMarkerQuarantinePrefix, task.RunID); err != nil {
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
	_, remediationProducer := remediationRoundForInvocation(t.RunID, t.ProducingInvocationID)
	validProducer := t.ProducingInvocationID == productionInvocationID(t.RunID) || remediationProducer
	if t.Version != productionPublicationTaskVersion || t.RunID == "" || t.ProjectID == "" ||
		!validProducer ||
		(t.LegacyRemediationNoop && !remediationProducer) ||
		t.VerificationID != productionVerificationInvocationIDForProducer(t.RunID, t.ProducingInvocationID) ||
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
	var task productionPublicationTask
	if err := strictjson.Decode(entry.Payload, &task, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return productionPublicationTask{}, errors.New("production publication task has trailing content")
		}
		return productionPublicationTask{}, err
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

// ProductionPublicationCompletion authenticates the selected run's final
// publication-task and terminal records and returns their exact producing
// invocation. It is the read-only completion boundary used by supervision:
// absence or a pending task is ordinary incompletion, while a dispatched task
// with absent or divergent terminal authority fails closed.
func ProductionPublicationCompletion(
	ctx context.Context, tx *store.ReadTx, run domain.Run,
) (domain.InvocationID, bool, error) {
	entry, err := tx.GetOutbox(ctx, productionPublicationTaskKey(run.ID))
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	task, err := decodeProductionPublicationTask(entry)
	if err != nil {
		return "", false, err
	}
	if task.RunID != run.ID || task.ProjectID != run.ProjectID {
		return "", false, fmt.Errorf(
			"production publication task disagrees with selected run: %w",
			domain.ErrParentKeyMismatch,
		)
	}
	if !entry.Dispatched() {
		return task.ProducingInvocationID, false, nil
	}
	terminalEntry, err := tx.GetInbox(ctx, string(task.ProducingInvocationID))
	if errors.Is(err, store.ErrNotFound) {
		return "", false, fmt.Errorf(
			"dispatched production publication task has no terminal record: %w",
			domain.ErrImmutableTransition,
		)
	}
	if err != nil {
		return "", false, err
	}
	terminal, err := decodeProductionTerminal(terminalEntry, run)
	if err != nil {
		return "", false, err
	}
	if err := validateProductionPublicationCompletion(run, task, terminal); err != nil {
		return "", false, err
	}
	return task.ProducingInvocationID, true, nil
}

func validateProductionPublicationCompletion(
	run domain.Run, task productionPublicationTask, terminal productionTerminalRecord,
) error {
	stage, stageFound := productionStageForInvocation(run, task.ProducingInvocationID)
	if run.ID != task.RunID || run.ProjectID != task.ProjectID ||
		!stageFound ||
		terminal.Status != exec.StatusCompleted ||
		terminal.InvocationID != task.ProducingInvocationID || terminal.RunID != task.RunID ||
		terminal.StageID != stage.ID || terminal.HeadSHA != task.HeadSHA ||
		!slices.Equal(terminal.Artifacts, task.Artifacts) || terminal.Summary != task.Summary {
		return fmt.Errorf("production terminal disagrees with durable publication task: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}

func (w *productionPublicationWorkflow) reconcile(ctx context.Context) (productionPublicationResult, error) {
	w.reconcileMu.Lock()
	defer w.reconcileMu.Unlock()
	if w.reviewRecoveryPending {
		if err := w.reviewRecovery(ctx); err != nil {
			return productionPublicationResult{}, fmt.Errorf("recover Codex reviews at startup: %w", err)
		}
		w.reviewRecoveryPending = false
	}
	if w.holdOnly {
		// The hold-only composition deliberately pauses queued publication
		// tasks (an attended daemon recognizing unattended work), which is a
		// hold an operator should see per run, not a silent return: record
		// the typed cause for each queued task before pausing (issue #394).
		if err := w.recordAttendedPublicationHolds(ctx); err != nil {
			return productionPublicationResult{}, err
		}
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
		markerQuarantined, err := w.quarantineTaskMarkers(ctx, task)
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
			// A work directory the daemon cannot prepare pauses the task just
			// as a retryable reconcile failure does, so it is the same
			// operator-visible hold: without this the run is paused with no
			// cause (or a stale one) on the read surface (issue #394).
			if obsErr := w.recordPublicationEnvironmentHold(ctx, task); obsErr != nil {
				joined = errors.Join(joined, fmt.Errorf("task %q: %w", entry.IdempotencyKey, obsErr))
			}
			w.deferHeldTask(task)
			continue
		}
		release, err := acquireFakePublicationLock(ctx, lockDir, entry.IdempotencyKey)
		if err != nil {
			if obsErr := w.recordPublicationEnvironmentHold(ctx, task); obsErr != nil {
				joined = errors.Join(joined, fmt.Errorf("task %q: %w", entry.IdempotencyKey, obsErr))
			}
			w.deferHeldTask(task)
			continue
		}
		// The attempt starts here, so an attended daemon's pause of this
		// queued task has ended: clearing it now keeps the read surface from
		// reporting attended_mode_active through a long fetch, verification,
		// and publication attempt (issue #394). Only that cause is cleared —
		// a hold this attempt is about to re-record (a definitive block, an
		// environmental back-off) keeps its row and its span. A failed
		// projection write is joined loud but never stops the attempt: the
		// observation surface has no authority over publication.
		if obsErr := w.clearAttendedPublicationHold(ctx, task); obsErr != nil {
			joined = errors.Join(joined, fmt.Errorf("task %q: %w", entry.IdempotencyKey, obsErr))
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
				// The environmental back-off is a hold an operator can see:
				// record its typed cause beside the retry window (issue
				// #394). A failed observation write is a store fault, joined
				// loud like any other.
				if obsErr := w.recordPublicationEnvironmentHold(ctx, task); obsErr != nil {
					joined = errors.Join(joined, fmt.Errorf("task %q: %w", entry.IdempotencyKey, obsErr))
				}
				w.deferHeldTask(task)
			} else {
				joined = errors.Join(joined, fmt.Errorf("task %q: %w", entry.IdempotencyKey, reconcileErr))
			}
			continue
		}
		result.completed += boolCount(outcome.completed)
		result.accepted += boolCount(outcome.accepted)
		if outcome.readiness != nil {
			switch outcome.readiness.Class {
			case domain.ReadinessBlocked:
				// Blocked readiness is represented by the blocked task path.
			case domain.ReadinessReadyClean:
				result.readyClean++
			case domain.ReadinessReadyDegraded:
				result.readyDegraded++
			}
		}
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
	var sqliteError *sqlite.Error
	return errors.Is(err, errProductionRetryable) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, sql.ErrTxDone) ||
		errors.Is(err, publish.ErrGitHubAPI) ||
		errors.Is(err, publish.ErrJanitorInactive) ||
		errors.Is(err, publish.ErrInstallationGrantUntrusted) ||
		errors.As(err, &gitError) ||
		errors.As(err, &networkError) ||
		errors.As(err, &pathError) ||
		errors.As(err, &sqliteError) ||
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
	readiness *domain.ReadinessVerdict
	blocked   bool
	prNumber  int
}

type productionBinding struct {
	run            domain.Run
	declaration    *domain.WorkUnitDeclaration
	spec           []byte
	specLoaded     bool
	admission      domain.ExecutionAdmission
	export         domain.ExecutionExport
	resolvedPolicy domain.ResolvedPolicy
	replay         ProductionReplay
	profile        domain.AutomationTrustProfile
	image          domain.ProjectImage
	remediation    *authenticatedRemediationTransition
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
		if _, remediated := remediationRoundForInvocation(
			task.RunID, task.ProducingInvocationID,
		); remediated {
			entry, err := tx.GetOutbox(ctx, string(task.ProducingInvocationID))
			if err != nil {
				return err
			}
			verified, err := authenticateRemediationInvocationTransition(
				ctx, tx, entry, task.RunID, binding.admission.StageID,
			)
			if err != nil || !entry.Dispatched() ||
				verified.request.BaseSHA != task.Replay.ObservedBaseSHA ||
				!reflect.DeepEqual(verified.publication, task.Publication) {
				return errors.Join(err, domain.ErrParentKeyMismatch)
			}
			binding.remediation = &verified
		}
		binding.resolvedPolicy, err = tx.GetResolvedPolicy(ctx, task.RunID)
		if err != nil {
			return err
		}
		declaration, declarationErr := tx.GetWorkUnitDeclarationByRun(ctx, task.RunID)
		switch {
		case declarationErr == nil:
			binding.declaration = &declaration
		case errors.Is(declarationErr, store.ErrNotFound):
		default:
			return declarationErr
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
	binding.spec, err = loadFakePublicationBlob(w.artifacts, binding.run.SpecDigest)
	if err != nil {
		return productionBinding{}, fmt.Errorf("load approved specification: %w", err)
	}
	binding.specLoaded = true
	binding.replay = task.Replay
	stage, stageFound := productionStageForInvocation(binding.run, task.ProducingInvocationID)
	if binding.run.ID != task.RunID || binding.run.ProjectID != task.ProjectID ||
		binding.run.SpecDigest != binding.admission.SpecDigest ||
		binding.admission.RunID != task.RunID ||
		!stageFound || binding.admission.StageID != stage.ID ||
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
	if binding.replay.ImportOptions.CommitMessage != "" {
		if !binding.specLoaded {
			// The atomic export/task persistence boundary has no blob-store
			// dependency. Preserve the recorded value there; loadBinding opens
			// and verifies the approved spec before the publication re-gate
			// authenticates the message itself.
			want.CommitMessage = binding.replay.ImportOptions.CommitMessage
		} else {
			var boundIssue *int
			if binding.declaration != nil {
				boundIssue = binding.declaration.BoundIssue
			}
			want.CommitMessage = FallbackCommitMessage(FallbackCommitMessageInput{
				Spec: binding.spec, BoundIssue: boundIssue, RunID: binding.run.ID,
				SpecDigest: binding.run.SpecDigest, Policy: policy,
			})
		}
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
				domain.HoldTrustBlocked,
			)
		}
		if productionPublicationPermanentExternalFailure(err) {
			return w.holdBlockedTask(
				ctx, task, importer.Result{CommitSHA: task.HeadSHA}, productionBlockExternal,
				domain.HoldExternalConflict,
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
	var reviewInstructions exec.ReviewInstructionBinding
	if !w.holdOnly {
		reviewInstructions, err = w.composeReviewInstructions(checkoutDir)
		if err != nil {
			return productionTaskOutcome{}, fmt.Errorf(
				"compose exact-base review instructions: %w", err)
		}
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
		if _, remediation := remediationRoundForInvocation(
			task.RunID, task.ProducingInvocationID,
		); remediation && imported.CommitSHA == binding.export.HeadSHA {
			paths := make([]string, 0, len(imported.Findings))
			allPathRejections := len(imported.Findings) > 0
			for _, finding := range imported.Findings {
				if finding.Kind != importer.FindingAllowlistViolation || finding.Path == "" {
					allPathRejections = false
					break
				}
				paths = append(paths, finding.Path)
			}
			if allPathRejections {
				return w.completeRemediationImportDissent(ctx, task, binding, paths)
			}
		}
		return productionTaskOutcome{}, fmt.Errorf("reconstructed execution export produced head %q with %d findings, want clean %q: %w",
			imported.CommitSHA, len(imported.Findings), binding.export.HeadSHA, domain.ErrParentKeyMismatch)
	}
	if binding.remediation != nil {
		sourceTree, err := w.loadRemediationSourceTree(ctx, task, binding)
		if err != nil {
			if errors.Is(err, errRemediationSourceIdentity) {
				return w.completeRemediationSourceIdentityDissent(ctx, task, binding, imported)
			}
			return productionTaskOutcome{}, err
		}
		if imported.TreeSHA == sourceTree {
			return w.completeRemediationNoop(ctx, task, binding, imported, sourceTree)
		}
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
	adoptedProfile, err := w.adoptedReviewProfileDigest(ctx, binding)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if found {
		finalizedIntent, err := w.hasFinalizedPublicationIntent(ctx, task.PublicationID)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if readyExists || finalizedIntent {
			verdict, _, err := w.assertReviewedCandidate(
				ctx, task, binding, checkpoint, reviewInstructions,
			)
			if err != nil {
				if errors.Is(err, errShadowReviewStopped) {
					return productionTaskOutcome{}, fmt.Errorf(
						"shadow review stopped after publication state existed: %w",
						domain.ErrParentKeyMismatch,
					)
				}
				if errors.Is(err, errShadowReviewBlocksReady) {
					return productionTaskOutcome{}, nil
				}
				if errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
					return w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
				}
				if errors.Is(err, domain.ErrParentKeyMismatch) {
					return w.holdBlockedTask(
						ctx, task, checkpoint.Imported,
						"Publication is durably held because this published run lacks a clean, candidate-bound review record under the current trust-approved reviewer configuration. Restore the approved reviewer configuration or disposition the run manually.",
						domain.HoldTrustBlocked,
					)
				}
				return productionTaskOutcome{}, err
			}
			candidate := productionCandidate(task, binding, checkpoint, adoptedProfile, nil)
			history, err := publish.LoadDispositionHistory(
				ctx, w.store, candidate, reviewInstructions.ResultDigest,
			)
			if err != nil {
				if errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
					return w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
				}
				if errors.Is(err, domain.ErrParentKeyMismatch) {
					return w.holdBlockedTask(
						ctx, task, checkpoint.Imported,
						"Publication is durably held because its disposition history no longer matches the current review and readiness authority.",
						domain.HoldTrustBlocked,
					)
				}
				return productionTaskOutcome{}, fmt.Errorf("load recovered publication disposition history: %w", err)
			}
			candidate.DispositionHistory = &history
			if readyExists {
				published, err := w.loadReadyPublicationOutcome(
					ctx, task, binding, checkpoint, candidate, verdict,
				)
				if err != nil {
					if held, handled, holdErr := w.holdPublicationRepairRefusal(
						ctx, task, checkpoint.Imported, err,
					); handled {
						return held, holdErr
					}
					return productionTaskOutcome{}, err
				}
				return w.completePublishedTask(ctx, task, binding, checkpoint, published, reviewInstructions)
			}
			published, outcomeFound, err := w.loadPublicationOutcome(
				ctx, task, candidate, w.convergePublicationOutcome,
			)
			if err != nil {
				if isDurablePublicationConflict(err) {
					return w.holdBlockedTask(
						ctx, task, imported,
						"Publication is durably held because the external branch or pull request conflicts with the committed identity. Inspect and repair that external state to resume recovery.",
						domain.HoldExternalConflict,
					)
				}
				if productionPublicationPermanentExternalFailure(err) {
					return w.holdBlockedTask(ctx, task, imported, productionBlockExternal, domain.HoldExternalConflict)
				}
				if held, handled, holdErr := w.holdPublicationRepairRefusal(
					ctx, task, checkpoint.Imported, err,
				); handled {
					return held, holdErr
				}
				return productionTaskOutcome{}, err
			}
			if outcomeFound {
				return w.completePublishedTask(ctx, task, binding, checkpoint, published, reviewInstructions)
			}
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
				domain.HoldRecipeRevoked,
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

	// The §7 review gate precedes every publication effect: a verification-clean
	// candidate is reviewed against its exact base and head before any branch
	// push or PR creation (issue #527). A pending review keeps the task queued
	// with nothing published; an escalated one is terminal-blocked with no PR;
	// only a clean pass falls through to publication, where the trust and
	// exact-base gates still re-gate the candidate.
	reviewState, err := w.reconcileReviewGate(
		ctx, task, binding, checkpoint, checkout, reviewInstructions,
	)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	switch reviewState {
	case productionReviewPending:
		return productionTaskOutcome{}, nil
	case productionReviewEscalated:
		return w.completeReviewEscalationTask(ctx, task, binding)
	case productionReviewPassed:
		// Fall through to the publication path below.
	}
	// Reconstruct the complete §6 verdict immediately before the first
	// publication effect. The same predicate runs again after recovery from a
	// committed external outcome, so neither the forward path nor replay can
	// flatten the review/verification pair into an inferred ready bit.
	_, persistReadiness, err := w.assertReviewedCandidate(ctx, task, binding, checkpoint, reviewInstructions)
	if err != nil {
		if errors.Is(err, errShadowReviewStopped) {
			// A shadow Stop is a definitive operator refusal at the same
			// pre-publication boundary as an escalated routed review. Use the
			// standard publication-block transaction so the task terminalizes,
			// crash recovery converges, and the durable operator surface is the
			// ordinary publish_blocked card rather than a vanished dispute.
			return w.completeBlockedTask(
				ctx, task, binding.run, checkpoint.Imported, checkpoint.Artifacts,
				productionBlockTrust,
			)
		}
		if errors.Is(err, errShadowReviewBlocksReady) {
			return productionTaskOutcome{}, nil
		}
		if errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
			return w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
		}
		if errors.Is(err, domain.ErrParentKeyMismatch) {
			return w.holdBlockedTask(
				ctx, task, checkpoint.Imported,
				"Publication is durably held because the complete current verification requirement set is not ready under its exact policy, base, and registry bindings.",
				domain.HoldTrustBlocked,
			)
		}
		return productionTaskOutcome{}, err
	}
	// The recipe is approved on this pre-publication path (guarded above), so
	// persist the readiness proofs now; the post-publication recovery path
	// defers this to its readyExists/recipe decision instead.
	if err := persistReadiness(ctx); err != nil {
		return productionTaskOutcome{}, err
	}
	adoptedProfile, adoptedErr := w.adoptedReviewProfileDigest(ctx, binding)
	if adoptedErr != nil {
		return productionTaskOutcome{}, adoptedErr
	}
	candidate := productionCandidate(task, binding, checkpoint, adoptedProfile, nil)
	dispositionHistory, err := publish.LoadDispositionHistory(
		ctx, w.store, candidate, reviewInstructions.ResultDigest,
	)
	if err != nil {
		if errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
			return w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
		}
		return productionTaskOutcome{}, fmt.Errorf("load publication disposition history: %w", err)
	}
	candidate.DispositionHistory = &dispositionHistory
	if published, found, err := w.loadPublicationOutcome(
		ctx, task, candidate, w.convergePublicationOutcome,
	); err != nil {
		if isDurablePublicationConflict(err) {
			return w.holdBlockedTask(
				ctx, task, checkpoint.Imported,
				"Publication is durably held because the external branch or pull request conflicts with the committed identity. Inspect and repair that external state to resume recovery.",
				domain.HoldExternalConflict,
			)
		}
		if productionPublicationPermanentExternalFailure(err) {
			return w.holdBlockedTask(ctx, task, checkpoint.Imported, productionBlockExternal, domain.HoldExternalConflict)
		}
		if held, handled, holdErr := w.holdPublicationRepairRefusal(
			ctx, task, checkpoint.Imported, err,
		); handled {
			return held, holdErr
		}
		return productionTaskOutcome{}, err
	} else if found {
		return w.completePublishedTask(ctx, task, binding, checkpoint, published, reviewInstructions)
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionPublicationEffect, DurableTransitionBefore); err != nil {
		return productionTaskOutcome{}, err
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
				domain.HoldExternalConflict,
			)
		}
		if productionPublicationPermanentExternalFailure(err) {
			return w.holdBlockedTask(ctx, task, checkpoint.Imported, productionBlockExternal, domain.HoldExternalConflict)
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
					domain.HoldTrustBlocked,
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
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionPublicationEffect, DurableTransitionAfter); err != nil {
		return productionTaskOutcome{}, err
	}
	if w.afterPublication != nil {
		if err := w.afterPublication(); err != nil {
			return productionTaskOutcome{}, fmt.Errorf("after production publication: %w",
				errors.Join(err, errProductionCrashSeam))
		}
	}
	return w.completePublishedTask(ctx, task, binding, checkpoint, published, reviewInstructions)
}

type productionReviewGateState uint8

const (
	productionReviewPending productionReviewGateState = iota
	productionReviewPassed
	productionReviewEscalated
)

func (w *productionPublicationWorkflow) productionReviewRequest(
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	id domain.InvocationID,
	round int,
	instructions exec.ReviewInstructionBinding,
) (exec.ReviewRequest, domain.Digest, error) {
	reviewWorkspace, err := w.reviewWorkspacePath(id)
	if err != nil {
		return exec.ReviewRequest{}, "", err
	}
	artifactDigests := make([]domain.Digest, len(checkpoint.Artifacts))
	for i := range checkpoint.Artifacts {
		artifactDigests[i] = checkpoint.Artifacts[i].Digest
	}
	verification, err := exec.NewReviewVerificationEvidence(
		exec.ReviewVerificationEvidence{
			Outcome:                checkpoint.Authorization.VerificationOutcome,
			RecipeDigest:           checkpoint.Authorization.VerificationRecipeDigest,
			EvidenceSnapshotDigest: checkpoint.Authorization.EvidenceSnapshotDigest,
			ArtifactDigests:        artifactDigests,
		},
	)
	if err != nil {
		return exec.ReviewRequest{}, "", err
	}
	request := exec.ReviewRequest{
		RunID: task.RunID, Round: round,
		Repo: binding.admission.Base.Repo, RepositoryID: binding.admission.Base.RepositoryID,
		BaseRef: binding.admission.Base.BaseRef,
		BaseSHA: binding.admission.Base.BaseSHA, HeadSHA: task.HeadSHA,
		Workspace: reviewWorkspace, Verification: verification,
		Instructions: instructions, RequestedAt: w.now().UTC(),
	}
	authority, err := request.AuthorityDigest()
	return request, authority, err
}

// ProductionReviewInvocationID derives the bounded ward identity for one
// run/round without embedding an arbitrarily long durable run ID.
func ProductionReviewInvocationID(runID domain.RunID, round int) domain.InvocationID {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", runID, round)))
	return domain.InvocationID(fmt.Sprintf("review-%x", sum[:12]))
}

func (w *productionPublicationWorkflow) reviewWorkspacePath(
	id domain.InvocationID,
) (string, error) {
	name := string(id)
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("review workspace invocation %q: %w", id, domain.ErrPathBoundaryMismatch)
	}
	return filepath.Join(w.workDir, "review-workspaces", name), nil
}

func findingAdjudicationBaseWorkspaceID(id domain.InvocationID) domain.InvocationID {
	return domain.InvocationID(string(id) + "-adjudication-base")
}

func (w *productionPublicationWorkflow) ensureReviewWorkspace(
	ctx context.Context, id domain.InvocationID, checkout PublicationCheckout, headSHA string,
) (string, error) {
	target, err := w.reviewWorkspacePath(id)
	if err != nil {
		return "", err
	}
	targetExists := false
	if info, err := os.Lstat(target); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("review workspace %q is not an owned directory: %w",
				target, domain.ErrPathBoundaryMismatch)
		}
		targetExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	// Retain, not move: under the pre-publication review anchor the sealed
	// publication checkout must survive for the subsequent PushHead when a
	// synchronous review source completes inline. The transport claims the
	// fresh staged destination and copies from that capability, so raw
	// materialization cannot clear a caller-nominated existing directory. Stage
	// the retained candidate in a sibling temp dir, then rename it, so it cannot leave a
	// partial workspace that a resumed unknown invocation could use on a later
	// pass. An existing path can be a pre-upgrade, unmaterialized copy from the
	// same pending invocation, so unknown means rebuild rather than accept it.
	// The staged materialization finishes before the old directory is removed;
	// os.Rename cannot replace a non-empty directory. If the process stops in
	// the resulting absent window, the same guard rebuilds on the next pass.
	staging, err := os.MkdirTemp(filepath.Dir(target), "staging-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging) //nolint:errcheck // best-effort staging cleanup
	staged := filepath.Join(staging, "workspace")
	if err := w.transport.RetainWorktree(ctx, checkout, staged, headSHA); err != nil {
		return "", fmt.Errorf("materialize review workspace: %w", err)
	}
	if targetExists {
		if err := os.RemoveAll(target); err != nil {
			return "", fmt.Errorf("replace review workspace: %w", err)
		}
	}
	if err := os.Rename(staged, target); err != nil {
		return "", fmt.Errorf("retain review workspace: %w", err)
	}
	return target, nil
}

func (w *productionPublicationWorkflow) removeReviewWorkspace(id domain.InvocationID) error {
	target, err := w.reviewWorkspacePath(id)
	if err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to remove unowned review workspace %q: %w",
			target, domain.ErrPathBoundaryMismatch)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove review workspace %q: %w", target, err)
	}
	return nil
}

func (w *productionPublicationWorkflow) reconcileReviewGate(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	workspace PublicationCheckout,
	reviewInstructions exec.ReviewInstructionBinding,
) (productionReviewGateState, error) {
	if binding.profile.Review.Mode != domain.ReviewFreesideInvoked {
		return productionReviewPending, fmt.Errorf(
			"production profile review mode %q cannot satisfy readiness: %w",
			binding.profile.Review.Mode, domain.ErrInvalidReviewMode,
		)
	}
	latestRecord, latestFailure, err := w.latestReviewState(ctx, task.RunID)
	if err != nil {
		return productionReviewPending, err
	}
	if latestRecord != nil && latestRecord.Outcome != domain.ReviewFindings {
		if err := w.removeReviewWorkspace(latestRecord.InvocationID); err != nil {
			return productionReviewPending, err
		}
	}
	if latestFailure != nil {
		if err := w.removeReviewWorkspace(latestFailure.InvocationID); err != nil {
			return productionReviewPending, err
		}
	}
	recoveredContradiction := false
	if latestFailure != nil && latestFailure.Class == domain.ReviewFailureContradiction &&
		(latestRecord == nil || latestFailure.Round > latestRecord.Round) {
		if latestFailure.BaseSHA != binding.admission.Base.BaseSHA ||
			latestFailure.HeadSHA != task.HeadSHA {
			return productionReviewPending, fmt.Errorf(
				"latest review failure is bound to a different candidate: %w",
				domain.ErrParentKeyMismatch,
			)
		}
		recovered, err := w.reviewContradictionRecovered(ctx, *latestFailure)
		if err != nil {
			return productionReviewPending, err
		}
		if !recovered {
			if err := w.putReviewContradictionAttention(ctx, task, *latestFailure); err != nil {
				return productionReviewPending, err
			}
			return productionReviewPending, nil
		}
		recoveredContradiction = true
	}
	if binding.profile.Review.ConfigDigest != w.reviewConfigurationDigest {
		// The gate consults the adoption whenever the pinned profile disagrees
		// with the effective configuration, not only while the parked failure
		// outranks the latest review row: after the adopted round persists its
		// record (or a later transient failure), a crash-recovered pass must
		// still fall through here instead of re-recording a configuration
		// failure and re-parking an already recovered run.
		approved, err := w.reviewConfigurationApproved(ctx, binding)
		if err != nil {
			return productionReviewPending, err
		}
		if !approved {
			if latestFailure != nil && latestFailure.Class == domain.ReviewFailureConfiguration &&
				(latestRecord == nil || latestFailure.Round > latestRecord.Round) {
				return w.parkReviewConfiguration(ctx, task, binding, *latestFailure)
			}
			round := 1
			if latestRecord != nil {
				round = latestRecord.Round + 1
			}
			if latestFailure != nil && latestFailure.Round >= round {
				round = latestFailure.Round + 1
			}
			return w.recordReviewSourceFailure(ctx, task,
				ProductionReviewInvocationID(task.RunID, round), round,
				binding.admission.Base.BaseSHA, task.HeadSHA,
				&exec.ReviewSourceFailure{
					Class: domain.ReviewFailureConfiguration,
					Err: reviewConfigurationUnapprovedError(
						binding.profile.Review.ConfigDigest,
						w.reviewConfigurationDigest,
					),
				})
		}
		// approved: the operator-authorized supersession approves the
		// effective configuration, so the gate falls through; when the latest
		// row is still the parked failure, the round machinery below re-gates
		// the exact-failure adoption binding before advancing past it.
	}
	if latestRecord != nil && latestRecord.ConfigurationDigest == w.reviewConfigurationDigest &&
		(latestFailure == nil || latestRecord.Round > latestFailure.Round) {
		if latestRecord.BaseSHA != binding.admission.Base.BaseSHA || latestRecord.HeadSHA != task.HeadSHA {
			superseded, err := w.remediationSupersedesReview(ctx, task, *latestRecord)
			if err != nil {
				return productionReviewPending, err
			}
			if !superseded {
				return productionReviewPending, fmt.Errorf(
					"latest review record is bound to a different candidate: %w", domain.ErrParentKeyMismatch,
				)
			}
		} else if latestRecord.InstructionDigest == reviewInstructions.ResultDigest {
			var remediationOutcome remediationReviewOutcome
			if _, remediated := remediationRoundForInvocation(
				task.RunID, task.ProducingInvocationID,
			); remediated {
				findings, err := w.reviewRecordFindings(ctx, *latestRecord)
				if err != nil {
					return productionReviewPending, err
				}
				remediationOutcome, err = w.reconcileRemediationReview(
					ctx, task, *latestRecord, findings, checkpoint.Imported.Claims)
				if err != nil {
					return productionReviewPending, err
				}
				if remediationOutcome.attention != "" {
					if err := w.putReviewAttentionWithActionsAndID(
						ctx, task, *latestRecord, remediationOutcome.attention,
						domain.AttentionReviewDispute,
						productionReviewItemID(task.RunID, latestRecord.Round),
						[]domain.Action{domain.ActionAdjudicate, domain.ActionDiscuss, domain.ActionStop},
						remediationOutcome.claims,
					); err != nil {
						return productionReviewPending, err
					}
					return productionReviewEscalated, nil
				}
			}
			shadowComplete, err := w.reconcileShadowReview(
				ctx, task, binding, checkpoint, workspace, reviewInstructions, *latestRecord,
			)
			if err != nil {
				return productionReviewPending, err
			}
			if !shadowComplete {
				return productionReviewPending, nil
			}
			if latestRecord.Outcome == domain.ReviewFindings {
				candidateWorkspace, err := w.ensureReviewWorkspace(
					ctx, latestRecord.InvocationID, workspace, task.HeadSHA)
				if err != nil {
					return productionReviewPending, err
				}
				baseWorkspaceID := findingAdjudicationBaseWorkspaceID(latestRecord.InvocationID)
				baseWorkspace, err := w.ensureReviewWorkspace(
					ctx, baseWorkspaceID, workspace, binding.admission.Base.BaseSHA)
				if err != nil {
					return productionReviewPending, err
				}
				var state productionReviewGateState
				if remediationOutcome.dissent != nil {
					state, err = w.reenterFindingAdjudication(
						ctx, task, binding, *latestRecord, baseWorkspace,
						candidateWorkspace, *remediationOutcome.dissent)
				} else {
					state, err = w.reconcileFindingAdjudication(
						ctx, task, binding, *latestRecord, baseWorkspace, candidateWorkspace)
				}
				if err != nil {
					return productionReviewPending, err
				}
				if err := w.removeReviewWorkspace(latestRecord.InvocationID); err != nil {
					return productionReviewPending, err
				}
				if err := w.removeReviewWorkspace(baseWorkspaceID); err != nil {
					return productionReviewPending, err
				}
				return state, nil
			}
			_, requestAuthority, err := w.productionReviewRequest(
				task, binding, checkpoint, latestRecord.InvocationID,
				latestRecord.Round, reviewInstructions,
			)
			if err != nil {
				return productionReviewPending, err
			}
			authorityVerifier, ok := w.reviewSource.(exec.ReviewRequestAuthorityVerifier)
			if !ok {
				return productionReviewPending, errors.New(
					"review source cannot re-gate persisted request authority")
			}
			if err := authorityVerifier.VerifyRequestAuthority(
				ctx, latestRecord.InvocationID, requestAuthority,
			); err != nil {
				return productionReviewPending, fmt.Errorf(
					"re-gate persisted review instruction authority: %w", err)
			}
			// This clean-pass re-entry is downstream of the record write that
			// already cleared the durable row; clear its durable twin too so a
			// crash-recovered pass never leaves an orphan pending retry.
			if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
				return tx.DeleteReviewRetry(ctx, task.RunID)
			}); err != nil {
				return productionReviewPending, err
			}
			delete(w.reviewRetryAfter, task.RunID)
			return productionReviewPassed, nil
		}
	}

	round := 1
	if latestRecord != nil {
		round = latestRecord.Round + 1
	}
	if latestFailure != nil && latestFailure.Round >= round {
		if latestFailure.BaseSHA != binding.admission.Base.BaseSHA ||
			latestFailure.HeadSHA != task.HeadSHA {
			return productionReviewPending, fmt.Errorf(
				"latest review failure is bound to a different candidate: %w",
				domain.ErrParentKeyMismatch,
			)
		}
		round = latestFailure.Round + 1
		if latestFailure.Class == domain.ReviewFailureQuota {
			record := domain.ReviewRecord{
				InvocationID: latestFailure.InvocationID, RunID: latestFailure.RunID,
				Round: latestFailure.Round, BaseSHA: latestFailure.BaseSHA, HeadSHA: latestFailure.HeadSHA,
			}
			if err := w.putReviewAttention(ctx, task, record,
				fmt.Sprintf("Codex review stopped because of a %s failure.", latestFailure.Class),
				domain.AttentionReviewDispute,
			); err != nil {
				return productionReviewPending, err
			}
			return productionReviewEscalated, nil
		}
		if latestFailure.Class == domain.ReviewFailureConfiguration {
			adopted, err := w.reviewConfigurationRecovered(ctx, binding, *latestFailure)
			if err != nil {
				return productionReviewPending, err
			}
			if !adopted {
				return w.parkReviewConfiguration(ctx, task, binding, *latestFailure)
			}
			// The adopted failure row's round may sit exactly at the hard
			// limit; the exhaustion item below must then live under the
			// recovered-identity namespace, because this round's ordinary
			// identity already carries the concluded review_configuration item.
			recoveredContradiction = true
		}
		if latestFailure.Class == domain.ReviewFailureTransient {
			retryAt := latestFailure.ObservedAt.Add(reviewRetryDelay(latestFailure.Round))
			if w.now().Before(retryAt) {
				w.reviewRetryAfter[task.RunID] = retryAt
			}
		}
	}
	hardLimit, err := w.reviewHardRoundLimit(ctx, task.RunID)
	if err != nil {
		return productionReviewPending, err
	}
	if round > hardLimit {
		record := domain.ReviewRecord{
			RunID: task.RunID, Round: hardLimit,
			BaseSHA: binding.admission.Base.BaseSHA, HeadSHA: task.HeadSHA,
		}
		if err := w.putReviewAttentionWithID(ctx, task, record,
			fmt.Sprintf("Review exhausted the resolved hard limit of %d rounds.", hardLimit),
			domain.AttentionReviewDiminishing,
			productionReviewHardLimitItemID(task.RunID, hardLimit, recoveredContradiction),
		); err != nil {
			return productionReviewPending, err
		}
		return productionReviewEscalated, nil
	}
	if retryAt, ok := w.reviewRetryAfter[task.RunID]; ok {
		if w.now().Before(retryAt) {
			return productionReviewPending, nil
		}
	} else {
		// No process-local deadline: a restart may have dropped a pending
		// same-invocation retry. Reconstruct it from the durable row, which
		// fails closed on a corrupt or unreadable row rather than retrying.
		pending, err := w.reconstructReviewRetryDeadline(ctx, task, binding, round)
		if err != nil {
			return productionReviewPending, err
		}
		if pending {
			return productionReviewPending, nil
		}
	}

	id := ProductionReviewInvocationID(task.RunID, round)
	req, requestAuthority, err := w.productionReviewRequest(
		task, binding, checkpoint, id, round, reviewInstructions,
	)
	if err != nil {
		return productionReviewPending, err
	}
	reviewWorkspace := req.Workspace
	authorityVerifier, ok := w.reviewSource.(exec.ReviewRequestAuthorityVerifier)
	if !ok {
		return w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			&exec.ReviewSourceFailure{
				Class: domain.ReviewFailureConfiguration,
				Err:   errors.New("review source cannot verify request authority"),
			},
		)
	}
	if supersessionVerifier, ok := w.reviewSource.(exec.ReviewRequestSupersessionVerifier); ok {
		if err := supersessionVerifier.VerifyReviewRequestSupersession(ctx, id, req); err != nil &&
			!errors.Is(err, exec.ErrUnknownInvocation) {
			if errors.Is(err, exec.ErrSupersededReviewRequest) || errors.Is(err, exec.ErrLegacyReviewRequest) {
				return w.recordReviewSourceFailure(ctx, task, id, round,
					binding.admission.Base.BaseSHA, task.HeadSHA,
					&exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err})
			}
			return w.retryOrRecordReviewFailure(ctx, task, id, round,
				binding.admission.Base.BaseSHA, task.HeadSHA, err)
		}
	}
	// Re-gate the persisted request against the engine's current authority
	// before Inspect may act on it: Inspect's restart-recovery window
	// relaunches from the decoded journal row, so a rewritten-but-valid row
	// must fail closed here, before it can prepare a workspace or start a
	// credential-bearing review, not only after Poll delivers a result.
	if err := authorityVerifier.VerifyRequestAuthority(ctx, id, requestAuthority); err != nil &&
		!errors.Is(err, exec.ErrUnknownInvocation) {
		if errors.Is(err, exec.ErrLegacyReviewRequest) {
			return w.recordReviewSourceFailure(ctx, task, id, round,
				binding.admission.Base.BaseSHA, task.HeadSHA,
				&exec.ReviewSourceFailure{
					Class: domain.ReviewFailureTransient,
					Err:   err,
				},
			)
		}
		return w.retryOrRecordReviewFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA, err)
	}
	status, err := w.reviewSource.Inspect(ctx, id)
	if errors.Is(err, exec.ErrUnknownInvocation) {
		// Unknown is also the workspace-staleness signal: no live review
		// container can reference this invocation's retained path, and the
		// launch below will seed its replacement. Keep that answer in this
		// control flow instead of querying the review source again.
		retainedWorkspace, workspaceErr := w.ensureReviewWorkspace(ctx, id, workspace, task.HeadSHA)
		if workspaceErr != nil {
			if errors.Is(workspaceErr, publish.ErrMaterializationRefused) {
				return w.recordReviewSourceFailure(ctx, task, id, round,
					binding.admission.Base.BaseSHA, task.HeadSHA,
					&exec.ReviewSourceFailure{
						Class: domain.ReviewFailureConfiguration,
						Err:   workspaceErr,
					},
				)
			}
			return productionReviewPending, workspaceErr
		}
		if retainedWorkspace != reviewWorkspace {
			return productionReviewPending, fmt.Errorf(
				"retained review workspace changed: %w", domain.ErrPathBoundaryMismatch,
			)
		}
		if err := runDurableTransitionHook(w.transitionHook,
			DurableTransitionReviewRequest, DurableTransitionBefore); err != nil {
			return productionReviewPending, err
		}
		if err := w.reviewSource.RequestReview(ctx, id, req); err != nil {
			return w.retryOrRecordReviewFailure(ctx, task, id, round, req.BaseSHA, req.HeadSHA, err)
		}
		if err := runDurableTransitionHook(w.transitionHook,
			DurableTransitionReviewRequest, DurableTransitionAfter); err != nil {
			return productionReviewPending, err
		}
		status, err = w.reviewSource.Inspect(ctx, id)
		if err != nil {
			return w.retryOrRecordReviewFailure(ctx, task, id, round, req.BaseSHA, req.HeadSHA, err)
		}
	}
	if err != nil {
		return w.retryOrRecordReviewFailure(
			ctx, task, id, round, binding.admission.Base.BaseSHA, task.HeadSHA, err,
		)
	}
	if status == exec.StatusPending || status == exec.StatusRunning {
		return productionReviewPending, nil
	}
	result, err := w.reviewSource.Poll(ctx, id)
	if errors.Is(err, exec.ErrNoResult) {
		err = normalizeTerminalReviewFailure(err)
	} else if err != nil {
		scheduled, schedErr := w.scheduleReviewInvocationRetry(
			ctx, id, task.RunID, round, binding.admission.Base.BaseSHA, task.HeadSHA, err)
		if schedErr != nil {
			return productionReviewPending, schedErr
		}
		if scheduled {
			return productionReviewPending, nil
		}
	}
	if errors.Is(err, exec.ErrResultNotReady) {
		return productionReviewPending, nil
	}
	if err != nil {
		return w.recordReviewSourceFailure(
			ctx, task, id, round, binding.admission.Base.BaseSHA, task.HeadSHA, err,
		)
	}
	if err := result.Validate(); err != nil {
		return w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			&exec.ReviewSourceFailure{Class: domain.ReviewFailureContradiction, Err: err},
		)
	}
	if result.InvocationID != id || result.BaseSHA != binding.admission.Base.BaseSHA ||
		result.HeadSHA != task.HeadSHA || result.ConfigurationDigest != w.reviewConfigurationDigest {
		return w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			&exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction,
				Err:   domain.ErrParentKeyMismatch,
			},
		)
	}
	if result.InstructionDigest != req.Instructions.ResultDigest {
		return w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA,
			&exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction,
				Err:   domain.ErrParentKeyMismatch,
			},
		)
	}
	if err := authorityVerifier.VerifyRequestAuthority(ctx, id, requestAuthority); err != nil {
		return w.retryOrRecordReviewFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA, err,
		)
	}
	if err := w.reviewSource.Verify(ctx, id, binding.admission.Base.BaseSHA, task.HeadSHA); err != nil {
		scheduled, schedErr := w.scheduleReviewInvocationRetry(
			ctx, id, task.RunID, round, binding.admission.Base.BaseSHA, task.HeadSHA, err)
		if schedErr != nil {
			return productionReviewPending, schedErr
		}
		if scheduled || errors.Is(err, exec.ErrResultNotReady) {
			return productionReviewPending, nil
		}
		return w.recordReviewSourceFailure(ctx, task, id, round,
			binding.admission.Base.BaseSHA, task.HeadSHA, err,
		)
	}
	for _, finding := range result.Findings {
		if finding.RunID != task.RunID {
			return w.recordReviewSourceFailure(ctx, task, id, round,
				binding.admission.Base.BaseSHA, task.HeadSHA,
				&exec.ReviewSourceFailure{
					Class: domain.ReviewFailureContradiction,
					Err:   domain.ErrParentKeyMismatch,
				},
			)
		}
	}
	outcome := domain.ReviewClean
	findingIDs := make([]domain.FindingID, len(result.Findings))
	if len(result.Findings) > 0 {
		outcome = domain.ReviewFindings
		for i := range result.Findings {
			findingIDs[i] = result.Findings[i].ID
		}
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: id, RunID: task.RunID, Round: round,
		Provider: result.Provider, ModelConfiguration: result.ModelConfiguration,
		ConfigurationDigest: result.ConfigurationDigest,
		InstructionDigest:   result.InstructionDigest,
		CostOwner:           result.CostOwner, BaseSHA: result.BaseSHA, HeadSHA: result.HeadSHA,
		CompletedAt: result.CompletedAt, CompletionEvidence: result.CompletionEvidence,
		Outcome: outcome, FindingIDs: findingIDs,
	})
	if err != nil {
		return productionReviewPending, err
	}
	remediationOutcome, err := w.reconcileRemediationReview(
		ctx, task, record, result.Findings, checkpoint.Imported.Claims)
	if err != nil {
		return productionReviewPending, err
	}
	// The completed record supersedes any pending same-invocation retry for
	// this run: clear the durable row atomically with the record it writes.
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionReviewResult, DurableTransitionBefore); err != nil {
		return productionReviewPending, err
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(ctx, record, result.Findings); err != nil {
			return err
		}
		for _, disposition := range remediationOutcome.dispositions {
			if err := tx.PutFindingDisposition(ctx, disposition); err != nil {
				return err
			}
		}
		return tx.DeleteReviewRetry(ctx, task.RunID)
	}); err != nil {
		return productionReviewPending, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionReviewResult, DurableTransitionAfter); err != nil {
		return productionReviewPending, err
	}
	shadowComplete, err := w.reconcileShadowReview(
		ctx, task, binding, checkpoint, workspace, reviewInstructions, record,
	)
	if err != nil {
		return productionReviewPending, err
	}
	if !shadowComplete {
		return productionReviewPending, nil
	}
	if remediationOutcome.attention != "" {
		if err := w.putReviewAttentionWithActionsAndID(
			ctx, task, record, remediationOutcome.attention, domain.AttentionReviewDispute,
			productionReviewItemID(task.RunID, record.Round),
			[]domain.Action{domain.ActionAdjudicate, domain.ActionDiscuss, domain.ActionStop},
			remediationOutcome.claims,
		); err != nil {
			return productionReviewPending, err
		}
		return productionReviewEscalated, nil
	}
	if record.Outcome == domain.ReviewFindings {
		baseWorkspaceID := findingAdjudicationBaseWorkspaceID(id)
		baseWorkspace, err := w.ensureReviewWorkspace(
			ctx, baseWorkspaceID, workspace, binding.admission.Base.BaseSHA)
		if err != nil {
			return productionReviewPending, err
		}
		var state productionReviewGateState
		if remediationOutcome.dissent != nil {
			state, err = w.reenterFindingAdjudication(
				ctx, task, binding, record, baseWorkspace, reviewWorkspace,
				*remediationOutcome.dissent)
		} else {
			state, err = w.reconcileFindingAdjudication(
				ctx, task, binding, record, baseWorkspace, reviewWorkspace)
		}
		if err != nil {
			return productionReviewPending, err
		}
		if err := w.removeReviewWorkspace(id); err != nil {
			return productionReviewPending, err
		}
		if err := w.removeReviewWorkspace(baseWorkspaceID); err != nil {
			return productionReviewPending, err
		}
		return state, nil
	}
	if err := w.removeReviewWorkspace(id); err != nil {
		return productionReviewPending, err
	}
	return productionReviewPassed, nil
}

func (w *productionPublicationWorkflow) classifyReviewFindings(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
) (bool, error) {
	if w.inference == nil || record.Outcome != domain.ReviewFindings {
		return false, nil
	}
	requiresAttention := false
	for _, findingID := range record.FindingIDs {
		var (
			finding        domain.Finding
			classification domain.Classification
			classified     bool
		)
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			finding, err = tx.GetFinding(ctx, findingID)
			if err != nil {
				return err
			}
			classification, err = tx.GetClassification(ctx, findingID, record.Round)
			if err == nil {
				classified = true
				return nil
			}
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}); err != nil {
			return true, err
		}
		if classified {
			persistedAttention, err := w.inference.EvaluateClassification(finding, classification)
			if err != nil {
				return true, err
			}
			requiresAttention = requiresAttention || persistedAttention
			continue
		}
		decision, err := w.inference.ClassifyFinding(
			ctx, string(task.ProjectID), string(task.RunID), finding, record.Round,
		)
		if err != nil {
			return true, err
		}
		requiresAttention = requiresAttention || decision.RequiresAttention
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutClassification(ctx, decision.Classification)
		}); err != nil {
			return true, err
		}
	}
	return requiresAttention, nil
}

func normalizeTerminalReviewFailure(err error) error {
	var failure *exec.ReviewSourceFailure
	if errors.As(err, &failure) {
		return err
	}
	return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
}

// reviewRetryDelay is the exponential same-invocation/terminal-transient
// backoff: 1s doubling per round, capped at round 9 (2^8 s). The single
// definition keeps the same-invocation scheduler, the terminal-transient
// reconstruction, and the durable-row reconstruction from drifting apart.
func reviewRetryDelay(round int) time.Duration {
	return time.Second << min(round-1, 8)
}

// scheduleReviewInvocationRetry paces a same-invocation transient retry: it
// sets the process-local deadline and durably records it so a restart during
// the backoff reconstructs the remaining delay instead of retrying
// immediately (issue #498). The row does not terminalize or advance the
// invocation. On a persist failure it still holds the process-local bound,
// then returns the error so the pass surfaces it rather than proceeding.
func (w *productionPublicationWorkflow) scheduleReviewInvocationRetry(
	ctx context.Context, id domain.InvocationID, runID domain.RunID, round int,
	baseSHA, headSHA string, cause error,
) (bool, error) {
	if exec.ClassifyReviewSourceFailure(cause) != domain.ReviewFailureTransient {
		return false, nil
	}
	observedAt := w.now().UTC()
	w.reviewRetryAfter[runID] = observedAt.Add(reviewRetryDelay(round))
	retry := domain.ReviewRetry{
		RunID: runID, InvocationID: id, Round: round,
		BaseSHA: baseSHA, HeadSHA: headSHA, ObservedAt: observedAt, Reason: cause.Error(),
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRetry(ctx, retry)
	}); err != nil {
		return true, fmt.Errorf("persist review retry %q: %w", id, err)
	}
	return true, nil
}

// retryOrRecordReviewFailure paces a transient observation failure as a
// same-invocation retry, or records a terminal failure otherwise. It
// centralizes the pending-or-record branch shared by the observation call
// sites; the two sites that also fold in ErrResultNotReady stay inline.
func (w *productionPublicationWorkflow) retryOrRecordReviewFailure(
	ctx context.Context, task productionPublicationTask, id domain.InvocationID,
	round int, baseSHA, headSHA string, cause error,
) (productionReviewGateState, error) {
	scheduled, err := w.scheduleReviewInvocationRetry(ctx, id, task.RunID, round, baseSHA, headSHA, cause)
	if err != nil {
		return productionReviewPending, err
	}
	if scheduled {
		return productionReviewPending, nil
	}
	return w.recordReviewSourceFailure(ctx, task, id, round, baseSHA, headSHA, cause)
}

// reconstructReviewRetryDeadline restores a pending same-invocation retry that
// a restart dropped from the process-local map. The decoded row is a delay
// claim, never authority: a row bound to a superseded round, invocation, or
// candidate is stale and dropped, and the deadline is re-derived from the
// row's round, so the row can only postpone a retry, never authorize skipping
// backoff, changing the invocation, or advancing the round. It returns
// (true, nil) when a live deadline still binds the pass to pending.
func (w *productionPublicationWorkflow) reconstructReviewRetryDeadline(
	ctx context.Context, task productionPublicationTask, binding productionBinding, round int,
) (bool, error) {
	var retry domain.ReviewRetry
	found := false
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetReviewRetry(ctx, task.RunID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		retry = got
		found = true
		return nil
	}); err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if retry.Round != round ||
		retry.InvocationID != ProductionReviewInvocationID(task.RunID, round) ||
		retry.BaseSHA != binding.admission.Base.BaseSHA || retry.HeadSHA != task.HeadSHA {
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.DeleteReviewRetry(ctx, task.RunID)
		}); err != nil {
			return false, err
		}
		return false, nil
	}
	retryAt := retry.ObservedAt.Add(reviewRetryDelay(retry.Round))
	if w.now().Before(retryAt) {
		w.reviewRetryAfter[task.RunID] = retryAt
		return true, nil
	}
	// Deadline already passed: proceed now; the next transient overwrites the
	// row and any terminal outcome clears it.
	return false, nil
}

func (w *productionPublicationWorkflow) latestReviewState(
	ctx context.Context, runID domain.RunID,
) (*domain.ReviewRecord, *domain.ReviewFailure, error) {
	var record *domain.ReviewRecord
	var failure *domain.ReviewFailure
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		gotRecord, err := tx.LatestReviewRecord(ctx, runID)
		if err == nil {
			record = &gotRecord
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		gotFailure, err := tx.LatestReviewFailure(ctx, runID)
		if err == nil {
			failure = &gotFailure
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	})
	return record, failure, err
}

func (w *productionPublicationWorkflow) reviewHardRoundLimit(
	ctx context.Context, runID domain.RunID,
) (int, error) {
	const defaultHardRoundLimit = 25
	var policy domain.ResolvedPolicy
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		policy, err = tx.GetResolvedPolicy(ctx, runID)
		return err
	}); err != nil {
		return 0, err
	}
	for _, key := range policy.Keys {
		if key.Key != "review.hard_round_limit" {
			continue
		}
		limit, err := strconv.Atoi(key.Value)
		if err != nil || limit < 1 {
			return 0, fmt.Errorf("resolved review.hard_round_limit %q: %w",
				key.Value, domain.ErrNonPositive)
		}
		return limit, nil
	}
	return defaultHardRoundLimit, nil
}

func (w *productionPublicationWorkflow) recordReviewSourceFailure(
	ctx context.Context,
	task productionPublicationTask,
	id domain.InvocationID,
	round int,
	baseSHA, headSHA string,
	sourceErr error,
) (productionReviewGateState, error) {
	class := exec.ClassifyReviewSourceFailure(sourceErr)
	failure := domain.ReviewFailure{
		InvocationID: id, RunID: task.RunID, Round: round,
		BaseSHA: baseSHA, HeadSHA: headSHA, Class: class,
		Reason: sourceErr.Error(), ObservedAt: w.now().UTC(),
	}
	// The terminal outcome supersedes any pending same-invocation retry for
	// this run: clear the durable row atomically with the failure it records.
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewFailure(ctx, failure); err != nil {
			return err
		}
		return tx.DeleteReviewRetry(ctx, task.RunID)
	}); err != nil {
		return productionReviewPending, err
	}
	if err := w.removeReviewWorkspace(id); err != nil {
		return productionReviewPending, err
	}
	switch class {
	case domain.ReviewFailureTransient:
		// A terminal transient advances the round; its deadline reconstructs
		// from the persisted ReviewFailure.ObservedAt, not the same-invocation
		// row (which the write above cleared).
		w.reviewRetryAfter[task.RunID] = w.now().Add(reviewRetryDelay(round))
		return productionReviewPending, nil
	case domain.ReviewFailureQuota:
		record := domain.ReviewRecord{
			InvocationID: failure.InvocationID, RunID: failure.RunID,
			Round: failure.Round, BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA,
		}
		if err := w.putReviewAttention(ctx, task, record,
			fmt.Sprintf("Codex review stopped because of a %s failure.", class),
			domain.AttentionReviewDispute,
		); err != nil {
			return productionReviewPending, err
		}
		return productionReviewEscalated, nil
	case domain.ReviewFailureConfiguration:
		// Parked, never terminalized (issue #611, revising issue #527
		// decision 3 for this class: the approved configuration cannot always
		// be restored, because the change may itself be a required safety
		// fix). The run stays tickable; the review gate raises the
		// recovery-bearing item with the trust binding in scope on the next
		// reconcile and holds the run until an operator decision.
		return productionReviewPending, nil
	case domain.ReviewFailureContradiction:
		if err := w.putReviewContradictionAttention(ctx, task, failure); err != nil {
			return productionReviewPending, err
		}
		return productionReviewPending, nil
	}
	return productionReviewPending, fmt.Errorf("review invocation %q has invalid failure class: %w",
		id, domain.ErrInvalidReviewFailureClass)
}

// reviewContradictionRecovered reports whether the latest command-backed
// transition for this run matches every coordinate and the exact persisted
// body digest of failure. An older recovery deliberately does not authorize a
// later contradiction.
func (w *productionPublicationWorkflow) reviewContradictionRecovered(
	ctx context.Context, failure domain.ReviewFailure,
) (bool, error) {
	var recovered bool
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		digest, err := tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
		if err != nil {
			return err
		}
		transition, found, err := tx.LatestReviewRecoveryTransition(ctx, failure.RunID)
		if err != nil {
			return err
		}
		recovered = found && transition.Binding().Matches(failure, digest)
		return nil
	})
	return recovered, err
}

// putReviewContradictionAttention exposes one persisted contradiction without
// terminalizing its publication task. The deterministic identity plus the
// preflight below keeps subsequent ticks from rewriting the same open item or
// consuming a new sync revision; any divergent row fails closed.
func (w *productionPublicationWorkflow) putReviewContradictionAttention(
	ctx context.Context, task productionPublicationTask, failure domain.ReviewFailure,
) error {
	var digest domain.Digest
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		digest, err = tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
		return err
	}); err != nil {
		return err
	}
	binding := domain.ReviewRecoveryBinding{
		RunID: failure.RunID, InvocationID: failure.InvocationID, Round: failure.Round,
		BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA, FailureDigest: digest,
	}
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: productionReviewItemID(task.RunID, failure.Round), ProjectID: task.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
		},
		Type: domain.AttentionReviewContradiction, Priority: domain.PriorityHigh,
		Reason:            fmt.Sprintf("Codex review contradicted its execution contract: %s", failure.Reason),
		RequestedDecision: []domain.Action{domain.ActionRecoverReview},
		PRHeadSHA:         failure.HeadSHA, ReviewRecoveryBinding: &binding,
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt,
		Status:    domain.StatusOpen,
	}, w.approvedRecipes)
	if err != nil {
		return err
	}
	var existing *domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, item.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		existing = &got
		return nil
	}); err != nil {
		return err
	}
	if existing != nil {
		item.CreatedAt = existing.CreatedAt
		// Delivery receipts legitimately advance the item's version and derived
		// timing while it remains parked. Ignore only those two mutable telemetry
		// fields; every identity, presentation, action, and recovery coordinate
		// must still match the persisted contradiction exactly.
		comparable := *existing
		comparable.ItemVersion = item.ItemVersion
		comparable.Timing = item.Timing
		if !reflect.DeepEqual(comparable, item) {
			return fmt.Errorf("review contradiction item %q diverges from failure %q: %w",
				item.ID, failure.InvocationID, domain.ErrReviewRecoveryBindingMismatch)
		}
		return nil
	}
	return w.attention.PutItem(ctx, item)
}

// reviewConfigurationAdoption returns the run's operator-authorized profile
// supersession when one is effective right now. Every trust fact is
// re-derived on this read: the transition itself re-gates its command, item
// binding, immutable failure row, latest-profile status, and
// review-configuration-only delta inside the store; this helper adds the run
// binding (the superseded revision must be the run's admission-pinned
// profile) and the effective-configuration equality (the adopted revision
// must approve the digest this daemon actually computes). A decoded or stale
// transition therefore never carries authority the operator did not grant to
// this exact run under this exact configuration.
func (w *productionPublicationWorkflow) reviewConfigurationAdoption(
	ctx context.Context, binding productionBinding,
) (domain.ReviewConfigurationRecoveryTransition, bool, error) {
	var (
		transition domain.ReviewConfigurationRecoveryTransition
		found      bool
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		transition, found, err = tx.LatestReviewConfigurationRecoveryTransition(ctx, binding.run.ID)
		if err != nil {
			// An ineffective row (the adopted revision was itself superseded,
			// or the delta widened) and a tampered or unbacked row all grant
			// nothing. The store classifies every determinate integrity or
			// policy rejection under one sentinel; treat it as no adoption so
			// the run stays visibly parked behind its item instead of
			// error-looping the lane, while any other failure is environmental
			// and propagates for retry.
			if errors.Is(err, domain.ErrReviewConfigRecoveryIneffective) {
				found = false
				return nil
			}
			return err
		}
		if !found {
			return nil
		}
		if transition.SupersededProfileDigest != binding.profile.ProfileDigest ||
			transition.Repo != binding.admission.Base.Repo ||
			transition.RepositoryID != binding.admission.Base.RepositoryID {
			found = false
			return nil
		}
		superseding, err := tx.GetTrustProfile(ctx, transition.SupersedingProfileDigest)
		if err != nil {
			return err
		}
		if superseding.Review.Mode != domain.ReviewFreesideInvoked ||
			superseding.Review.ConfigDigest != w.reviewConfigurationDigest {
			found = false
		}
		return nil
	})
	if err != nil || !found {
		return domain.ReviewConfigurationRecoveryTransition{}, false, err
	}
	return transition, true, nil
}

func reviewConfigurationUnapprovedError(pinned, effective domain.Digest) error {
	return fmt.Errorf(
		"profile pins %s, daemon effective is %s: %w",
		pinned, effective, domain.ErrReviewConfigurationUnapproved,
	)
}

// reviewConfigurationApproved reports whether the run's trust context
// approves the daemon's effective reviewer configuration: directly through
// its admission-pinned profile, or through an effective operator-authorized
// review-configuration-only supersession.
func (w *productionPublicationWorkflow) reviewConfigurationApproved(
	ctx context.Context, binding productionBinding,
) (bool, error) {
	if binding.profile.Review.ConfigDigest == w.reviewConfigurationDigest {
		return true, nil
	}
	_, adopted, err := w.reviewConfigurationAdoption(ctx, binding)
	return adopted, err
}

// reviewConfigurationRecovered reports whether the run's effective adoption
// also authorizes advancing past exactly this parked failure row: the
// transition must match every coordinate and the exact persisted body
// digest, so an older recovery never authorizes a later failure.
func (w *productionPublicationWorkflow) reviewConfigurationRecovered(
	ctx context.Context, binding productionBinding, failure domain.ReviewFailure,
) (bool, error) {
	transition, adopted, err := w.reviewConfigurationAdoption(ctx, binding)
	if err != nil || !adopted {
		return false, err
	}
	var digest domain.Digest
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		digest, err = tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
		return err
	}); err != nil {
		return false, err
	}
	return transition.Binding().Matches(failure, digest), nil
}

// parkReviewConfiguration holds one run on a configuration-class review
// failure without terminalizing its publication task (issue #611, revising
// issue #527 decision 3 for this class). The run stays tickable: it resumes
// when an operator adopts a review-configuration-only profile supersession
// that approves the effective configuration, and it terminalizes only when
// the operator concludes the displayed item without an effective recovery
// (a stop, or an adoption whose target has since been superseded).
func (w *productionPublicationWorkflow) parkReviewConfiguration(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	failure domain.ReviewFailure,
) (productionReviewGateState, error) {
	if binding.admission.TrustProfileDigest == nil {
		// Recovery rebinds an admission-pinned revision; a legacy admission
		// without the pin resolves its profile as the repository's latest on
		// every load, so the recovery binding would drift under the parked
		// item. Keep #527 decision 3's terminalizing dispute for that shape.
		record := domain.ReviewRecord{
			InvocationID: failure.InvocationID, RunID: failure.RunID,
			Round: failure.Round, BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA,
		}
		if err := w.putReviewAttention(ctx, task, record,
			"Codex review stopped because of a configuration failure.",
			domain.AttentionReviewDispute,
		); err != nil {
			return productionReviewPending, err
		}
		return productionReviewEscalated, nil
	}
	itemID := productionReviewItemID(task.RunID, failure.Round)
	var existing *domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		existing = &got
		return nil
	}); err != nil {
		return productionReviewPending, err
	}
	if existing != nil && existing.Type == domain.AttentionReviewDispute {
		// A pre-#611 daemon raised the terminalizing dispute at this identity
		// and crashed before completing the task. Honor the contract that
		// item presented rather than failing closed against its shape.
		return productionReviewEscalated, nil
	}
	if existing != nil && existing.Type == domain.AttentionReviewConfiguration &&
		existing.Status != domain.StatusOpen {
		// Presence, not effectiveness, separates the operator's two exits: a
		// conclusion that appended an adoption keeps the run parked (the
		// signet transaction may have landed between this tick's transition
		// and item reads, or the adoption may not be effective yet), while a
		// conclusion without one is the explicit decline that ends the run
		// exactly as #527 decision 3 always did for this class.
		var digest domain.Digest
		var adopted bool
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			digest, err = tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
			if err != nil {
				return err
			}
			adopted, err = tx.HasReviewConfigurationRecoveryTransition(
				ctx, failure.RunID, failure.InvocationID, digest)
			return err
		}); err != nil {
			return productionReviewPending, err
		}
		if adopted {
			return productionReviewPending, nil
		}
		return productionReviewEscalated, nil
	}
	if err := w.putReviewConfigurationAttention(ctx, task, binding, failure); err != nil {
		return productionReviewPending, err
	}
	return productionReviewPending, nil
}

// putReviewConfigurationAttention exposes one parked configuration failure
// without terminalizing its publication task. The deterministic identity plus
// the preflight below keeps subsequent ticks from rewriting the same open
// item or consuming a new sync revision; any divergent row fails closed.
func (w *productionPublicationWorkflow) putReviewConfigurationAttention(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	failure domain.ReviewFailure,
) error {
	var digest domain.Digest
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		digest, err = tx.ReviewFailureBodyDigest(ctx, failure.InvocationID)
		return err
	}); err != nil {
		return err
	}
	recovery := domain.ReviewConfigurationRecoveryBinding{
		RunID: failure.RunID, InvocationID: failure.InvocationID, Round: failure.Round,
		BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA, FailureDigest: digest,
		Repo: binding.admission.Base.Repo, RepositoryID: binding.admission.Base.RepositoryID,
		SupersededProfileDigest: binding.profile.ProfileDigest,
	}
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: productionReviewItemID(task.RunID, failure.Round), ProjectID: task.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
		},
		Type: domain.AttentionReviewConfiguration, Priority: domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Codex review parked on a reviewer configuration the trust profile no longer approves: %s",
			failure.Reason),
		RequestedDecision: []domain.Action{
			domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop,
		},
		PRHeadSHA: failure.HeadSHA, ReviewConfigurationRecovery: &recovery,
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &createdAt,
		Status:    domain.StatusOpen,
	}, w.approvedRecipes)
	if err != nil {
		return err
	}
	var existing *domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, item.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		existing = &got
		return nil
	}); err != nil {
		return err
	}
	if existing != nil {
		item.CreatedAt = existing.CreatedAt
		// Delivery receipts advance the item's version and derived timing, and
		// a discuss decision attaches its conversation, all while the item
		// remains parked. Ignore only those mutable fields; every identity,
		// presentation, action, and recovery coordinate must still match the
		// persisted failure exactly.
		comparable := *existing
		comparable.ItemVersion = item.ItemVersion
		comparable.Timing = item.Timing
		comparable.ConversationID = item.ConversationID
		if !reflect.DeepEqual(comparable, item) {
			return fmt.Errorf("review configuration item %q diverges from failure %q: %w",
				item.ID, failure.InvocationID, domain.ErrReviewConfigRecoveryBindingMismatch)
		}
		return nil
	}
	return w.attention.PutItem(ctx, item)
}

func (w *productionPublicationWorkflow) putReviewAttention(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	reason string,
	itemType domain.AttentionType,
) error {
	return w.putReviewAttentionWithID(
		ctx, task, record, reason, itemType,
		productionReviewItemID(task.RunID, record.Round),
	)
}

func (w *productionPublicationWorkflow) putReviewAttentionWithID(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	reason string,
	itemType domain.AttentionType,
	itemID domain.ItemID,
) error {
	return w.putReviewAttentionWithActionsAndID(
		ctx, task, record, reason, itemType, itemID,
		[]domain.Action{domain.ActionAdjudicate, domain.ActionDiscuss, domain.ActionStop},
		nil,
	)
}

func (w *productionPublicationWorkflow) putReviewAttentionWithActionsAndID(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	reason string,
	itemType domain.AttentionType,
	itemID domain.ItemID,
	disputeActions []domain.Action,
	agentClaims []domain.AgentClaim,
) error {
	runID := task.RunID
	actions := []domain.Action{domain.ActionFinishNow}
	if itemType == domain.AttentionReviewDispute {
		actions = disputeActions
	}
	// The first item written at this round's deterministic identity durably
	// binds its routing decision. Classification may recover differently on a
	// later reconciliation, but changing the item's type in place is forbidden
	// and would otherwise strand terminalization behind an immutable-transition
	// error. Reuse the bound decision after verifying its run coordinates.
	var existing *domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		existing = &got
		return nil
	}); err != nil {
		return err
	}
	if existing != nil {
		validType := existing.Type == domain.AttentionReviewDiminishing ||
			existing.Type == domain.AttentionReviewDispute
		validSubject := existing.Subject.Type == domain.SubjectRun &&
			existing.Subject.ID == domain.SubjectID(task.RunID) &&
			existing.Subject.RunID != nil && *existing.Subject.RunID == task.RunID
		if !validType || existing.ProjectID != task.ProjectID || !validSubject ||
			existing.PRHeadSHA != task.HeadSHA {
			return fmt.Errorf("review attention item %q disagrees with run %q: %w",
				itemID, task.RunID, domain.ErrParentKeyMismatch)
		}
		if existing.Type != domain.AttentionReviewDispute {
			return nil
		}
		if agentClaims == nil {
			if existing.Status == domain.StatusOpen &&
				!slices.Equal(existing.RequestedDecision, disputeActions) {
				repaired := *existing
				repaired.RequestedDecision = disputeActions
				repaired.ItemVersion++
				return w.attention.PutItem(ctx, repaired)
			}
			return nil
		}
		createdAt := w.attentionCreatedAt()
		if existing.CreatedAt != nil {
			createdAt = *existing.CreatedAt
		}
		desired, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: itemID, ProjectID: task.ProjectID,
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID},
			Type:    itemType, Priority: domain.PriorityNormal, Reason: reason,
			RequestedDecision: actions, AgentClaims: agentClaims,
			PRHeadSHA: task.HeadSHA, ItemVersion: existing.ItemVersion + 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
			CreatedAt: &createdAt,
		}, w.approvedRecipes)
		if err != nil {
			return err
		}
		claimsMatch := reflect.DeepEqual(existing.AgentClaims, desired.AgentClaims)
		digestsMatch := slices.Equal(existing.ArtifactDigests, desired.ArtifactDigests)
		presentationMatches := slices.Equal(existing.RequestedDecision, desired.RequestedDecision) &&
			existing.Reason == desired.Reason && claimsMatch && digestsMatch
		if !presentationMatches && existing.Status != domain.StatusOpen {
			return fmt.Errorf("review attention item %q presentation disagrees with run %q: %w",
				itemID, task.RunID, domain.ErrParentKeyMismatch)
		}
		// Reconcile an open dispute's context-appropriate controls and bound
		// claims. The routing decision and recovery bindings stay fixed; the
		// rendered decision surface advances together under one new version.
		executableDisputeActions := disputeActions
		if existing.Status == domain.StatusOpen && !presentationMatches {
			repaired := *existing
			repaired.RequestedDecision = executableDisputeActions
			repaired.Reason = desired.Reason
			repaired.AgentClaims = desired.AgentClaims
			repaired.ArtifactDigests = desired.ArtifactDigests
			repaired.ItemVersion++
			return w.attention.PutItem(ctx, repaired)
		}
		return nil
	}
	createdAt := w.attentionCreatedAt()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: task.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID},
		Type:    itemType, Priority: domain.PriorityNormal, Reason: reason,
		RequestedDecision: actions, AgentClaims: agentClaims,
		PRHeadSHA: task.HeadSHA, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
		CreatedAt: &createdAt,
	}, w.approvedRecipes)
	if err != nil {
		return err
	}
	return w.attention.PutItem(ctx, item)
}

// completeReviewEscalationTask terminalizes a run whose §7 review escalated
// before publication: no branch push, no PR, no publication intent. The durable
// AttentionItem written by the gate (PRHeadSHA = task.HeadSHA) is the operator
// surface; the terminal outcome carries no PR number (issue #527, decision 3).
func (w *productionPublicationWorkflow) completeReviewEscalationTask(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
) (productionTaskOutcome, error) {
	accepted, err := w.recordCompletedTerminalAtBoundary(ctx, binding.run, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.finishTask(ctx, task); err != nil {
		return productionTaskOutcome{}, err
	}
	return productionTaskOutcome{
		completed: true, accepted: accepted, blocked: true,
	}, nil
}

// assertReviewedCandidate is the readiness reconstruction boundary: it fails
// closed unless the latest review state is a clean pass bound to this exact
// candidate (base and head), authoritative over any failure round, produced
// under the current, trust-profile-approved reviewer configuration and
// instruction authority. To stay no weaker than the pre-publication gate it
// replaces on recovery, it re-checks the same profile axes that gate enforces
// before accepting a clean record: the Freeside-invoked review mode and the
// trust profile's approval of the current reviewer configuration
// (profile.Review.ConfigDigest == reviewConfigurationDigest). A missing,
// stale, findings, or foreign-candidate record, a non-Freeside review mode, or
// a reviewer configuration the trust profile no longer approves blocks
// readiness rather than deriving it silently, so a run published under the
// retired post-publication order that never recorded such a review is held for
// manual operator disposition instead (issue #527, decision 2; Codex round 4
// closed the profile-approval axis). A fully-ready old-order run, which does
// carry a clean candidate-bound record under an approved configuration,
// re-derives readiness idempotently through this same check.
func productionReadinessResolutions(binding productionBinding, generation uint64) ([]domain.RequirementResolution, error) {
	setDigest, err := domain.ProductionRequirementSetDigest(generation)
	if err != nil {
		return nil, err
	}
	definitions := domain.ProductionRequirementDefinitions()
	resolutions := make([]domain.RequirementResolution, 0, len(definitions))
	for _, definition := range definitions {
		resolution, err := domain.NewRequirementResolution(domain.RequirementResolutionInput{
			RequirementKey: definition.Key, CheckClass: definition.Class,
			Kind: definition.Kind, Applicable: definition.Applicable,
			BaseDependent:           definition.BaseDependent,
			RequirementSetDigest:    setDigest,
			FloorRegistryGeneration: generation,
			ResolvedPolicyDigest:    binding.resolvedPolicy.Digest,
		})
		if err != nil {
			return nil, err
		}
		resolutions = append(resolutions, resolution)
	}
	return resolutions, nil
}

func (w *productionPublicationWorkflow) currentReadinessVerdict(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	reviewInstructions exec.ReviewInstructionBinding,
	reviewRecord domain.ReviewRecord,
) (domain.ReadinessVerdict, func(context.Context) error, error) {
	resolutions, err := productionReadinessResolutions(
		binding, w.store.VerificationFloorRegistryGeneration(),
	)
	if err != nil {
		return domain.ReadinessVerdict{}, nil, err
	}
	// EvaluateReadiness rejects only an empty resolution set, not a strict
	// subset of the requirement set: it trusts its caller to pass the complete
	// set (the general fix, evaluating completeness against the trusted
	// definitions itself, is deferred to #688). This production caller owns
	// that completeness, so assert it here rather than let a future change to
	// productionReadinessResolutions derive readiness by omitting a requirement.
	resolved := make(map[domain.RequirementKey]struct{}, len(resolutions))
	for _, resolution := range resolutions {
		resolved[resolution.RequirementKey] = struct{}{}
	}
	for _, definition := range domain.ProductionRequirementDefinitions() {
		if _, ok := resolved[definition.Key]; !ok {
			return domain.ReadinessVerdict{}, nil, fmt.Errorf(
				"production readiness resolution set omits requirement %q: %w",
				definition.Key, domain.ErrParentKeyMismatch)
		}
	}
	base := binding.admission.Base
	verificationProof, err := domain.NewCheckProof(
		resolutions[0], task.HeadSHA, &base, binding.image.RecipeDigest,
	)
	if err != nil {
		return domain.ReadinessVerdict{}, nil, err
	}
	verificationState, err := domain.NewPassedCheckState(resolutions[0], verificationProof)
	if err != nil {
		return domain.ReadinessVerdict{}, nil, err
	}
	reviewRecipeDigest, err := digestJSON(struct {
		Configuration domain.Digest `json:"configuration"`
		Instructions  domain.Digest `json:"instructions"`
	}{reviewRecord.ConfigurationDigest, reviewInstructions.ResultDigest})
	if err != nil {
		return domain.ReadinessVerdict{}, nil, err
	}
	reviewProof, err := domain.NewCheckProof(resolutions[1], task.HeadSHA, &base, reviewRecipeDigest)
	if err != nil {
		return domain.ReadinessVerdict{}, nil, err
	}
	reviewState, err := domain.NewPassedCheckState(resolutions[1], reviewProof)
	if err != nil {
		return domain.ReadinessVerdict{}, nil, err
	}
	states := []domain.CheckState{verificationState, reviewState}
	target := domain.EvaluationTarget{CandidateHead: task.HeadSHA, Base: &base}
	verdict, err := domain.EvaluateReadiness(target, resolutions, states, nil)
	if err != nil {
		return domain.ReadinessVerdict{}, nil, err
	}
	if checkpoint.Authorization.VerificationOutcome != domain.VerificationPassed ||
		verdict.Class == domain.ReadinessBlocked {
		return domain.ReadinessVerdict{}, nil, fmt.Errorf("current verification set is blocked: %w", domain.ErrParentKeyMismatch)
	}
	// The verdict above is a pure evaluation; persistence is a separate,
	// recipe-gated step returned as a closure so the caller runs it only when
	// it is actually creating new readiness under an approved recipe. Skipping
	// it for an already-ready candidate is what lets that candidate finish
	// after its verification recipe is revoked (issue #527, decision 2), rather
	// than fail on the store's recipe gate.
	persist := func(ctx context.Context) error {
		return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			for _, resolution := range resolutions {
				if err := tx.RecordRequirementResolution(ctx, resolution); err != nil {
					return err
				}
			}
			if err := tx.RecordCheckProof(ctx, verificationProof); err != nil {
				return err
			}
			// assertReviewedCandidate re-derived the run-scoped independent-review
			// authority (profile plus adoption) before reaching this persistence,
			// so assert it to the store; the store fails closed on an
			// independent-review proof it has not been told is authorized.
			tx.AuthorizeIndependentReviewRecipe(reviewRecipeDigest)
			return tx.RecordCheckProof(ctx, reviewProof)
		})
	}
	return verdict, persist, nil
}

func (w *productionPublicationWorkflow) assertReviewedCandidate(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	reviewInstructions exec.ReviewInstructionBinding,
) (domain.ReadinessVerdict, func(context.Context) error, error) {
	if err := w.shadowReviewBlocksReady(ctx, task, binding); err != nil {
		return domain.ReadinessVerdict{}, nil, err
	}
	record, failure, err := w.latestReviewState(ctx, task.RunID)
	if err != nil {
		return domain.ReadinessVerdict{}, nil, err
	}
	approved, err := w.reviewConfigurationApproved(ctx, binding)
	if err != nil {
		return domain.ReadinessVerdict{}, nil, err
	}
	if !approved {
		return domain.ReadinessVerdict{}, nil, reviewConfigurationUnapprovedError(
			binding.profile.Review.ConfigDigest, w.reviewConfigurationDigest,
		)
	}
	if record != nil && record.ConfigurationDigest != w.reviewConfigurationDigest {
		return domain.ReadinessVerdict{}, nil, fmt.Errorf(
			"clean review record configuration is %s: %w",
			record.ConfigurationDigest,
			reviewConfigurationUnapprovedError(
				record.ConfigurationDigest, w.reviewConfigurationDigest,
			),
		)
	}
	roundComplete := false
	if record != nil {
		roundComplete, err = w.reviewRoundDispositionComplete(ctx, *record)
		if err != nil {
			return domain.ReadinessVerdict{}, nil, err
		}
	}
	if binding.profile.Review.Mode != domain.ReviewFreesideInvoked ||
		record == nil ||
		!roundComplete ||
		record.InstructionDigest != reviewInstructions.ResultDigest ||
		record.BaseSHA != binding.admission.Base.BaseSHA ||
		record.HeadSHA != task.HeadSHA ||
		(failure != nil && failure.Round >= record.Round) {
		return domain.ReadinessVerdict{}, nil, fmt.Errorf(
			"published candidate lacks a clean, candidate-bound review record under the current trust-approved reviewer configuration: %w",
			domain.ErrParentKeyMismatch,
		)
	}
	return w.currentReadinessVerdict(ctx, task, binding, checkpoint, reviewInstructions, *record)
}

func (w *productionPublicationWorkflow) completePublishedTask(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
	reviewInstructions exec.ReviewInstructionBinding,
) (productionTaskOutcome, error) {
	// Readiness reconstruction boundary: a published candidate reaches readiness
	// only through a clean, candidate-bound review record under the current
	// reviewer configuration, failing closed otherwise (issue #527, decision 2).
	// The §7 review gate itself runs before publication in reconcileTask; this
	// re-gate keeps every crash-recovery call site correct, including an
	// old-order run that published before recording a clean, candidate-bound
	// review.
	verdict, persistReadiness, err := w.assertReviewedCandidate(ctx, task, binding, checkpoint, reviewInstructions)
	if err != nil {
		// Decision 2 promises operator-visible disposition, not a lane-fatal
		// error. The re-gate stays fail-closed (never silent readiness); only
		// its failure handling is task-scoped here. On a fail-closed mismatch
		// (ErrParentKeyMismatch or ErrReviewConfigurationUnapproved) this holds
		// the one run with an operator-visible
		// item, exactly as the sibling recipe-approval re-gate below does, so
		// the reconcile lane keeps advancing every other queued publication and
		// the run recovers if the reviewer configuration is restored (an
		// old-order run with no record instead stays held until the operator
		// dispositions it). A store read failure from latestReviewState is
		// environmental and still propagates for retry. The already-published
		// PR carries no binding on this held item, matching the recipe re-gate's
		// pre-existing behavior across axes (tracked as a follow-up, not #527).
		if errors.Is(err, errShadowReviewStopped) {
			return productionTaskOutcome{}, fmt.Errorf(
				"shadow review stopped after publication completed: %w",
				domain.ErrParentKeyMismatch,
			)
		}
		if errors.Is(err, errShadowReviewBlocksReady) {
			return productionTaskOutcome{}, nil
		}
		if errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
			return w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
		}
		if errors.Is(err, domain.ErrParentKeyMismatch) {
			return w.holdBlockedTask(
				ctx, task, checkpoint.Imported,
				"Publication is durably held because this published run lacks a clean, candidate-bound review record under the current trust-approved reviewer configuration. Restore the approved reviewer configuration or disposition the run manually.",
				domain.HoldTrustBlocked,
			)
		}
		return productionTaskOutcome{}, err
	}
	yieldHistory, err := w.reviewYieldHistory(ctx, task.RunID)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	readyExists, err := w.hasCompatibleReadyItem(
		ctx, task, binding, checkpoint, published, verdict, yieldHistory,
	)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if !readyExists {
		// Creating new readiness requires a still-approved recipe: persisting the
		// clean-verification proof re-runs the store's recipe gate. A recipe
		// revoked after publication takes the durable recipe-revoked hold here
		// rather than failing the persistence with a lane-fatal error, so the
		// proof write never runs under a revoked recipe. An already durably-ready
		// candidate skips this block and finishes below without re-deriving its
		// proofs, which is what lets it complete after its recipe is revoked
		// (issue #527, decision 2); the verdict it reports was evaluated in
		// memory above, independent of the recipe gate.
		if !w.approvedRecipes[binding.image.RecipeDigest] {
			return w.holdBlockedTask(
				ctx, task, checkpoint.Imported,
				"Publication is durably held because current trust no longer approves the verification recipe that authorized the candidate. Restore that approval before creating readiness.",
				domain.HoldRecipeRevoked,
			)
		}
		if err := persistReadiness(ctx); err != nil {
			return productionTaskOutcome{}, err
		}
		ready, err := w.readyItem(task, checkpoint, published, verdict, yieldHistory)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if err := runDurableTransitionHook(w.transitionHook,
			DurableTransitionReadyItem, DurableTransitionBefore); err != nil {
			return productionTaskOutcome{}, err
		}
		if err := w.attention.PutItem(ctx, ready); err != nil {
			return productionTaskOutcome{}, err
		}
		if err := runDurableTransitionHook(w.transitionHook,
			DurableTransitionReadyItem, DurableTransitionAfter); err != nil {
			return productionTaskOutcome{}, err
		}
	}
	// Every ready item receives a daemon-internal PR binding, whether or not
	// the run carries an optional §5.18 work-unit declaration. The plain-ticker
	// active-resource reconciler owns merge/close observation and must be able
	// to resume from this exact identity after a restart.
	if err := w.recordReadyItemPRBinding(ctx, task, binding, published); err != nil {
		return productionTaskOutcome{}, err
	}
	// The §5.18 work-unit PR binding converges on every pass through the
	// published state, write-once from the same first-party facts. Both
	// bindings commit before the durable watches arm, so a startup
	// reconciliation pass never sees an active item with an ambiguous PR.
	if err := w.recordWorkUnitPRBinding(ctx, task, binding, published); err != nil {
		return productionTaskOutcome{}, err
	}
	// The §5.16 publication watches converge beside the item on every pass
	// through here, so a crash between the item and its watches heals.
	if err := w.armReadyItemWatches(ctx, task, binding); err != nil {
		return productionTaskOutcome{}, err
	}
	// The ready milestone converges once the ready item durably exists; a
	// crash between the two re-reaches this point through the readyExists
	// branch and the first-observation-wins append (issue #394).
	if err := w.appendPublicationMilestone(ctx, task, domain.MilestonePublicationReady, nil); err != nil {
		return productionTaskOutcome{}, err
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
	accepted, err := w.recordCompletedTerminalAtBoundary(ctx, binding.run, task)
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
		completed: true, accepted: accepted, readiness: &verdict, prNumber: published.PRNumber,
	}, nil
}

func (w *productionPublicationWorkflow) holdReviewConfigurationMismatch(
	ctx context.Context,
	task productionPublicationTask,
	imported importer.Result,
	err error,
) (productionTaskOutcome, error) {
	return w.holdBlockedTask(
		ctx, task, imported,
		fmt.Sprintf(
			"Publication is durably held because the reviewer configuration lacks approval under the current trust-approved reviewer configuration: %v. Restore the approved reviewer configuration or disposition the run manually.",
			err,
		),
		domain.HoldTrustBlocked,
	)
}

func (w *productionPublicationWorkflow) hasCompatibleReadyItem(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
	verdict domain.ReadinessVerdict,
	yieldHistory domain.ReviewYieldHistory,
) (bool, error) {
	historicalRecipes := mapsClone(w.approvedRecipes)
	historicalRecipes[binding.image.RecipeDigest] = true
	expectedReady, err := w.readyItemWithRecipes(
		task, checkpoint, published, verdict, yieldHistory, historicalRecipes,
	)
	if err != nil {
		return false, err
	}
	var expectedRedacted domain.AttentionItem
	if !w.approvedRecipes[binding.image.RecipeDigest] {
		redactedCheckpoint := checkpoint
		redactedCheckpoint.Artifacts = nil
		expectedRedacted, err = w.readyItemWithRecipes(
			task, redactedCheckpoint, published, verdict, yieldHistory, historicalRecipes,
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
	cause domain.RunHoldReason,
) (productionTaskOutcome, error) {
	// The durable hold's typed cause is stated by each call site, never
	// derived from the operator prose; the retry window paces the write
	// (issue #394).
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordRunHold(ctx, tx, task.RunID, task.PublicationID, cause, w.now().UTC())
	}); err != nil {
		return productionTaskOutcome{}, err
	}
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
		item.CreatedAt = current.CreatedAt
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

// recordAttendedPublicationHolds records the attended_mode_active hold for
// every queued publication task the hold-only composition is pausing, paced
// the same way as the engine's dispatch-side holds. A malformed task key
// records nothing here: the unquarantined-row handling belongs to the
// active composition, and the projection never widens its authority.
func (w *productionPublicationWorkflow) recordAttendedPublicationHolds(ctx context.Context) error {
	var pending []store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, KindProductionPublicationRequested)
		return err
	}); err != nil {
		return err
	}
	for _, entry := range pending {
		runID, ok := productionRunIDFromPublicationTaskKey(entry.IdempotencyKey)
		if !ok {
			continue
		}
		now := w.now().UTC()
		if !w.holdPace.due("hold:"+string(runID), string(domain.HoldAttendedModeActive), now) {
			continue
		}
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return recordRunHold(ctx, tx, runID,
				productionPublicationInvocationID(runID),
				domain.HoldAttendedModeActive, now)
		}); err != nil {
			w.holdPace.forget("hold:" + string(runID))
			return err
		}
	}
	return nil
}

// appendPublicationMilestone records one publication-lane milestone for the
// task's run, keyed to the publication invocation. Projection only: it rides
// its own bookkeeping transaction because the underlying facts (attention
// items) commit through signet, and first-observation-wins convergence
// covers the crash window between the two.
func (w *productionPublicationWorkflow) appendPublicationMilestone(
	ctx context.Context, task productionPublicationTask,
	kind domain.RunMilestoneKind, cause *domain.RunHoldReason,
) error {
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		invocation := task.PublicationID
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: task.RunID, Kind: kind,
			InvocationID: &invocation, Reason: cause,
			RecordedAt: w.now().UTC(),
		})
	})
}

// recordWorkUnitPRBinding captures the §5.18 exact work-unit binding once
// the run's PR durably exists: every coordinate is a first-party fact (the
// admitted base revision, the publish result's PR number, the publication
// task's head). An undeclared run records nothing. The record is
// write-once; a converged re-pass must restate the same coordinates
// (compared modulo the stamped instant), and a disagreement fails loud —
// publication converges on exactly one PR, so a second binding is
// corruption, never an update.
func (w *productionPublicationWorkflow) recordWorkUnitPRBinding(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	published publish.Result,
) error {
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, task.RunID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		record := domain.WorkUnitPRBinding{
			UnitID:       declaration.ID,
			Repo:         binding.admission.Base.Repo,
			RepositoryID: binding.admission.Base.RepositoryID,
			PRNumber:     published.PRNumber,
			BaseRef:      binding.admission.Base.BaseRef,
			HeadSHA:      task.HeadSHA,
			RecordedAt:   w.now().UTC(),
		}
		existing, err := tx.GetWorkUnitPRBinding(ctx, declaration.ID)
		switch {
		case err == nil:
			want := record
			want.RecordedAt = existing.RecordedAt
			if want != existing {
				return fmt.Errorf("stored work-unit pr binding disagrees with the published state: %w",
					store.ErrImmutableConflict)
			}
			return nil
		case errors.Is(err, store.ErrNotFound):
			return tx.RecordWorkUnitPRBinding(ctx, record)
		default:
			return err
		}
	})
}

func (w *productionPublicationWorkflow) recordReadyItemPRBinding(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	published publish.Result,
) error {
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		record := domain.ReadyItemPRBinding{
			ItemID:                  productionReadyItemID(task.RunID),
			RunID:                   task.RunID,
			ProducingInvocationID:   task.ProducingInvocationID,
			PublicationInvocationID: task.PublicationID,
			PublicationIdentity:     published.Identity.Digest(),
			Repo:                    binding.admission.Base.Repo,
			RepositoryID:            binding.admission.Base.RepositoryID,
			PRNumber:                published.PRNumber,
			BaseRef:                 binding.admission.Base.BaseRef,
			HeadSHA:                 task.HeadSHA,
			RecordedAt:              w.now().UTC(),
		}
		existing, err := tx.GetReadyItemPRBinding(ctx, record.ItemID)
		switch {
		case err == nil:
			want := record
			want.RecordedAt = existing.RecordedAt
			if want != existing {
				return fmt.Errorf("stored ready-item pr binding disagrees with the published state: %w",
					store.ErrImmutableConflict)
			}
			return nil
		case errors.Is(err, store.ErrNotFound):
			return tx.RecordReadyItemPRBinding(ctx, record)
		default:
			return err
		}
	})
}

func (w *productionPublicationWorkflow) deferHeldTask(task productionPublicationTask) {
	w.holdRetryAfter[task.RunID] = w.now().Add(w.holdRetryInterval)
}

// recordPublicationEnvironmentHold states the typed cause behind every
// environmental pause of one task: a work directory the daemon cannot
// prepare, a lock it cannot take, or a retryable reconcile failure (issue
// #394). The bounded retry window paces the write. A cancelled context is the
// daemon stopping rather than a cause an operator can act on, so it records
// nothing; any other write failure is a store fault the caller joins loud.
func (w *productionPublicationWorkflow) recordPublicationEnvironmentHold(
	ctx context.Context, task productionPublicationTask,
) error {
	if ctx.Err() != nil {
		return nil
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordRunHold(ctx, tx, task.RunID, task.PublicationID,
			domain.HoldPublicationEnvironment, w.now().UTC())
	})
}

// publicationAttemptPaceState paces the clear below on the hold key without
// ever naming a hold reason: it is the pacer's record that this run's
// attended-mode hold was already cleared, not an observation of a hold.
const publicationAttemptPaceState = "attempt-accepted"

// clearAttendedPublicationHold ends the hold-only composition's pause once an
// active composition accepts the queued task. Cause-scoped, so a hold this
// attempt is about to re-record keeps its row and its span, and paced like
// the hold writes so the reconcile cadence does not turn an already-cleared
// run into a delete per pass.
func (w *productionPublicationWorkflow) clearAttendedPublicationHold(
	ctx context.Context, task productionPublicationTask,
) error {
	key := "hold:" + string(task.RunID)
	if !w.holdPace.due(key, publicationAttemptPaceState, w.now().UTC()) {
		return nil
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ClearRunHoldCause(ctx, task.RunID, domain.HoldAttendedModeActive)
	}); err != nil {
		w.holdPace.forget(key)
		return err
	}
	return nil
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
	verdict domain.ReadinessVerdict,
) (publish.Result, error) {
	yieldHistory, err := w.reviewYieldHistory(ctx, task.RunID)
	if err != nil {
		return publish.Result{}, err
	}
	published, found, err := w.loadPublicationOutcome(ctx, task, candidate, func(
		ctx context.Context,
		candidate publish.Candidate,
		identity publish.Identity,
		outcome publish.Outcome,
	) error {
		published := publish.Result{
			Identity: identity, Branch: outcome.Branch, PRNumber: outcome.PRNumber,
		}
		compatible, err := w.hasCompatibleReadyItem(
			ctx, task, binding, checkpoint, published, verdict, yieldHistory,
		)
		if err != nil {
			return err
		}
		if !compatible {
			return fmt.Errorf("ready item disappeared during finalized recovery: %w", store.ErrNotFound)
		}
		return w.convergePublicationOutcome(ctx, candidate, identity, outcome)
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

func (w *productionPublicationWorkflow) convergePublicationOutcome(
	ctx context.Context,
	candidate publish.Candidate,
	identity publish.Identity,
	outcome publish.Outcome,
) error {
	return w.publisher.ConvergeOutcome(
		ctx, candidate, w.approvedRecipes, identity, outcome,
	)
}

func (w *productionPublicationWorkflow) holdPublicationRepairRefusal(
	ctx context.Context,
	task productionPublicationTask,
	imported importer.Result,
	err error,
) (productionTaskOutcome, bool, error) {
	var reason string
	var cause domain.RunHoldReason
	switch {
	case errors.Is(err, domain.ErrUnapprovedRecipe):
		reason = productionBlockRepairRecipeRevoked
		cause = domain.HoldRecipeRevoked
	case errors.Is(err, publish.ErrTrustProfileDrift),
		errors.Is(err, publish.ErrUnauthorizedPublication):
		reason = productionBlockRepairTrustRefused
		cause = domain.HoldTrustBlocked
	default:
		return productionTaskOutcome{}, false, nil
	}
	held, holdErr := w.holdBlockedTask(ctx, task, imported, reason, cause)
	return held, true, holdErr
}

func (w *productionPublicationWorkflow) armReadyItemWatches(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) error {
	var item domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(ctx, productionReadyItemID(task.RunID))
		return err
	}); err != nil {
		return fmt.Errorf("arm publication watches: %w", err)
	}
	return armPublicationWatches(ctx, w.store, item,
		binding.admission.Base.Repo, binding.admission.Base.BaseRef,
		binding.admission.Base.BaseSHA, w.now())
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
	intent, err := publish.DecodeStoredIntent(entry)
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
	if err := publish.ValidateIntentDispositionHistory(intent, candidate); err != nil {
		return publish.Result{}, false, fmt.Errorf(
			"production publication intent disposition history: %w", err,
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
		if productionPublicationRetryableFailure(err) || isDurablePublicationConflict(err) ||
			errors.Is(err, domain.ErrUnapprovedRecipe) ||
			errors.Is(err, publish.ErrTrustProfileDrift) ||
			errors.Is(err, publish.ErrUnauthorizedPublication) {
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

func (w *productionPublicationWorkflow) hasFinalizedPublicationIntent(
	ctx context.Context,
	invocationID domain.InvocationID,
) (bool, error) {
	key, err := publish.IntentKey(invocationID, publish.IntentKindPublication)
	if err != nil {
		return false, err
	}
	var entry store.QueueEntry
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, key)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return entry.Kind == publish.IntentKindPublication && entry.Dispatched(), nil
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
	return domain.Digest(contentaddr.Format(sum[:]))
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
		TaskKey: productionPublicationTaskKey(task.RunID), HeadSHA: task.HeadSHA,
		ProjectImage: binding.image.ID,
		Imported:     imported, Authorization: authorization, Artifacts: artifacts,
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionVerificationEvidence, DurableTransitionBefore); err != nil {
		return productionVerificationCheckpoint{}, err
	}
	if err := w.persistCheckpoint(ctx, task, checkpoint); err != nil {
		return productionVerificationCheckpoint{}, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionVerificationEvidence, DurableTransitionAfter); err != nil {
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
			ctx, productionVerificationCheckpointKey(task.RunID, task.HeadSHA),
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
	var (
		entry  store.QueueEntry
		legacy bool
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetInbox(ctx, productionVerificationCheckpointKey(task.RunID, task.HeadSHA))
		if errors.Is(err, store.ErrNotFound) &&
			task.ProducingInvocationID == productionInvocationID(task.RunID) {
			entry, err = tx.GetInbox(ctx, "production-verification/"+string(task.RunID))
			legacy = err == nil
		}
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
	var checkpoint productionVerificationCheckpoint
	if err := strictjson.Decode(
		entry.Payload, &checkpoint, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return productionVerificationCheckpoint{}, false, fmt.Errorf(
				"production verification checkpoint has trailing content: %w",
				domain.ErrParentKeyMismatch,
			)
		}
		return productionVerificationCheckpoint{}, false, fmt.Errorf(
			"decode durable production verification checkpoint: %w",
			errors.Join(err, domain.ErrParentKeyMismatch),
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
	versionMatches := !legacy && checkpoint.Version == productionVerificationVersion &&
		checkpoint.HeadSHA == task.HeadSHA
	if legacy {
		versionMatches = checkpoint.Version == productionVerificationVersionV1 && checkpoint.HeadSHA == ""
	}
	if !versionMatches ||
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
			// A transient blob fault stays retryable; only a digest mismatch or
			// an absent blob is a durable checkpoint contradiction. Forcing every
			// fault terminal would stop a valid run on a transient I/O blip.
			return productionVerificationCheckpoint{}, false, fmt.Errorf(
				"verify durable production checkpoint artifact: %w",
				retryableOrTerminal(ctx, err, domain.ErrParentKeyMismatch),
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

// loadRemediationSourceTree reconstructs the prior reviewed candidate's tree
// identity from the daemon-authored verification checkpoint. Commit identity
// is deliberately insufficient here: every stage invocation receives a fresh
// commit date, so identical content normally reconstructs a different commit.
func (w *productionPublicationWorkflow) loadRemediationSourceTree(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
) (string, error) {
	if binding.remediation == nil {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	request := binding.remediation.request
	round, ok := remediationRoundForInvocation(task.RunID, task.ProducingInvocationID)
	if !ok || request.Round != round {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	var (
		entry  store.QueueEntry
		legacy bool
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetInbox(ctx, productionVerificationCheckpointKey(task.RunID, request.HeadSHA))
		if errors.Is(err, store.ErrNotFound) {
			entry, err = tx.GetInbox(ctx, "production-verification/"+string(task.RunID))
			legacy = err == nil
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	if err != nil {
		return "", remediationSourceReadError(ctx, err)
	}
	if entry.Kind != productionVerificationCheckpointKind {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	var checkpoint productionVerificationCheckpoint
	if err := strictjson.Decode(
		entry.Payload, &checkpoint, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return "", fmt.Errorf("decode remediation source checkpoint: %w",
			errors.Join(errRemediationSourceIdentity, err, domain.ErrParentKeyMismatch))
	}
	importDigest, err := digestJSON(checkpoint.Imported)
	if err != nil {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(checkpoint.Artifacts)
	if err != nil {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	authorization := checkpoint.Authorization
	var (
		storedAuthorization domain.CandidateAuthorization
		sourceImage         domain.ProjectImage
	)
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		storedAuthorization, err = tx.GetCandidateAuthorization(ctx, authorization.ID)
		if err != nil {
			return err
		}
		sourceImage, err = tx.GetProjectImage(ctx, checkpoint.ProjectImage)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	if err != nil {
		return "", remediationSourceReadError(ctx, err)
	}
	versionMatches := !legacy && checkpoint.Version == productionVerificationVersion &&
		checkpoint.HeadSHA == request.HeadSHA
	if legacy {
		versionMatches = checkpoint.Version == productionVerificationVersionV1 && checkpoint.HeadSHA == ""
	}
	if !versionMatches || checkpoint.TaskKey != productionPublicationTaskKey(task.RunID) ||
		checkpoint.Imported.CommitSHA != request.HeadSHA ||
		!validCommitSHA(checkpoint.Imported.TreeSHA) || len(checkpoint.Imported.Findings) != 0 ||
		authorization.Validate() != nil ||
		authorization.Repo != binding.admission.Base.Repo ||
		authorization.BaseSHA != request.BaseSHA || authorization.HeadSHA != request.HeadSHA ||
		authorization.ImportResultDigest != importDigest ||
		authorization.EvidenceSnapshotDigest != evidenceDigest ||
		authorization.VerificationOutcome != domain.VerificationPassed ||
		!validRemediationSourceVerificationID(task.RunID, round, authorization.InvocationID) ||
		!reflect.DeepEqual(storedAuthorization, authorization) ||
		sourceImage.ID != checkpoint.ProjectImage || sourceImage.Repository != authorization.Repo ||
		sourceImage.RepositoryID != binding.admission.Base.RepositoryID ||
		sourceImage.RecipeDigest != authorization.VerificationRecipeDigest {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	for _, artifact := range checkpoint.Artifacts {
		if err := verifyFakePublicationBlob(w.artifacts, artifact); err != nil {
			// A transient open/read/close fault on a checkpoint blob is
			// operational and must stay retryable; only a digest mismatch or an
			// absent blob is a durable source-identity contradiction. Classify
			// like the adjacent checkpoint/authorization/image reads above.
			return "", fmt.Errorf("verify remediation source checkpoint artifact: %w",
				retryableOrTerminal(ctx, err, errRemediationSourceIdentity))
		}
	}
	return checkpoint.Imported.TreeSHA, nil
}

// retryableOrTerminal keeps a transient open/read/close fault on a
// daemon-authored checkpoint blob retryable, while a deterministic contradiction
// (a digest mismatch, or an absent blob the store reports non-operationally) is
// terminalized with the caller's durable sentinel. Context cancellation stays
// the context error. Callers must not force every blob fault to a permanent
// dispute: a transient I/O blip would otherwise stop an otherwise valid run.
func retryableOrTerminal(ctx context.Context, err error, sentinel error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if productionPublicationRetryableFailure(err) {
		return err
	}
	return errors.Join(sentinel, err)
}

func remediationSourceReadError(ctx context.Context, err error) error {
	return retryableOrTerminal(ctx, err, errRemediationSourceIdentity)
}

func validRemediationSourceVerificationID(
	runID domain.RunID,
	currentRound int,
	invocationID domain.InvocationID,
) bool {
	if invocationID == productionVerificationInvocationID(runID) {
		return true
	}
	for round := 1; round < currentRound; round++ {
		if invocationID == productionVerificationInvocationIDForProducer(
			runID, remediationInvocationID(runID, round),
		) {
			return true
		}
	}
	return false
}

func productionCandidate(
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	adoptedProfile *domain.Digest,
	dispositionHistory *publish.DispositionHistory,
) publish.Candidate {
	recipe := binding.image.RecipeDigest
	authorization := checkpoint.Authorization.ID
	profile := binding.profile.ProfileDigest
	return publish.Candidate{
		Repo: binding.admission.Base.Repo, BaseRef: binding.admission.Base.BaseRef,
		HeadSHA: task.HeadSHA, Title: task.Publication.Title,
		Body: task.Publication.Body, DispositionHistory: dispositionHistory,
		Artifacts: checkpoint.Artifacts, RecipeDigest: &recipe,
		InvocationID: task.PublicationID, RunID: task.RunID,
		AuthorizationID: &authorization, TrustProfileDigest: &profile,
		AdoptedTrustProfileDigest: adoptedProfile,
	}
}

// adoptedReviewProfileDigest returns the profile revision an effective
// review-configuration adoption rebinds this run to, nil when none is
// effective. The publish drift gate re-derives the claim from the trust
// source before honoring it.
func (w *productionPublicationWorkflow) adoptedReviewProfileDigest(
	ctx context.Context, binding productionBinding,
) (*domain.Digest, error) {
	transition, adopted, err := w.reviewConfigurationAdoption(ctx, binding)
	if err != nil || !adopted {
		return nil, err
	}
	digest := transition.SupersedingProfileDigest
	return &digest, nil
}

func (w *productionPublicationWorkflow) readyItem(
	task productionPublicationTask,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
	verdict domain.ReadinessVerdict,
	yieldHistory domain.ReviewYieldHistory,
) (domain.AttentionItem, error) {
	return w.readyItemWithRecipes(
		task, checkpoint, published, verdict, yieldHistory, w.approvedRecipes,
	)
}

func (w *productionPublicationWorkflow) readyItemWithRecipes(
	task productionPublicationTask,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
	verdict domain.ReadinessVerdict,
	yieldHistory domain.ReviewYieldHistory,
	approvedRecipes map[domain.Digest]bool,
) (domain.AttentionItem, error) {
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: productionReadyItemID(task.RunID), ProjectID: task.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: fmt.Sprintf("Published %s#%d and completed production verification.",
			checkpoint.Authorization.Repo, published.PRNumber),
		RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionMarkSeen, domain.ActionDismiss, domain.ActionStop,
		},
		EvidenceSnapshot: checkpoint.Artifacts, AgentClaims: checkpoint.Imported.Claims,
		PRHeadSHA: checkpoint.Imported.CommitSHA,
		PRReference: &domain.PRReference{
			Repo: checkpoint.Authorization.Repo, Number: published.PRNumber,
		},
		Readiness: &domain.ReadinessSummary{
			Class: verdict.Class, EvaluationSetDigest: verdict.EvaluationSetDigest,
		},
		YieldHistory:     &yieldHistory,
		CommitPlanNotice: checkpoint.Imported.CommitPlanNotice,
		ItemVersion:      1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &createdAt,
		Status:    domain.StatusOpen,
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
	createdAt := w.attentionCreatedAt()
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
		CreatedAt: &createdAt,
		Status:    domain.StatusOpen,
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
	// The recovered definitive block projects the same milestone the live
	// path records; current.Reason already passed the closed-set gate above
	// (issue #394).
	if cause, ok := productionBlockReason(current.Reason); ok {
		if err := w.appendPublicationMilestone(
			ctx, task, domain.MilestonePublicationBlocked, &cause); err != nil {
			return nil, err
		}
	}
	accepted, err := w.recordCompletedTerminalAtBoundary(ctx, binding.run, task)
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
	// The definitive reasons are a closed set; an unmapped one is the same
	// contract violation recovery fails on. Only the code crosses into the
	// projection (issue #394).
	cause, ok := productionBlockReason(reason)
	if !ok {
		return productionTaskOutcome{}, fmt.Errorf(
			"production blocked reason %q has no observation code: %w",
			reason, domain.ErrInvalidRunHoldReason)
	}
	if err := w.appendPublicationMilestone(ctx, task, domain.MilestonePublicationBlocked, &cause); err != nil {
		return productionTaskOutcome{}, err
	}
	if w.afterBlocked != nil {
		if err := w.afterBlocked(); err != nil {
			return productionTaskOutcome{}, fmt.Errorf("after production blocked item: %w",
				errors.Join(err, errProductionCrashSeam))
		}
	}
	accepted, err := w.recordCompletedTerminalAtBoundary(ctx, run, task)
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
		item.CreatedAt = current.CreatedAt
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
	stage, ok := productionStageForInvocation(run, task.ProducingInvocationID)
	if !ok {
		return false, domain.ErrParentKeyMismatch
	}
	return (&Engine{store: w.store}).recordProductionTerminalWithAuthority(ctx, run, productionTerminalRecord{
		InvocationID: task.ProducingInvocationID, RunID: task.RunID,
		StageID: stage.ID, Status: exec.StatusCompleted,
		HeadSHA: task.HeadSHA, Artifacts: slices.Clone(task.Artifacts), Summary: task.Summary,
	}, false)
}

func (w *productionPublicationWorkflow) recordCompletedTerminalAtBoundary(
	ctx context.Context,
	run domain.Run,
	task productionPublicationTask,
) (bool, error) {
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionTerminalCompletion, DurableTransitionBefore); err != nil {
		return false, err
	}
	accepted, err := w.recordCompletedTerminal(ctx, run, task)
	if err != nil {
		return false, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionTerminalCompletion, DurableTransitionAfter); err != nil {
		return false, err
	}
	return accepted, nil
}

func (w *productionPublicationWorkflow) finishTask(
	ctx context.Context, task productionPublicationTask,
) error {
	return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, productionPublicationTaskKey(task.RunID))
	})
}
