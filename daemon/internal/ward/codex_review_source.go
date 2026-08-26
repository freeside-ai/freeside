package ward

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

var ErrCodexReviewOutcomeNotFound = errors.New("codex review outcome not found")

const codexProductionReviewPromptVersion = "codex-production-review-prompt-v3"

const codexProductionReviewRules = `Apply these daemon-owned Freeside review rules:
1. Trust re-derivation
   Flag: a change treats stored or caller-supplied publish eligibility, approval, or provenance bits as authoritative without re-running the applicable gate against current trusted state.
   Safe path: re-run the applicable gate against current trusted state and fail closed when trust cannot be established.
2. Verification integrity
   Flag: a change deletes or skips a failing test, loosens an assertion, or broadens a lint exclusion merely to make the change pass.
   Safe path: fix the implementation, or revise a genuinely obsolete check as its own visible and justified change.
3. Credential containment
   Flag: a change logs, exports, persists, or exposes credential material outside sealed stores and bounded daemon-owned delivery surfaces.
   Safe path: keep durable secrets sealed; use only private, daemon-owned ephemeral snapshots or volumes with read-only consumer mounts and cleanup for runtime delivery; refer to credentials by name and use placeholders in fixtures and examples.`

type CodexReviewSourceConfig struct {
	Lifecycle            *CodexReviewLifecycle
	Review               CodexReviewConfig
	Journal              CodexReviewJournal
	WorkspaceSizeMB      int64
	AuthMode             CodexAuthMode
	AuthIdentityID       domain.AuthIdentityID
	AuthSnapshot         string
	InstructionArtifacts CodexReviewInstructionArtifacts
	ConfigurationDigest  domain.Digest
	CostOwner            string
	Now                  func() time.Time

	// provider supplies the vendor-varying labels, version tags, and review
	// command. It is unexported so external callers cannot set it; the
	// constructor defaults it to the Codex provider, and the same-package Claude
	// runtime (#865) injects its own.
	provider reviewProvider
}

// CodexReviewInstructionArtifacts is the content-addressed closure used to
// reconstruct every source and the composed result before launch. Open must
// leave operational filesystem failures discoverable through *fs.PathError or
// syscall.Errno; missing content and unknown error shapes fail closed.
type CodexReviewInstructionArtifacts interface {
	Open(domain.Digest) (io.ReadCloser, error)
}

// CodexReviewSourceOutcome is durably collected before topology cleanup. The
// journal's separate ready bit proves cleanup finished before Poll can expose
// either a pass or a failure to the workflow.
type CodexReviewSourceOutcome struct {
	InvocationID       domain.InvocationID       `json:"invocation_id"`
	Result             *exec.ReviewResult        `json:"result"`
	CollectionEvidence domain.Digest             `json:"collection_evidence,omitempty"`
	FailureClass       domain.ReviewFailureClass `json:"failure_class,omitempty"`
	Failure            string                    `json:"failure,omitempty"`
	AbortRequired      bool                      `json:"abort_required,omitempty"`
}

// Validate rejects an outcome whose result and failure representations overlap
// or whose terminal payload is malformed. It is the provider-agnostic shape
// gate the persistence layer (wardstore) runs on write and read; it deliberately
// does not recompute the provider-namespaced completion evidence, which requires
// the caller's trusted provider and is re-gated by verifyCompletionEvidence at
// the domain layer where that provider is known.
func (o CodexReviewSourceOutcome) Validate() error {
	if o.InvocationID == "" {
		return domain.ErrEmptyID
	}
	if o.Result != nil {
		if o.Result.InvocationID != o.InvocationID {
			return domain.ErrParentKeyMismatch
		}
		if o.FailureClass != "" || o.Failure != "" || o.AbortRequired {
			return domain.ErrParentKeyMismatch
		}
		if err := o.Result.Validate(); err != nil || !contentaddr.Valid(string(o.CollectionEvidence)) {
			return errors.Join(err, domain.ErrInvalidReviewCompletionEvidence)
		}
		return nil
	}
	if o.FailureClass == "" || o.Failure == "" {
		return domain.ErrParentKeyMismatch
	}
	validClass := false
	for _, class := range domain.AllReviewFailureClasses {
		validClass = validClass || class == o.FailureClass
	}
	if !validClass {
		return domain.ErrInvalidReviewFailureClass
	}
	return nil
}

// verifyCompletionEvidence re-gates a loaded outcome's completion evidence
// against the caller's trusted provider, never a provider chosen by the decoded
// row. Two fail-closed gates: the persisted provider label must match the
// trusted provider (a rewritten row that flips the label and recomputes the
// unkeyed evidence cannot self-validate against a different validator), and the
// evidence must then recompute. Making it provider-aware fixes the #875 handoff
// (a Codex-only recomputation turned every Claude result into a durable
// contradiction). A failure/fence outcome carries no result evidence and passes.
func (o CodexReviewSourceOutcome) verifyCompletionEvidence(provider reviewProvider) error {
	if o.Result == nil {
		return nil
	}
	if o.Result.Provider != provider.providerLabel() {
		return domain.ErrInvalidReviewCompletionEvidence
	}
	evidence, err := reviewResultEvidence(provider, *o.Result, o.CollectionEvidence)
	if err != nil || evidence != o.Result.CompletionEvidence {
		return errors.Join(err, domain.ErrInvalidReviewCompletionEvidence)
	}
	return nil
}

// CodexReviewSource adapts ward's started-container handoff into the durable
// ReviewSource contract.
type CodexReviewSource struct {
	cfg      CodexReviewSourceConfig
	mu       sync.Mutex
	launches map[domain.InvocationID]*CodexReviewLaunch
}

var _ exec.ReviewSource = (*CodexReviewSource)(nil)

