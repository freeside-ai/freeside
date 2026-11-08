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
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	exporter "github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"golang.org/x/sys/unix"
)

const (
	fakePublicationMaxCommitTimestamp = int64(4102444800)

	// OperatingModeAttendedDev is the only mode the 1A.1 fake-candidate
	// workflow accepts. Starting this explicit workflow is a manual attended
	// operation; it does not enable auto-start or unattended publication.
	OperatingModeAttendedDev = fakepublication.OperatingModeAttended
)

// FakePublicationConfig supplies the already-reviewed deterministic and
// external boundaries for the 1A.1 workflow.
type FakePublicationConfig struct {
	WorkDir string
	// ProtectedRoots names every daemon-owned file or directory that the
	// attended workspace must not contain.
	ProtectedRoots  []string
	Recipe          []byte
	RecipePath      string
	ApprovedRecipes map[domain.Digest]bool
	Transport       PublicationTransport
	Publisher       *publish.Publisher
	Artifacts       ArtifactStore
	NewRoom         func(home string) verify.Room
	Now             func() time.Time
	// AfterPublicationFinalized is a crash-checkpoint seam used by recovery
	// tests after the durable outcome commits but before terminal attention.
	AfterPublicationFinalized func() error
}

// WithFakePublication enables the 1A.1 fake-candidate workflow.
func WithFakePublication(cfg FakePublicationConfig) Option {
	return func(e *Engine) error {
		workflow, err := newFakePublicationWorkflow(e.store, e.signet, cfg)
		if err != nil {
			return fmt.Errorf("configure fake publication: %w", err)
		}
		e.publication = workflow
		return nil
	}
}

// FakePublicationSpec is the explicit, attended request to export and publish
// one fake candidate.
type FakePublicationSpec struct {
	RunID                    domain.RunID
	ProjectID                domain.ProjectID
	WorkspaceDir             string
	Repo                     string
	BaseRef                  string
	BaseSHA                  string
	AllowedPaths             []string
	VerificationInvocationID domain.InvocationID
	PublicationInvocationID  domain.InvocationID
	Title                    string
	Body                     string
	CommitDate               time.Time
	OperatingMode            string
}

type fakePublicationCandidateCheckpoint struct {
	Version       string                        `json:"version"`
	TaskKey       string                        `json:"task_key"`
	Imported      importer.Result               `json:"imported"`
	Authorization domain.CandidateAuthorization `json:"authorization"`
	Artifacts     []domain.Artifact             `json:"artifacts"`
}

type fakePublicationWorkflow struct {
	store                     *store.Store
	attention                 attentionService
	workDir                   string
	protectedRoots            []string
	recipeDigest              domain.Digest
	recipePath                string
	approvedRecipes           map[domain.Digest]bool
	transport                 PublicationTransport
	publisher                 *publish.Publisher
	artifacts                 ArtifactStore
	newRoom                   func(home string) verify.Room
	now                       func() time.Time
	afterPublicationFinalized func() error

	reconcileMu sync.Mutex
	candidates  map[domain.InvocationID]publish.RecoveryCandidate
}

func (w *fakePublicationWorkflow) attentionCreatedAt() time.Time {
	if w.now == nil {
		return time.Now().UTC()
	}
	return w.now().UTC()
}

type fakePublicationReconcileResult struct {
	completed    int
	ready        int
	blocked      int
	lastPRNumber int
}

func newFakePublicationWorkflow(
	st *store.Store,
	attention attentionService,
	cfg FakePublicationConfig,
) (*fakePublicationWorkflow, error) {
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
	if _, err := verify.ParseRecipe(cfg.Recipe); err != nil {
		return nil, fmt.Errorf("trusted recipe: %w", err)
	}
	recipeDigest := verify.RecipeDigest(cfg.Recipe)
	if !cfg.ApprovedRecipes[recipeDigest] {
		return nil, fmt.Errorf("trusted recipe %s is not approved", recipeDigest)
	}
	recipePath := cfg.RecipePath
	if recipePath == "" {
		recipePath = verify.DefaultRecipePath
	}
	if err := validateFakePublicationRecipePath(recipePath); err != nil {
		return nil, fmt.Errorf("trusted recipe path %q: %w", recipePath, err)
	}
	if _, err := cfg.Artifacts.Put(recipeDigest, bytes.NewReader(cfg.Recipe)); err != nil {
		return nil, fmt.Errorf("persist trusted recipe %s: %w", recipeDigest, err)
	}
	if _, err := loadFakePublicationRecipe(cfg.Artifacts, recipeDigest); err != nil {
		return nil, fmt.Errorf("reload trusted recipe %s: %w", recipeDigest, err)
	}
	if err := makeFakePublicationDirectory(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create work directory: %w", err)
	}
	if len(cfg.ProtectedRoots) == 0 {
		return nil, errors.New("fake publication requires daemon-owned protected roots")
	}
	protectedRoots := make([]string, 0, len(cfg.ProtectedRoots)+1)
	protectedRoots = append(protectedRoots, workDir)
	for _, root := range cfg.ProtectedRoots {
		if root == "" {
			return nil, errors.New("fake publication protected root is empty")
		}
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve fake publication protected root: %w", err)
		}
		protectedRoots = append(protectedRoots, absolute)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &fakePublicationWorkflow{
		store: st, attention: attention, workDir: workDir,
		protectedRoots: protectedRoots,
		recipeDigest:   recipeDigest, recipePath: recipePath,
		approvedRecipes: maps.Clone(cfg.ApprovedRecipes),
		transport:       cfg.Transport, publisher: cfg.Publisher, artifacts: cfg.Artifacts,
		newRoom: cfg.NewRoom, now: now,
		afterPublicationFinalized: cfg.AfterPublicationFinalized,
		candidates:                map[domain.InvocationID]publish.RecoveryCandidate{},
	}, nil
}

// StartFakePublication captures one immutable handoff, then commits its
// attended_dev task and Run before any git, verification, or GitHub effect.
// A replay converges on the originally committed timestamps and rejects
// changed bindings without re-reading the source workspace.
func (e *Engine) StartFakePublication(ctx context.Context, spec FakePublicationSpec) (domain.Run, error) {
	if e.publication == nil {
		return domain.Run{}, errors.New("start fake publication: workflow is not configured")
	}
	return e.publication.start(ctx, spec)
}

func (w *fakePublicationWorkflow) start(ctx context.Context, spec FakePublicationSpec) (domain.Run, error) {
	task, explicitCommitDate, err := w.newTask(ctx, spec)
	if err != nil {
		return domain.Run{}, fmt.Errorf("start fake publication: %w", err)
	}
	key := fakePublicationTaskKey(task.RunID)

	var committed fakePublicationTask
	var installedHandoff string
	err = w.store.Write(ctx, func(tx *store.WriteTx) error {
		entry, err := tx.GetOutbox(ctx, key)
		if errors.Is(err, store.ErrNotFound) {
			if err := validateNewFakePublicationBindings(ctx, tx, task); err != nil {
				return err
			}
			workspace, err := openValidatedFakePublicationWorkspace(
				task.WorkspaceDir, w.protectedRoots,
			)
			if err != nil {
				return err
			}
			handoffDigest, commitErr := w.commitHandoff(task, workspace.FS())
			if commitErr == nil {
				installedHandoff = task.HandoffDir
			}
			if err := errors.Join(commitErr, workspace.Close()); err != nil {
				return err
			}
			task.HandoffDigest = handoffDigest
			if err := claimFakePublicationInvocations(ctx, tx, task); err != nil {
				return err
			}
			payload, err := encodeFakePublicationTask(task)
			if err != nil {
				return err
			}
			entry, inserted, err := tx.EnqueueOutbox(
				ctx, key, fakePublicationTaskKind, payload,
			)
			if err != nil {
				return err
			}
			if !inserted || entry.IdempotencyKey != key ||
				entry.Kind != fakePublicationTaskKind ||
				!bytes.Equal(entry.Payload, payload) {
				return fmt.Errorf("new task row disagrees with committed task: %w",
					domain.ErrParentKeyMismatch)
			}
			committed = task
		} else if err != nil {
			return err
		} else {
			if entry.IdempotencyKey != key || entry.Kind != fakePublicationTaskKind {
				return fmt.Errorf("task row disagrees with key or kind: %w",
					domain.ErrParentKeyMismatch)
			}
			prior, err := decodeFakePublicationTask(entry.Payload)
			if err != nil {
				return fmt.Errorf("decode prior task: %w", err)
			}
			priorWorkDir, err := fakePublicationTaskWorkDir(prior)
			if err != nil {
				return fmt.Errorf("recover prior task work directory: %w", err)
			}
			if priorWorkDir != w.workDir {
				return fmt.Errorf(
					"task %q work directory %q, configured %q: %w",
					key, priorWorkDir, w.workDir, domain.ErrImmutableTransition,
				)
			}
			if !sameFakePublicationRequest(prior, task, explicitCommitDate) {
				return fmt.Errorf("task %q fixed bindings disagree with stored task: %w",
					key, domain.ErrImmutableTransition)
			}
			migrated, err := convergeFakePublicationPolicyTx(ctx, tx, prior)
			if err != nil {
				return err
			}
			if err := validateFakePublicationRun(ctx, tx, prior); err != nil {
				return err
			}
			if err := claimFakePublicationInvocations(ctx, tx, prior); err != nil {
				return err
			}
			committed = prior
			if migrated {
				return nil
			}
			return errReplay
		}
		run := publicationRun(committed)
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, publicationPolicy(committed)); err != nil {
			return err
		}
		return nil
	})
	if err != nil && installedHandoff != "" {
		err = errors.Join(err, rollbackFakePublicationHandoff(installedHandoff))
	}
	if errors.Is(err, errReplay) {
		return w.storedPublicationRun(ctx, committed.RunID)
	}
	if err != nil {
		return domain.Run{}, fmt.Errorf("start fake publication: %w", err)
	}
	return w.storedPublicationRun(ctx, committed.RunID)
}

func (w *fakePublicationWorkflow) storedPublicationRun(ctx context.Context, id domain.RunID) (domain.Run, error) {
	var run domain.Run
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, id)
		return err
	})
	return run, err
}

