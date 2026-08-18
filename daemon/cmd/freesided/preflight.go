package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/topicstore"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

const (
	compositionManifestVersion = "freeside-production-composition-v1"
	preflightMaxSeedEntries    = 100_000
)

var errCompositionPreflight = errors.New("production composition preflight failed")

type compositionStatus string

const (
	compositionPassed compositionStatus = "passed"
	compositionFailed compositionStatus = "failed"
	compositionNotRun compositionStatus = "not_run"
)

// AllCompositionStatuses is the complete manifest-status registration set.
var AllCompositionStatuses = []compositionStatus{
	compositionPassed,
	compositionFailed,
	compositionNotRun,
}

func (s compositionStatus) valid() bool {
	switch s {
	case compositionPassed, compositionFailed, compositionNotRun:
		return true
	default:
		return false
	}
}

type compositionCheck struct {
	Name        string            `json:"name"`
	Status      compositionStatus `json:"status"`
	Evidence    string            `json:"evidence"`
	Remediation string            `json:"remediation,omitempty"`
}

type compositionIdentity struct {
	SourceDigest               domain.Digest                 `json:"source_digest,omitempty"`
	PolicyDigest               domain.Digest                 `json:"policy_digest,omitempty"`
	PublicationDigest          domain.Digest                 `json:"publication_digest,omitempty"`
	WorkUnitDigest             domain.Digest                 `json:"work_unit_digest,omitempty"`
	ImplementationRunID        domain.RunID                  `json:"implementation_run_id,omitempty"`
	ImplementationInvocationID domain.InvocationID           `json:"implementation_invocation_id,omitempty"`
	CommitAuthor               engine.ProductionCommitAuthor `json:"-"`
}

type compositionImage struct {
	Role           string `json:"role"`
	Requested      string `json:"requested"`
	ResolvedDigest string `json:"resolved_digest,omitempty"`
}

type compositionManifest struct {
	Version                   string                 `json:"version"`
	Status                    compositionStatus      `json:"status"`
	Rig                       daemonlock.RigManifest `json:"rig"`
	DaemonBuild               string                 `json:"daemon_build"`
	ServerURL                 string                 `json:"server_url"`
	Repository                string                 `json:"repository"`
	RepositoryID              int64                  `json:"repository_id"`
	BaseRef                   string                 `json:"base_ref"`
	BaseSHA                   string                 `json:"base_sha"`
	ProfileDigest             domain.Digest          `json:"profile_digest,omitempty"`
	ReviewConfigurationDigest domain.Digest          `json:"review_configuration_digest,omitempty"`
	ReviewInstructionsPresent bool                   `json:"review_instructions_present"`
	ReviewInstructionsDigest  domain.Digest          `json:"review_instructions_digest,omitempty"`
	BuildEgressDigest         domain.Digest          `json:"build_egress_configuration_digest"`
	AllowedPaths              []string               `json:"allowed_paths"`
	ClaudeAuthIdentity        domain.AuthIdentityID  `json:"claude_auth_identity"`
	ClaudeAuthVolume          string                 `json:"claude_auth_volume"`
	CodexAuthIdentity         domain.AuthIdentityID  `json:"codex_auth_identity"`
	Identity                  compositionIdentity    `json:"identity"`
	Images                    []compositionImage     `json:"images"`
	Checks                    []compositionCheck     `json:"checks"`
}

type preflightConfig struct {
	DBPath                    string
	RigTokenFile              string
	ServerURL                 string
	ContainerBin              string
	AgentImage                string
	ExporterImage             string
	ReviewImage               string
	Repo                      string
	RepositoryCheckout        string
	RepositoryID              int64
	BaseRef                   string
	BaseSHA                   string
	ApprovedRecipe            domain.Digest
	AuthIdentityID            domain.AuthIdentityID
	AuthVolume                string
	ReviewInputRoot           string
	ReviewAuthMode            ward.CodexAuthMode
	ReviewAuthIdentityID      domain.AuthIdentityID
	ReviewAuthSnapshot        string
	ReviewInstructions        string
	PublicationStateDir       string
	PublicationCredentialsDir string
	ReviewModel               string
	ReviewReasoningEffort     string
	ReviewCostOwner           string
	ReviewWorkspaceSizeMB     int64
	SpecPath                  string
	PolicyPath                string
	PublicationPath           string
	WorkUnitPath              string
	ProjectID                 domain.ProjectID
	BuildProxy                string
	LaunchAgentLabel          string
	AllowedPaths              []string
	PublicationAuthor         engine.ProductionCommitAuthor
}

type databaseInspection struct {
	SchemaVersion           int
	ExpectedSchemaVersion   int
	ProfileDigest           domain.Digest
	ProfileRepo             string
	ProfileRepositoryID     int64
	OpenError               error
	ProfileError            error
	ReviewError             error
	CredentialError         error
	ReviewCredentialError   error
	ReviewReenrollmentError error
	ReviewAuthStoreVolume   string
	ReviewRefreshStrategy   domain.RefreshStrategy
	ProjectImage            domain.ProjectImage
	ProjectImageFound       bool
	ProjectImageError       error
}

type imageInspection struct {
	Digest string
	Error  error
}

type codexCredentialInspection struct {
	ResolvedPath string
	ExpiresAt    *time.Time
	Error        error
}

type repositoryAuthorityInspection struct {
	BaseError      error
	AuthorityError error
}

type preflightEnvironment interface {
	AuthenticateRig(string, string) (daemonlock.RigManifest, error)
	InspectDatabase(context.Context, preflightConfig, domain.Digest) databaseInspection
	InspectImage(context.Context, string, string, []string, string) imageInspection
	InspectCodexCredential(context.Context, preflightConfig, time.Time, bool) codexCredentialInspection
	CheckTopicKey(string) error
	CheckSeed(context.Context, string) error
	InspectRepositoryAuthority(context.Context, preflightConfig, time.Time) repositoryAuthorityInspection
	CheckAuthVolume(context.Context, string, string, string) error
	DatabaseIdle(string) error
	SupervisedDaemon(context.Context, string) (bool, error)
	ProbeDaemon(context.Context, string) (string, bool, error)
}

type productionPreflightEnvironment struct{ rig productionRigHost }

func newProductionPreflightEnvironment() productionPreflightEnvironment {
	return productionPreflightEnvironment{rig: newProductionRigHost()}
}

func runPreflightMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runPreflightCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided preflight:", err)
		os.Exit(1)
	}
}

func runPreflightCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runPreflightCommandWithEnvironment(
		ctx, args, stdout, stderr, newProductionPreflightEnvironment(), time.Now().UTC(), buildVersion(),
	)
}