func NewCodexReviewSource(cfg CodexReviewSourceConfig) (*CodexReviewSource, error) {
	if !cfg.Lifecycle.valid() {
		return nil, fmt.Errorf("codex review source: %w: lifecycle is not initialized", ErrInvalidConfig)
	}
	if cfg.provider == nil {
		cfg.provider = codexReviewProvider{}
	}
	if lifecycleProvider := cfg.Lifecycle.reviewProvider(); lifecycleProvider.providerLabel() != cfg.provider.providerLabel() {
		return nil, fmt.Errorf(
			"codex review source: %w: lifecycle provider %q does not match source provider %q",
			ErrInvalidConfig, lifecycleProvider.providerLabel(), cfg.provider.providerLabel(),
		)
	}
	if cfg.Journal == nil || cfg.Review.Journal != cfg.Journal ||
		cfg.Review.VolumeLifecycleLeaser == nil || cfg.WorkspaceSizeMB <= 0 ||
		cfg.AuthIdentityID == "" || cfg.AuthSnapshot == "" || cfg.InstructionArtifacts == nil ||
		!contentaddr.Valid(string(cfg.ConfigurationDigest)) || cfg.CostOwner == "" {
		return nil, errors.New("codex review source: incomplete configuration")
	}
	digest, err := reviewConfigurationDigest(
		cfg.provider, cfg.Review, cfg.WorkspaceSizeMB, cfg.AuthMode, cfg.AuthIdentityID,
		cfg.CostOwner,
	)
	if err != nil || digest != cfg.ConfigurationDigest {
		return nil, fmt.Errorf("codex review source: configuration digest mismatch: %w",
			errors.Join(err, ErrInvalidCodexReviewSpec))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &CodexReviewSource{cfg: cfg, launches: make(map[domain.InvocationID]*CodexReviewLaunch)}, nil
}

func (s *CodexReviewSource) RequestReview(
	ctx context.Context, id domain.InvocationID, req exec.ReviewRequest,
) error {
	if err := req.Validate(); err != nil || !runIDPattern.MatchString(string(id)) {
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureConfiguration,
			Err:   errors.Join(err, ErrInvalidCodexReviewSpec),
		}
	}
	if _, err := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id)); err == nil {
		return fmt.Errorf("codex review request %s: %w", id, exec.ErrDuplicateStart)
	} else if !errors.Is(err, exec.ErrUnknownInvocation) {
		if errors.Is(err, ErrCodexReviewRequestRejected) {
			return s.rejectPersistedRequest(ctx, id, err)
		}
		return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	}
	if err := s.cfg.Journal.PutCodexReviewRequest(ctx, string(id), req); err != nil {
		return &exec.ReviewSourceFailure{Class: classifyCodexObservationFailure(err), Err: err}
	}
	if err := s.startRequestedReview(ctx, id, req); err != nil {
		return err
	}
	return nil
}

func (s *CodexReviewSource) startRequestedReview(
	ctx context.Context, id domain.InvocationID, req exec.ReviewRequest,
) error {
	releaseRun, err := s.cfg.Lifecycle.acquireCodexReviewRun(ctx, string(id))
	if err != nil {
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient,
			Err:   codexReviewOperationalf("acquire Codex review source run gate: %v", err),
		}
	}
	defer releaseRun()
	if err := codexReviewOutcomeFence(ctx, s.reviewProvider(), s.cfg.Journal, string(id)); err != nil {
		return &exec.ReviewSourceFailure{Class: classifyCodexLaunchFailure(err), Err: err}
	}
	if err := s.cfg.Lifecycle.authorizeRuntime(
		ctx, reviewRuntimeResourceNamesFor(s.reviewProvider(), string(id)),
	); err != nil {
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient,
			Err:   codexReviewOperationalf("authorize Codex review runtime resources: %v", err),
		}
	}
	candidate := domain.BaseRevision{
		Repo: req.Repo, RepositoryID: req.RepositoryID, BaseRef: req.BaseRef, BaseSHA: req.HeadSHA,
	}
	workspace, err := s.cfg.Journal.GetCodexReviewWorkspaceBinding(ctx, string(id))
	if errors.Is(err, ErrCodexReviewWorkspaceNotFound) ||
		(err == nil && workspace.CreationFingerprint == "") {
		workspace, err = s.cfg.Lifecycle.PrepareCodexReviewWorkspace(
			ctx, s.cfg.Journal, string(id), req.Workspace, candidate, s.cfg.WorkspaceSizeMB,
		)
	}
	if err != nil {
		return &exec.ReviewSourceFailure{Class: classifyCodexLaunchFailure(err), Err: err}
	}
	instructions, instructionFile, err := s.materializeReviewInstructions(ctx, id, req.Instructions)
	if err != nil {
		class := classifyCodexInstructionMaterializationFailure(err)
		if class == domain.ReviewFailureTransient {
			if cleanupErr := removeCodexReviewInstructionSnapshot(
				s.cfg.Review.InputRoot, string(id),
			); cleanupErr != nil {
				return codexReviewLaunchCleanupFailure(err, cleanupErr)
			}
			return &exec.ReviewSourceFailure{Class: class, Err: err}
		}
		cleanupErr := s.cfg.Lifecycle.cleanupOrphanedCodexReviewWorkspace(
			context.WithoutCancel(ctx), s.cfg.Journal, string(id))
		if cleanupErr != nil {
			return codexReviewLaunchCleanupFailure(err, cleanupErr)
		}
		return &exec.ReviewSourceFailure{Class: class, Err: err}
	}
	launch, err := s.cfg.Lifecycle.codexReview(ctx, s.cfg.Review, CodexReviewLaunchSpec{
		RunID: string(id), WorkflowRunID: req.RunID, Image: s.cfg.Review.ApprovedImage,
		WorkspaceSourceRunID: string(id), WorkspaceVolume: workspace.Volume,
		ExpectedHead: req.HeadSHA, Prompt: s.reviewProvider().reviewPrompt(req),
		Boundary: CodexReviewFreshStart, AuthMode: s.cfg.AuthMode,
		AuthIdentityID: s.cfg.AuthIdentityID, AuthSnapshot: s.cfg.AuthSnapshot,
		Instructions: instructions, InstructionFile: instructionFile,
		InstructionBinding: req.Instructions,
	})
	if err != nil {
		if classifyCodexLaunchFailure(err) == domain.ReviewFailureTransient {
			if cleanupErr := removeCodexReviewInstructionSnapshot(s.cfg.Review.InputRoot, string(id)); cleanupErr != nil {
				return codexReviewLaunchCleanupFailure(err, cleanupErr)
			}
			return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
		}
		cleanupErr := s.cfg.Lifecycle.CleanupCodexReviewWorkspace(
			context.WithoutCancel(ctx), s.cfg.Journal, string(id))
		if cleanupErr != nil {
			return codexReviewLaunchCleanupFailure(err, cleanupErr)
		}
		instructionCleanupErr := removeCodexReviewInstructionSnapshot(s.cfg.Review.InputRoot, string(id))
		if instructionCleanupErr != nil {
			return codexReviewLaunchCleanupFailure(err, instructionCleanupErr)
		}
		return &exec.ReviewSourceFailure{
			Class: classifyCodexLaunchFailure(err),
			Err:   err,
		}
	}
	s.mu.Lock()
	s.launches[id] = launch
	s.mu.Unlock()
	return nil
}

func (s *CodexReviewSource) materializeReviewInstructions(
	ctx context.Context,
	id domain.InvocationID,
	binding exec.ReviewInstructionBinding,
) (VendorInstructions, string, error) {
	reconstructed, err := s.reconstructReviewInstructions(ctx, binding)
	if err != nil {
		return VendorInstructions{}, "", err
	}
	path, err := s.writeReviewInstructionFile(id, reconstructed)
	if err != nil {
		return VendorInstructions{}, "", err
	}
	// The instruction vendor must be the source's own provider: the launch-shape
	// gate compares it against the injected provider's vendor, so a Codex-hardcoded
	// value would make a Claude source reject every request before launch.
	return VendorInstructions{
		Vendor:   s.reviewProvider().vendor(),
		Delivery: domain.VendorInstructionDeliveryAppendFile,
		Present:  true, Digest: binding.ResultDigest, Body: reconstructed,
	}, path, nil
}