func validateNewFakePublicationBindings(
	ctx context.Context,
	tx *store.WriteTx,
	task fakePublicationTask,
) error {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return err
	}
	if state.SyncEpoch != task.StoreEpoch {
		return fmt.Errorf(
			"store epoch changed during admission: %w", domain.ErrParentKeyMismatch,
		)
	}
	profile, err := tx.LatestTrustProfile(ctx, task.Repo)
	if err != nil {
		return fmt.Errorf("recheck reviewed trust profile: %w", err)
	}
	if err := profile.Validate(); err != nil {
		return fmt.Errorf("recheck reviewed trust profile: %w", err)
	}
	if profile.Repo != task.Repo || profile.ProfileDigest != task.TrustProfileDigest {
		return fmt.Errorf(
			"reviewed trust profile changed during admission: %w",
			domain.ErrParentKeyMismatch,
		)
	}
	reservation, err := fakePublicationReservation(task)
	if err != nil {
		return err
	}
	// A publisher intent already at this invocation's key means somebody
	// published under the identity this task is about to commit itself to, so
	// admission refuses. This task's own reservation does not: re-admitting the
	// same request must converge, not refuse itself.
	if err := publish.CheckInvocationAvailable(ctx, tx, reservation); err != nil {
		return err
	}
	return nil
}

func claimFakePublicationInvocations(
	ctx context.Context,
	tx *store.WriteTx,
	task fakePublicationTask,
) error {
	owners, err := expectedFakePublicationInvocationOwners(task)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		if err := claimFakePublicationInvocation(
			ctx, tx, owner.RunID, owner.InvocationID, owner.Role, owner.BindingDigest,
		); err != nil {
			return err
		}
	}
	// The owner rows above bind each invocation to this run for the engine's
	// own reconciliation checks. They cannot stop another publisher from taking
	// the publication intent's key before this task reconciles, because no
	// publisher consults them. Occupying that key is what does (#308).
	reservation, err := fakePublicationReservation(task)
	if err != nil {
		return err
	}
	return publish.ClaimInvocation(ctx, tx, reservation)
}

// validateAndClaim re-checks the task's durable bindings and holds its
// publication invocation, in one write transaction.
//
// The claim is here, and not only at admission, because a task admitted before
// the reservation contract existed carries no reservation, and recovery reaches
// reconciliation without passing through admission: restarting under a build
// that has the contract would otherwise never install one, leaving that task's
// invocation key unprotected for the rest of its life. Claiming is idempotent
// for a task that already holds its reservation and accepts the settled intent
// of one that already published.
//
// Validating and claiming share a transaction so no writer can take the key in
// the gap between them. Both are bookkeeping rather than client-visible state,
// so the transaction is internal and bumps no revision.
func (w *fakePublicationWorkflow) validateAndClaim(
	ctx context.Context,
	task fakePublicationTask,
) error {
	if err := ensureFakePublicationPolicy(ctx, w.store, task); err != nil {
		return err
	}
	reservation, err := fakePublicationReservation(task)
	if err != nil {
		return err
	}
	return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := validateFakePublicationReconciliation(ctx, tx, task); err != nil {
			return err
		}
		return publish.ClaimInvocation(ctx, tx, reservation)
	})
}

// fakePublicationReservation is the claim this task holds on the publication
// intent's key from admission until it publishes. It carries the same run
// identity the publication-role owner row does, so the two cannot disagree
// about who owns the invocation.
func fakePublicationReservation(task fakePublicationTask) (publish.Reservation, error) {
	return publish.NewReservation(task.PublicationInvocationID, task.RunID)
}

func expectedFakePublicationInvocationOwners(
	task fakePublicationTask,
) ([]fakePublicationInvocationOwner, error) {
	verificationBinding, err := fakePublicationVerificationBindingDigest(task)
	if err != nil {
		return nil, err
	}
	publicationBinding, err := digestJSON(struct {
		RunID domain.RunID `json:"run_id"`
	}{RunID: task.RunID})
	if err != nil {
		return nil, fmt.Errorf("digest publication invocation binding: %w", err)
	}
	return []fakePublicationInvocationOwner{
		{
			Version:      fakePublicationInvocationOwnerVersion,
			InvocationID: task.VerificationInvocationID, Role: "verification",
			BindingDigest: verificationBinding, RunID: task.RunID,
		},
		{
			Version:      fakePublicationInvocationOwnerVersion,
			InvocationID: task.PublicationInvocationID, Role: "publication",
			BindingDigest: publicationBinding, RunID: task.RunID,
		},
	}, nil
}

func fakePublicationVerificationBindingDigest(
	task fakePublicationTask,
) (domain.Digest, error) {
	binding := struct {
		Version                  string              `json:"version"`
		StoreEpoch               string              `json:"store_epoch"`
		HandoffDigest            domain.Digest       `json:"handoff_digest"`
		Repo                     string              `json:"repo"`
		BaseRef                  string              `json:"base_ref"`
		BaseSHA                  string              `json:"base_sha"`
		AllowedPaths             []string            `json:"allowed_paths"`
		RecipeDigest             domain.Digest       `json:"recipe_digest"`
		RecipePath               string              `json:"recipe_path"`
		TrustProfileDigest       domain.Digest       `json:"trust_profile_digest"`
		VerificationInvocationID domain.InvocationID `json:"verification_invocation_id"`
		CommitDate               time.Time           `json:"commit_date"`
		StartedAt                time.Time           `json:"started_at"`
		OperatingMode            string              `json:"operating_mode"`
	}{
		Version: task.Version, StoreEpoch: task.StoreEpoch,
		HandoffDigest: task.HandoffDigest,
		Repo:          task.Repo, BaseRef: task.BaseRef, BaseSHA: task.BaseSHA,
		AllowedPaths: slices.Clone(task.AllowedPaths),
		RecipeDigest: task.RecipeDigest, RecipePath: task.RecipePath,
		TrustProfileDigest:       task.TrustProfileDigest,
		VerificationInvocationID: task.VerificationInvocationID,
		CommitDate:               task.CommitDate,
		StartedAt:                task.StartedAt,
		OperatingMode:            task.OperatingMode,
	}
	digest, err := digestJSON(binding)
	if err != nil {
		return "", fmt.Errorf("digest verification invocation binding: %w", err)
	}
	return digest, nil
}

func claimFakePublicationInvocation(
	ctx context.Context,
	tx *store.WriteTx,
	runID domain.RunID,
	invocationID domain.InvocationID,
	role string,
	bindingDigest domain.Digest,
) error {
	owner := fakePublicationInvocationOwner{
		Version:       fakePublicationInvocationOwnerVersion,
		InvocationID:  invocationID,
		Role:          role,
		BindingDigest: bindingDigest,
		RunID:         runID,
	}
	payload, err := json.Marshal(owner)
	if err != nil {
		return fmt.Errorf("encode publication invocation owner: %w", err)
	}
	key := fakePublicationInvocationOwnerKey(invocationID)
	entry, inserted, err := tx.EnqueueOutbox(
		ctx, key, fakePublicationInvocationOwnerKind, payload,
	)
	if err != nil {
		return fmt.Errorf("claim publication invocation: %w", err)
	}
	if entry.IdempotencyKey != key || entry.Kind != fakePublicationInvocationOwnerKind {
		return fmt.Errorf("publication invocation owner row disagrees with key or kind: %w",
			domain.ErrParentKeyMismatch)
	}
	committed, err := decodeFakePublicationInvocationOwner(entry.Payload)
	if err != nil {
		return fmt.Errorf("decode publication invocation owner: %w", err)
	}
	if !sameFakePublicationInvocationReservation(committed, owner) {
		return fmt.Errorf(
			"%s invocation %q belongs to incompatible run %q as %s: %w",
			role, invocationID, committed.RunID, committed.Role,
			domain.ErrParentKeyMismatch,
		)
	}
	if inserted {
		if err := tx.MarkOutboxDispatched(ctx, key); err != nil {
			return fmt.Errorf("complete publication invocation claim: %w", err)
		}
	} else if !entry.Dispatched() {
		return fmt.Errorf("publication invocation owner %q is not complete: %w",
			key, domain.ErrParentKeyMismatch)
	}
	return nil
}

func (w *fakePublicationWorkflow) newTask(
	ctx context.Context,
	spec FakePublicationSpec,
) (fakePublicationTask, bool, error) {
	if spec.RunID == "" || spec.ProjectID == "" || spec.VerificationInvocationID == "" ||
		spec.PublicationInvocationID == "" {
		return fakePublicationTask{}, false, domain.ErrEmptyID
	}
	if spec.VerificationInvocationID == spec.PublicationInvocationID {
		return fakePublicationTask{}, false, errors.New("verification and publication invocation ids must differ")
	}
	if spec.OperatingMode != OperatingModeAttendedDev {
		return fakePublicationTask{}, false, fmt.Errorf("operating mode %q is not %s", spec.OperatingMode, OperatingModeAttendedDev)
	}
	if spec.Repo == "" || spec.BaseRef == "" || !validCommitSHA(spec.BaseSHA) ||
		spec.Title == "" {
		return fakePublicationTask{}, false, errors.New("repository, base ref, full base SHA, and title are required")
	}
	if err := publish.ValidateRepository(spec.Repo); err != nil {
		return fakePublicationTask{}, false, fmt.Errorf("repository %q: %w", spec.Repo, err)
	}
	if err := publish.ValidateBranchName(spec.BaseRef); err != nil {
		return fakePublicationTask{}, false, fmt.Errorf("base ref %q: %w", spec.BaseRef, err)
	}
	if len(spec.AllowedPaths) == 0 {
		return fakePublicationTask{}, false, errors.New("at least one candidate path allowlist pattern is required")
	}
	if err := validateFakePublicationAllowlist(spec.AllowedPaths); err != nil {
		return fakePublicationTask{}, false, err
	}
	if spec.WorkspaceDir == "" {
		return fakePublicationTask{}, false, errors.New("workspace is required")
	}
	if err := publish.ValidateCandidateBody(spec.Body); err != nil {
		return fakePublicationTask{}, false, err
	}
	workspaceDir, err := filepath.Abs(spec.WorkspaceDir)
	if err != nil {
		return fakePublicationTask{}, false, fmt.Errorf("resolve workspace: %w", err)
	}
	var profile domain.AutomationTrustProfile
	var state store.ServerState
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		profile, err = tx.LatestTrustProfile(ctx, spec.Repo)
		if err != nil {
			return err
		}
		state, err = tx.ServerState(ctx)
		return err
	})
	if err != nil {
		return fakePublicationTask{}, false, fmt.Errorf("read reviewed trust profile: %w", err)
	}
	if err := profile.Validate(); err != nil {
		return fakePublicationTask{}, false, fmt.Errorf("reviewed trust profile: %w", err)
	}
	if profile.Repo != spec.Repo {
		return fakePublicationTask{}, false, errors.New("reviewed trust profile names another repository")
	}
	startedAt := w.now().UTC()
	explicitCommitDate := !spec.CommitDate.IsZero()
	commitDate := spec.CommitDate
	if commitDate.IsZero() {
		commitDate = startedAt
	} else {
		commitDate = commitDate.UTC()
	}
	if err := validateFakePublicationCommitDate(commitDate); err != nil {
		return fakePublicationTask{}, false, err
	}
	task := fakePublicationTask{
		Version: fakePublicationTaskVersion,
		RunID:   spec.RunID, ProjectID: spec.ProjectID, StoreEpoch: state.SyncEpoch,
		WorkspaceDir: workspaceDir,
		Repo:         spec.Repo, BaseRef: spec.BaseRef, BaseSHA: spec.BaseSHA,
		AllowedPaths: slices.Clone(spec.AllowedPaths),
		RecipeDigest: w.recipeDigest, RecipePath: w.recipePath,
		TrustProfileDigest:       profile.ProfileDigest,
		VerificationInvocationID: spec.VerificationInvocationID,
		PublicationInvocationID:  spec.PublicationInvocationID,
		Title:                    spec.Title, Body: spec.Body,
		CommitDate: commitDate, CommitDateExplicit: explicitCommitDate, StartedAt: startedAt,
		OperatingMode: spec.OperatingMode,
	}
	if err := validateFakePublicationTaskUTF8(task); err != nil {
		return fakePublicationTask{}, false, err
	}
	handoffDir, err := w.expectedHandoffDir(task)
	if err != nil {
		return fakePublicationTask{}, false, fmt.Errorf("bind handoff: %w", err)
	}
	task.HandoffDir = handoffDir
	return task, explicitCommitDate, nil
}

