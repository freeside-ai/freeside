package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// Production driver composition (#237): the daemon's -driver=claude mode
// wires the real ward gate, its durable journal and lease adapters, the
// Claude stage driver, and the admission gate the engine records every
// dispatch under. The fake-driver mode is untouched, so the 1A.0 walking
// skeleton keeps its exact behaviour.

// claudeDriverConfig is the operator-supplied half of the production
// composition. Everything here is deliberately explicit: a defaulted image,
// endpoint, or identity would put an unaudited value into the durable
// admission record.
type claudeDriverConfig struct {
	AgentImage        domain.ImageRef
	ExporterImage     string
	ContainerBin      string
	SeedRoot          string
	StateDir          string
	RigTokenFile      string
	ProviderEndpoints []string
	// The prompt-package files are trusted implementation, elaboration, and
	// remediation inputs. The daemon derives every digest from ingested bytes.
	PromptPackageFile            string
	ElaborationPromptPackageFile string
	RemediationPromptPackageFile string
	VendorInstructions           string
	Repo                         string
	RepositoryID                 int64
	BaseRef                      string
	BaseSHA                      string
	AuthIdentityID               domain.AuthIdentityID
	AllowedPaths                 []string
	// RunConformance executes the store-backed full ward suite against this
	// exact runtime/image/configuration before the engine can admit work.
	RunConformance bool
	// StateRoot and CredentialsDir locate the GitHub App authority and
	// credential material the exact-base transport authenticates with.
	StateRoot                   string
	CredentialsDir              string
	OperatingMode               domain.OperatingMode
	ReviewImage                 string
	ReviewInputRoot             string
	ReviewAuthMode              ward.CodexAuthMode
	ReviewAuthIdentityID        domain.AuthIdentityID
	ReviewAuthSnapshot          string
	ReviewInstructions          string
	ReviewModel                 string
	ReviewReasoningEffort       string
	ReviewCostOwner             string
	ReviewWorkspaceSizeMB       int64
	ShadowReviewImage           string
	ShadowReviewAuthSnapshot    string
	ShadowReviewModel           string
	ShadowReviewReasoningEffort string
	ShadowReviewCostOwner       string
	ShadowReviewWorkspaceSizeMB int64
	ShadowReviewRate            float64
}

var errBackendConformanceUnavailable = errors.New("exact passing backend conformance proof is unavailable")

func (c claudeDriverConfig) validate() error {
	switch {
	case c.AgentImage == "":
		return fmt.Errorf("-agent-image is required in claude driver mode")
	case c.ExporterImage == "":
		return fmt.Errorf("-exporter-image is required in claude driver mode")
	case c.SeedRoot == "":
		return fmt.Errorf("-seed-root is required in claude driver mode")
	case len(c.ProviderEndpoints) == 0:
		return fmt.Errorf("-provider-endpoints is required in claude driver mode")
	case c.PromptPackageFile == "":
		return fmt.Errorf("-prompt-package is required in claude driver mode")
	case c.ElaborationPromptPackageFile == "":
		return fmt.Errorf("-elaboration-prompt-package is required in claude driver mode")
	case c.RemediationPromptPackageFile == "":
		return fmt.Errorf("-remediation-prompt-package is required in claude driver mode")
	case c.VendorInstructions == "":
		return fmt.Errorf("-vendor-instructions is required in claude driver mode")
	case c.Repo == "" || c.RepositoryID <= 0 || c.BaseRef == "" || c.BaseSHA == "":
		return fmt.Errorf("-repo, -repository-id, -base-ref, and -base-sha are required in claude driver mode")
	case publish.ValidateRepository(c.Repo) != nil:
		return fmt.Errorf("-repo is not a valid transport repository")
	case strings.HasPrefix(c.BaseRef, "refs/") || publish.ValidateBranchName(c.BaseRef) != nil:
		return fmt.Errorf("-base-ref is not a valid transport branch")
	case publish.ValidateCommitSHA(c.BaseSHA) != nil:
		return fmt.Errorf("-base-sha must be a full lowercase commit SHA")
	case c.AuthIdentityID == "":
		return fmt.Errorf("-auth-identity is required in claude driver mode")
	case !explicitAllowedPaths(c.AllowedPaths):
		// The importer's declared-path scope is a containment control (§5.6,
		// §5.8), so unattended work states it explicitly; inheriting a
		// match-everything default would let an agent rewrite any path in the
		// managed repository under a policy nobody wrote.
		return fmt.Errorf("-allowed-paths must name explicit declared paths in claude driver mode")
	case c.StateDir == "":
		return fmt.Errorf("-state-dir is required in claude driver mode")
	case c.RigTokenFile != "" &&
		(!filepath.IsAbs(c.RigTokenFile) || filepath.Clean(c.RigTokenFile) != c.RigTokenFile):
		return fmt.Errorf("-rig-token-file must be a clean absolute path")
	case c.StateRoot == "" || c.CredentialsDir == "":
		return fmt.Errorf("-publication-state-dir and -publication-credentials-dir are required in claude driver mode")
	case c.OperatingMode == domain.ModeUnattended &&
		(c.ReviewImage == "" || c.ReviewInputRoot == "" || c.ReviewAuthIdentityID == "" ||
			c.ReviewAuthSnapshot == "" || c.ReviewInstructions == "" || c.ReviewModel == "" ||
			c.ReviewReasoningEffort == "" || c.ReviewCostOwner == "" || c.ReviewWorkspaceSizeMB <= 0):
		return fmt.Errorf("codex review configuration is required in claude driver mode")
	case c.OperatingMode == domain.ModeUnattended && domain.ImageRef(c.ReviewImage).Validate() != nil:
		return fmt.Errorf("-review-image must be digest-pinned")
	case c.OperatingMode == domain.ModeUnattended &&
		(!filepath.IsAbs(c.ReviewInputRoot) || filepath.Clean(c.ReviewInputRoot) != c.ReviewInputRoot):
		return fmt.Errorf("-review-input-root must be a clean absolute path")
	case c.OperatingMode == domain.ModeUnattended &&
		(strings.IndexByte(c.ReviewModel, 0) >= 0 || strings.IndexByte(c.ReviewReasoningEffort, 0) >= 0):
		return fmt.Errorf("codex review model configuration must be NUL-free")
	case c.OperatingMode == domain.ModeUnattended &&
		c.ReviewAuthMode != ward.CodexAuthSubscription && c.ReviewAuthMode != ward.CodexAuthAPIKey:
		return fmt.Errorf("-review-auth-mode is invalid")
	case c.ShadowReviewImage != "" && c.OperatingMode != domain.ModeUnattended:
		return fmt.Errorf("shadow review requires unattended mode")
	case c.ShadowReviewImage == "" &&
		(c.ShadowReviewAuthSnapshot != "" || c.ShadowReviewModel != "" ||
			c.ShadowReviewReasoningEffort != "" || c.ShadowReviewCostOwner != ""):
		return fmt.Errorf("-shadow-review-image is required when shadow review fields are set")
	case c.ShadowReviewImage != "" &&
		(c.ShadowReviewAuthSnapshot == "" || c.ShadowReviewModel == "" ||
			c.ShadowReviewReasoningEffort == "" || c.ShadowReviewCostOwner == "" ||
			c.ShadowReviewWorkspaceSizeMB <= 0 || math.IsNaN(c.ShadowReviewRate) ||
			math.IsInf(c.ShadowReviewRate, 0) || c.ShadowReviewRate < 0 || c.ShadowReviewRate > 1):
		return fmt.Errorf("complete shadow review configuration is required when enabled")
	case c.ShadowReviewImage != "" && domain.ImageRef(c.ShadowReviewImage).Validate() != nil:
		return fmt.Errorf("-shadow-review-image must be digest-pinned")
	case c.ShadowReviewImage != "" &&
		(strings.IndexByte(c.ShadowReviewModel, 0) >= 0 ||
			strings.IndexByte(c.ShadowReviewReasoningEffort, 0) >= 0):
		return fmt.Errorf("shadow review model configuration must be NUL-free")
	}
	return nil
}