func (s *CodexReviewSource) reconstructReviewInstructions(
	ctx context.Context,
	binding exec.ReviewInstructionBinding,
) ([]byte, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	if binding.CompositionVersion != exec.ReviewInstructionCompositionVersion {
		return nil, fmt.Errorf(
			"review instruction composition version %q cannot launch; current version is %q",
			binding.CompositionVersion, exec.ReviewInstructionCompositionVersion,
		)
	}
	host := exec.ReviewHostInstructionInput{Present: binding.HostDigest != nil}
	if binding.HostDigest != nil {
		body, err := s.readReviewInstructionArtifact(ctx, *binding.HostDigest)
		if err != nil {
			return nil, fmt.Errorf("load review host instructions: %w", err)
		}
		host.Body = body
	}
	sources := make([]exec.ReviewInstructionSourceInput, len(binding.RepositorySources))
	for i, source := range binding.RepositorySources {
		body, err := s.readReviewInstructionArtifact(ctx, source.Digest)
		if err != nil {
			return nil, fmt.Errorf(
				"load review repository instructions %q: %w", source.Path, err)
		}
		sources[i] = exec.ReviewInstructionSourceInput{Path: source.Path, Body: body}
	}
	reconstructed, gotBinding, err := exec.ComposeCodexReviewInstructions(host, sources)
	if err != nil || !sameReviewInstructionBinding(gotBinding, binding) {
		return nil, errors.Join(
			err, errors.New("reconstructed review instructions diverge from request authority"))
	}
	persisted, err := s.readReviewInstructionArtifact(ctx, binding.ResultDigest)
	if err != nil {
		return nil, fmt.Errorf("load persisted review instruction result: %w", err)
	}
	if !slices.Equal(persisted, reconstructed) {
		return nil, errors.New("persisted review instruction result is invalid")
	}
	return reconstructed, nil
}

func (s *CodexReviewSource) readReviewInstructionArtifact(
	ctx context.Context, digest domain.Digest,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, err := s.cfg.InstructionArtifacts.Open(digest)
	if err != nil {
		if codexReviewInstructionOpenIsOperational(err) {
			return nil, errors.Join(ErrCodexReviewOperational, err)
		}
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, domain.MaxVendorInstructionBytes+1))
	closeErr := reader.Close()
	if int64(len(body)) > domain.MaxVendorInstructionBytes {
		return nil, fmt.Errorf("review instruction artifact exceeds the byte limit: %w", ErrConformance)
	}
	if readErr != nil {
		return nil, errors.Join(ErrCodexReviewOperational, readErr, closeErr)
	}
	sum := sha256.Sum256(body)
	if domain.Digest(contentaddr.Format(sum[:])) != digest {
		return nil, fmt.Errorf("review instruction artifact does not match its digest: %w", ErrConformance)
	}
	if closeErr != nil {
		return nil, errors.Join(ErrCodexReviewOperational, closeErr)
	}
	return body, nil
}

func codexReviewInstructionOpenIsOperational(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno)
}

func (s *CodexReviewSource) writeReviewInstructionFile(
	id domain.InvocationID, body []byte,
) (string, error) {
	target := s.instructionFile(id)
	directory := filepath.Dir(target)
	if err := ensureCodexReviewInstructionRoot(s.cfg.Review.InputRoot); err != nil {
		return "", err
	}
	if err := removeCodexReviewInstructionSnapshot(s.cfg.Review.InputRoot, string(id)); err != nil {
		return "", err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("create review instruction snapshot directory: %w", err))
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // target is an owned deterministic snapshot path
	if err != nil {
		_ = os.Remove(directory)
		return "", errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("create review instruction snapshot: %w", err))
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(target)
		_ = os.Remove(directory)
		return "", errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("protect review instruction snapshot: %w", err))
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		_ = os.Remove(target)
		_ = os.Remove(directory)
		return "", errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("write review instruction snapshot: %w", err))
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(target)
		_ = os.Remove(directory)
		return "", errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("close review instruction snapshot: %w", err))
	}
	return target, nil
}

func sameReviewInstructionBinding(a, b exec.ReviewInstructionBinding) bool {
	hostEqual := a.HostDigest == nil && b.HostDigest == nil ||
		a.HostDigest != nil && b.HostDigest != nil && *a.HostDigest == *b.HostDigest
	return hostEqual && a.CompositionVersion == b.CompositionVersion &&
		a.ResultDigest == b.ResultDigest && slices.Equal(a.RepositorySources, b.RepositorySources)
}

func (s *CodexReviewSource) instructionFile(id domain.InvocationID) string {
	return filepath.Join(s.instructionRoot(), string(id), "AGENTS.md")
}

func (s *CodexReviewSource) instructionRoot() string {
	return codexReviewInstructionRoot(s.cfg.Review.InputRoot)
}

func codexReviewInstructionRoot(inputRoot string) string {
	return filepath.Join(inputRoot, ".freeside-review-instructions")
}

func ensureCodexReviewInstructionRoot(inputRoot string) error {
	rootInfo, err := os.Lstat(inputRoot)
	if err != nil {
		return errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("inspect review input root: %w", err))
	}
	rootStat, rootOwned := rootInfo.Sys().(*syscall.Stat_t)
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0o077 != 0 ||
		!rootOwned || !codexReviewUIDMatches(rootStat, os.Geteuid()) {
		return errors.New("review input root is not a private daemon-owned directory")
	}
	root := codexReviewInstructionRoot(inputRoot)
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("create review instruction root: %w", err))
	}
	info, err := os.Lstat(root)
	if err != nil {
		return errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("inspect review instruction root: %w", err))
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 ||
		!owned || !codexReviewUIDMatches(stat, os.Geteuid()) {
		return errors.New("review instruction root is not a private daemon-owned directory")
	}
	return nil
}

func removeCodexReviewInstructionSnapshot(inputRoot string, id string) error {
	if !runIDPattern.MatchString(id) {
		return fmt.Errorf("review instruction snapshot id is invalid: %w", ErrConformance)
	}
	root := codexReviewInstructionRoot(inputRoot)
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("inspect review instruction root: %w", err))
	}
	rootStat, rootOwned := rootInfo.Sys().(*syscall.Stat_t)
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0o077 != 0 ||
		!rootOwned || !codexReviewUIDMatches(rootStat, os.Geteuid()) {
		return fmt.Errorf("review instruction root is not a private daemon-owned directory: %w", ErrConformance)
	}
	directory := filepath.Join(root, id)
	directoryInfo, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("inspect review instruction snapshot directory: %w", err))
	}
	directoryStat, directoryOwned := directoryInfo.Sys().(*syscall.Stat_t)
	if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 ||
		directoryInfo.Mode().Perm()&0o077 != 0 || !directoryOwned ||
		!codexReviewUIDMatches(directoryStat, os.Geteuid()) {
		return fmt.Errorf("review instruction snapshot directory is not a private daemon-owned directory: %w", ErrConformance)
	}
	target := filepath.Join(directory, "AGENTS.md")
	targetInfo, err := os.Lstat(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("inspect review instruction snapshot: %w", err))
	}
	if err == nil {
		targetStat, targetOwned := targetInfo.Sys().(*syscall.Stat_t)
		if !targetInfo.Mode().IsRegular() || targetInfo.Mode()&os.ModeSymlink != 0 ||
			targetInfo.Mode().Perm()&0o077 != 0 || !targetOwned ||
			!codexReviewUIDMatches(targetStat, os.Geteuid()) {
			return fmt.Errorf("review instruction snapshot is not a private daemon-owned regular file: %w",
				ErrConformance)
		}
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("remove review instruction snapshot: %w", err))
	}
	if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
		if errors.Is(err, syscall.ENOTEMPTY) {
			return fmt.Errorf("review instruction snapshot directory contains unexpected entries: %w",
				ErrConformance)
		}
		return errors.Join(ErrCodexReviewOperational,
			fmt.Errorf("remove review instruction snapshot directory: %w", err))
	}
	return nil
}