func (w *fakePublicationWorkflow) reconcile(ctx context.Context) (fakePublicationReconcileResult, error) {
	w.reconcileMu.Lock()
	defer w.reconcileMu.Unlock()

	var pending []store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, fakePublicationTaskKind)
		return err
	}); err != nil {
		return fakePublicationReconcileResult{}, err
	}

	var result fakePublicationReconcileResult
	var taskErr error
	for _, entry := range pending {
		outcome, err := w.reconcileEntry(ctx, entry)
		if err != nil {
			if errors.Is(err, store.ErrTaskWorkflowClosed) {
				continue
			}
			if errors.Is(err, publish.ErrJanitorInactive) {
				continue
			}
			taskErr = errors.Join(
				taskErr, fmt.Errorf("task %q: %w", entry.IdempotencyKey, err),
			)
			continue
		}
		result.completed++
		result.ready += boolCount(outcome.ready)
		result.blocked += boolCount(outcome.blocked)
		if outcome.prNumber > 0 {
			result.lastPRNumber = outcome.prNumber
		}
	}
	return result, taskErr
}

func (w *fakePublicationWorkflow) reconcileRun(
	ctx context.Context,
	runID domain.RunID,
) (fakePublicationReconcileResult, error) {
	w.reconcileMu.Lock()
	defer w.reconcileMu.Unlock()

	key := fakePublicationTaskKey(runID)
	var entry store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, key)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fakePublicationReconcileResult{}, nil
		}
		return fakePublicationReconcileResult{}, err
	}
	if entry.IdempotencyKey != key || entry.Kind != fakePublicationTaskKind {
		return fakePublicationReconcileResult{}, fmt.Errorf(
			"task %q has kind %q: %w", key, entry.Kind, domain.ErrParentKeyMismatch,
		)
	}
	if entry.Dispatched() {
		task, err := decodeFakePublicationTask(entry.Payload)
		if err != nil {
			return fakePublicationReconcileResult{}, err
		}
		if task.RunID != runID {
			return fakePublicationReconcileResult{}, fmt.Errorf(
				"task %q names run %q: %w", key, task.RunID, domain.ErrParentKeyMismatch,
			)
		}
		if err := ensureFakePublicationPolicy(ctx, w.store, task); err != nil {
			return fakePublicationReconcileResult{}, err
		}
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			return validateFakePublicationReconciliation(ctx, tx, task)
		}); err != nil {
			return fakePublicationReconcileResult{}, err
		}
		if _, found, err := w.recoverTerminalTask(ctx, task); err != nil {
			return fakePublicationReconcileResult{}, err
		} else if !found {
			return fakePublicationReconcileResult{}, fmt.Errorf(
				"dispatched task %q has no terminal item", key,
			)
		}
		return fakePublicationReconcileResult{}, nil
	}
	outcome, err := w.reconcileEntry(ctx, entry)
	if err != nil {
		return fakePublicationReconcileResult{}, fmt.Errorf("task %q: %w", key, err)
	}
	return fakePublicationReconcileResult{
		completed: 1, ready: boolCount(outcome.ready), blocked: boolCount(outcome.blocked),
		lastPRNumber: outcome.prNumber,
	}, nil
}

func (w *fakePublicationWorkflow) reconcileEntry(
	ctx context.Context,
	entry store.QueueEntry,
) (outcome taskOutcome, err error) {
	release, err := w.acquireTaskLock(ctx, entry.IdempotencyKey)
	if err != nil {
		return taskOutcome{}, err
	}
	defer func() {
		err = errors.Join(err, release())
	}()
	return w.reconcileEntryLocked(ctx, entry)
}

func (w *fakePublicationWorkflow) reconcileEntryLocked(
	ctx context.Context,
	entry store.QueueEntry,
) (taskOutcome, error) {
	task, err := decodeBoundFakePublicationTask(entry)
	if err != nil {
		return taskOutcome{}, err
	}
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		return tx.RequireRunWorkflowOpen(ctx, task.RunID)
	}); err != nil {
		return taskOutcome{}, err
	}
	expectedHandoff, err := w.expectedHandoffDir(task)
	if err != nil {
		return taskOutcome{}, fmt.Errorf("handoff binding: %w", err)
	}
	if task.HandoffDir != expectedHandoff {
		return taskOutcome{}, fmt.Errorf("handoff %q, want %q: %w",
			task.HandoffDir, expectedHandoff, domain.ErrParentKeyMismatch)
	}
	if err := w.validateAndClaim(ctx, task); err != nil {
		return taskOutcome{}, err
	}
	if outcome, found, err := w.recoverTerminalTask(ctx, task); err != nil {
		return taskOutcome{}, err
	} else if found {
		return outcome, nil
	}
	return w.reconcileTask(ctx, task)
}

type fakePublicationReconciliationReader interface {
	fakePublicationRunReader
	GetOutbox(context.Context, string) (store.QueueEntry, error)
}

func validateFakePublicationReconciliation(
	ctx context.Context,
	reader fakePublicationReconciliationReader,
	task fakePublicationTask,
) error {
	if err := validateFakePublicationRun(ctx, reader, task); err != nil {
		return err
	}
	owners, err := expectedFakePublicationInvocationOwners(task)
	if err != nil {
		return err
	}
	for _, expected := range owners {
		key := fakePublicationInvocationOwnerKey(expected.InvocationID)
		entry, err := reader.GetOutbox(ctx, key)
		if err != nil {
			return fmt.Errorf("load %s invocation owner %q: %w",
				expected.Role, expected.InvocationID, err)
		}
		if entry.IdempotencyKey != key ||
			entry.Kind != fakePublicationInvocationOwnerKind ||
			!entry.Dispatched() {
			return fmt.Errorf(
				"%s invocation owner %q is not the completed reservation: %w",
				expected.Role, expected.InvocationID, domain.ErrParentKeyMismatch,
			)
		}
		committed, err := decodeFakePublicationInvocationOwner(entry.Payload)
		if err != nil {
			return fmt.Errorf("decode %s invocation owner %q: %w",
				expected.Role, expected.InvocationID, err)
		}
		if !sameFakePublicationInvocationReservation(committed, expected) {
			return fmt.Errorf(
				"%s invocation owner %q disagrees with task: %w",
				expected.Role, expected.InvocationID, domain.ErrParentKeyMismatch,
			)
		}
	}
	return nil
}

func sameFakePublicationInvocationReservation(
	committed, expected fakePublicationInvocationOwner,
) bool {
	// RunID identifies the first claimant for diagnostics. Verification
	// reservations deliberately permit another run to reuse one exact
	// immutable request; the binding digest carries that authority.
	return committed.Version == expected.Version &&
		committed.InvocationID == expected.InvocationID &&
		committed.Role == expected.Role &&
		committed.BindingDigest == expected.BindingDigest
}

func validateFakePublicationRun(
	ctx context.Context,
	reader fakePublicationRunReader,
	task fakePublicationTask,
) error {
	run, err := reader.GetRun(ctx, task.RunID)
	if err != nil {
		return fmt.Errorf("load publication run %q: %w", task.RunID, err)
	}
	expected := publicationRun(task)
	expected.TaskID = run.TaskID
	if !reflect.DeepEqual(run, expected) {
		return fmt.Errorf(
			"publication run %q disagrees with task: %w",
			task.RunID, domain.ErrParentKeyMismatch,
		)
	}
	policy, err := reader.GetResolvedPolicy(ctx, task.RunID)
	if err != nil {
		return fmt.Errorf("load publication resolved policy %q: %w", task.RunID, err)
	}
	if !reflect.DeepEqual(policy, publicationPolicy(task)) {
		return fmt.Errorf(
			"publication resolved policy %q disagrees with task: %w",
			task.RunID, domain.ErrParentKeyMismatch,
		)
	}
	return nil
}

func validatePublicationCheckout(
	checkout PublicationCheckout,
	task fakePublicationTask,
	requestedDir string,
	requestedParent fs.FileInfo,
) (string, error) {
	return validatePublicationCheckoutBinding(
		checkout, task.Repo, task.BaseRef, task.BaseSHA, requestedDir, requestedParent,
	)
}

func (w *fakePublicationWorkflow) acquireTaskLock(
	ctx context.Context,
	taskKey string,
) (func() error, error) {
	lockDir := filepath.Join(w.workDir, "task-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create publication task lock directory: %w", err)
	}
	return acquirePublicationLock(ctx, lockDir, taskKey)
}

type taskOutcome struct {
	ready    bool
	blocked  bool
	prNumber int
}