// explicitAllowedPaths requires every pattern to name a literal top-level
// repository path. A leading glob segment (for example **/*, */**, or ?*)
// can be semantically match-all even when it is not the literal "**"; such a
// pattern does not declare a containment boundary.
func explicitAllowedPaths(patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	if err := importer.ValidatePathPatterns(patterns); err != nil {
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

// admissionCapabilitySnapshot is the selected mode's engine floor as a
// durable snapshot, for the store policy that re-gates every admission.
func admissionCapabilitySnapshot(mode domain.OperatingMode) domain.CapabilitySnapshot {
	return exec.NewCapabilitySet(admissionFloor(mode)...).Snapshot()
}

func admissionFloor(mode domain.OperatingMode) []exec.Capability {
	switch mode {
	case domain.ModeAttendedDev:
		return attendedAdmissionFloor
	case domain.ModeUnattended:
		return unattendedAdmissionFloor
	}
	return nil
}

// ingestPromptPackage stores the prompt package's bytes and returns their
// content address, so the digest the admission records and the bytes the
// materializer resolves are the same object by construction.
func ingestPromptPackage(blobs *signet.BlobStore, path string) (domain.Digest, []byte, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: operator-configured control-plane prompt package
	if err != nil {
		return "", nil, fmt.Errorf("read prompt package: %w", err)
	}
	if len(body) == 0 {
		return "", nil, fmt.Errorf("prompt package %s is empty", path)
	}
	sum := sha256.Sum256(body)
	digest := domain.Digest(contentaddr.Format(sum[:]))
	if _, err := blobs.Put(digest, bytes.NewReader(body)); err != nil {
		return "", nil, fmt.Errorf("store prompt package: %w", err)
	}
	return digest, body, nil
}

// attendedAdmissionFloor is ward's honest base capability class before the
// full conformance suite earns the two unattended-only proofs.
var attendedAdmissionFloor = []exec.Capability{
	exec.CapDetachableWorkspace,
	exec.CapPostExitExport,
	exec.CapReadOnlyRemount,
}

// unattendedAdmissionFloor is the §5.7 minimum for unattended dispatch: the
// attended handoff floor plus the two capabilities the full conformance suite
// earns. It mirrors ward's own
// unattended floor; the store re-derives the same plan-mandated minimum on
// every admission, so a drift here fails closed rather than admitting less.
var unattendedAdmissionFloor = []exec.Capability{
	exec.CapDetachableWorkspace,
	exec.CapPostExitExport,
	exec.CapReadOnlyRemount,
	exec.CapNetworklessExport,
	exec.CapEnforcedProviderEgress,
}

// exportRecorder adapts the store's internal-transaction export seam to the
// driver's port (the wardstore adapter precedent: the driver holds no store
// handle, and RecordExecutionExport is not client-visible state).
type exportRecorder struct {
	store *store.Store
}

func (r exportRecorder) RecordExecutionExport(
	ctx context.Context,
	record domain.ExecutionExport,
	replay claude.ExecutionReplay,
) error {
	err := engine.RecordExecutionExport(ctx, r.store, record, engine.ProductionReplay{
		InvocationID: replay.InvocationID, ObservedBaseSHA: replay.ObservedBaseSHA,
		HeadSHA: replay.HeadSHA, Manifest: replay.Manifest,
		ManifestDigest: replay.ManifestDigest, Evidence: replay.Evidence,
		EvidenceManifestDigest: replay.EvidenceManifestDigest,
		CommitPlanDigest:       replay.CommitPlanDigest,
		ImportOptions:          replay.ImportOptions,
	})
	if errors.Is(err, store.ErrImmutableConflict) {
		return errors.Join(err, domain.ErrImmutableTransition)
	}
	return err
}

// storeAdmissionAuthority binds private driver state back to the durable
// admission and the trust profile its mode requires at import. Unattended
// execution uses its exact admission-bound revision; attended_dev resolves the
// currently active profile. The state file is replay data, not an
// authorization source.
type storeAdmissionAuthority struct {
	store                     *store.Store
	blobs                     *signet.BlobStore
	allowedPaths              []string
	commitAuthors             productionCommitAuthorResolver
	authenticatedStartAuthors *productionCommitAuthorAuthenticationCache
}

type productionCommitAuthorResolver interface {
	Resolve(context.Context, string) (publish.AppBotIdentity, error)
	Revalidate(context.Context, string) (publish.AppBotIdentity, error)
}

const maxProductionCommitAuthorAuthentications = 1024

type productionCommitAuthorAuthenticationBinding struct {
	repo      string
	appSlug   string
	botUserID int64
}

type productionCommitAuthorAuthenticationCache struct {
	mu      sync.Mutex
	entries map[domain.InvocationID]productionCommitAuthorAuthenticationBinding
}

func newProductionCommitAuthorAuthenticationCache() *productionCommitAuthorAuthenticationCache {
	return &productionCommitAuthorAuthenticationCache{
		entries: map[domain.InvocationID]productionCommitAuthorAuthenticationBinding{},
	}
}

func (c *productionCommitAuthorAuthenticationCache) authenticate(
	id domain.InvocationID,
	binding productionCommitAuthorAuthenticationBinding,
	authenticate func() error,
) error {
	if c == nil {
		return authenticate()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.entries[id]; ok && cached == binding {
		return nil
	}
	if err := authenticate(); err != nil {
		return err
	}
	if _, exists := c.entries[id]; !exists && len(c.entries) >= maxProductionCommitAuthorAuthentications {
		clear(c.entries)
	}
	c.entries[id] = binding
	return nil
}

func (c *productionCommitAuthorAuthenticationCache) forget(id domain.InvocationID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}

func (a storeAdmissionAuthority) admission(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec, requireCurrent bool,
) (domain.ExecutionAdmission, []string, error) {
	var admission domain.ExecutionAdmission
	var allowedPaths []string
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if requireCurrent {
			admission, err = tx.GetExecutionAdmission(ctx, id)
		} else {
			admission, err = tx.GetExecutionAdmissionRecord(ctx, id)
		}
		if err != nil {
			return err
		}
		if requireCurrent {
			// Existing attempts bypass fresh admission on replay. Re-run the
			// recording-time conformance check here so an intent admitted under
			// backend configuration A cannot start or recover after this daemon
			// has restarted under a current proof for B. The authenticate gate
			// tolerates a same-configuration recheck in progress (a supersession
			// marker for A) so this daemon's own startup re-proof cannot make an
			// in-flight admission permanently unauthenticatable (issue #761); a
			// proof for a different configuration B still refuses.
			if err := tx.AuthenticateBackendConformant(ctx, admission); err != nil {
				return err
			}
		}
		policy, err := tx.GetResolvedPolicy(ctx, admission.RunID)
		if err != nil {
			return err
		}
		if requireCurrent {
			allowedPaths, err = resolvedPathAllowlist(
				policy, admission.PolicyDigest, a.allowedPaths,
			)
		} else {
			allowedPaths, err = recordedPathAllowlist(policy, admission.PolicyDigest)
		}
		return err
	})
	if err != nil {
		return domain.ExecutionAdmission{}, nil, err
	}
	if !reflect.DeepEqual(exec.StartSpecFromAdmission(admission), spec) {
		return domain.ExecutionAdmission{}, nil, fmt.Errorf(
			"start spec disagrees with admission %s: %w",
			admission.ID, domain.ErrParentKeyMismatch)
	}
	return admission, allowedPaths, nil
}

// submittedPathBoundary refuses a policy that names no enforceable declared
// paths. It is the submission-time half of resolvedPathAllowlist: that gate
// compares the policy against one daemon's configuration and so must hold
// rather than fail, which leaves a policy carrying no usable boundary at all
// held forever. Refusing it at submission keeps that state out of the store.
func submittedPathBoundary(policy domain.ResolvedPolicy) error {
	for _, key := range policy.Keys {
		if key.Key != "paths" {
			continue
		}
		if !explicitAllowedPaths(splitNonEmpty(key.Value)) {
			return fmt.Errorf(
				"resolved policy paths %q are not an explicit declared-path allowlist: %w",
				key.Value, domain.ErrPathBoundaryMismatch,
			)
		}
		return nil
	}
	return fmt.Errorf(
		"resolved policy declares no paths key: %w", domain.ErrPathBoundaryMismatch,
	)
}

// resolvedPathAllowlist binds the submitted, digest-addressed policy to the
// manually configured Phase 1A.2 containment boundary. Until per-run ward
// configuration exists, accepting a different path set would make the
// durable run policy an audit label rather than the policy import enforces.
func resolvedPathAllowlist(
	policy domain.ResolvedPolicy, admittedDigest domain.Digest, configured []string,
) ([]string, error) {
	// A digest disagreement is record corruption and stays fatal. Every other
	// verdict here compares the durable policy against this daemon's current
	// -allowed-paths, so it is a configuration verdict: it must fail closed
	// without starting the writer, and it must not end the reconcile loop,
	// because an operator reconfiguring the boundary resolves it without
	// touching the recorded attempt (engine.MutableAdmissionPolicyRefusal).
	if policy.Digest != admittedDigest {
		return nil, fmt.Errorf(
			"resolved policy digest %q disagrees with admitted %q: %w",
			policy.Digest, admittedDigest, domain.ErrParentKeyMismatch,
		)
	}
	for _, key := range policy.Keys {
		if key.Key != "paths" {
			continue
		}
		paths := splitNonEmpty(key.Value)
		if !explicitAllowedPaths(paths) {
			return nil, fmt.Errorf(
				"resolved paths policy is not an explicit allowlist: %w",
				domain.ErrPathBoundaryMismatch,
			)
		}
		if !slices.Equal(paths, configured) {
			return nil, fmt.Errorf(
				"resolved paths %q disagree with configured %q: %w",
				paths, configured, domain.ErrPathBoundaryMismatch,
			)
		}
		return slices.Clone(paths), nil
	}
	return nil, fmt.Errorf(
		"resolved policy has no paths key: %w", domain.ErrPathBoundaryMismatch,
	)
}

func recordedPathAllowlist(
	policy domain.ResolvedPolicy,
	admittedDigest domain.Digest,
) ([]string, error) {
	if policy.Digest != admittedDigest {
		return nil, fmt.Errorf(
			"resolved policy digest %q disagrees with admitted %q: %w",
			policy.Digest, admittedDigest, domain.ErrParentKeyMismatch,
		)
	}
	for _, key := range policy.Keys {
		if key.Key != "paths" {
			continue
		}
		paths := splitNonEmpty(key.Value)
		if !explicitAllowedPaths(paths) {
			return nil, fmt.Errorf(
				"recorded resolved paths are not an explicit allowlist: %w",
				domain.ErrPathBoundaryMismatch,
			)
		}
		return slices.Clone(paths), nil
	}
	return nil, fmt.Errorf(
		"resolved policy has no paths key: %w", domain.ErrPathBoundaryMismatch,
	)
}

func (a storeAdmissionAuthority) AuthenticateAdmission(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	_, _, err := a.admission(ctx, id, spec, false)
	return err
}

func (a storeAdmissionAuthority) AuthenticateStart(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	admission, _, err := a.admission(ctx, id, spec, true)
	if err != nil {
		return err
	}
	return a.authenticateInvocationStart(ctx, id, admission, spec.Base.Repo)
}

func (a storeAdmissionAuthority) authenticateInvocationStart(
	ctx context.Context, id domain.InvocationID, admission domain.ExecutionAdmission, repo string,
) error {
	elaboration, err := a.authenticateElaborationInvocation(ctx, id, admission)
	if err != nil || elaboration {
		return err
	}
	_, _, err = a.authenticateInvocationCommitAuthorForStart(
		ctx, id, admission, repo,
	)
	return err
}

func (a storeAdmissionAuthority) authenticateElaborationInvocation(
	ctx context.Context, id domain.InvocationID, admission domain.ExecutionAdmission,
) (bool, error) {
	elaboration := false
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(ctx, string(id))
		if err != nil {
			return err
		}
		if entry.Kind != engine.KindElaborationInvocationRequested &&
			entry.Kind != engine.KindElaborationDiscussionRequested {
			return nil
		}
		elaboration = true
		if entry.Kind == engine.KindElaborationDiscussionRequested {
			return engine.AuthenticateElaborationDiscussionTransition(
				ctx, tx, entry, admission.RunID, admission.StageID,
			)
		}
		return engine.AuthenticateElaborationInvocationTransition(
			ctx, tx, entry, admission.RunID, admission.StageID,
		)
	})
	if errors.Is(err, store.ErrNotFound) {
		if elaboration || engine.IsElaborationInvocationIdentity(
			id, admission.RunID, admission.StageID,
		) {
			return true, fmt.Errorf("authenticate elaboration invocation marker: %w", err)
		}
		return false, nil
	}
	if err != nil {
		return elaboration, fmt.Errorf("authenticate elaboration invocation marker: %w", err)
	}
	return elaboration, nil
}

