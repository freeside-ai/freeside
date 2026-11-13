package engine

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
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
	// ProveVerificationAbsent proves every container the invocation's bound
	// rooms started, in every pass, is absent and its command joined, and
	// marks a failure to prove with ward.ErrVerificationAbsenceUnproven. The
	// verification reservation is released only on its proof. Required
	// whenever NewBoundRoom is set.
	ProveVerificationAbsent func(context.Context, domain.Run, domain.InvocationID) error
	// ProveVerificationContainersAbsent is ProveVerificationAbsent without
	// the joined-command condition. Only the operator-confirmed release
	// (Engine.ReleaseVerificationMemory with commandGone) calls it; every
	// automatic release keeps the strict proof. Required whenever
	// NewBoundRoom is set.
	ProveVerificationContainersAbsent func(context.Context, domain.Run, domain.InvocationID) error
	// RebuildProjectImage lets the lane rebuild the project image for a
	// candidate whose dependency change stays within policy (plan §5.7). Nil
	// leaves every such candidate refused as rebuild_not_configured.
	RebuildProjectImage *ProjectImageRebuild
	// ImageUpkeepPending is image upkeep's pending-resolution lookup
	// (ImageUpkeep.PendingResolution). A re-entry whose base no recorded
	// image serves holds while it reports a resolution and stops as before
	// when it reports none. Nil reports none.
	ImageUpkeepPending func(context.Context) (*domain.ImageUpkeepHold, error)
	// OpenIssues lists the filing target's open issues for the duplicate
	// search a follow-up filing waits on (plan §7 Finding Adjudication). Nil
	// leaves every filing unproposed, with the run's refusal notice.
	OpenIssues   *publish.OpenIssueLister
	ReviewSource exec.ReviewSource
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
	// ReviewGuide is the producing agent's guide resolved against the
	// imported commit and its base, or nil when the import carries none.
	// Checkpoints written before it existed decode with it nil.
	ReviewGuide *domain.ReviewGuide `json:"review_guide"`
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
	operatorRecipes         map[domain.Digest]bool
	repositoryID            int64
	newRoom                 func(domain.ProjectImage, ward.ContainerSize) (ProductionVerificationRoom, error)
	newBoundRoom            func(domain.ProjectImage, ward.ContainerSize, domain.Run, domain.InvocationID) (ProductionVerificationRoom, error)
	proveVerificationAbsent func(context.Context, domain.Run, domain.InvocationID) error
	// proveVerificationContainersAbsent is the operator-confirmed release's
	// proof (ProductionPublicationConfig.ProveVerificationContainersAbsent).
	proveVerificationContainersAbsent func(context.Context, domain.Run, domain.InvocationID) error
	// verificationReproved and verificationHeldAlerts are the end-of-task
	// proof's process state (verification_memory_held.go), read and written
	// under reconcileMu.
	verificationReproved   map[domain.InvocationID]bool
	verificationHeldAlerts map[domain.InvocationID]bool
	rebuild                *ProjectImageRebuild
	// hostVerificationRoom admits a verification job onto the host executor
	// and builds its room (Engine.admitHostVerificationRoom). engine.New sets
	// it; nil admits nothing.
	hostVerificationRoom            func(context.Context, productionPublicationTask) (verify.Room, HostVerificationAdmission, error)
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
	openIssues                      *publish.OpenIssueLister
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
	// running counts the lane's verification jobs and image builds for the
	// restart readiness read; Engine.New sets it from WithRunningWork.
	running     *RunningWork
	reconcileMu sync.Mutex
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
	if cfg.NewBoundRoom != nil && (cfg.ProveVerificationAbsent == nil || cfg.ProveVerificationContainersAbsent == nil) {
		// A bound room's containers outlive its pass, so only the
		// cross-pass proofs may release the reservation.
		return nil, errors.New("bound verification rooms need an absence proof")
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
		reviewAdmitter:                    cfg.ReviewAdmission,
		newBoundRoom:                      cfg.NewBoundRoom,
		proveVerificationAbsent:           cfg.ProveVerificationAbsent,
		proveVerificationContainersAbsent: cfg.ProveVerificationContainersAbsent,
		rebuild:                           cfg.RebuildProjectImage,
		imageUpkeepPending:                cfg.ImageUpkeepPending,
		openIssues:                        cfg.OpenIssues,
		remediationPromptPackage:          cfg.RemediationPromptPackageDigest,
		shadowReviewSource:                cfg.ShadowReviewSource,
		reviewRecovery:                    cfg.ReviewRecovery, reviewRecoveryPending: true,
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
	pending, releasedWaits, concludedWaits, err := w.readPendingTasks(ctx)
	if err != nil {
		return result, errors.Join(joined, err)
	}
	if err := w.retireClosureWaits(ctx, concludedWaits); err != nil {
		joined = errors.Join(joined, fmt.Errorf("retire concluded closure waits: %w", err))
	}
	w.pruneHeldTaskRetries(pending)
	// The end-of-task proof after the loop treats a verification reservation
	// as ended only when no pending task this pass read belongs to its run,
	// and none at all when a pending task could not be read.
	pendingRuns := make(map[domain.RunID]bool, len(pending))
	pendingUnread := false
	for _, entry := range pending {
		task, err := w.decodePendingTask(ctx, entry)
		if err != nil {
			pendingUnread = true
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
		pendingRuns[task.RunID] = true
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
	if err := w.reproveEndedVerificationReservations(ctx, pendingRuns, !pendingUnread); err != nil {
		joined = errors.Join(joined, fmt.Errorf("re-prove ended verification reservations: %w", err))
	}
	return result, joined
}

// readPendingTasks reads every task the lane's pass works: the pending
// publication, reevaluation, and continuation rows, and the published tasks
// whose closure wait the human gate has released. It also returns the keys of
// those released tasks and the closure waits the pass retires.
func (w *productionPublicationWorkflow) readPendingTasks(
	ctx context.Context,
) (pending []store.QueueEntry, releasedWaits, concludedWaits []string, err error) {
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
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
	})
	return pending, releasedWaits, concludedWaits, err
}

