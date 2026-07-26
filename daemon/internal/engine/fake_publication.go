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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	exporter "github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	fakePublicationTaskKind    = "engine.fake_publication"
	fakePublicationTaskVersion = "freeside.engine.fake-publication/v1"
	fakePublicationStageName   = "fake_candidate_publication"

	// OperatingModeAttendedDev is the only mode the 1A.1 fake-candidate
	// workflow accepts. Starting this explicit workflow is a manual attended
	// operation; it does not enable auto-start or unattended publication.
	OperatingModeAttendedDev = "attended_dev"
)

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
// publish lane's sealed git transport.
type PublicationTransport interface {
	FetchBase(ctx context.Context, repo, baseRef, baseSHA, dir string) (PublicationCheckout, error)
	PushHead(ctx context.Context, checkout PublicationCheckout, in publish.IdentityInput) (publish.PushResult, error)
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

func (t *GitPublicationTransport) PushHead(
	ctx context.Context,
	checkout PublicationCheckout,
	in publish.IdentityInput,
) (publish.PushResult, error) {
	sealed, ok := checkout.(gitPublicationCheckout)
	if !ok || sealed.owner != t {
		return publish.PushResult{}, ErrForeignPublicationCheckout
	}
	return t.transport.PushHead(ctx, sealed.checkout, in)
}

// FakePublicationConfig supplies the already-reviewed deterministic and
// external boundaries for the 1A.1 workflow.
type FakePublicationConfig struct {
	WorkDir         string
	Recipe          []byte
	RecipePath      string
	ApprovedRecipes map[domain.Digest]bool
	Transport       PublicationTransport
	Publisher       *publish.Publisher
	Artifacts       ArtifactStore
	NewRoom         func(home string) verify.Room
	Now             func() time.Time
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
	WorkspaceDir             string              `json:"workspace_dir"`
	HandoffDir               string              `json:"handoff_dir"`
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
	StartedAt                time.Time           `json:"started_at"`
	OperatingMode            string              `json:"operating_mode"`
}

type fakePublicationWorkflow struct {
	store           *store.Store
	attention       attentionService
	workDir         string
	recipe          []byte
	recipeDigest    domain.Digest
	recipePath      string
	approvedRecipes map[domain.Digest]bool
	transport       PublicationTransport
	publisher       *publish.Publisher
	artifacts       ArtifactStore
	newRoom         func(home string) verify.Room
	now             func() time.Time

	reconcileMu sync.Mutex
	candidates  map[domain.InvocationID]publish.Candidate
}

type attentionService interface {
	PutItem(context.Context, domain.AttentionItem) error
}

// ArtifactStore is the durable content-addressed boundary for verifier-authored
// report and transcript bytes. Metadata never commits before these bytes.
type ArtifactStore interface {
	Put(domain.Digest, io.Reader) (bool, error)
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
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create work directory: %w", err)
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
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &fakePublicationWorkflow{
		store: st, attention: attention, workDir: workDir,
		recipe: slices.Clone(cfg.Recipe), recipeDigest: recipeDigest, recipePath: recipePath,
		approvedRecipes: maps.Clone(cfg.ApprovedRecipes),
		transport:       cfg.Transport, publisher: cfg.Publisher, artifacts: cfg.Artifacts,
		newRoom: cfg.NewRoom, now: now,
		candidates: map[domain.InvocationID]publish.Candidate{},
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
	payload, err := encodeFakePublicationTask(task)
	if err != nil {
		return domain.Run{}, err
	}

	var committed fakePublicationTask
	err = w.store.Write(ctx, func(tx *store.WriteTx) error {
		entry, inserted, err := tx.EnqueueOutbox(ctx, key, fakePublicationTaskKind, payload)
		if err != nil {
			return err
		}
		if entry.IdempotencyKey != key || entry.Kind != fakePublicationTaskKind {
			return fmt.Errorf("task row disagrees with key or kind: %w", domain.ErrParentKeyMismatch)
		}
		if inserted {
			if err := validateFakePublicationWorkspace(task.WorkspaceDir); err != nil {
				return err
			}
			if err := w.commitHandoff(task); err != nil {
				return err
			}
			committed = task
		} else {
			prior, err := decodeFakePublicationTask(entry.Payload)
			if err != nil {
				return fmt.Errorf("decode prior task: %w", err)
			}
			if !sameFakePublicationRequest(prior, task, explicitCommitDate) {
				return fmt.Errorf("task %q fixed bindings disagree with stored task: %w",
					key, domain.ErrImmutableTransition)
			}
			committed = prior
			return errReplay
		}
		run := publicationRun(committed)
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return nil
	})
	if errors.Is(err, errReplay) {
		return publicationRun(committed), nil
	}
	if err != nil {
		return domain.Run{}, fmt.Errorf("start fake publication: %w", err)
	}
	return publicationRun(committed), nil
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
	if len(spec.AllowedPaths) == 0 {
		return fakePublicationTask{}, false, errors.New("at least one candidate path allowlist pattern is required")
	}
	workspaceDir, err := filepath.Abs(spec.WorkspaceDir)
	if err != nil {
		return fakePublicationTask{}, false, fmt.Errorf("resolve workspace: %w", err)
	}
	var profile domain.AutomationTrustProfile
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		profile, err = tx.LatestTrustProfile(ctx, spec.Repo)
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
	task := fakePublicationTask{
		Version: fakePublicationTaskVersion,
		RunID:   spec.RunID, ProjectID: spec.ProjectID,
		WorkspaceDir: workspaceDir,
		Repo:         spec.Repo, BaseRef: spec.BaseRef, BaseSHA: spec.BaseSHA,
		AllowedPaths: slices.Clone(spec.AllowedPaths),
		RecipeDigest: w.recipeDigest, RecipePath: w.recipePath,
		TrustProfileDigest:       profile.ProfileDigest,
		VerificationInvocationID: spec.VerificationInvocationID,
		PublicationInvocationID:  spec.PublicationInvocationID,
		Title:                    spec.Title, Body: spec.Body,
		CommitDate: commitDate, StartedAt: startedAt,
		OperatingMode: spec.OperatingMode,
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
	for _, entry := range pending {
		task, err := decodeFakePublicationTask(entry.Payload)
		if err != nil {
			return result, fmt.Errorf("task %q: %w", entry.IdempotencyKey, err)
		}
		if entry.IdempotencyKey != fakePublicationTaskKey(task.RunID) {
			return result, fmt.Errorf("task %q names run %q: %w",
				entry.IdempotencyKey, task.RunID, domain.ErrParentKeyMismatch)
		}
		expectedHandoff, err := w.expectedHandoffDir(task)
		if err != nil {
			return result, fmt.Errorf("task %q handoff binding: %w", entry.IdempotencyKey, err)
		}
		if task.HandoffDir != expectedHandoff {
			return result, fmt.Errorf("task %q handoff %q, want %q: %w",
				entry.IdempotencyKey, task.HandoffDir, expectedHandoff,
				domain.ErrParentKeyMismatch)
		}
		outcome, err := w.reconcileTask(ctx, task)
		if err != nil {
			if errors.Is(err, publish.ErrJanitorInactive) {
				continue
			}
			return result, fmt.Errorf("task %q: %w", entry.IdempotencyKey, err)
		}
		result.completed++
		result.ready += boolCount(outcome.ready)
		result.blocked += boolCount(outcome.blocked)
		if outcome.prNumber > 0 {
			result.lastPRNumber = outcome.prNumber
		}
	}
	return result, nil
}

type taskOutcome struct {
	ready    bool
	blocked  bool
	prNumber int
}

func (w *fakePublicationWorkflow) reconcileTask(ctx context.Context, task fakePublicationTask) (taskOutcome, error) {
	if err := requireCommittedHandoff(task); err != nil {
		return taskOutcome{}, err
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

	checkout, err := w.transport.FetchBase(
		ctx, task.Repo, task.BaseRef, task.BaseSHA, filepath.Join(scratch, "checkout"),
	)
	if err != nil {
		return taskOutcome{}, fmt.Errorf("fetch exact base: %w", err)
	}
	if checkout.Repo() != task.Repo || checkout.BaseRef() != task.BaseRef ||
		checkout.BaseSHA() != task.BaseSHA {
		return taskOutcome{}, fmt.Errorf("transport checkout disagrees with task: %w", domain.ErrParentKeyMismatch)
	}

	imported, err := importer.Import(ctx, task.HandoffDir, checkout.Dir(), importer.Options{
		BaseSHA: task.BaseSHA, CommitDate: task.CommitDate, Policy: importPolicy,
	})
	if err != nil {
		return taskOutcome{}, fmt.Errorf("gauntlet import: %w", err)
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

	home := filepath.Join(scratch, "verify-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return taskOutcome{}, fmt.Errorf("create verification home: %w", err)
	}
	verified, err := verify.Verify(ctx, checkout.Dir(), verify.Options{
		HeadSHA: imported.CommitSHA, BaseSHA: task.BaseSHA,
		InvocationID: task.VerificationInvocationID,
		RecipeSource: verify.ConfigRecipe(w.recipe), RecipePath: task.RecipePath,
		Room: w.newRoom(home), ApprovedRecipes: w.approvedRecipes,
		Changes: imported.Changes,
		Policy: verify.Policy{
			ExtraVerificationControlPatterns: slices.Clone(
				profile.ProtectedPaths.ExtraVerificationControlPatterns,
			),
		},
	})
	if err != nil {
		return taskOutcome{}, fmt.Errorf("clean verification: %w", err)
	}
	if verified.HeadSHA != imported.CommitSHA || verified.RecipeDigest != task.RecipeDigest {
		return taskOutcome{}, fmt.Errorf("verification result disagrees with task binding: %w",
			domain.ErrParentKeyMismatch)
	}

	artifacts := make([]domain.Artifact, len(verified.Evidence))
	for i, evidence := range verified.Evidence {
		artifacts[i] = evidence.Artifact
	}
	findings := candidateFindings(imported.Findings, verified.Findings)
	verificationOutcome := domain.VerificationFailed
	if verified.Outcome == verify.OutcomePassed {
		verificationOutcome = domain.VerificationPassed
	}
	importDigest, err := digestJSON(imported)
	if err != nil {
		return taskOutcome{}, fmt.Errorf("digest import account: %w", err)
	}
	authorization, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: task.Repo, BaseSHA: task.BaseSHA, HeadSHA: imported.CommitSHA,
		ImportResultDigest: importDigest, VerificationRecipeDigest: verified.RecipeDigest,
		VerificationOutcome: verificationOutcome, Findings: findings,
		TrustProfileDigest: task.TrustProfileDigest,
		InvocationID:       task.VerificationInvocationID, CreatedAt: task.StartedAt,
	})
	if err != nil {
		return taskOutcome{}, fmt.Errorf("construct candidate authorization: %w", err)
	}
	if err := w.persistCandidateAccount(ctx, verified.Evidence, authorization); err != nil {
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

	recipeDigest := task.RecipeDigest
	authorizationID := authorization.ID
	profileDigest := task.TrustProfileDigest
	candidate := publish.Candidate{
		Repo: task.Repo, BaseRef: task.BaseRef, HeadSHA: imported.CommitSHA,
		Title: task.Title, Body: task.Body, Artifacts: artifacts,
		RecipeDigest: &recipeDigest, InvocationID: task.PublicationInvocationID,
		AuthorizationID: &authorizationID, TrustProfileDigest: &profileDigest,
	}
	w.candidates[task.PublicationInvocationID] = candidate
	defer delete(w.candidates, task.PublicationInvocationID)

	published, err := w.publisher.PublishAfterGate(
		ctx,
		candidate,
		w.approvedRecipes,
		func(ctx context.Context, identityInput publish.IdentityInput) error {
			_, err := w.transport.PushHead(ctx, checkout, identityInput)
			return err
		},
	)
	if err != nil {
		return taskOutcome{}, fmt.Errorf("publish candidate: %w", err)
	}
	if _, err := publish.DrainPublicationIntent(
		ctx, w.store, w.publisher, w, task.PublicationInvocationID,
	); err != nil {
		return taskOutcome{}, fmt.Errorf("finalize publication: %w", err)
	}
	ready, err := w.readyItem(task, imported, artifacts, published)
	if err != nil {
		return taskOutcome{}, err
	}
	if err := w.attention.PutItem(ctx, ready); err != nil {
		return taskOutcome{}, fmt.Errorf("create ready item: %w", err)
	}
	if err := w.finishTask(ctx, task); err != nil {
		return taskOutcome{}, err
	}
	return taskOutcome{ready: true, prNumber: published.PRNumber}, nil
}

// Resolve implements publish.CandidateResolver for the publication intent
// being finalized in the same reconciliation pass. The candidate is rebuilt
// from the durable task on every pass before any drain, so a restart never
// trusts process-local state as workflow authority.
func (w *fakePublicationWorkflow) Resolve(
	_ context.Context,
	intent publish.Intent,
) (publish.Candidate, map[domain.Digest]bool, error) {
	candidate, ok := w.candidates[intent.InvocationID]
	if !ok {
		return publish.Candidate{}, nil, fmt.Errorf(
			"no rebuilt candidate for invocation %q", intent.InvocationID,
		)
	}
	return candidate, maps.Clone(w.approvedRecipes), nil
}

func (w *fakePublicationWorkflow) commitHandoff(task fakePublicationTask) error {
	if _, err := os.Lstat(task.HandoffDir); err == nil {
		return errors.New("handoff path already exists before task commit")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect handoff target: %w", err)
	}
	parent := filepath.Dir(task.HandoffDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create handoff parent: %w", err)
	}
	temp, err := os.MkdirTemp(parent, ".handoff-")
	if err != nil {
		return fmt.Errorf("create handoff scratch: %w", err)
	}
	// Export wants to create its output directory, while MkdirTemp gives this
	// call a private claimed parent. Put the helper output one level below it.
	output := filepath.Join(temp, "output")
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temp)
		}
	}()
	if _, err := exporter.Export(os.DirFS(task.WorkspaceDir), output, exporter.Options{
		MaxBlobBytes:          importer.DefaultMaxBlobBytes,
		MaxTotalBlobBytes:     importer.DefaultMaxTotalBytes,
		MaxEntries:            importer.DefaultMaxEntries,
		MaxEvidenceBlobBytes:  importer.DefaultMaxBlobBytes,
		MaxEvidenceTotalBytes: importer.DefaultMaxTotalBytes,
	}); err != nil {
		return fmt.Errorf("export helper: %w", err)
	}
	if err := os.Rename(output, task.HandoffDir); err != nil {
		return fmt.Errorf("commit handoff: %w", err)
	}
	committed = true
	_ = os.RemoveAll(temp)
	return nil
}

func requireCommittedHandoff(task fakePublicationTask) error {
	info, err := os.Lstat(task.HandoffDir)
	if err != nil {
		return fmt.Errorf("inspect committed handoff: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("committed handoff path is not a real directory")
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

func (w *fakePublicationWorkflow) persistCandidateAccount(
	ctx context.Context,
	evidence []verify.Evidence,
	authorization domain.CandidateAuthorization,
) error {
	for _, item := range evidence {
		if _, err := w.artifacts.Put(item.Artifact.Digest, bytes.NewReader(item.Content)); err != nil {
			return fmt.Errorf("persist evidence artifact %s: %w", item.Artifact.ID, err)
		}
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		for _, item := range evidence {
			if err := tx.PutArtifact(ctx, item.Artifact); err != nil {
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
	if err := w.attention.PutItem(ctx, item); err != nil {
		return fmt.Errorf("create publish-blocked item: %w", err)
	}
	return nil
}

func (w *fakePublicationWorkflow) finishTask(ctx context.Context, task fakePublicationTask) error {
	return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, fakePublicationTaskKey(task.RunID))
	})
}

func (w *fakePublicationWorkflow) expectedHandoffDir(task fakePublicationTask) (string, error) {
	task.HandoffDir = ""
	payload, err := json.Marshal(task)
	if err != nil {
		return "", fmt.Errorf("encode handoff binding: %w", err)
	}
	sum := sha256.Sum256(payload)
	return filepath.Join(w.workDir, "handoffs", hex.EncodeToString(sum[:])), nil
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

func publicationRun(task fakePublicationTask) domain.Run {
	stageID := domain.StageID("publication-" + string(task.RunID))
	return domain.Run{
		ID: task.RunID, ProjectID: task.ProjectID,
		SpecDigest:   digestBytes(mustEncodeFakePublicationTask(task)),
		PolicyDigest: task.TrustProfileDigest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: task.RunID, Name: fakePublicationStageName,
			Attempts: []domain.Attempt{{
				ID:      domain.AttemptID("attempt-" + string(task.PublicationInvocationID)),
				StageID: stageID, Number: 1, InvocationID: task.PublicationInvocationID,
			}},
		}},
	}
}

func fakePublicationTaskKey(runID domain.RunID) string {
	return fakePublicationTaskKind + "/" + string(runID)
}

func readyItemID(runID domain.RunID) domain.ItemID {
	return domain.ItemID("ready-" + string(runID))
}

func blockedItemID(runID domain.RunID) domain.ItemID {
	return domain.ItemID("publish-blocked-" + string(runID))
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

func (task fakePublicationTask) validate() error {
	if task.Version != fakePublicationTaskVersion {
		return fmt.Errorf("unknown task version %q", task.Version)
	}
	if task.RunID == "" || task.ProjectID == "" || task.VerificationInvocationID == "" ||
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
		task.TrustProfileDigest == "" || task.Title == "" {
		return domain.ErrEmptyField
	}
	if task.CommitDate.IsZero() || task.StartedAt.IsZero() ||
		task.CommitDate.Location() != time.UTC || task.StartedAt.Location() != time.UTC {
		return errors.New("task timestamps must be non-zero UTC")
	}
	if len(task.AllowedPaths) == 0 {
		return errors.New("task has no candidate path allowlist")
	}
	for _, path := range task.AllowedPaths {
		if path == "" {
			return errors.New("task allowed path is empty")
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

var _ publish.CandidateResolver = (*fakePublicationWorkflow)(nil)