func (a storeAdmissionAuthority) ImportOptions(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec, opts importer.Options,
) (importer.Options, error) {
	admission, allowedPaths, err := a.admission(ctx, id, spec, true)
	if err != nil {
		return importer.Options{}, err
	}
	profile, err := a.importTrustProfile(ctx, admission)
	if err != nil {
		return importer.Options{}, err
	}
	if profile.Repo != spec.Base.Repo || profile.RepositoryID != spec.Base.RepositoryID {
		return importer.Options{}, fmt.Errorf(
			"trust profile repository disagrees with admitted base: %w",
			domain.ErrParentKeyMismatch)
	}
	opts.Policy, err = opts.Policy.WithProtectedPaths(profile)
	if err != nil {
		return importer.Options{}, err
	}
	opts.Policy.Allowlist = allowedPaths
	opts.CommitMessage, err = a.fallbackCommitMessage(ctx, admission, opts.Policy)
	if err != nil {
		return importer.Options{}, err
	}
	// An elaboration run imports under the specification finding profile: it
	// publishes a typed JSON result, never workspace content, so incidental
	// investigation debris must not definitively fail the invocation (#768).
	if err := a.applyElaborationFindingProfile(ctx, id, admission, &opts); err != nil {
		return importer.Options{}, err
	}
	author, production, err := a.invocationImportAuthor(ctx, id, admission, spec.Base.Repo)
	if err != nil {
		return importer.Options{}, err
	}
	if production {
		opts.AuthorName, opts.AuthorEmail = author.Name(), author.Email()
	}
	a.authenticatedStartAuthors.forget(id)
	return opts, nil
}

// applyElaborationFindingProfile sets the specification finding profile when
// the invocation is an authenticated elaboration one, so both the live import
// and its terminal replay reconstruct the same profile from the same durable
// marker (the omitempty field keeps a non-elaboration policy byte-identical).
func (a storeAdmissionAuthority) applyElaborationFindingProfile(
	ctx context.Context, id domain.InvocationID, admission domain.ExecutionAdmission, opts *importer.Options,
) error {
	elaboration, err := a.authenticateElaborationInvocation(ctx, id, admission)
	if err != nil {
		return err
	}
	if elaboration {
		profile := importer.FindingProfileSpecification
		opts.Policy.FindingProfile = &profile
	}
	return nil
}

func (a storeAdmissionAuthority) ImportOptionsRecord(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec, opts importer.Options,
) (importer.Options, error) {
	admission, allowedPaths, err := a.admission(ctx, id, spec, false)
	if err != nil {
		return importer.Options{}, err
	}
	profile, err := a.importTrustProfile(ctx, admission)
	if err != nil {
		return importer.Options{}, err
	}
	if profile.Repo != spec.Base.Repo || profile.RepositoryID != spec.Base.RepositoryID {
		return importer.Options{}, fmt.Errorf(
			"trust profile repository disagrees with admitted base: %w",
			domain.ErrParentKeyMismatch)
	}
	opts.Policy, err = opts.Policy.WithProtectedPaths(profile)
	if err != nil {
		return importer.Options{}, err
	}
	opts.Policy.Allowlist = allowedPaths
	opts.CommitMessage, err = a.fallbackCommitMessage(ctx, admission, opts.Policy)
	if err != nil {
		return importer.Options{}, err
	}
	// Reconstruct the same specification profile the live import used, from the
	// same durable elaboration marker, so the replayed ImportOptions match.
	if err := a.applyElaborationFindingProfile(ctx, id, admission, &opts); err != nil {
		return importer.Options{}, err
	}
	// This reconstructs an already-completed import from immutable records.
	// App attribution was authenticated before the actual import; requiring
	// live GitHub authority here would strand terminal replay after a restart.
	author, production, err := a.invocationImportRecordAuthor(ctx, id, admission)
	if err != nil {
		return importer.Options{}, err
	}
	if production {
		opts.AuthorName, opts.AuthorEmail = author.Name(), author.Email()
	}
	return opts, nil
}

func (a storeAdmissionAuthority) invocationImportAuthor(
	ctx context.Context, id domain.InvocationID, admission domain.ExecutionAdmission, repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	elaboration, err := a.authenticateElaborationInvocation(ctx, id, admission)
	if err != nil || elaboration {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return a.authenticateInvocationCommitAuthorRevalidated(
		ctx, id, admission, repo,
	)
}

func (a storeAdmissionAuthority) invocationImportRecordAuthor(
	ctx context.Context, id domain.InvocationID, admission domain.ExecutionAdmission,
) (engine.ProductionCommitAuthor, bool, error) {
	elaboration, err := a.authenticateElaborationInvocation(ctx, id, admission)
	if err != nil || elaboration {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return a.invocationCommitAuthor(ctx, id, admission)
}

func (a storeAdmissionAuthority) fallbackCommitMessage(
	ctx context.Context,
	admission domain.ExecutionAdmission,
	policy importer.Policy,
) (string, error) {
	if a.blobs == nil {
		return "", errors.New("derive fallback commit message: nil blob store")
	}
	var (
		run        domain.Run
		boundIssue *int
	)
	if err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, admission.RunID)
		if err != nil {
			return err
		}
		declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, admission.RunID)
		switch {
		case err == nil:
			boundIssue = declaration.BoundIssue
			return nil
		case errors.Is(err, store.ErrNotFound):
			return nil
		default:
			return err
		}
	}); err != nil {
		return "", fmt.Errorf("derive fallback commit message authority: %w", err)
	}
	if run.SpecDigest != admission.SpecDigest {
		return "", fmt.Errorf(
			"run specification digest %q disagrees with admission %q: %w",
			run.SpecDigest, admission.SpecDigest, domain.ErrParentKeyMismatch,
		)
	}
	spec, err := readVerifiedBlob(ctx, a.blobs, run.SpecDigest)
	if err != nil {
		return "", fmt.Errorf("read approved specification: %w", err)
	}
	return engine.FallbackCommitMessage(engine.FallbackCommitMessageInput{
		Spec: spec, BoundIssue: boundIssue, RunID: run.ID,
		SpecDigest: run.SpecDigest, Policy: policy,
	}), nil
}

func readVerifiedBlob(
	ctx context.Context,
	blobs *signet.BlobStore,
	digest domain.Digest,
) ([]byte, error) {
	body, err := blobs.OpenContext(ctx, digest)
	if err != nil {
		return nil, err
	}
	hasher := sha256.New()
	var content bytes.Buffer
	_, copyErr := io.Copy(io.MultiWriter(&content, hasher), body)
	closeErr := body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return nil, err
	}
	got := domain.Digest(contentaddr.Format(hasher.Sum(nil)))
	if got != digest {
		return nil, fmt.Errorf("body hashes to %s, want %s", got, digest)
	}
	return content.Bytes(), nil
}

func (a storeAdmissionAuthority) productionCommitAuthor(
	ctx context.Context,
	id domain.InvocationID,
	mode domain.OperatingMode,
) (engine.ProductionCommitAuthor, bool, error) {
	var entry store.QueueEntry
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(id))
		return err
	})
	if mode != domain.ModeUnattended && errors.Is(err, store.ErrNotFound) {
		return engine.ProductionCommitAuthor{}, false, nil
	}
	if err != nil {
		return engine.ProductionCommitAuthor{}, false, fmt.Errorf("load production commit author: %w", err)
	}
	if entry.Kind != engine.KindProductionInvocationRequested {
		if mode != domain.ModeUnattended {
			return engine.ProductionCommitAuthor{}, false, nil
		}
		return engine.ProductionCommitAuthor{}, false, fmt.Errorf(
			"unattended invocation %q has no production ownership marker: %w",
			id, domain.ErrParentKeyMismatch,
		)
	}
	publication, present, err := engine.ProductionInvocationPublication(entry)
	if err != nil {
		return engine.ProductionCommitAuthor{}, false, fmt.Errorf("authenticate production commit author: %w", err)
	}
	if !present {
		return engine.ProductionCommitAuthor{}, false, nil
	}
	return publication.CommitAuthor, true, nil
}

// invocationCommitAuthor authenticates the durable marker for either an
// initial production invocation or a remediation invocation. Remediation
// inherits only the original production request's commit-author claim after
// the complete review/adjudication transition is reconstructed in this same
// store snapshot.
func (a storeAdmissionAuthority) invocationCommitAuthor(
	ctx context.Context,
	id domain.InvocationID,
	admission domain.ExecutionAdmission,
) (engine.ProductionCommitAuthor, bool, error) {
	var (
		entry       store.QueueEntry
		publication engine.ProductionPublication
		remediation bool
		feedback    bool
	)
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(id))
		if err != nil {
			return err
		}
		if entry.Kind == engine.KindOperatorFeedbackInvocationRequested {
			feedback = true
			publication, err = engine.AuthenticateOperatorFeedbackInvocationTransition(
				ctx, tx, entry, admission.RunID, admission.StageID,
			)
			return err
		}
		if entry.Kind != engine.KindRemediationInvocationRequested {
			return nil
		}
		remediation = true
		publication, err = engine.AuthenticateRemediationInvocationTransition(
			ctx, tx, entry, admission.RunID, admission.StageID,
		)
		return err
	})
	if feedback {
		if err != nil {
			return engine.ProductionCommitAuthor{}, false,
				fmt.Errorf("authenticate operator-feedback commit author: %w", err)
		}
		return publication.CommitAuthor, true, nil
	}
	if remediation {
		if err != nil {
			return engine.ProductionCommitAuthor{}, false,
				fmt.Errorf("authenticate remediation commit author: %w", err)
		}
		return publication.CommitAuthor, true, nil
	}
	if err != nil && (admission.OperatingMode == domain.ModeUnattended ||
		!errors.Is(err, store.ErrNotFound)) {
		return engine.ProductionCommitAuthor{}, false, fmt.Errorf(
			"load invocation commit author: %w", err)
	}
	return a.productionCommitAuthor(ctx, id, admission.OperatingMode)
}

