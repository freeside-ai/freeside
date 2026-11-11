package daemoncompose

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
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/admissionauth"
	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/exportscan"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
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

// ClaudeDriverConfig is the operator-supplied half of the production
// composition. Everything here is deliberately explicit: a defaulted image,
// endpoint, or identity would put an unaudited value into the durable
// admission record.
type ClaudeDriverConfig struct {
	Judgments     JudgmentConfig
	AgentImage    domain.ImageRef
	ExporterImage string
	ContainerBin  string
	SeedRoot      string
	StateDir      string
	RigTokenFile  string
	// WriterStopTimeout bounds how long the implementation writer container may
	// run before it must reach observed "stopped" (ward's writer-termination
	// handoff check). Zero keeps ward's built-in default (10m). Production sets
	// it larger because a legitimate implementation can exceed 10m; a too-short
	// bound aborts the run at the writer-termination handoff with a context
	// deadline instead of publishing.
	WriterStopTimeout time.Duration
	// WriterStallInterval is how long a running writer may go without a
	// provider response before the advisory invocation_stalled notice is
	// raised. It never changes WriterStopTimeout. Zero keeps ward's default.
	WriterStallInterval time.Duration
	// Rebuild holds the host-specific inputs for rebuilding a project image
	// when a candidate changes its dependencies within policy (plan §5.7). An
	// empty BaseBuildRef leaves the daemon unable to rebuild: such a candidate
	// then blocks publication as it did before.
	Rebuild           projectimage.BuildInputs
	ProviderEndpoints []string
	// The prompt-package files are trusted implementation, specification, and
	// remediation inputs. The daemon derives every digest from ingested bytes.
	PromptPackageFile              string
	SpecificationPromptPackageFile string
	RemediationPromptPackageFile   string
	VendorInstructions             string
	Repo                           string
	RepositoryID                   int64
	BaseRef                        string
	BaseSHA                        string
	// AgentTreeCheckout and AgentTreeCommit name the admitted-agent tree: a
	// local checkout and the exact commit whose policy/ tree admission reads.
	// Every identity the daemon runs under comes from that tree's lineup.
	AgentTreeCheckout, AgentTreeCommit string
	AllowedPaths                       []string
	// RunConformance executes the store-backed full ward suite against this
	// exact runtime/image/configuration before the engine can admit work.
	RunConformance bool
	// HostVerification names the macOS verification executor; an empty root
	// configures none (ComposeHostVerificationExecutor).
	HostVerification MacverifyExecutorConfig
	// StateRoot and CredentialsDir locate the GitHub App authority and
	// credential material the exact-base transport authenticates with.
	StateRoot                   string
	CredentialsDir              string
	OperatingMode               domain.OperatingMode
	ReviewImage                 string
	ReviewInputRoot             string
	ReviewAuthMode              ward.CodexAuthMode
	ReviewAuthSnapshot          string
	ReviewInstructions          string
	ReviewWorkspaceSizeMB       int64
	ShadowReviewImage           string
	ShadowReviewAuthSnapshot    string
	ShadowReviewWorkspaceSizeMB int64
	ShadowReviewRate            float64

	// ReviewProvider is the routed review provider the reviewer line's
	// harness selects (ResolveReviewSelection, #397).
	ReviewProvider ward.RoutedReviewProvider
	// The review identities, cost owners, models, and efforts are resolved
	// from the reviewer and shadow reviewer lineup lines
	// (ResolveReviewSelection), never set by a flag. An effort is the native
	// value the launch passes. They feed the review sources and the approved
	// configuration digests, which is why they stay beside the review fields.
	ReviewAuthIdentityID        domain.AuthIdentityID
	ReviewCostOwner             string
	ReviewModel                 string
	ReviewReasoningEffort       string
	ShadowReviewAuthIdentityID  domain.AuthIdentityID
	ShadowReviewCostOwner       string
	ShadowReviewModel           string
	ShadowReviewReasoningEffort string
}

var errBackendConformanceUnavailable = errors.New("exact passing backend conformance proof is unavailable")

func (c ClaudeDriverConfig) Validate() error {
	if c.Rebuild.BaseBuildRef == "" {
		if c.Rebuild.BuildProxy != "" || len(c.Rebuild.DNS) != 0 {
			return fmt.Errorf("-build-proxy and -build-dns configure the project-image rebuild and need -base-build-ref")
		}
	} else if err := c.Rebuild.Validate(); err != nil {
		return fmt.Errorf("project-image rebuild flags: %w", err)
	}
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
	case c.SpecificationPromptPackageFile == "":
		return fmt.Errorf("-specification-prompt-package is required in claude driver mode")
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
	case c.AgentTreeCheckout == "" || c.AgentTreeCommit == "":
		return fmt.Errorf("-agent-tree and -agent-tree-commit are required in claude driver mode")
	case !filepath.IsAbs(c.AgentTreeCheckout) || filepath.Clean(c.AgentTreeCheckout) != c.AgentTreeCheckout:
		return fmt.Errorf("-agent-tree must be a clean absolute path")
	case !engine.ExplicitAllowedPaths(c.AllowedPaths):
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
		(c.ReviewImage == "" || c.ReviewInputRoot == "" ||
			c.ReviewAuthSnapshot == "" || c.ReviewInstructions == "" || c.ReviewWorkspaceSizeMB <= 0):
		return fmt.Errorf("review configuration is required in claude driver mode")
	case c.OperatingMode == domain.ModeUnattended && domain.ImageRef(c.ReviewImage).Validate() != nil:
		return fmt.Errorf("-review-image must be digest-pinned")
	case c.OperatingMode == domain.ModeUnattended &&
		(!filepath.IsAbs(c.ReviewInputRoot) || filepath.Clean(c.ReviewInputRoot) != c.ReviewInputRoot):
		return fmt.Errorf("-review-input-root must be a clean absolute path")
	case c.OperatingMode == domain.ModeUnattended && !slices.Contains(ReviewAuthModes, c.ReviewAuthMode):
		// Whether the mode suits the reviewer's harness is checked once the
		// line is resolved (RequireReviewAuthMode).
		return fmt.Errorf("-review-auth-mode is invalid")
	case c.ShadowReviewImage != "" && c.OperatingMode != domain.ModeUnattended:
		return fmt.Errorf("shadow review requires unattended mode")
	case c.ShadowReviewImage == "" && c.ShadowReviewAuthSnapshot != "":
		return fmt.Errorf("-shadow-review-image is required when shadow review fields are set")
	case c.ShadowReviewImage != "" &&
		(c.ShadowReviewAuthSnapshot == "" ||
			c.ShadowReviewWorkspaceSizeMB <= 0 || math.IsNaN(c.ShadowReviewRate) ||
			math.IsInf(c.ShadowReviewRate, 0) || c.ShadowReviewRate < 0 || c.ShadowReviewRate > 1):
		return fmt.Errorf("complete shadow review configuration is required when enabled")
	case c.ShadowReviewImage != "" && domain.ImageRef(c.ShadowReviewImage).Validate() != nil:
		return fmt.Errorf("-shadow-review-image must be digest-pinned")
	}
	return nil
}

// AdmissionCapabilitySnapshot is the selected mode's engine floor as a
// durable snapshot, for the store policy that re-gates every admission.
func AdmissionCapabilitySnapshot(mode domain.OperatingMode) domain.CapabilitySnapshot {
	return exec.NewCapabilitySet(AdmissionFloor(mode)...).Snapshot()
}

func AdmissionFloor(mode domain.OperatingMode) []exec.Capability {
	switch mode {
	case domain.ModeAttendedDev:
		return AttendedAdmissionFloor
	case domain.ModeUnattended:
		return unattendedAdmissionFloor
	}
	return nil
}

