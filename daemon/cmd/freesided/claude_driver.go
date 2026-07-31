package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
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
	ProviderEndpoints []string
	// PromptPackageFile is the trusted prompt-package file. The daemon
	// ingests its bytes into the artifact blob store and derives the digest,
	// rather than taking a digest on trust: the materializer resolves that
	// digest from the blob store at every dispatch, so a configured digest
	// whose bytes were never stored fails every start.
	PromptPackageFile  string
	VendorInstructions string
	Repo               string
	RepositoryID       int64
	BaseRef            string
	BaseSHA            string
	AuthIdentityID     domain.AuthIdentityID
	AllowedPaths       []string
	// RunConformance executes the store-backed full ward suite against this
	// exact runtime/image/configuration before the engine can admit work.
	RunConformance bool
	// StateRoot and CredentialsDir locate the GitHub App authority and
	// credential material the exact-base transport authenticates with.
	StateRoot      string
	CredentialsDir string
	OperatingMode  domain.OperatingMode
}

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
	case c.StateRoot == "" || c.CredentialsDir == "":
		return fmt.Errorf("-publication-state-dir and -publication-credentials-dir are required in claude driver mode")
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

// unattendedCapabilitySnapshot is the same floor as a durable snapshot, for
// the store policy that re-gates every recorded admission.
func unattendedCapabilitySnapshot() domain.CapabilitySnapshot {
	return exec.NewCapabilitySet(unattendedAdmissionFloor...).Snapshot()
}

// ingestPromptPackage stores the prompt package's bytes and returns their
// content address, so the digest the admission records and the bytes the
// materializer resolves are the same object by construction.
func ingestPromptPackage(blobs *signet.BlobStore, path string) (domain.Digest, error) {
	body, err := os.ReadFile(path) //nolint:gosec // G304: operator-configured control-plane prompt package
	if err != nil {
		return "", fmt.Errorf("read prompt package: %w", err)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("prompt package %s is empty", path)
	}
	sum := sha256.Sum256(body)
	digest := domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
	if _, err := blobs.Put(digest, bytes.NewReader(body)); err != nil {
		return "", fmt.Errorf("store prompt package: %w", err)
	}
	return digest, nil
}

// unattendedAdmissionFloor is the §5.7 minimum an unattended dispatch is
// admitted against: the three handoff capabilities plus the two the full
// conformance suite earns (the networkless exporter boundary and the
// enforced provider-egress writer boundary). It mirrors ward's own
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
type exportRecorder struct{ store *store.Store }

func (r exportRecorder) RecordExecutionExport(ctx context.Context, record domain.ExecutionExport) error {
	return r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordExecutionExport(ctx, record)
	})
}

// storeAdmissionAuthority binds private driver state back to the durable
// admission and its exact trust profile. The state file is replay data, not
// an authorization source.
type storeAdmissionAuthority struct {
	store        *store.Store
	allowedPaths []string
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
			// has restarted under a current proof for B.
			if err := tx.RequireBackendConformant(ctx, admission); err != nil {
				return err
			}
			policy, err := tx.GetResolvedPolicy(ctx, admission.RunID)
			if err != nil {
				return err
			}
			allowedPaths, err = resolvedPathAllowlist(
				policy, admission.PolicyDigest, a.allowedPaths,
			)
			return err
		}
		return nil
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

func (a storeAdmissionAuthority) AuthenticateAdmission(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	_, _, err := a.admission(ctx, id, spec, false)
	return err
}

func (a storeAdmissionAuthority) AuthenticateStart(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	_, _, err := a.admission(ctx, id, spec, true)
	return err
}

func (a storeAdmissionAuthority) ImportOptions(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec, opts importer.Options,
) (importer.Options, error) {
	admission, allowedPaths, err := a.admission(ctx, id, spec, true)
	if err != nil {
		return importer.Options{}, err
	}
	if admission.TrustProfileDigest == nil {
		return importer.Options{}, fmt.Errorf(
			"admission %s carries no trust profile: %w",
			admission.ID, domain.ErrEmptyField)
	}
	var profile domain.AutomationTrustProfile
	err = a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		profile, err = tx.GetTrustProfile(ctx, *admission.TrustProfileDigest)
		return err
	})
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
	return opts, nil
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
	return r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
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