// codexReviewLaunchCleanupFailure prevents a terminal launch refusal from
// advancing past a workspace whose cleanup has not yet converged.
func codexReviewLaunchCleanupFailure(launchErr, cleanupErr error) error {
	class := classifyCodexObservationFailure(cleanupErr)
	if class == domain.ReviewFailureTransient {
		return &exec.ReviewSourceFailure{Class: class, Err: errors.Join(launchErr, cleanupErr)}
	}
	return &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureContradiction, Err: errors.Join(launchErr, cleanupErr),
	}
}

func codexProductionReviewPrompt(req exec.ReviewRequest) string {
	evidence, _ := json.Marshal(req.Verification)
	return fmt.Sprintf(`Review the exact candidate at head %s against base %s. The preceding verification evidence is %s.

Apply a precision-first admission test focused on correctness, security, data loss, and regressions. Admit a finding only when all of these are true:
- The head-versus-base change introduced it.
- It has a demonstrable failure path.
- It is discrete and actionable.
- It is not speculative, pre-existing, or merely stylistic.
- It survives a deliberate attempt to falsify it against the code and diff.

%s

Return every finding that meets this bar through the required JSON schema. An empty findings array means that nothing met the admission bar; it does not claim the candidate is flawless.`,
		req.HeadSHA, req.BaseSHA, evidence, codexProductionReviewRules)
}

func (s *CodexReviewSource) Inspect(
	ctx context.Context, id domain.InvocationID,
) (exec.Status, error) {
	outcome, ready, err := s.cfg.Journal.GetCodexReviewOutcome(ctx, string(id))
	if err == nil {
		if err := errors.Join(outcome.Validate(), outcome.verifyCompletionEvidence(s.reviewProvider())); err != nil {
			return "", &exec.ReviewSourceFailure{Class: domain.ReviewFailureContradiction, Err: err}
		}
		if outcome.InvocationID != id {
			return "", &exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction, Err: domain.ErrParentKeyMismatch,
			}
		}
		if !ready {
			if err := s.finishCleanup(ctx, id, outcome.AbortRequired); err != nil {
				return codexReviewCleanupStatus(err)
			}
		}
		if outcome.Result != nil {
			return exec.StatusCompleted, nil
		}
		return exec.StatusFailed, nil
	}
	if !errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		if errors.Is(err, ErrCodexReviewOutcomeRejected) {
			return s.rejectedOutcomeStatus(ctx, id, err)
		}
		return "", &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	}
	request, err := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id))
	if err != nil {
		if errors.Is(err, ErrCodexReviewRequestRejected) {
			rejectErr := s.rejectPersistedRequest(ctx, id, err)
			return "", rejectErr
		}
		if !errors.Is(err, exec.ErrUnknownInvocation) {
			return "", &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
		}
		return "", err
	}
	if err := request.Validate(); err != nil {
		rejectErr := s.rejectPersistedRequest(ctx, id, err)
		if rejectErr != nil {
			return "", rejectErr
		}
	}
	intent, intentErr := s.cfg.Journal.GetCodexReviewIntent(ctx, string(id))
	if intentErr == nil && intent.validateIdentity(string(id)) != nil {
		return "", &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction,
			Err:   errors.New("persisted Codex review launch intent is invalid"),
		}
	}
	if errors.Is(intentErr, ErrCodexReviewIntentNotFound) ||
		(intentErr == nil && intent.State != CodexReviewIntentStarted) {
		if err := s.startRequestedReview(ctx, id, request); err != nil {
			return "", err
		}
		return exec.StatusRunning, nil
	}
	if intentErr != nil {
		return "", &exec.ReviewSourceFailure{
			Class: classifyCodexObservationFailure(intentErr), Err: intentErr,
		}
	}
	state, err := s.cfg.Lifecycle.InspectCodexReview(ctx, s.cfg.Review, string(id))
	if err != nil {
		return "", &exec.ReviewSourceFailure{Class: classifyCodexObservationFailure(err), Err: err}
	}
	if state == StateRunning {
		s.mu.Lock()
		launch := s.launches[id]
		s.mu.Unlock()
		if launch == nil {
			outcome = CodexReviewSourceOutcome{
				InvocationID:  id,
				FailureClass:  domain.ReviewFailureTransient,
				Failure:       "daemon restarted while Codex review was running; the invocation proxy was lost",
				AbortRequired: true,
			}
			if err := s.cfg.Journal.PutCodexReviewOutcome(ctx, string(id), outcome); err != nil {
				return "", codexReviewOutcomeWriteFailure(err)
			}
			if err := s.finishCleanup(ctx, id, true); err != nil {
				return codexReviewCleanupStatus(err)
			}
			return exec.StatusFailed, nil
		}
		return exec.StatusRunning, nil
	}
	collection, err := s.cfg.Lifecycle.CollectCodexReview(ctx, s.cfg.Review, string(id))
	if err != nil {
		if !errors.Is(err, ErrCodexReviewOutputInvalid) {
			return "", &exec.ReviewSourceFailure{Class: classifyCodexObservationFailure(err), Err: err}
		}
		outcome = CodexReviewSourceOutcome{
			InvocationID: id, FailureClass: domain.ReviewFailureContradiction,
			Failure: fmt.Sprintf("Codex review returned invalid raw output: %v", err),
		}
	} else {
		outcome = s.normalizeCollection(id, request, collection)
		if err := errors.Join(outcome.Validate(), outcome.verifyCompletionEvidence(s.reviewProvider())); err != nil {
			// A collected result that fails validation is a content contradiction
			// inside a healthy topology: the reviewer answered, the answer is
			// unusable. Persist it as a durable failure and finish authenticated
			// cleanup; an unpersisted early return would terminalize the
			// invocation while leaving the stopped credential-bearing topology
			// behind forever. Topology contradictions (conformance failures from
			// inspect/collect/cleanup) stay loud without cleanup above, because
			// there the topology itself can no longer be trusted to tear down.
			outcome = CodexReviewSourceOutcome{
				InvocationID:       id,
				FailureClass:       domain.ReviewFailureContradiction,
				Failure:            fmt.Sprintf("Codex review returned an invalid collected result: %v", err),
				CollectionEvidence: outcome.CollectionEvidence,
			}
		}
	}
	if err := s.cfg.Journal.PutCodexReviewOutcome(ctx, string(id), outcome); err != nil {
		return "", codexReviewOutcomeWriteFailure(err)
	}
	if err := s.finishCleanup(ctx, id, false); err != nil {
		return codexReviewCleanupStatus(err)
	}
	if outcome.Result != nil {
		return exec.StatusCompleted, nil
	}
	return exec.StatusFailed, nil
}