func (w *fakePublicationWorkflow) reconcileTask(
	ctx context.Context,
	task fakePublicationTask,
) (outcome taskOutcome, err error) {
	if err := requireCommittedHandoff(task); err != nil {
		return taskOutcome{}, err
	}
	if recovered, found, err := w.recoverFinalizedPublication(ctx, task); err != nil {
		return taskOutcome{}, err
	} else if found {
		return recovered, nil
	}
	profile, err := w.loadBoundProfile(ctx, task)
	if err != nil {
		return taskOutcome{}, err
	}
	importPolicy, err := (importer.Policy{Allowlist: slices.Clone(task.AllowedPaths)}).WithProtectedPaths(profile)
	if err != nil {
		return taskOutcome{}, err
	}

	scratch, err := os.MkdirTemp("", "freeside-fake-publication-")
	if err != nil {
		return taskOutcome{}, fmt.Errorf("create publication scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // scratch was created by this invocation

	scratchInfo, err := os.Stat(scratch)
	if err != nil {
		return taskOutcome{}, fmt.Errorf("stat publication scratch: %w", err)
	}
	requestedCheckoutDir := filepath.Join(scratch, "checkout")
	checkout, err := w.transport.FetchBase(
		ctx, task.Repo, task.BaseRef, task.BaseSHA, requestedCheckoutDir,
	)
	if err != nil {
		return taskOutcome{}, fmt.Errorf("fetch exact base: %w", err)
	}
	checkoutDir, err := validatePublicationCheckout(
		checkout, task, requestedCheckoutDir, scratchInfo,
	)
	if err != nil {
		return taskOutcome{}, err
	}

	// The attended 1A.1 task predates production spec artifacts. Its operator
	// title is nevertheless immutable inside publicationRun's task digest, so
	// use that authority rather than interpreting the task JSON as spec bytes.
	run := publicationRun(task)
	commitMessage := fallbackCommitMessageFromApprovedTitle(
		task.Title, nil, task.RunID, run.SpecDigest, importPolicy,
	)
	imported, err := importer.Import(ctx, task.HandoffDir, checkoutDir, importer.Options{
		BaseSHA: task.BaseSHA, CommitMessage: commitMessage,
		CommitDate: task.CommitDate, Policy: importPolicy,
	})
	if err != nil {
		return taskOutcome{}, fmt.Errorf("gauntlet import: %w", err)
	}
	if err := requireCommittedHandoff(task); err != nil {
		return taskOutcome{}, fmt.Errorf("recheck handoff after import: %w", err)
	}
	if imported.CommitSHA == "" {
		if err := w.putBlockedItem(ctx, task, "", imported.Claims, nil, imported.CommitPlanNotice,
			"Gauntlet containment withheld a candidate commit.", nil); err != nil {
			return taskOutcome{}, err
		}
		if err := w.finishTask(ctx, task); err != nil {
			return taskOutcome{}, err
		}
		return taskOutcome{blocked: true}, nil
	}

	checkpoint, found, err := w.loadCandidateCheckpoint(task)
	if err != nil {
		return taskOutcome{}, err
	}
	if found {
		authenticated, err := w.buildCandidateCheckpoint(
			ctx, task, imported, profile, checkoutDir, scratch,
		)
		if err != nil {
			return taskOutcome{}, err
		}
		if !reflect.DeepEqual(checkpoint, authenticated) {
			return taskOutcome{}, fmt.Errorf(
				"candidate checkpoint does not match fresh verification: %w",
				domain.ErrParentKeyMismatch,
			)
		}
		checkpoint = authenticated
	} else {
		checkpoint, err = w.verifyCandidate(ctx, task, imported, profile, checkoutDir, scratch)
		if err != nil {
			return taskOutcome{}, err
		}
	}
	authorization := checkpoint.Authorization
	artifacts := checkpoint.Artifacts
	if err := w.persistCandidateMetadata(ctx, artifacts, authorization); err != nil {
		return taskOutcome{}, err
	}
	if !authorization.AuthorizesPublication {
		if err := w.putBlockedItem(ctx, task, imported.CommitSHA, imported.Claims, artifacts,
			imported.CommitPlanNotice, "Verification or policy findings blocked publication.",
			publishBlockTrustRule(domain.TrustRuleVerificationFailed)); err != nil {
			return taskOutcome{}, err
		}
		if err := w.finishTask(ctx, task); err != nil {
			return taskOutcome{}, err
		}
		return taskOutcome{blocked: true}, nil
	}

	report, err := loadVerificationReport(w.artifacts, checkpoint.Artifacts, task.VerificationInvocationID)
	if err != nil {
		return taskOutcome{}, err
	}
	candidate := fakePublicationCandidate(task, checkpoint, report)
	w.candidates[task.PublicationInvocationID] = publish.RecoveryCandidate{
		Candidate: candidate, ApprovedRecipes: maps.Clone(w.approvedRecipes),
		PublishHead: func(ctx context.Context, gated publish.GatedHead) error {
			_, err := w.transport.PushHead(ctx, checkout, gated)
			return err
		},
	}
	defer delete(w.candidates, task.PublicationInvocationID)

	identity, err := publicationIdentity(candidate)
	if err != nil {
		return taskOutcome{}, err
	}
	identityLockDir := filepath.Join(w.workDir, "identity-locks")
	if err := os.MkdirAll(identityLockDir, 0o700); err != nil {
		return taskOutcome{}, fmt.Errorf("create publication identity lock directory: %w", err)
	}
	releaseIdentity, err := acquirePublicationLock(
		ctx, identityLockDir, string(identity.Digest()),
	)
	if err != nil {
		return taskOutcome{}, err
	}
	defer func() {
		err = errors.Join(err, releaseIdentity())
	}()

	if published, found, err := w.loadPublicationOutcome(ctx, candidate); err != nil {
		return taskOutcome{}, err
	} else if found {
		return w.completePublishedTask(ctx, task, imported, artifacts, published)
	}
	published, err := w.publisher.PublishAfterGateAndFinalize(
		ctx,
		candidate,
		w.approvedRecipes,
		func(ctx context.Context, gated publish.GatedHead) error {
			_, err := w.transport.PushHead(ctx, checkout, gated)
			return err
		},
	)
	if err != nil {
		if isDefinitiveTrustRefusal(err) {
			pendingIntent, pendingErr := w.hasPendingPublicationIntent(
				ctx, task.PublicationInvocationID,
			)
			if pendingErr != nil {
				return taskOutcome{}, errors.Join(err, pendingErr)
			}
			if pendingIntent {
				return taskOutcome{}, fmt.Errorf(
					"publication intent retained with its recovery task: %w", err,
				)
			}
			if putErr := w.putBlockedItem(
				ctx, task, imported.CommitSHA, imported.Claims, artifacts,
				imported.CommitPlanNotice,
				"Current trust state definitively blocked publication.",
				publishBlockFacts(domain.HoldTrustBlocked, err),
			); putErr != nil {
				return taskOutcome{}, errors.Join(err, putErr)
			}
			if finishErr := w.finishTask(ctx, task); finishErr != nil {
				return taskOutcome{}, errors.Join(err, finishErr)
			}
			return taskOutcome{blocked: true}, nil
		}
		return taskOutcome{}, fmt.Errorf("publish candidate: %w", err)
	}
	if w.afterPublicationFinalized != nil {
		if err := w.afterPublicationFinalized(); err != nil {
			return taskOutcome{}, fmt.Errorf("after publication finalized: %w", err)
		}
	}
	return w.completePublishedTask(ctx, task, imported, artifacts, published)
}

func (w *fakePublicationWorkflow) completePublishedTask(
	ctx context.Context,
	task fakePublicationTask,
	imported importer.Result,
	artifacts []domain.Artifact,
	published publish.Result,
) (taskOutcome, error) {
	ready, err := w.readyItem(ctx, task, imported, artifacts, published)
	if err != nil {
		return taskOutcome{}, err
	}
	ready, err = bindFakePublicationTerminalItem(task, ready)
	if err != nil {
		return taskOutcome{}, fmt.Errorf("bind ready item: %w", err)
	}
	if err := w.putTerminalItem(ctx, ready); err != nil {
		return taskOutcome{}, fmt.Errorf("create ready item: %w", err)
	}
	// The §5.16 publication watches converge beside the item, from its
	// stored form (putTerminalItem may have converged onto an existing
	// version), exactly as the production lane arms them.
	var stored domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetAttentionItem(ctx, readyItemID(task.RunID))
		return err
	}); err != nil {
		return taskOutcome{}, fmt.Errorf("arm publication watches: %w", err)
	}
	if err := armPublicationWatches(ctx, w.store, stored, w.now()); err != nil {
		return taskOutcome{}, fmt.Errorf("arm publication watches: %w", err)
	}
	if err := w.finishTask(ctx, task); err != nil {
		return taskOutcome{}, err
	}
	return taskOutcome{ready: true, prNumber: published.PRNumber}, nil
}

func (w *fakePublicationWorkflow) recoverFinalizedPublication(
	ctx context.Context,
	task fakePublicationTask,
) (taskOutcome, bool, error) {
	checkpoint, found, err := w.loadCandidateCheckpoint(task)
	if err != nil || !found {
		return taskOutcome{}, false, err
	}
	if !checkpoint.Authorization.AuthorizesPublication {
		return taskOutcome{}, false, nil
	}
	report, err := loadVerificationReport(w.artifacts, checkpoint.Artifacts, task.VerificationInvocationID)
	if err != nil {
		return taskOutcome{}, false, err
	}
	published, found, err := w.loadPublicationOutcome(
		ctx, fakePublicationCandidate(task, checkpoint, report),
	)
	if err != nil || !found {
		return taskOutcome{}, found, err
	}
	outcome, err := w.completePublishedTask(
		ctx, task, checkpoint.Imported, checkpoint.Artifacts, published,
	)
	return outcome, true, err
}

func fakePublicationCandidate(
	task fakePublicationTask,
	checkpoint fakePublicationCandidateCheckpoint,
	report []byte,
) publish.Candidate {
	recipeDigest := task.RecipeDigest
	authorizationID := checkpoint.Authorization.ID
	profileDigest := task.TrustProfileDigest
	return publish.Candidate{
		Repo: task.Repo, BaseRef: task.BaseRef, HeadSHA: checkpoint.Imported.CommitSHA,
		Title: task.Title, Body: task.Body, Artifacts: checkpoint.Artifacts,
		VerificationReport: report, ImportResult: &checkpoint.Imported,
		Advisories:   publish.AdvisoryFindings(checkpoint.Authorization.Findings),
		RecipeDigest: &recipeDigest, InvocationID: task.PublicationInvocationID,
		RunID:           task.RunID,
		AuthorizationID: &authorizationID, TrustProfileDigest: &profileDigest,
	}
}

