package ward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	defaultCodexAuthEnrollmentLeaseDuration = 2 * time.Minute
	defaultCodexAuthEnrollmentTeardown      = 30 * time.Second
	defaultCodexAuthEnrollmentFloor         = time.Hour
	defaultCodexAuthEnrollmentThreshold     = 2 * time.Hour
)

var errCodexAuthEnrollmentRetryLiveStore = errors.New("verified codex auth enrollment needs live-store rotation")

// CodexAuthEnrollmentFailure is the credential-free terminal class exposed
// to ward's persistence adapter.
type CodexAuthEnrollmentFailure string

const (
	CodexAuthEnrollmentReplacementFailed  CodexAuthEnrollmentFailure = "auth_store_replacement_failed"
	CodexAuthEnrollmentVerificationFailed CodexAuthEnrollmentFailure = "verification_failed"
)

// AllCodexAuthEnrollmentFailures is the complete registration point for
// credential-free enrollment terminal classes.
var AllCodexAuthEnrollmentFailures = []CodexAuthEnrollmentFailure{
	CodexAuthEnrollmentReplacementFailed,
	CodexAuthEnrollmentVerificationFailed,
}

func (f CodexAuthEnrollmentFailure) valid() bool {
	switch f {
	case CodexAuthEnrollmentReplacementFailed, CodexAuthEnrollmentVerificationFailed:
		return true
	default:
		return false
	}
}

// CodexAuthEnrollmentJournal is ward's narrow port to the #684 journal and
// recovery-binding contract. Implementations own the transaction that creates
// an initial identity and marker, or authenticates the existing marker, before
// opening the pending operation under the exact lease.
type CodexAuthEnrollmentJournal interface {
	Begin(
		ctx context.Context,
		identity domain.AuthIdentity,
		projectID domain.ProjectID,
		holder domain.InvocationID,
		now, expiresAt time.Time,
	) (domain.AuthStoreMutationLease, error)
	Fail(
		ctx context.Context,
		id domain.AuthIdentityID,
		holder domain.InvocationID,
		fence int64,
		class CodexAuthEnrollmentFailure,
		at time.Time,
	) error
	Verify(
		ctx context.Context,
		id domain.AuthIdentityID,
		holder domain.InvocationID,
		fence int64,
		digest domain.Digest,
		expiresAt, verifiedAt time.Time,
	) error
	RecoverableVerified(
		ctx context.Context,
		identity domain.AuthIdentity,
	) (domain.CodexReenrollmentRecoveryBinding, bool, error)
	ProjectVerified(ctx context.Context, id domain.AuthIdentityID) (domain.AttentionItem, error)
}

// CodexAuthEnrollmentConfig identifies one operator-supplied login and its
// daemon-owned destination. InputRoot and AuthStoreRoot must be separate
// private directory trees so an enrollment input cannot be mistaken for the
// live review store.
type CodexAuthEnrollmentConfig struct {
	InputRoot      string
	InputFile      string
	AuthStoreRoot  string
	AuthStorePath  string
	AuthIdentityID domain.AuthIdentityID
	ProjectID      domain.ProjectID

	Journal         CodexAuthEnrollmentJournal
	AuthStoreLeaser AuthStoreLeaser
	AuthRefresher   CodexAuthRefresher
	Now             func() time.Time

	LeaseDuration               time.Duration
	TeardownTimeout             time.Duration
	AccessTokenLifetimeFloor    time.Duration
	AccessTokenRefreshThreshold time.Duration
}

// CodexAuthEnrollmentResult contains only the durable, non-secret recovery
// coordinates an operator needs to finish the command-backed resolution.
type CodexAuthEnrollmentResult struct {
	AuthIdentityID       domain.AuthIdentityID `json:"auth_identity_id"`
	AuthStorePath        string                `json:"auth_store_path"`
	LeaseFence           int64                 `json:"lease_fence"`
	AuthStoreDigest      domain.Digest         `json:"auth_store_digest"`
	AccessTokenExpiresAt time.Time             `json:"access_token_expires_at"`
	AttentionItemID      domain.ItemID         `json:"attention_item_id"`
	AttentionItemVersion int                   `json:"attention_item_version"`
}