func codexReviewOutcomeWriteFailure(err error) error {
	return &exec.ReviewSourceFailure{
		Class: classifyCodexObservationFailure(err),
		Err:   fmt.Errorf("persist Codex review outcome: %w", err),
	}
}

func codexReviewCleanupStatus(err error) (exec.Status, error) {
	class := classifyCodexObservationFailure(err)
	return "", &exec.ReviewSourceFailure{Class: class, Err: err}
}

func (s *CodexReviewSource) finishCleanup(
	ctx context.Context, id domain.InvocationID, abort bool,
) error {
	s.mu.Lock()
	launch := s.launches[id]
	delete(s.launches, id)
	s.mu.Unlock()
	if launch != nil {
		_ = launch.Close()
	}
	var err error
	if abort {
		err = s.cfg.Lifecycle.AbortCodexReview(ctx, s.cfg.Review, string(id))
	} else {
		err = s.cfg.Lifecycle.CleanupCodexReview(ctx, s.cfg.Review, string(id))
	}
	if err != nil {
		return err
	}
	if err := removeCodexReviewInstructionSnapshot(s.cfg.Review.InputRoot, string(id)); err != nil {
		return err
	}
	return s.cfg.Journal.MarkCodexReviewOutcomeReady(ctx, string(id))
}

// reviewProvider returns the source's vendor seam, defaulting to Codex when a
// direct struct construction (e.g. a focused test) left it unset. Construction
// through NewCodexReviewSource always populates it.
func (s *CodexReviewSource) reviewProvider() reviewProvider {
	if s.cfg.provider != nil {
		return s.cfg.provider
	}
	return codexReviewProvider{}
}

func (s *CodexReviewSource) normalizeCollection(
	id domain.InvocationID, req exec.ReviewRequest, collection CodexReviewCollection,
) CodexReviewSourceOutcome {
	provider := s.reviewProvider()
	evidenceBytes := fmt.Appendf(nil, "%s:%d:", provider.completionEvidenceVersion(), len(collection.Events))
	evidenceBytes = append(evidenceBytes, collection.Events...)
	evidenceBytes = fmt.Appendf(evidenceBytes, ":%d:", len(collection.Result))
	evidenceBytes = append(evidenceBytes, collection.Result...)
	evidenceBytes = fmt.Appendf(evidenceBytes, ":%d", collection.ExitStatus)
	collectionEvidence := domain.Digest(contentaddr.Sum(evidenceBytes))
	if collection.ExitStatus != 0 {
		class, terminalMessage := classifyReviewTerminalFailure(provider, collection.Events)
		failure := fmt.Sprintf("Codex review exited with status %d", collection.ExitStatus)
		if codexRefreshAttemptFailure([]byte(terminalMessage)) {
			failure = "Codex review attempted an in-container credential refresh"
		}
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: class,
			Failure:      failure,
		}
	}
	var raw struct {
		Findings *[]struct {
			Severity string `json:"severity"`
			Location *struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
			} `json:"location"`
			Explanation string `json:"explanation"`
		} `json:"findings"`
	}
	if err := RejectDuplicateJSONKeys(collection.Result); err != nil {
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "Codex review returned malformed structured output",
		}
	}
	if err := strictjson.Decode(
		collection.Result, &raw, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return CodexReviewSourceOutcome{
				InvocationID: id,
				FailureClass: domain.ReviewFailureContradiction,
				Failure:      "Codex review returned trailing structured output",
			}
		}
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "Codex review returned malformed structured output",
		}
	}
	if raw.Findings == nil {
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "Codex review omitted the required findings array",
		}
	}
	contradiction := func(failure string) CodexReviewSourceOutcome {
		return CodexReviewSourceOutcome{
			InvocationID: id, FailureClass: domain.ReviewFailureContradiction, Failure: failure,
		}
	}
	completedAt := s.cfg.Now().UTC()
	findings := make([]domain.Finding, len(*raw.Findings))
	for i, item := range *raw.Findings {
		severity := domain.FindingSeverity(item.Severity)
		if !slices.Contains(domain.AllFindingSeverities, severity) {
			return contradiction("Codex review returned an out-of-domain finding severity")
		}
		if item.Location == nil {
			return contradiction("Codex review omitted a required finding location")
		}
		location := domain.FindingLocation{
			Path: item.Location.Path, StartLine: item.Location.StartLine, EndLine: item.Location.EndLine,
		}
		// The ward schema requires a concrete line range (start_line, end_line ≥ 1).
		// The whole-file location (0,0) is a native-ingest shape a codex review
		// never emits, so reject it alongside a partial, non-positive, or inverted
		// range as a contradiction rather than admitting a schema-escaping location.
		if err := location.Validate(); err != nil || location.StartLine < 1 {
			return contradiction("Codex review returned an invalid finding location")
		}
		identity := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s",
			id, req.BaseSHA, req.HeadSHA, severity,
			location.Path, location.StartLine, location.EndLine, item.Explanation)
		sum := sha256.Sum256([]byte(identity))
		findings[i] = domain.Finding{
			ID: domain.FindingID(fmt.Sprintf("review-%x", sum[:12])), RunID: req.RunID,
			Source: provider.sourceLabel(), Severity: severity, Location: &location,
			Message: item.Explanation, RawText: item.Explanation, CreatedAt: completedAt,
		}
	}
	result := exec.ReviewResult{
		InvocationID: id, BaseSHA: req.BaseSHA, HeadSHA: req.HeadSHA,
		Provider: provider.providerLabel(), ModelConfiguration: s.cfg.Review.Model + "/" + s.cfg.Review.ReasoningEffort,
		ConfigurationDigest: s.cfg.ConfigurationDigest,
		InstructionDigest:   req.Instructions.ResultDigest,
		CostOwner:           s.cfg.CostOwner, CompletedAt: completedAt,
		Findings: findings,
	}
	result.CompletionEvidence, _ = reviewResultEvidence(provider, result, collectionEvidence)
	return CodexReviewSourceOutcome{
		InvocationID: id, Result: &result, CollectionEvidence: collectionEvidence,
	}
}

// CodexReviewResultEvidence binds the normalized result to the authenticated,
// bounded raw collection account before either is persisted.
func CodexReviewResultEvidence(
	result exec.ReviewResult, collectionEvidence domain.Digest,
) (domain.Digest, error) {
	return reviewResultEvidence(codexReviewProvider{}, result, collectionEvidence)
}