// AgentExpiryMargin is how far past an attempt's deadline an expiring
// credential must last (§5.4 step 4): slack for the teardown that still holds
// the store after the handoff budget ends.
const AgentExpiryMargin = 5 * time.Minute

// LoadAgentSelection reads the admitted-agent tree at the operator's exact
// commit and returns the selection that admits ward stages through its
// lineup. The tree is read once: a later commit takes effect at the next
// start, so every admission of one daemon run cites one revision. The caller
// sets the attempt budget and the gate once the backend is composed.
func LoadAgentSelection(ctx context.Context, cfg ClaudeDriverConfig) (*engine.AgentSelection, error) {
	tree, revision, err := ReadAgentTree(ctx, cfg.AgentTreeCheckout, cfg.AgentTreeCommit)
	if err != nil {
		return nil, err
	}
	egress := slices.Clone(cfg.ProviderEndpoints)
	slices.Sort(egress)
	return &engine.AgentSelection{
		Tree: tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
		EffectiveEgress: egress, ExpiryMargin: AgentExpiryMargin,
	}, nil
}

// ReadAgentTree reads and parses the admitted-agent tree at an exact commit
// of a local checkout and returns it with its revision.
func ReadAgentTree(ctx context.Context, checkout, commit string) (agenttree.Tree, domain.Digest, error) {
	scratch, err := os.MkdirTemp("", "freesided-agent-tree-")
	if err != nil {
		return agenttree.Tree{}, "", fmt.Errorf("agent tree scratch: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	files, err := agenttree.ReadCommit(ctx, checkout, scratch, commit)
	if err != nil {
		return agenttree.Tree{}, "", fmt.Errorf("read agent tree: %w", err)
	}
	tree, err := agenttree.Parse(files)
	if err != nil {
		return agenttree.Tree{}, "", fmt.Errorf("parse agent tree at %s: %w", commit, err)
	}
	revision, err := agenttree.Revision(files)
	if err != nil {
		return agenttree.Tree{}, "", fmt.Errorf("agent tree revision at %s: %w", commit, err)
	}
	return tree, revision, nil
}

func readPromptPackage(path string) ([]byte, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: operator-configured control-plane prompt package
	if err != nil {
		return nil, fmt.Errorf("read prompt package: %w", err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("prompt package %s is empty", path)
	}
	return body, nil
}

type PromptPackages struct {
	Implementation []byte
	Specification  []byte
	Remediation    []byte
}

// ReadPromptPackages shares composition's file and role checks with the
// deputy's early validation, without opening a store or writing blobs.
func ReadPromptPackages(cfg ClaudeDriverConfig) (PromptPackages, error) {
	var packages PromptPackages
	var err error
	if packages.Implementation, err = readPromptPackage(cfg.PromptPackageFile); err != nil {
		return PromptPackages{}, fmt.Errorf("implementation prompt package: %w", err)
	}
	if packages.Specification, err = readPromptPackage(cfg.SpecificationPromptPackageFile); err != nil {
		return PromptPackages{}, fmt.Errorf("specification prompt package: %w", err)
	}
	if err := claude.ValidatePromptPackageRoles(packages.Implementation, packages.Specification); err != nil {
		return PromptPackages{}, fmt.Errorf("prompt package roles: %w", err)
	}
	if packages.Remediation, err = readPromptPackage(cfg.RemediationPromptPackageFile); err != nil {
		return PromptPackages{}, fmt.Errorf("remediation prompt package: %w", err)
	}
	if err := claude.ValidatePromptPackageRoles(packages.Implementation, packages.Remediation); err != nil {
		return PromptPackages{}, fmt.Errorf("remediation prompt package roles: %w", err)
	}
	return packages, nil
}

// storePromptPackage persists the exact validated bytes by content address;
// composition must not re-read a mutable file after validating its role.
func storePromptPackage(blobs *signet.BlobStore, body []byte) (domain.Digest, error) {
	sum := sha256.Sum256(body)
	digest := domain.Digest(contentaddr.Format(sum[:]))
	if _, err := blobs.Put(digest, bytes.NewReader(body)); err != nil {
		return "", fmt.Errorf("store prompt package: %w", err)
	}
	return digest, nil
}

// AttendedAdmissionFloor is ward's honest base capability class before the
// full conformance suite earns the two unattended-only proofs.
var AttendedAdmissionFloor = []exec.Capability{
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

// ExportRecorder adapts the store's internal-transaction export seam to the
// driver's port (the wardstore adapter precedent: the driver holds no store
// handle, and RecordExecutionExport is not client-visible state). usage
// receives an unattended export's Claude usage capture once the export
// commits, since that export is never collected; nil drops it with a warning
// on logger.
type ExportRecorder struct {
	Store  *store.Store
	usage  engine.ClaudeUsageSink
	logger *slog.Logger
}

func (r ExportRecorder) RecordExecutionExport(
	ctx context.Context,
	record domain.ExecutionExport,
	replay claude.ExecutionReplay,
) error {
	err := engine.RecordExecutionExport(ctx, r.Store, record, productionReplayFrom(replay), r.usage, r.logger)
	if errors.Is(err, store.ErrImmutableConflict) {
		return errors.Join(err, domain.ErrImmutableTransition)
	}
	return err
}

// productionReplayFrom carries every field of the driver's replay to the
// engine's. A field left behind is lost silently, as the Claude usage
// capture once was (#2716), so a test pins that the two field sets match.
func productionReplayFrom(replay claude.ExecutionReplay) engine.ProductionReplay {
	return engine.ProductionReplay{
		InvocationID: replay.InvocationID, ObservedBaseSHA: replay.ObservedBaseSHA,
		HeadSHA: replay.HeadSHA, Manifest: replay.Manifest,
		ManifestDigest: replay.ManifestDigest, Evidence: replay.Evidence,
		EvidenceManifestDigest: replay.EvidenceManifestDigest,
		CommitPlanDigest:       replay.CommitPlanDigest,
		ImportOptions:          replay.ImportOptions,
		Usage:                  replay.Usage,
		ClaudeUsage:            replay.ClaudeUsage,
	}
}

func (r ExportRecorder) LookupExecutionExport(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionExport, bool, error) {
	return r.lookupExecutionExport(ctx, id, true)
}

func (r ExportRecorder) LookupExecutionExportRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionExport, bool, error) {
	return r.lookupExecutionExport(ctx, id, false)
}

func (r ExportRecorder) RecordCurrentImportStart(
	ctx context.Context, start domain.CurrentImportStart,
) error {
	err := r.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordCurrentImportStart(ctx, start)
	})
	if errors.Is(err, store.ErrImmutableConflict) {
		return errors.Join(err, domain.ErrImmutableTransition)
	}
	return err
}

func (r ExportRecorder) LookupCurrentImportStart(
	ctx context.Context, id domain.InvocationID,
) (domain.CurrentImportStart, bool, error) {
	var start domain.CurrentImportStart
	err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		start, err = tx.GetCurrentImportStart(ctx, id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.CurrentImportStart{}, false, nil
	}
	return start, err == nil, err
}

func (r ExportRecorder) lookupExecutionExport(
	ctx context.Context, id domain.InvocationID, requireCurrent bool,
) (domain.ExecutionExport, bool, error) {
	var record domain.ExecutionExport
	err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
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

func (r ExportRecorder) RecordExecutionOutcome(
	ctx context.Context, record domain.ExecutionOutcome,
) error {
	return r.Store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExecutionOutcome(ctx, record)
	})
}

func (r ExportRecorder) LookupExecutionOutcome(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionOutcome, bool, error) {
	return r.lookupExecutionOutcome(ctx, id, true)
}