func (w *fakePublicationWorkflow) loadPublicationOutcome(
	ctx context.Context,
	candidate publish.Candidate,
) (publish.Result, bool, error) {
	identity, err := publicationIdentity(candidate)
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
			"publication intent %q read back key %q: %w",
			intentKey, entry.IdempotencyKey, domain.ErrParentKeyMismatch,
		)
	}
	if entry.Kind == publish.IntentKindReservation {
		// The key still holds this task's reservation, so nothing has been
		// published under this invocation yet. Reading that as an error would
		// break every reconcile, since admission is what put it there.
		held, err := publish.DecodeReservation(entry.Payload)
		if err != nil {
			return publish.Result{}, false, err
		}
		if held.InvocationID != candidate.InvocationID || held.RunID != candidate.RunID {
			return publish.Result{}, false, fmt.Errorf(
				"publication invocation %q is reserved by run %q: %w",
				intentKey, held.RunID, domain.ErrParentKeyMismatch,
			)
		}
		return publish.Result{}, false, nil
	}
	if entry.Kind != publish.IntentKindPublication {
		return publish.Result{}, false, fmt.Errorf(
			"publication intent %q has kind %q: %w",
			intentKey, entry.Kind, domain.ErrParentKeyMismatch,
		)
	}
	if !entry.Dispatched() {
		return publish.Result{}, false, nil
	}
	intent, err := publish.DecodeStoredIntent(entry)
	if err != nil {
		return publish.Result{}, false, err
	}
	if intent.InvocationID != candidate.InvocationID || intent.Identity != identity.Digest() ||
		intent.Repo != candidate.Repo || intent.BaseRef != candidate.BaseRef ||
		intent.SourceHeadSHA != candidate.HeadSHA ||
		(candidate.AuthorizationID != nil && intent.AuthorizationID != *candidate.AuthorizationID) {
		return publish.Result{}, false, fmt.Errorf(
			"publication intent disagrees with candidate: %w", domain.ErrParentKeyMismatch,
		)
	}
	if err := w.validatePublicationOutcomeAuthorization(
		ctx, candidate, intent.AuthorizationID,
	); err != nil {
		return publish.Result{}, false, err
	}
	outcome, found, err := publish.LoadOutcome(
		ctx, w.store, candidate, w.approvedRecipes, w.publisher.VerifyOutcome,
	)
	if err != nil || !found {
		return publish.Result{}, found, err
	}
	if outcome.Repo != candidate.Repo || outcome.BaseRef != candidate.BaseRef ||
		outcome.HeadSHA != candidate.HeadSHA {
		return publish.Result{}, false, fmt.Errorf(
			"publication outcome disagrees with candidate: %w", domain.ErrParentKeyMismatch,
		)
	}
	return publish.Result{
		Identity: identity, Branch: outcome.Branch, PRNumber: outcome.PRNumber,
	}, true, nil
}

func (w *fakePublicationWorkflow) validatePublicationOutcomeAuthorization(
	ctx context.Context,
	candidate publish.Candidate,
	authorizationID domain.Digest,
) error {
	if candidate.RecipeDigest == nil {
		return errors.New("publication outcome candidate has no recipe binding")
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(candidate.Artifacts)
	if err != nil {
		return fmt.Errorf("digest publication outcome evidence: %w", err)
	}
	var authorization domain.CandidateAuthorization
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		authorization, err = tx.GetCandidateAuthorization(ctx, authorizationID)
		return err
	}); err != nil {
		return fmt.Errorf("load publication outcome authorization %s: %w",
			authorizationID, err)
	}
	if err := authorization.Validate(); err != nil {
		return fmt.Errorf("publication outcome authorization %s: %w",
			authorizationID, err)
	}
	if authorization.ID != authorizationID ||
		authorization.Repo != candidate.Repo ||
		authorization.HeadSHA != candidate.HeadSHA ||
		authorization.VerificationRecipeDigest != *candidate.RecipeDigest ||
		authorization.EvidenceSnapshotDigest != evidenceDigest ||
		(candidate.TrustProfileDigest != nil &&
			authorization.TrustProfileDigest != *candidate.TrustProfileDigest) ||
		!authorization.AuthorizesPublication {
		return fmt.Errorf(
			"publication outcome authorization disagrees with evidence: %w",
			domain.ErrParentKeyMismatch,
		)
	}
	return nil
}

// Resolve implements publish.CandidateResolver for the publication intent
// being finalized in the same reconciliation pass. The candidate is rebuilt
// from the durable task on every pass before any drain, so a restart never
// trusts process-local state as workflow authority.
func (w *fakePublicationWorkflow) Resolve(
	_ context.Context,
	intent publish.Intent,
) (publish.RecoveryCandidate, error) {
	candidate, ok := w.candidates[intent.InvocationID]
	if !ok {
		return publish.RecoveryCandidate{}, fmt.Errorf(
			"no rebuilt candidate for invocation %q", intent.InvocationID,
		)
	}
	candidate.ApprovedRecipes = maps.Clone(candidate.ApprovedRecipes)
	return candidate, nil
}

func (w *fakePublicationWorkflow) commitHandoff(
	task fakePublicationTask,
	workspace fs.FS,
) (domain.Digest, error) {
	parent := filepath.Dir(task.HandoffDir)
	if err := makeFakePublicationDirectory(parent, 0o700); err != nil {
		return "", fmt.Errorf("create handoff parent: %w", err)
	}
	temp, err := os.MkdirTemp(parent, ".handoff-")
	if err != nil {
		return "", fmt.Errorf("create handoff scratch: %w", err)
	}
	output := filepath.Join(temp, "output")
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temp)
		}
	}()
	if _, err := exporter.Export(workspace, output, exporter.Options{
		MaxBlobBytes:          importer.DefaultMaxBlobBytes,
		MaxTotalBlobBytes:     importer.DefaultMaxTotalBytes,
		MaxEntries:            importer.DefaultMaxEntries,
		MaxEvidenceBlobBytes:  importer.DefaultMaxBlobBytes,
		MaxEvidenceTotalBytes: importer.DefaultMaxTotalBytes,
	}); err != nil {
		return "", fmt.Errorf("export helper: %w", err)
	}
	if err := syncFakePublicationTree(output); err != nil {
		return "", fmt.Errorf("sync handoff: %w", err)
	}
	digest, err := digestFakePublicationTree(output)
	if err != nil {
		return "", fmt.Errorf("digest exported handoff: %w", err)
	}
	if _, err := os.Lstat(task.HandoffDir); err == nil {
		existingDigest, digestErr := digestFakePublicationTree(task.HandoffDir)
		switch {
		case digestErr != nil:
			if rollbackErr := rollbackFakePublicationHandoff(task.HandoffDir); rollbackErr != nil {
				return "", errors.Join(
					fmt.Errorf("inspect orphaned handoff: %w", digestErr),
					rollbackErr,
				)
			}
		case existingDigest == digest:
			if err := atomicfile.SyncDir(parent); err != nil {
				return "", fmt.Errorf("sync adopted handoff directory entry: %w", err)
			}
			return digest, nil
		default:
			if err := rollbackFakePublicationHandoff(task.HandoffDir); err != nil {
				return "", fmt.Errorf("replace divergent orphaned handoff: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect handoff target: %w", err)
	}
	if err := installFakePublicationHandoff(
		output, task.HandoffDir, atomicfile.SyncDir,
	); err != nil {
		return "", err
	}
	committed = true
	_ = os.RemoveAll(temp)
	return digest, nil
}

func rollbackFakePublicationHandoff(path string) error {
	return rollbackFakePublicationHandoffWithSync(path, atomicfile.SyncDir)
}

func installFakePublicationHandoff(
	output, destination string,
	syncDir func(string) error,
) error {
	if err := os.Rename(output, destination); err != nil {
		return fmt.Errorf("commit handoff: %w", err)
	}
	if err := syncDir(filepath.Dir(destination)); err != nil {
		return errors.Join(
			fmt.Errorf("commit handoff directory entry: %w", err),
			rollbackFakePublicationHandoffWithSync(destination, syncDir),
		)
	}
	return nil
}

func rollbackFakePublicationHandoffWithSync(
	path string,
	syncDir func(string) error,
) error {
	removeErr := os.RemoveAll(path)
	syncErr := syncDir(filepath.Dir(path))
	if removeErr != nil {
		removeErr = fmt.Errorf("roll back committed handoff: %w", removeErr)
	}
	if syncErr != nil {
		syncErr = fmt.Errorf("sync rolled-back handoff directory entry: %w", syncErr)
	}
	return errors.Join(removeErr, syncErr)
}

func requireCommittedHandoff(task fakePublicationTask) error {
	info, err := os.Lstat(task.HandoffDir)
	if err != nil {
		return fmt.Errorf("inspect committed handoff: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("committed handoff path is not a real directory")
	}
	digest, err := digestFakePublicationTree(task.HandoffDir)
	if err != nil {
		return fmt.Errorf("digest committed handoff: %w", err)
	}
	if digest != task.HandoffDigest {
		return fmt.Errorf("committed handoff digest %s, want %s: %w",
			digest, task.HandoffDigest, domain.ErrParentKeyMismatch)
	}
	return nil
}

func (w *fakePublicationWorkflow) loadBoundProfile(
	ctx context.Context,
	task fakePublicationTask,
) (domain.AutomationTrustProfile, error) {
	var profile domain.AutomationTrustProfile
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		profile, err = tx.GetTrustProfile(ctx, task.TrustProfileDigest)
		return err
	})
	if err != nil {
		return domain.AutomationTrustProfile{}, fmt.Errorf("load bound trust profile: %w", err)
	}
	if profile.Repo != task.Repo {
		return domain.AutomationTrustProfile{}, fmt.Errorf("bound profile names %q, want %q: %w",
			profile.Repo, task.Repo, domain.ErrParentKeyMismatch)
	}
	return profile, nil
}

func (w *fakePublicationWorkflow) verifyCandidate(
	ctx context.Context,
	task fakePublicationTask,
	imported importer.Result,
	profile domain.AutomationTrustProfile,
	checkoutDir, scratch string,
) (fakePublicationCandidateCheckpoint, error) {
	checkpoint, err := w.buildCandidateCheckpoint(
		ctx, task, imported, profile, checkoutDir, scratch,
	)
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, err
	}
	return w.installCandidateCheckpoint(task, imported, checkpoint)
}