func reviewResultEvidence(
	provider reviewProvider, result exec.ReviewResult, collectionEvidence domain.Digest,
) (domain.Digest, error) {
	result.CompletionEvidence = ""
	body, err := json.Marshal(struct {
		Version            string            `json:"version"`
		CollectionEvidence domain.Digest     `json:"collection_evidence"`
		Result             exec.ReviewResult `json:"result"`
	}{provider.resultEvidenceVersion(), collectionEvidence, result})
	if err != nil {
		return "", err
	}
	return domain.Digest(contentaddr.Sum(body)), nil
}

// CodexReviewConfigurationDigest binds the trust profile to every
// deployment-owned input that can change a production review's behavior.
// Credential material and host paths are deliberately excluded.
type codexReviewConfigurationEnvelope struct {
	Version                     string                `json:"version"`
	Topology                    string                `json:"topology"`
	ApprovedImage               string                `json:"approved_image"`
	ObserverImage               string                `json:"observer_image"`
	WorkspaceTarget             string                `json:"workspace_target"`
	WorkspaceSizeMB             int64                 `json:"workspace_size_mb"`
	ProviderEndpoints           []string              `json:"provider_endpoints"`
	Model                       string                `json:"model"`
	ReasoningEffort             string                `json:"reasoning_effort"`
	AccessTokenLifetimeFloor    int64                 `json:"access_token_lifetime_floor_ns"`
	AccessTokenRefreshThreshold int64                 `json:"access_token_refresh_threshold_ns"`
	AuthMode                    CodexAuthMode         `json:"auth_mode"`
	AuthIdentityID              domain.AuthIdentityID `json:"auth_identity_id"`
	CostOwner                   string                `json:"cost_owner"`
	CommandTemplateDigest       string                `json:"command_template_digest"`
	PromptProtocol              string                `json:"prompt_protocol"`
}

func newCodexReviewConfigurationEnvelope(
	provider reviewProvider,
	cfg CodexReviewConfig,
	workspaceSizeMB int64,
	authMode CodexAuthMode,
	authIdentityID domain.AuthIdentityID,
	costOwner string,
) (codexReviewConfigurationEnvelope, error) {
	if cfg.WorkspaceTarget == "" || !digestPinnedImagePattern.MatchString(cfg.ApprovedImage) ||
		!digestPinnedImagePattern.MatchString(cfg.ObserverImage) || cfg.Model == "" ||
		cfg.ReasoningEffort == "" || len(cfg.ProviderEndpoints) == 0 ||
		workspaceSizeMB <= 0 || !authMode.valid() || !provider.acceptsAuthMode(authMode) ||
		authIdentityID == "" || costOwner == "" {
		return codexReviewConfigurationEnvelope{}, ErrInvalidCodexReviewSpec
	}
	if provider.requiresExpiringCredential() &&
		(cfg.AccessTokenLifetimeFloor <= 0 || codexAuthRefreshThreshold(cfg) <= cfg.AccessTokenLifetimeFloor) {
		return codexReviewConfigurationEnvelope{}, ErrInvalidCodexReviewSpec
	}
	endpoints := slices.Clone(cfg.ProviderEndpoints)
	slices.Sort(endpoints)
	return codexReviewConfigurationEnvelope{
		Version: provider.configurationVersion(), Topology: provider.topologyVersion(),
		ApprovedImage: cfg.ApprovedImage, ObserverImage: cfg.ObserverImage,
		WorkspaceTarget: cfg.WorkspaceTarget, WorkspaceSizeMB: workspaceSizeMB,
		ProviderEndpoints: endpoints, Model: cfg.Model, ReasoningEffort: cfg.ReasoningEffort,
		AccessTokenLifetimeFloor:    int64(cfg.AccessTokenLifetimeFloor),
		AccessTokenRefreshThreshold: int64(codexAuthRefreshThreshold(cfg)), AuthMode: authMode,
		AuthIdentityID: authIdentityID, CostOwner: costOwner,
		CommandTemplateDigest: digestStrings(provider.reviewCommand(
			cfg.WorkspaceTarget, cfg.Model, cfg.ReasoningEffort, "<runtime-review-prompt>",
		)),
		PromptProtocol: provider.promptProtocol(),
	}, nil
}

func digestCodexReviewConfigurationEnvelope(
	envelope codexReviewConfigurationEnvelope,
) (domain.Digest, error) {
	body, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return domain.Digest(contentaddr.Sum(body)), nil
}

func CodexReviewConfigurationDigest(
	cfg CodexReviewConfig,
	workspaceSizeMB int64,
	authMode CodexAuthMode,
	authIdentityID domain.AuthIdentityID,
	costOwner string,
) (domain.Digest, error) {
	return reviewConfigurationDigest(
		codexReviewProvider{}, cfg, workspaceSizeMB, authMode, authIdentityID, costOwner,
	)
}

func reviewConfigurationDigest(
	provider reviewProvider,
	cfg CodexReviewConfig,
	workspaceSizeMB int64,
	authMode CodexAuthMode,
	authIdentityID domain.AuthIdentityID,
	costOwner string,
) (domain.Digest, error) {
	envelope, err := newCodexReviewConfigurationEnvelope(
		provider, cfg, workspaceSizeMB, authMode, authIdentityID, costOwner,
	)
	if err != nil {
		return "", err
	}
	return digestCodexReviewConfigurationEnvelope(envelope)
}

func (s *CodexReviewSource) Poll(
	ctx context.Context, id domain.InvocationID,
) (exec.ReviewResult, error) {
	outcome, ready, err := s.cfg.Journal.GetCodexReviewOutcome(ctx, string(id))
	if errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		if _, requestErr := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id)); requestErr != nil {
			if errors.Is(requestErr, exec.ErrUnknownInvocation) {
				return exec.ReviewResult{}, requestErr
			}
			if errors.Is(requestErr, ErrCodexReviewRequestRejected) {
				return exec.ReviewResult{}, s.rejectPersistedRequest(ctx, id, requestErr)
			}
			return exec.ReviewResult{}, codexReviewResultNotReady(requestErr)
		}
		return exec.ReviewResult{}, exec.ErrResultNotReady
	}
	if err != nil {
		if errors.Is(err, ErrCodexReviewOutcomeRejected) {
			return exec.ReviewResult{}, s.rejectPersistedOutcome(ctx, id, err)
		}
		return exec.ReviewResult{}, codexReviewResultNotReady(err)
	}
	if err := errors.Join(outcome.Validate(), outcome.verifyCompletionEvidence(s.reviewProvider())); err != nil {
		return exec.ReviewResult{}, &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction, Err: err,
		}
	}
	if outcome.InvocationID != id {
		return exec.ReviewResult{}, &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction, Err: domain.ErrParentKeyMismatch,
		}
	}
	if !ready {
		return exec.ReviewResult{}, exec.ErrResultNotReady
	}
	if outcome.Result == nil {
		return exec.ReviewResult{}, errors.Join(exec.ErrNoResult,
			&exec.ReviewSourceFailure{
				Class: outcome.FailureClass, Err: errors.New(outcome.Failure),
			})
	}
	return *outcome.Result, nil
}

