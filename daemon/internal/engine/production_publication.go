package engine

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
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
	productionBlockHandoffConflict       = domain.PublicationBlockHandoffConflict
	productionBlockHandoffVerification   = domain.PublicationBlockHandoffVerification
	productionBlockReentryRetired        = domain.PublicationBlockReentryRetired
	productionBlockReentryConflict       = domain.PublicationBlockBaseAdvancedConflict
	productionBlockReentryVerification   = domain.PublicationBlockBaseAdvancedVerification
	productionBlockReentryRoundLimit     = domain.PublicationBlockBaseAdvancedRoundLimit
	productionBlockMemoryLimit           = domain.PublicationBlockVerificationMemoryLimit
	productionBlockExternal              = "Publication is durably held because the external service permanently refused the committed operation. Repair that external state to resume recovery."
	productionBlockConflict              = "Publication is durably held because the external branch or pull request conflicts with the committed identity. Inspect and repair that external state to resume recovery."
	productionBlockSuccessorPRNotListed  = "Publication is held because GitHub still lists no pull request on the successor branch after repeated checks. Confirm the pull request exists and is open on that branch; publication resumes once GitHub shows it."
	productionBlockSuccessorHeadLagging  = "Publication is held because GitHub still shows the pull request at its previous head after repeated checks, although the branch is at the pushed head. Confirm the pull request tracks that branch; publication resumes once GitHub shows the pushed head."
	// successorObservationPendingTolerance is how many consecutive attempts
	// of one successor may see GitHub's PR view lagging its branch before
	// the operator is asked to look (#1544). At the default 30-second
	// holdRetryInterval the fourth attempt, about 90 seconds in, raises the
	// card: long past the moment GitHub normally needs, short enough that a
	// PR that is really missing is not hidden for long.
	successorObservationPendingTolerance = 3
	// closedPublicationUnsettledTolerance is how many unsettled observations
	// of a closed run's publication, with no settle between them, record its
	// external_conflict hold (#2343). The third comes about 90 seconds in at
	// the default holdRetryInterval, past a pull request GitHub has not
	// listed yet.
	closedPublicationUnsettledTolerance = 3
	// closedPublicationMaxBackoffShift caps the closed-run observation's
	// doubling at 32 times holdRetryInterval, 16 minutes at the default. An
	// intent nothing can settle then costs a few GitHub calls an hour instead
	// of one per lane pass, about 88 a minute in #2343.
	closedPublicationMaxBackoffShift = 5
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
	// Usage is the driver's extracted telemetry, appended in the same write
	// that records the export. It is not replay data, so the publication
	// task never persists it and a decoded task replay carries none.
	Usage []exec.UsageMeasurement `json:"-"`
	// ClaudeUsage is the writer turn's Claude usage capture, which
	// RecordExecutionExport hands to the sink after a committed production
	// write, because that export is never collected. Like Usage it is driver
	// data, not replay data: the publication task never persists it, so a
	// decoded task replay carries none and the publication lane reports
	// nothing.
	ClaudeUsage *exec.ClaudeUsageCapture `json:"-"`
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
	// RepositoryID is the daemon's one repository. The recipes image-policy
	// adoptions approved for it join ApprovedRecipes wherever the workflow
	// gates verification or evidence. Zero means no adoption applies.
	RepositoryID int64
	NewRoom      func(domain.ProjectImage, ward.ContainerSize) (ProductionVerificationRoom, error)
	// NewBoundRoom supplies concrete task-bound ownership for production.
	// NewRoom remains the isolated test/attended factory.
	NewBoundRoom func(domain.ProjectImage, ward.ContainerSize, domain.Run, domain.InvocationID) (ProductionVerificationRoom, error)
	// RebuildProjectImage lets the lane rebuild the project image for a
	// candidate whose dependency change stays within policy (plan §5.7). Nil
	// leaves every such candidate refused as rebuild_not_configured.
	RebuildProjectImage *ProjectImageRebuild
	// ImageUpkeepPending is image upkeep's pending-resolution lookup
	// (ImageUpkeep.PendingResolution). A re-entry whose base no recorded
	// image serves holds while it reports a resolution and stops as before
	// when it reports none. Nil reports none.
	ImageUpkeepPending func(context.Context) (*domain.ImageUpkeepHold, error)
	ReviewSource       exec.ReviewSource
	// ReviewAdmission admits a routed or shadow review invocation before its
	// source is asked (plan §5.4). The workflow stores the snapshot it returns
	// once and puts that stored snapshot on every request for the invocation.
	ReviewAdmission ReviewAdmitter
	// RemediationPromptPackageDigest selects the trusted prompt package for
	// implementation-role follow-up invocations created by finding
	// adjudication or accepted operator feedback.
	RemediationPromptPackageDigest domain.Digest
	// ShadowReviewSource is optional. When present, every other shadow field
	// is required and the source remains observation-only: it cannot satisfy
	// routed review, advance rounds, or authorize publication.
	ShadowReviewSource              exec.ReviewSource
	ShadowReviewConfigurationDigest domain.Digest
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
	HoldOnly          bool
	RecipeReadTimeout time.Duration
	HoldRetryInterval time.Duration
	Now               func() time.Time
	// PreparationDigest is the running binary's toolchain and preparation
	// digest, which a project image reused at another commit must have been
	// built with. Empty selects projectimage.PreparationDigest(); a test sets
	// another value to stand for a daemon whose builder sources changed.
	PreparationDigest    domain.Digest
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
		if e.artifacts == nil {
			e.artifacts = cfg.Artifacts
		}
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
	LegacyRemediationNoop bool                         `json:"remediation_noop,omitempty"`
	VerificationID        domain.InvocationID          `json:"verification_invocation_id"`
	PublicationID         domain.InvocationID          `json:"publication_invocation_id"`
	HeadSHA               string                       `json:"head_sha"`
	Artifacts             []domain.Digest              `json:"artifacts,omitempty"`
	Replay                ProductionReplay             `json:"replay"`
	Publication           ProductionPublication        `json:"publication"`
	Summary               string                       `json:"summary,omitempty"`
	Successor             *domain.PublicationSuccessor `json:"successor,omitempty"`
	// AuthorizingTaskKey names the task whose verification checkpoint holds
	// this task's candidate authorization. Only a legacy task that re-enters
	// before publication sets it (reentersBeforePublication). Such tasks are
	// no longer created (issue #2644); the field stays so a queued one still
	// decodes and is retired.
	AuthorizingTaskKey string `json:"authorizing_task_key,omitempty"`
	// reevaluation is reconstructed from a signet intent. It is never part of
	// the original publication task row, whose bytes remain immutable.
	reevaluation *productionReevaluation               `json:"-"`
	continuation *domain.PublicationContinuationIntent `json:"-"`
	// scopeDecision is re-derived from the accepted command on each replay.
	scopeDecision *domain.ScopeDecisionFacts `json:"-"`
}

type productionReevaluation struct {
	CommandID          string
	ItemVersion        int
	IntentKey          string
	TrustProfileDigest domain.Digest
	ReviewRound        int
}

type productionVerificationCheckpoint struct {
	Version       string                        `json:"version"`
	TaskKey       string                        `json:"task_key"`
	HeadSHA       string                        `json:"head_sha"`
	ProjectImage  domain.Digest                 `json:"project_image"`
	Imported      importer.Result               `json:"imported"`
	Authorization domain.CandidateAuthorization `json:"authorization"`
	Artifacts     []domain.Artifact             `json:"artifacts"`
	DiffStats     *domain.DiffStats             `json:"diff_stats"`
}

type productionPublicationWorkflow struct {
	// launchBound is the engine's startup memory bound; review admissions and
	// verification jobs reserve against the budget in force, launchBoundAt.
	launchBound ward.LaunchBound
	logger      *slog.Logger
	store       *store.Store
	attention   attentionService
	workDir     string
	transport   PublicationTransport
	publisher   *publish.Publisher
	artifacts   ArtifactStore
	// operatorRecipes is the operator's approved-recipe set. Wherever an
	// image's recipe or recipe-bearing evidence is gated, read the set
	// through recipes or recipesIn, which add the repository's adopted
	// recipes; an item or artifact with no such evidence may pass it as is.
	operatorRecipes                 map[domain.Digest]bool
	repositoryID                    int64
	newRoom                         func(domain.ProjectImage, ward.ContainerSize) (ProductionVerificationRoom, error)
	newBoundRoom                    func(domain.ProjectImage, ward.ContainerSize, domain.Run, domain.InvocationID) (ProductionVerificationRoom, error)
	rebuild                         *ProjectImageRebuild
	beginTaskWork                   func(context.Context, domain.RunID) (context.Context, func(), error)
	claimDispatch                   func(context.Context, domain.RunID) error
	reviewSource                    exec.ReviewSource
	reviewAdmitter                  ReviewAdmitter
	remediationPromptPackage        domain.Digest
	validateDelivery                func(context.Context, exec.StartSpec) error
	feedbackAdmission               *admitter
	shadowReviewSource              exec.ReviewSource
	findingAdjudicator              findingAdjudicator
	signet                          *signet.Service
	inference                       *inference.Client
	reviewRecovery                  func(context.Context) error
	reviewRecoveryPending           bool
	reviewConfigurationDigest       domain.Digest
	shadowReviewConfigurationDigest domain.Digest
	shadowReviewDefaultRate         float64
	shadowReviewFailure             func(domain.RunID, int, domain.ReviewFailureClass, error)
	reviewHostInstructions          ReviewHostInstructions
	holdOnly                        bool
	recipeReadTimeout               time.Duration
	holdRetryInterval               time.Duration
	now                             func() time.Time
	holdRetryAfter                  map[string]time.Time
	// successorObservationPending counts consecutive successor attempts, by
	// task intent key, that saw GitHub's PR view lagging the branch (#1544).
	// Process state only: a restart starts the tolerance window again.
	successorObservationPending map[string]int
	reviewRetryAfter            map[domain.RunID]time.Time
	// closedPublicationRetry paces the closed-workflow publication
	// observation per run (#2343). Process state only: a restart starts each
	// schedule again from one immediate attempt, and the run's durable hold
	// carries the finding across it.
	closedPublicationRetry map[domain.RunID]closedPublicationPace
	// reconcileClosedPublication replaces the publisher's closed-run
	// observation when set, so a test can count attempts.
	reconcileClosedPublication func(context.Context, domain.RunID) error
	// holdPace bounds this workflow's per-pass hold projection writes: the
	// hold-only composition's observations, and the active composition's
	// clear when it accepts a queued task (issue #394). Process state only,
	// never authority.
	holdPace             observationPace
	preparationDigest    domain.Digest
	imageUpkeepPending   func(context.Context) (*domain.ImageUpkeepHold, error)
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
	feedbackTransitions int
	completed           int
	accepted            int
	readyClean          int
	readyDegraded       int
	blocked             int
	lastPR              int
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
		(!cfg.HoldOnly && (cfg.ReviewSource == nil || cfg.ReviewAdmission == nil ||
			!contentaddr.Valid(string(cfg.ReviewConfigurationDigest)) ||
			cfg.ReviewHostInstructions.validate() != nil)) {
		return nil, errors.New("nil dependency")
	}
	if shadowEnabled {
		if !contentaddr.Valid(string(cfg.ShadowReviewConfigurationDigest)) ||
			math.IsNaN(cfg.ShadowReviewDefaultRate) || math.IsInf(cfg.ShadowReviewDefaultRate, 0) ||
			cfg.ShadowReviewDefaultRate < 0 || cfg.ShadowReviewDefaultRate > 1 ||
			cfg.ShadowReviewFailure == nil {
			return nil, errors.New("invalid shadow review configuration")
		}
	} else if cfg.ShadowReviewConfigurationDigest != "" ||
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
	if cfg.RebuildProjectImage != nil {
		if cfg.RebuildProjectImage.Builder == nil {
			return nil, errors.New("project-image rebuild needs a builder")
		}
		if err := cfg.RebuildProjectImage.Inputs.Validate(); err != nil {
			return nil, fmt.Errorf("project-image rebuild inputs: %w", err)
		}
		if cfg.RebuildProjectImage.Timeout < 0 {
			return nil, errors.New("project-image rebuild timeout is negative")
		}
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
		operatorRecipes: mapsClone(cfg.ApprovedRecipes), repositoryID: cfg.RepositoryID,
		newRoom: cfg.NewRoom, reviewSource: cfg.ReviewSource,
		reviewAdmitter:           cfg.ReviewAdmission,
		newBoundRoom:             cfg.NewBoundRoom,
		rebuild:                  cfg.RebuildProjectImage,
		imageUpkeepPending:       cfg.ImageUpkeepPending,
		remediationPromptPackage: cfg.RemediationPromptPackageDigest,
		shadowReviewSource:       cfg.ShadowReviewSource,
		reviewRecovery:           cfg.ReviewRecovery, reviewRecoveryPending: true,
		reviewConfigurationDigest:       cfg.ReviewConfigurationDigest,
		shadowReviewConfigurationDigest: cfg.ShadowReviewConfigurationDigest,
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
		holdRetryAfter:              make(map[string]time.Time),
		successorObservationPending: make(map[string]int),
		reviewRetryAfter:            make(map[domain.RunID]time.Time),
		closedPublicationRetry:      make(map[domain.RunID]closedPublicationPace),
		preparationDigest:           cmp.Or(cfg.PreparationDigest, projectimage.PreparationDigest()),
		afterVerification:           cfg.AfterVerification,
		afterPublication:            cfg.AfterPublication, afterReady: cfg.AfterReady,
		afterBlocked: cfg.AfterBlocked, afterTerminal: cfg.AfterTerminal,
		afterTaskLockRelease: cfg.AfterTaskLockRelease,
		transitionHook:       cfg.TransitionHook,
	}, nil
}