func (a storeAdmissionAuthority) authenticateInvocationCommitAuthorForStart(
	ctx context.Context,
	id domain.InvocationID,
	admission domain.ExecutionAdmission,
	repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	claimed, production, err := a.invocationCommitAuthor(ctx, id, admission)
	if err != nil || !production {
		return claimed, production, err
	}
	binding := productionCommitAuthorAuthenticationBinding{
		repo: repo, appSlug: claimed.AppSlug, botUserID: claimed.BotUserID,
	}
	err = a.authenticatedStartAuthors.authenticate(id, binding, func() error {
		return a.authenticateProductionCommitAuthorClaim(ctx, repo, claimed, false)
	})
	if err != nil {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return claimed, true, nil
}

func (a storeAdmissionAuthority) authenticateInvocationCommitAuthorRevalidated(
	ctx context.Context,
	id domain.InvocationID,
	admission domain.ExecutionAdmission,
	repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	claimed, production, err := a.invocationCommitAuthor(ctx, id, admission)
	if err != nil || !production {
		return claimed, production, err
	}
	if err := a.authenticateProductionCommitAuthorClaim(ctx, repo, claimed, true); err != nil {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return claimed, true, nil
}

func (a storeAdmissionAuthority) authenticateProductionCommitAuthor(
	ctx context.Context,
	id domain.InvocationID,
	mode domain.OperatingMode,
	repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	return a.authenticateProductionCommitAuthorWith(
		ctx, id, mode, repo, false,
	)
}

func (a storeAdmissionAuthority) authenticateProductionCommitAuthorForStart(
	ctx context.Context,
	id domain.InvocationID,
	mode domain.OperatingMode,
	repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	claimed, production, err := a.productionCommitAuthor(ctx, id, mode)
	if err != nil || !production {
		return claimed, production, err
	}
	binding := productionCommitAuthorAuthenticationBinding{
		repo: repo, appSlug: claimed.AppSlug, botUserID: claimed.BotUserID,
	}
	err = a.authenticatedStartAuthors.authenticate(id, binding, func() error {
		return a.authenticateProductionCommitAuthorClaim(ctx, repo, claimed, false)
	})
	if err != nil {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return claimed, true, nil
}

func (a storeAdmissionAuthority) authenticateProductionCommitAuthorRevalidated(
	ctx context.Context,
	id domain.InvocationID,
	mode domain.OperatingMode,
	repo string,
) (engine.ProductionCommitAuthor, bool, error) {
	return a.authenticateProductionCommitAuthorWith(
		ctx, id, mode, repo, true,
	)
}

func (a storeAdmissionAuthority) authenticateProductionCommitAuthorWith(
	ctx context.Context,
	id domain.InvocationID,
	mode domain.OperatingMode,
	repo string,
	revalidate bool,
) (engine.ProductionCommitAuthor, bool, error) {
	claimed, production, err := a.productionCommitAuthor(ctx, id, mode)
	if err != nil || !production {
		return claimed, production, err
	}
	if err := a.authenticateProductionCommitAuthorClaim(ctx, repo, claimed, revalidate); err != nil {
		return engine.ProductionCommitAuthor{}, false, err
	}
	return claimed, true, nil
}

func (a storeAdmissionAuthority) authenticateProductionCommitAuthorClaim(
	ctx context.Context,
	repo string,
	claimed engine.ProductionCommitAuthor,
	revalidate bool,
) error {
	if a.commitAuthors == nil {
		return fmt.Errorf(
			"authenticate production commit author: no App identity resolver: %w",
			publish.ErrAppBotIdentityMismatch,
		)
	}
	var resolved publish.AppBotIdentity
	var err error
	if revalidate {
		resolved, err = a.commitAuthors.Revalidate(ctx, repo)
	} else {
		resolved, err = a.commitAuthors.Resolve(ctx, repo)
	}
	if err != nil {
		return fmt.Errorf(
			"authenticate production commit author: %w", err,
		)
	}
	if claimed.AppSlug != resolved.AppSlug || claimed.BotUserID != resolved.BotUserID {
		return fmt.Errorf(
			"authenticate production commit author: durable attribution disagrees with selected App: %w",
			publish.ErrAppBotIdentityMismatch,
		)
	}
	return nil
}

func (a storeAdmissionAuthority) importTrustProfile(
	ctx context.Context,
	admission domain.ExecutionAdmission,
) (domain.AutomationTrustProfile, error) {
	var profile domain.AutomationTrustProfile
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		if admission.TrustProfileDigest != nil {
			var err error
			profile, err = tx.GetTrustProfile(ctx, *admission.TrustProfileDigest)
			return err
		}
		if admission.OperatingMode != domain.ModeAttendedDev {
			return fmt.Errorf(
				"admission %s under mode %q carries no trust profile: %w",
				admission.ID, admission.OperatingMode, domain.ErrEmptyField,
			)
		}
		var err error
		profile, err = tx.LatestTrustProfile(ctx, admission.Base.Repo)
		return err
	})
	return profile, err
}

func (r exportRecorder) LookupExecutionExport(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionExport, bool, error) {
	return r.lookupExecutionExport(ctx, id, true)
}

func (r exportRecorder) LookupExecutionExportRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionExport, bool, error) {
	return r.lookupExecutionExport(ctx, id, false)
}

func (r exportRecorder) RecordCurrentImportStart(
	ctx context.Context, start domain.CurrentImportStart,
) error {
	err := r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordCurrentImportStart(ctx, start)
	})
	if errors.Is(err, store.ErrImmutableConflict) {
		return errors.Join(err, domain.ErrImmutableTransition)
	}
	return err
}

func (r exportRecorder) LookupCurrentImportStart(
	ctx context.Context, id domain.InvocationID,
) (domain.CurrentImportStart, bool, error) {
	var start domain.CurrentImportStart
	err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		start, err = tx.GetCurrentImportStart(ctx, id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.CurrentImportStart{}, false, nil
	}
	return start, err == nil, err
}

func (r exportRecorder) lookupExecutionExport(
	ctx context.Context, id domain.InvocationID, requireCurrent bool,
) (domain.ExecutionExport, bool, error) {
	var record domain.ExecutionExport
	err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if requireCurrent {
			record, err = tx.GetExecutionExport(ctx, id)
		} else {
			record, err = tx.GetExecutionExportRecord(ctx, id)
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.ExecutionExport{}, false, nil
	}
	return record, err == nil, err
}

func (r exportRecorder) RecordExecutionOutcome(
	ctx context.Context, record domain.ExecutionOutcome,
) error {
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExecutionOutcome(ctx, record)
	})
}

func (r exportRecorder) LookupExecutionOutcome(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionOutcome, bool, error) {
	return r.lookupExecutionOutcome(ctx, id, true)
}

func (r exportRecorder) LookupExecutionOutcomeRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionOutcome, bool, error) {
	return r.lookupExecutionOutcome(ctx, id, false)
}

func (r exportRecorder) lookupExecutionOutcome(
	ctx context.Context, id domain.InvocationID, requireCurrent bool,
) (domain.ExecutionOutcome, bool, error) {
	var record domain.ExecutionOutcome
	err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if requireCurrent {
			record, err = tx.GetExecutionOutcome(ctx, id)
		} else {
			record, err = tx.GetExecutionOutcomeRecord(ctx, id)
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.ExecutionOutcome{}, false, nil
	}
	return record, err == nil, err
}

func (r exportRecorder) RecordExportRejection(
	ctx context.Context, rejection domain.ExportRejection,
) error {
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExportRejection(ctx, rejection)
	})
}

func (r exportRecorder) LookupExportRejection(
	ctx context.Context, id domain.InvocationID,
) (domain.ExportRejection, bool, error) {
	var record domain.ExportRejection
	err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetExportRejection(ctx, id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.ExportRejection{}, false, nil
	}
	return record, err == nil, err
}

// artifactStore persists released evidence bytes, the immutable artifact rows
// the imported claims name, and the invocation-bound claim record that carries
// the complete labeled AgentClaim set (label and inline text included) for the
// durable review surface. An artifact row alone drops the label and text, so it
// is not that record.
type artifactStore struct {
	blobs *signet.BlobStore
	store *store.Store
}

func (a artifactStore) PutBlob(_ context.Context, digest domain.Digest, body []byte) error {
	if _, err := a.blobs.Put(digest, bytes.NewReader(body)); err != nil {
		return err
	}
	// Put verifies the bytes against the digest, so a stored blob is
	// resolvable by the address the result will name.
	return nil
}

func (a artifactStore) RecordClaims(
	ctx context.Context, id domain.InvocationID, claims []domain.AgentClaim,
) error {
	if len(claims) == 0 {
		return nil
	}
	return a.store.Write(ctx, func(tx *store.WriteTx) error {
		for _, claim := range claims {
			artifact, err := domain.NewArtifact(domain.ArtifactInput{
				ID: claim.Artifact, Type: domain.ArtifactKindEvidence, Digest: claim.Digest,
				Provenance: claim.Provenance,
			}, nil)
			if err != nil {
				return fmt.Errorf("agent claim %q: %w", claim.Label, err)
			}
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return fmt.Errorf("persist agent claim %q for %s: %w", claim.Label, id, err)
			}
		}
		// The claim record and its artifact rows share this transaction, so the
		// invocation-bound record either lands complete or not at all: no artifact
		// row survives without the labeled claim set that names it, and RecordClaims
		// never reports success for a partial record. PutAgentClaims is write-once,
		// so a re-run replaying the identical set converges and a differing set is
		// an ErrImmutableConflict.
		if err := tx.PutAgentClaims(ctx, id, claims); err != nil {
			return fmt.Errorf("persist agent claims for %s: %w", id, err)
		}
		return nil
	})
}

// transportSeeder adapts the publish transport's exact-base checkouts to the
// driver's Seeder port. These are the only materialization paths that stamp
// the canonical repository name and numeric identity ward refuses to seed a
// workspace without. Anything that becomes a ward SeedBaseCheckout source
// must use FetchBaseWorktree; FetchBase is for the import lane alone.
type transportSeeder struct{ transport *publish.Transport }

func (s transportSeeder) FetchBase(ctx context.Context, repo, baseRef, baseSHA, dir string) error {
	_, err := s.transport.FetchBase(ctx, repo, baseRef, baseSHA, dir)
	return classifyTransportSeedError(err)
}

func (s transportSeeder) FetchBaseWorktree(
	ctx context.Context, repo, baseRef, baseSHA, dir string,
) error {
	_, err := s.transport.FetchBaseWorktree(ctx, repo, baseRef, baseSHA, dir)
	return classifyTransportSeedError(err)
}

func classifyTransportSeedError(err error) error {
	if err == nil {
		return err
	}
	if errors.Is(err, publish.ErrRemoteMissingBase) ||
		errors.Is(err, publish.ErrMaterializationRefused) ||
		isPermanentSeedCredentialError(err) {
		return fmt.Errorf("%w: %w", claude.ErrSeedRefused, err)
	}
	var gitErr *publish.TransportGitError
	if errors.As(err, &gitErr) && gitErr.Refusal == publish.RefusalAuth {
		return fmt.Errorf("%w: %w", claude.ErrSeedRefused, err)
	}
	var apiErr *publish.APIError
	if errors.As(err, &apiErr) &&
		apiErr.Status >= http.StatusBadRequest && apiErr.Status < http.StatusInternalServerError &&
		apiErr.Status != http.StatusRequestTimeout &&
		apiErr.Status != http.StatusConflict &&
		apiErr.Status != http.StatusTooEarly &&
		apiErr.Status != http.StatusTooManyRequests {
		return fmt.Errorf("%w: %w", claude.ErrSeedRefused, err)
	}
	return fmt.Errorf("%w: %w", claude.ErrSeedRetryable, err)
}