func runPreflightCommandWithEnvironment(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	environment preflightEnvironment,
	now time.Time,
	daemonBuild string,
) error {
	cfg, err := parsePreflightConfig(args, stderr)
	if err != nil {
		return err
	}
	manifest := newCompositionManifest(cfg, daemonBuild, now)
	rig, rigErr := readRigAcquisition(cfg.RigTokenFile)
	if rigErr == nil {
		manifest.Rig, rigErr = environment.AuthenticateRig(rig.Manifest.Resources.StateRoot, rig.Token)
	}
	identity, identityErr := inspectCompositionIdentity(cfg)
	manifest.Identity = identity
	cfg.PublicationAuthor = identity.CommitAuthor
	reviewDigest, reviewDigestErr := reviewConfigurationDigest(cfg)
	if reviewDigestErr == nil {
		manifest.ReviewConfigurationDigest = reviewDigest
	}
	evaluateComposition(ctx, &manifest, cfg, environment, now, rigErr, identityErr, reviewDigestErr)
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode composition manifest: %w", err)
	}
	body = append(body, '\n')
	if _, err := stdout.Write(body); err != nil {
		return fmt.Errorf("write composition manifest: %w", err)
	}
	if manifest.Status == compositionFailed {
		return errCompositionPreflight
	}
	return nil
}