// artifactStore persists released evidence bytes and the immutable artifact
// rows the imported claims name. Persisting the complete labeled AgentClaim on
// an invocation-bound review surface is tracked in #381; an artifact row alone
// is not that claim record.
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
				ID: claim.Artifact, Type: "evidence", Digest: claim.Digest,
				Provenance: claim.Provenance,
			}, nil)
			if err != nil {
				return fmt.Errorf("agent claim %q: %w", claim.Label, err)
			}
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return fmt.Errorf("persist agent claim %q for %s: %w", claim.Label, id, err)
			}
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
// pass for this exact composition with -run-conformance; without that flag,
// only a matching prior pass is restored and missing evidence fails closed.
type claudeComposition struct {
	driver  *claude.Driver
	backend *ward.Backend
	env     engine.AdmissionEnvironment
	derive  engine.AdmissionDerivation
	closer  sessionCloser
	janitor *janitorSession
}

// composeClaudeDriver builds the production ward gate and Claude driver.
// Nothing here reaches the network or the runtime; the caller runs the
// conformance suite and the driver's restart reconciliation before the
// engine loop starts.
func composeClaudeDriver(
	ctx context.Context, st *store.Store, blobs *signet.BlobStore, cfg claudeDriverConfig,
) (_ *claudeComposition, err error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	promptPackage, err := ingestPromptPackage(blobs, cfg.PromptPackageFile)
	if err != nil {
		return nil, err
	}
	transport, janitor, err := claudeTransport(ctx, st, cfg)
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
	backend, backendErr := ward.New(ward.NewCLIRuntime(cfg.ContainerBin), ward.Config{
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
	})
	if backendErr != nil {
		return nil, fmt.Errorf("compose ward backend: %w", backendErr)
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
		if err := runClaudeConformance(ctx, st, transport, backend, cfg); err != nil {
			return nil, fmt.Errorf("run ward conformance: %w", err)
		}
	} else if haveConformance &&
		conformance.Outcome == domain.ConformancePassed &&
		conformance.ConfigurationDigest == backend.ConfigurationDigest() {
		if err := backend.RestoreConformance(conformance); err != nil {
			return nil, err
		}
	}

	driver, driverErr := claude.New(claude.Config{
		Lifetime:   ctx,
		Dir:        filepath.Join(cfg.StateDir, "claude-driver"),
		SeedRoot:   cfg.SeedRoot,
		ExportRoot: filepath.Join(cfg.StateDir, "ward-exports"),
		Gate:       backend,
		Seeder:     transportSeeder{transport: transport},
		Exports:    exportRecorder{store: st},
		Outcomes:   exportRecorder{store: st},
		Authority: storeAdmissionAuthority{
			store: st, allowedPaths: slices.Clone(cfg.AllowedPaths),
		},
		Artifacts: artifactStore{blobs: blobs, store: st},
		Volumes:   adapters.Leaser,
		PreJob: func(ctx context.Context, id domain.InvocationID) error {
			sum := sha256.Sum256([]byte(id))
			return backend.PreJob(ctx, hex.EncodeToString(sum[:8]))
		},
		Import: importer.Options{
			Policy: importer.Policy{Allowlist: cfg.AllowedPaths},
		},
		Now: func() time.Time { return time.Now().UTC() },
	})
	if driverErr != nil {
		return nil, fmt.Errorf("compose claude driver: %w", driverErr)
	}

	identity := cfg.AuthIdentityID
	env := engine.AdmissionEnvironment{
		OperatingMode:       cfg.OperatingMode,
		CredentialMode:      domain.CredentialSubscriptionContained,
		EgressProfile:       domain.EgressProviderOnly,
		ImageRef:            cfg.AgentImage,
		PromptPackageDigest: promptPackage,
		VendorInstructions: engine.VendorInstructionConfig{
			Vendor: domain.AgentVendorClaude, HostPath: cfg.VendorInstructions,
		},
		// Base and Workspace are per-attempt and supplied by derive below;
		// the static values here would be wrong the moment a second work item
		// is submitted.
		AuthIdentityID: &identity,
	}
	composition := &claudeComposition{
		driver: driver, backend: backend,
		env:     env,
		derive:  claudeAdmissionDerivation(cfg),
		closer:  sessionGroup{driver, janitor},
		janitor: janitor,
	}
	return composition, nil
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
) error {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("mint conformance run identity: %w", err)
	}
	runID := "conf-" + hex.EncodeToString(nonce[:])
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
	if err := (transportSeeder{transport: transport}).FetchBaseWorktree(
		ctx, cfg.Repo, cfg.BaseRef, cfg.BaseSHA, seedDir,
	); err != nil {
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
) (*publish.Transport, *janitorSession, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	keystore, err := publish.NewKeystore(cfg.CredentialsDir, cfg.StateRoot)
	if err != nil {
		return nil, nil, err
	}
	authority, err := publish.NewInstallationAuthorityStore(cfg.StateRoot)
	if err != nil {
		return nil, nil, err
	}
	janitor, err := publish.NewInstallationJanitor(
		keystore, client, defaultGitHubAPIBase, authority, authority, time.Now,
		defaultJanitorRemovalBound,
	)
	if err != nil {
		return nil, nil, err
	}
	recorder, err := publish.NewStoreRecorder(st)
	if err != nil {
		return nil, nil, err
	}
	trust, err := publish.NewStoreTrustSource(st)
	if err != nil {
		return nil, nil, err
	}
	minter := publish.NewMinterWithJanitor(
		keystore, client, defaultGitHubAPIBase, recorder, trust, time.Now, janitor,
	)
	transport, err := publish.NewTransport(
		publish.NewCachedTokenSource(minter, time.Now),
		publish.TransportOptions{RemoteBase: defaultGitHubRemoteBase},
	)
	if err != nil {
		return nil, nil, err
	}
	apps, err := keystore.ListApps()
	if err != nil {
		return nil, nil, err
	}
	if len(apps) == 0 {
		return nil, nil, publish.ErrNoAppCredentials
	}
	registrationIDs := make([]int64, 0, len(apps))
	for _, app := range apps {
		registrationIDs = append(registrationIDs, app.AppID)
	}
	session, err := startJanitorSession(
		ctx, janitor, registrationIDs, defaultJanitorInterval,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("start installation janitor: %w", err)
	}
	return transport, session, nil
}