func isPermanentSeedCredentialError(err error) bool {
	for _, target := range []error{
		publish.ErrNoAppCredentials,
		publish.ErrNoAppRegistration,
		publish.ErrLegacyAppMigrationRequired,
		publish.ErrUnreadableRegistration,
		publish.ErrCredentialPermissions,
		publish.ErrAmbiguousAppRegistration,
		publish.ErrAppRegistrationMismatch,
		publish.ErrAppVisibilityMismatch,
		publish.ErrGrantMismatch,
		publish.ErrTokenExpiry,
		publish.ErrInstallationResolution,
		publish.ErrNoInstallation,
		publish.ErrAmbiguousInstallation,
		publish.ErrInstallationGrantUntrusted,
		publish.ErrInstallationAuthoritySnapshot,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// claudeComposition is the live production wiring one daemon run owns.
//
// Unattended admission requires a durable, generation-current
// backend-conformance record. The operator may request a fresh store-backed
// startup pass for this exact composition with -run-conformance; otherwise a
// matching prior pass is restored. The daemon keeps the same conformance
// closure for every scheduled doctor pass.
type claudeComposition struct {
	driver                          *claude.Driver
	backend                         *ward.Backend
	authority                       *publish.InstallationAuthorityStore
	publicationTransport            engine.PublicationTransport
	publisher                       *publish.Publisher
	reviewSource                    exec.ReviewSource
	shadowReviewSource              exec.ReviewSource
	reviewRecovery                  func(context.Context) error
	reviewConfigurationDigest       domain.Digest
	shadowReviewConfigurationDigest domain.Digest
	shadowReviewCostOwner           string
	shadowReviewRate                float64
	reviewHostInstructions          engine.ReviewHostInstructions
	containerBin                    string
	env                             engine.AdmissionEnvironment
	elaborationPromptPackage        domain.Digest
	remediationPromptPackage        domain.Digest
	derive                          engine.AdmissionDerivation
	runConformance                  func(context.Context) error
	closer                          sessionCloser
	janitor                         *janitorSession
	// observeBaseTip is the base-advance watch's conditional ref read
	// through the publish reconciler (§5.11 conditional requests; §5.16
	// base_advance_watch consumer).
	observeBaseTip func(context.Context, domain.ScheduleBaseWatch) (string, error)
	// observePull and observeIssue are the §5.18 base-watch capture and
	// status-independent completion sweep's conditional reads through the same
	// reconciler.
	observePull  pullObserver
	observeIssue issueObserver
	// observeLabelIssues is the label-intake loop's conditional read of the open
	// issues carrying one initiator label, through the same reconciler (#659).
	observeLabelIssues labelIssueObserver
	// evictLabelIssues drops a (repo, label) label-issue cache after a failed
	// durable intake write, so the next tick re-observes unconditionally.
	evictLabelIssues func(repo, label string)
	// observeReview is the §5.16 native-review observation's conditional reads
	// through the same reconciler (issue #497).
	observeReview nativeReviewObserver
	// reviewInvalidate evicts a PR's cached review-activity validators after a
	// failed durable append, so the next tick re-fetches unconditionally rather
	// than riding a 304 and stranding the un-persisted rows (issue #497).
	reviewInvalidate func(repo string, number int)
	// evictConcluded drops the four conditional-request cache entries owned by
	// a ready resource after its attention item leaves the open state.
	evictConcluded func(domain.ReadyItemPRBinding, *int)
}

// ErrProjectImageComposition marks an unattended composition whose configured
// agent image cannot be bound to a matching immutable project-image record.
// Refusing at startup turns the run-482 misconfiguration (a base the pinned
// image was never built from) into a daemon-start failure instead of a run
// that spends implementation before publication binding rejects it.
var ErrProjectImageComposition = errors.New("configured agent image has no matching project-image record")

// resolveProjectImagePreparation binds the configured agent image to the
// immutable project_images provenance production publication already treats as
// authority (engine.productionPublicationWorkflow.loadBinding), and returns the
// workspace-hydration argv the launch command must run. It refuses when no
// record names the image; when its recorded repository, repository ID, or
// commit disagrees with the configured repo/base; or when the decoded
// preparation command is not the fixed image-owned helper the builder and
// onboarding policy admit (projectimage.PreparationPath). Naming each mismatch,
// because the implementer must hydrate from an image built for exactly this
// repository and base (a mismatched base would make the preparation helper's
// manifest guard exit 42), running exactly the approved helper.
//
// The command re-gate is the reconstruction trust boundary (AGENTS.md daemon
// conventions): PreparationCommand is a store field decoded here, so a
// corrupted or tampered row that is otherwise self-consistent must not reach
// the root launch argv. Validate only rejects empty/NUL commands and
// shell-quoting only prevents injection, so neither bounds *which* command
// runs; onboarding gates the same field to the identical fixed value, and this
// boundary re-runs that gate rather than trusting the decoded argv.
func resolveProjectImagePreparation(
	image domain.ProjectImage, found bool, cfg claudeDriverConfig,
) ([]string, error) {
	switch {
	case !found:
		return nil, fmt.Errorf("agent image %q: %w", cfg.AgentImage, ErrProjectImageComposition)
	case image.Repository != cfg.Repo:
		return nil, fmt.Errorf(
			"project-image repository %q disagrees with configured %q: %w",
			image.Repository, cfg.Repo, ErrProjectImageComposition)
	case image.RepositoryID != cfg.RepositoryID:
		return nil, fmt.Errorf(
			"project-image repository id %d disagrees with configured %d: %w",
			image.RepositoryID, cfg.RepositoryID, ErrProjectImageComposition)
	case image.CommitSHA != cfg.BaseSHA:
		return nil, fmt.Errorf(
			"project-image commit %s disagrees with configured base %s: %w",
			image.CommitSHA, cfg.BaseSHA, ErrProjectImageComposition)
	case !slices.Equal(image.PreparationCommand, []string{projectimage.PreparationPath}):
		return nil, fmt.Errorf(
			"project-image preparation command %q is not the approved %q: %w",
			image.PreparationCommand, projectimage.PreparationPath, ErrProjectImageComposition)
	}
	return slices.Clone(image.PreparationCommand), nil
}

// composeClaudeDriver builds the production ward gate and Claude driver.
// Nothing here reaches the network or the runtime; the caller runs the
// conformance suite and the driver's restart reconciliation before the
// engine loop starts.
func composeClaudeDriver(
	ctx context.Context, st *store.Store, blobs *signet.BlobStore, cfg claudeDriverConfig,
	logger *slog.Logger,
) (_ *claudeComposition, err error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	reviewConfigurationDigest, err := requireApprovedReviewConfiguration(ctx, st, cfg)
	if err != nil {
		return nil, err
	}
	shadowReviewDigests, err := requireApprovedShadowReviewConfiguration(ctx, st, cfg)
	if err != nil {
		return nil, err
	}
	promptPackage, promptPackageBody, err := ingestPromptPackage(blobs, cfg.PromptPackageFile)
	if err != nil {
		return nil, err
	}
	elaborationPromptPackage, elaborationPromptPackageBody, err := ingestPromptPackage(
		blobs, cfg.ElaborationPromptPackageFile)
	if err != nil {
		return nil, err
	}
	if err := claude.ValidatePromptPackageRoles(
		promptPackageBody, elaborationPromptPackageBody); err != nil {
		return nil, fmt.Errorf("prompt package roles: %w", err)
	}
	remediationPromptPackage, remediationPromptPackageBody, err := ingestPromptPackage(
		blobs, cfg.RemediationPromptPackageFile)
	if err != nil {
		return nil, err
	}
	if err := claude.ValidatePromptPackageRoles(
		promptPackageBody, remediationPromptPackageBody); err != nil {
		return nil, fmt.Errorf("remediation prompt package roles: %w", err)
	}
	// Resolve the workspace-hydration command before any network-touching
	// composition, so a base/image misconfiguration fails at startup. Only the
	// unattended path runs a project image; the attended conversation-turn path
	// carries no preparation and stays unchanged.
	var preparation []string
	if cfg.OperatingMode == domain.ModeUnattended {
		var (
			image domain.ProjectImage
			found bool
		)
		if readErr := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			image, found, err = tx.GetProjectImageByRef(ctx, cfg.AgentImage)
			return err
		}); readErr != nil {
			return nil, fmt.Errorf("resolve project image: %w", readErr)
		}
		if preparation, err = resolveProjectImagePreparation(image, found, cfg); err != nil {
			return nil, err
		}
	}
	authority, err := publish.NewInstallationAuthorityStore(cfg.StateRoot)
	if err != nil {
		return nil, err
	}
	transport, publisher, commitAuthors, janitor, reconciler, err := claudeTransport(ctx, st, cfg, authority)
	if err != nil {
		return nil, err
	}
	publicationTransport, err := engine.NewGitPublicationTransport(transport)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, janitor.Close(context.Background()))
		}
	}()
	adapters, adapterErr := wardstore.New(st)
	if adapterErr != nil {
		return nil, fmt.Errorf("compose ward store adapters: %w", adapterErr)
	}
	runtime := ward.NewCLIRuntime(cfg.ContainerBin)
	wardConfig := ward.Config{
		AgentImage:    string(cfg.AgentImage),
		ExporterImage: cfg.ExporterImage,
		// The trusted in-image helper's fixed argv, from the gauntlet lane's
		// own contract rather than a string repeated here: the daemon and the
		// image cannot drift apart on how the export is produced.
		ExporterCommand:   export.HelperCommand(),
		SeedRoot:          cfg.SeedRoot,
		ExportRoot:        filepath.Join(cfg.StateDir, "ward-exports"),
		ProviderEndpoints: cfg.ProviderEndpoints,
		Scanner:           credentialScanner{},
		AuthStoreLeaser:   adapters.Leaser,
		Journal:           adapters.Journal,
	}
	backend, backendErr := ward.New(runtime, wardConfig)
	if backendErr != nil {
		return nil, fmt.Errorf("compose ward backend: %w", backendErr)
	}
	reviewLifecycle, lifecycleErr := ward.NewCodexReviewLifecycle(
		runtime, wardConfig, productionRigRuntimeAuthorizer(cfg.StateDir, cfg.RigTokenFile),
	)
	if lifecycleErr != nil {
		return nil, fmt.Errorf("compose Codex review lifecycle: %w", lifecycleErr)
	}
	var (
		reviewSource           exec.ReviewSource
		reviewHostInstructions engine.ReviewHostInstructions
	)
	volumeLeaser, err := ward.NewRuntimeCodexReviewVolumeLeaser(runtime)
	if err != nil {
		return nil, fmt.Errorf("compose Codex review volume lifecycle: %w", err)
	}
	reviewRecovery, err := ward.NewCodexReviewRecovery(
		reviewLifecycle, adapters.Journal, volumeLeaser, cfg.ReviewInputRoot)
	if err != nil {
		return nil, fmt.Errorf("compose Codex review recovery: %w", err)
	}
	var shadowReviewRecovery func(context.Context) error
	var shadowReviewSource exec.ReviewSource
	if cfg.ShadowReviewImage != "" {
		shadowLifecycle, lifecycleErr := ward.NewClaudeReviewLifecycle(
			runtime, wardConfig, productionRigRuntimeAuthorizer(cfg.StateDir, cfg.RigTokenFile),
		)
		if lifecycleErr != nil {
			return nil, fmt.Errorf("compose Claude shadow review lifecycle: %w", lifecycleErr)
		}
		shadowRecovery, recoveryErr := ward.NewCodexReviewRecovery(
			shadowLifecycle, adapters.Journal, volumeLeaser, cfg.ReviewInputRoot,
		)
		if recoveryErr != nil {
			return nil, fmt.Errorf("compose Claude shadow review recovery: %w", recoveryErr)
		}
		shadowReviewRecovery = shadowRecovery.Reconcile
		shadowConfig := ward.CodexReviewConfig{
			InputRoot: cfg.ReviewInputRoot, WorkspaceTarget: "/workspace/project",
			ProviderEndpoints: []string{"api.anthropic.com:443"},
			ApprovedImage:     cfg.ShadowReviewImage, ObserverImage: cfg.ExporterImage,
			Model: cfg.ShadowReviewModel, ReasoningEffort: cfg.ShadowReviewReasoningEffort,
			AuthStoreLeaser: adapters.Leaser, AuthState: adapters.AuthState,
			Now: func() time.Time { return time.Now().UTC() }, Journal: adapters.Journal,
			VolumeLifecycleLeaser: volumeLeaser,
		}
		shadowReviewSource, err = ward.NewClaudeReviewSource(ward.CodexReviewSourceConfig{
			Lifecycle: shadowLifecycle, Review: shadowConfig, Journal: adapters.Journal,
			WorkspaceSizeMB: cfg.ShadowReviewWorkspaceSizeMB,
			AuthMode:        ward.CodexAuthSetupToken, AuthIdentityID: cfg.AuthIdentityID,
			AuthSnapshot: cfg.ShadowReviewAuthSnapshot, InstructionArtifacts: blobs,
			ConfigurationDigest: shadowReviewDigests.runtime,
			CostOwner:           cfg.ShadowReviewCostOwner, Now: func() time.Time { return time.Now().UTC() },
		})
		if err != nil {
			return nil, fmt.Errorf("compose Claude shadow review source: %w", err)
		}
	}
	if cfg.OperatingMode == domain.ModeUnattended {
		forbiddenInstructionPaths := []string{cfg.ReviewAuthSnapshot}
		if cfg.ShadowReviewAuthSnapshot != "" {
			forbiddenInstructionPaths = append(forbiddenInstructionPaths, cfg.ShadowReviewAuthSnapshot)
		}
		reviewHostInstructions, err = engine.SnapshotReviewHostInstructions(
			ctx, cfg.ReviewInstructions, forbiddenInstructionPaths...,
		)
		if err != nil {
			return nil, fmt.Errorf("snapshot Codex review host instructions: %w", err)
		}
		reviewEndpoints := []string{"chatgpt.com:443"}
		if cfg.ReviewAuthMode == ward.CodexAuthAPIKey {
			reviewEndpoints = []string{"api.openai.com:443"}
		}
		reviewConfig := ward.CodexReviewConfig{
			InputRoot: cfg.ReviewInputRoot, WorkspaceTarget: "/workspace/project",
			ProviderEndpoints: reviewEndpoints, ApprovedImage: cfg.ReviewImage,
			ObserverImage: cfg.ExporterImage, Model: cfg.ReviewModel,
			ReasoningEffort: cfg.ReviewReasoningEffort, AccessTokenLifetimeFloor: time.Hour,
			AccessTokenRefreshThreshold: 2 * time.Hour,
			AuthStoreLeaser:             adapters.Leaser, AuthRefresher: ward.NewCodexAuthHTTPRefresher(),
			AuthState: adapters.AuthState,
			Now:       func() time.Time { return time.Now().UTC() }, Journal: adapters.Journal,
			VolumeLifecycleLeaser: volumeLeaser,
		}
		reviewSource, err = ward.NewCodexReviewSource(ward.CodexReviewSourceConfig{
			Lifecycle: reviewLifecycle, Review: reviewConfig, Journal: adapters.Journal,
			WorkspaceSizeMB: cfg.ReviewWorkspaceSizeMB, AuthMode: cfg.ReviewAuthMode,
			AuthIdentityID: cfg.ReviewAuthIdentityID, AuthSnapshot: cfg.ReviewAuthSnapshot,
			InstructionArtifacts: blobs,
			ConfigurationDigest:  reviewConfigurationDigest,
			CostOwner:            cfg.ReviewCostOwner, Now: func() time.Time { return time.Now().UTC() },
		})
		if err != nil {
			return nil, fmt.Errorf("compose Codex review source: %w", err)
		}
	}
	var (
		conformance     domain.BackendConformance
		haveConformance bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		conformance, haveConformance, err = tx.LatestBackendConformance(
			ctx, domain.BackendFreshVMReadOnlyVolumeHandoff)
		return err
	}); err != nil {
		return nil, fmt.Errorf("restore ward backend conformance: %w", err)
	}
	if cfg.RunConformance {
		if err := runClaudeConformance(
			ctx, st, transport, backend, cfg, janitor.WithStableCoverage,
		); err != nil {
			return nil, fmt.Errorf("run ward conformance: %w", err)
		}
	} else if err := restoreClaudeBackendConformance(
		backend, conformance, haveConformance, cfg.OperatingMode == domain.ModeUnattended,
	); err != nil {
		return nil, err
	}

	driver, driverErr := claude.New(claude.Config{
		Lifetime:     ctx,
		Dir:          filepath.Join(cfg.StateDir, "claude-driver"),
		SeedRoot:     cfg.SeedRoot,
		ExportRoot:   filepath.Join(cfg.StateDir, "ward-exports"),
		Gate:         backend,
		Seeder:       transportSeeder{transport: transport},
		Exports:      exportRecorder{store: st},
		ImportStarts: exportRecorder{store: st},
		Outcomes:     exportRecorder{store: st},
		Authority: storeAdmissionAuthority{
			store: st, blobs: blobs, allowedPaths: slices.Clone(cfg.AllowedPaths),
			commitAuthors:             commitAuthors,
			authenticatedStartAuthors: newProductionCommitAuthorAuthenticationCache(),
		},
		Artifacts: artifactStore{blobs: blobs, store: st},
		Volumes:   adapters.Leaser,
		PreJob: func(ctx context.Context, id domain.InvocationID) error {
			if err := bindRigInvocationResources(cfg.StateDir, cfg.RigTokenFile, id); err != nil {
				return err
			}
			return backend.PreJob(ctx, ward.PreJobRunIDForInvocation(id))
		},
		Import: importer.Options{
			Policy: importer.Policy{Allowlist: cfg.AllowedPaths},
		},
		Preparation: preparation,
		Now:         func() time.Time { return time.Now().UTC() },
		Logger:      logger,
	})
	if driverErr != nil {
		return nil, fmt.Errorf("compose claude driver: %w", driverErr)
	}

	identity := cfg.AuthIdentityID
	env := engine.AdmissionEnvironment{
		OperatingMode:             cfg.OperatingMode,
		CredentialMode:            domain.CredentialSubscriptionContained,
		EgressProfile:             domain.EgressProviderOnly,
		EnforceableEgressProfiles: []domain.EgressProfile{domain.EgressProviderOnly},
		ImageRef:                  cfg.AgentImage,
		PromptPackageDigest:       promptPackage,
		ReviewConfigurationDigest: reviewConfigurationDigest,
		VendorInstructions: engine.VendorInstructionConfig{
			Vendor:   domain.AgentVendorClaude,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
			HostPath: cfg.VendorInstructions,
		},
		// Base and Workspace are per-attempt and supplied by derive below;
		// the static values here would be wrong the moment a second work item
		// is submitted.
		AuthIdentityID: &identity,
	}
	composition := &claudeComposition{
		driver: driver, backend: backend, authority: authority,
		observeBaseTip: func(obsCtx context.Context, watch domain.ScheduleBaseWatch) (string, error) {
			obs, err := reconciler.ReconcileRef(obsCtx, watch.Repo, watch.BaseRef)
			if err != nil {
				return "", err
			}
			if !obs.Exists {
				return "", fmt.Errorf("base ref %s/%s does not exist", watch.Repo, watch.BaseRef)
			}
			return obs.SHA, nil
		},
		observePull: func(obsCtx context.Context, repo string, number int) (publish.PullObservation, error) {
			return reconciler.ReconcilePull(obsCtx, repo, number)
		},
		observeIssue: func(obsCtx context.Context, repo string, number int) (publish.IssueObservation, error) {
			return reconciler.ReconcileIssue(obsCtx, repo, number)
		},
		observeLabelIssues: func(obsCtx context.Context, repo, label string) (publish.LabelIssuesObservation, error) {
			return reconciler.ReconcileLabelIssues(obsCtx, repo, label)
		},
		evictLabelIssues: reconciler.EvictLabelIssues,
		observeReview: func(obsCtx context.Context, repo string, number int) (publish.PullReviewObservation, error) {
			return reconciler.ReconcilePullReviewActivity(obsCtx, repo, number)
		},
		reviewInvalidate: reconciler.EvictPullReviewActivity,
		evictConcluded: func(binding domain.ReadyItemPRBinding, boundIssue *int) {
			reconciler.EvictRef(binding.Repo, binding.BaseRef)
			reconciler.EvictPull(binding.Repo, binding.PRNumber)
			reconciler.EvictPullReviewActivity(binding.Repo, binding.PRNumber)
			if boundIssue != nil {
				reconciler.EvictIssue(binding.Repo, *boundIssue)
			}
		},
		publicationTransport: publicationTransport,
		publisher:            publisher, reviewSource: reviewSource, shadowReviewSource: shadowReviewSource,
		reviewRecovery: func(recoveryCtx context.Context) error {
			err := reviewRecovery.Reconcile(recoveryCtx)
			if shadowReviewRecovery != nil {
				err = errors.Join(err, shadowReviewRecovery(recoveryCtx))
			}
			return err
		},
		reviewConfigurationDigest: reviewConfigurationDigest, containerBin: cfg.ContainerBin,
		shadowReviewConfigurationDigest: shadowReviewDigests.runtime,
		shadowReviewCostOwner:           cfg.ShadowReviewCostOwner, shadowReviewRate: cfg.ShadowReviewRate,
		reviewHostInstructions:   reviewHostInstructions,
		env:                      env,
		elaborationPromptPackage: elaborationPromptPackage,
		remediationPromptPackage: remediationPromptPackage,
		derive:                   claudeAdmissionDerivation(cfg),
		runConformance: func(runCtx context.Context) error {
			return runClaudeConformance(
				runCtx, st, transport, backend, cfg, janitor.WithStableCoverage,
			)
		},
		closer:  sessionGroup{driver, janitor},
		janitor: janitor,
	}
	return composition, nil
}

