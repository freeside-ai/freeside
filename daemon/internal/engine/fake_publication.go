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
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	exporter "github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"golang.org/x/sys/unix"
)

const (
	fakePublicationTaskKind               = "engine.fake_publication"
	fakePublicationInvocationOwnerKind    = "engine.fake_publication_invocation_owner"
	fakePublicationInvocationOwnerVersion = "freeside.engine.fake-publication-invocation-owner/v1"
	fakePublicationTaskVersion            = "freeside.engine.fake-publication/v1"
	fakePublicationStageName              = "fake_candidate_publication"
	fakePublicationMaxCommitTimestamp     = int64(4102444800)

	// OperatingModeAttendedDev is the only mode the 1A.1 fake-candidate
	// workflow accepts. Starting this explicit workflow is a manual attended
	// operation; it does not enable auto-start or unattended publication.
	OperatingModeAttendedDev = "attended_dev"
)

// FakePublicationTaskKind identifies the durable outbox payload whose recipe
// blob must remain in backup closure for replay after restore.
const FakePublicationTaskKind = fakePublicationTaskKind

// FakePublicationInvocationOwnerKind identifies a completed durable
// invocation-owner claim.
const FakePublicationInvocationOwnerKind = fakePublicationInvocationOwnerKind

// ErrForeignPublicationCheckout marks a checkout capability that did not
// originate at the configured transport adapter.
var ErrForeignPublicationCheckout = errors.New("publication checkout belongs to a different transport")

// PublicationCheckout is the narrow capability the engine needs from a
// daemon-owned base checkout. The publish package's sealed Checkout and test
// fakes both satisfy it without exposing mutable transport bindings.
type PublicationCheckout interface {
	Dir() string
	BaseSHA() string
	BaseRef() string
	Repo() string
}

// PublicationTransport is the engine-facing composition seam over the
// publish lane's sealed git transport. PushHead takes a publish.GatedHead
// rather than a bare identity: only Publisher's post-gate callback can
// produce one, so the seam itself cannot express an ungated push (#288).
type PublicationTransport interface {
	FetchBase(ctx context.Context, repo, baseRef, baseSHA, dir string) (PublicationCheckout, error)
	RetainWorktree(ctx context.Context, checkout PublicationCheckout, dest, headSHA string) error
	PushHead(ctx context.Context, checkout PublicationCheckout, gated publish.GatedHead) (publish.PushResult, error)
}

// GitPublicationTransport adapts publish.Transport's sealed Checkout to the
// engine-facing interface while preserving its provenance check.
type GitPublicationTransport struct {
	transport *publish.Transport
}

type gitPublicationCheckout struct {
	checkout publish.Checkout
	owner    *GitPublicationTransport
}

func (c gitPublicationCheckout) Dir() string     { return c.checkout.Dir() }
func (c gitPublicationCheckout) BaseSHA() string { return c.checkout.BaseSHA() }
func (c gitPublicationCheckout) BaseRef() string { return c.checkout.BaseRef() }
func (c gitPublicationCheckout) Repo() string    { return c.checkout.Repo() }

// NewGitPublicationTransport constructs the production transport adapter.
func NewGitPublicationTransport(transport *publish.Transport) (*GitPublicationTransport, error) {
	if transport == nil {
		return nil, errors.New("publication transport: nil git transport")
	}
	return &GitPublicationTransport{transport: transport}, nil
}

func (t *GitPublicationTransport) FetchBase(
	ctx context.Context,
	repo, baseRef, baseSHA, dir string,
) (PublicationCheckout, error) {
	if t == nil || t.transport == nil {
		return nil, errors.New("publication transport: nil git transport")
	}
	checkout, err := t.transport.FetchBase(ctx, repo, baseRef, baseSHA, dir)
	if err != nil {
		return nil, err
	}
	return gitPublicationCheckout{checkout: checkout, owner: t}, nil
}

func (t *GitPublicationTransport) RetainWorktree(
	ctx context.Context, checkout PublicationCheckout, dest, headSHA string,
) error {
	if t == nil || t.transport == nil {
		return errors.New("publication transport: nil git transport")
	}
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return ErrForeignPublicationCheckout
	}
	return t.transport.RetainWorktree(ctx, sealed.checkout.Dir(), dest, headSHA)
}

// PushHead forwards both capabilities to the sealed transport: the
// adapter's own owner check keeps a foreign checkout out, and the
// publish.GatedHead it passes through is the Publisher's gate proof, which
// this adapter can neither mint nor weaken.
func (t *GitPublicationTransport) PushHead(
	ctx context.Context,
	checkout PublicationCheckout,
	gated publish.GatedHead,
) (publish.PushResult, error) {
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return publish.PushResult{}, ErrForeignPublicationCheckout
	}
	return t.transport.PushHead(ctx, sealed.checkout, gated)
}

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