const janitorStartupTimeout = 2 * time.Minute

type janitorRunner interface {
	Run(context.Context, time.Duration) error
	ActiveFor(int64) bool
}

type janitorSession struct {
	cancel   context.CancelFunc
	finished chan struct{}
	stopOnce sync.Once
	runErr   error
}

func startJanitorSession(
	parent context.Context,
	janitor janitorRunner,
	registrationIDs []int64,
	interval time.Duration,
) (*janitorSession, error) {
	if janitor == nil {
		return nil, errors.New("nil installation janitor")
	}
	if len(registrationIDs) == 0 {
		return nil, publish.ErrNoAppCredentials
	}
	runCtx, cancel := context.WithCancel(parent)
	session := &janitorSession{cancel: cancel, finished: make(chan struct{})}
	go func() {
		session.runErr = janitor.Run(runCtx, interval)
		close(session.finished)
	}()

	startupCtx, stopWaiting := context.WithTimeout(parent, janitorStartupTimeout)
	defer stopWaiting()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		active := true
		for _, registrationID := range registrationIDs {
			if !janitor.ActiveFor(registrationID) {
				active = false
				break
			}
		}
		if active {
			return session, nil
		}
		select {
		case <-session.finished:
			if session.runErr != nil {
				return nil, session.runErr
			}
			return nil, errors.New("installation janitor stopped before publishing coverage")
		case <-startupCtx.Done():
			cancel()
			<-session.finished
			return nil, errors.Join(
				fmt.Errorf("installation janitor did not publish coverage: %w", startupCtx.Err()),
				session.runErr,
			)
		case <-ticker.C:
		}
	}
}

func (s *janitorSession) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.stopOnce.Do(s.cancel)
	select {
	case <-s.finished:
		return s.runErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *janitorSession) Result() error {
	if s == nil {
		return nil
	}
	<-s.finished
	return s.runErr
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
	materializer, err := exec.NewMaterializer(productionInputSource{blobs: blobs}, exec.MaterializerOptions{
		MaxInputBytes: 4 << 20, MaxTotalBytes: 32 << 20,
	})
	if err != nil {
		return nil, fmt.Errorf("compose materializer: %w", err)
	}
	return exec.NewMaterializingStageDriver(materializer, c.driver)
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