// decodePendingTask reconstructs one entry readPendingTasks returned.
func (w *productionPublicationWorkflow) decodePendingTask(
	ctx context.Context, entry store.QueueEntry,
) (productionPublicationTask, error) {
	switch entry.Kind {
	case domain.PublicationContinuationRequestedKind:
		return w.reconstructPublicationContinuationTask(ctx, entry)
	case signet.PublicationReevaluationRequestedKind:
		return w.reconstructProductionReevaluationTask(ctx, entry)
	default:
		return decodeProductionPublicationTask(entry)
	}
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
			derivation := FallbackCommitMessageInput{
				Spec: binding.spec, BoundIssue: boundIssue, RunID: binding.run.ID,
				SpecDigest: binding.run.SpecDigest, Policy: policy,
			}
			// A run imported before the v2 recipe stored the v1 message, so
			// either derivation is the daemon's own; anything else falls to
			// the v2 derivation and the comparison below refuses it.
			want.CommitMessage = FallbackCommitMessage(derivation)
			recorded := binding.replay.ImportOptions.CommitMessage
			if FallbackCommitMessageMatches(derivation, recorded) {
				want.CommitMessage = recorded
			}
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

// runAfterVerification runs the test-only crash seam that follows a stored
// verification on the forward, reentry, and handoff paths. label names the
// verification in the returned error.
func (w *productionPublicationWorkflow) runAfterVerification(label string) error {
	if w.afterVerification == nil {
		return nil
	}
	if err := w.afterVerification(); err != nil {
		return fmt.Errorf("after %s: %w", label, errors.Join(err, errProductionCrashSeam))
	}
	return nil
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
	// Scope-conflict recovery writes only task.scopeDecision, which route
	// doesn't read, so the route computed here holds for the whole pass.
	route := task.route()
	switch route {
	case productionPublicationRouteReentryInPlace:
		return w.reconcileReentryTask(ctx, task, binding)
	case productionPublicationRouteRetireUnpublishedReentry, productionPublicationRouteContinuation,
		productionPublicationRouteForward:
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
	switch route {
	case productionPublicationRouteReentryInPlace:
		return productionTaskOutcome{}, fmt.Errorf("publication route %q reached unpublished-head retirement", route)
	case productionPublicationRouteRetireUnpublishedReentry:
		return w.retireReentryTask(ctx, task, binding)
	case productionPublicationRouteContinuation, productionPublicationRouteForward:
	}
	scratch, err := os.MkdirTemp(w.workDir, ".production-publication-")
	if err != nil {
		return productionTaskOutcome{}, err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // run-owned scratch
	checkout, checkoutDir, err := w.fetchAdmittedBase(ctx, scratch, binding.admission.Base)
	var fetchErr *publicationBaseFetchError
	if errors.As(err, &fetchErr) {
		return w.publicationTransportFailure(ctx, task, "fetch exact publication base", fetchErr.err)
	}
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
	// Advisory findings (plan §5.8, revision 42, and §5.5) are the one class
	// a clean reconstruction may carry: they never block, and the
	// authorization built below binds them so the publisher can surface them.
	// The stance is the bound profile's, never the replay payload's.
	if imported.CommitSHA != binding.export.HeadSHA ||
		!importer.AllAdvisory(imported.Findings, binding.profile.CandidateAutomationChanges) {
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
	switch route {
	case productionPublicationRouteReentryInPlace, productionPublicationRouteRetireUnpublishedReentry:
		return productionTaskOutcome{}, fmt.Errorf("publication route %q reached publication continuation", route)
	case productionPublicationRouteContinuation:
		return w.startPublicationContinuation(ctx, task, checkoutDir)
	case productionPublicationRouteForward:
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

	// Progress is the state read here, at the start of the pass. Verification
	// writes a checkpoint further down, and nothing after it reads progress.
	progress, checkpoint, err := w.readPublicationProgress(ctx, task, binding, imported)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	blocked, err := w.recoverDefinitiveBlockedTask(
		ctx, task, binding, imported, checkpoint, progress,
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
	switch progress {
	case productionPublicationProgressUnverified, productionPublicationProgressVerified:
	case productionPublicationProgressIntentFinalized, productionPublicationProgressReady:
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
		// A recovered candidate names the tip its publication was decided
		// against: the sealed check's, or the evidence base for an intent
		// committed before handoff checks existed.
		sealed, found, err := w.loadHandoffCheck(ctx, task)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		var handoff *domain.HandoffCheck
		if found {
			handoff = &sealed
		}
		candidate, heldCandidate, err := w.composePublicationCandidate(
			ctx, task, binding, checkpoint, adoptedProfile, report, reviewInstructions, handoff,
		)
		if err != nil || heldCandidate != nil {
			return derefOutcome(heldCandidate), err
		}
		if progress == productionPublicationProgressReady {
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
	recipes, err := w.recipes(ctx)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if !recipes[binding.image.RecipeDigest] {
		pendingIntent, err := w.hasPendingPublicationIntent(ctx, task.PublicationID)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if progress.hasCheckpoint() || pendingIntent {
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
	switch progress {
	case productionPublicationProgressUnverified:
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
		if err := w.runAfterVerification("production verification"); err != nil {
			return productionTaskOutcome{}, err
		}
	case productionPublicationProgressVerified, productionPublicationProgressIntentFinalized:
	case productionPublicationProgressReady:
		// The ready arm of the recovery switch above always returns.
		return productionTaskOutcome{}, fmt.Errorf("publication progress %q reached verification", progress)
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
	candidate, heldCandidate, err := w.composePublicationCandidate(
		ctx, task, binding, checkpoint, adoptedProfile, report, reviewInstructions, &handoff,
	)
	if err != nil || heldCandidate != nil {
		return derefOutcome(heldCandidate), err
	}
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

// composePublicationCandidate composes the candidate the forward and recovery
// branches of reconcileTask publish or recover: its metadata, its handoff
// tip, its successor binding, and its disposition history. A non-nil outcome
// is a durable hold the caller returns. A composition error the retry
// classifier accepts, such as a busy store read of the closure checkpoint,
// returns with no outcome, so the run loop backs off and retries (#3001).
//
// The handoff check is an input because the branches choose it differently.
// Forward passes the check reconcileHandoff just returned. Recovery passes
// the publication's sealed check, so the body and closure it renders name
// the tip the publication was decided against, or nil for an intent
// committed before handoff checks existed, which keeps its evidence base.
// Authoring and closure admission run before this, on forward only.
func (w *productionPublicationWorkflow) composePublicationCandidate(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	adoptedProfile *domain.Digest,
	report []byte,
	reviewInstructions exec.ReviewInstructionBinding,
	handoff *domain.HandoffCheck,
) (publish.Candidate, *productionTaskOutcome, error) {
	candidate, err := w.productionCandidate(ctx, task, binding, checkpoint, adoptedProfile, nil, report)
	if err != nil {
		// A store read that failed on the environment retries. A state
		// contradiction (ErrParentKeyMismatch) or an untyped error holds.
		if productionPublicationRetryableFailure(err) {
			return publish.Candidate{}, nil, err
		}
		held, holdErr := w.holdPublicMetadataTask(ctx, task, checkpoint.Imported, err)
		return publish.Candidate{}, &held, holdErr
	}
	if handoff != nil {
		candidate.BaseSHA = handoff.TipSHA
		candidate.HandoffCheck = handoff
	}
	if err := w.bindSuccessorCandidate(ctx, task, &candidate); err != nil {
		return publish.Candidate{}, nil, err
	}
	history, err := publish.LoadDispositionHistory(
		ctx, w.store, candidate, reviewInstructions.ResultDigest,
	)
	if err != nil {
		var held productionTaskOutcome
		switch {
		case errors.Is(err, domain.ErrReviewConfigurationUnapproved):
			held, err = w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
		case errors.Is(err, domain.ErrParentKeyMismatch):
			held, err = w.holdDispositionHistoryMismatch(ctx, task, checkpoint.Imported)
		default:
			return publish.Candidate{}, nil, fmt.Errorf("load publication disposition history: %w", err)
		}
		return publish.Candidate{}, &held, err
	}
	candidate.DispositionHistory = &history
	return candidate, nil, nil
}

// productionCandidate composes the publication candidate. Its title and body
// come from publicationTitleBody, which renders v1 for a v1 or literal record
// and the stored authored artifact for a v2 record (falling back to v1 on any
// read or gate failure). Every publication path (forward publish, crash
// recovery, and drift repair through the candidate this returns) runs through
// here, so a given candidate replays byte-identical metadata. A closure
// checkpoint read error from applyClosureReference returns unchanged.
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
		ReadinessDetail:   &readiness.detail,
		YieldHistory:      &yieldHistory,
		DiffStats:         checkpoint.DiffStats,
		ReviewGuide:       checkpoint.ReviewGuide,
		ReviewGuideNotice: checkpoint.Imported.ReviewGuideNotice,
		DisplayNames:      names,
		CommitPlanNotice:  checkpoint.Imported.CommitPlanNotice,
		ScopeDecision:     task.scopeDecision,
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
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
	progress productionPublicationProgress,
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
	if progress == productionPublicationProgressReady {
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
		if !progress.hasCheckpoint() || checkpoint.Authorization.AuthorizesPublication {
			return nil, fmt.Errorf("production blocked item %q has an impossible verification reason: %w",
				current.ID, domain.ErrParentKeyMismatch)
		}
		artifacts = checkpoint.Artifacts
	case productionBlockReentryRetired:
		// A retired re-entry is ended before any checkpoint is read, with no
		// evidence.
	case productionBlockHandoffVerification:
		if !progress.hasCheckpoint() || !checkpoint.Authorization.AuthorizesPublication {
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
		if !progress.hasCheckpoint() || !checkpoint.Authorization.AuthorizesPublication {
			return nil, fmt.Errorf("production blocked item %q has an impossible publication reason: %w",
				current.ID, domain.ErrParentKeyMismatch)
		}
		artifacts = checkpoint.Artifacts
	case productionBlockMemoryLimit:
		// A first cycle's killed verification records no checkpoint and no
		// evidence. An unpublished re-entry's carries the authorizing
		// checkpoint it re-verified, like its other base-advance blocks.
		if progress.hasCheckpoint() {
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