// recipes is the recipe set the workflow gates verification and evidence
// against (plan §5.7, Project-image upkeep): the operator's set plus the
// digests image-policy adoptions approved for the daemon's repository. It
// is read for each decision because an approve is recorded while the
// daemon runs. The map is the caller's to keep.
func (w *productionPublicationWorkflow) recipes(ctx context.Context) (map[domain.Digest]bool, error) {
	// A pure constructor test omits persistence, as displayNames allows; it
	// has no adoptions to add.
	if w.store == nil {
		return mapsClone(w.operatorRecipes), nil
	}
	var recipes map[domain.Digest]bool
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		recipes, err = w.recipesIn(ctx, tx)
		return err
	})
	return recipes, err
}

// recipesIn is recipes inside the caller's transaction.
func (w *productionPublicationWorkflow) recipesIn(
	ctx context.Context, tx *store.ReadTx,
) (map[domain.Digest]bool, error) {
	adoptions, err := tx.ListImagePolicyAdoptions(ctx, w.repositoryID)
	if err != nil {
		return nil, fmt.Errorf("read the repository's adopted recipes: %w", err)
	}
	recipes := mapsClone(w.operatorRecipes)
	for _, adoption := range adoptions {
		if adoption.RecipeDigest != nil {
			recipes[*adoption.RecipeDigest] = true
		}
	}
	return recipes, nil
}