type backendConformanceRestorer interface {
	ConfigurationDigest() domain.Digest
	RestoreConformance(domain.BackendConformance) error
}

func restoreClaudeBackendConformance(
	backend backendConformanceRestorer, conformance domain.BackendConformance,
	found, required bool,
) error {
	if !found || conformance.Outcome != domain.ConformancePassed ||
		conformance.ConfigurationDigest != backend.ConfigurationDigest() {
		if required {
			return fmt.Errorf("restore production backend conformance: %w",
				errBackendConformanceUnavailable)
		}
		return nil
	}
	return backend.RestoreConformance(conformance)
}

// requireApprovedReviewConfiguration re-gates the caller-selected review and
// exporter images before composition can construct a transport or execute the
// conformance runtime against repository content.
func requireApprovedReviewConfiguration(
	ctx context.Context, st *store.Store, cfg claudeDriverConfig,
) (domain.Digest, error) {
	if cfg.OperatingMode != domain.ModeUnattended {
		return "", nil
	}
	digest, err := claudeReviewConfigurationDigest(cfg)
	if err != nil {
		return "", err
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		profiles, err := tx.InspectLatestTrustProfiles(ctx)
		if err != nil {
			return err
		}
		matched := false
		for _, current := range profiles {
			if current.Repo != cfg.Repo {
				continue
			}
			if current.ReconstructionError != nil || current.Profile.RepositoryID != cfg.RepositoryID {
				return errors.Join(current.ReconstructionError, domain.ErrRepositoryIdentityMismatch)
			}
			matched = true
		}
		if !matched {
			return fmt.Errorf("target repository has no active trust profile: %w",
				domain.ErrReviewConfigurationUnapproved)
		}
		return tx.RequireReviewConfigurationApproved(ctx, digest)
	}); err != nil {
		return "", fmt.Errorf("require approved Codex review configuration: %w", err)
	}
	return digest, nil
}