// EnrollCodexAuth seeds or replaces one Codex host auth store, deliberately
// spends the operator's refresh token through the shared daemon-owned refresh
// transaction, and projects exact verified evidence onto the recovery item.
// The operator still resolves that item through resolve_reenrollment; this
// function never bypasses signet's command-backed match-or-refuse boundary.
func EnrollCodexAuth(
	ctx context.Context, cfg CodexAuthEnrollmentConfig,
) (_ CodexAuthEnrollmentResult, retErr error) {
	if err := normalizeCodexAuthEnrollmentConfig(&cfg); err != nil {
		return CodexAuthEnrollmentResult{}, err
	}
	inputRoot, err := resolvePrivateCodexAuthRoot(cfg.InputRoot)
	if err != nil {
		return CodexAuthEnrollmentResult{}, fmt.Errorf("enrollment input root: %w", err)
	}
	storeRoot, err := resolvePrivateCodexAuthRoot(cfg.AuthStoreRoot)
	if err != nil {
		return CodexAuthEnrollmentResult{}, fmt.Errorf("auth-store root: %w", err)
	}
	if privateRootsOverlap(inputRoot, storeRoot) {
		return CodexAuthEnrollmentResult{}, errors.New("enrollment input root and auth-store root must be separate trees")
	}
	storePath, err := resolveCodexAuthStoreTarget(storeRoot, cfg.AuthStorePath)
	if err != nil {
		return CodexAuthEnrollmentResult{}, err
	}
	identity := domain.AuthIdentity{
		ID: cfg.AuthIdentityID, Provider: "openai", AuthStoreMutationLease: true,
		AuthStoreVolume: storePath, MaxParallelExecutions: 1,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
	}
	retryLiveStore := false
	if recovered, found, err := cfg.Journal.RecoverableVerified(ctx, identity); err != nil {
		return CodexAuthEnrollmentResult{}, fmt.Errorf("recover verified Codex re-enrollment: %w", err)
	} else if found {
		result, ok, err := recoverVerifiedCodexAuthEnrollment(
			ctx, cfg, storeRoot, storePath, recovered,
		)
		if err != nil {
			if !errors.Is(err, errCodexAuthEnrollmentRetryLiveStore) {
				return CodexAuthEnrollmentResult{}, err
			}
			retryLiveStore = true
		}
		if ok {
			return result, nil
		}
	}
	inputPath := storePath
	var inputBody []byte
	if retryLiveStore {
		_, inputBody, err = readCodexReviewInput(storeRoot, storePath, maxCodexAuthSnapshotBytes)
	} else {
		inputPath, inputBody, err = readCodexReviewInput(inputRoot, cfg.InputFile, maxCodexAuthSnapshotBytes)
	}
	if err != nil {
		return CodexAuthEnrollmentResult{}, fmt.Errorf("read enrollment auth.json: %w", err)
	}
	if !retryLiveStore && inputPath == storePath {
		return CodexAuthEnrollmentResult{}, errors.New("enrollment input and live auth store must be different files")
	}
	inputAuth, _, err := inspectCodexHostAuth(CodexAuthSubscription, inputBody)
	if err != nil || inputAuth.Tokens == nil || inputAuth.Tokens.RefreshToken == nil ||
		*inputAuth.Tokens.RefreshToken == "" {
		return CodexAuthEnrollmentResult{}, errors.New("enrollment auth.json is not a refreshable Codex subscription login")
	}
	owner, err := newOwnershipLabel()
	if err != nil {
		return CodexAuthEnrollmentResult{}, errors.New("mint Codex auth enrollment holder")
	}
	holder := domain.InvocationID("codex-auth-enrollment-" + owner.Value)
	now := cfg.Now()
	lease, err := cfg.Journal.Begin(
		ctx, identity, cfg.ProjectID, holder, now, now.Add(cfg.LeaseDuration),
	)
	if err != nil {
		return CodexAuthEnrollmentResult{}, fmt.Errorf("begin Codex auth enrollment: %w", err)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.TeardownTimeout)
		defer cancel()
		releaseErr := cfg.AuthStoreLeaser.Release(
			releaseCtx, cfg.AuthIdentityID, holder, lease.Fence, cfg.Now(),
		)
		if releaseErr != nil && !errors.Is(releaseErr, ErrLeaseWindowEnded) {
			retErr = errors.Join(retErr, fmt.Errorf("release Codex auth enrollment lease: %w", releaseErr))
		}
	}()

	fail := func(class CodexAuthEnrollmentFailure, cause error) (CodexAuthEnrollmentResult, error) {
		if !class.valid() {
			return CodexAuthEnrollmentResult{}, errors.Join(cause, errors.New("invalid Codex auth enrollment failure class"))
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.TeardownTimeout)
		defer cancel()
		persistErr := cfg.Journal.Fail(
			persistCtx, cfg.AuthIdentityID, holder, lease.Fence, class, cfg.Now(),
		)
		return CodexAuthEnrollmentResult{}, errors.Join(cause, persistErr)
	}
	refreshCfg := CodexReviewConfig{
		InputRoot:                   storeRoot,
		AccessTokenLifetimeFloor:    cfg.AccessTokenLifetimeFloor,
		AccessTokenRefreshThreshold: cfg.AccessTokenRefreshThreshold,
		AuthStoreLeaser:             cfg.AuthStoreLeaser,
		AuthRefresher:               cfg.AuthRefresher,
		Now:                         cfg.Now,
	}
	if err := verifyCodexAuthRefreshLease(
		ctx, refreshCfg, lease, cfg.AuthIdentityID, holder,
	); err != nil {
		return fail(CodexAuthEnrollmentReplacementFailed, err)
	}
	mutate, err := codexAuthLeaseMutationGuard(ctx, refreshCfg, lease)
	if err != nil {
		return fail(CodexAuthEnrollmentReplacementFailed, err)
	}
	var rotatedBody []byte
	var expiresAt time.Time
	if _, statErr := os.Lstat(storePath); statErr == nil {
		resolvedPath, body, metadata, err := readCodexReviewInputWithMetadata(
			storeRoot, storePath, maxCodexAuthSnapshotBytes,
		)
		if err != nil || resolvedPath != storePath {
			return fail(CodexAuthEnrollmentVerificationFailed,
				errors.New("existing Codex auth store failed hardening verification"))
		}
		recovered, err := recoverCodexAuthRefreshTransactionUnderLease(
			storeRoot, storePath, cfg.AuthIdentityID, body, metadata,
			cfg.AccessTokenRefreshThreshold, true, mutate,
		)
		if err != nil {
			if !errors.Is(err, errCodexAuthRefreshPredecessorMayBeConsumed) {
				return fail(CodexAuthEnrollmentVerificationFailed, err)
			}
			if err := discardCodexAuthRefreshIntentForReenrollmentUnderLease(
				storeRoot, storePath, cfg.AuthIdentityID, body, metadata, mutate,
			); err != nil {
				return fail(CodexAuthEnrollmentVerificationFailed, err)
			}
		}
		if recovered {
			if err := verifyCodexAuthRefreshLease(
				ctx, refreshCfg, lease, cfg.AuthIdentityID, holder,
			); err != nil {
				return fail(CodexAuthEnrollmentVerificationFailed, err)
			}
			_, rotatedBody, _, err = readCodexReviewInputWithMetadata(
				storeRoot, storePath, maxCodexAuthSnapshotBytes,
			)
			var recoveredExpiry *time.Time
			if err == nil {
				_, recoveredExpiry, err = inspectCodexHostAuth(CodexAuthSubscription, rotatedBody)
			}
			if err != nil || recoveredExpiry == nil {
				return fail(CodexAuthEnrollmentVerificationFailed,
					errors.New("recovered Codex auth rotation is unusable"))
			}
			expiresAt = recoveredExpiry.UTC()
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fail(CodexAuthEnrollmentReplacementFailed,
			errors.New("inspect existing Codex auth store"))
	}
	if rotatedBody == nil {
		var body []byte
		var metadata codexReviewInputMetadata
		if err := mutate(func() error {
			if err := atomicfile.WriteFile(storePath, inputBody, 0o600); err != nil {
				return errors.New("replace Codex auth store")
			}
			resolvedPath, replacedBody, replacedMetadata, err := readCodexReviewInputWithMetadata(
				storeRoot, storePath, maxCodexAuthSnapshotBytes,
			)
			if err != nil || resolvedPath != storePath || !bytes.Equal(replacedBody, inputBody) {
				return errors.New("replaced Codex auth store failed hardening verification")
			}
			body, metadata = replacedBody, replacedMetadata
			return nil
		}); err != nil {
			return fail(CodexAuthEnrollmentReplacementFailed, err)
		}
		auth, _, err := inspectCodexHostAuth(CodexAuthSubscription, body)
		if err != nil {
			return fail(CodexAuthEnrollmentVerificationFailed, err)
		}
		rotatedBody, expiresAt, err = rotateCodexAuthStoreUnderLease(
			ctx, refreshCfg, cfg.AuthIdentityID, storePath, body, metadata,
			auth, lease, holder, true,
		)
		if err != nil {
			return fail(CodexAuthEnrollmentVerificationFailed, err)
		}
	}
	agentSnapshot, _, err := codexReviewAgentAuthSnapshot(CodexAuthSubscription, rotatedBody)
	if err != nil {
		return fail(CodexAuthEnrollmentVerificationFailed, err)
	}
	snapshotExpiry, err := inspectCodexAuthSnapshot(CodexAuthSubscription, agentSnapshot)
	verifiedAt := cfg.Now()
	if err != nil || snapshotExpiry == nil || !snapshotExpiry.Equal(expiresAt) ||
		expiresAt.Sub(verifiedAt) < cfg.AccessTokenLifetimeFloor {
		return fail(CodexAuthEnrollmentVerificationFailed, errors.New("codex enrollment produced an unusable access-only snapshot"))
	}
	if err := verifyCodexAuthRefreshLease(
		ctx, refreshCfg, lease, cfg.AuthIdentityID, holder,
	); err != nil {
		return fail(CodexAuthEnrollmentVerificationFailed, err)
	}
	digest := domain.Digest(contentaddr.Sum(rotatedBody))
	if err := cfg.Journal.Verify(
		ctx, cfg.AuthIdentityID, holder, lease.Fence, digest, expiresAt, verifiedAt,
	); err != nil {
		return fail(CodexAuthEnrollmentVerificationFailed, err)
	}
	if err := verifyCodexAuthRefreshLease(
		ctx, refreshCfg, lease, cfg.AuthIdentityID, holder,
	); err != nil {
		return CodexAuthEnrollmentResult{}, err
	}
	observedIntent, err := readCodexAuthRefreshIntent(storeRoot, storePath, cfg.AuthIdentityID)
	if err != nil || observedIntent.Intent.PendingDigest != string(digest) {
		return CodexAuthEnrollmentResult{}, errors.New(
			"verified Codex auth enrollment intent changed before removal",
		)
	}
	if err := mutate(func() error {
		return removeObservedCodexAuthRefreshIntent(
			storeRoot, storePath, cfg.AuthIdentityID, observedIntent,
		)
	}); err != nil {
		return CodexAuthEnrollmentResult{}, fmt.Errorf(
			"finish verified Codex auth enrollment transaction: %w", err,
		)
	}
	item, err := cfg.Journal.ProjectVerified(ctx, cfg.AuthIdentityID)
	if err != nil {
		return CodexAuthEnrollmentResult{}, fmt.Errorf("project verified Codex re-enrollment: %w", err)
	}
	binding := domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: cfg.AuthIdentityID, LeaseFence: lease.Fence,
		AuthStoreDigest: digest, AccessTokenExpiresAt: expiresAt,
	}
	if err := validateProjectedCodexAuthEnrollment(item, binding); err != nil {
		return CodexAuthEnrollmentResult{}, err
	}
	return CodexAuthEnrollmentResult{
		AuthIdentityID: cfg.AuthIdentityID, AuthStorePath: storePath,
		LeaseFence: lease.Fence, AuthStoreDigest: digest,
		AccessTokenExpiresAt: expiresAt,
		AttentionItemID:      item.ID, AttentionItemVersion: item.ItemVersion,
	}, nil
}