func (w *fakePublicationWorkflow) buildCandidateCheckpoint(
	ctx context.Context,
	task fakePublicationTask,
	imported importer.Result,
	profile domain.AutomationTrustProfile,
	checkoutDir, scratch string,
) (fakePublicationCandidateCheckpoint, error) {
	home := filepath.Join(scratch, "verify-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return fakePublicationCandidateCheckpoint{}, fmt.Errorf("create verification home: %w", err)
	}
	recipe, err := loadFakePublicationRecipe(w.artifacts, task.RecipeDigest)
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, err
	}
	verified, err := verify.Verify(ctx, checkoutDir, verify.Options{
		HeadSHA: imported.CommitSHA, BaseSHA: task.BaseSHA,
		InvocationID: task.VerificationInvocationID,
		RecipeSource: verify.ConfigRecipe(recipe), RecipePath: task.RecipePath,
		Room: w.newRoom(home), ApprovedRecipes: w.approvedRecipes, Changes: imported.Changes,
		// A stable per-task time, not a wall clock: the emitted evidence is
		// content-addressed and write-once, and this checkpoint can re-run, so
		// the evidence metadata must converge byte-identically on replay
		// (mirrors the checkpoint's CandidateAuthorization CreatedAt).
		Now: task.StartedAt,
		Policy: verify.Policy{ExtraVerificationControlPatterns: slices.Clone(
			profile.ProtectedPaths.ExtraVerificationControlPatterns,
		)},
	})
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, fmt.Errorf("clean verification: %w", err)
	}
	if verified.HeadSHA != imported.CommitSHA || verified.RecipeDigest != task.RecipeDigest {
		return fakePublicationCandidateCheckpoint{}, fmt.Errorf(
			"verification result disagrees with task binding: %w", domain.ErrParentKeyMismatch)
	}
	artifacts := make([]domain.Artifact, len(verified.Evidence))
	for i, item := range verified.Evidence {
		artifacts[i] = item.Artifact
		if _, err := w.artifacts.Put(item.Artifact.Digest, bytes.NewReader(item.Content)); err != nil {
			return fakePublicationCandidateCheckpoint{}, fmt.Errorf(
				"persist evidence artifact %s: %w", item.Artifact.ID, err)
		}
		if err := verifyArtifactBlob(w.artifacts, item.Artifact); err != nil {
			return fakePublicationCandidateCheckpoint{}, err
		}
	}
	outcome := domain.VerificationFailed
	if verified.Outcome == verify.OutcomePassed {
		outcome = domain.VerificationPassed
	}
	importDigest, err := digestJSON(imported)
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, fmt.Errorf("digest import account: %w", err)
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(artifacts)
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, fmt.Errorf("digest evidence snapshot: %w", err)
	}
	authorization, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: task.Repo, BaseSHA: task.BaseSHA, HeadSHA: imported.CommitSHA,
		ImportResultDigest: importDigest, VerificationRecipeDigest: verified.RecipeDigest,
		EvidenceSnapshotDigest: evidenceDigest, VerificationOutcome: outcome,
		Findings:           candidateFindings(imported.Findings, verified.Findings),
		TrustProfileDigest: task.TrustProfileDigest,
		InvocationID:       task.VerificationInvocationID, CreatedAt: task.StartedAt,
	})
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, fmt.Errorf("construct candidate authorization: %w", err)
	}
	checkpoint := fakePublicationCandidateCheckpoint{
		Version: "freeside.fake-publication-candidate/v3",
		TaskKey: fakePublicationTaskKey(task.RunID), Imported: imported,
		Authorization: authorization, Artifacts: artifacts,
	}
	return checkpoint, nil
}

func (w *fakePublicationWorkflow) installCandidateCheckpoint(
	task fakePublicationTask,
	imported importer.Result,
	checkpoint fakePublicationCandidateCheckpoint,
) (fakePublicationCandidateCheckpoint, error) {
	body, err := json.Marshal(checkpoint)
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, err
	}
	path := w.candidateCheckpointPath(task)
	parent := filepath.Dir(path)
	if err := makeFakePublicationDirectory(parent, 0o700); err != nil {
		return fakePublicationCandidateCheckpoint{}, err
	}
	temp, err := os.CreateTemp(parent, ".candidate-")
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, err
	}
	tempName := temp.Name()
	defer os.Remove(tempName) //nolint:errcheck // scratch cleanup cannot repair a completed checkpoint
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return fakePublicationCandidateCheckpoint{}, err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fakePublicationCandidateCheckpoint{}, err
	}
	if err := temp.Close(); err != nil {
		return fakePublicationCandidateCheckpoint{}, err
	}
	installed, err := installFakePublicationCheckpoint(tempName, path)
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, err
	}
	if !installed {
		existing, found, err := w.loadCandidateCheckpoint(task)
		if err != nil {
			return fakePublicationCandidateCheckpoint{}, err
		}
		if !found {
			return fakePublicationCandidateCheckpoint{}, errors.New(
				"candidate checkpoint disappeared after concurrent installation",
			)
		}
		if !reflect.DeepEqual(existing, checkpoint) {
			return fakePublicationCandidateCheckpoint{}, fmt.Errorf(
				"concurrent candidate checkpoint disagrees with fresh verification: %w",
				domain.ErrParentKeyMismatch,
			)
		}
		return existing, nil
	}
	if err := atomicfile.SyncDir(parent); err != nil {
		return fakePublicationCandidateCheckpoint{}, err
	}
	return checkpoint, nil
}

// installFakePublicationCheckpoint atomically publishes one immutable
// checkpoint without replacing a winner installed by a concurrent process.
func installFakePublicationCheckpoint(source, destination string) (bool, error) {
	if err := os.Link(source, destination); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (w *fakePublicationWorkflow) loadCandidateCheckpoint(
	task fakePublicationTask,
) (fakePublicationCandidateCheckpoint, bool, error) {
	path := w.candidateCheckpointPath(task)
	body, err := os.ReadFile(path) //nolint:gosec // path derives from the task-bound daemon work root
	if errors.Is(err, os.ErrNotExist) {
		return fakePublicationCandidateCheckpoint{}, false, nil
	}
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, false, err
	}
	if err := syncFakePublicationCheckpointDirectory(path, atomicfile.SyncDir); err != nil {
		return fakePublicationCandidateCheckpoint{}, false,
			fmt.Errorf("sync existing candidate checkpoint directory: %w", err)
	}
	var checkpoint fakePublicationCandidateCheckpoint
	if err := strictjson.Decode(body, &checkpoint, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return fakePublicationCandidateCheckpoint{}, false,
				errors.New("candidate checkpoint has trailing content")
		}
		return fakePublicationCandidateCheckpoint{}, false, err
	}
	importDigest, err := digestJSON(checkpoint.Imported)
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, false, err
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(checkpoint.Artifacts)
	if err != nil {
		return fakePublicationCandidateCheckpoint{}, false, err
	}
	a := checkpoint.Authorization
	if checkpoint.Version != "freeside.fake-publication-candidate/v3" ||
		checkpoint.TaskKey != fakePublicationTaskKey(task.RunID) || a.Validate() != nil ||
		a.Repo != task.Repo || a.BaseSHA != task.BaseSHA ||
		a.HeadSHA != checkpoint.Imported.CommitSHA ||
		a.ImportResultDigest != importDigest || a.VerificationRecipeDigest != task.RecipeDigest ||
		a.EvidenceSnapshotDigest != evidenceDigest ||
		a.TrustProfileDigest != task.TrustProfileDigest ||
		a.InvocationID != task.VerificationInvocationID || !a.CreatedAt.Equal(task.StartedAt) ||
		len(checkpoint.Artifacts) != 2 {
		return fakePublicationCandidateCheckpoint{}, false,
			fmt.Errorf("candidate checkpoint disagrees with task: %w", domain.ErrParentKeyMismatch)
	}
	for _, artifact := range checkpoint.Artifacts {
		if err := domain.ValidatePublishEligibility(artifact, w.approvedRecipes); err != nil {
			return fakePublicationCandidateCheckpoint{}, false, err
		}
		provenance := artifact.Provenance
		if provenance.ProducerClass != domain.ProducerVerifier ||
			provenance.ProducerInvocationID != task.VerificationInvocationID ||
			provenance.SourceHeadSHA != checkpoint.Imported.CommitSHA ||
			provenance.VerificationRecipeDigest == nil ||
			*provenance.VerificationRecipeDigest != task.RecipeDigest {
			return fakePublicationCandidateCheckpoint{}, false, domain.ErrEvidenceHeadMismatch
		}
		if err := verifyArtifactBlob(w.artifacts, artifact); err != nil {
			return fakePublicationCandidateCheckpoint{}, false, err
		}
	}
	return checkpoint, true, nil
}

func syncFakePublicationCheckpointDirectory(
	checkpointPath string,
	syncDir func(string) error,
) error {
	return syncDir(filepath.Dir(checkpointPath))
}

func (w *fakePublicationWorkflow) candidateCheckpointPath(task fakePublicationTask) string {
	return filepath.Join(w.workDir, "candidates", filepath.Base(task.HandoffDir)+".json")
}

// makeFakePublicationDirectory re-syncs the deepest existing boundary, then
// creates every missing directory one level at a time and syncs its parent
// before proceeding. A later file or directory fsync cannot make an unsynced
// ancestor entry durable on its own.
func makeFakePublicationDirectory(path string, mode fs.FileMode) error {
	return makeFakePublicationDirectoryWithSync(path, mode, atomicfile.SyncDir)
}

func makeFakePublicationDirectoryWithSync(
	path string,
	mode fs.FileMode,
	syncDir func(string) error,
) error {
	var missing []string
	var existing string
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", current)
			}
			existing = current
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		if parent := filepath.Dir(current); parent == current {
			return fmt.Errorf("no existing ancestor for %s", path)
		}
	}
	if err := syncDir(filepath.Dir(existing)); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], mode); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, statErr := os.Stat(missing[i])
			if statErr != nil {
				return statErr
			}
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", missing[i])
			}
		}
		if err := syncDir(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	return nil
}

// syncFakePublicationTree finalizes an exported handoff bottom-up before its
// directory is renamed into the durable task binding.
func syncFakePublicationTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		switch {
		case entry.IsDir():
			dirs = append(dirs, path)
			return nil
		case entry.Type().IsRegular():
			file, err := os.Open(path) //nolint:gosec // private exporter output rooted at root
			if err != nil {
				return err
			}
			syncErr := file.Sync()
			return errors.Join(syncErr, file.Close())
		default:
			return fmt.Errorf("handoff output %s is not a regular file or directory", path)
		}
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := atomicfile.SyncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func (w *fakePublicationWorkflow) persistCandidateMetadata(
	ctx context.Context,
	artifacts []domain.Artifact,
	authorization domain.CandidateAuthorization,
) error {
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		for _, artifact := range artifacts {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		return tx.RecordCandidateAuthorization(ctx, authorization)
	})
}