type fakePublicationTask struct {
	Version                  string              `json:"version"`
	RunID                    domain.RunID        `json:"run_id"`
	ProjectID                domain.ProjectID    `json:"project_id"`
	StoreEpoch               string              `json:"store_epoch"`
	WorkspaceDir             string              `json:"workspace_dir"`
	HandoffDir               string              `json:"handoff_dir"`
	HandoffDigest            domain.Digest       `json:"handoff_digest"`
	Repo                     string              `json:"repo"`
	BaseRef                  string              `json:"base_ref"`
	BaseSHA                  string              `json:"base_sha"`
	AllowedPaths             []string            `json:"allowed_paths"`
	RecipeDigest             domain.Digest       `json:"recipe_digest"`
	RecipePath               string              `json:"recipe_path"`
	TrustProfileDigest       domain.Digest       `json:"trust_profile_digest"`
	VerificationInvocationID domain.InvocationID `json:"verification_invocation_id"`
	PublicationInvocationID  domain.InvocationID `json:"publication_invocation_id"`
	Title                    string              `json:"title"`
	Body                     string              `json:"body"`
	CommitDate               time.Time           `json:"commit_date"`
	CommitDateExplicit       bool                `json:"commit_date_explicit"`
	StartedAt                time.Time           `json:"started_at"`
	OperatingMode            string              `json:"operating_mode"`
}