func mapsClone[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (w *productionPublicationWorkflow) reconcile(ctx context.Context) (productionPublicationResult, error) {
	w.reconcileMu.Lock()
	defer w.reconcileMu.Unlock()
	if w.reviewRecoveryPending {
		// Startup only: recovery aborts every started review as orphaned, so a
		// runtime pass would kill live ones. abortRecoveredReview reaps the one
		// review an operator recovery frees.
		if err := w.reviewRecovery(ctx); err != nil {
			return productionPublicationResult{}, fmt.Errorf("recover reviews at startup: %w", err)
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
	var result productionPublicationResult
	var joined error
	feedbackTransitions, err := w.reconcileOperatorFeedback(ctx)
	result.feedbackTransitions = feedbackTransitions
	if err != nil {
		joined = errors.Join(joined, fmt.Errorf("reconcile returned operator feedback: %w", err))
	}
	var pending []store.QueueEntry
	var releasedWaits, concludedWaits []string
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		reevaluations, err := tx.ListPendingOutbox(ctx, signet.PublicationReevaluationRequestedKind)
		if err != nil {
			return err
		}
		pending = append(pending, reevaluations...)
		continuations, err := tx.ListPendingOutbox(ctx, domain.PublicationContinuationRequestedKind)
		if err != nil {
			return err
		}
		pending = append(pending, continuations...)
		// A published task whose human-gate closure decision has landed
		// re-enters beside the pending rows (publication_closure_wait.go).
		released, concluded, err := releasedClosureWaitTasks(ctx, tx, pending)
		if err != nil {
			return err
		}
		for _, entry := range released {
			releasedWaits = append(releasedWaits, entry.IdempotencyKey)
		}
		concludedWaits = concluded
		pending = append(pending, released...)
		return nil
	}); err != nil {
		return result, errors.Join(joined, err)
	}
	if err := w.retireClosureWaits(ctx, concludedWaits); err != nil {
		joined = errors.Join(joined, fmt.Errorf("retire concluded closure waits: %w", err))
	}
	w.pruneHeldTaskRetries(pending)
	for _, entry := range pending {
		var task productionPublicationTask
		var err error
		switch entry.Kind {
		case domain.PublicationContinuationRequestedKind:
			task, err = w.reconstructPublicationContinuationTask(ctx, entry)
		case signet.PublicationReevaluationRequestedKind:
			task, err = w.reconstructProductionReevaluationTask(ctx, entry)
		default:
			task, err = decodeProductionPublicationTask(entry)
		}
		if err != nil {
			// A reevaluation intent is returned authority, not an engine-owned
			// task row. The authoritative issue contract requires a mismatch to
			// fail closed without turning the untrusted coordinates into a store
			// mutation, including a quarantine item.
			if entry.Kind == signet.PublicationReevaluationRequestedKind {
				joined = errors.Join(joined, fmt.Errorf(
					"reevaluation intent %q cannot be reconstructed: %w: %w",
					entry.IdempotencyKey, err, domain.ErrParentKeyMismatch,
				))
				continue
			}
			// A task row this daemon cannot reconstruct is the marker's
			// failure mode one row over: a newer daemon writes a newer task
			// too, so joining the error here would end Engine.Run on every
			// pass for as long as the row exists (#424). Quarantine the run
			// it names and leave the row pending for a daemon that can read
			// it. A row whose key this lane could not have filed names no
			// run, so it stays loud.
			if runID, ok := productionRunIDFromPendingTaskKey(entry.IdempotencyKey); ok {
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
		if err := w.requireWorkflowOpen(ctx, task.RunID); err != nil {
			if workflowAdvancementFenced(err) {
				continue
			}
			joined = errors.Join(joined, err)
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
		if retryAfter, held := w.holdRetryAfter[task.intentKey()]; held {
			if w.now().Before(retryAfter) {
				continue
			}
			delete(w.holdRetryAfter, task.intentKey())
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
		release, err := acquirePublicationLock(ctx, lockDir, entry.IdempotencyKey)
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
		if !waitsOnImage(reconcileErr) {
			if obsErr := w.clearImageUpkeepHold(ctx, task); obsErr != nil {
				joined = errors.Join(joined, fmt.Errorf("task %q: %w", entry.IdempotencyKey, obsErr))
			}
		}
		if reconcileErr != nil {
			if workflowAdvancementFenced(reconcileErr) {
				continue
			}
			if productionPublicationRetryableFailure(reconcileErr) {
				// The environmental back-off is a hold an operator can see:
				// record its typed cause beside the retry window (issue
				// #394). A failed observation write is a store fault, joined
				// loud like any other.
				if obsErr := w.recordRetryableHold(ctx, task, reconcileErr); obsErr != nil {
					joined = errors.Join(joined, fmt.Errorf("task %q: %w", entry.IdempotencyKey, obsErr))
				}
				w.deferHeldTask(task)
			} else {
				// A published task re-entered only to release its closure hold
				// fails loud once, not on every pass: its wait retires first, so a
				// pull request the repair cannot reach (for example one merged
				// before the pass observed it) stays a draft rather than wedging
				// the lane across restarts.
				if slices.Contains(releasedWaits, entry.IdempotencyKey) {
					reconcileErr = errors.Join(reconcileErr, w.retireClosureWaits(ctx, []string{entry.IdempotencyKey}))
				}
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
		errors.Is(err, store.ErrStaleWrite) ||
		errors.Is(err, store.ErrProjectAuthorityMissing)
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
		errors.Is(err, publish.ErrSuccessorObservationPending) ||
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
	for key := range w.holdRetryAfter {
		if _, found := pendingKeys[key]; !found {
			delete(w.holdRetryAfter, key)
		}
	}
	for key := range w.successorObservationPending {
		if _, found := pendingKeys[key]; !found {
			delete(w.successorObservationPending, key)
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

// productionReadiness is the §6 verdict beside its card-facing projection
// (issue #982). currentReadinessVerdict evaluates the verdict and keeps its
// target and states; withHandoff projects the detail from those same inputs
// once the handoff tip is known, so the ready item's summary and detail can
// never describe different sets.
type productionReadiness struct {
	verdict domain.ReadinessVerdict
	target  domain.EvaluationTarget
	states  []domain.CheckState
	detail  domain.ReadinessDetail
}

// withHandoff projects the card-facing detail, naming the target tip the
// handoff check resolved and, when that tip had moved past the evidence base,
// the verified merge of the head into it (#1735).
func (r productionReadiness) withHandoff(tipSHA, mergeSHA string) (productionReadiness, error) {
	detail, err := domain.NewReadinessDetail(r.target, r.verdict, r.states, tipSHA, mergeSHA)
	if err != nil {
		return productionReadiness{}, err
	}
	r.detail = detail
	return r, nil
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
	// image is the project image the run was admitted with. It answers
	// whether the run's base is served and names the recipe.
	image domain.ProjectImage
	// rebuilt is the image the run is bound to for the verified commit when
	// that is not image (production_rebuild.go); nil otherwise.
	rebuilt     *domain.ProjectImage
	remediation *authenticatedRemediationTransition
	// reentry is set for a cycle that re-enters in place (production_reentry.go).
	// For such a cycle admission is the predecessor producer's with its base
	// commit replaced by the cycle's base, so every reader of the admitted
	// base below evaluates against the base the authority names.
	reentry *reentryCycle
	// evaluatedSHA is the prospective merge the handoff check verifies in
	// place of the head. Only the handoff cycles set it
	// (production_handoff.go); it is empty for every other cycle, and no
	// review sees it.
	evaluatedSHA string
}

func (w *productionPublicationWorkflow) loadBinding(
	ctx context.Context, task productionPublicationTask,
) (productionBinding, error) {
	if task.reentersInPlace() {
		return w.loadReentryBinding(ctx, task)
	}
	var binding productionBinding
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding.run, err = tx.GetRun(ctx, task.RunID)
		if err != nil {
			return err
		}
		if err := authenticateTaskSuccessor(ctx, tx, task); err != nil {
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
		if task.reevaluation != nil {
			binding.profile, err = tx.GetTrustProfile(ctx, task.reevaluation.TrustProfileDigest)
		} else if binding.admission.TrustProfileDigest == nil {
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
	binding.spec, err = loadArtifactBlob(w.artifacts, binding.run.SpecDigest)
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
		!productionReplayPrecedesCompletion(binding.replay.ImportOptions.CommitDate, binding.export.RecordedAt) ||
		(task.reevaluation == nil && binding.admission.TrustProfileDigest != nil &&
			binding.profile.ProfileDigest != *binding.admission.TrustProfileDigest) ||
		binding.profile.Repo != binding.admission.Base.Repo ||
		binding.profile.RepositoryID != binding.admission.Base.RepositoryID ||
		binding.image.Repository != binding.admission.Base.Repo ||
		binding.image.RepositoryID != binding.admission.Base.RepositoryID {
		return productionBinding{}, fmt.Errorf("production publication binding disagrees with durable authority: %w",
			domain.ErrParentKeyMismatch)
	}
	if err := validateProductionReplayOptions(binding, task.Publication); err != nil {
		return productionBinding{}, err
	}
	if err := w.resolveVerificationImage(ctx, &binding, task.HeadSHA); err != nil {
		return productionBinding{}, err
	}
	return binding, nil
}

// requireProjectImageServesBase refuses to build a verification room from an
// image that cannot serve baseSHA (domain.ProjectImage.AdmissibleAt). Both
// verification paths call it immediately before the room exists, so a task
// that already holds its verification evidence is never refused: that
// evidence was produced under a passing check, and one clause of the verdict
// follows the running binary.
//
// checkoutDir is the checkout the caller proved with
// validatePublicationCheckoutBinding. The inputs are read from baseSHA's tree
// by its exact name, never the worktree, so nothing imported or fetched into
// the checkout since can change them.
//
// The verdict is re-derived from the immutable image record and the base tree
// instead of being read from the admission: the admission names the image and
// the base, and a stored "compatible" bit would be a trust bit this boundary
// could not re-check. An image built at exactly baseSHA is admitted from the
// record alone, as it always was.
//
// A refusal carries domain.ErrProjectImageIncompatible and each caller decides
// what it ends. A base that holds a dependency manifest in a shape no image
// can have baked is the same refusal. A plumbing fault reading the checkout is
// returned as itself.
func requireProjectImageServesBase(
	ctx context.Context, image domain.ProjectImage, checkoutDir, baseSHA string,
	preparation domain.Digest,
) error {
	if image.CommitSHA == baseSHA {
		return nil
	}
	inputs, err := projectimage.ObserveBaseInputs(ctx, "git", checkoutDir, baseSHA, image.InputLayout())
	if errors.Is(err, verify.ErrCommitFileUnreadable) {
		return fmt.Errorf("%w: %w (image %s)", domain.ErrProjectImageIncompatible, err, image.ImageRef)
	}
	if err != nil {
		return fmt.Errorf("observe project-image inputs at base %s: %w", baseSHA, err)
	}
	return image.AdmissibleAt(inputs, preparation)
}

// Replay uses the start-pinned commit date to reproduce HeadSHA. RecordedAt
// describes export completion and may be later; legacy exports use the same
// instant for both. Neither timestamp may be rewritten during reconstruction.
func productionReplayPrecedesCompletion(commitDate, recordedAt time.Time) bool {
	return !commitDate.IsZero() && !recordedAt.IsZero() && !commitDate.After(recordedAt)
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
	task, err := decodeProductionPublicationTaskWithPublication(entry, ProductionPublication.validateRetained)
	if err != nil {
		return nil, err
	}
	if task.reentersInPlace() {
		// Nothing was imported, so the row retains no replay or result blob.
		return nil, nil
	}
	return append(productionReplayDigests(task.Replay), task.Artifacts...), nil
}

func sameOptionalDigest(a, b *domain.Digest) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (w *productionPublicationWorkflow) reconcileTask(
	ctx context.Context, task productionPublicationTask,
) (outcome productionTaskOutcome, resultErr error) {
	binding, err := w.loadBinding(ctx, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if w.beginTaskWork != nil {
		workCtx, finish, err := w.beginTaskWork(ctx, binding.run.ID)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		parentCtx := ctx
		defer func() {
			finish()
			if resultErr != nil && workCtx.Err() != nil && parentCtx.Err() == nil {
				fenceErr := w.store.Read(parentCtx, func(tx *store.ReadTx) error {
					return requireTaskExecutionOpen(parentCtx, tx, task.RunID)
				})
				if errors.Is(fenceErr, store.ErrTaskCancellationFenced) {
					resultErr = fenceErr
				}
			}
		}()
		ctx = workCtx
	}
	if task.Successor != nil || task.continuation != nil {
		err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			return tx.RequireIncompletePublication(ctx, task.RunID)
		})
		if errors.Is(err, store.ErrPublicationCompleted) {
			if task.continuation != nil {
				return w.refuseCompletedPublicationContinuation(ctx, task)
			}
			return productionTaskOutcome{}, nil
		}
		if err != nil {
			return productionTaskOutcome{}, err
		}
	}
	if task.reentersInPlace() {
		return w.reconcileReentryTask(ctx, task, binding)
	}
	if outcome, err := w.recoverScopeConflictTask(ctx, &task, binding); err != nil {
		if errors.Is(err, domain.ErrParentKeyMismatch) || errors.Is(err, domain.ErrCardFactInconsistent) {
			w.deferHeldTask(task)
			return productionTaskOutcome{}, recordProductionQuarantine(ctx, w.store, w.attention,
				productionScopeQuarantinePrefix, task.RunID, task.ProjectID, productionQuarantineScopeConflict)
		}
		return productionTaskOutcome{}, err
	} else if outcome != nil {
		if err := releaseProductionQuarantine(ctx, w.store, w.attention, productionScopeQuarantinePrefix, task.RunID); err != nil {
			return productionTaskOutcome{}, err
		}
		return *outcome, nil
	}
	if err := releaseProductionQuarantine(ctx, w.store, w.attention, productionScopeQuarantinePrefix, task.RunID); err != nil {
		return productionTaskOutcome{}, err
	}
	if task.Successor != nil && task.Successor.ReentersUnpublishedHead() {
		return w.retireReentryTask(ctx, task, binding)
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
	// FetchBase deliberately leaves the import worktree empty. Discover
	// instructions in a separate materialization of its pinned base, before
	// importing any candidate-controlled files into the original checkout.
	reviewInstructions, held, err := w.exactBaseReviewInstructions(
		ctx, task, checkout, filepath.Join(scratch, "base-instructions"),
		binding.admission.Base.BaseSHA,
	)
	if err != nil || held != nil {
		if held != nil {
			return *held, nil
		}
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
	// Advisory findings (plan §5.8, revision 42) are the one class a clean
	// reconstruction may carry: they never block, and the authorization
	// built below binds them so the publisher can surface them.
	if imported.CommitSHA != binding.export.HeadSHA || !importer.AllAdvisory(imported.Findings) {
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
	if task.continuation != nil {
		return w.startPublicationContinuation(ctx, task, checkoutDir)
	}
	if binding.remediation != nil {
		sourceTree, err := w.loadRemediationSourceTree(ctx, task, binding, checkoutDir)
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
			task.readyItemID(), domain.ErrParentKeyMismatch,
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
			readiness, _, err := w.assertReviewedCandidate(
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
			// A ready item recovered below is compared with one rebuilt from
			// this readiness, so its detail names the same handoff tip.
			readiness, err = w.withPublishedHandoff(ctx, task, binding, readiness)
			if err != nil {
				return productionTaskOutcome{}, err
			}
			report, err := loadVerificationReport(w.artifacts, checkpoint.Artifacts, task.verificationInvocationID())
			if err != nil {
				return productionTaskOutcome{}, productionPublicationRetryableError(err)
			}
			candidate, err := w.productionCandidate(ctx, task, binding, checkpoint, adoptedProfile, nil, report)
			if err != nil {
				return w.holdPublicMetadataTask(ctx, task, checkpoint.Imported, err)
			}
			if err := w.applySealedHandoffCheck(ctx, task, &candidate); err != nil {
				return productionTaskOutcome{}, err
			}
			if err := w.bindSuccessorCandidate(ctx, task, &candidate); err != nil {
				return productionTaskOutcome{}, err
			}
			history, err := publish.LoadDispositionHistory(
				ctx, w.store, candidate, reviewInstructions.ResultDigest,
			)
			if err != nil {
				if errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
					return w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
				}
				if errors.Is(err, domain.ErrParentKeyMismatch) {
					return w.holdDispositionHistoryMismatch(ctx, task, checkpoint.Imported)
				}
				return productionTaskOutcome{}, fmt.Errorf("load recovered publication disposition history: %w", err)
			}
			candidate.DispositionHistory = &history
			if readyExists {
				published, err := w.loadReadyPublicationOutcome(
					ctx, task, binding, checkpoint, candidate, readiness,
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
						durablePublicationConflictReason(err),
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
	recipes, err := w.recipes(ctx)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if !recipes[binding.image.RecipeDigest] {
		pendingIntent, err := w.hasPendingPublicationIntent(ctx, task.PublicationID)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if found || pendingIntent {
			return w.holdBlockedTask(
				ctx, task, imported,
				"Publication is durably held because current trust no longer approves the admitted project-image recipe. Restore that approval to recover the committed publication intent.",
				domain.HoldRecipeRevoked,
			)
		}
		return w.completeBlockedTask(
			ctx, task, binding.run, imported, nil,
			productionBlockRecipeRevoked, productionRerunnableBlockActions,
			publishBlockTrustRule(domain.TrustRuleRecipeUnapproved),
		)
	}
	if !found {
		checkpoint, err = w.verifyAndCheckpoint(ctx, task, binding, imported, checkoutDir)
		if verificationMemoryLimit(err) {
			return w.completeBlockedTask(
				ctx, task, binding.run, imported, nil,
				productionBlockMemoryLimit, productionRerunnableBlockActions,
				publishBlockHoldReason(domain.HoldVerificationMemoryLimit),
			)
		}
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
			productionBlockVerification, productionRerunnableBlockActions,
			publishBlockTrustRule(domain.TrustRuleVerificationFailed),
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
	case productionReviewContinue:
		return productionTaskOutcome{}, fmt.Errorf(
			"review continuation escaped the review gate: %w", domain.ErrParentKeyMismatch)
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
				productionBlockTrust, productionShadowStopBlockActions, nil,
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
	// Handoff (plan §6, §7, revision 110): resolve the target's tip and, when
	// it moved past the evidence base, verify the head merged into it. No
	// review runs here; the head is the one review passed.
	handoff, heldHandoff, err := w.reconcileHandoff(ctx, task, binding, checkout, checkoutDir, checkpoint)
	if err != nil || heldHandoff != nil {
		return derefOutcome(heldHandoff), err
	}
	adoptedProfile, adoptedErr := w.adoptedReviewProfileDigest(ctx, binding)
	if adoptedErr != nil {
		return productionTaskOutcome{}, adoptedErr
	}
	report, err := loadVerificationReport(w.artifacts, checkpoint.Artifacts, task.verificationInvocationID())
	if err != nil {
		return productionTaskOutcome{}, productionPublicationRetryableError(err)
	}
	// Run the publication author once for this reviewed candidate, after the
	// clean review and before the candidate body is composed (§7 order). It is
	// idempotent and never blocks: an unavailable author or a screen failure
	// records a durable v1 fallback (issue #1419 Part B).
	if err := w.reconcilePublicationAuthoring(
		ctx, task, binding, checkpoint, reviewInstructions, checkoutDir, report,
	); err != nil {
		return productionTaskOutcome{}, productionPublicationRetryableError(err)
	}
	// Admit the source-issue-closure proposal for this reviewed candidate and
	// bind the merge into the tip the handoff checked, right after the author
	// run where the author input is available (§7 order). It is idempotent and
	// never blocks: a source that is not closable records a durable no-close
	// answer (issue #1419 Part D). An approval bound to an earlier base never
	// authorizes a Closes against the checked tip.
	if err := w.reconcilePublicationClosure(
		ctx, task, binding, handoff.TipSHA, checkpoint, reviewInstructions, checkoutDir, report,
	); err != nil {
		return productionTaskOutcome{}, productionPublicationRetryableError(err)
	}
	candidate, err := w.productionCandidate(ctx, task, binding, checkpoint, adoptedProfile, nil, report)
	if err != nil {
		return w.holdPublicMetadataTask(ctx, task, checkpoint.Imported, err)
	}
	candidate.BaseSHA = handoff.TipSHA
	candidate.HandoffCheck = &handoff
	if err := w.bindSuccessorCandidate(ctx, task, &candidate); err != nil {
		return productionTaskOutcome{}, err
	}
	dispositionHistory, err := publish.LoadDispositionHistory(
		ctx, w.store, candidate, reviewInstructions.ResultDigest,
	)
	if err != nil {
		if errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
			return w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
		}
		if errors.Is(err, domain.ErrParentKeyMismatch) {
			return w.holdDispositionHistoryMismatch(ctx, task, checkpoint.Imported)
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
				durablePublicationConflictReason(err),
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
	published, err := w.publishCandidate(ctx, task, candidate, checkout)
	// Only a success or a non-retryable failure ends a lagging window. A
	// transient failure between lagging reads (a 5xx, a dropped push) says
	// nothing about the lag, and clearing on it would let a flaky GitHub
	// postpone the card for a PR that is really missing.
	if err == nil || !productionPublicationRetryableFailure(err) {
		delete(w.successorObservationPending, task.intentKey())
	}
	if err != nil {
		if errors.Is(err, publish.ErrSuccessorObservationPending) {
			return w.holdLaggingSuccessorObservation(ctx, task, checkpoint.Imported, err)
		}
		if isDurablePublicationConflict(err) {
			return w.holdBlockedTask(
				ctx, task, checkpoint.Imported,
				durablePublicationConflictReason(err),
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
			// An unsatisfied handoff check here is never a moved tip: the
			// handoff above verified the tip it sealed. It is the daemon
			// contradicting its own seal, or a check sealed before migration
			// 0129 that names no audit (#2073); rerunning the task seals a
			// new one. It blocks with the base-advance rule all the same.
			reason := productionBlockTrust
			if errors.Is(err, publish.ErrHandoffCheckUnsatisfied) {
				reason = productionBlockBaseAdvanced
			}
			outcome, blockErr := w.completeBlockedTask(
				ctx, task, binding.run, checkpoint.Imported, checkpoint.Artifacts,
				reason, productionRerunnableBlockActions,
				publishBlockFacts(domain.HoldTrustBlocked, err),
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

// exactBaseReviewInstructions composes the review instructions from the
// pinned base alone, materialized apart from the checkout so no
// candidate-controlled file can shape them. A refusal holds the task and
// returns its outcome; the hold-only composition composes nothing.
func (w *productionPublicationWorkflow) exactBaseReviewInstructions(
	ctx context.Context,
	task productionPublicationTask,
	checkout PublicationCheckout,
	dir, baseSHA string,
) (exec.ReviewInstructionBinding, *productionTaskOutcome, error) {
	if w.holdOnly {
		return exec.ReviewInstructionBinding{}, nil, nil
	}
	hold := func(reason string) (exec.ReviewInstructionBinding, *productionTaskOutcome, error) {
		outcome, err := w.holdBlockedTask(
			ctx, task, importer.Result{CommitSHA: task.HeadSHA}, reason, domain.HoldTrustBlocked)
		if err != nil {
			return exec.ReviewInstructionBinding{}, nil, err
		}
		return exec.ReviewInstructionBinding{}, &outcome, nil
	}
	if err := w.transport.RetainWorktree(ctx, checkout, dir, baseSHA); err != nil {
		if errors.Is(err, publish.ErrMaterializationRefused) {
			return hold("Publication is durably held because the trusted base cannot be materialized for review instructions. Resolve the repository materialization problem before retrying.")
		}
		return exec.ReviewInstructionBinding{}, nil, fmt.Errorf(
			"materialize exact-base review instructions: %w", err)
	}
	instructions, err := w.composeReviewInstructions(dir)
	if err != nil {
		if errors.Is(err, errReviewInstructionsRefused) {
			return hold("Publication is durably held because the trusted review instructions cannot be composed within the approved delivery limits. Resolve the instruction configuration problem before retrying.")
		}
		return exec.ReviewInstructionBinding{}, nil, fmt.Errorf(
			"compose exact-base review instructions: %w", err)
	}
	return instructions, nil, nil
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
) (productionReadiness, func(context.Context) error, error) {
	resolutions, err := productionReadinessResolutions(
		binding, w.store.VerificationFloorRegistryGeneration(),
	)
	if err != nil {
		return productionReadiness{}, nil, err
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
			return productionReadiness{}, nil, fmt.Errorf(
				"production readiness resolution set omits requirement %q: %w",
				definition.Key, domain.ErrParentKeyMismatch)
		}
	}
	base := binding.admission.Base
	// A binding that names a prospective merge evaluated it in place of the
	// head, so both proofs and the target name that merge. The
	// proofs an earlier cycle recorded for the same head name none, and
	// EvaluationTarget.covers refuses them.
	newProof := func(resolution domain.RequirementResolution, recipe domain.Digest) (domain.CheckProof, error) {
		if binding.evaluatedSHA == "" {
			return domain.NewCheckProof(resolution, task.HeadSHA, &base, recipe)
		}
		return domain.NewProspectiveMergeCheckProof(resolution, domain.ProspectiveMergeIdentity{
			BaseSHA: base.BaseSHA, HeadSHA: task.HeadSHA, MergeSHA: binding.evaluatedSHA,
		}, base, recipe)
	}
	verificationProof, err := newProof(resolutions[0], binding.image.RecipeDigest)
	if err != nil {
		return productionReadiness{}, nil, err
	}
	verificationState, err := domain.NewPassedCheckState(resolutions[0], verificationProof)
	if err != nil {
		return productionReadiness{}, nil, err
	}
	reviewRecipeDigest, err := digestJSON(struct {
		Configuration domain.Digest `json:"configuration"`
		Instructions  domain.Digest `json:"instructions"`
	}{reviewRecord.ConfigurationDigest, reviewInstructions.ResultDigest})
	if err != nil {
		return productionReadiness{}, nil, err
	}
	reviewProof, err := newProof(resolutions[1], reviewRecipeDigest)
	if err != nil {
		return productionReadiness{}, nil, err
	}
	reviewState, err := domain.NewPassedCheckState(resolutions[1], reviewProof)
	if err != nil {
		return productionReadiness{}, nil, err
	}
	states := []domain.CheckState{verificationState, reviewState}
	target := domain.EvaluationTarget{
		CandidateHead: task.HeadSHA, Base: &base, MergeSHA: binding.evaluatedSHA,
	}
	verdict, err := domain.EvaluateReadiness(target, resolutions, states, nil)
	if err != nil {
		return productionReadiness{}, nil, err
	}
	if checkpoint.Authorization.VerificationOutcome != domain.VerificationPassed ||
		verdict.Class == domain.ReadinessBlocked {
		return productionReadiness{}, nil, fmt.Errorf("current verification set is blocked: %w", domain.ErrParentKeyMismatch)
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
	// The card-facing detail is projected later, by withHandoff, from the
	// same target, verdict, and states as the verdict itself, so its bound
	// head, base, and entries are the evaluation's, never re-derived by the
	// item producer.
	return productionReadiness{verdict: verdict, target: target, states: states}, persist, nil
}

func (w *productionPublicationWorkflow) assertReviewedCandidate(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	reviewInstructions exec.ReviewInstructionBinding,
) (productionReadiness, func(context.Context) error, error) {
	if err := w.shadowReviewBlocksReady(ctx, task, binding); err != nil {
		return productionReadiness{}, nil, err
	}
	record, failure, err := w.latestReviewState(ctx, task.RunID)
	if err != nil {
		return productionReadiness{}, nil, err
	}
	approved, err := w.reviewConfigurationApproved(ctx, binding)
	if err != nil {
		return productionReadiness{}, nil, err
	}
	if !approved {
		return productionReadiness{}, nil, reviewConfigurationUnapprovedError(
			binding.profile.Review.ConfigDigest, w.reviewConfigurationDigest,
		)
	}
	if record != nil && record.ConfigurationDigest != w.reviewConfigurationDigest {
		return productionReadiness{}, nil, fmt.Errorf(
			"clean review record configuration is %s: %w",
			record.ConfigurationDigest,
			reviewConfigurationUnapprovedError(
				record.ConfigurationDigest, w.reviewConfigurationDigest,
			),
		)
	}
	roundComplete := false
	if record != nil {
		roundComplete, err = w.reviewRoundDispositionComplete(ctx, task, *record)
		if err != nil {
			return productionReadiness{}, nil, err
		}
	}
	if binding.profile.Review.Mode != domain.ReviewFreesideInvoked ||
		record == nil ||
		!roundComplete ||
		record.InstructionDigest != reviewInstructions.ResultDigest ||
		record.BaseSHA != binding.admission.Base.BaseSHA ||
		record.HeadSHA != task.HeadSHA ||
		(failure != nil && failure.Round >= record.Round) {
		return productionReadiness{}, nil, fmt.Errorf(
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
	if err := w.requireWorkflowOpen(ctx, task.RunID); err != nil {
		return productionTaskOutcome{}, err
	}
	// Readiness reconstruction boundary: a published candidate reaches readiness
	// only through a clean, candidate-bound review record under the current
	// reviewer configuration, failing closed otherwise (issue #527, decision 2).
	// The §7 review gate itself runs before publication in reconcileTask; this
	// re-gate keeps every crash-recovery call site correct, including an
	// old-order run that published before recording a clean, candidate-bound
	// review.
	readiness, persistReadiness, err := w.assertReviewedCandidate(ctx, task, binding, checkpoint, reviewInstructions)
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
		// environmental and still propagates for retry. holdBlockedTask attaches
		// the already-published pull request to the held item and records its
		// binding, on this axis and the recipe re-gate below (issue #531).
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
	readiness, err = w.withPublishedHandoff(ctx, task, binding, readiness)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	yieldHistory, err := w.reviewYieldHistory(ctx, task.RunID)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	readyExists, err := w.hasCompatibleReadyItem(
		ctx, task, binding, checkpoint, published, readiness, yieldHistory,
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
		recipes, err := w.recipes(ctx)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if !recipes[binding.image.RecipeDigest] {
			return w.holdBlockedTask(
				ctx, task, checkpoint.Imported,
				"Publication is durably held because current trust no longer approves the verification recipe that authorized the candidate. Restore that approval before creating readiness.",
				domain.HoldRecipeRevoked,
			)
		}
		if err := persistReadiness(ctx); err != nil {
			return productionTaskOutcome{}, err
		}
		ready, err := w.readyItem(ctx, task, checkpoint, published, readiness, yieldHistory)
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
	if err := w.recordWorkUnitPRBinding(ctx, task, binding.admission.Base, published.PRNumber); err != nil {
		return productionTaskOutcome{}, err
	}
	// The §5.16 publication watches converge beside the item on every pass
	// through here, so a crash between the item and its watches heals.
	if err := w.armReadyItemWatches(ctx, task); err != nil {
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
	readyID := task.readyItemID()
	if err := w.supersedeBlockedHold(ctx, task, blockedHoldReadyReason, &readyID); err != nil {
		return productionTaskOutcome{}, err
	}
	accepted, err := w.recordCompletedTerminalAtBoundary(
		ctx, binding.run, task, signet.PublicationReevaluationPublished)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if w.afterTerminal != nil {
		if err := w.afterTerminal(); err != nil {
			return productionTaskOutcome{}, fmt.Errorf("after production terminal: %w",
				errors.Join(err, errProductionCrashSeam))
		}
	}
	// A pull request a human-gate closure decision still holds as a draft is
	// revisited once the decision lands; one no longer held retires its wait.
	// The wait settles in the transaction that retires the task row
	// (publication_closure_wait.go).
	if err := w.finishPublishedTask(ctx, task, binding); err != nil {
		return productionTaskOutcome{}, err
	}
	return productionTaskOutcome{
		completed: true, accepted: accepted, readiness: &readiness.verdict, prNumber: published.PRNumber,
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

// holdDispositionHistoryMismatch holds one task whose disposition history
// contradicts the current review and readiness authority. It is a per-run
// trust-record contradiction, so it holds the task instead of stopping the
// daemon; both disposition-history load sites share it so they cannot drift.
func (w *productionPublicationWorkflow) holdDispositionHistoryMismatch(
	ctx context.Context,
	task productionPublicationTask,
	imported importer.Result,
) (productionTaskOutcome, error) {
	return w.holdBlockedTask(
		ctx, task, imported,
		"Publication is durably held because its disposition history no longer matches the current review and readiness authority.",
		domain.HoldTrustBlocked,
	)
}

func (w *productionPublicationWorkflow) hasCompatibleReadyItem(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
	readiness productionReadiness,
	yieldHistory domain.ReviewYieldHistory,
) (bool, error) {
	recipes, err := w.recipes(ctx)
	if err != nil {
		return false, err
	}
	approved := recipes[binding.image.RecipeDigest]
	historicalRecipes := recipes
	historicalRecipes[binding.image.RecipeDigest] = true
	expectedReady, err := w.readyItemWithRecipes(
		ctx, task, checkpoint, published, readiness, yieldHistory, historicalRecipes,
	)
	if err != nil {
		return false, err
	}
	var expectedRedacted domain.AttentionItem
	if !approved {
		redactedCheckpoint := checkpoint
		redactedCheckpoint.Artifacts = nil
		expectedRedacted, err = w.readyItemWithRecipes(
			ctx, task, redactedCheckpoint, published, readiness, yieldHistory, historicalRecipes,
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
	var current domain.AttentionItem
	currentErr := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		current, err = tx.GetAttentionItemRecord(ctx, task.blockedItemID())
		return err
	})
	if currentErr != nil && !errors.Is(currentErr, store.ErrNotFound) {
		return productionTaskOutcome{}, currentErr
	}
	currentFound := currentErr == nil
	// The pull request is resolved before the first write, so a hold that
	// cannot restate the reference it already carries writes nothing.
	pull, err := w.heldPullRequest(ctx, task, current, currentFound, cause)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	// The durable hold's typed cause is stated by each call site, never
	// derived from the operator prose; the retry window paces the write
	// (issue #394).
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordRunHold(ctx, tx, task.RunID, task.PublicationID, cause, holdDetail{}, w.now().UTC())
	}); err != nil {
		return productionTaskOutcome{}, err
	}
	item, err := w.blockedHoldItem(ctx, task, imported, reason, cause, pull)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	outcome := productionTaskOutcome{blocked: !currentFound}
	if currentFound {
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
		// The reference is part of the comparison so a hold that opened before
		// its run published gains the pull request on its next version.
		unchanged := current.Reason == item.Reason &&
			slices.Equal(current.RequestedDecision, item.RequestedDecision) &&
			reflect.DeepEqual(current.AgentClaims, item.AgentClaims) &&
			reflect.DeepEqual(current.CommitPlanNotice, item.CommitPlanNotice) &&
			reflect.DeepEqual(current.PRReference, item.PRReference)
		if !unchanged {
			item.ItemVersion = current.ItemVersion + 1
			item.Timing = current.Timing
			item.ConversationID = current.ConversationID
			item.CreatedAt = current.CreatedAt
			item.ExpiresWhen = current.ExpiresWhen
			if err := w.attention.PutItem(ctx, item); err != nil {
				return productionTaskOutcome{}, err
			}
		}
	} else if err := w.attention.PutItem(ctx, item); err != nil {
		return productionTaskOutcome{}, err
	}
	// The bindings converge on every pass, unchanged item included, so a crash
	// between the item and its bindings heals on the next paced retry.
	if pull != nil {
		if err := w.recordHeldItemPRBinding(ctx, task, *pull); err != nil {
			return productionTaskOutcome{}, err
		}
		// A declared work unit can complete while its run is held, so its
		// binding must not wait for readiness.
		if err := w.recordWorkUnitPRBinding(ctx, task, pull.base, pull.number); err != nil {
			return productionTaskOutcome{}, err
		}
	}
	w.deferHeldTask(task)
	return outcome, nil
}

// recordedPullRequest is the pull request a run's publication records prove
// it published, with the coordinates its held-item binding needs.
type recordedPullRequest struct {
	identity domain.Digest
	base     domain.BaseRevision
	number   int
	// producer is the invocation whose export the pull request's head came
	// from. A re-entered cycle has no producer of its own and names its
	// predecessor's.
	producer domain.InvocationID
}

func (p recordedPullRequest) reference() domain.PRReference {
	return domain.PRReference{Repo: p.base.Repo, Number: p.number}
}

// heldPullRequest decides which pull request a hold carries (issue #531). A
// hold carries its run's recorded pull request unless its cause is an
// external conflict, where that pull request is the thing in doubt. A
// reference already on the item overrides the cause: the store refuses a
// later version that drops or changes it, so the hold restates it or fails
// before writing anything.
func (w *productionPublicationWorkflow) heldPullRequest(
	ctx context.Context,
	task productionPublicationTask,
	current domain.AttentionItem,
	currentFound bool,
	cause domain.RunHoldReason,
) (*recordedPullRequest, error) {
	carried := currentFound && current.PRReference != nil
	if !carried && cause == domain.HoldExternalConflict {
		return nil, nil
	}
	pull, err := w.recordedPullRequest(ctx, task)
	if err != nil {
		return nil, err
	}
	if carried && (pull == nil || pull.reference() != *current.PRReference) {
		return nil, fmt.Errorf(
			"production publication hold %q carries a pull request the publication records no longer prove: %w",
			current.ID, domain.ErrParentKeyMismatch,
		)
	}
	return pull, nil
}

// recordedPullRequest reads the pull request the task's publication recorded,
// from the store alone. It returns nil while the publication has no recorded
// outcome. The number comes from a decoded outcome row, so it is returned
// only after the same comparisons the store's binding proof makes
// (validateItemPRBindingAgainst): the dispatched intent against the task, the
// outcome against the intent, and both against the producing admission. A
// disagreement is an error, never a missing pull request.
//
// publish.LoadOutcome is not used here: its verifier converges the outcome
// with the forge, and a refused repair is one of the holds this serves.
func (w *productionPublicationWorkflow) recordedPullRequest(
	ctx context.Context, task productionPublicationTask,
) (*recordedPullRequest, error) {
	if task.reentersInPlace() {
		return w.reentryPullRequest(ctx, task)
	}
	intentKey, err := publish.IntentKey(task.PublicationID, publish.IntentKindPublication)
	if err != nil {
		return nil, err
	}
	var found *recordedPullRequest
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(ctx, intentKey)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IdempotencyKey != intentKey {
			return fmt.Errorf("production publication intent read back the wrong key: %w",
				domain.ErrParentKeyMismatch)
		}
		if entry.Kind != publish.IntentKindPublication || !entry.Dispatched() {
			return nil
		}
		intent, err := publish.DecodeStoredIntent(entry)
		if err != nil {
			return fmt.Errorf("decode durable production publication intent: %w",
				errors.Join(err, domain.ErrParentKeyMismatch))
		}
		if intent.InvocationID != task.PublicationID || intent.SourceHeadSHA != task.HeadSHA ||
			intent.ProducingInvocationID != task.ProducingInvocationID ||
			intent.ReservationRunID != task.RunID {
			return fmt.Errorf("production publication intent disagrees with task: %w",
				domain.ErrParentKeyMismatch)
		}
		outcomeKey := publicationrecord.OutcomeKey(intent.Identity)
		outcomeEntry, err := tx.GetInbox(ctx, outcomeKey)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if outcomeEntry.IdempotencyKey != outcomeKey || outcomeEntry.Kind != publish.IntentKindOutcome {
			return fmt.Errorf("production publication outcome %q has kind %q: %w",
				outcomeKey, outcomeEntry.Kind, domain.ErrParentKeyMismatch)
		}
		outcome, err := publish.DecodeOutcome(outcomeEntry.Payload)
		if err != nil {
			return fmt.Errorf("decode durable production publication outcome: %w",
				errors.Join(err, domain.ErrParentKeyMismatch))
		}
		if outcome.Identity != intent.Identity || outcome.Repo != intent.Repo ||
			outcome.BaseRef != intent.BaseRef || outcome.HeadSHA != task.HeadSHA ||
			outcome.Branch != publicationrecord.ExpectedBranch(intent) ||
			!reflect.DeepEqual(outcome.Successor, intent.Successor) {
			return fmt.Errorf("production publication outcome disagrees with its intent: %w",
				domain.ErrParentKeyMismatch)
		}
		admission, err := tx.GetExecutionAdmissionRecord(ctx, task.ProducingInvocationID)
		if err != nil {
			return fmt.Errorf("production publication producing admission: %w", err)
		}
		if admission.RunID != task.RunID || admission.Base.Repo != intent.Repo ||
			admission.Base.BaseRef != intent.BaseRef {
			return fmt.Errorf("production publication intent disagrees with its admission: %w",
				domain.ErrParentKeyMismatch)
		}
		found = &recordedPullRequest{
			identity: intent.Identity, base: admission.Base, number: outcome.PRNumber,
			producer: task.ProducingInvocationID,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
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
		if err != nil {
			return err
		}
		reevaluations, err := tx.ListPendingOutbox(ctx, signet.PublicationReevaluationRequestedKind)
		if err != nil {
			return err
		}
		pending = append(pending, reevaluations...)
		continuations, err := tx.ListPendingOutbox(ctx, domain.PublicationContinuationRequestedKind)
		if err != nil {
			return err
		}
		pending = append(pending, continuations...)
		return nil
	}); err != nil {
		return err
	}
	runIDs := make([]domain.RunID, 0, len(pending))
	var joined error
	for _, entry := range pending {
		var runID domain.RunID
		var ok bool
		if entry.Kind == signet.PublicationReevaluationRequestedKind {
			task, err := w.reconstructProductionReevaluationTask(ctx, entry)
			if err != nil {
				joined = errors.Join(joined, fmt.Errorf(
					"reevaluation intent %q cannot be reconstructed: %w: %w",
					entry.IdempotencyKey, err, domain.ErrParentKeyMismatch,
				))
				continue
			}
			runID, ok = task.RunID, true
		} else {
			runID, ok = productionRunIDFromPendingTaskKey(entry.IdempotencyKey)
		}
		if !ok {
			continue
		}
		runIDs = append(runIDs, runID)
	}
	if joined != nil {
		return joined
	}
	for _, runID := range runIDs {
		now := w.now().UTC()
		if !w.holdPace.due("hold:"+string(runID), string(domain.HoldAttendedModeActive), now) {
			continue
		}
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return recordRunHold(ctx, tx, runID,
				domain.ProductionPublicationInvocationID(runID),
				domain.HoldAttendedModeActive, holdDetail{}, now)
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
		if kind == domain.MilestonePublicationBlocked {
			invocation = task.blockedMilestoneInvocationID()
		}
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: task.RunID, Kind: kind,
			InvocationID: &invocation, Reason: cause,
			RecordedAt: w.now().UTC(),
		})
	})
}

// recordWorkUnitPRBinding captures the §5.18 exact work-unit binding once
// the run's PR durably exists: every coordinate is a first-party fact (the
// admitted base revision, the recorded PR number, the publication task's
// head). A held run records it from its hold, a ready run from readiness.
// An undeclared run records nothing. The record is write-once; a converged
// re-pass must restate the same coordinates
// (compared modulo the stamped instant), and a disagreement fails loud —
// publication converges on exactly one PR, so a second binding is
// corruption, never an update.
func (w *productionPublicationWorkflow) recordWorkUnitPRBinding(
	ctx context.Context,
	task productionPublicationTask,
	base domain.BaseRevision,
	prNumber int,
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
			Repo:         base.Repo,
			RepositoryID: base.RepositoryID,
			PRNumber:     prNumber,
			BaseRef:      base.BaseRef,
			HeadSHA:      task.HeadSHA,
			RecordedAt:   w.now().UTC(),
		}
		existing, err := tx.GetWorkUnitPRBinding(ctx, declaration.ID)
		switch {
		case err == nil:
			want := record
			want.RecordedAt = existing.RecordedAt
			if task.Successor != nil {
				if err := authenticateTaskSuccessor(ctx, &tx.ReadTx, task); err != nil {
					return err
				}
				// The first binding remains historical authority. The new ready
				// binding supplies the effective head for merge observation.
				want.HeadSHA = existing.HeadSHA
			}
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
			ItemID:                  task.readyItemID(),
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

// recordHeldItemPRBinding records the pull request behind a hold that carries
// one, so the active-resource reconciler can observe it while no ready item
// exists. It is write-once like the ready binding: a repeated pass restates
// the same record, and a different pull request for the same item fails loud.
// The store proves the coordinates against the publication records on the
// write and on every read.
func (w *productionPublicationWorkflow) recordHeldItemPRBinding(
	ctx context.Context,
	task productionPublicationTask,
	pull recordedPullRequest,
) error {
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		record := domain.HeldItemPRBinding{
			ItemID:                  task.blockedItemID(),
			RunID:                   task.RunID,
			ProducingInvocationID:   pull.producer,
			PublicationInvocationID: task.PublicationID,
			PublicationIdentity:     pull.identity,
			Repo:                    pull.base.Repo,
			RepositoryID:            pull.base.RepositoryID,
			PRNumber:                pull.number,
			BaseRef:                 pull.base.BaseRef,
			HeadSHA:                 task.HeadSHA,
			RecordedAt:              w.now().UTC(),
		}
		existing, err := tx.GetHeldItemPRBinding(ctx, record.ItemID)
		switch {
		case err == nil:
			want := record
			want.RecordedAt = existing.RecordedAt
			if want != existing {
				return fmt.Errorf("stored held-item pr binding disagrees with the published state: %w",
					store.ErrImmutableConflict)
			}
			return nil
		case errors.Is(err, store.ErrNotFound):
			return tx.RecordHeldItemPRBinding(ctx, record)
		default:
			// A corrupt binding reads as an inconsistent row, never as absent.
			return err
		}
	})
}

// holdLaggingSuccessorObservation absorbs a successor attempt whose read of
// GitHub's PR view lagged the branch (#1544). Within the tolerance it returns
// the error unchanged, so the retryable path records the
// publication_environment hold and paces the next attempt; no operator card
// is written for a window that heals itself. Past the tolerance it raises the
// external_conflict card, stating what the daemon saw, and keeps raising it on
// each paced attempt until one succeeds or fails another way.
func (w *productionPublicationWorkflow) holdLaggingSuccessorObservation(
	ctx context.Context,
	task productionPublicationTask,
	imported importer.Result,
	err error,
) (productionTaskOutcome, error) {
	if w.successorObservationPending == nil {
		w.successorObservationPending = make(map[string]int)
	}
	key := task.intentKey()
	w.successorObservationPending[key]++
	if w.successorObservationPending[key] <= successorObservationPendingTolerance {
		return productionTaskOutcome{}, err
	}
	reason := productionBlockSuccessorPRNotListed
	if errors.Is(err, publish.ErrSuccessorPRHeadLagging) {
		reason = productionBlockSuccessorHeadLagging
	}
	return w.holdBlockedTask(ctx, task, imported, reason, domain.HoldExternalConflict)
}

func (w *productionPublicationWorkflow) deferHeldTask(task productionPublicationTask) {
	w.holdRetryAfter[task.intentKey()] = w.now().Add(w.holdRetryInterval)
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
			domain.HoldPublicationEnvironment, holdDetail{}, w.now().UTC())
	})
}

// recordRetryableHold records the hold behind a retryable reconcile failure:
// image_upkeep_pending for a task waiting on an image (a re-entry whose base
// waits on upkeep, or a rebuild the busy builder could not start), and
// publication_environment for any other environmental back-off.
func (w *productionPublicationWorkflow) recordRetryableHold(
	ctx context.Context, task productionPublicationTask, err error,
) error {
	if launchMemoryRefusal(err) {
		return noticeLaunchMemoryRefusal(
			ctx, w.store, w.attention, &w.holdPace, task.RunID, task.verificationInvocationID(), task.ProjectID, err)
	}
	var capacity *domain.MachineCapacityError
	if errors.As(err, &capacity) {
		// Recorded each retry window, which paces it, so its numbers stay
		// current while the job waits.
		return w.store.Write(ctx, func(tx *store.WriteTx) error {
			return recordRunHold(ctx, tx, task.RunID, task.verificationInvocationID(),
				domain.HoldMachineCapacity, holdDetailFor(err), w.now().UTC())
		})
	}
	var pending *ImageUpkeepPendingError
	upkeep := errors.As(err, &pending)
	if !upkeep && !errors.Is(err, errProjectImageBuilderBusy) {
		return w.recordPublicationEnvironmentHold(ctx, task)
	}
	if ctx.Err() != nil {
		return nil
	}
	hold := domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingRebuild}
	if upkeep {
		hold = pending.Hold
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordRunHold(ctx, tx, task.RunID, task.PublicationID,
			domain.HoldImageUpkeepPending, holdDetail{imageUpkeep: &hold}, w.now().UTC())
	}); err != nil {
		return err
	}
	// The clear after an attempt that no longer waits must not be paced
	// away while this hold is on the read surface.
	w.holdPace.forget(imageUpkeepClearKey(task))
	return nil
}

// imageUpkeepClearKey paces clearImageUpkeepHold per run.
func imageUpkeepClearKey(task productionPublicationTask) string {
	return "image-upkeep:" + string(task.RunID)
}

// clearImageUpkeepHold ends the image_upkeep_pending hold once an attempt
// ends without waiting on an image again. It runs after the attempt, not at
// its start, so a wait that recurs keeps its row and its span. Paced, so a
// run that never held costs one delete per pacing window, not one per pass.
func (w *productionPublicationWorkflow) clearImageUpkeepHold(
	ctx context.Context, task productionPublicationTask,
) error {
	key := imageUpkeepClearKey(task)
	if !w.holdPace.due(key, publicationAttemptPaceState, w.now().UTC()) {
		return nil
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ClearRunHoldCause(ctx, task.RunID, domain.HoldImageUpkeepPending)
	}); err != nil {
		w.holdPace.forget(key)
		return err
	}
	return nil
}

// waitsOnImage reports whether a reconcile failure is a wait on a project
// image (recordRetryableHold), which keeps the image_upkeep_pending hold.
func waitsOnImage(err error) bool {
	return productionPublicationRetryableFailure(err) &&
		(errors.Is(err, ErrImageUpkeepPending) || errors.Is(err, errProjectImageBuilderBusy))
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

// blockedHoldReadyReason concludes a blocked hold the run's ready item
// replaces.
const blockedHoldReadyReason = "Superseded when the run became ready for final review."

// supersedeBlockedHold supersedes the run's open blocked hold, if any, with
// reason as its conclusion and successor naming the item that replaces it.
func (w *productionPublicationWorkflow) supersedeBlockedHold(
	ctx context.Context,
	task productionPublicationTask,
	reason string,
	successor *domain.ItemID,
) error {
	var item domain.AttentionItem
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(ctx, task.blockedItemID())
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
	item.ItemVersion++
	item, err = item.ConcludeByDaemon(domain.ConcludedBySupersession, w.now().UTC(), reason, successor)
	if err != nil {
		return err
	}
	return w.attention.PutItem(ctx, item)
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
		body, err := loadArtifactBlob(w.artifacts, *replay.CommitPlanDigest)
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
	body, err := loadArtifactBlob(w.artifacts, digest)
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

// verificationRoomAndRecipe constructs the networkless room for one cycle's
// verification from the image the binding verifies in, and reads that image's
// recipe through it.
func (w *productionPublicationWorkflow) verificationRoomAndRecipe(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) (ProductionVerificationRoom, []byte, error) {
	return w.roomAndRecipe(ctx, task, binding, binding.verificationImage())
}

// reserveVerification reserves the verification job's memory before its
// first container, the recipe read (plan §5.4). A job takes no pool slot, so
// the memory gate is its whole scheduling decision. One that does not fit yet
// records its wait and hold and returns a retryable error, so the task stays
// pending and a later pass asks again; a grant ends that hold. One the budget
// refuses for good is retryable too, so the job waits under the notice
// recordRetryableHold raises.
func (w *productionPublicationWorkflow) reserveVerification(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) error {
	// Read before the gate's write, so an unbudgeted job writes nothing. No
	// command clears a recorded budget, so a budget read here still applies
	// in the gate. The gate reads the budget again in its own transaction, so
	// one lowered between the two reads still bounds this reservation.
	bound, err := currentLaunchBound(ctx, w.store, w.launchBound)
	if err != nil || !bound.Budgeted() {
		return err
	}
	sizes, err := ward.ResolveLaunchSizes(binding.resolvedPolicy.Keys, ward.LaunchBound{})
	if err != nil {
		return fmt.Errorf("resolve verification size: %w", err)
	}
	id := task.verificationInvocationID()
	now := w.now().UTC()
	reservation, _, err := bound.LaunchReservation(
		id, task.RunID, domain.ReservationVerification, sizes.Verification)
	var waited bool
	if err == nil {
		err = w.store.Write(ctx, func(tx *store.WriteTx) error {
			if err := requireTaskExecutionOpen(ctx, &tx.ReadTx, task.RunID); err != nil {
				return err
			}
			gate, err := launchBoundAt(ctx, &tx.ReadTx, w.launchBound)
			if err != nil {
				return err
			}
			if err := tx.RequireMachineCapacity(ctx, gate.BudgetMiB, reservation, now); err != nil {
				return err
			}
			hold, present, err := tx.GetRunHold(ctx, task.RunID)
			if err != nil {
				return err
			}
			waited = present && hold.Reason == domain.HoldMachineCapacity
			return tx.ClearRunHoldCause(ctx, task.RunID, domain.HoldMachineCapacity)
		})
	}
	if errors.Is(err, domain.ErrMachineCapacity) {
		if orderErr := recordLaunchOrder(ctx, w.store, w.attention, task.RunID, id, err, now); orderErr != nil {
			return errors.Join(err, orderErr)
		}
		return productionPublicationRetryableError(fmt.Errorf("reserve verification memory: %w", err))
	}
	if launchMemoryRefusal(err) {
		return productionPublicationRetryableError(fmt.Errorf("reserve verification memory: %w", err))
	}
	if err != nil {
		return err
	}
	if waited {
		w.holdPace.forget("hold:" + string(task.RunID))
	}
	return releaseProductionQuarantine(ctx, w.store, w.attention, launchMemoryRefusedPrefix, task.RunID)
}

// releaseVerification ends the verification job's reservation once its room
// returned with every container proved absent, which is any return but one
// marked ward.ErrVerificationAbsenceUnproven. An unproven one keeps the
// reservation until a later pass's room, or a confirmed task stop, proves it.
func (w *productionPublicationWorkflow) releaseVerification(
	ctx context.Context, task productionPublicationTask, roomErr error,
) error {
	if errors.Is(roomErr, ward.ErrVerificationAbsenceUnproven) {
		return nil
	}
	bound, err := currentLaunchBound(ctx, w.store, w.launchBound)
	if err != nil || !bound.Budgeted() {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ReleaseVerificationReservation(ctx, task.verificationInvocationID(), w.now().UTC())
	})
}

// roomAndRecipe constructs a networkless room from image and reads the
// image's recipe through it, refusing a recipe that is not the one the
// admitted image was built with.
func (w *productionPublicationWorkflow) roomAndRecipe(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
	image domain.ProjectImage,
) (ProductionVerificationRoom, []byte, error) {
	// The writer start already resolved this durable policy, so a failure
	// here means the stored policy changed under the run.
	sizes, err := ward.ResolveLaunchSizes(binding.resolvedPolicy.Keys, ward.LaunchBound{})
	if err != nil {
		return nil, nil, fmt.Errorf("resolve verification size: %w", err)
	}
	var room ProductionVerificationRoom
	if w.newBoundRoom != nil {
		room, err = w.newBoundRoom(image, sizes.Verification, binding.run, task.verificationInvocationID())
	} else {
		room, err = w.newRoom(image, sizes.Verification)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("construct networkless verification room: %w", err)
	}
	readCtx, cancelRead := context.WithTimeout(ctx, w.recipeReadTimeout)
	recipe, err := room.ReadRecipe(readCtx)
	cancelRead()
	if err != nil {
		return nil, nil, fmt.Errorf(
			"load project-image verification recipe: %w",
			productionPublicationRetryableError(err),
		)
	}
	if len(recipe) > verify.DefaultMaxRecipeBytes {
		return nil, nil, fmt.Errorf(
			"project-image verification recipe exceeds the %d-byte cap",
			verify.DefaultMaxRecipeBytes,
		)
	}
	if got := verify.RecipeDigest(recipe); got != binding.image.RecipeDigest {
		return nil, nil, fmt.Errorf(
			"project-image verification recipe digest %s, want %s: %w",
			got, binding.image.RecipeDigest, domain.ErrParentKeyMismatch,
		)
	}
	return room, recipe, nil
}

// storeVerificationEvidence writes each evidence blob to the artifact store
// and reads it back, so a checkpoint never names evidence the store lacks.
func (w *productionPublicationWorkflow) storeVerificationEvidence(
	evidence []verify.Evidence,
) ([]domain.Artifact, error) {
	artifacts := make([]domain.Artifact, len(evidence))
	for index, item := range evidence {
		artifacts[index] = item.Artifact
		if _, err := w.artifacts.Put(item.Artifact.Digest, bytes.NewReader(item.Content)); err != nil {
			return nil, err
		}
		if err := verifyArtifactBlob(w.artifacts, item.Artifact); err != nil {
			return nil, err
		}
	}
	return artifacts, nil
}

func (w *productionPublicationWorkflow) verifyAndCheckpoint(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	imported importer.Result,
	checkoutDir string,
) (productionVerificationCheckpoint, error) {
	repair, err := w.repairVerification(ctx, binding, binding.admission.Base.BaseSHA)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	var refusal *rebuildRefusal
	if repair {
		// A maintenance task's image is the one its refusal says does not
		// serve the base; the candidate is verified only in an image rebuilt
		// for its head.
		binding, refusal, err = w.bindRepairVerificationImage(ctx, task, binding, checkoutDir, imported.CommitSHA)
	} else {
		// Composition admitted this image at this base before the run started,
		// so a refusal here is a disagreement with durable authority, not a
		// hold.
		if err := requireProjectImageServesBase(
			ctx, binding.image, checkoutDir, binding.admission.Base.BaseSHA, w.preparationDigest,
		); err != nil {
			if errors.Is(err, domain.ErrProjectImageIncompatible) {
				err = fmt.Errorf("production publication binding disagrees with durable authority: %w",
					errors.Join(domain.ErrParentKeyMismatch, err))
			}
			return productionVerificationCheckpoint{}, err
		}
		binding, refusal, err = w.bindVerificationImage(
			ctx, task, binding, checkoutDir, binding.admission.Base.BaseSHA, imported.CommitSHA)
	}
	if err != nil {
		// A rebuilt image the run is bound to is durable authority as the
		// admitted one is, so its refusal is the same disagreement.
		if errors.Is(err, domain.ErrProjectImageIncompatible) {
			err = fmt.Errorf("production publication binding disagrees with durable authority: %w",
				errors.Join(domain.ErrParentKeyMismatch, err))
		}
		return productionVerificationCheckpoint{}, err
	}
	recipes, err := w.recipes(ctx)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	var verified verify.Result
	if refusal != nil {
		// No room is built for a refused gate. The refusal is recorded as this
		// candidate's failed verification, so the block, its recovery, and a
		// rerun are the ones every failed verification already has.
		verified, err = refusedRebuildVerification(task, binding, imported.CommitSHA, *refusal, recipes)
		if err != nil {
			return productionVerificationCheckpoint{}, err
		}
	} else {
		if err := w.reserveVerification(ctx, task, binding); err != nil {
			return productionVerificationCheckpoint{}, err
		}
		room, recipe, err := w.verificationRoomAndRecipe(ctx, task, binding)
		if err != nil {
			return productionVerificationCheckpoint{}, errors.Join(err, w.releaseVerification(ctx, task, err))
		}
		verified, err = verify.Verify(ctx, checkoutDir, verify.Options{
			HeadSHA: imported.CommitSHA, BaseSHA: binding.admission.Base.BaseSHA,
			InvocationID: task.verificationInvocationID(), RecipeSource: verify.ConfigRecipe(recipe),
			RecipePath: verify.DefaultRecipePath, Room: room,
			ApprovedRecipes: recipes, Changes: imported.Changes,
			// A stable per-verification time, not a wall clock: the emitted evidence
			// is content-addressed and write-once, and this checkpoint can re-run, so
			// the evidence metadata must converge byte-identically on replay (mirrors
			// the checkpoint's CandidateAuthorization CreatedAt).
			Now: binding.export.RecordedAt,
			Policy: verify.Policy{ExtraVerificationControlPatterns: slices.Clone(
				binding.profile.ProtectedPaths.ExtraVerificationControlPatterns,
			)},
		})
		if releaseErr := w.releaseVerification(ctx, task, err); releaseErr != nil {
			return productionVerificationCheckpoint{}, errors.Join(err, releaseErr)
		}
		if err != nil {
			if !productionVerificationStateContradiction(err) && !verificationMemoryLimit(err) {
				err = productionPublicationRetryableError(err)
			}
			return productionVerificationCheckpoint{}, fmt.Errorf("clean production verification: %w", err)
		}
	}
	if verified.HeadSHA != imported.CommitSHA || verified.RecipeDigest != binding.image.RecipeDigest {
		return productionVerificationCheckpoint{}, fmt.Errorf("verification disagrees with project-image binding: %w",
			domain.ErrParentKeyMismatch)
	}
	artifacts, err := w.storeVerificationEvidence(verified.Evidence)
	if err != nil {
		return productionVerificationCheckpoint{}, err
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
		InvocationID:       task.verificationInvocationID(), CreatedAt: binding.export.RecordedAt,
	})
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	diffStats, err := deriveDiffStats(
		ctx, w.workDir, checkoutDir, authorization.BaseSHA, authorization.HeadSHA,
	)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	checkpoint := productionVerificationCheckpoint{
		Version: productionVerificationVersion,
		TaskKey: task.intentKey(), HeadSHA: task.HeadSHA,
		ProjectImage: binding.verificationImage().ID,
		Imported:     imported, Authorization: authorization, Artifacts: artifacts,
		DiffStats: diffStats,
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

// verificationMemoryLimit reports a verification room killed at its declared
// memory limit. The room runs at the size the run's durable policy resolved,
// so a retry meets the same limit: the caller ends the cycle on it instead of
// holding for a retry. A larger size is the capacity card's (#1598).
func verificationMemoryLimit(err error) bool {
	var limit *ward.MemoryLimitError
	return errors.As(err, &limit) && limit.Class == ward.LaunchVerification
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
			ctx, task.verificationCheckpointKey(),
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
		entry, err = tx.GetInbox(ctx, task.verificationCheckpointKey())
		if errors.Is(err, store.ErrNotFound) &&
			task.reevaluation == nil &&
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
	authorizingTaskKey := task.intentKey()
	if task.AuthorizingTaskKey != "" {
		authorizingTaskKey = task.AuthorizingTaskKey
	}
	if !versionMatches ||
		checkpoint.TaskKey != authorizingTaskKey ||
		checkpoint.ProjectImage != binding.verificationImage().ID || authorization.Validate() != nil ||
		authorization.Repo != binding.admission.Base.Repo ||
		authorization.BaseSHA != binding.admission.Base.BaseSHA ||
		authorization.HeadSHA != task.HeadSHA ||
		authorization.ImportResultDigest != importDigest ||
		authorization.VerificationRecipeDigest != binding.image.RecipeDigest ||
		authorization.EvidenceSnapshotDigest != evidenceDigest ||
		authorization.TrustProfileDigest != binding.profile.ProfileDigest ||
		authorization.InvocationID != task.verificationInvocationID() {
		return productionVerificationCheckpoint{}, false, fmt.Errorf("production verification checkpoint disagrees with task: %w",
			domain.ErrParentKeyMismatch)
	}
	if checkpoint.DiffStats != nil {
		if err := checkpoint.DiffStats.Validate(); err != nil ||
			checkpoint.DiffStats.BaseSHA != authorization.BaseSHA ||
			checkpoint.DiffStats.HeadSHA != authorization.HeadSHA {
			return productionVerificationCheckpoint{}, false, fmt.Errorf(
				"production verification checkpoint diff stats disagree with authorization: %w",
				domain.ErrParentKeyMismatch,
			)
		}
	}
	for _, artifact := range checkpoint.Artifacts {
		if err := verifyArtifactBlob(w.artifacts, artifact); err != nil {
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
//
// checkoutDir is a checkout holding the request's base. Only the re-entry
// branch reads it, to rebuild the tree its checkpoint must name.
func (w *productionPublicationWorkflow) loadRemediationSourceTree(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkoutDir string,
) (string, error) {
	if binding.remediation == nil {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	request := binding.remediation.request
	round, ok := remediationRoundForInvocation(task.RunID, task.ProducingInvocationID)
	if !ok || request.Round != round {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	if task.Successor != nil && remediatesExternalFindings(*task.Successor, request.Round) {
		return w.loadReentryRemediationSourceTree(ctx, task, binding, request, checkoutDir)
	}
	if task.Successor != nil && task.Successor.AdmitsHandoffConflictRemediation(domain.RemediationInvocationIntent(request)) {
		// A handoff conflict's first remediation rebuilt the candidate on the
		// sealed tip, which the checkout holds as its base. It changed nothing
		// when its tree is the tip's: the head would propose no change.
		return w.reentryTreeSHA(ctx, checkoutDir, request.BaseSHA)
	}
	var (
		entry  store.QueueEntry
		legacy bool
	)
	sourceTaskKey := task.intentKey()
	sourceCheckpointKey := productionVerificationCheckpointKey(task.RunID, request.HeadSHA, "")
	var continuationVerification domain.InvocationID
	if task.Successor != nil && task.Successor.EffectiveOrigin() == domain.PublicationSuccessorRemediation {
		sourceCheckpointKey += "/continuation/" + task.Successor.CommandID
		if request.Round == task.Successor.ReviewRound-1 {
			if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
				r, _, err := tx.PublicationContinuationIntent(ctx, task.RunID, task.Successor.CommandID)
				if err != nil {
					return err
				}
				row, err := tx.GetOutbox(ctx, r.SourceTaskKey)
				if err != nil {
					return err
				}
				source, err := decodeProductionPublicationTask(row)
				if err != nil {
					return err
				}
				sourceTaskKey = signet.PublicationReevaluationKey(task.RunID, task.Successor.ReevaluationCommandID)
				sourceCheckpointKey = productionVerificationCheckpointKey(task.RunID, request.HeadSHA, task.Successor.ReevaluationCommandID)
				continuationVerification = domain.InvocationID(string(source.VerificationID) + "/reevaluation/" + task.Successor.ReevaluationCommandID)
				return nil
			}); err != nil {
				return "", errors.Join(errRemediationSourceIdentity, err)
			}
		}
	}
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetInbox(ctx, sourceCheckpointKey)
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
	sourceVerificationValid := validRemediationSourceVerificationID(task.RunID, round, authorization.InvocationID)
	if continuationVerification != "" {
		sourceVerificationValid = authorization.InvocationID == continuationVerification
	}
	if task.Successor != nil && authorization.InvocationID == productionVerificationInvocationIDForProducer(task.RunID, task.Successor.FeedbackInvocationID) {
		sourceVerificationValid = true
	}
	if !versionMatches || checkpoint.TaskKey != sourceTaskKey ||
		checkpoint.Imported.CommitSHA != request.HeadSHA ||
		!validCommitSHA(checkpoint.Imported.TreeSHA) || !importer.AllAdvisory(checkpoint.Imported.Findings) ||
		authorization.Validate() != nil ||
		authorization.Repo != binding.admission.Base.Repo ||
		authorization.BaseSHA != request.BaseSHA || authorization.HeadSHA != request.HeadSHA ||
		authorization.ImportResultDigest != importDigest ||
		authorization.EvidenceSnapshotDigest != evidenceDigest ||
		authorization.VerificationOutcome != domain.VerificationPassed ||
		!sourceVerificationValid ||
		!reflect.DeepEqual(storedAuthorization, authorization) ||
		sourceImage.ID != checkpoint.ProjectImage || sourceImage.Repository != authorization.Repo ||
		sourceImage.RepositoryID != binding.admission.Base.RepositoryID ||
		sourceImage.RecipeDigest != authorization.VerificationRecipeDigest {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	for _, artifact := range checkpoint.Artifacts {
		if err := verifyArtifactBlob(w.artifacts, artifact); err != nil {
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

// loadReentryRemediationSourceTree is loadRemediationSourceTree for the
// remediation an external review cycle's first round started. That cycle
// verified its head as a re-entered cycle: under its own checkpoint kind,
// with no import account and no stored authorization. The tree the
// remediator's import is compared with is the one that checkpoint recorded,
// re-bound to the request and the run the way the ordinary checkpoint is.
//
// The ordinary checkpoint's outcome and tree are bound through its stored
// authorization. This one has none, so the row is trusted for neither. The
// outcome is read back from the cycle's verification report. The tree is held
// to the remediation input the request authenticates: it carries the
// base-to-head patch the remediator starts from, derived apart from the
// checkpoint, and the tree that patch rebuilds on the base in checkoutDir
// must be the recorded one.
func (w *productionPublicationWorkflow) loadReentryRemediationSourceTree(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	request remediationInvocationRequest,
	checkoutDir string,
) (string, error) {
	// The re-entered task's key: the cycle's head under the cycle's
	// publication, which the remediated task shares.
	key := reentryCheckpointKey(task.RunID, request.HeadSHA, task.Successor.PublicationID())
	var entry store.QueueEntry
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetInbox(ctx, key)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	if err != nil {
		return "", remediationSourceReadError(ctx, err)
	}
	if entry.Kind != productionReentryCheckpointKind {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	var checkpoint productionReentryCheckpoint
	if err := strictjson.Decode(
		entry.Payload, &checkpoint, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return "", fmt.Errorf("decode re-entry remediation source checkpoint: %w",
			errors.Join(errRemediationSourceIdentity, err, domain.ErrParentKeyMismatch))
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(checkpoint.Artifacts)
	if err != nil {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	// An external review cycle evaluates its head itself, never a merge, so
	// the recorded tree is the head's. A checkpoint from before the tree was
	// recorded has no source identity to offer.
	if checkpoint.Version != productionReentryCheckpointVersion ||
		checkpoint.TaskKey != task.intentKey() ||
		checkpoint.BaseSHA != request.BaseSHA || checkpoint.HeadSHA != request.HeadSHA ||
		checkpoint.EvaluatedSHA != "" || !checkpoint.clean() ||
		!validCommitSHA(checkpoint.TreeSHA) ||
		checkpoint.EvidenceSnapshotDigest != evidenceDigest {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	// The checkpoint is held to the image the cycle verified its head under,
	// as loadReentryCheckpoint holds it: the admitted image, or the rebuild
	// the run bound to that head (production_rebuild.go), which keeps the
	// admitted recipe. It is never held to an image it names itself: another
	// registered image of the same repository, with its own recipe, is
	// self-consistent and still not the one the cycle verified under.
	source := binding
	if err := w.resolveVerificationImage(ctx, &source, request.HeadSHA); err != nil {
		if errors.Is(err, domain.ErrParentKeyMismatch) {
			return "", errors.Join(errRemediationSourceIdentity, err)
		}
		return "", remediationSourceReadError(ctx, err)
	}
	if checkpoint.ProjectImage != source.verificationImage().ID ||
		checkpoint.RecipeDigest != binding.image.RecipeDigest {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	for _, artifact := range checkpoint.Artifacts {
		if err := verifyArtifactBlob(w.artifacts, artifact); err != nil {
			return "", fmt.Errorf("verify re-entry remediation source checkpoint artifact: %w",
				retryableOrTerminal(ctx, err, errRemediationSourceIdentity))
		}
	}
	// The clean outcome is the verifier's to state, as loadReentryCheckpoint
	// holds it: read back from the cycle's own report, not from the row.
	raw, err := loadVerificationReport(
		w.artifacts, checkpoint.Artifacts, reentryVerificationInvocationID(*task.Successor))
	if err != nil {
		return "", fmt.Errorf("load re-entry remediation source report: %w",
			retryableOrTerminal(ctx, err, errRemediationSourceIdentity))
	}
	report, err := verify.ParseReport(raw)
	if err != nil || !checkpoint.reportAgrees(report, request.BaseSHA, request.HeadSHA, "") {
		return "", errors.Join(errRemediationSourceIdentity, err, domain.ErrParentKeyMismatch)
	}
	input, _, err := loadRemediationInput(w.artifacts, request.InputArtifactDigest)
	if err != nil {
		return "", fmt.Errorf("load re-entry remediation source input: %w",
			retryableOrTerminal(ctx, err, errRemediationSourceIdentity))
	}
	if input.BaseSHA != request.BaseSHA || input.HeadSHA != request.HeadSHA {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	scratch, err := os.MkdirTemp(w.workDir, ".remediation-source-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	_, rebuilt, err := patchedTree(ctx, scratch, checkoutDir, request.BaseSHA, input.CandidatePatchBase64)
	if err != nil {
		return "", fmt.Errorf("rebuild re-entry remediation source tree: %w",
			retryableOrTerminal(ctx, err, errRemediationSourceIdentity))
	}
	if rebuilt != checkpoint.TreeSHA {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	return checkpoint.TreeSHA, nil
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

// productionCandidate composes the publication candidate. Its title and body
// come from publicationTitleBody, which renders v1 for a v1 or literal record
// and the stored authored artifact for a v2 record (falling back to v1 on any
// read or gate failure). Every publication path (forward publish, crash
// recovery, and drift repair through the candidate this returns) runs through
// here, so a given candidate replays byte-identical metadata.
func (w *productionPublicationWorkflow) productionCandidate(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	adoptedProfile *domain.Digest,
	dispositionHistory *publish.DispositionHistory,
	report []byte,
) (publish.Candidate, error) {
	title, body, err := w.publicationTitleBody(ctx, task, binding, checkpoint)
	if err != nil {
		return publish.Candidate{}, err
	}
	recipe := binding.image.RecipeDigest
	authorization := checkpoint.Authorization.ID
	profile := binding.profile.ProfileDigest
	candidate := publish.Candidate{
		Repo: binding.admission.Base.Repo, BaseRef: binding.admission.Base.BaseRef,
		BaseSHA: binding.admission.Base.BaseSHA,
		HeadSHA: task.HeadSHA, Title: title, Branch: task.Publication.Branch,
		Body: body, DispositionHistory: dispositionHistory,
		VerificationReport: report, ImportResult: &checkpoint.Imported,
		ScopeDecision: task.scopeDecision,
		Advisories:    publish.AdvisoryFindings(checkpoint.Authorization.Findings),
		Artifacts:     checkpoint.Artifacts, RecipeDigest: &recipe,
		InvocationID: task.PublicationID, RunID: task.RunID,
		AuthorizationID: &authorization, TrustProfileDigest: &profile,
		AdoptedTrustProfileDigest: adoptedProfile,
	}
	// Hand the publisher the source-issue reference for this candidate. Every
	// publication path (forward, recovery, drift repair) composes through here,
	// so the same reference replays byte-identically; a v1 record keeps its
	// prose "Source issue:" line and sets nothing here (issue #1419 Part D).
	if err := w.applyClosureReference(ctx, task, &candidate); err != nil {
		return publish.Candidate{}, err
	}
	return candidate, nil
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
	ctx context.Context,
	task productionPublicationTask,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
	readiness productionReadiness,
	yieldHistory domain.ReviewYieldHistory,
) (domain.AttentionItem, error) {
	recipes, err := w.recipes(ctx)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	return w.readyItemWithRecipes(
		ctx, task, checkpoint, published, readiness, yieldHistory, recipes,
	)
}

func (w *productionPublicationWorkflow) readyItemWithRecipes(
	ctx context.Context,
	task productionPublicationTask,
	checkpoint productionVerificationCheckpoint,
	published publish.Result,
	readiness productionReadiness,
	yieldHistory domain.ReviewYieldHistory,
	approvedRecipes map[domain.Digest]bool,
) (domain.AttentionItem, error) {
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID}
	names, err := displayNames(ctx, w.store, task.ProjectID, subject)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	actions := []domain.Action{
		domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionMarkSeen,
		domain.ActionDismiss, domain.ActionStop,
	}
	if task.reentersInPlace() {
		// A re-entered cycle imported nothing, so it holds no candidate to
		// hand back to the implementation agent (issue #502). The action is
		// withheld rather than refused later: accepting a decision supersedes
		// the item, which would spend the re-earned readiness on nothing.
		actions = []domain.Action{
			domain.ActionOpenPR, domain.ActionMarkSeen, domain.ActionDismiss, domain.ActionStop,
		}
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: task.readyItemID(), ProjectID: task.ProjectID,
		Subject: subject,
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: fmt.Sprintf("Published %s#%d and completed production verification.",
			checkpoint.Authorization.Repo, published.PRNumber),
		RequestedDecision: actions,
		EvidenceSnapshot:  checkpoint.Artifacts,
		AgentClaims: normalizeSummaryClaims(
			checkpoint.Imported.Claims, task.ProducingInvocationID),
		PRHeadSHA: checkpoint.Imported.CommitSHA,
		PRReference: &domain.PRReference{
			Repo: checkpoint.Authorization.Repo, Number: published.PRNumber,
		},
		Readiness: &domain.ReadinessSummary{
			Class: readiness.verdict.Class, EvaluationSetDigest: readiness.verdict.EvaluationSetDigest,
		},
		ReadinessDetail:  &readiness.detail,
		YieldHistory:     &yieldHistory,
		DiffStats:        checkpoint.DiffStats,
		DisplayNames:     names,
		CommitPlanNotice: checkpoint.Imported.CommitPlanNotice,
		ScopeDecision:    task.scopeDecision,
		ItemVersion:      1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &createdAt,
		Status:    domain.StatusOpen,
	}, approvedRecipes)
}

func (w *productionPublicationWorkflow) blockedItemWithActionsAndRecipes(
	ctx context.Context,
	task productionPublicationTask,
	imported importer.Result,
	artifacts []domain.Artifact,
	reason string,
	actions []domain.Action,
	approvedRecipes map[domain.Digest]bool,
	facts *domain.PublishBlockFacts,
) (domain.AttentionItem, error) {
	return w.newBlockedItem(
		ctx, task, imported, artifacts, reason, actions, approvedRecipes, facts, nil,
	)
}

func (w *productionPublicationWorkflow) blockedItem(
	ctx context.Context,
	task productionPublicationTask,
	imported importer.Result,
	artifacts []domain.Artifact,
	reason string,
) (domain.AttentionItem, error) {
	recipes, err := w.recipes(ctx)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	return w.blockedItemWithActionsAndRecipes(
		ctx, task, imported, artifacts, reason,
		productionShadowStopBlockActions, recipes, nil,
	)
}

var (
	productionRerunnableBlockActions = []domain.Action{
		domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
	}
	productionShadowStopBlockActions = []domain.Action{
		domain.ActionInspectTrustFailure, domain.ActionStop,
	}
)

func (w *productionPublicationWorkflow) blockedHoldItem(
	ctx context.Context,
	task productionPublicationTask,
	imported importer.Result,
	reason string,
	cause domain.RunHoldReason,
	pull *recordedPullRequest,
) (domain.AttentionItem, error) {
	// open_pr is offered only with the reference it navigates to; signet
	// recognizes a hold by exactly these two decision lists
	// (signet.PublicationHoldDecision).
	actions := []domain.Action{domain.ActionInspectTrustFailure}
	var reference *domain.PRReference
	if pull != nil {
		actions = append(actions, domain.ActionOpenPR)
		value := pull.reference()
		reference = &value
	}
	return w.newBlockedItem(
		ctx, task, imported, nil, reason, actions,
		w.operatorRecipes, publishBlockHoldReason(cause), reference,
	)
}

func (w *productionPublicationWorkflow) newBlockedItem(
	ctx context.Context,
	task productionPublicationTask,
	imported importer.Result,
	artifacts []domain.Artifact,
	reason string,
	actions []domain.Action,
	approvedRecipes map[domain.Digest]bool,
	facts *domain.PublishBlockFacts,
	reference *domain.PRReference,
) (domain.AttentionItem, error) {
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID}
	names, err := displayNames(ctx, w.store, task.ProjectID, subject)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: task.blockedItemID(), ProjectID: task.ProjectID,
		Subject: subject,
		Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
		Reason:            reason,
		RequestedDecision: actions,
		EvidenceSnapshot:  artifacts,
		AgentClaims:       normalizeSummaryClaims(imported.Claims, task.ProducingInvocationID),
		PRHeadSHA:         imported.CommitSHA,
		PRReference:       reference,
		CommitPlanNotice:  imported.CommitPlanNotice,
		PublishBlock:      facts,
		DisplayNames:      names,
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
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
		current, err = tx.GetAttentionItemRecord(ctx, task.blockedItemID())
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if signet.PublicationHoldDecision(current) {
		return nil, nil
	}
	if !slices.Equal(current.RequestedDecision, productionRerunnableBlockActions) &&
		!slices.Equal(current.RequestedDecision, productionShadowStopBlockActions) {
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
		productionBlockHandoffConflict,
		productionBlockHandoffVerification,
		productionBlockReentryRetired,
		productionBlockReentryConflict,
		productionBlockReentryVerification,
		productionBlockReentryRoundLimit,
		productionBlockMemoryLimit,
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
	case productionBlockReentryRetired:
		// A retired re-entry is ended before any checkpoint is read, with no
		// evidence.
	case productionBlockHandoffVerification:
		if !checkpointFound || !checkpoint.Authorization.AuthorizesPublication {
			return nil, fmt.Errorf("production blocked item %q has an impossible handoff reason: %w",
				current.ID, domain.ErrParentKeyMismatch)
		}
		handoffArtifacts, err := w.handoffBlockArtifacts(ctx, task, binding)
		if err != nil {
			return nil, err
		}
		artifacts = handoffArtifacts
	case productionBlockTrust, productionBlockBaseAdvanced, productionBlockHandoffConflict,
		productionBlockReentryConflict, productionBlockReentryVerification, productionBlockReentryRoundLimit:
		if !checkpointFound || !checkpoint.Authorization.AuthorizesPublication {
			return nil, fmt.Errorf("production blocked item %q has an impossible publication reason: %w",
				current.ID, domain.ErrParentKeyMismatch)
		}
		artifacts = checkpoint.Artifacts
	case productionBlockMemoryLimit:
		// A first cycle's killed verification records no checkpoint and no
		// evidence. An unpublished re-entry's carries the authorizing
		// checkpoint it re-verified, like its other base-advance blocks.
		if checkpointFound {
			if !checkpoint.Authorization.AuthorizesPublication {
				return nil, fmt.Errorf("production blocked item %q has an impossible memory-limit reason: %w",
					current.ID, domain.ErrParentKeyMismatch)
			}
			artifacts = checkpoint.Artifacts
		}
	default:
		// The known-reason gate above keeps this switch exhaustive if another
		// definitive reason is added without its own reconstruction contract.
		return nil, fmt.Errorf("production blocked item %q has no reason contract: %w",
			current.ID, domain.ErrParentKeyMismatch)
	}
	historicalRecipes, err := w.recipes(ctx)
	if err != nil {
		return nil, err
	}
	historicalRecipes[binding.image.RecipeDigest] = true
	facts, err := definitivePublishBlockFacts(current.Reason)
	if err != nil {
		return nil, fmt.Errorf("production blocked item %q has no fact contract: %w", current.ID, err)
	}
	expected, err := w.blockedItemWithActionsAndRecipes(
		ctx, task, imported, artifacts, current.Reason, current.RequestedDecision, historicalRecipes,
		facts,
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
	accepted, err := w.recordCompletedTerminalAtBoundary(
		ctx, binding.run, task, signet.PublicationReevaluationBlocked)
	if err != nil {
		return nil, err
	}
	if err := w.finishTask(ctx, task); err != nil {
		return nil, err
	}
	return &productionTaskOutcome{completed: true, accepted: accepted, blocked: true}, nil
}

// closureBlockedReason is the closed card's sentence for a closure proposal
// whose publication completed blocked, a definitive trust refusal included.
const closureBlockedReason = "Superseded because the publication was blocked; a rerun that publishes proposes closure again."

// retireBlockedClosureProposals retires the run's open closure card when its
// publication completes blocked: the card names a merge that will not happen.
// A rerun reuses the publication id, and binding its merge opens a fresh card
// because none is left open.
func (w *productionPublicationWorkflow) retireBlockedClosureProposals(ctx context.Context, runID domain.RunID) error {
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.RetireOpenClosureProposals(ctx, runID, closureBlockedReason, w.now().UTC())
		return err
	})
}

func (w *productionPublicationWorkflow) completeBlockedTask(
	ctx context.Context,
	task productionPublicationTask,
	run domain.Run,
	imported importer.Result,
	artifacts []domain.Artifact,
	reason string,
	actions []domain.Action,
	facts *domain.PublishBlockFacts,
) (productionTaskOutcome, error) {
	recipes, err := w.recipes(ctx)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	item, err := w.newBlockedItem(ctx, task, imported, artifacts, reason, actions, recipes, facts, nil)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.retireBlockedClosureProposals(ctx, task.RunID); err != nil {
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
	accepted, err := w.recordCompletedTerminalAtBoundary(
		ctx, run, task, signet.PublicationReevaluationBlocked)
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
	if item.Subject.TaskID == nil {
		item.Subject.TaskID = current.Subject.TaskID
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
	completion *signet.PublicationReevaluationCompletion,
) (bool, error) {
	stage, ok := productionStageForInvocation(run, task.ProducingInvocationID)
	if !ok {
		return false, domain.ErrParentKeyMismatch
	}
	return (&Engine{store: w.store}).recordProductionTerminalWithCompletion(ctx, run, productionTerminalRecord{
		InvocationID: task.ProducingInvocationID, RunID: task.RunID,
		StageID: stage.ID, Status: exec.StatusCompleted,
		HeadSHA: task.HeadSHA, Artifacts: slices.Clone(task.Artifacts), Summary: task.Summary,
	}, false, completion)
}

func (w *productionPublicationWorkflow) recordCompletedTerminalAtBoundary(
	ctx context.Context,
	run domain.Run,
	task productionPublicationTask,
	outcome signet.PublicationReevaluationOutcome,
) (bool, error) {
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionTerminalCompletion, DurableTransitionBefore); err != nil {
		return false, err
	}
	completion, err := w.publicationReevaluationCompletion(
		ctx, task, outcome)
	if err != nil {
		return false, err
	}
	accepted, err := w.recordCompletedTerminal(ctx, run, task, completion)
	if err != nil {
		return false, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionTerminalCompletion, DurableTransitionAfter); err != nil {
		return false, err
	}
	return accepted, nil
}

func (w *productionPublicationWorkflow) publicationReevaluationCompletion(
	ctx context.Context,
	task productionPublicationTask,
	outcome signet.PublicationReevaluationOutcome,
) (*signet.PublicationReevaluationCompletion, error) {
	if task.reevaluation == nil {
		return nil, nil
	}
	evidenceItemID, err := w.reevaluationSourceItemID(ctx, task)
	if err != nil {
		return nil, err
	}
	var item domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(ctx, evidenceItemID)
		return err
	}); err != nil {
		return nil, err
	}
	completion := &signet.PublicationReevaluationCompletion{
		RunID: task.RunID, CommandID: task.reevaluation.CommandID,
		IntentKey: task.reevaluation.IntentKey, Outcome: outcome,
		PRHeadSHA: task.HeadSHA, EvidenceItemID: item.ID,
		EvidenceItemVersion: item.ItemVersion,
		TerminalInvocationID: signet.PublicationReevaluationTerminalInvocationID(
			task.reevaluation.CommandID),
	}
	if _, err := signet.EncodePublicationReevaluationCompletion(*completion); err != nil {
		return nil, err
	}
	return completion, nil
}

func (w *productionPublicationWorkflow) reevaluationSourceItemID(
	ctx context.Context, task productionPublicationTask,
) (domain.ItemID, error) {
	if task.reevaluation == nil {
		return "", nil
	}
	var entry store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, task.reevaluation.IntentKey)
		return err
	}); err != nil {
		return "", err
	}
	request, err := signet.DecodePublicationReevaluationRequest(entry.Payload)
	if err != nil || request.CommandID != task.reevaluation.CommandID || request.RunID != task.RunID {
		return "", errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return request.ItemID, nil
}

func (w *productionPublicationWorkflow) finishTask(
	ctx context.Context, task productionPublicationTask,
) error {
	return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, task.intentKey())
	})
}