func codexReviewResultNotReady(err error) error {
	return errors.Join(exec.ErrResultNotReady, &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureTransient,
		Err:   err,
	})
}

func (s *CodexReviewSource) Verify(
	ctx context.Context, id domain.InvocationID, expectedBase, expectedHead string,
) error {
	request, err := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id))
	if err != nil {
		if errors.Is(err, exec.ErrUnknownInvocation) {
			return err
		}
		if errors.Is(err, ErrCodexReviewRequestRejected) {
			return s.rejectPersistedRequest(ctx, id, err)
		}
		return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	}
	if err := request.Validate(); err != nil {
		return s.rejectPersistedRequest(ctx, id, err)
	}
	result, err := s.Poll(ctx, id)
	if err != nil {
		return err
	}
	if result.InvocationID != id || request.BaseSHA != result.BaseSHA ||
		request.HeadSHA != result.HeadSHA ||
		request.Instructions.ResultDigest != result.InstructionDigest {
		return domain.ErrParentKeyMismatch
	}
	if result.BaseSHA != expectedBase || result.HeadSHA != expectedHead {
		return exec.ErrStaleHead
	}
	return nil
}

func (s *CodexReviewSource) VerifyRequestAuthority(
	ctx context.Context, id domain.InvocationID, expected domain.Digest,
) error {
	request, err := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id))
	if err != nil {
		if errors.Is(err, exec.ErrUnknownInvocation) {
			return err
		}
		if errors.Is(err, ErrCodexReviewRequestRejected) {
			return s.rejectPersistedRequest(ctx, id, err)
		}
		return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	}
	authority, err := request.AuthorityDigest()
	if err != nil || authority != expected {
		if reconcileErr := s.reconcileRejectedRequest(ctx, id); reconcileErr != nil {
			return reconcileErr
		}
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureContradiction,
			Err:   errors.Join(err, domain.ErrParentKeyMismatch),
		}
	}
	if _, err := s.reconstructReviewInstructions(ctx, request.Instructions); err != nil {
		class := classifyCodexInstructionMaterializationFailure(err)
		if class == domain.ReviewFailureContradiction {
			if reconcileErr := s.reconcileRejectedRequest(ctx, id); reconcileErr != nil {
				return reconcileErr
			}
		}
		return &exec.ReviewSourceFailure{
			Class: class,
			Err:   fmt.Errorf("reconstruct review instruction authority: %w", err),
		}
	}
	return nil
}

func (s *CodexReviewSource) VerifyReviewRequestSupersession(
	ctx context.Context, id domain.InvocationID, expected exec.ReviewRequest,
) error {
	request, err := s.cfg.Journal.GetCodexReviewRequest(ctx, string(id))
	if err != nil {
		if errors.Is(err, exec.ErrUnknownInvocation) {
			return err
		}
		if errors.Is(err, ErrCodexReviewRequestRejected) {
			return s.rejectPersistedRequest(ctx, id, err)
		}
		return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	}
	if err := request.Validate(); err != nil {
		return s.rejectPersistedRequest(ctx, id, err)
	}
	withCurrentInstructions := request
	withCurrentInstructions.Instructions = expected.Instructions
	currentAuthority, err := withCurrentInstructions.AuthorityDigest()
	expectedAuthority, expectedErr := expected.AuthorityDigest()
	if err != nil || expectedErr != nil || currentAuthority != expectedAuthority ||
		sameReviewInstructionBinding(request.Instructions, expected.Instructions) {
		return nil
	}
	if reconcileErr := s.reconcileRejectedRequest(ctx, id); reconcileErr != nil {
		return reconcileErr
	}
	return &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureTransient,
		Err:   exec.ErrSupersededReviewRequest,
	}
}

func (s *CodexReviewSource) rejectPersistedRequest(
	ctx context.Context, id domain.InvocationID, cause error,
) error {
	if reconcileErr := s.reconcileRejectedRequest(ctx, id); reconcileErr != nil {
		return errors.Join(reconcileErr, cause)
	}
	return &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureContradiction,
		Err:   errors.Join(ErrCodexReviewRequestRejected, cause),
	}
}

func (s *CodexReviewSource) rejectPersistedOutcome(
	ctx context.Context, id domain.InvocationID, cause error,
) error {
	if cleanupErr := s.finishCleanup(ctx, id, true); cleanupErr != nil {
		return &exec.ReviewSourceFailure{
			Class: classifyCodexObservationFailure(cleanupErr), Err: cleanupErr,
		}
	}
	return &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureContradiction,
		Err:   errors.Join(ErrCodexReviewOutcomeRejected, cause),
	}
}

func (s *CodexReviewSource) rejectedOutcomeStatus(
	ctx context.Context, id domain.InvocationID, cause error,
) (exec.Status, error) {
	err := s.rejectPersistedOutcome(ctx, id, cause)
	return "", err
}

// reconcileRejectedRequest converges teardown for an invocation whose
// persisted request has just been rejected (rewritten past its recomputed
// authority, or invalid), before the rejection terminalizes the invocation.
// Without it the rejection strands whatever the original request already
// started: reconciliation halts at the immutable contradiction before
// Inspect, so no later pass would ever abort the credential-bearing
// topology. Once the durable binding exists (every state from prepared
// onward), the invocation is aborted through the durable outcome/ready
// protocol, so a crash mid-teardown resumes on the next verification
// instead of leaking; that teardown authenticates purely against the
// launch intent, binding, and ownership labels, never any row the
// rejected request could have influenced (the workspace reap and the
// outcome row are re-checked here for the same reason). A never-started
// or already-closed invocation reaps at most its prepared workspace
// volume. Preparing spans both sides of binding persistence, so rejection
// first writes an outcome that atomically fences the preparing-to-prepared
// transition before cleanup decides which durable topology now exists.
// A nil return means nothing this source started remains live, and the
// caller reports the rejection itself.
func (s *CodexReviewSource) reconcileRejectedRequest(
	ctx context.Context, id domain.InvocationID,
) error {
	outcome, ready, err := s.cfg.Journal.GetCodexReviewOutcome(ctx, string(id))
	if err == nil {
		if validateErr := errors.Join(outcome.Validate(), outcome.verifyCompletionEvidence(s.reviewProvider())); validateErr != nil || outcome.InvocationID != id {
			return &exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction,
				Err:   errors.Join(validateErr, domain.ErrParentKeyMismatch),
			}
		}
		if ready {
			return nil
		}
	} else if !errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		if errors.Is(err, ErrCodexReviewOutcomeRejected) {
			return s.rejectPersistedOutcome(ctx, id, err)
		}
		return &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: err}
	} else {
		// Persist the fence before waiting for an in-process launch. It must also
		// exist when no intent is visible yet: a launch holding the run gate may
		// not have reached BeginIntent, and its prepared transition must still
		// lose to this rejection.
		rejected := CodexReviewSourceOutcome{
			InvocationID:  id,
			FailureClass:  domain.ReviewFailureContradiction,
			Failure:       "persisted Codex review request was rejected after launch admission; any prepared invocation is aborted",
			AbortRequired: true,
		}
		if err := s.cfg.Journal.PutCodexReviewOutcome(ctx, string(id), rejected); err != nil {
			return codexReviewOutcomeWriteFailure(err)
		}
	}

	releaseRun, err := s.cfg.Lifecycle.acquireCodexReviewRun(ctx, string(id))
	if err != nil {
		return &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient,
			Err:   codexReviewOperationalf("acquire Codex review rejection run gate: %v", err),
		}
	}
	defer releaseRun()
	// abort=true regardless of the decoded AbortRequired bit: in this threat
	// model the row is rewritable, a flipped bit against a live container would
	// refuse teardown forever, and abort also reaps an already stopped topology.
	if cleanupErr := s.finishRejectedRequestCleanup(ctx, id); cleanupErr != nil {
		return &exec.ReviewSourceFailure{
			Class: classifyCodexObservationFailure(cleanupErr), Err: cleanupErr,
		}
	}
	return nil
}