type fakePublicationInvocationOwner struct {
	Version       string              `json:"version"`
	InvocationID  domain.InvocationID `json:"invocation_id"`
	Role          string              `json:"role"`
	BindingDigest domain.Digest       `json:"binding_digest"`
	RunID         domain.RunID        `json:"run_id"`
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

type fakePublicationPolicyRecovery struct {
	store *store.Store
	mu    sync.Mutex
	epoch string
}

type attentionService interface {
	PutItem(context.Context, domain.AttentionItem) error
}

// ArtifactStore is the durable content-addressed boundary for the trusted
// recipe and verifier-authored report and transcript bytes. Metadata and tasks
// never commit before the bytes they require.
type ArtifactStore interface {
	Put(domain.Digest, io.Reader) (bool, error)
	Open(domain.Digest) (io.ReadCloser, error)
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

// ConvergeLegacyFakePublicationPolicies completes the epoch's one-time
// dispatched-task policy upgrade. The daemon calls it synchronously before
// starting the scheduler; reconciliation retains the same gate for restores
// and callers that do not use the daemon composition root.
func (e *Engine) ConvergeLegacyFakePublicationPolicies(ctx context.Context) error {
	if e.fakePublicationPolicy == nil {
		return nil
	}
	return e.fakePublicationPolicy.converge(ctx)
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
		return publicationRun(committed), nil
	}
	if err != nil {
		return domain.Run{}, fmt.Errorf("start fake publication: %w", err)
	}
	return publicationRun(committed), nil
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

func (r *fakePublicationPolicyRecovery) converge(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var recoveryEpoch string
	var owned []store.QueueEntry
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		recoveryEpoch = state.SyncEpoch
		if r.epoch == recoveryEpoch {
			return nil
		}
		pending, err := tx.ListPendingOutbox(ctx, fakePublicationTaskKind)
		if err != nil {
			return err
		}
		dispatched, err := tx.ListDispatchedOutbox(ctx, fakePublicationTaskKind)
		if err != nil {
			return err
		}
		owned = append(pending, dispatched...)
		return nil
	}); err != nil {
		return err
	}

	if r.epoch == recoveryEpoch {
		return nil
	}
	var recoveryErr error
	// Both pending and completed v1 tasks can own watches: publication arms them
	// before marking the task dispatched, so a crash can persist either status.
	// Converge all durable owners once per store epoch before processing new
	// work. A restart or in-place restore gets a fresh pass, while steady-state
	// reconciliation does not rescan unbounded history.
	for _, entry := range owned {
		task, err := decodeBoundFakePublicationTask(entry)
		if err == nil {
			err = ensureFakePublicationPolicy(ctx, r.store, task)
		}
		if err != nil {
			recoveryErr = errors.Join(
				recoveryErr,
				fmt.Errorf("task %q policy convergence: %w", entry.IdempotencyKey, err),
			)
		}
	}
	if recoveryErr != nil {
		return recoveryErr
	}
	r.epoch = recoveryEpoch
	return nil
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

type fakePublicationRunReader interface {
	GetRun(context.Context, domain.RunID) (domain.Run, error)
	GetResolvedPolicy(context.Context, domain.RunID) (domain.ResolvedPolicy, error)
}

// fakePublicationPolicyState recognizes exactly two representations: the
// current authenticated policy body, or the pre-upgrade v1 run whose trust
// profile digest stood in for policy and which has no policy row. Anything
// else is corruption, never migration input.
func fakePublicationPolicyState(
	ctx context.Context,
	reader fakePublicationRunReader,
	task fakePublicationTask,
) (bool, error) {
	run, err := reader.GetRun(ctx, task.RunID)
	if err != nil {
		return false, fmt.Errorf("load publication run %q: %w", task.RunID, err)
	}
	if reflect.DeepEqual(run, publicationRun(task)) {
		policy, err := reader.GetResolvedPolicy(ctx, task.RunID)
		if err != nil {
			return false, fmt.Errorf("load publication resolved policy %q: %w", task.RunID, err)
		}
		if !reflect.DeepEqual(policy, publicationPolicy(task)) {
			return false, fmt.Errorf(
				"publication resolved policy %q disagrees with task: %w",
				task.RunID, domain.ErrParentKeyMismatch,
			)
		}
		return false, nil
	}
	if !reflect.DeepEqual(run, legacyPublicationRun(task)) {
		return false, fmt.Errorf(
			"publication run %q disagrees with task: %w",
			task.RunID, domain.ErrParentKeyMismatch,
		)
	}
	if _, err := reader.GetResolvedPolicy(ctx, task.RunID); err == nil {
		return false, fmt.Errorf(
			"legacy publication run %q unexpectedly has a resolved policy: %w",
			task.RunID, domain.ErrParentKeyMismatch,
		)
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("load legacy publication resolved policy %q: %w", task.RunID, err)
	}
	return true, nil
}

func convergeFakePublicationPolicyTx(
	ctx context.Context,
	tx *store.WriteTx,
	task fakePublicationTask,
) (bool, error) {
	legacy, err := fakePublicationPolicyState(ctx, tx, task)
	if err != nil || !legacy {
		return false, err
	}
	if err := tx.MigrateLegacyTrustProfileRunPolicy(
		ctx, legacyPublicationRun(task), publicationRun(task), publicationPolicy(task),
	); err != nil {
		return false, err
	}
	return true, nil
}

func ensureFakePublicationPolicy(
	ctx context.Context,
	st *store.Store,
	task fakePublicationTask,
) error {
	legacy := false
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		legacy, err = fakePublicationPolicyState(ctx, tx, task)
		return err
	}); err != nil || !legacy {
		return err
	}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		migrated, err := convergeFakePublicationPolicyTx(ctx, tx, task)
		if err != nil {
			return err
		}
		if !migrated {
			return errReplay
		}
		return nil
	})
	if errors.Is(err, errReplay) {
		return nil
	}
	return err
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
	if !reflect.DeepEqual(run, publicationRun(task)) {
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

func validatePublicationCheckoutBinding(
	checkout PublicationCheckout,
	repo, baseRef, baseSHA, requestedDir string,
	requestedParent fs.FileInfo,
) (string, error) {
	value := reflect.ValueOf(checkout)
	if checkout == nil ||
		((value.Kind() == reflect.Chan || value.Kind() == reflect.Func ||
			value.Kind() == reflect.Interface || value.Kind() == reflect.Map ||
			value.Kind() == reflect.Pointer || value.Kind() == reflect.Slice) &&
			value.IsNil()) {
		return "", fmt.Errorf("transport returned a nil checkout: %w", domain.ErrParentKeyMismatch)
	}
	if checkout.Repo() != repo || checkout.BaseRef() != baseRef ||
		checkout.BaseSHA() != baseSHA {
		return "", fmt.Errorf("transport checkout disagrees with task: %w", domain.ErrParentKeyMismatch)
	}
	parent, err := os.Stat(filepath.Dir(requestedDir))
	if err != nil {
		return "", fmt.Errorf("stat requested checkout parent: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if requestedParent == nil || !os.SameFile(requestedParent, parent) {
		return "", fmt.Errorf(
			"transport replaced requested checkout parent: %w", domain.ErrParentKeyMismatch,
		)
	}
	requestedInfo, err := os.Lstat(requestedDir)
	if err != nil {
		return "", fmt.Errorf("stat requested checkout directory: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if requestedInfo.Mode()&os.ModeSymlink != 0 || !requestedInfo.IsDir() {
		return "", fmt.Errorf(
			"transport checkout path is not a real directory: %w", domain.ErrParentKeyMismatch,
		)
	}
	requested, err := filepath.EvalSymlinks(requestedDir)
	if err != nil {
		return "", fmt.Errorf("resolve requested checkout directory: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	returned, err := filepath.EvalSymlinks(checkout.Dir())
	if err != nil {
		return "", fmt.Errorf("resolve returned checkout directory: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if filepath.Clean(returned) != filepath.Clean(requested) {
		return "", fmt.Errorf(
			"transport checkout directory %q, want %q: %w",
			returned, requested, domain.ErrParentKeyMismatch,
		)
	}
	return requested, nil
}

func (w *fakePublicationWorkflow) acquireTaskLock(
	ctx context.Context,
	taskKey string,
) (func() error, error) {
	lockDir := filepath.Join(w.workDir, "task-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create publication task lock directory: %w", err)
	}
	return acquireFakePublicationLock(ctx, lockDir, taskKey)
}

func acquireFakePublicationLock(
	ctx context.Context,
	lockDir, key string,
) (func() error, error) {
	digest := sha256.Sum256([]byte(key))
	lockPath := filepath.Join(lockDir, hex.EncodeToString(digest[:])+".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // fixed digest name under daemon-owned work directory
	if err != nil {
		return nil, fmt.Errorf("open publication lock: %w", err)
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(unix.Flock(int(lock.Fd()), unix.LOCK_UN), lock.Close())
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = lock.Close()
			return nil, fmt.Errorf("lock publication: %w", err)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = lock.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
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
			"Gauntlet containment withheld a candidate commit."); err != nil {
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
			imported.CommitPlanNotice, "Verification or policy findings blocked publication."); err != nil {
			return taskOutcome{}, err
		}
		if err := w.finishTask(ctx, task); err != nil {
			return taskOutcome{}, err
		}
		return taskOutcome{blocked: true}, nil
	}

	candidate := fakePublicationCandidate(task, checkpoint)
	w.candidates[task.PublicationInvocationID] = publish.RecoveryCandidate{
		Candidate: candidate, ApprovedRecipes: maps.Clone(w.approvedRecipes),
		PublishHead: func(ctx context.Context, gated publish.GatedHead) error {
			_, err := w.transport.PushHead(ctx, checkout, gated)
			return err
		},
	}
	defer delete(w.candidates, task.PublicationInvocationID)

	identity, err := fakePublicationIdentity(candidate)
	if err != nil {
		return taskOutcome{}, err
	}
	identityLockDir := filepath.Join(w.workDir, "identity-locks")
	if err := os.MkdirAll(identityLockDir, 0o700); err != nil {
		return taskOutcome{}, fmt.Errorf("create publication identity lock directory: %w", err)
	}
	releaseIdentity, err := acquireFakePublicationLock(
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
	ready, err := w.readyItem(task, imported, artifacts, published)
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
	if err := armPublicationWatches(ctx, w.store, stored,
		task.Repo, task.BaseRef, task.BaseSHA, w.now()); err != nil {
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
	published, found, err := w.loadPublicationOutcome(
		ctx, fakePublicationCandidate(task, checkpoint),
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
) publish.Candidate {
	recipeDigest := task.RecipeDigest
	authorizationID := checkpoint.Authorization.ID
	profileDigest := task.TrustProfileDigest
	return publish.Candidate{
		Repo: task.Repo, BaseRef: task.BaseRef, HeadSHA: checkpoint.Imported.CommitSHA,
		Title: task.Title, Body: task.Body, Artifacts: checkpoint.Artifacts,
		RecipeDigest: &recipeDigest, InvocationID: task.PublicationInvocationID,
		RunID:           task.RunID,
		AuthorizationID: &authorizationID, TrustProfileDigest: &profileDigest,
	}
}

func (w *fakePublicationWorkflow) loadPublicationOutcome(
	ctx context.Context,
	candidate publish.Candidate,
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
	intent, err := publish.DecodeIntent(entry.Payload)
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

func fakePublicationIdentity(candidate publish.Candidate) (publish.Identity, error) {
	digests := make([]domain.Digest, len(candidate.Artifacts))
	for i, artifact := range candidate.Artifacts {
		digests[i] = artifact.Digest
	}
	return publish.DeriveIdentity(publish.IdentityInput{
		Repo: candidate.Repo, BaseRef: candidate.BaseRef,
		SourceHeadSHA: candidate.HeadSHA, ArtifactDigests: digests,
		RecipeDigest: candidate.RecipeDigest,
	})
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
			if err := syncFakePublicationDirectory(parent); err != nil {
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
		output, task.HandoffDir, syncFakePublicationDirectory,
	); err != nil {
		return "", err
	}
	committed = true
	_ = os.RemoveAll(temp)
	return digest, nil
}

func rollbackFakePublicationHandoff(path string) error {
	return rollbackFakePublicationHandoffWithSync(path, syncFakePublicationDirectory)
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
		if err := verifyFakePublicationBlob(w.artifacts, item.Artifact); err != nil {
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
	if err := syncFakePublicationDirectory(parent); err != nil {
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
	if err := syncFakePublicationCheckpointDirectory(path, syncFakePublicationDirectory); err != nil {
		return fakePublicationCandidateCheckpoint{}, false,
			fmt.Errorf("sync existing candidate checkpoint directory: %w", err)
	}
	var checkpoint fakePublicationCandidateCheckpoint
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&checkpoint); err != nil {
		return fakePublicationCandidateCheckpoint{}, false, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fakePublicationCandidateCheckpoint{}, false,
			errors.New("candidate checkpoint has trailing content")
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
		if err := verifyFakePublicationBlob(w.artifacts, artifact); err != nil {
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
	return makeFakePublicationDirectoryWithSync(path, mode, syncFakePublicationDirectory)
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
		if err := syncFakePublicationDirectory(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func syncFakePublicationDirectory(path string) error {
	dir, err := os.Open(path) //nolint:gosec // daemon-owned work directory path
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
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
	task fakePublicationTask,
	imported importer.Result,
	artifacts []domain.Artifact,
	published publish.Result,
) (domain.AttentionItem, error) {
	runID := task.RunID
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: readyItemID(task.RunID), ProjectID: task.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
		},
		Type: domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: fmt.Sprintf("%s#%d is published and ready for final review.",
			task.Repo, published.PRNumber),
		RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionMarkSeen, domain.ActionDismiss, domain.ActionStop,
		},
		EvidenceSnapshot: artifacts, AgentClaims: imported.Claims,
		PRHeadSHA: imported.CommitSHA, CommitPlanNotice: imported.CommitPlanNotice,
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
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
) error {
	runID := task.RunID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: blockedItemID(task.RunID), ProjectID: task.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
		},
		Type: domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
		Reason: reason,
		RequestedDecision: []domain.Action{
			domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
		},
		EvidenceSnapshot: artifacts, AgentClaims: claims,
		PRHeadSHA: headSHA, CommitPlanNotice: notice,
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
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

func compatibleTerminalItem(expected, current domain.AttentionItem) bool {
	normalized := current
	normalized.ItemVersion = expected.ItemVersion
	normalized.Status = expected.Status
	normalized.DecidedAt = expected.DecidedAt
	normalized.Timing = expected.Timing
	// The base-advance watch maintains this fact after creation (§5.16), so
	// like status and timing it never disqualifies an otherwise matching
	// item during recovery.
	normalized.BaseFreshness = expected.BaseFreshness
	// Readiness invalidation is likewise recorded after creation, in the same
	// transition that supersedes the item (§7, issue #496): the active-resource
	// reconciler or the base-advance watch can set it during a crash seam before
	// finishTask runs. Recovery must accept the resulting terminal item as
	// compatible with the freshly derived ready shape rather than failing with
	// "disagrees" and stranding the publication task; the transition check below
	// still enforces that open -> superseded is a legal move.
	normalized.ReadinessInvalidation = expected.ReadinessInvalidation
	if !reflect.DeepEqual(normalized, expected) {
		return false
	}
	if reflect.DeepEqual(current, expected) {
		return true
	}
	return domain.ValidateAttentionItemTransition(expected, current) == nil
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
		if err := verifyFakePublicationBlob(w.artifacts, artifact); err != nil {
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
		prefix := task.Repo + "#"
		const suffix = " is published and ready for final review."
		if !strings.HasPrefix(item.Reason, prefix) || !strings.HasSuffix(item.Reason, suffix) {
			return taskOutcome{}, false, fmt.Errorf("ready item %q has invalid reason", item.ID)
		}
		number := strings.TrimSuffix(strings.TrimPrefix(item.Reason, prefix), suffix)
		prNumber, err := strconv.Atoi(number)
		if err != nil || prNumber <= 0 {
			return taskOutcome{}, false, fmt.Errorf(
				"ready item %q has invalid pull request number", item.ID,
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
	item.Reason = strings.TrimRight(item.Reason, "\n")
	digest, err := fakePublicationTerminalDigest(task, item)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	item.Reason += "\n\n" + fakePublicationTerminalBindingPrefix +
		string(digest) + fakePublicationTerminalBindingSuffix
	if err := item.Validate(); err != nil {
		return domain.AttentionItem{}, err
	}
	return item, nil
}

func validateFakePublicationTerminalBinding(
	task fakePublicationTask,
	item domain.AttentionItem,
) (domain.AttentionItem, error) {
	reason, got, ok := ParseFakePublicationTerminalReason(item.Reason)
	if !ok {
		return domain.AttentionItem{}, fmt.Errorf(
			"missing task binding: %w", domain.ErrParentKeyMismatch,
		)
	}
	item.Reason = reason
	want, err := fakePublicationTerminalDigest(task, item)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	if got != string(want) {
		return domain.AttentionItem{}, fmt.Errorf(
			"task binding %q, want %q: %w", got, want, domain.ErrParentKeyMismatch,
		)
	}
	return item, nil
}

// ParseFakePublicationTerminalReason separates the human-facing reason from
// the engine's hidden task binding. Recovery validates the returned digest;
// clients use the reason only after reconciliation has accepted the terminal.
func ParseFakePublicationTerminalReason(reason string) (string, string, bool) {
	separator := "\n\n" + fakePublicationTerminalBindingPrefix
	offset := strings.LastIndex(reason, separator)
	if offset < 0 || !strings.HasSuffix(reason, fakePublicationTerminalBindingSuffix) {
		return "", "", false
	}
	digest := reason[offset+len(separator) : len(reason)-len(fakePublicationTerminalBindingSuffix)]
	return reason[:offset], digest, digest != ""
}

func fakePublicationTerminalDigest(
	task fakePublicationTask,
	item domain.AttentionItem,
) (domain.Digest, error) {
	item.ItemVersion = 1
	item.Status = domain.StatusOpen
	item.DecidedAt = nil
	item.Timing = domain.TimingSummary{}
	payload, err := json.Marshal(struct {
		Task fakePublicationTask  `json:"task"`
		Item domain.AttentionItem `json:"item"`
	}{Task: task, Item: item})
	if err != nil {
		return "", fmt.Errorf("encode terminal binding: %w", err)
	}
	sum := sha256.Sum256(payload)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:])), nil
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

func publicationRun(task fakePublicationTask) domain.Run {
	stageID := domain.StageID("publication-" + string(task.RunID))
	return domain.Run{
		ID: task.RunID, ProjectID: task.ProjectID,
		SpecDigest:   digestBytes(mustEncodeFakePublicationTask(task)),
		PolicyDigest: publicationPolicy(task).Digest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: task.RunID, Name: fakePublicationStageName,
			Attempts: []domain.Attempt{{
				ID:      domain.AttemptID("attempt-" + string(task.PublicationInvocationID)),
				StageID: stageID, Number: 1, InvocationID: task.PublicationInvocationID,
			}},
		}},
	}
}

func legacyPublicationRun(task fakePublicationTask) domain.Run {
	run := publicationRun(task)
	run.PolicyDigest = task.TrustProfileDigest
	return run
}

func publicationPolicy(task fakePublicationTask) domain.ResolvedPolicy {
	policy, err := domain.NewResolvedPolicy(task.RunID, []domain.PolicyKey{{
		Key:   "trust_profile_digest",
		Value: string(task.TrustProfileDigest),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: task.TrustProfileDigest,
		},
	}})
	if err != nil {
		panic(fmt.Sprintf("construct fake publication policy: %v", err))
	}
	return policy
}

func fakePublicationTaskKey(runID domain.RunID) string {
	return fakePublicationTaskKind + "/" + string(runID)
}

func fakePublicationInvocationOwnerKey(invocationID domain.InvocationID) string {
	return fakePublicationInvocationOwnerKind + "/" + string(invocationID)
}

// FakePublicationReadyItemID returns the workflow-owned ready-item namespace
// for a run.
func FakePublicationReadyItemID(runID domain.RunID) domain.ItemID {
	return fakePublicationTerminalItemID("ready", runID)
}

// FakePublicationBlockedItemID returns the workflow-owned blocked-item
// namespace for a run.
func FakePublicationBlockedItemID(runID domain.RunID) domain.ItemID {
	return fakePublicationTerminalItemID("blocked", runID)
}

func fakePublicationTerminalItemID(kind string, runID domain.RunID) domain.ItemID {
	sum := sha256.Sum256([]byte(fakePublicationTaskKind + "\x00" + kind + "\x00" + string(runID)))
	return domain.ItemID("fake-publication-" + kind + "-" + hex.EncodeToString(sum[:]))
}

func readyItemID(runID domain.RunID) domain.ItemID {
	return FakePublicationReadyItemID(runID)
}

func blockedItemID(runID domain.RunID) domain.ItemID {
	return FakePublicationBlockedItemID(runID)
}

func encodeFakePublicationTask(task fakePublicationTask) ([]byte, error) {
	if err := task.validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("encode fake publication task: %w", err)
	}
	return payload, nil
}

func mustEncodeFakePublicationTask(task fakePublicationTask) []byte {
	payload, err := encodeFakePublicationTask(task)
	if err != nil {
		panic(fmt.Sprintf("engine fake publication task invariant: %v", err))
	}
	return payload
}

func decodeFakePublicationTask(payload []byte) (fakePublicationTask, error) {
	var task fakePublicationTask
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&task); err != nil {
		return fakePublicationTask{}, fmt.Errorf("decode fake publication task: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fakePublicationTask{}, errors.New("decode fake publication task: trailing data")
	}
	if err := task.validate(); err != nil {
		return fakePublicationTask{}, err
	}
	return task, nil
}

// FakePublicationBackupPayloadDigests validates a durable fake-publication
// entry and returns the externally stored blobs required to replay it.
func FakePublicationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	task, err := decodeBoundFakePublicationTask(entry)
	if err != nil {
		return nil, err
	}
	return []domain.Digest{task.RecipeDigest}, nil
}

// FakePublicationInvocationOwnerBackupPayloadDigests validates a completed
// invocation-owner entry. The claim is self-contained and needs no blobs.
func FakePublicationInvocationOwnerBackupPayloadDigests(
	entry store.QueueEntry,
) ([]domain.Digest, error) {
	if entry.Kind != FakePublicationInvocationOwnerKind || !entry.Dispatched() {
		return nil, domain.ErrParentKeyMismatch
	}
	owner, err := decodeFakePublicationInvocationOwner(entry.Payload)
	if err != nil {
		return nil, err
	}
	if entry.IdempotencyKey != fakePublicationInvocationOwnerKey(owner.InvocationID) {
		return nil, domain.ErrParentKeyMismatch
	}
	return nil, nil
}

func decodeBoundFakePublicationTask(entry store.QueueEntry) (fakePublicationTask, error) {
	if entry.Kind != FakePublicationTaskKind {
		return fakePublicationTask{}, domain.ErrParentKeyMismatch
	}
	task, err := decodeFakePublicationTask(entry.Payload)
	if err != nil {
		return fakePublicationTask{}, err
	}
	if entry.IdempotencyKey != fakePublicationTaskKey(task.RunID) {
		return fakePublicationTask{}, fmt.Errorf("names run %q: %w",
			task.RunID, domain.ErrParentKeyMismatch)
	}
	return task, nil
}

func decodeFakePublicationInvocationOwner(
	payload []byte,
) (fakePublicationInvocationOwner, error) {
	var owner fakePublicationInvocationOwner
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&owner); err != nil {
		return fakePublicationInvocationOwner{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fakePublicationInvocationOwner{}, errors.New("trailing data")
	}
	if owner.Version != fakePublicationInvocationOwnerVersion {
		return fakePublicationInvocationOwner{}, fmt.Errorf(
			"unknown publication invocation owner version %q", owner.Version,
		)
	}
	if owner.InvocationID == "" || owner.RunID == "" {
		return fakePublicationInvocationOwner{}, domain.ErrEmptyID
	}
	if !validSHA256Digest(owner.BindingDigest) {
		return fakePublicationInvocationOwner{}, domain.ErrEmptyField
	}
	if owner.Role != "verification" && owner.Role != "publication" {
		return fakePublicationInvocationOwner{}, fmt.Errorf(
			"unknown publication invocation role %q", owner.Role,
		)
	}
	return owner, nil
}

func (task fakePublicationTask) validate() error {
	if task.Version != fakePublicationTaskVersion {
		return fmt.Errorf("unknown task version %q", task.Version)
	}
	if err := validateFakePublicationTaskUTF8(task); err != nil {
		return err
	}
	if task.RunID == "" || task.ProjectID == "" || task.StoreEpoch == "" ||
		task.VerificationInvocationID == "" ||
		task.PublicationInvocationID == "" {
		return domain.ErrEmptyID
	}
	if task.VerificationInvocationID == task.PublicationInvocationID {
		return errors.New("task reuses one invocation across verification and publication")
	}
	if task.OperatingMode != OperatingModeAttendedDev {
		return fmt.Errorf("task operating mode %q is not %s", task.OperatingMode, OperatingModeAttendedDev)
	}
	if task.WorkspaceDir == "" || !filepath.IsAbs(task.WorkspaceDir) ||
		task.HandoffDir == "" || !filepath.IsAbs(task.HandoffDir) {
		return errors.New("task workspace and handoff paths must be absolute")
	}
	if task.Repo == "" || task.BaseRef == "" || !validCommitSHA(task.BaseSHA) ||
		task.RecipeDigest == "" || task.RecipePath == "" ||
		task.TrustProfileDigest == "" || task.Title == "" ||
		!validSHA256Digest(task.HandoffDigest) {
		return domain.ErrEmptyField
	}
	if task.CommitDate.IsZero() || task.StartedAt.IsZero() ||
		task.CommitDate.Location() != time.UTC || task.StartedAt.Location() != time.UTC {
		return errors.New("task timestamps must be non-zero UTC")
	}
	if err := validateFakePublicationCommitDate(task.CommitDate); err != nil {
		return fmt.Errorf("task %w", err)
	}
	if len(task.AllowedPaths) == 0 {
		return errors.New("task has no candidate path allowlist")
	}
	for _, path := range task.AllowedPaths {
		if path == "" {
			return errors.New("task allowed path is empty")
		}
	}
	if err := publish.ValidateRepository(task.Repo); err != nil {
		return fmt.Errorf("task repository %q: %w", task.Repo, err)
	}
	if err := publish.ValidateBranchName(task.BaseRef); err != nil {
		return fmt.Errorf("task base ref %q: %w", task.BaseRef, err)
	}
	if err := validateFakePublicationAllowlist(task.AllowedPaths); err != nil {
		return fmt.Errorf("task allowlist: %w", err)
	}
	if err := validateFakePublicationRecipePath(task.RecipePath); err != nil {
		return fmt.Errorf("task recipe path: %w", err)
	}
	if err := publish.ValidateCandidateBody(task.Body); err != nil {
		return fmt.Errorf("task publication body: %w", err)
	}
	return nil
}

func validateFakePublicationCommitDate(commitDate time.Time) error {
	if commitDate.Before(time.Unix(0, 0).UTC()) {
		return errors.New("commit date must not precede the Unix epoch")
	}
	// Git's raw-date parser rejects 2100-01-01 and later even though those
	// timestamps still fit in an unsigned 32-bit integer.
	if !commitDate.Before(time.Unix(fakePublicationMaxCommitTimestamp, 0).UTC()) {
		return errors.New("commit date must precede 2100-01-01 UTC")
	}
	return nil
}

func validateFakePublicationTaskUTF8(task fakePublicationTask) error {
	fields := []struct {
		name  string
		value string
	}{
		{"version", task.Version},
		{"run_id", string(task.RunID)},
		{"project_id", string(task.ProjectID)},
		{"store_epoch", task.StoreEpoch},
		{"workspace_dir", task.WorkspaceDir},
		{"handoff_dir", task.HandoffDir},
		{"handoff_digest", string(task.HandoffDigest)},
		{"repo", task.Repo},
		{"base_ref", task.BaseRef},
		{"base_sha", task.BaseSHA},
		{"recipe_digest", string(task.RecipeDigest)},
		{"recipe_path", task.RecipePath},
		{"trust_profile_digest", string(task.TrustProfileDigest)},
		{"verification_invocation_id", string(task.VerificationInvocationID)},
		{"publication_invocation_id", string(task.PublicationInvocationID)},
		{"title", task.Title},
		{"body", task.Body},
		{"operating_mode", task.OperatingMode},
	}
	for _, field := range fields {
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("task %s is not valid UTF-8", field.name)
		}
	}
	for i, pattern := range task.AllowedPaths {
		if !utf8.ValidString(pattern) {
			return fmt.Errorf("task allowed_paths[%d] is not valid UTF-8", i)
		}
	}
	return nil
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

func candidateFindings(
	importFindings []importer.Finding,
	verifyFindings []verify.Finding,
) []domain.CandidateFinding {
	out := make([]domain.CandidateFinding, 0, len(importFindings)+len(verifyFindings))
	for _, finding := range importFindings {
		out = append(out, finding.Candidate())
	}
	for _, finding := range verifyFindings {
		category := domain.ControlPlaneVerificationRecipes
		out = append(out, domain.CandidateFinding{
			Class: domain.FindingClassControlPlane, Category: &category,
			Origin: domain.FindingOriginVerification, Kind: string(finding.Kind),
			Path: finding.Path, PathHex: finding.PathHex, Detail: finding.Detail,
			Disposition: domain.DispositionBlocking,
		})
	}
	return out
}

func digestJSON(value any) (domain.Digest, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digestBytes(payload), nil
}

func digestBytes(payload []byte) domain.Digest {
	sum := sha256.Sum256(payload)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func validCommitSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, r := range sha {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validSHA256Digest(digest domain.Digest) bool {
	return contentaddr.Valid(string(digest))
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
	return domain.Digest("sha256:" + hex.EncodeToString(hash.Sum(nil))), nil
}

// validateFakePublicationAllowlist mirrors the importer's slash-separated
// glob grammar at admission. A deterministic caller error must not become a
// durable outbox row that fails identically on every reconciliation.
func validateFakePublicationAllowlist(patterns []string) error {
	for _, pattern := range patterns {
		for _, segment := range strings.Split(pattern, "/") {
			if segment == "**" {
				continue
			}
			if _, err := path.Match(segment, ""); err != nil {
				return fmt.Errorf("invalid candidate path allowlist pattern %q: %w", pattern, err)
			}
		}
	}
	return nil
}

func validateFakePublicationRecipePath(recipePath string) error {
	if recipePath == "" || strings.HasPrefix(recipePath, "/") ||
		strings.ContainsAny(recipePath, `:\*?[]`) {
		return errors.New(
			"must be a relative slash path without colon, backslash, or glob metacharacters",
		)
	}
	for _, component := range strings.Split(recipePath, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("component %q is not allowed", component)
		}
	}
	return nil
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
	body, err := loadFakePublicationBlob(artifacts, digest)
	if err != nil {
		return nil, fmt.Errorf("load trusted recipe %s: %w", digest, err)
	}
	if _, err := verify.ParseRecipe(body); err != nil {
		return nil, fmt.Errorf("parse trusted recipe %s: %w", digest, err)
	}
	return body, nil
}

func verifyFakePublicationBlob(store ArtifactStore, artifact domain.Artifact) error {
	body, err := store.Open(artifact.Digest)
	if err != nil {
		return fmt.Errorf("open candidate checkpoint artifact %s: %w", artifact.ID, err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, body)
	closeErr := body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("read candidate checkpoint artifact %s: %w", artifact.ID, err)
	}
	got := domain.Digest("sha256:" + hex.EncodeToString(hasher.Sum(nil)))
	if got != artifact.Digest {
		return fmt.Errorf(
			"candidate checkpoint artifact %s body hashes to %s, want %s",
			artifact.ID, got, artifact.Digest,
		)
	}
	return nil
}

func loadFakePublicationBlob(
	store ArtifactStore,
	digest domain.Digest,
) ([]byte, error) {
	body, err := store.Open(digest)
	if err != nil {
		return nil, fmt.Errorf("open body: %w", err)
	}
	hasher := sha256.New()
	var content bytes.Buffer
	_, copyErr := io.Copy(io.MultiWriter(&content, hasher), body)
	closeErr := body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	got := domain.Digest("sha256:" + hex.EncodeToString(hasher.Sum(nil)))
	if got != digest {
		return nil, fmt.Errorf("body hashes to %s, want %s", got, digest)
	}
	return content.Bytes(), nil
}

func isDefinitiveTrustRefusal(err error) bool {
	return errors.Is(err, publish.ErrTrustProfileDrift) ||
		errors.Is(err, publish.ErrTargetBaseAdvanced)
}

var _ publish.CandidateResolver = (*fakePublicationWorkflow)(nil)