func (w *fakePublicationWorkflow) readyItem(
	ctx context.Context,
	task fakePublicationTask,
	imported importer.Result,
	artifacts []domain.Artifact,
	published publish.Result,
) (domain.AttentionItem, error) {
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	subject := domain.Subject{
		Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
	}
	names, err := displayNames(ctx, w.store, task.ProjectID, subject)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: readyItemID(task.RunID), ProjectID: task.ProjectID,
		Subject: subject,
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: fmt.Sprintf("%s#%d is published and ready for final review.",
			task.Repo, published.PRNumber),
		RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionMarkSeen, domain.ActionDismiss, domain.ActionStop,
		},
		EvidenceSnapshot: artifacts, AgentClaims: imported.Claims,
		PRHeadSHA: imported.CommitSHA,
		PRReference: &domain.PRReference{
			Repo: task.Repo, Number: published.PRNumber,
		},
		CommitPlanNotice: imported.CommitPlanNotice,
		DisplayNames:     names,
		ItemVersion:      1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &createdAt,
		Status:    domain.StatusOpen,
	}, w.approvedRecipes)
}

func (w *fakePublicationWorkflow) putBlockedItem(
	ctx context.Context,
	task fakePublicationTask,
	headSHA string,
	claims []domain.AgentClaim,
	artifacts []domain.Artifact,
	notice *domain.CommitPlanNoticeReason,
	reason string,
	facts *domain.PublishBlockFacts,
) error {
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	subject := domain.Subject{
		Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
	}
	names, err := displayNames(ctx, w.store, task.ProjectID, subject)
	if err != nil {
		return err
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: blockedItemID(task.RunID), ProjectID: task.ProjectID,
		Subject: subject,
		Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
		Reason: reason,
		RequestedDecision: []domain.Action{
			domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
		},
		EvidenceSnapshot: artifacts, AgentClaims: claims,
		PRHeadSHA: headSHA, CommitPlanNotice: notice,
		PublishBlock: facts,
		DisplayNames: names,
		ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt,
		Status:    domain.StatusOpen,
	}, w.approvedRecipes)
	if err != nil {
		return fmt.Errorf("construct publish-blocked item: %w", err)
	}
	item, err = bindFakePublicationTerminalItem(task, item)
	if err != nil {
		return fmt.Errorf("bind publish-blocked item: %w", err)
	}
	if err := w.putTerminalItem(ctx, item); err != nil {
		return fmt.Errorf("create publish-blocked item: %w", err)
	}
	return nil
}

func (w *fakePublicationWorkflow) putTerminalItem(
	ctx context.Context,
	item domain.AttentionItem,
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
		current, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	}); readErr != nil {
		return errors.Join(err, readErr)
	}
	if !compatibleTerminalItem(item, current) {
		return err
	}
	return nil
}

func (w *fakePublicationWorkflow) recoverTerminalTask(
	ctx context.Context,
	task fakePublicationTask,
) (taskOutcome, bool, error) {
	var items []domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		for _, id := range []domain.ItemID{
			readyItemID(task.RunID), blockedItemID(task.RunID),
		} {
			item, err := tx.GetAttentionItem(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			items = append(items, item)
		}
		return nil
	}); err != nil {
		return taskOutcome{}, false, err
	}
	if len(items) == 0 {
		return taskOutcome{}, false, nil
	}
	if len(items) != 1 {
		return taskOutcome{}, false, fmt.Errorf(
			"run %q has multiple terminal publication items", task.RunID,
		)
	}
	item := items[0]
	if item.ProjectID != task.ProjectID || item.Subject.Type != domain.SubjectRun ||
		item.Subject.ID != domain.SubjectID(task.RunID) || item.Subject.RunID == nil ||
		*item.Subject.RunID != task.RunID {
		return taskOutcome{}, false, fmt.Errorf(
			"terminal publication item %q does not match task: %w",
			item.ID, domain.ErrParentKeyMismatch,
		)
	}
	for _, artifact := range item.EvidenceSnapshot {
		if err := verifyArtifactBlob(w.artifacts, artifact); err != nil {
			return taskOutcome{}, false, fmt.Errorf(
				"terminal item %q evidence: %w", item.ID, err,
			)
		}
	}
	outcome := taskOutcome{}
	switch item.Type {
	case domain.AttentionPublishBlocked:
		if item.ID != blockedItemID(task.RunID) {
			return taskOutcome{}, false, fmt.Errorf(
				"blocked item has unexpected id %q: %w", item.ID, domain.ErrParentKeyMismatch,
			)
		}
		if _, err := validateFakePublicationTerminalBinding(task, item); err != nil {
			return taskOutcome{}, false, fmt.Errorf(
				"blocked item %q: %w", item.ID, err,
			)
		}
		outcome.blocked = true
	case domain.AttentionReadyForFinalReview:
		if item.ID != readyItemID(task.RunID) {
			return taskOutcome{}, false, fmt.Errorf(
				"ready item has unexpected id %q: %w", item.ID, domain.ErrParentKeyMismatch,
			)
		}
		boundItem, err := validateFakePublicationTerminalBinding(task, item)
		if err != nil {
			return taskOutcome{}, false, fmt.Errorf(
				"ready item %q: %w", item.ID, err,
			)
		}
		item = boundItem
		prNumber := item.PRReference.Number
		if item.PRReference.Repo != task.Repo {
			return taskOutcome{}, false, fmt.Errorf(
				"ready item %q names repository %q, want %q",
				item.ID, item.PRReference.Repo, task.Repo,
			)
		}
		recipeDigest := task.RecipeDigest
		profileDigest := task.TrustProfileDigest
		published, found, err := w.loadPublicationOutcome(ctx, publish.Candidate{
			Repo: task.Repo, BaseRef: task.BaseRef, HeadSHA: item.PRHeadSHA,
			Artifacts: item.EvidenceSnapshot, RecipeDigest: &recipeDigest,
			TrustProfileDigest: &profileDigest,
			InvocationID:       task.PublicationInvocationID,
			RunID:              task.RunID,
		})
		if err != nil {
			return taskOutcome{}, false, err
		}
		if !found || published.PRNumber != prNumber {
			return taskOutcome{}, false, fmt.Errorf(
				"ready item %q has no matching publication outcome", item.ID,
			)
		}
		outcome.ready = true
		outcome.prNumber = prNumber
	default:
		return taskOutcome{}, false, fmt.Errorf(
			"terminal item %q has unexpected type %q", item.ID, item.Type,
		)
	}
	if err := w.finishTask(ctx, task); err != nil {
		return taskOutcome{}, false, err
	}
	return outcome, true, nil
}

const (
	fakePublicationTerminalBindingPrefix = "<!-- freeside:fake-publication-terminal="
	fakePublicationTerminalBindingSuffix = " -->"
)

func bindFakePublicationTerminalItem(
	task fakePublicationTask,
	item domain.AttentionItem,
) (domain.AttentionItem, error) {
	return fakepublication.BindTerminal(fakepublication.Task(task), item)
}

func validateFakePublicationTerminalBinding(
	task fakePublicationTask,
	item domain.AttentionItem,
) (domain.AttentionItem, error) {
	return fakepublication.ValidateTerminalBinding(fakepublication.Task(task), item)
}

// ParseFakePublicationTerminalReason separates the human-facing reason from
// the engine's hidden task binding. Recovery validates the returned digest;
// clients use the reason only after reconciliation has accepted the terminal.
func ParseFakePublicationTerminalReason(reason string) (string, string, bool) {
	return fakepublication.ParseTerminalReason(reason)
}

func fakePublicationTerminalDigest(
	task fakePublicationTask,
	item domain.AttentionItem,
) (domain.Digest, error) {
	return fakepublication.TerminalDigest(fakepublication.Task(task), item)
}

// fakePublicationTerminalDigestBeforePRReference reproduces the immutable
// item shape used by fake-publication v1 before AttentionItem gained the
// pr_reference member. Keep this explicit field list frozen: reconstructing
// the old digest from today's struct would silently include future members.
func fakePublicationTerminalDigestBeforePRReference(
	task fakePublicationTask,
	item domain.AttentionItem,
) (domain.Digest, error) {
	return fakepublication.TerminalDigestBeforePRReference(fakepublication.Task(task), item)
}

func (w *fakePublicationWorkflow) finishTask(ctx context.Context, task fakePublicationTask) error {
	return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, fakePublicationTaskKey(task.RunID))
	})
}

func (w *fakePublicationWorkflow) hasPendingPublicationIntent(
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
	for _, entry := range pending {
		if entry.IdempotencyKey == key {
			return true, nil
		}
	}
	return false, nil
}

func (w *fakePublicationWorkflow) expectedHandoffDir(task fakePublicationTask) (string, error) {
	return expectedFakePublicationHandoffDir(w.workDir, task)
}

func expectedFakePublicationHandoffDir(
	workDir string,
	task fakePublicationTask,
) (string, error) {
	task.HandoffDir = ""
	task.HandoffDigest = ""
	task.StartedAt = time.Time{}
	task.TrustProfileDigest = ""
	if !task.CommitDateExplicit {
		task.CommitDate = time.Time{}
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return "", fmt.Errorf("encode handoff binding: %w", err)
	}
	sum := sha256.Sum256(payload)
	return filepath.Join(workDir, "handoffs", hex.EncodeToString(sum[:])), nil
}

func fakePublicationTaskWorkDir(task fakePublicationTask) (string, error) {
	workDir := filepath.Dir(filepath.Dir(task.HandoffDir))
	expected, err := expectedFakePublicationHandoffDir(workDir, task)
	if err != nil {
		return "", err
	}
	if task.HandoffDir != expected {
		return "", fmt.Errorf("handoff %q, want %q: %w",
			task.HandoffDir, expected, domain.ErrParentKeyMismatch)
	}
	return workDir, nil
}