func claudeReviewConfigurationDigest(cfg claudeDriverConfig) (domain.Digest, error) {
	endpoints := []string{"chatgpt.com:443"}
	if cfg.ReviewAuthMode == ward.CodexAuthAPIKey {
		endpoints = []string{"api.openai.com:443"}
	}
	digest, err := ward.CodexReviewConfigurationDigest(ward.CodexReviewConfig{
		InputRoot: cfg.ReviewInputRoot, WorkspaceTarget: "/workspace/project",
		ProviderEndpoints: endpoints, ApprovedImage: cfg.ReviewImage,
		ObserverImage: cfg.ExporterImage, Model: cfg.ReviewModel,
		ReasoningEffort:          cfg.ReviewReasoningEffort,
		AccessTokenLifetimeFloor: time.Hour, AccessTokenRefreshThreshold: 2 * time.Hour,
	}, cfg.ReviewWorkspaceSizeMB, cfg.ReviewAuthMode, cfg.ReviewAuthIdentityID, cfg.ReviewCostOwner)
	if err != nil {
		return "", fmt.Errorf("digest Codex review configuration: %w", err)
	}
	return digest, nil
}

func requireApprovedShadowReviewConfiguration(
	ctx context.Context, st *store.Store, cfg claudeDriverConfig,
) (shadowReviewDigests, error) {
	if cfg.OperatingMode != domain.ModeUnattended || cfg.ShadowReviewImage == "" {
		return shadowReviewDigests{}, nil
	}
	digests, err := shadowReviewConfigurationDigests(cfg)
	if err != nil {
		return shadowReviewDigests{}, err
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		profiles, err := tx.InspectLatestTrustProfiles(ctx)
		if err != nil {
			return err
		}
		matched := false
		for _, current := range profiles {
			if current.Repo != cfg.Repo {
				continue
			}
			if current.ReconstructionError != nil || current.Profile.RepositoryID != cfg.RepositoryID {
				return errors.Join(current.ReconstructionError, domain.ErrRepositoryIdentityMismatch)
			}
			matched = true
		}
		if !matched {
			return fmt.Errorf("target repository has no active trust profile: %w",
				domain.ErrShadowReviewConfigUnapproved)
		}
		return tx.RequireShadowReviewConfigurationApproved(
			ctx, domain.ShadowReviewClaudeLocal, digests.approval,
		)
	}); err != nil {
		return shadowReviewDigests{}, fmt.Errorf(
			"require approved Claude shadow review configuration: %w", err,
		)
	}
	return digests, nil
}

type shadowReviewDigests struct {
	runtime  domain.Digest
	approval domain.Digest
}

const shadowReviewCompositionDigestVersion = "freeside-shadow-review-composition/v1"

func shadowReviewConfigurationDigests(cfg claudeDriverConfig) (shadowReviewDigests, error) {
	runtimeDigest, err := ward.ClaudeReviewConfigurationDigest(ward.CodexReviewConfig{
		InputRoot: cfg.ReviewInputRoot, WorkspaceTarget: "/workspace/project",
		ProviderEndpoints: []string{"api.anthropic.com:443"},
		ApprovedImage:     cfg.ShadowReviewImage, ObserverImage: cfg.ExporterImage,
		Model: cfg.ShadowReviewModel, ReasoningEffort: cfg.ShadowReviewReasoningEffort,
	}, cfg.ShadowReviewWorkspaceSizeMB, ward.CodexAuthSetupToken,
		cfg.AuthIdentityID, cfg.ShadowReviewCostOwner)
	if err != nil {
		return shadowReviewDigests{}, fmt.Errorf(
			"digest Claude shadow review runtime configuration: %w", err,
		)
	}
	approvalDigest, err := shadowReviewCompositionDigest(runtimeDigest, cfg.ShadowReviewRate)
	if err != nil {
		return shadowReviewDigests{}, err
	}
	return shadowReviewDigests{runtime: runtimeDigest, approval: approvalDigest}, nil
}

// shadowReviewCompositionDigest extends ward's invocation-runtime digest with
// the deployment-owned sampling rate. The runtime digest remains the value the
// source and returned evidence re-authenticate; this outer digest is the exact
// independently owner-approved composition from #912.
func shadowReviewCompositionDigest(runtimeDigest domain.Digest, rate float64) (domain.Digest, error) {
	if !contentaddr.Valid(string(runtimeDigest)) || math.IsNaN(rate) || math.IsInf(rate, 0) ||
		rate < 0 || rate > 1 {
		return "", errors.New("invalid Claude shadow review composition")
	}
	if rate == 0 {
		rate = 0
	}
	body, err := json.Marshal(struct {
		Version       string        `json:"version"`
		RuntimeDigest domain.Digest `json:"runtime_digest"`
		Rate          float64       `json:"rate"`
	}{shadowReviewCompositionDigestVersion, runtimeDigest, rate})
	if err != nil {
		return "", fmt.Errorf("encode Claude shadow review composition: %w", err)
	}
	return domain.Digest(contentaddr.Sum(body)), nil
}

func bindRigInvocationResources(
	stateRoot, tokenFile string, invocationID domain.InvocationID,
) error {
	resources := ward.RuntimeResourceNamesFor(claude.RunIDFor(invocationID))
	resources.Containers = append(
		resources.Containers, ward.PreJobContainerNameForInvocation(invocationID),
	)
	return bindRigRuntimeResources(stateRoot, tokenFile, resources)
}

func productionRigRuntimeAuthorizer(
	stateRoot, tokenFile string,
) ward.RuntimeResourceAuthorizer {
	if tokenFile == "" {
		return nil
	}
	return func(_ context.Context, resources ward.RuntimeResourceNames) error {
		return bindRigRuntimeResources(stateRoot, tokenFile, resources)
	}
}

func bindRigRuntimeResources(
	stateRoot, tokenFile string, resources ward.RuntimeResourceNames,
) error {
	if tokenFile == "" {
		return nil
	}
	token, err := readRigToken(tokenFile)
	if err != nil {
		return fmt.Errorf("read production rig token: %w", err)
	}
	if _, err := daemonlock.BindRigRuntimeResources(
		stateRoot, token, resources.Containers, resources.Volumes, resources.Networks,
	); err != nil {
		return fmt.Errorf("bind production rig runtime resources: %w", err)
	}
	return nil
}

type storeConformanceRecorder struct {
	store *store.Store
}

func (r storeConformanceRecorder) RecordBackendConformance(
	ctx context.Context, record domain.BackendConformance,
) error {
	return r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(ctx, record)
		return err
	})
}

func runClaudeConformance(
	ctx context.Context,
	st *store.Store,
	transport *publish.Transport,
	backend *ward.Backend,
	cfg claudeDriverConfig,
	withStableCoverage func(func() error) error,
) error {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("mint conformance run identity: %w", err)
	}
	runID := "conf-" + hex.EncodeToString(nonce[:])
	if err := bindRigRuntimeResources(
		cfg.StateDir, cfg.RigTokenFile, ward.FullConformanceRuntimeResourceNamesFor(runID),
	); err != nil {
		return fmt.Errorf("bind production rig conformance resources: %w", err)
	}
	if err := os.MkdirAll(cfg.SeedRoot, 0o700); err != nil {
		return fmt.Errorf("create conformance seed root: %w", err)
	}
	seedDir := filepath.Join(cfg.SeedRoot, "."+runID)
	if _, err := os.Stat(seedDir); err == nil {
		return fmt.Errorf("conformance seed path unexpectedly exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect conformance seed path: %w", err)
	}
	defer func() { _ = os.RemoveAll(seedDir) }()
	// The worktree variant, exactly as the run seed uses: this directory
	// becomes a ward SeedBaseCheckout, and the gate's observer proves the
	// workspace's raw worktree against HEAD, so the repository-only fetch is
	// refused as dirty before conformance can pass. These are the only two
	// production SeedBaseCheckout sources; the publication lane's fetches
	// keep the repository-only shape on purpose.
	if withStableCoverage == nil {
		return errors.New("conformance: nil janitor coverage coordinator")
	}
	if err := withStableCoverage(func() error {
		return (transportSeeder{transport: transport}).FetchBaseWorktree(
			ctx, cfg.Repo, cfg.BaseRef, cfg.BaseSHA, seedDir,
		)
	}); err != nil {
		return fmt.Errorf("fetch exact conformance base: %w", err)
	}
	suite, err := ward.NewSuite(backend, ward.SuiteFixture{ //nolint:gosec // G101: fixture carries an inert synthetic marker, never a credential
		AgentImage:       string(cfg.AgentImage),
		CredentialTarget: "/var/lib/freeside/conformance-token",
		CredentialMarker: "FREESIDE_CONF_" + strings.ToUpper(hex.EncodeToString(nonce[:])),
		RunID:            runID,
		Seed: ward.WorkspaceSeed{
			Mode:      ward.SeedBaseCheckout,
			SourceDir: seedDir,
			Base: domain.BaseRevision{
				Repo: cfg.Repo, RepositoryID: cfg.RepositoryID,
				BaseRef: cfg.BaseRef, BaseSHA: cfg.BaseSHA,
			},
		},
	}, ward.WithConformanceRecorder(storeConformanceRecorder{store: st}))
	if err != nil {
		return err
	}
	return suite.Full(ctx)
}