func (s *CodexReviewSource) finishRejectedRequestCleanup(
	ctx context.Context, id domain.InvocationID,
) error {
	intent, err := s.cfg.Journal.GetCodexReviewIntent(ctx, string(id))
	if errors.Is(err, ErrCodexReviewIntentNotFound) {
		if cleanupErr := s.cfg.Lifecycle.CleanupCodexReviewWorkspace(
			ctx, s.cfg.Journal, string(id),
		); cleanupErr != nil && !errors.Is(cleanupErr, ErrCodexReviewWorkspaceNotFound) {
			return cleanupErr
		}
	} else if err != nil {
		return err
	} else if intent.validateIdentity(string(id)) != nil {
		return failf(CheckControlPlaneIsolation, "persisted Codex review launch intent is invalid")
	} else {
		switch intent.State {
		case CodexReviewIntentPreparing, CodexReviewIntentPrepared, CodexReviewIntentStarting:
			if err := s.cfg.Lifecycle.recoverCodexReviewIntent(ctx, s.cfg.Review, intent, true); err != nil {
				return err
			}
		case CodexReviewIntentStarted:
			return s.finishCleanup(ctx, id, true)
		case CodexReviewIntentClosed:
			if cleanupErr := s.cfg.Lifecycle.CleanupCodexReviewWorkspace(
				ctx, s.cfg.Journal, string(id),
			); cleanupErr != nil && !errors.Is(cleanupErr, ErrCodexReviewWorkspaceNotFound) {
				return cleanupErr
			}
		}
	}
	if err := removeCodexReviewInstructionSnapshot(s.cfg.Review.InputRoot, string(id)); err != nil {
		return err
	}
	return s.cfg.Journal.MarkCodexReviewOutcomeReady(ctx, string(id))
}

// classifyCodexLaunchFailure maps a pre-start (workspace-preparation and ward
// launch) failure to its durable ReviewFailureClass. Precedence is
// operational > contradiction > configuration:
//
//   - ErrCodexReviewOperational first, so runtime/storage I/O stays transient
//     even when a shared path wraps it in the conformance class.
//   - ErrConformance next: every launch-reachable conformance failure is an
//     authenticated live/durable contradiction found after launch admission
//     (changed auth/instruction snapshot, command/mount divergence, invalid or
//     divergent journal binding, foreign/unprovable owned object, persisted
//     binding disagreement), so it fails loudly rather than presenting as a
//     repairable deployment problem. Checked before the spec sentinel so an
//     error carrying both takes the loud stop branch (fail-closed).
//   - ErrInvalidCodexReviewSpec last: only invalid static deployment/request
//     shape is operator-repairable configuration.
//
// This is classifyCodexObservationFailure plus the trailing spec branch. The
// two are kept separate rather than merged: the observation classifier
// deliberately has no spec branch (a post-launch spec error, were one to
// arise, falls through to transient), so folding one in would add
// spec->configuration to the observation path. #499 fences that off as a
// non-goal (no change to observation-path classification); the spec branch
// stays launch-only.
func classifyCodexLaunchFailure(err error) domain.ReviewFailureClass {
	if errors.Is(err, ErrCodexReviewOperational) {
		return domain.ReviewFailureTransient
	}
	if errors.Is(err, ErrConformance) {
		return domain.ReviewFailureContradiction
	}
	if errors.Is(err, ErrInvalidCodexReviewSpec) {
		return domain.ReviewFailureConfiguration
	}
	return domain.ReviewFailureTransient
}

// classifyCodexInstructionMaterializationFailure is deliberately fail-closed:
// only positively identified operational I/O and cancellation may retry the
// same invocation before a credential-bearing reviewer starts.
func classifyCodexInstructionMaterializationFailure(err error) domain.ReviewFailureClass {
	if errors.Is(err, ErrCodexReviewOperational) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return domain.ReviewFailureTransient
	}
	return domain.ReviewFailureContradiction
}

func classifyCodexObservationFailure(err error) domain.ReviewFailureClass {
	if errors.Is(err, ErrCodexReviewOperational) {
		return domain.ReviewFailureTransient
	}
	if errors.Is(err, ErrConformance) {
		return domain.ReviewFailureContradiction
	}
	return domain.ReviewFailureTransient
}

func classifyCodexTerminalFailure(events []byte) (domain.ReviewFailureClass, string) {
	return classifyReviewTerminalFailure(codexReviewProvider{}, events)
}

func classifyReviewTerminalFailure(
	provider reviewProvider, events []byte,
) (domain.ReviewFailureClass, string) {
	message := provider.terminalFailureMessage(events)
	text := strings.ToLower(message)
	switch {
	case codexRefreshAttemptFailure([]byte(message)):
		return domain.ReviewFailureConfiguration, message
	case strings.Contains(text, "quota"), strings.Contains(text, "rate limit"),
		strings.Contains(text, "too many requests"):
		return domain.ReviewFailureQuota, message
	case strings.Contains(text, "authentication"), strings.Contains(text, "unauthorized"),
		strings.Contains(text, "invalid api key"), strings.Contains(text, "configuration"):
		return domain.ReviewFailureConfiguration, message
	default:
		return domain.ReviewFailureTransient, message
	}
}

func codexTerminalFailureMessage(events []byte) string {
	var message string
	for line := range bytes.SplitSeq(events, []byte("\n")) {
		var terminal struct {
			Type  string `json:"type"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := RejectDuplicateJSONKeys(line); err != nil {
			continue
		}
		if err := json.Unmarshal(line, &terminal); err != nil || terminal.Type != "turn.failed" {
			continue
		}
		message = terminal.Error.Message
	}
	return message
}

func codexRefreshAttemptFailure(events []byte) bool {
	text := strings.ToLower(string(events))
	return strings.Contains(text, "failed to refresh token") ||
		strings.Contains(text, "refresh token was already used") ||
		strings.Contains(text, "invalid 'refresh_token'")
}