func openValidatedFakePublicationWorkspace(
	workspaceDir string,
	protectedRoots []string,
) (*os.Root, error) {
	workspace, err := os.OpenRoot(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	closeWithError := func(err error) (*os.Root, error) {
		return nil, errors.Join(err, workspace.Close())
	}
	pinned, err := workspace.Stat(".")
	if err != nil {
		return closeWithError(fmt.Errorf("stat opened workspace root: %w", err))
	}
	resolved, err := filepath.EvalSymlinks(workspaceDir)
	if err != nil {
		return closeWithError(fmt.Errorf("resolve workspace links: %w", err))
	}
	current, err := os.Stat(resolved)
	if err != nil {
		return closeWithError(fmt.Errorf("stat resolved workspace: %w", err))
	}
	if !os.SameFile(pinned, current) {
		return closeWithError(errors.New("workspace changed while opening its root"))
	}
	if err := validateFakePublicationWorkspace(resolved); err != nil {
		return closeWithError(err)
	}
	for _, protected := range protectedRoots {
		if err := validateFakePublicationPathSeparation(resolved, protected); err != nil {
			return closeWithError(fmt.Errorf(
				"workspace overlaps daemon-owned root %q: %w", protected, err,
			))
		}
	}
	current, err = os.Stat(resolved)
	if err != nil {
		return closeWithError(fmt.Errorf("restat resolved workspace: %w", err))
	}
	if !os.SameFile(pinned, current) {
		return closeWithError(errors.New("workspace changed during validation"))
	}
	return workspace, nil
}

func validateFakePublicationWorkspace(workspaceDir string) error {
	info, err := os.Stat(workspaceDir)
	if err != nil {
		return fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return errors.New("workspace is not a directory")
	}
	return nil
}

func validateFakePublicationPathSeparation(workspaceDir, workDir string) error {
	workspace, err := filepath.EvalSymlinks(workspaceDir)
	if err != nil {
		return fmt.Errorf("resolve workspace links: %w", err)
	}
	work, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return fmt.Errorf("resolve work directory links: %w", err)
	}
	if fakePublicationPathContains(workspace, work) ||
		fakePublicationPathContains(work, workspace) {
		return errors.New("workspace and publication work directory overlap")
	}
	return nil
}

func fakePublicationPathContains(outer, inner string) bool {
	outerInfo, err := os.Stat(outer)
	if err != nil {
		return false
	}
	for current := inner; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return false
		}
		if os.SameFile(outerInfo, info) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

// FakePublicationReadyItemID returns the workflow-owned ready-item namespace
// for a run.
func FakePublicationReadyItemID(runID domain.RunID) domain.ItemID {
	return fakepublication.ReadyItemID(runID)
}

// FakePublicationBlockedItemID returns the workflow-owned blocked-item
// namespace for a run.
func FakePublicationBlockedItemID(runID domain.RunID) domain.ItemID {
	return fakepublication.BlockedItemID(runID)
}

func (task fakePublicationTask) validate() error {
	return fakepublication.Task(task).Validate()
}

func validateFakePublicationCommitDate(commitDate time.Time) error {
	return fakepublication.ValidateCommitDate(commitDate)
}

func validateFakePublicationTaskUTF8(task fakePublicationTask) error {
	return fakepublication.ValidateTaskUTF8(fakepublication.Task(task))
}

func sameFakePublicationRequest(
	prior, proposed fakePublicationTask,
	explicitCommitDate bool,
) bool {
	if !explicitCommitDate {
		proposed.CommitDate = prior.CommitDate
	}
	proposed.StartedAt = prior.StartedAt
	// HandoffDir is derived from the complete committed task and verified
	// during reconciliation; it is not a caller-controlled request field.
	// TrustProfileDigest deliberately stays out of the request comparison.
	// A replay re-reads current trust only to construct a possible new task,
	// but an existing run remains bound to its originally reviewed profile;
	// Publisher's fresh drift gate then refuses a superseded binding before
	// the transport callback can push.
	return prior.Version == proposed.Version &&
		prior.RunID == proposed.RunID &&
		prior.ProjectID == proposed.ProjectID &&
		prior.WorkspaceDir == proposed.WorkspaceDir &&
		prior.Repo == proposed.Repo &&
		prior.BaseRef == proposed.BaseRef &&
		prior.BaseSHA == proposed.BaseSHA &&
		slices.Equal(prior.AllowedPaths, proposed.AllowedPaths) &&
		prior.RecipeDigest == proposed.RecipeDigest &&
		prior.RecipePath == proposed.RecipePath &&
		prior.VerificationInvocationID == proposed.VerificationInvocationID &&
		prior.PublicationInvocationID == proposed.PublicationInvocationID &&
		prior.Title == proposed.Title &&
		prior.Body == proposed.Body &&
		prior.CommitDateExplicit == proposed.CommitDateExplicit &&
		prior.CommitDate.Equal(proposed.CommitDate) &&
		prior.StartedAt.Equal(proposed.StartedAt) &&
		prior.OperatingMode == proposed.OperatingMode
}

func digestFakePublicationTree(root string) (domain.Digest, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			_, _ = fmt.Fprintf(hash, "dir\x00%s\x00%o\x00", relative, info.Mode().Perm())
			return nil
		case entry.Type().IsRegular():
			file, err := os.OpenFile( //nolint:gosec // daemon-owned handoff opened without following links
				name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0,
			)
			if err != nil {
				return err
			}
			opened, err := file.Stat()
			if err != nil {
				_ = file.Close()
				return err
			}
			if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
				_ = file.Close()
				return errors.New("handoff member changed while opening")
			}
			_, _ = fmt.Fprintf(
				hash, "file\x00%s\x00%o\x00%d\x00",
				relative, opened.Mode().Perm(), opened.Size(),
			)
			written, copyErr := io.Copy(hash, file)
			after, statErr := file.Stat()
			closeErr := file.Close()
			if err := errors.Join(copyErr, statErr, closeErr); err != nil {
				return err
			}
			if written != opened.Size() || !os.SameFile(opened, after) ||
				after.Size() != opened.Size() ||
				!after.ModTime().Equal(opened.ModTime()) {
				return errors.New("handoff member changed while hashing")
			}
			return nil
		default:
			return fmt.Errorf(
				"handoff member %q is not a regular file or directory",
				relative,
			)
		}
	})
	if err != nil {
		return "", err
	}
	return domain.Digest(contentaddr.Format(hash.Sum(nil))), nil
}

// validateFakePublicationAllowlist mirrors the importer's slash-separated
// glob grammar at admission. A deterministic caller error must not become a
// durable outbox row that fails identically on every reconciliation.
func validateFakePublicationAllowlist(patterns []string) error {
	return fakepublication.ValidateAllowlist(patterns)
}

func validateFakePublicationRecipePath(recipePath string) error {
	return fakepublication.ValidateRecipePath(recipePath)
}

// FakePublicationReplay is the durable bootstrap state a one-shot command
// needs before replaying its original request.
type FakePublicationReplay struct {
	Recipe       []byte
	WorkspaceDir string
	WorkDir      string
	Dispatched   bool
}

// FakePublicationReplayBinding is the task identity needed before ambient
// workspace and work-root aliases are resolved during command replay.
type FakePublicationReplayBinding struct {
	WorkspaceDir string
	WorkDir      string
	Dispatched   bool
}

// LoadFakePublicationReplayBinding recovers the durable filesystem identities
// without requiring the artifact store to be opened first.
func LoadFakePublicationReplayBinding(
	ctx context.Context,
	st *store.Store,
	runID domain.RunID,
) (FakePublicationReplayBinding, bool, error) {
	task, entry, found, err := loadFakePublicationReplayTask(ctx, st, runID)
	if err != nil || !found {
		return FakePublicationReplayBinding{}, found, err
	}
	workDir, err := fakePublicationTaskWorkDir(task)
	if err != nil {
		return FakePublicationReplayBinding{}, false,
			fmt.Errorf("task %q work directory: %w", entry.IdempotencyKey, err)
	}
	return FakePublicationReplayBinding{
		WorkspaceDir: task.WorkspaceDir,
		WorkDir:      workDir,
		Dispatched:   entry.Dispatched(),
	}, true, nil
}

// LoadFakePublicationReplay recovers the exact approved recipe, canonical
// filesystem identities, and dispatch status bound into one durable task.
func LoadFakePublicationReplay(
	ctx context.Context,
	st *store.Store,
	artifacts ArtifactStore,
	runID domain.RunID,
) (FakePublicationReplay, bool, error) {
	if st == nil || artifacts == nil {
		return FakePublicationReplay{}, false,
			errors.New("load fake publication replay: nil dependency")
	}
	task, entry, found, err := loadFakePublicationReplayTask(ctx, st, runID)
	if err != nil || !found {
		return FakePublicationReplay{}, found, err
	}
	recipe, err := loadFakePublicationRecipe(artifacts, task.RecipeDigest)
	if err != nil {
		return FakePublicationReplay{}, false,
			fmt.Errorf("task %q recipe: %w", entry.IdempotencyKey, err)
	}
	workDir, err := fakePublicationTaskWorkDir(task)
	if err != nil {
		return FakePublicationReplay{}, false,
			fmt.Errorf("task %q work directory: %w", entry.IdempotencyKey, err)
	}
	return FakePublicationReplay{
		Recipe: recipe, WorkspaceDir: task.WorkspaceDir, WorkDir: workDir,
		Dispatched: entry.Dispatched(),
	}, true, nil
}

func loadFakePublicationReplayTask(
	ctx context.Context,
	st *store.Store,
	runID domain.RunID,
) (fakePublicationTask, store.QueueEntry, bool, error) {
	if st == nil {
		return fakePublicationTask{}, store.QueueEntry{}, false,
			errors.New("load fake publication replay task: nil store")
	}
	key := fakePublicationTaskKey(runID)
	var entry store.QueueEntry
	var task fakePublicationTask
	taskFound := false
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, key)
		if err != nil {
			return err
		}
		taskFound = true
		if entry.IdempotencyKey != key || entry.Kind != fakePublicationTaskKind {
			return fmt.Errorf("task %q has kind %q: %w",
				key, entry.Kind, domain.ErrParentKeyMismatch)
		}
		task, err = decodeFakePublicationTask(entry.Payload)
		if err != nil {
			return fmt.Errorf("task %q: %w", key, err)
		}
		if task.RunID != runID {
			return fmt.Errorf("task %q names run %q: %w",
				key, task.RunID, domain.ErrParentKeyMismatch)
		}
		return nil
	}); err != nil {
		if !taskFound && errors.Is(err, store.ErrNotFound) {
			return fakePublicationTask{}, store.QueueEntry{}, false, nil
		}
		return fakePublicationTask{}, store.QueueEntry{}, false, err
	}
	if err := ensureFakePublicationPolicy(ctx, st, task); err != nil {
		return fakePublicationTask{}, store.QueueEntry{}, false, err
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		return validateFakePublicationReconciliation(ctx, tx, task)
	}); err != nil {
		return fakePublicationTask{}, store.QueueEntry{}, false, err
	}
	return task, entry, true, nil
}

func loadFakePublicationRecipe(
	artifacts ArtifactStore,
	digest domain.Digest,
) ([]byte, error) {
	body, err := loadArtifactBlob(artifacts, digest)
	if err != nil {
		return nil, fmt.Errorf("load trusted recipe %s: %w", digest, err)
	}
	if _, err := verify.ParseRecipe(body); err != nil {
		return nil, fmt.Errorf("parse trusted recipe %s: %w", digest, err)
	}
	return body, nil
}

var _ publish.CandidateResolver = (*fakePublicationWorkflow)(nil)