func (r ExportRecorder) LookupExecutionOutcomeRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionOutcome, bool, error) {
	return r.lookupExecutionOutcome(ctx, id, false)
}

func (r ExportRecorder) lookupExecutionOutcome(
	ctx context.Context, id domain.InvocationID, requireCurrent bool,
) (domain.ExecutionOutcome, bool, error) {
	var record domain.ExecutionOutcome
	err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
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

func (r ExportRecorder) RecordExportRejection(
	ctx context.Context, rejection domain.ExportRejection,
) error {
	return r.Store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExportRejection(ctx, rejection)
	})
}

func (r ExportRecorder) LookupExportRejection(
	ctx context.Context, id domain.InvocationID,
) (domain.ExportRejection, bool, error) {
	var record domain.ExportRejection
	err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
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
			// The artifact row names the same bytes the claim does, so it carries
			// the claim's validated media type, size, and recorded time. The
			// source flips to run: an Artifact rides the run channel (the claim
			// is the claim-channel reference for the same digest).
			artifactMetadata := claim.Metadata
			artifactMetadata.Source = domain.EvidenceSourceRun
			artifact, err := domain.NewArtifact(domain.ArtifactInput{
				ID: claim.Artifact, Type: domain.ArtifactKindEvidence, Digest: claim.Digest,
				Provenance: claim.Provenance,
				Metadata:   artifactMetadata,
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

func (a artifactStore) LookupClaims(
	ctx context.Context, id domain.InvocationID,
) ([]domain.AgentClaim, bool, error) {
	var claims []domain.AgentClaim
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		claims, err = tx.GetAgentClaims(ctx, id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	return claims, err == nil, err
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

// ClaudeComposition is the live production wiring one daemon run owns.
//
// Unattended admission requires a durable, generation-current
// backend-conformance record. The operator may request a fresh store-backed
// startup pass for this exact composition with -run-conformance; otherwise a
// matching prior pass is restored. The daemon keeps the same conformance
// closure for every scheduled doctor pass.
type ClaudeComposition struct {
	driver                          *claude.Driver
	Backend                         *ward.Backend
	authority                       *publish.InstallationAuthorityStore
	publicationTransport            engine.PublicationTransport
	publisher                       *publish.Publisher
	FollowUpFiler                   *publish.FollowUpFiler
	ReviewReplier                   *publish.ExternalReviewReplier
	reviewSource                    exec.ReviewSource
	shadowReviewSource              exec.ReviewSource
	reviewAdmission                 engine.ReviewAdmitter
	reviewRecovery                  func(context.Context) error
	ReviewConfigurationDigest       domain.Digest
	shadowReviewConfigurationDigest domain.Digest
	// selectionFailure names the writer role that failed its startup
	// admission check; while set, the admission gate holds every attempt.
	selectionFailure           *RoleAdmissionError
	shadowReviewRate           float64
	reviewHostInstructions     engine.ReviewHostInstructions
	ContainerBin               string
	env                        engine.AdmissionEnvironment
	specificationPromptPackage domain.Digest
	remediationPromptPackage   domain.Digest
	derive                     engine.AdmissionDerivation
	// selectImage selects each unattended writer attempt's image from the
	// admitted-image pointer, and bindImageUpkeep gives it image upkeep's
	// pending-resolution lookup once upkeep is composed. Both are nil for
	// an attended composition, which runs no project image.
	selectImage     engine.ImageSelection
	bindImageUpkeep func(func(context.Context) (*domain.ImageUpkeepHold, error))
	runConformance  func(context.Context) error
	// hostVerification is the macOS verification executor, nil when none
	// is configured.
	hostVerification *macverify.Backend
	Closer           SessionCloser
	janitor          *janitorSession
	// observeRepositoryTip is the conditional ref read through the publish
	// reconciler (§5.11 conditional requests) for the active-resource
	// reconciler's tip watch and image upkeep, which feed project-image
	// upkeep (§5.7).
	observeRepositoryTip func(ctx context.Context, repo, branch string) (string, error)
	// fetchTip fetches the configured base branch at tipSHA into dir for
	// image upkeep and returns the repository ID the checkout carries.
	fetchTip func(ctx context.Context, tipSHA, dir string) (int64, error)
	// observePull and observeIssue are the §5.18 completion sweep's
	// conditional reads through the same reconciler.
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
	observeReview NativeReviewObserver
	// reviewInvalidate evicts a PR's cached review-activity validators after a
	// failed durable append, so the next tick re-fetches unconditionally rather
	// than riding a 304 and stranding the un-persisted rows (issue #497).
	reviewInvalidate func(repo string, number int)
	// evictConcluded drops the four conditional-request cache entries owned by
	// a ready resource after its attention item leaves the open state.
	evictConcluded func(domain.ReadyItemPRBinding, *int)
	// issueReadTokens authenticates research fetches of the managed
	// repository's issues; nil when no App transport is composed.
	issueReadTokens publish.TokenSource
}

// seedAdmittedImagePointer makes the configured agent image the pointer's
// seed (plan §5.7, Project-image upkeep): a repository with no pointer for
// the Claude agent base gets one naming the configured image, whether or not
// it serves the configured base, since a base no image serves holds attempts
// rather than refusing the start. An existing pointer is never overwritten,
// because once the upkeep job owns it the configured flag may be the older
// image; a disagreement is logged and the pointer kept. Writer selection
// reads the pointer (claudeImageSelector).
func seedAdmittedImagePointer(
	ctx context.Context, st *store.Store, image domain.ProjectImage, now time.Time,
	logger *slog.Logger,
) error {
	var kept domain.AdmittedImagePointer
	err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		existing, _, found, err := tx.GetAdmittedImagePointer(
			ctx, image.RepositoryID, domain.HarnessClientClaudeCode)
		if err != nil || found {
			kept = existing
			return err
		}
		return tx.AdvanceAdmittedImagePointer(ctx, domain.AdmittedImagePointer{
			RepositoryID: image.RepositoryID, Repository: image.Repository,
			Harness: domain.HarnessClientClaudeCode, ImageID: image.ID,
			ImageRef: image.ImageRef, AdvancedAt: now,
		})
	})
	if err != nil {
		return fmt.Errorf("seed admitted image pointer: %w", err)
	}
	if kept.ImageID != "" && kept.ImageID != image.ID {
		logger.Info("admitted image pointer differs from the configured agent image; keeping the pointer",
			"repository_id", image.RepositoryID, "pointer_image_ref", kept.ImageRef,
			"configured_image_ref", image.ImageRef)
	}
	return nil
}

// ExactBaseFetcher materializes the configured base at dir, which does not
// exist yet, and reports the numeric identity of the repository it fetched
// from. Production binds it to the publication transport's FetchBase.
type ExactBaseFetcher func(ctx context.Context, dir string) (repositoryID int64, err error)

// ObserveCompositionBaseInputs fetches the configured base once into private
// scratch and reads the project-image inputs from it. Startup has no checkout
// of the managed repository, and the transport's exact-base fetch is the same
// proof every later boundary uses: the checkout holds cfg.BaseSHA only when
// the remote's base branch does.
func ObserveCompositionBaseInputs(
	ctx context.Context, fetch ExactBaseFetcher, cfg ClaudeDriverConfig,
	layout domain.ProjectImageInputLayout,
) (domain.ProjectImageBaseInputs, error) {
	scratch, err := os.MkdirTemp("", "freesided-project-image-base-")
	if err != nil {
		return domain.ProjectImageBaseInputs{}, fmt.Errorf("create base scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort cleanup of startup-owned observation state
	dir := filepath.Join(scratch, "checkout")
	repositoryID, err := fetch(ctx, dir)
	if err != nil {
		return domain.ProjectImageBaseInputs{}, fmt.Errorf("fetch exact base: %w", err)
	}
	if repositoryID != cfg.RepositoryID {
		return domain.ProjectImageBaseInputs{}, fmt.Errorf(
			"fetched base carries repository id %d, not the configured %d: %w",
			repositoryID, cfg.RepositoryID, domain.ErrRepositoryIdentityMismatch)
	}
	// "git" from PATH is the binary the transport that fetched dir runs
	// (claudeTransport leaves TransportOptions.GitPath at its default).
	return projectimage.ObserveBaseInputs(ctx, "git", dir, cfg.BaseSHA, layout)
}

// wardEnforceableEgressProfiles is the closed set of writer egress profiles
// this composition can materialize: the two the ward handoff proxy enforces
// and Suite.Full proves before supports_enforced_provider_egress is declared.
// Listing a profile here lets policy or a capability manifest select it, so a
// profile ward refuses (provider_web_read) must stay out.
func wardEnforceableEgressProfiles() []domain.EgressProfile {
	return []domain.EgressProfile{domain.EgressProviderOnly, domain.EgressProviderRegistry}
}

// composeClaudeDriver builds the production ward gate and Claude driver. Its
// network work is the publication transport's: the janitor's startup pass,
// and one exact-base fetch when the project image was built at a commit other
// than the configured base. It starts no runtime work unless cfg.RunConformance
// asks for the suite; the caller runs the driver's restart reconciliation
// before the engine loop starts.
// usage receives an unattended writer turn's Claude usage capture when its
// export is recorded; nil, as for a daemon without the Claude collector,
// drops it with a warning.
func composeClaudeDriver(
	ctx context.Context, st *store.Store, blobs *signet.BlobStore, cfg ClaudeDriverConfig,
	usage engine.ClaudeUsageSink, logger *slog.Logger,
) (_ *ClaudeComposition, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	agents, err := LoadAgentSelection(ctx, cfg)
	if err != nil {
		return nil, err
	}
	utcNow := func() time.Time { return time.Now().UTC() }
	// The review identities come from the lineup before anything reads them:
	// the approved configuration digests below take them as inputs.
	if cfg, err = ResolveReviewSelection(ctx, st, agents.Tree, cfg, utcNow()); err != nil {
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
	packages, err := ReadPromptPackages(cfg)
	if err != nil {
		return nil, err
	}
	promptPackage, err := storePromptPackage(blobs, packages.Implementation)
	if err != nil {
		return nil, fmt.Errorf("implementation prompt package: %w", err)
	}
	specificationPromptPackage, err := storePromptPackage(blobs, packages.Specification)
	if err != nil {
		return nil, fmt.Errorf("specification prompt package: %w", err)
	}
	remediationPromptPackage, err := storePromptPackage(blobs, packages.Remediation)
	if err != nil {
		return nil, fmt.Errorf("remediation prompt package: %w", err)
	}
	// Resolve the workspace-hydration command before any network-touching
	// composition, so an image the record alone rules out fails at startup
	// without a network round trip. Only the unattended path runs a project
	// image; the attended conversation-turn path carries no preparation and
	// stays unchanged.
	var (
		preparation []string
		image       domain.ProjectImage
	)
	if cfg.OperatingMode == domain.ModeUnattended {
		var found bool
		if readErr := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			image, found, err = tx.GetProjectImageByRef(ctx, cfg.AgentImage)
			return err
		}); readErr != nil {
			return nil, fmt.Errorf("resolve project image: %w", readErr)
		}
		if preparation, err = projectimage.ResolvePreparation(image, found, cfg.AgentImage, cfg.Repo, cfg.RepositoryID); err != nil {
			return nil, err
		}
	}
	authority, err := publish.NewInstallationAuthorityStore(cfg.StateRoot)
	if err != nil {
		return nil, err
	}
	transport, publisher, commitAuthors, followUpFiler, janitor, reconciler, reviewReplier, issueReadTokens, err := claudeTransport(ctx, st, cfg, authority)
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
	// The base tree exists only on the remote, so reading it waits for the
	// transport. Stable coverage keeps the fetch's installation token from
	// failing under a concurrent pass.
	baseInputs := newBaseInputsMemo(func(
		ctx context.Context, layout domain.ProjectImageInputLayout,
	) (inputs domain.ProjectImageBaseInputs, err error) {
		err = janitor.WithStableCoverage(func() error {
			inputs, err = ObserveCompositionBaseInputs(ctx,
				func(ctx context.Context, dir string) (int64, error) {
					checkout, err := transport.FetchBase(ctx, cfg.Repo, cfg.BaseRef, cfg.BaseSHA, dir)
					if err != nil {
						return 0, err
					}
					return checkout.RepositoryID(), nil
				}, cfg, layout)
			return err
		})
		return inputs, err
	})
	if cfg.OperatingMode == domain.ModeUnattended {
		if err := seedAdmittedImagePointer(ctx, st, image, utcNow(), logger); err != nil {
			return nil, err
		}
	}
	adapters, adapterErr := wardstore.New(st)
	if adapterErr != nil {
		return nil, fmt.Errorf("compose ward store adapters: %w", adapterErr)
	}
	runtime := ward.NewCLIRuntime(cfg.ContainerBin)
	conformanceLeaser := &AdapterConformanceLeaser{Leaser: adapters.Leaser, Store: st}
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
		// Zero leaves ward's own default in place (config.withDefaults).
		WriterStopTimeout: cfg.WriterStopTimeout,
		StallInterval:     cfg.WriterStallInterval,
		Scanner:           exportscan.Scanner{},
		AuthStoreLeaser:   conformanceLeaser,
		Journal:           adapters.Journal,
		Logger:            logger,
		// Operator input files are read from the same immutable,
		// digest-addressed store admission described them from; ward
		// re-hashes every byte it stages.
		InputBlobs: blobs,
		// The handoff and review namespaces leave the rig once their
		// journal records close, so one deputy hold serves any number of
		// tasks.
		ReleaseRuntimeResources: ProductionRigRuntimeReleaser(cfg.StateDir, cfg.RigTokenFile),
	}
	backend, backendErr := ward.New(runtime, wardConfig)
	if backendErr != nil {
		return nil, fmt.Errorf("compose ward backend: %w", backendErr)
	}
	if err := recoverAdapterConformance(ctx, st, backend, cfg, conformanceLeaser); err != nil {
		return nil, err
	}
	// The Codex review lifecycle, volume leaser, and recovery stay composed
	// whatever the reviewer line's harness, so a restart onto a Claude
	// reviewer still reconciles the review intents and workspaces an
	// earlier Codex reviewer left in the journal.
	reviewLifecycle, lifecycleErr := ward.NewCodexReviewLifecycle(
		runtime, wardConfig, ProductionRigRuntimeAuthorizer(cfg.StateDir, cfg.RigTokenFile),
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
	routedClaude := cfg.OperatingMode == domain.ModeUnattended &&
		cfg.ReviewProvider.Vendor == domain.AgentVendorClaude
	// One Claude lifecycle and one recovery serve the routed and the shadow
	// Claude source when both run, rather than one per source.
	var (
		claudeLifecycle      *ward.CodexReviewLifecycle
		claudeReviewRecovery func(context.Context) error
	)
	if routedClaude || cfg.ShadowReviewImage != "" {
		claudeLifecycle, lifecycleErr = ward.NewClaudeReviewLifecycle(
			runtime, wardConfig, ProductionRigRuntimeAuthorizer(cfg.StateDir, cfg.RigTokenFile),
		)
		if lifecycleErr != nil {
			return nil, fmt.Errorf("compose Claude review lifecycle: %w", lifecycleErr)
		}
		claudeRecovery, recoveryErr := ward.NewCodexReviewRecovery(
			claudeLifecycle, adapters.Journal, volumeLeaser, cfg.ReviewInputRoot,
		)
		if recoveryErr != nil {
			return nil, fmt.Errorf("compose Claude review recovery: %w", recoveryErr)
		}
		claudeReviewRecovery = claudeRecovery.Reconcile
	}
	var shadowReviewSource exec.ReviewSource
	// Each composed review role, with the allowlist its proxy admits, which a
	// review admission snapshot records for the role.
	reviewRoles := map[domain.RoleName]engine.ReviewRole{}
	if cfg.ShadowReviewImage != "" {
		shadowRole := ReviewRole(domain.RoleShadowReviewer, ShadowReviewProvider(),
			[]string{"api.anthropic.com:443"})
		reviewRoles[domain.RoleShadowReviewer] = shadowRole
		shadowConfig := ward.CodexReviewConfig{
			InputRoot: cfg.ReviewInputRoot, WorkspaceTarget: "/workspace/project",
			ProviderEndpoints: shadowRole.EffectiveEgress,
			ApprovedImage:     cfg.ShadowReviewImage, ObserverImage: cfg.ExporterImage,
			Model: cfg.ShadowReviewModel, ReasoningEffort: cfg.ShadowReviewReasoningEffort,
			AuthStoreLeaser: adapters.Leaser, AuthState: adapters.AuthState,
			Now: func() time.Time { return time.Now().UTC() }, Journal: adapters.Journal,
			VolumeLifecycleLeaser: volumeLeaser,
		}
		shadowReviewSource, err = ward.NewClaudeReviewSource(ward.CodexReviewSourceConfig{
			Lifecycle: claudeLifecycle, Review: shadowConfig, Journal: adapters.Journal,
			WorkspaceSizeMB: cfg.ShadowReviewWorkspaceSizeMB,
			AuthMode:        ward.CodexAuthSetupToken, AuthIdentityID: cfg.ShadowReviewAuthIdentityID,
			AuthSnapshot: cfg.ShadowReviewAuthSnapshot, InstructionArtifacts: blobs,
			ConfigurationDigest: shadowReviewDigests.Runtime,
			CostOwner:           cfg.ShadowReviewCostOwner, Now: utcNow,
			Admit: ReviewAdmission(st, agents.Tree, shadowRole, utcNow),
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
			return nil, fmt.Errorf("snapshot review host instructions: %w", err)
		}
		routedRole := ReviewRole(domain.RoleReviewer, cfg.ReviewProvider,
			RoutedReviewEndpoints(cfg.ReviewProvider, cfg.ReviewAuthMode))
		reviewRoles[domain.RoleReviewer] = routedRole
		reviewConfig := RoutedReviewConfig(cfg.ReviewProvider, ward.CodexReviewConfig{
			InputRoot: cfg.ReviewInputRoot, WorkspaceTarget: "/workspace/project",
			ProviderEndpoints: routedRole.EffectiveEgress, ApprovedImage: cfg.ReviewImage,
			ObserverImage: cfg.ExporterImage, Model: cfg.ReviewModel,
			ReasoningEffort: cfg.ReviewReasoningEffort,
			AuthStoreLeaser: adapters.Leaser, AuthState: adapters.AuthState,
			Now: func() time.Time { return time.Now().UTC() }, Journal: adapters.Journal,
			VolumeLifecycleLeaser: volumeLeaser,
		})
		sourceConfig := ward.CodexReviewSourceConfig{
			Lifecycle: reviewLifecycle, Review: reviewConfig, Journal: adapters.Journal,
			WorkspaceSizeMB: cfg.ReviewWorkspaceSizeMB, AuthMode: cfg.ReviewAuthMode,
			AuthIdentityID: cfg.ReviewAuthIdentityID, AuthSnapshot: cfg.ReviewAuthSnapshot,
			InstructionArtifacts: blobs,
			ConfigurationDigest:  reviewConfigurationDigest,
			CostOwner:            cfg.ReviewCostOwner, Now: utcNow,
			Admit: ReviewAdmission(st, agents.Tree, routedRole, utcNow),
			Credential: reviewCredential(cfg.ReviewProvider, wardstore.CredentialReader{
				Store: st,
				ReadSetupToken: ProductionSetupTokenReader(
					runtime, cfg.ExporterImage, ProductionRigRuntimeAuthorizer(cfg.StateDir, cfg.RigTokenFile)),
				Now: utcNow,
			}.Read),
		}
		reviewSource, err = newRoutedReviewSource(cfg.ReviewProvider, sourceConfig, claudeLifecycle)
		if err != nil {
			return nil, fmt.Errorf("compose %s review source: %w", cfg.ReviewProvider.HarnessClient, err)
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
	if cfg.OperatingMode == domain.ModeUnattended {
		if err := startWardOnSelectedImage(
			ctx, st, backend, cfg, baseInputs, image.ImageRef, conformance, haveConformance, logger,
		); err != nil {
			return nil, err
		}
	}
	if cfg.RunConformance {
		if err := runClaudeConformance(
			ctx, st, transport, backend, cfg, janitor.WithStableCoverage, conformanceLeaser,
		); err != nil {
			return nil, fmt.Errorf("run ward conformance: %w", err)
		}
	} else if err := restoreClaudeBackendConformance(
		backend, conformance, haveConformance, cfg.OperatingMode == domain.ModeUnattended,
	); err != nil {
		return nil, err
	}
	hostVerification, err := ComposeHostVerificationExecutor(
		ctx, st, cfg.HostVerification, cfg.RunConformance, logger, proveHostVerification)
	if err != nil {
		return nil, err
	}

	driver, driverErr := claude.New(claude.Config{
		Lifetime:     ctx,
		Dir:          filepath.Join(cfg.StateDir, "claude-driver"),
		SeedRoot:     cfg.SeedRoot,
		ExportRoot:   filepath.Join(cfg.StateDir, "ward-exports"),
		Gate:         backend,
		Seeder:       transportSeeder{transport: transport},
		Exports:      ExportRecorder{Store: st, usage: usage, logger: logger},
		ImportStarts: ExportRecorder{Store: st, usage: usage, logger: logger},
		Outcomes:     ExportRecorder{Store: st, usage: usage, logger: logger},
		Authority: admissionauth.Authority{
			Store: st, Blobs: blobs, AllowedPaths: slices.Clone(cfg.AllowedPaths),
			CommitAuthors: commitAuthors,
			StartAuthors:  admissionauth.NewCommitAuthorCache(),
		},
		Artifacts: artifactStore{blobs: blobs, store: st},
		Volumes:   adapters.Leaser,
		PreJob: func(ctx context.Context, id domain.InvocationID) error {
			if err := bindRigInvocationResources(cfg.StateDir, cfg.RigTokenFile, id); err != nil {
				return err
			}
			if err := backend.PreJob(ctx, ward.PreJobRunIDForInvocation(id)); err != nil {
				return err
			}
			releasePreJobContainer(ctx, runtime, wardConfig.ReleaseRuntimeResources, id, logger)
			return nil
		},
		Stall: InvocationStallNotice(st, StallInterval(cfg.WriterStallInterval), time.Now),
		AdoptedRegistryHosts: func(ctx context.Context, repositoryID int64) ([]string, error) {
			var hosts []string
			err := st.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				hosts, err = tx.AdoptedRegistryHosts(ctx, repositoryID)
				return err
			})
			return hosts, err
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

	// Selection activates only when every writer role passes admission for the
	// prompt this daemon runs it on; until then the gate holds every attempt.
	// A marked generation is left to each attempt's admission.
	agents.AttemptBudget = backend.HandoffTimeout()
	selectionFailure := CheckWriterRoles(ctx, st, *agents, map[domain.RoleName]domain.Digest{
		domain.RoleSpecifier: specificationPromptPackage, domain.RoleImplementer: promptPackage,
		domain.RoleRemediator: remediationPromptPackage,
	}, utcNow())
	agents.Gate = AgentSelectionGate(st, selectionFailure)
	env := engine.AdmissionEnvironment{
		OperatingMode:             cfg.OperatingMode,
		CredentialMode:            domain.CredentialSubscriptionContained,
		EgressProfile:             domain.EgressProviderOnly,
		EnforceableEgressProfiles: wardEnforceableEgressProfiles(),
		// An unattended attempt's image is selected per attempt
		// (selectImage below); this is the attended composition's.
		ImageRef:                  cfg.AgentImage,
		PromptPackageDigest:       promptPackage,
		ReviewConfigurationDigest: reviewConfigurationDigest,
		VendorInstructions: engine.VendorInstructionConfig{
			Vendor:   domain.AgentVendorClaude,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
			HostPath: cfg.VendorInstructions,
		},
		// Base and Workspace are per-attempt and supplied by derive below;
		// the static values here would be wrong the moment a second task
		// is submitted.
		Agents: agents,
	}
	follower := newConformanceFollower(backend, func(runCtx context.Context) error {
		return runClaudeConformance(runCtx, st, transport, backend, cfg, janitor.WithStableCoverage, conformanceLeaser)
	}, time.Now, logger.With("subsystem", "writer_image_conformance"))
	var (
		selectImage     engine.ImageSelection
		bindImageUpkeep func(func(context.Context) (*domain.ImageUpkeepHold, error))
	)
	if cfg.OperatingMode == domain.ModeUnattended {
		selector := &claudeImageSelector{
			store: st, repo: cfg.Repo, repositoryID: cfg.RepositoryID, baseSHA: cfg.BaseSHA,
			backend: backend, base: baseInputs, follower: follower,
		}
		selectImage, bindImageUpkeep = selector.selectImage, selector.bindImageUpkeep
	}
	observeRefTip := refTipObserver(reconciler)
	composition := &ClaudeComposition{
		driver: driver, Backend: backend, authority: authority,
		hostVerification: hostVerification,
		selectionFailure: selectionFailure,
		FollowUpFiler:    followUpFiler,
		ReviewReplier:    reviewReplier,
		issueReadTokens:  issueReadTokens,
		fetchTip: func(fetchCtx context.Context, tipSHA, dir string) (repositoryID int64, err error) {
			// Stable coverage keeps the fetch's installation token from
			// failing under a concurrent janitor pass.
			err = janitor.WithStableCoverage(func() error {
				checkout, err := transport.FetchBase(fetchCtx, cfg.Repo, cfg.BaseRef, tipSHA, dir)
				if err != nil {
					return err
				}
				repositoryID = checkout.RepositoryID()
				return nil
			})
			return repositoryID, err
		},
		observeRepositoryTip: observeRefTip,
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
		reviewAdmission: ReviewAdmitter(st, *agents, reviewRoles),
		reviewRecovery: func(recoveryCtx context.Context) error {
			err := reviewRecovery.Reconcile(recoveryCtx)
			if claudeReviewRecovery != nil {
				err = errors.Join(err, claudeReviewRecovery(recoveryCtx))
			}
			return err
		},
		ReviewConfigurationDigest: reviewConfigurationDigest, ContainerBin: cfg.ContainerBin,
		shadowReviewConfigurationDigest: shadowReviewDigests.Runtime,
		shadowReviewRate:                cfg.ShadowReviewRate,
		reviewHostInstructions:          reviewHostInstructions,
		env:                             env,
		specificationPromptPackage:      specificationPromptPackage,
		remediationPromptPackage:        remediationPromptPackage,
		derive:                          claudeAdmissionDerivation(cfg),
		selectImage:                     selectImage,
		bindImageUpkeep:                 bindImageUpkeep,
		runConformance:                  follower.runScheduled,
		// The follower stops first, so a pass in flight ends before the
		// janitor its fetches ride on closes.
		Closer:  sessionGroup{follower, driver, janitor},
		janitor: janitor,
	}
	return composition, nil
}

// startWardOnSelectedImage points ward at the image this start selects
// before any proof is restored or run (startupWriterImage). A base the start
// cannot read fails the start, as reading it always has; a base no image
// serves does not.
func startWardOnSelectedImage(
	ctx context.Context, st *store.Store, backend startupWardBackend, cfg ClaudeDriverConfig,
	base *baseInputsMemo, pointed domain.ImageRef, conformance domain.BackendConformance, haveConformance bool,
	logger *slog.Logger,
) error {
	proved, err := startupProvedImage(ctx, st, cfg.RepositoryID, backend.ConfigurationDigestFor,
		conformance, haveConformance)
	if err != nil {
		return fmt.Errorf("find the proved writer image: %w", err)
	}
	selected, found, err := selectRecordedImage(ctx, st, cfg.Repo, cfg.RepositoryID, cfg.BaseSHA, proved, base)
	if err != nil {
		return err
	}
	if !found {
		// The pointer may have moved since the seed was read; ward starts on
		// whatever it names now.
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			_, image, ok, err := tx.GetAdmittedImagePointer(ctx, cfg.RepositoryID, domain.HarnessClientClaudeCode)
			if ok {
				pointed = image.ImageRef
			}
			return err
		}); err != nil {
			return fmt.Errorf("read admitted image pointer: %w", err)
		}
		logger.Warn("no admitted project image serves the configured base; writer attempts hold until one does",
			"base_sha", cfg.BaseSHA, "pointer_image_ref", pointed)
	}
	initial := startupWriterImage(selected, found, pointed, proved, cfg.RunConformance)
	if err := backend.RepointAgentImage(string(initial)); err != nil {
		return fmt.Errorf("start ward on writer image %s: %w", initial, err)
	}
	logger.Info("ward starts on writer image", "image", initial, "selected", selected.ImageRef, "proved", proved)
	return nil
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

// newRoutedReviewSource builds the routed review source on the provider the
// reviewer line selected. sourceConfig carries the Codex lifecycle; a Claude
// source runs on claudeLifecycle, the one the shadow source shares.
func newRoutedReviewSource(
	provider ward.RoutedReviewProvider, sourceConfig ward.CodexReviewSourceConfig,
	claudeLifecycle *ward.CodexReviewLifecycle,
) (exec.ReviewSource, error) {
	switch provider.Vendor {
	case domain.AgentVendorCodex:
		sourceConfig.Review.AuthRefresher = ward.NewCodexAuthHTTPRefresher()
		return ward.NewCodexReviewSource(sourceConfig)
	case domain.AgentVendorClaude:
		// A setup token has no refresh, so the source takes no refresher.
		sourceConfig.Lifecycle = claudeLifecycle
		return ward.NewClaudeReviewSource(sourceConfig)
	}
	return nil, fmt.Errorf("no routed review provider for vendor %q", provider.Vendor)
}

// requireApprovedReviewConfiguration re-gates the caller-selected review and
// exporter images before composition can construct a transport or execute the
// conformance runtime against repository content.
func requireApprovedReviewConfiguration(
	ctx context.Context, st *store.Store, cfg ClaudeDriverConfig,
) (domain.Digest, error) {
	if cfg.OperatingMode != domain.ModeUnattended {
		return "", nil
	}
	digest, err := ClaudeReviewConfigurationDigest(cfg)
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
		return "", fmt.Errorf("require approved review configuration: %w", err)
	}
	return digest, nil
}

// ClaudeReviewConfigurationDigest is the trust-profile digest of the routed
// review the reviewer line selected, Codex or Claude. The trust profile must
// approve it before composition goes on (requireApprovedReviewConfiguration),
// so flipping the line to another harness without re-approving refuses.
func ClaudeReviewConfigurationDigest(cfg ClaudeDriverConfig) (domain.Digest, error) {
	provider := cfg.ReviewProvider
	digest, err := provider.ConfigurationDigest(RoutedReviewConfig(provider, ward.CodexReviewConfig{
		InputRoot: cfg.ReviewInputRoot, WorkspaceTarget: "/workspace/project",
		ProviderEndpoints: RoutedReviewEndpoints(provider, cfg.ReviewAuthMode), ApprovedImage: cfg.ReviewImage,
		ObserverImage: cfg.ExporterImage, Model: cfg.ReviewModel,
		ReasoningEffort: cfg.ReviewReasoningEffort,
	}), cfg.ReviewWorkspaceSizeMB, cfg.ReviewAuthMode, cfg.ReviewAuthIdentityID, cfg.ReviewCostOwner)
	if err != nil {
		return "", fmt.Errorf("digest %s review configuration: %w", provider.HarnessClient, err)
	}
	return digest, nil
}

func requireApprovedShadowReviewConfiguration(
	ctx context.Context, st *store.Store, cfg ClaudeDriverConfig,
) (shadowReviewDigests, error) {
	if cfg.OperatingMode != domain.ModeUnattended || cfg.ShadowReviewImage == "" {
		return shadowReviewDigests{}, nil
	}
	digests, err := ShadowReviewConfigurationDigests(cfg)
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
			ctx, domain.ShadowReviewClaudeLocal, digests.Approval,
		)
	}); err != nil {
		return shadowReviewDigests{}, fmt.Errorf(
			"require approved Claude shadow review configuration: %w", err,
		)
	}
	return digests, nil
}

type shadowReviewDigests struct {
	Runtime  domain.Digest
	Approval domain.Digest
}

const shadowReviewCompositionDigestVersion = "freeside-shadow-review-composition/v1"

func ShadowReviewConfigurationDigests(cfg ClaudeDriverConfig) (shadowReviewDigests, error) {
	runtimeDigest, err := ward.ClaudeReviewConfigurationDigest(ward.CodexReviewConfig{
		InputRoot: cfg.ReviewInputRoot, WorkspaceTarget: "/workspace/project",
		ProviderEndpoints: []string{"api.anthropic.com:443"},
		ApprovedImage:     cfg.ShadowReviewImage, ObserverImage: cfg.ExporterImage,
		Model: cfg.ShadowReviewModel, ReasoningEffort: cfg.ShadowReviewReasoningEffort,
	}, cfg.ShadowReviewWorkspaceSizeMB, ward.CodexAuthSetupToken,
		cfg.ShadowReviewAuthIdentityID, cfg.ShadowReviewCostOwner)
	if err != nil {
		return shadowReviewDigests{}, fmt.Errorf(
			"digest Claude shadow review runtime configuration: %w", err,
		)
	}
	approvalDigest, err := ShadowReviewCompositionDigest(runtimeDigest, cfg.ShadowReviewRate)
	if err != nil {
		return shadowReviewDigests{}, err
	}
	return shadowReviewDigests{Runtime: runtimeDigest, Approval: approvalDigest}, nil
}

// ShadowReviewCompositionDigest extends ward's invocation-runtime digest with
// the deployment-owned sampling rate. The runtime digest remains the value the
// source and returned evidence re-authenticate; this outer digest is the exact
// independently owner-approved composition from #912.
func ShadowReviewCompositionDigest(runtimeDigest domain.Digest, rate float64) (domain.Digest, error) {
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
	return BindRigRuntimeResources(stateRoot, tokenFile, resources)
}

// releasePreJobContainer releases the invocation's pre-job container after
// Backend.PreJob has reaped it. Only the pre-job container: the handoff
// namespace bound beside it is released when the handoff's journal record
// closes. A start retry runs this hook again, so it binds the name again
// before the next probe. A failed release is logged and keeps the name.
func releasePreJobContainer(
	ctx context.Context, runtime ward.Runtime, release ward.RuntimeResourceReleaser,
	id domain.InvocationID, logger *slog.Logger,
) {
	if err := ward.ReleaseAbsentRuntimeResources(ctx, runtime, release, ward.RuntimeResourceNames{
		Containers: []string{ward.PreJobContainerNameForInvocation(id)},
	}); err != nil && logger != nil {
		logger.Warn("rig pre-job container name stays bound after a failed release",
			"invocation", string(id), "error", err)
	}
}

// ProductionRigRuntimeReleaser is the release half of
// ProductionRigRuntimeAuthorizer: nil without a rig token file, otherwise it
// unbinds names from the held rig. Callers reach it only through
// ward.ReleaseAbsentRuntimeResources (ward.RuntimeResourceReleaser).
func ProductionRigRuntimeReleaser(
	stateRoot, tokenFile string,
) ward.RuntimeResourceReleaser {
	if tokenFile == "" {
		return nil
	}
	return func(_ context.Context, resources ward.RuntimeResourceNames) error {
		token, err := ReadRigToken(tokenFile)
		if err != nil {
			return fmt.Errorf("read production rig token: %w", err)
		}
		if _, err := daemonlock.UnbindRigRuntimeResources(
			stateRoot, token, resources.Containers, resources.Volumes, resources.Networks,
		); err != nil {
			return fmt.Errorf("release production rig runtime resources: %w", err)
		}
		return nil
	}
}

func ProductionRigRuntimeAuthorizer(
	stateRoot, tokenFile string,
) ward.RuntimeResourceAuthorizer {
	if tokenFile == "" {
		return nil
	}
	return func(_ context.Context, resources ward.RuntimeResourceNames) error {
		return BindRigRuntimeResources(stateRoot, tokenFile, resources)
	}
}

func BindRigRuntimeResources(
	stateRoot, tokenFile string, resources ward.RuntimeResourceNames,
) error {
	if tokenFile == "" {
		return nil
	}
	token, err := ReadRigToken(tokenFile)
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

type StoreConformanceRecorder struct {
	Store *store.Store
}

func (r StoreConformanceRecorder) RecordBackendConformance(
	ctx context.Context, record domain.BackendConformance,
) error {
	return r.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(ctx, record)
		return err
	})
}

func runClaudeConformance(
	ctx context.Context,
	st *store.Store,
	transport *publish.Transport,
	backend *ward.Backend,
	cfg ClaudeDriverConfig,
	withStableCoverage func(func() error) error,
	leaser *AdapterConformanceLeaser,
) error {
	if backend != nil {
		if err := recoverAdapterConformance(ctx, st, backend, cfg, leaser); err != nil {
			return err
		}
	}
	// Full runs two synthetic handoffs, one per enforced egress profile. Each
	// is a run of its own, minted in the same conf-<16 hex> shape so both sets
	// of objects stay inside the namespace the production rig owns.
	var nonce, registryNonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("mint conformance run identity: %w", err)
	}
	if _, err := rand.Read(registryNonce[:]); err != nil {
		return fmt.Errorf("mint conformance registry run identity: %w", err)
	}
	runID := "conf-" + hex.EncodeToString(nonce[:])
	registryRunID := "conf-" + hex.EncodeToString(registryNonce[:])
	if err := BindRigRuntimeResources(
		cfg.StateDir, cfg.RigTokenFile,
		ward.FullConformanceRuntimeResourceNamesFor(runID, registryRunID),
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
	// The suite proves the image ward currently names, which follows writer
	// selection, not the configured seed.
	agentImage, _ := backend.ConformanceIdentity()
	suite, err := ward.NewSuite(backend, ward.SuiteFixture{ //nolint:gosec // G101: fixture carries an inert synthetic marker, never a credential
		AgentImage:       agentImage,
		CredentialTarget: "/var/lib/freeside/conformance-token",
		CredentialMarker: "FREESIDE_CONF_" + strings.ToUpper(hex.EncodeToString(nonce[:])),
		RunID:            runID,
		RegistryRunID:    registryRunID,
		Seed: ward.WorkspaceSeed{
			Mode:      ward.SeedBaseCheckout,
			SourceDir: seedDir,
			Base: domain.BaseRevision{
				Repo: cfg.Repo, RepositoryID: cfg.RepositoryID,
				BaseRef: cfg.BaseRef, BaseSHA: cfg.BaseSHA,
			},
		},
	}, ward.WithConformanceRecorder(StoreConformanceRecorder{Store: st}))
	if err != nil {
		return err
	}
	if err := suite.Full(ctx); err != nil {
		return err
	}
	// Only a passed ward suite reaches here, so no new adapter record rests
	// on a failed runner proof. A proof that no subscription can run (the
	// implementer runs another adapter, or every subscription that could run
	// its line is marked or expiring) records nothing, and the adapter's last
	// record stands across an image change: that record is scoped to the
	// harness build its digest names, not the image.
	return proveClaudeAdapter(ctx, st, backend, cfg, leaser)
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
//
// Three exceptions need no such change, and the engine applies them after
// this derivation. It admits a maintenance task at its refusal's tip, a
// handoff conflict's remediation at the tip its handoff check sealed (issue
// #2646), and a later return to the agent or remediation at that tip when
// its candidate was written there (issue #2699), instead of this base. Each
// tip comes from an immutable record, the refusal or the sealed
// handoff_conflict authority, so every attempt derives the same one.
func claudeAdmissionDerivation(cfg ClaudeDriverConfig) engine.AdmissionDerivation {
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
// lane's App-authority chain and janitor. Filing shares that authority but
// mints a separate, narrower token for its issue and identity calls, and so
// does the returned issue-read source the research fetcher authenticates with.
func claudeTransport(
	ctx context.Context,
	st *store.Store,
	cfg ClaudeDriverConfig,
	authority *publish.InstallationAuthorityStore,
) (*publish.Transport, *publish.Publisher, *publish.GitHubAppBotIdentityResolver, *publish.FollowUpFiler, *janitorSession, *publish.Reconciler, *publish.ExternalReviewReplier, publish.TokenSource, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	keystore, err := publish.NewKeystore(cfg.CredentialsDir, cfg.StateRoot)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	recorder, err := publish.NewStoreRecorder(st)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	janitor, err := publish.NewInstallationJanitor(
		keystore, client, DefaultGitHubAPIBase, authority, authority, recorder, recorder, time.Now,
		DefaultJanitorRemovalBound,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	trust, err := publish.NewStoreTrustSource(st)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	minter := publish.NewMinterWithJanitor(
		keystore, client, DefaultGitHubAPIBase, recorder, trust, time.Now, janitor,
	)
	tokens := publish.NewCachedTokenSource(minter, time.Now)
	commitAuthors, err := publish.NewGitHubAppBotIdentityResolver(
		tokens, keystore, client, DefaultGitHubAPIBase, time.Now,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	followUpFiler, err := composeFollowUpFiler(st, minter, keystore, client, DefaultGitHubAPIBase)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	reviewReplier, err := publish.NewExternalReviewReplier(st, tokens, client, DefaultGitHubAPIBase, commitAuthors, time.Now)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	transport, err := publish.NewTransport(
		tokens,
		publish.TransportOptions{RemoteBase: DefaultGitHubRemoteBase},
	)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	auditor, err := publish.NewGitHubWorkflowAuditor(
		tokens, client, DefaultGitHubAPIBase, time.Now,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	ledger, err := publish.NewStoreLedger(st)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	authorizations, err := publish.NewStoreAuthorizationSource(st)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	publisher := publish.NewPublisher(
		tokens, client, DefaultGitHubAPIBase, auditor, ledger, trust, authorizations,
	)
	if err := transport.AuthorizePublisher(publisher); err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	apps, err := keystore.ListApps()
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	if len(apps) == 0 {
		return nil, nil, nil, nil, nil, nil, nil, nil, publish.ErrNoAppCredentials
	}
	registrationIDs := make([]int64, 0, len(apps))
	for _, app := range apps {
		registrationIDs = append(registrationIDs, app.AppID)
	}
	session, err := startJanitorSession(ctx, janitor, registrationIDs)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, fmt.Errorf("start installation janitor: %w", err)
	}
	// Research reads the managed repository's issues on its own read-only
	// grant, never on the publication token.
	issueReadTokens := publish.NewIssueReadTokenSource(minter, time.Now)
	return transport, publisher, commitAuthors, followUpFiler, session,
		publish.NewReconciler(tokens, client, DefaultGitHubAPIBase), reviewReplier, issueReadTokens, nil
}

// composeFollowUpFiler keeps both issue calls and bot identity reads on the
// filing grant. Sharing the publication resolver would widen this path.
func composeFollowUpFiler(st *store.Store, minter *publish.Minter, keystore *publish.Keystore,
	client *http.Client, baseURL string,
) (*publish.FollowUpFiler, error) {
	tokens := publish.NewFollowUpFilingTokenSource(minter, time.Now)
	identity, err := publish.NewGitHubAppBotIdentityResolver(tokens, keystore, client, baseURL, time.Now)
	if err != nil {
		return nil, err
	}
	return publish.NewFollowUpFiler(st, tokens, client, baseURL, identity, time.Now)
}

// FollowUpFilerBinding is read after composition, when a committed approve wakes
// the filer. A fake-driver composition must return a genuinely nil interface.
func FollowUpFilerBinding(composition *ClaudeComposition) signet.FollowUpFiler {
	if composition == nil || composition.FollowUpFiler == nil {
		return nil
	}
	return composition.FollowUpFiler
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

type sessionGroup []SessionCloser

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
func (c *ClaudeComposition) stageDriver(blobs *signet.BlobStore) (exec.StageDriver, error) {
	materializer, err := ProductionMaterializer(blobs)
	if err != nil {
		return nil, fmt.Errorf("compose materializer: %w", err)
	}
	return exec.NewMaterializingStageDriver(materializer, c.driver)
}

func ProductionMaterializer(blobs *signet.BlobStore) (*exec.Materializer, error) {
	return exec.NewMaterializer(productionInputSource{blobs: blobs}, exec.MaterializerOptions{
		MaxInputBytes: exec.ProductionMaxInputBytes,
		MaxTotalBytes: exec.ProductionMaxTotalInputBytes,
	})
}

func ProductionSpecificationDeliveryValidator(
	materializer *exec.Materializer,
) func(context.Context, exec.StartSpec) error {
	return productionPromptDeliveryValidator(materializer, engine.ErrSpecificationInputUndeliverable)
}

func ProductionImplementationDeliveryValidator(
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

// refTipObserver reads a branch's tip SHA through the reconciler's
// conditional ref read. A 304 returns the cached SHA, so a caller that
// shares the cache must compare SHAs, not trust NotModified. An absent ref
// is an error: both watches need a tip to compare.
func refTipObserver(
	reconciler *publish.Reconciler,
) func(ctx context.Context, repo, branch string) (string, error) {
	return func(ctx context.Context, repo, branch string) (string, error) {
		obs, err := reconciler.ReconcileRef(ctx, repo, branch)
		if err != nil {
			return "", err
		}
		if !obs.Exists {
			return "", fmt.Errorf("ref %s/%s does not exist", repo, branch)
		}
		return obs.SHA, nil
	}
}