func recoverVerifiedCodexAuthEnrollment(
	ctx context.Context,
	cfg CodexAuthEnrollmentConfig,
	storeRoot, storePath string,
	binding domain.CodexReenrollmentRecoveryBinding,
) (CodexAuthEnrollmentResult, bool, error) {
	owner, err := newOwnershipLabel()
	if err != nil {
		return CodexAuthEnrollmentResult{}, false, errors.New("mint Codex auth recovery holder")
	}
	holder := domain.InvocationID("codex-auth-recovery-" + owner.Value)
	now := cfg.Now()
	lease, err := cfg.AuthStoreLeaser.Acquire(
		ctx, cfg.AuthIdentityID, holder, now, now.Add(cfg.LeaseDuration),
	)
	if err != nil {
		return CodexAuthEnrollmentResult{}, false,
			fmt.Errorf("acquire Codex auth recovery lease: %w", err)
	}
	release := func() error {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.TeardownTimeout)
		defer cancel()
		err := cfg.AuthStoreLeaser.Release(
			releaseCtx, cfg.AuthIdentityID, holder, lease.Fence, cfg.Now(),
		)
		if errors.Is(err, ErrLeaseWindowEnded) {
			return nil
		}
		return err
	}
	refreshCfg := CodexReviewConfig{
		InputRoot: storeRoot, AccessTokenLifetimeFloor: cfg.AccessTokenLifetimeFloor,
		AccessTokenRefreshThreshold: cfg.AccessTokenRefreshThreshold,
		AuthStoreLeaser:             cfg.AuthStoreLeaser, AuthRefresher: cfg.AuthRefresher, Now: cfg.Now,
	}
	if err := verifyCodexAuthRefreshLease(
		ctx, refreshCfg, lease, cfg.AuthIdentityID, holder,
	); err != nil {
		return CodexAuthEnrollmentResult{}, false, errors.Join(err, release())
	}
	mutate, err := codexAuthLeaseMutationGuard(ctx, refreshCfg, lease)
	if err != nil {
		return CodexAuthEnrollmentResult{}, false, errors.Join(err, release())
	}
	resolvedPath, body, metadata, err := readCodexReviewInputWithMetadata(
		storeRoot, storePath, maxCodexAuthSnapshotBytes,
	)
	if err != nil || resolvedPath != storePath ||
		domain.Digest(contentaddr.Sum(body)) != binding.AuthStoreDigest {
		return CodexAuthEnrollmentResult{}, false, release()
	}
	if _, err := recoverCodexAuthRefreshTransactionUnderLease(
		storeRoot, storePath, cfg.AuthIdentityID, body, metadata,
		cfg.AccessTokenRefreshThreshold, false, mutate,
	); err != nil {
		return CodexAuthEnrollmentResult{}, false, errors.Join(err, release())
	}
	_, body, _, err = readCodexReviewInputWithMetadata(
		storeRoot, storePath, maxCodexAuthSnapshotBytes,
	)
	if err != nil || domain.Digest(contentaddr.Sum(body)) != binding.AuthStoreDigest {
		return CodexAuthEnrollmentResult{}, false, release()
	}
	agentSnapshot, _, err := codexReviewAgentAuthSnapshot(CodexAuthSubscription, body)
	if err != nil {
		return CodexAuthEnrollmentResult{}, false, release()
	}
	expiresAt, err := inspectCodexAuthSnapshot(CodexAuthSubscription, agentSnapshot)
	if err != nil || expiresAt == nil || !expiresAt.Equal(binding.AccessTokenExpiresAt) {
		return CodexAuthEnrollmentResult{}, false, release()
	}
	if expiresAt.Sub(cfg.Now()) < cfg.AccessTokenLifetimeFloor {
		if err := release(); err != nil {
			return CodexAuthEnrollmentResult{}, false, err
		}
		return CodexAuthEnrollmentResult{}, false, errCodexAuthEnrollmentRetryLiveStore
	}
	if err := verifyCodexAuthRefreshLease(
		ctx, refreshCfg, lease, cfg.AuthIdentityID, holder,
	); err != nil {
		return CodexAuthEnrollmentResult{}, false, errors.Join(err, release())
	}
	item, err := cfg.Journal.ProjectVerified(ctx, cfg.AuthIdentityID)
	if err != nil {
		return CodexAuthEnrollmentResult{}, false,
			errors.Join(fmt.Errorf("project verified Codex re-enrollment: %w", err), release())
	}
	if err := validateProjectedCodexAuthEnrollment(item, binding); err != nil {
		return CodexAuthEnrollmentResult{}, false, errors.Join(err, release())
	}
	return CodexAuthEnrollmentResult{
		AuthIdentityID: cfg.AuthIdentityID, AuthStorePath: storePath,
		LeaseFence: binding.LeaseFence, AuthStoreDigest: binding.AuthStoreDigest,
		AccessTokenExpiresAt: binding.AccessTokenExpiresAt,
		AttentionItemID:      item.ID, AttentionItemVersion: item.ItemVersion,
	}, true, release()
}