func parsePreflightConfig(args []string, stderr io.Writer) (preflightConfig, error) {
	flags := flag.NewFlagSet("freesided preflight", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var cfg preflightConfig
	flags.StringVar(&cfg.RigTokenFile, "rig-token-file", "", "rig hold acquisition JSON (required)")
	flags.StringVar(&cfg.ServerURL, "server-url", "", "operator client URL for the leased listener (required)")
	flags.StringVar(&cfg.ContainerBin, "container-bin", "container", "Apple container CLI path")
	flags.StringVar(&cfg.AgentImage, "agent-image", "", "digest-pinned implementer image (required)")
	flags.StringVar(&cfg.ExporterImage, "exporter-image", "", "digest-pinned exporter image (required)")
	flags.StringVar(&cfg.ReviewImage, "review-image", "", "digest-pinned reviewer image (required)")
	flags.StringVar(&cfg.Repo, "repo", "", "managed owner/name repository (required)")
	flags.StringVar(&cfg.RepositoryCheckout, "repository-checkout", "", "local checkout whose origin proves the managed base (required)")
	flags.Int64Var(&cfg.RepositoryID, "repository-id", 0, "canonical numeric repository id (required)")
	flags.StringVar(&cfg.BaseRef, "base-ref", "", "managed repository base branch (required)")
	flags.StringVar(&cfg.BaseSHA, "base-sha", "", "exact base commit (required)")
	flags.Func("approved-recipe", "approved verification recipe digest (required)", func(raw string) error {
		if cfg.ApprovedRecipe != "" {
			return errors.New("-approved-recipe may be supplied once")
		}
		cfg.ApprovedRecipe = domain.Digest(raw)
		return nil
	})
	flags.StringVar((*string)(&cfg.AuthIdentityID), "auth-identity", "", "Claude auth identity id (required)")
	flags.StringVar(&cfg.AuthVolume, "auth-volume", "", "Claude credential volume (required)")
	flags.StringVar(&cfg.ReviewInputRoot, "review-input-root", "", "private review input root (required)")
	flags.StringVar((*string)(&cfg.ReviewAuthMode), "review-auth-mode", "", "review auth mode (required)")
	flags.StringVar((*string)(&cfg.ReviewAuthIdentityID), "review-auth-identity", "", "review auth identity id (required)")
	flags.StringVar(&cfg.ReviewAuthSnapshot, "review-auth-snapshot", "", "review auth snapshot under input root (required)")
	flags.StringVar(&cfg.ReviewInstructions, "review-instructions", "", "review host instructions file (required)")
	flags.StringVar(&cfg.PublicationStateDir, "publication-state-dir", "", "publication state directory (required)")
	flags.StringVar(&cfg.PublicationCredentialsDir, "publication-credentials-dir", "", "publication credentials directory (required)")
	flags.StringVar(&cfg.ReviewModel, "review-model", "", "pinned review model (required)")
	flags.StringVar(&cfg.ReviewReasoningEffort, "review-reasoning-effort", "", "review reasoning effort (required)")
	flags.StringVar(&cfg.ReviewCostOwner, "review-cost-owner", "", "review cost owner (required)")
	flags.Int64Var(&cfg.ReviewWorkspaceSizeMB, "review-workspace-size-mb", 8192, "review workspace size")
	flags.StringVar(&cfg.SpecPath, "spec", "", "source specification file (required)")
	flags.StringVar(&cfg.PolicyPath, "policy", "", "resolved policy file (required)")
	flags.StringVar(&cfg.PublicationPath, "publication", "", "publication metadata file (required)")
	flags.StringVar(&cfg.WorkUnitPath, "work-unit", "", "work-unit declaration file (optional)")
	flags.StringVar((*string)(&cfg.ProjectID), "project", "", "project id (required)")
	flags.StringVar(&cfg.BuildProxy, "build-proxy", "", "optional supported image-build proxy URL")
	flags.StringVar(&cfg.LaunchAgentLabel, "launch-agent-label", defaultRigLaunchAgentLabel, "supervised daemon label")
	flags.Func("allowed-paths", "comma-separated explicit production path allowlist (required)", func(raw string) error {
		cfg.AllowedPaths = splitNonEmpty(raw)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return preflightConfig{}, err
	}
	if flags.NArg() != 0 {
		return preflightConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if cfg.RigTokenFile == "" || cfg.ServerURL == "" || cfg.AgentImage == "" ||
		cfg.ExporterImage == "" || cfg.ReviewImage == "" || cfg.Repo == "" || cfg.RepositoryCheckout == "" ||
		cfg.RepositoryID <= 0 || cfg.BaseRef == "" || cfg.BaseSHA == "" ||
		cfg.ApprovedRecipe == "" || cfg.AuthIdentityID == "" || cfg.AuthVolume == "" || cfg.ReviewInputRoot == "" ||
		cfg.ReviewAuthMode == "" || cfg.ReviewAuthIdentityID == "" ||
		cfg.ReviewAuthSnapshot == "" || cfg.ReviewInstructions == "" || len(cfg.AllowedPaths) == 0 ||
		cfg.PublicationStateDir == "" || cfg.PublicationCredentialsDir == "" ||
		cfg.ReviewModel == "" ||
		cfg.ReviewReasoningEffort == "" || cfg.ReviewCostOwner == "" ||
		cfg.ReviewWorkspaceSizeMB <= 0 || cfg.SpecPath == "" || cfg.PolicyPath == "" ||
		cfg.PublicationPath == "" || cfg.ProjectID == "" {
		return preflightConfig{}, errors.New("all production composition flags except -work-unit and -build-proxy are required")
	}
	return cfg, nil
}

func newCompositionManifest(cfg preflightConfig, daemonBuild string, now time.Time) compositionManifest {
	names := []string{
		"rig_manifest", "state_database", "topic_key", "daemon_build",
		"listener_server_url", "daemon_conflict", "repository_base", "trust_profile",
		"review_configuration", "source_implementation_identity", "review_instructions", "seed_root",
		"publication_authority",
		"exporter_image", "implementer_image", "reviewer_image", "claude_credentials",
		"codex_credentials", "build_egress_configuration", "build_egress_reachability",
	}
	checks := make([]compositionCheck, 0, len(names))
	for _, name := range names {
		checks = append(checks, compositionCheck{Name: name, Status: compositionNotRun, Evidence: "not evaluated"})
	}
	return compositionManifest{
		Version: compositionManifestVersion, Status: compositionPassed,
		DaemonBuild: daemonBuild, ServerURL: cfg.ServerURL,
		Repository: cfg.Repo, RepositoryID: cfg.RepositoryID, BaseRef: cfg.BaseRef, BaseSHA: cfg.BaseSHA,
		BuildEgressDigest:  domain.Digest(contentaddr.Sum([]byte(cfg.BuildProxy))),
		AllowedPaths:       slices.Clone(cfg.AllowedPaths),
		ClaudeAuthIdentity: cfg.AuthIdentityID, ClaudeAuthVolume: cfg.AuthVolume,
		CodexAuthIdentity: cfg.ReviewAuthIdentityID,
		Images: []compositionImage{
			{Role: "exporter", Requested: cfg.ExporterImage},
			{Role: "implementer", Requested: cfg.AgentImage},
			{Role: "reviewer", Requested: cfg.ReviewImage},
		},
		Checks: checks,
	}
}

func evaluateComposition(
	ctx context.Context,
	manifest *compositionManifest,
	cfg preflightConfig,
	environment preflightEnvironment,
	now time.Time,
	rigErr, identityErr, reviewDigestErr error,
) {
	var database databaseInspection
	daemonIdle := false
	if rigErr != nil {
		failCheck(manifest, "rig_manifest", "rig acquisition could not be read and authenticated", "reacquire the production rig lease (#796)")
	} else {
		resources := manifest.Rig.Resources
		cfg.DBPath = resources.DatabasePath
		if !canonicalAbsolute(resources.StateRoot) || !canonicalAbsolute(resources.DatabasePath) ||
			!canonicalAbsolute(resources.SeedRoot) {
			failCheck(manifest, "rig_manifest", "rig resources are not canonical absolute paths", "release and reacquire the production rig lease (#796)")
		} else {
			passCheck(manifest, "rig_manifest", "rig manifest binds canonical state, database, listener, and seed resources")
		}
	}

	if cfg.DBPath == "" {
		for _, check := range []string{
			"state_database", "topic_key", "daemon_conflict", "trust_profile",
			"review_configuration", "claude_credentials",
		} {
			notRunCheck(manifest, check, "rig database path was unavailable")
		}
	} else {
		database = environment.InspectDatabase(ctx, cfg, manifest.ReviewConfigurationDigest)
		if database.OpenError != nil {
			failCheck(manifest, "state_database", "database is absent, unreadable, or not at this binary's schema", "restore or migrate the production database with the supported daemon")
		} else {
			passCheck(manifest, "state_database", fmt.Sprintf(
				"canonical database schema version %d matches binary schema version %d",
				database.SchemaVersion, database.ExpectedSchemaVersion,
			))
		}
		if err := environment.CheckTopicKey(cfg.DBPath); err != nil {
			failCheck(manifest, "topic_key", "topic key is missing or invalid", "restore the private topic key beside the database (#521)")
		} else {
			passCheck(manifest, "topic_key", "private topic key is present and valid")
		}
		if database.OpenError != nil {
			for _, check := range []string{"trust_profile", "review_configuration", "claude_credentials"} {
				notRunCheck(manifest, check, "database inspection did not complete")
			}
		} else {
			if database.ProfileError != nil {
				failCheck(manifest, "trust_profile", "target repository has no matching readable active trust profile", "rerun onboarding and approve the current repository trust profile")
			} else {
				manifest.ProfileDigest = database.ProfileDigest
				passCheck(manifest, "trust_profile", fmt.Sprintf(
					"active profile %s binds repository %s (%d)",
					database.ProfileDigest, database.ProfileRepo, database.ProfileRepositoryID,
				))
			}
			if reviewDigestErr != nil || database.ReviewError != nil {
				failCheck(manifest, "review_configuration", "effective reviewer configuration does not match every active profile", "converge and approve the reviewer configuration (#532)")
			} else {
				passCheck(manifest, "review_configuration", "effective reviewer configuration digest is approved")
			}
		}
		daemonIdle = evaluateDaemonConflict(ctx, manifest, cfg, environment)
	}
	var repositoryInspection repositoryAuthorityInspection
	repositoryInspectionNotRun := false
	if err := validateCompositionRepositoryBase(cfg); err != nil {
		repositoryInspection = repositoryAuthorityInspection{BaseError: err, AuthorityError: err}
	} else if cfg.DBPath == "" || database.OpenError != nil || !daemonIdle {
		err := errors.New("production database and daemon must be idle for authenticated repository observation")
		repositoryInspection = repositoryAuthorityInspection{BaseError: err, AuthorityError: err}
		repositoryInspectionNotRun = true
	} else {
		repositoryInspection = environment.InspectRepositoryAuthority(ctx, cfg, now)
	}
	if repositoryInspectionNotRun {
		notRunCheck(manifest, "repository_base", "authenticated repository observation requires the production audit database")
	} else if repositoryInspection.BaseError != nil {
		failCheck(manifest, "repository_base", "repository, live branch ancestry, base, or approved recipe is invalid", "fetch the canonical GitHub base branch into the managed checkout and select a reachable full base SHA and approved recipe")
	} else {
		passCheck(manifest, "repository_base", "repository, live branch ancestry, exact base commit, and approved recipe are canonical")
	}

	if daemonBuild := manifest.DaemonBuild; daemonBuild == "" || daemonBuild == "devel" || daemonBuild == "(devel)" ||
		strings.HasSuffix(daemonBuild, "-dirty") {
		failCheck(manifest, "daemon_build", "daemon build has no immutable build identity", "rebuild freesided with its source commit in the version ldflag")
	} else {
		passCheck(manifest, "daemon_build", "daemon build identity is "+daemonBuild)
	}
	if rigErr != nil {
		notRunCheck(manifest, "listener_server_url", "rig listener was unavailable")
	} else if want := "http://" + manifest.Rig.Resources.ListenAddress; cfg.ServerURL != want || !validServerURL(cfg.ServerURL) {
		failCheck(manifest, "listener_server_url", "operator server URL does not exactly name the leased listener", "set the client server URL to http://<leased-listener>")
	} else {
		passCheck(manifest, "listener_server_url", "operator server URL exactly names the leased listener")
	}

	if identityErr != nil {
		failCheck(manifest, "source_implementation_identity", "submission inputs could not be interpreted canonically", "correct the specification, policy, publication, or work-unit input")
	} else {
		passCheck(manifest, "source_implementation_identity", fmt.Sprintf(
			"source %s deterministically reserves run %s and invocation %s",
			manifest.Identity.SourceDigest,
			manifest.Identity.ImplementationRunID,
			manifest.Identity.ImplementationInvocationID,
		))
	}

	if instructions, err := engine.SnapshotReviewHostInstructions(
		ctx, cfg.ReviewInstructions, cfg.ReviewAuthSnapshot,
	); err != nil {
		failCheck(manifest, "review_instructions", "review host instructions are absent, unreadable, or invalid", "provide a stable review-instructions file outside the credential snapshot")
	} else {
		manifest.ReviewInstructionsPresent = instructions.Present
		manifest.ReviewInstructionsDigest = instructions.Digest
		passCheck(manifest, "review_instructions", "review host instructions can be snapshotted before submission")
	}
	if repositoryInspectionNotRun {
		notRunCheck(manifest, "publication_authority", "authenticated publication authority observation requires the production audit database")
	} else if repositoryInspection.AuthorityError != nil {
		failCheck(manifest, "publication_authority", "publication authority, credentials, or claimed App bot identity are unavailable or inconsistent", "restore the repository-bound GitHub App authority and canonical bot attribution before submission")
	} else {
		passCheck(manifest, "publication_authority", "publication authority and claimed App bot identity match the repository-bound registration")
	}

	if rigErr != nil {
		notRunCheck(manifest, "seed_root", "rig seed root was unavailable")
	} else if err := environment.CheckSeed(ctx, manifest.Rig.Resources.SeedRoot); err != nil {
		failCheck(manifest, "seed_root", "canonical seed root is dirty or unreadable", "restore the exact-base clean seed checkout (#781)")
	} else {
		passCheck(manifest, "seed_root", "canonical seed root is clean")
	}

	imageChecks := []struct {
		check       string
		role        string
		ref         string
		tools       []string
		versionTool string
		index       int
	}{
		{"exporter_image", "exporter", cfg.ExporterImage, ward.RequiredObserverTools(), "", 0},
		{"implementer_image", "implementer", cfg.AgentImage, []string{"sh", "git", "claude"}, "claude", 1},
		{"reviewer_image", "reviewer", cfg.ReviewImage, []string{"sh", "git", "codex"}, "codex", 2},
	}
	for _, image := range imageChecks {
		if err := domain.ImageRef(image.ref).Validate(); err != nil || strings.HasPrefix(image.ref, "-") {
			failCheck(manifest, image.check, image.role+" image reference is not a canonical digest pin", "rebuild and re-pin the "+image.role+" image")
			continue
		}
		if image.role == "implementer" {
			if database.OpenError != nil {
				notRunCheck(manifest, image.check, "project-image provenance could not be read from the production database")
				continue
			}
			if err := validatePreflightProjectImage(database.ProjectImage, database.ProjectImageFound, cfg); err != nil || database.ProjectImageError != nil {
				failCheck(manifest, image.check, "implementer image has no matching approved project-image provenance", "build and record an image for this repository, base, recipe, and approved preparation command")
				continue
			}
		}
		inspection := environment.InspectImage(ctx, cfg.ContainerBin, image.ref, image.tools, image.versionTool)
		manifest.Images[image.index].ResolvedDigest = inspection.Digest
		expected := imageDigestFromRef(image.ref)
		if inspection.Error != nil || expected == "" || inspection.Digest != expected {
			failCheck(manifest, image.check, image.role+" image digest or required executable capabilities are stale", "rebuild and re-pin the "+image.role+" image")
		} else {
			passCheck(manifest, image.check, fmt.Sprintf(
				"%s image resolved exact digest %s and passed required tool probes",
				image.role, inspection.Digest,
			))
		}
	}

	if cfg.DBPath == "" || database.OpenError != nil {
		notRunCheck(manifest, "claude_credentials", "database inspection did not complete")
	} else if database.CredentialError != nil || environment.CheckAuthVolume(
		ctx, cfg.ContainerBin, cfg.AuthVolume, cfg.ExporterImage,
	) != nil {
		failCheck(manifest, "claude_credentials", "Claude auth identity or setup-token credential manifest is absent or invalid", "record a lease-enabled Claude identity and restore its setup-token volume before production submission")
	} else {
		passCheck(manifest, "claude_credentials", "Claude auth identity and setup-token credential manifest are ready")
	}

	if cfg.DBPath == "" || database.OpenError != nil {
		notRunCheck(manifest, "codex_credentials", "database inspection did not complete")
	} else if credential := environment.InspectCodexCredential(
		ctx, cfg, now, database.ReviewRefreshStrategy == domain.RefreshOnDemand,
	); credential.Error != nil || database.ReviewCredentialError != nil ||
		database.ReviewReenrollmentError != nil ||
		(cfg.ReviewAuthMode == ward.CodexAuthSubscription &&
			database.ReviewAuthStoreVolume != credential.ResolvedPath) {
		failCheck(manifest, "codex_credentials", "Codex auth identity or credential snapshot is absent, invalid, or below its lifetime floor", "refresh or re-enroll the Codex credential (#599)")
	} else if credential.ExpiresAt == nil {
		passCheck(manifest, "codex_credentials", "Codex API-key snapshot is ready; no expiry is declared")
	} else {
		passCheck(manifest, "codex_credentials", "Codex access snapshot is ready until "+credential.ExpiresAt.UTC().Format(time.RFC3339))
	}

	if err := projectimage.ValidateBuildProxy(cfg.BuildProxy); err != nil {
		failCheck(manifest, "build_egress_configuration", "configured project-image build proxy has an unsupported shape", "configure an unauthenticated HTTP build proxy (#519)")
	} else if cfg.BuildProxy == "" {
		passCheck(manifest, "build_egress_configuration", "direct project-image build egress is selected")
	} else {
		passCheck(manifest, "build_egress_configuration", "supported host-only HTTP build proxy is configured")
	}
	notRunCheck(manifest, "build_egress_reachability", "live build-egress reachability is intentionally not exercised by this read-only preflight")
}

func validateCompositionRepositoryBase(cfg preflightConfig) error {
	if err := publish.ValidateRepository(cfg.Repo); err != nil {
		return err
	}
	if err := publish.ValidateBranchName(cfg.BaseRef); err != nil {
		return err
	}
	if err := publish.ValidateCommitSHA(cfg.BaseSHA); err != nil {
		return err
	}
	if !contentaddr.Valid(string(cfg.ApprovedRecipe)) {
		return errors.New("approved recipe is not a canonical digest")
	}
	return nil
}

func validatePreflightProjectImage(
	image domain.ProjectImage,
	found bool,
	cfg preflightConfig,
) error {
	if _, err := resolveProjectImagePreparation(image, found, claudeDriverConfig{
		AgentImage: domain.ImageRef(cfg.AgentImage), Repo: cfg.Repo,
		RepositoryID: cfg.RepositoryID, BaseSHA: cfg.BaseSHA,
	}); err != nil {
		return err
	}
	if image.RecipeDigest != cfg.ApprovedRecipe {
		return fmt.Errorf("project-image recipe %s disagrees with approved recipe %s: %w",
			image.RecipeDigest, cfg.ApprovedRecipe, ErrProjectImageComposition)
	}
	return nil
}

func evaluateDaemonConflict(
	ctx context.Context,
	manifest *compositionManifest,
	cfg preflightConfig,
	environment preflightEnvironment,
) bool {
	if err := environment.DatabaseIdle(cfg.DBPath); err != nil {
		failCheck(manifest, "daemon_conflict", "production database is already held by a daemon", "stop the conflicting daemon before submission (#796)")
		return false
	} else if supervised, err := environment.SupervisedDaemon(ctx, cfg.LaunchAgentLabel); err != nil {
		failCheck(manifest, "daemon_conflict", "supervised-daemon state could not be inspected", "repair launchd inspection before submission (#796)")
		return false
	} else if supervised {
		failCheck(manifest, "daemon_conflict", "a supervised daemon is loaded", "stop the supervised daemon before submission (#796)")
		return false
	} else if description, live, err := environment.ProbeDaemon(ctx, manifest.Rig.Resources.ListenAddress); err != nil {
		failCheck(manifest, "daemon_conflict", "leased listener could not be probed", "repair listener access before submission (#796)")
		return false
	} else if live {
		failCheck(manifest, "daemon_conflict", "leased listener is occupied by "+description, "stop the process occupying the leased listener (#796)")
		return false
	} else {
		passCheck(manifest, "daemon_conflict", "database, supervisor, and leased listener are idle")
		return true
	}
}

func inspectCompositionIdentity(cfg preflightConfig) (compositionIdentity, error) {
	spec, err := readSubmissionFile(cfg.SpecPath)
	if err != nil {
		return compositionIdentity{}, err
	}
	policyFile, err := readSubmissionFile(cfg.PolicyPath)
	if err != nil {
		return compositionIdentity{}, err
	}
	if err := ward.RejectDuplicateJSONKeys(policyFile.body); err != nil {
		return compositionIdentity{}, err
	}
	var keys []domain.PolicyKey
	if err := strictjson.Decode(
		policyFile.body, &keys, strictjson.TolerateInvalidUTF8, strictjson.Limit(maxSubmissionFileBytes),
	); err != nil {
		return compositionIdentity{}, err
	}
	policyDigest, err := (domain.ResolvedPolicy{Keys: keys}).ComputeDigest()
	if err != nil {
		return compositionIdentity{}, err
	}
	publicationFile, err := readSubmissionFile(cfg.PublicationPath)
	if err != nil {
		return compositionIdentity{}, err
	}
	if err := ward.RejectDuplicateJSONKeys(publicationFile.body); err != nil {
		return compositionIdentity{}, err
	}
	var publication engine.ProductionPublication
	if err := strictjson.Decode(
		publicationFile.body, &publication,
		strictjson.TolerateInvalidUTF8, strictjson.Limit(maxSubmissionFileBytes),
	); err != nil {
		return compositionIdentity{}, err
	}
	if err := publication.Validate(); err != nil {
		return compositionIdentity{}, err
	}
	publicationBody, err := json.Marshal(publication)
	if err != nil {
		return compositionIdentity{}, err
	}
	publicationDigest := publicationFile.digest
	publicationIdentityDigest := submissionBytes(publicationBody).digest
	var (
		workUnit       *domain.WorkUnitDeclarationInput
		workUnitDigest domain.Digest
	)
	if cfg.WorkUnitPath != "" {
		workUnitFile, err := readSubmissionFile(cfg.WorkUnitPath)
		if err != nil {
			return compositionIdentity{}, err
		}
		if err := ward.RejectDuplicateJSONKeys(workUnitFile.body); err != nil {
			return compositionIdentity{}, err
		}
		var declared submittedWorkUnit
		if err := strictjson.Decode(
			workUnitFile.body, &declared,
			strictjson.TolerateInvalidUTF8, strictjson.Limit(maxSubmissionFileBytes),
		); err != nil {
			return compositionIdentity{}, err
		}
		slices.Sort(declared.DependsOnIssues)
		declared.DependsOnIssues = slices.Compact(declared.DependsOnIssues)
		workUnit = &domain.WorkUnitDeclarationInput{
			CompletionCriterion: declared.CompletionCriterion,
			BoundIssue:          declared.BoundIssue,
			DependsOnIssues:     declared.DependsOnIssues,
			DeclaredPaths:       declaredPathScope(keys),
			ContractSerialized:  declared.ContractSerialized,
		}
		canonicalBody, err := json.Marshal(declared)
		if err != nil {
			return compositionIdentity{}, err
		}
		workUnitDigest = submissionBytes(canonicalBody).digest
	}
	runID := defaultSubmissionRunID(
		cfg.ProjectID, spec.digest, policyDigest, publicationIdentityDigest, workUnitDigest,
	)
	elaborationRunID, err := engine.ElaborationRunIDForImplementation(runID)
	if err != nil {
		return compositionIdentity{}, err
	}
	resolvedPolicy, err := domain.NewResolvedPolicy(elaborationRunID, keys)
	if err != nil {
		return compositionIdentity{}, err
	}
	if err := submittedPathBoundary(resolvedPolicy); err != nil {
		return compositionIdentity{}, err
	}
	if _, err := resolvedPathAllowlist(resolvedPolicy, resolvedPolicy.Digest, cfg.AllowedPaths); err != nil {
		return compositionIdentity{}, err
	}
	if workUnit != nil {
		if _, err := domain.NewWorkUnitDeclaration(
			*workUnit, runID, cfg.ProjectID, time.Unix(0, 0).UTC(),
		); err != nil {
			return compositionIdentity{}, err
		}
	}
	return compositionIdentity{
		SourceDigest: spec.digest, PolicyDigest: policyDigest,
		PublicationDigest: publicationDigest, WorkUnitDigest: workUnitDigest,
		ImplementationRunID:        runID,
		ImplementationInvocationID: domain.InvocationID("inv-implement-" + string(runID)),
		CommitAuthor:               publication.CommitAuthor,
	}, nil
}

func inspectRepositoryAuthority(
	ctx context.Context, cfg preflightConfig, now time.Time,
) repositoryAuthorityInspection {
	localErr := inspectLocalRepositoryBase(ctx, cfg)
	authorityStore, err := publish.NewInstallationAuthorityStore(cfg.PublicationStateDir)
	if err != nil {
		return repositoryAuthorityInspection{BaseError: errors.Join(localErr, err), AuthorityError: err}
	}
	keystore, err := publish.NewKeystore(cfg.PublicationCredentialsDir, cfg.PublicationStateDir)
	if err != nil {
		return repositoryAuthorityInspection{BaseError: errors.Join(localErr, err), AuthorityError: err}
	}
	apps, err := keystore.ListApps()
	if err != nil || len(apps) == 0 {
		authorityErr := errors.New("no usable GitHub App credentials")
		return repositoryAuthorityInspection{BaseError: errors.Join(localErr, authorityErr), AuthorityError: authorityErr}
	}
	var selected *publish.AppCredentials
	for _, app := range apps {
		authority, err := authorityStore.InstallationAuthority(ctx, app.AppID)
		if err != nil {
			return repositoryAuthorityInspection{BaseError: errors.Join(localErr, err), AuthorityError: err}
		}
		allows, err := publish.InstallationAuthorityAllowsRepository(
			app, authority, cfg.RepositoryID, now,
		)
		if err != nil {
			return repositoryAuthorityInspection{BaseError: errors.Join(localErr, err), AuthorityError: err}
		}
		if !allows {
			continue
		}
		if selected != nil {
			authorityErr := errors.New("multiple GitHub Apps have authority for the target repository")
			return repositoryAuthorityInspection{BaseError: errors.Join(localErr, authorityErr), AuthorityError: authorityErr}
		}
		candidate := app
		selected = &candidate
	}
	if selected == nil {
		authorityErr := errors.New("no registered GitHub App has authority for the target repository")
		return repositoryAuthorityInspection{BaseError: errors.Join(localErr, authorityErr), AuthorityError: authorityErr}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resolved, err := publish.InspectAppBotIdentity(
		ctx, *selected, client, defaultGitHubAPIBase,
	)
	if err != nil {
		return repositoryAuthorityInspection{BaseError: errors.Join(localErr, err), AuthorityError: err}
	}
	if resolved.AppSlug != cfg.PublicationAuthor.AppSlug ||
		resolved.BotUserID != cfg.PublicationAuthor.BotUserID {
		return repositoryAuthorityInspection{
			BaseError:      errors.Join(localErr, publish.ErrAppBotIdentityMismatch),
			AuthorityError: publish.ErrAppBotIdentityMismatch,
		}
	}
	if localErr != nil {
		return repositoryAuthorityInspection{BaseError: localErr}
	}
	if cfg.DBPath == "" {
		authorityErr := errors.New("production database is unavailable for installation-token audit")
		return repositoryAuthorityInspection{BaseError: errors.Join(localErr, authorityErr), AuthorityError: authorityErr}
	}
	lock, err := daemonlock.Acquire(cfg.DBPath)
	if err != nil {
		return repositoryAuthorityInspection{BaseError: err, AuthorityError: err}
	}
	defer lock.Close() //nolint:errcheck // acquisition succeeded; inspection errors remain primary
	st, err := store.OpenExisting(ctx, cfg.DBPath, store.Options{
		ApprovedRecipes: map[domain.Digest]bool{cfg.ApprovedRecipe: true},
	})
	if err != nil {
		return repositoryAuthorityInspection{BaseError: errors.Join(localErr, err), AuthorityError: err}
	}
	defer st.Close() //nolint:errcheck // the mint audit transaction owns its durability result
	recorder, err := publish.NewStoreRecorder(st)
	if err != nil {
		return repositoryAuthorityInspection{BaseError: errors.Join(localErr, err), AuthorityError: err}
	}
	clock := func() time.Time { return now }
	minter := publish.NewMinter(keystore, client, defaultGitHubAPIBase, recorder, nil, clock)
	tokens := publish.NewAuthorityInspectionTokenSource(
		minter, authorityStore, selected.AppID, cfg.RepositoryID,
	)
	transport, err := publish.NewTransport(
		tokens, publish.TransportOptions{GitPath: "git", RemoteBase: defaultGitHubRemoteBase},
	)
	if err != nil {
		return repositoryAuthorityInspection{BaseError: errors.Join(localErr, err), AuthorityError: err}
	}
	scratch, err := os.MkdirTemp("", "freeside-preflight-base-")
	if err != nil {
		return repositoryAuthorityInspection{BaseError: errors.Join(localErr, err)}
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort cleanup of preflight-owned observation state
	checkout, err := transport.FetchBase(
		ctx, cfg.Repo, cfg.BaseRef, cfg.BaseSHA, filepath.Join(scratch, "checkout"),
	)
	if err != nil || checkout.RepositoryID() != cfg.RepositoryID {
		if err == nil {
			err = domain.ErrRepositoryIdentityMismatch
		}
		return repositoryAuthorityInspection{BaseError: err, AuthorityError: err}
	}
	return repositoryAuthorityInspection{BaseError: localErr}
}

func inspectLocalRepositoryBase(ctx context.Context, cfg preflightConfig) error {
	if !canonicalAbsolute(cfg.RepositoryCheckout) {
		return errors.New("repository checkout is not a canonical absolute path")
	}
	remote, err := runPreflightGit(ctx, cfg.RepositoryCheckout, "remote", "get-url", "origin")
	if err != nil || !githubRemoteMatchesRepository(strings.TrimSpace(string(remote)), cfg.Repo) {
		return errors.New("repository checkout origin does not match the managed repository")
	}
	resolved, err := runPreflightGit(
		ctx, cfg.RepositoryCheckout, "rev-parse", "--verify", cfg.BaseSHA+"^{commit}",
	)
	if err != nil || strings.TrimSpace(string(resolved)) != cfg.BaseSHA {
		return errors.New("managed base commit is absent from the local object graph")
	}
	return nil
}

func reviewConfigurationDigest(cfg preflightConfig) (domain.Digest, error) {
	if err := ward.ValidateCodexReviewModelConfiguration(
		cfg.ReviewModel, cfg.ReviewReasoningEffort,
	); err != nil {
		return "", err
	}
	endpoints := []string{"chatgpt.com:443"}
	if cfg.ReviewAuthMode == ward.CodexAuthAPIKey {
		endpoints = []string{"api.openai.com:443"}
	}
	return ward.CodexReviewConfigurationDigest(ward.CodexReviewConfig{
		InputRoot: cfg.ReviewInputRoot, WorkspaceTarget: "/workspace/project",
		ProviderEndpoints: endpoints, ApprovedImage: cfg.ReviewImage,
		ObserverImage: cfg.ExporterImage, Model: cfg.ReviewModel,
		ReasoningEffort:          cfg.ReviewReasoningEffort,
		AccessTokenLifetimeFloor: time.Hour, AccessTokenRefreshThreshold: 2 * time.Hour,
	}, cfg.ReviewWorkspaceSizeMB, cfg.ReviewAuthMode, cfg.ReviewAuthIdentityID, cfg.ReviewCostOwner)
}

func (productionPreflightEnvironment) InspectDatabase(
	ctx context.Context, cfg preflightConfig, reviewDigest domain.Digest,
) databaseInspection {
	inspection := databaseInspection{}
	st, err := store.OpenReadOnly(ctx, cfg.DBPath, store.Options{
		ApprovedRecipes: map[domain.Digest]bool{cfg.ApprovedRecipe: true},
	})
	if err != nil {
		inspection.OpenError = err
		return inspection
	}
	defer st.Close() //nolint:errcheck // inspection errors are reported independently
	inspection.SchemaVersion, inspection.OpenError = st.SchemaVersion(ctx)
	inspection.ExpectedSchemaVersion, err = store.CurrentSchemaVersion()
	if inspection.OpenError == nil {
		inspection.OpenError = err
	}
	if inspection.OpenError != nil {
		return inspection
	}
	err = st.Read(ctx, func(tx *store.ReadTx) error {
		profiles, err := tx.InspectLatestTrustProfiles(ctx)
		if err != nil {
			inspection.ProfileError = err
		} else {
			for _, current := range profiles {
				if current.Repo != cfg.Repo {
					continue
				}
				if current.ReconstructionError != nil || current.Profile.RepositoryID != cfg.RepositoryID {
					inspection.ProfileError = errors.Join(
						current.ReconstructionError, domain.ErrRepositoryIdentityMismatch,
					)
					break
				}
				inspection.ProfileDigest = current.Profile.ProfileDigest
				inspection.ProfileRepo = current.Profile.Repo
				inspection.ProfileRepositoryID = current.Profile.RepositoryID
			}
			if inspection.ProfileDigest == "" && inspection.ProfileError == nil {
				inspection.ProfileError = errors.New("target repository has no active profile")
			}
		}
		inspection.ReviewError = tx.RequireReviewConfigurationApproved(ctx, reviewDigest)
		identity, err := tx.GetAuthIdentity(ctx, cfg.AuthIdentityID)
		if err != nil || identity.Provider != "claude" || identity.AuthStoreVolume != cfg.AuthVolume ||
			!identity.AuthStoreMutationLease {
			inspection.CredentialError = errors.Join(err, errors.New("identity cannot support the leased Claude auth store"))
		}
		if cfg.ReviewAuthMode == ward.CodexAuthSubscription {
			reviewIdentity, err := tx.GetAuthIdentity(ctx, cfg.ReviewAuthIdentityID)
			inspection.ReviewAuthStoreVolume = reviewIdentity.AuthStoreVolume
			inspection.ReviewRefreshStrategy = reviewIdentity.RefreshStrategy
			if err != nil || reviewIdentity.ID != cfg.ReviewAuthIdentityID ||
				reviewIdentity.Provider != "openai" || !reviewIdentity.AuthStoreMutationLease ||
				!reviewIdentity.SupportsReadOnlyAuthSnapshot {
				inspection.ReviewCredentialError = errors.Join(
					err, errors.New("identity cannot support lease-held Codex auth snapshot refresh"),
				)
			}
		}
		inspection.ProjectImage, inspection.ProjectImageFound, inspection.ProjectImageError = tx.GetProjectImageByRef(ctx, domain.ImageRef(cfg.AgentImage))
		return nil
	})
	if err != nil {
		inspection.OpenError = err
		return inspection
	}
	if cfg.ReviewAuthMode == ward.CodexAuthSubscription && inspection.ReviewCredentialError == nil {
		adapters, err := wardstore.New(st)
		if err != nil {
			inspection.ReviewReenrollmentError = err
		} else if needs, err := adapters.AuthState.NeedsCodexAuthReenrollment(
			ctx, cfg.ReviewAuthIdentityID,
		); err != nil || needs {
			inspection.ReviewReenrollmentError = errors.Join(
				err, errors.New("codex auth identity has an active re-enrollment hold"),
			)
		}
	}
	return inspection
}

func (productionPreflightEnvironment) AuthenticateRig(stateRoot, token string) (daemonlock.RigManifest, error) {
	return daemonlock.AuthenticateRig(stateRoot, token)
}

func (productionPreflightEnvironment) InspectImage(
	ctx context.Context, containerBin, ref string, tools []string, versionTool string,
) imageInspection {
	digest, err := projectimage.InspectImageDigest(ctx, containerBin, ref)
	if err != nil {
		return imageInspection{Error: err}
	}
	if err := probeImageTools(ctx, containerBin, ref, tools, versionTool); err != nil {
		return imageInspection{Digest: digest, Error: err}
	}
	return imageInspection{Digest: digest}
}

func (productionPreflightEnvironment) InspectCodexCredential(
	_ context.Context, cfg preflightConfig, now time.Time, refreshOnDemand bool,
) codexCredentialInspection {
	resolvedPath, expiresAt, err := ward.InspectCodexAuthReadiness(
		cfg.ReviewInputRoot, cfg.ReviewAuthSnapshot, cfg.ReviewAuthMode,
		cfg.ReviewAuthIdentityID, now,
		time.Hour, 2*time.Hour, refreshOnDemand,
	)
	return codexCredentialInspection{
		ResolvedPath: resolvedPath, ExpiresAt: expiresAt, Error: err,
	}
}

func (productionPreflightEnvironment) CheckTopicKey(dbPath string) error {
	return topicstore.InspectKey(dbPath)
}

func (productionPreflightEnvironment) CheckSeed(ctx context.Context, seedRoot string) error {
	return ward.CheckCanonicalSeedWorkspaceClean(ctx, seedRoot, preflightMaxSeedEntries)
}

func (productionPreflightEnvironment) InspectRepositoryAuthority(
	ctx context.Context, cfg preflightConfig, now time.Time,
) repositoryAuthorityInspection {
	return inspectRepositoryAuthority(ctx, cfg, now)
}

func runPreflightGit(ctx context.Context, checkout string, args ...string) ([]byte, error) {
	argv := append([]string{"-C", checkout}, args...)
	command := osexec.CommandContext(ctx, "git", argv...) //nolint:gosec // G204: fixed git verb argv carries validated repo coordinates as separate opaque arguments
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
	stdout := &preflightOutput{max: 64 << 10}
	stderr := &preflightOutput{max: 64 << 10}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := procbound.Run(command, procbound.DefaultWaitDelay); err != nil {
		return nil, err
	}
	if stdout.truncated || stderr.truncated {
		return nil, errors.New("git observation exceeded limit")
	}
	return stdout.buf.Bytes(), nil
}

func githubRemoteMatchesRepository(remote, repo string) bool {
	wantPath := repo + ".git"
	if scpHost, scpPath, ok := strings.Cut(remote, ":"); ok && !strings.Contains(remote, "://") {
		user, host, ok := strings.Cut(scpHost, "@")
		return ok && user == "git" && githubHost(host) && strings.TrimSuffix(scpPath, ".git") == repo
	}
	parsed, err := url.Parse(remote)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "ssh") || !githubHost(parsed.Hostname()) ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme == "https" && parsed.User != nil {
		return false
	}
	if parsed.Scheme == "ssh" && (parsed.User == nil || parsed.User.String() != "git") {
		return false
	}
	path := strings.TrimPrefix(parsed.EscapedPath(), "/")
	return path == wantPath || path == repo
}

func githubHost(host string) bool {
	return host == "github.com" || strings.HasSuffix(host, ".github.com")
}

func (productionPreflightEnvironment) CheckAuthVolume(
	ctx context.Context, containerBin, name, exporterImage string,
) error {
	runtime := ward.NewCLIRuntime(containerBin)
	volumes, err := runtime.ListVolumes(ctx)
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		if volume.Name == name {
			return ward.InspectCredentialVolumeManifest(
				ctx, runtime, exporterImage, name, ward.CredentialManifestSetupToken,
			)
		}
	}
	return errors.New("credential volume is absent")
}

func (productionPreflightEnvironment) DatabaseIdle(dbPath string) error {
	lock, err := daemonlock.Acquire(dbPath)
	if err != nil {
		return err
	}
	return lock.Close()
}

func (e productionPreflightEnvironment) SupervisedDaemon(
	ctx context.Context, label string,
) (bool, error) {
	return e.rig.SupervisedDaemon(ctx, label)
}

func (e productionPreflightEnvironment) ProbeDaemon(
	ctx context.Context, address string,
) (string, bool, error) {
	return e.rig.ProbeDaemon(ctx, address)
}

func probeImageTools(
	ctx context.Context, containerBin, ref string, tools []string, versionTool string,
) error {
	const script = `version_tool=$1
shift
missing=0
for tool do
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf 'freeside-missing-tool:%s\n' "$tool"
    missing=1
  fi
done
if [ "$missing" -ne 0 ]; then exit 127; fi
if [ "$version_tool" != - ]; then "$version_tool" --version >/dev/null 2>&1; fi`
	if versionTool == "" {
		versionTool = "-"
	}
	args := []string{
		"run", "--rm", "--network", "none", "--", ref,
		"sh", "-c", script, "sh", versionTool,
	}
	args = append(args, tools...)
	command := osexec.CommandContext(ctx, containerBin, args...) //nolint:gosec // executable is operator-selected; fixed arguments keep image input opaque
	output := &preflightOutput{max: 64 << 10}
	command.Stdout = output
	command.Stderr = output
	if err := procbound.Run(command, procbound.DefaultWaitDelay); err != nil {
		return fmt.Errorf("image capability probe failed: %w", err)
	}
	if output.truncated {
		return errors.New("image capability probe output exceeded limit")
	}
	return nil
}

type preflightOutput struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (o *preflightOutput) Write(p []byte) (int, error) {
	n := len(p)
	if o.buf.Len() >= o.max {
		o.truncated = true
		return n, nil
	}
	remaining := o.max - o.buf.Len()
	if len(p) > remaining {
		o.truncated = true
		p = p[:remaining]
	}
	_, _ = o.buf.Write(p)
	return n, nil
}

func imageDigestFromRef(ref string) string {
	_, digest, ok := strings.Cut(ref, "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return ""
	}
	for _, r := range digest[len("sha256:"):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return digest
}

func validServerURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "http" && parsed.Host != "" && parsed.User == nil &&
		parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func canonicalAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func passCheck(manifest *compositionManifest, name, evidence string) {
	setCheck(manifest, name, compositionPassed, evidence, "")
}

func failCheck(manifest *compositionManifest, name, evidence, remediation string) {
	manifest.Status = compositionFailed
	setCheck(manifest, name, compositionFailed, evidence, remediation)
}

func notRunCheck(manifest *compositionManifest, name, evidence string) {
	setCheck(manifest, name, compositionNotRun, evidence, "")
}

func setCheck(
	manifest *compositionManifest,
	name string,
	status compositionStatus,
	evidence, remediation string,
) {
	if !status.valid() {
		panic("invalid composition status " + status)
	}
	for i := range manifest.Checks {
		if manifest.Checks[i].Name == name {
			manifest.Checks[i].Status = status
			manifest.Checks[i].Evidence = evidence
			manifest.Checks[i].Remediation = remediation
			return
		}
	}
	panic("unknown composition check " + name)
}