// claudeAdmissionDerivation supplies the per-attempt workspace, and the
// operator-configured base beside it.
//
// The workspace must be derived per attempt (each ward handoff owns its own
// volume) and is derived exactly as the driver derives it, so the gate
// creates the volume the admission recorded. The base is the operator's
// -base-sha: Phase 1A.2 runs under manually configured unattended
// preconditions, and resolving a branch tip at admission time would need a
// publish-lane transport capability that does not exist yet. Deferred
// deliberately: pinning the base per run, rather than per daemon, is a
// shared-contract change (the Run carries no base).
func claudeAdmissionDerivation(cfg claudeDriverConfig) engine.AdmissionDerivation {
	base := domain.BaseRevision{
		Repo: cfg.Repo, RepositoryID: cfg.RepositoryID,
		BaseRef: cfg.BaseRef, BaseSHA: cfg.BaseSHA,
	}
	return func(_ context.Context, id domain.InvocationID) (string, domain.BaseRevision, error) {
		return claude.WorkspaceFor(id), base, nil
	}
}

// claudeTransport builds the authenticated exact-base transport the driver
// seeds workspaces and import checkouts from. It reuses the publication
// lane's App-authority chain: one credential path, one janitor, one set of
// minted installation tokens, rather than a second way to reach the same
// repository.
func claudeTransport(
	ctx context.Context,
	st *store.Store,
	cfg claudeDriverConfig,
	authority *publish.InstallationAuthorityStore,
) (*publish.Transport, *publish.Publisher, *publish.GitHubAppBotIdentityResolver, *janitorSession, *publish.Reconciler, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	keystore, err := publish.NewKeystore(cfg.CredentialsDir, cfg.StateRoot)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	recorder, err := publish.NewStoreRecorder(st)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	janitor, err := publish.NewInstallationJanitor(
		keystore, client, defaultGitHubAPIBase, authority, authority, recorder, time.Now,
		defaultJanitorRemovalBound,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	trust, err := publish.NewStoreTrustSource(st)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	minter := publish.NewMinterWithJanitor(
		keystore, client, defaultGitHubAPIBase, recorder, trust, time.Now, janitor,
	)
	tokens := publish.NewCachedTokenSource(minter, time.Now)
	commitAuthors, err := publish.NewGitHubAppBotIdentityResolver(
		tokens, keystore, client, defaultGitHubAPIBase, time.Now,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	transport, err := publish.NewTransport(
		tokens,
		publish.TransportOptions{RemoteBase: defaultGitHubRemoteBase},
	)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	auditor, err := publish.NewGitHubWorkflowAuditor(
		tokens, client, defaultGitHubAPIBase, time.Now,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	ledger, err := publish.NewStoreLedger(st)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	authorizations, err := publish.NewStoreAuthorizationSource(st)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	publisher := publish.NewPublisher(
		tokens, client, defaultGitHubAPIBase, auditor, ledger, trust, authorizations,
	)
	if err := transport.AuthorizePublisher(publisher); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	apps, err := keystore.ListApps()
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if len(apps) == 0 {
		return nil, nil, nil, nil, nil, publish.ErrNoAppCredentials
	}
	registrationIDs := make([]int64, 0, len(apps))
	for _, app := range apps {
		registrationIDs = append(registrationIDs, app.AppID)
	}
	session, err := startJanitorSession(ctx, janitor, registrationIDs)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("start installation janitor: %w", err)
	}
	return transport, publisher, commitAuthors, session,
		publish.NewReconciler(tokens, client, defaultGitHubAPIBase), nil
}

const janitorStartupTimeout = 2 * time.Minute

type janitorRunner interface {
	RunScheduledPass(context.Context) error
	ActiveFor(int64) bool
	WithStableCoverage(func() error) error
	RegistrationFaults() []publish.JanitorRegistrationFault
	ChurningRegistrations() []publish.JanitorRegistrationChurn
	IncompleteRegistrations() []publish.JanitorRegistrationIncomplete
	PendingReady(publish.PendingInstallationEnvelope) (int64, bool)
}

// janitorSession exposes the daemon's janitor to the composition: the
// stable-coverage coordinator conformance uses, and the scheduled pass the
// §5.16 scheduler's janitor kind fires. The pre-1B always-on goroutine is
// gone — startJanitorSession primes coverage with one synchronous pass (the
// same coverage-before-ready startup gate the loop start used to provide)
// and the durable scheduler owns every later pass; a pass failure there
// stops the scheduler loop, which stays daemon-fatal exactly as a stopped
// janitor was.
type janitorSession struct {
	janitor janitorRunner
}

func startJanitorSession(
	parent context.Context,
	janitor janitorRunner,
	registrationIDs []int64,
) (*janitorSession, error) {
	if janitor == nil {
		return nil, errors.New("nil installation janitor")
	}
	if len(registrationIDs) == 0 {
		return nil, publish.ErrNoAppCredentials
	}
	startupCtx, stopWaiting := context.WithTimeout(parent, janitorStartupTimeout)
	defer stopWaiting()
	if err := janitor.RunScheduledPass(startupCtx); err != nil {
		return nil, fmt.Errorf("installation janitor startup pass: %w", err)
	}
	for _, registrationID := range registrationIDs {
		if janitor.ActiveFor(registrationID) {
			continue
		}
		causes := make([]error, 0, 1)
		for _, fault := range janitor.RegistrationFaults() {
			if fault.RegistrationID == registrationID {
				causes = append(causes, fault.Err)
			}
		}
		for _, churn := range janitor.ChurningRegistrations() {
			if churn.RegistrationID == registrationID {
				causes = append(causes, fmt.Errorf(
					"removal churn for %d consecutive passes",
					churn.ConsecutivePasses,
				))
			}
		}
		for _, incomplete := range janitor.IncompleteRegistrations() {
			if incomplete.RegistrationID == registrationID {
				causes = append(causes, errors.New("reconciliation incomplete: removal budget"))
			}
		}
		return nil, errors.Join(fmt.Errorf(
			"installation janitor did not publish coverage for registration %d",
			registrationID), errors.Join(causes...))
	}
	return &janitorSession{janitor: janitor}, nil
}

// RunScheduledPass is the scheduler-fired janitor pass (§5.16 janitor kind).
func (s *janitorSession) RunScheduledPass(ctx context.Context) error {
	if s == nil || s.janitor == nil {
		return errors.New("nil installation janitor session")
	}
	return s.janitor.RunScheduledPass(ctx)
}

func (s *janitorSession) RegistrationFaults() []publish.JanitorRegistrationFault {
	if s == nil || s.janitor == nil {
		return nil
	}
	return s.janitor.RegistrationFaults()
}

func (s *janitorSession) WithStableCoverage(fn func() error) error {
	if s == nil || s.janitor == nil {
		return errors.New("nil installation janitor session")
	}
	return s.janitor.WithStableCoverage(fn)
}

// PendingReady is the janitor's onboarding transition signal, exposed for
// the installation-poll schedule kind.
func (s *janitorSession) PendingReady(
	envelope publish.PendingInstallationEnvelope,
) (int64, bool) {
	if s == nil || s.janitor == nil {
		return 0, false
	}
	return s.janitor.PendingReady(envelope)
}

// Close is retained for the session-group shape; the janitor holds no
// goroutine to stop, and the scheduler's context owns pass cancellation.
func (s *janitorSession) Close(context.Context) error {
	return nil
}

type sessionGroup []sessionCloser

func (g sessionGroup) Close(ctx context.Context) error {
	var result error
	for _, session := range g {
		if session != nil {
			result = errors.Join(result, session.Close(ctx))
		}
	}
	return result
}

// stageDriver wraps the Claude driver in the production materializing seam,
// so digest verification always completes before an intent is committed.
func (c *claudeComposition) stageDriver(blobs *signet.BlobStore) (exec.StageDriver, error) {
	materializer, err := productionMaterializer(blobs)
	if err != nil {
		return nil, fmt.Errorf("compose materializer: %w", err)
	}
	return exec.NewMaterializingStageDriver(materializer, c.driver)
}

func productionMaterializer(blobs *signet.BlobStore) (*exec.Materializer, error) {
	return exec.NewMaterializer(productionInputSource{blobs: blobs}, exec.MaterializerOptions{
		MaxInputBytes: exec.ProductionMaxInputBytes,
		MaxTotalBytes: exec.ProductionMaxTotalInputBytes,
	})
}

func productionElaborationDeliveryValidator(
	materializer *exec.Materializer,
) func(context.Context, exec.StartSpec) error {
	return productionPromptDeliveryValidator(materializer, engine.ErrElaborationInputUndeliverable)
}

func productionImplementationDeliveryValidator(
	materializer *exec.Materializer,
) func(context.Context, exec.StartSpec) error {
	return productionPromptDeliveryValidator(materializer, engine.ErrProductionInputUndeliverable)
}

func productionPromptDeliveryValidator(
	materializer *exec.Materializer,
	undeliverable error,
) func(context.Context, exec.StartSpec) error {
	return func(ctx context.Context, spec exec.StartSpec) error {
		inputs, err := materializer.Materialize(ctx, spec)
		if err != nil {
			if errors.Is(err, exec.ErrInputTooLarge) {
				return fmt.Errorf("%w: %w", undeliverable, err)
			}
			return err
		}
		if err := claude.ValidatePromptInputs(inputs); err != nil {
			if errors.Is(err, claude.ErrUnsupportedStart) {
				return fmt.Errorf("%w: %w", undeliverable, err)
			}
			return err
		}
		return nil
	}
}

// productionInputSource separates an operational filesystem refusal from a
// durable missing/corrupt input. Only the former may hold an outbox item for a
// later pass; the materializer keeps the permanent signet sentinels loud.
type productionInputSource struct{ blobs *signet.BlobStore }

func (s productionInputSource) OpenContext(
	ctx context.Context, digest domain.Digest,
) (io.ReadCloser, error) {
	body, err := s.blobs.OpenContext(ctx, digest)
	if err != nil {
		if errors.Is(err, signet.ErrBlobNotFound) || errors.Is(err, signet.ErrInvalidDigest) {
			return nil, err
		}
		return nil, fmt.Errorf("open production input: %w",
			errors.Join(exec.ErrInputUnavailable, err))
	}
	return retryableInputReadCloser{ReadCloser: body}, nil
}

type retryableInputReadCloser struct{ io.ReadCloser }

func (r retryableInputReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = errors.Join(exec.ErrInputUnavailable, err)
	}
	return n, err
}

func (r retryableInputReadCloser) Close() error {
	if err := r.ReadCloser.Close(); err != nil {
		return errors.Join(exec.ErrInputUnavailable, err)
	}
	return nil
}