func validateProjectedCodexAuthEnrollment(
	item domain.AttentionItem,
	binding domain.CodexReenrollmentRecoveryBinding,
) error {
	if err := item.Validate(); err != nil {
		return fmt.Errorf("projected Codex re-enrollment item is invalid: %w", err)
	}
	if item.Status != domain.StatusOpen ||
		!item.Offers(domain.ActionResolveReenrollment) ||
		item.CodexReenrollmentRecoveryBinding == nil ||
		*item.CodexReenrollmentRecoveryBinding != binding {
		return errors.New("projected Codex re-enrollment item does not match verified evidence")
	}
	return nil
}

func normalizeCodexAuthEnrollmentConfig(cfg *CodexAuthEnrollmentConfig) error {
	if cfg == nil || cfg.InputRoot == "" || cfg.InputFile == "" ||
		cfg.AuthStoreRoot == "" || cfg.AuthStorePath == "" ||
		cfg.AuthIdentityID == "" || cfg.ProjectID == "" || cfg.Journal == nil ||
		cfg.AuthStoreLeaser == nil || cfg.AuthRefresher == nil {
		return errors.New("codex auth enrollment configuration is incomplete")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = defaultCodexAuthEnrollmentLeaseDuration
	}
	if cfg.TeardownTimeout == 0 {
		cfg.TeardownTimeout = defaultCodexAuthEnrollmentTeardown
	}
	if cfg.AccessTokenLifetimeFloor == 0 {
		cfg.AccessTokenLifetimeFloor = defaultCodexAuthEnrollmentFloor
	}
	if cfg.AccessTokenRefreshThreshold == 0 {
		cfg.AccessTokenRefreshThreshold = defaultCodexAuthEnrollmentThreshold
	}
	if cfg.LeaseDuration <= 0 || cfg.TeardownTimeout <= 0 ||
		cfg.AccessTokenLifetimeFloor <= 0 ||
		cfg.AccessTokenRefreshThreshold < cfg.AccessTokenLifetimeFloor {
		return errors.New("codex auth enrollment durations are invalid")
	}
	if now := cfg.Now(); now.IsZero() || now.Location() != time.UTC {
		return errors.New("codex auth enrollment clock must return nonzero UTC instants")
	}
	return nil
}

func resolvePrivateCodexAuthRoot(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("root is not a private directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !codexReviewUIDMatches(stat, os.Geteuid()) {
		return "", errors.New("root is not owned by the effective user")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", errors.New("root cannot be resolved to a clean absolute path")
	}
	return resolved, nil
}

func resolveCodexAuthStoreTarget(root, target string) (string, error) {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target || !cliSafe(target) {
		return "", errors.New("auth-store path is not a clean absolute CLI-safe path")
	}
	parent, err := resolvePrivateCodexAuthRoot(filepath.Dir(target))
	if err != nil {
		return "", fmt.Errorf("auth-store parent: %w", err)
	}
	if !pathInsideRoot(root, parent) {
		return "", errors.New("auth-store path resolves outside its private root")
	}
	return filepath.Join(parent, filepath.Base(target)), nil
}

func privateRootsOverlap(a, b string) bool {
	return pathInsideRoot(a, b) || pathInsideRoot(b, a)
}

func pathInsideRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
