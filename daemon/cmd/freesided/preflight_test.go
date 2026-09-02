package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type fakePreflightEnvironment struct {
	rig                  daemonlock.RigManifest
	database             databaseInspection
	imageErrors          map[string]error
	imageCalls           map[string]int
	topicError           error
	seedError            error
	authVolumeError      error
	authVolumeCalls      int
	authVolumeAuthorizer ward.RuntimeResourceAuthorizer
	repositoryBaseError  error
	authorityError       error
	repositoryCalls      int
	idleError            error
	supervised           bool
	live                 bool
	codexError           error
	codexExpiresAt       *time.Time
	codexCalls           int
	publicationAuthor    engine.ProductionCommitAuthor
}

func (e *fakePreflightEnvironment) AuthenticateRig(string, string) (daemonlock.RigManifest, error) {
	return e.rig, nil
}

func (e *fakePreflightEnvironment) InspectDatabase(
	context.Context, preflightConfig, domain.Digest,
) databaseInspection {
	return e.database
}

func (e *fakePreflightEnvironment) InspectImage(
	_ context.Context, _ string, ref string, _ []string, versionTool string,
) imageInspection {
	role := versionTool
	if role == "" {
		role = "exporter"
	}
	e.imageCalls[role]++
	return imageInspection{Digest: imageDigestFromRef(ref), Error: e.imageErrors[role]}
}

func (e *fakePreflightEnvironment) InspectCodexCredential(
	_ context.Context, cfg preflightConfig, _ time.Time, _ bool,
) codexCredentialInspection {
	e.codexCalls++
	return codexCredentialInspection{
		ResolvedPath: cfg.ReviewAuthSnapshot, ExpiresAt: e.codexExpiresAt, Error: e.codexError,
	}
}

func (e *fakePreflightEnvironment) CheckTopicKey(string) error { return e.topicError }

func (e *fakePreflightEnvironment) CheckSeed(context.Context, string) error {
	return e.seedError
}

func (e *fakePreflightEnvironment) InspectRepositoryAuthority(
	_ context.Context, cfg preflightConfig, _ time.Time,
) repositoryAuthorityInspection {
	e.repositoryCalls++
	e.publicationAuthor = cfg.PublicationAuthor
	return repositoryAuthorityInspection{
		BaseError: e.repositoryBaseError, AuthorityError: e.authorityError,
	}
}

func (e *fakePreflightEnvironment) CheckAuthVolume(
	_ context.Context, _, _, _ string, authorize ward.RuntimeResourceAuthorizer,
) error {
	e.authVolumeCalls++
	e.authVolumeAuthorizer = authorize
	return e.authVolumeError
}

func (e *fakePreflightEnvironment) DatabaseIdle(string) error { return e.idleError }

func (e *fakePreflightEnvironment) SupervisedDaemon(context.Context, string) (bool, error) {
	return e.supervised, nil
}

func (e *fakePreflightEnvironment) ProbeDaemon(
	context.Context, string,
) (string, bool, error) {
	return "test process", e.live, nil
}

func TestPreflightManifestGolden(t *testing.T) {
	args, environment := preflightFixture(t)
	var stdout, stderr bytes.Buffer
	if err := runPreflightCommandWithEnvironment(
		t.Context(), args, &stdout, &stderr, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23",
	); err != nil {
		t.Fatalf("preflight: %v; stderr=%s\nmanifest=%s", err, stderr.String(), stdout.String())
	}
	golden.Assert(t, "preflight-manifest", stdout.Bytes())
	if environment.publicationAuthor.AppSlug != "freeside-test" ||
		environment.publicationAuthor.BotUserID != 12345 {
		t.Fatalf("publication author = %+v", environment.publicationAuthor)
	}
}

func TestPreflightCoversEnabledShadowReviewConfiguration(t *testing.T) {
	args, environment := preflightFixture(t)
	setupToken := enableShadowReviewFixture(t, &args)
	var stdout, stderr bytes.Buffer
	if err := runPreflightCommandWithEnvironment(
		t.Context(), args, &stdout, &stderr, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23",
	); err != nil {
		t.Fatalf("preflight: %v; stderr=%s\nmanifest=%s", err, stderr.String(), stdout.String())
	}
	var manifest compositionManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ShadowReviewConfigurationDigest == "" ||
		checkStatus(manifest, "shadow_review_configuration") != compositionPassed ||
		checkStatus(manifest, "shadow_reviewer_image") != compositionPassed {
		t.Fatalf("shadow preflight manifest = %+v", manifest)
	}
	if strings.Contains(stdout.String(), "fixture-token") || strings.Contains(stdout.String(), setupToken) {
		t.Fatal("shadow credential material or host path leaked into the manifest")
	}
}

func TestPreflightRejectsMalformedShadowReviewCredential(t *testing.T) {
	args, environment := preflightFixture(t)
	setupToken := enableShadowReviewFixture(t, &args)
	if err := os.WriteFile(setupToken, []byte("invalid\ntoken"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := runPreflightCommandWithEnvironment(
		t.Context(), args, &stdout, &stderr, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23",
	)
	if !errors.Is(err, errCompositionPreflight) {
		t.Fatalf("preflight error = %v; stderr=%s", err, stderr.String())
	}
	var manifest compositionManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if checkStatus(manifest, "shadow_review_configuration") != compositionFailed {
		t.Fatalf("shadow review configuration check = %+v", manifest.Checks)
	}
}

func TestPreflightShadowApprovalPrecedesProtectedAccess(t *testing.T) {
	args, environment := preflightFixture(t)
	enableShadowReviewFixture(t, &args)
	environment.database.ShadowReviewError = domain.ErrShadowReviewConfigUnapproved
	var stdout, stderr bytes.Buffer
	err := runPreflightCommandWithEnvironment(
		t.Context(), args, &stdout, &stderr, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23",
	)
	if !errors.Is(err, errCompositionPreflight) {
		t.Fatalf("preflight error = %v; stderr=%s", err, stderr.String())
	}
	var manifest compositionManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if checkStatus(manifest, "shadow_review_configuration") != compositionFailed ||
		checkStatus(manifest, "repository_base") != compositionNotRun ||
		checkStatus(manifest, "shadow_reviewer_image") != compositionNotRun ||
		checkStatus(manifest, "claude_credentials") != compositionNotRun ||
		checkStatus(manifest, "codex_credentials") != compositionNotRun {
		t.Fatalf("shadow approval ordering checks = %+v", manifest.Checks)
	}
	if environment.repositoryCalls != 0 || environment.authVolumeCalls != 0 ||
		environment.codexCalls != 0 {
		t.Fatalf("protected access calls: repository=%d auth-volume=%d codex=%d",
			environment.repositoryCalls, environment.authVolumeCalls, environment.codexCalls)
	}
	for role, calls := range environment.imageCalls {
		if calls != 0 {
			t.Fatalf("image probe %s called %d times before shadow approval", role, calls)
		}
	}
}

func TestProductionPreflightDatabaseStopsAtShadowApproval(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	profile := approveShadowReviewProfile(t)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(
			ctx, profile, time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC),
		)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := preflightConfig{
		DBPath: dbPath, Repo: profile.Repo, RepositoryID: profile.RepositoryID,
		ApprovedRecipe: domain.Digest("sha256:" + strings.Repeat("7", 64)),
		AuthIdentityID: "missing-claude", ReviewAuthMode: ward.CodexAuthSubscription,
		ReviewAuthIdentityID: "missing-codex",
		ShadowReviewImage:    "ghcr.io/x/claude@sha256:" + strings.Repeat("6", 64),
		ExporterImage:        "ghcr.io/x/exporter@sha256:" + strings.Repeat("5", 64),
		ReviewInputRoot:      "/var/freeside/review-inputs",
		ShadowReviewModel:    "claude-opus", ShadowReviewReasoningEffort: "high",
		ShadowReviewCostOwner: "subscription:shadow", ShadowReviewWorkspaceSizeMB: 4096,
		ShadowReviewRate: 0.2,
	}
	inspection := (productionPreflightEnvironment{}).InspectDatabase(
		ctx, cfg, profile.Review.ConfigDigest,
	)
	if !errors.Is(inspection.ShadowReviewError, domain.ErrShadowReviewConfigUnapproved) {
		t.Fatalf("shadow approval error = %v", inspection.ShadowReviewError)
	}
	assertNoPostShadowApprovalDatabaseProbes(t, inspection)

	shadowDigest, err := preflightShadowReviewConfigurationDigest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewShadowReviewConfigurationApproval(
		domain.ShadowReviewConfigurationApprovalInput{
			Repo: profile.Repo, RepositoryID: profile.RepositoryID,
			Source: domain.ShadowReviewClaudeLocal, ConfigurationDigest: shadowDigest,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	st, err = store.OpenExisting(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		now := time.Date(2026, 8, 25, 20, 1, 0, 0, time.UTC)
		if err := tx.RecordInactiveShadowReviewConfigurationApproval(ctx, approval, now); err != nil {
			return err
		}
		return tx.ActivateShadowReviewConfigurationApproval(
			ctx, approval.Repo, approval.Source, approval.ApprovalDigest, now,
		)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	wrongIdentity := cfg
	wrongIdentity.RepositoryID++
	inspection = (productionPreflightEnvironment{}).InspectDatabase(
		ctx, wrongIdentity, profile.Review.ConfigDigest,
	)
	if !errors.Is(inspection.ShadowReviewError, domain.ErrShadowReviewConfigUnapproved) ||
		!errors.Is(inspection.ShadowReviewError, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("target identity shadow approval error = %v", inspection.ShadowReviewError)
	}
	assertNoPostShadowApprovalDatabaseProbes(t, inspection)
}

func assertNoPostShadowApprovalDatabaseProbes(t *testing.T, inspection databaseInspection) {
	t.Helper()
	if inspection.ShadowReviewAuthorized || inspection.CredentialError != nil ||
		inspection.ReviewCredentialError != nil ||
		inspection.ReviewReenrollmentError != nil || inspection.ProjectImageError != nil ||
		inspection.ProjectImageFound {
		t.Fatalf("post-approval database probes ran: %+v", inspection)
	}
}

func TestPreflightRejectsShadowCredentialAsReviewInstructions(t *testing.T) {
	args, environment := preflightFixture(t)
	setupToken := enableShadowReviewFixture(t, &args)
	for i := range args {
		if args[i] == "-review-instructions" {
			args[i+1] = setupToken
			break
		}
	}
	var stdout, stderr bytes.Buffer
	err := runPreflightCommandWithEnvironment(
		t.Context(), args, &stdout, &stderr, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23",
	)
	if !errors.Is(err, errCompositionPreflight) {
		t.Fatalf("preflight error = %v; stderr=%s", err, stderr.String())
	}
	var manifest compositionManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if checkStatus(manifest, "review_instructions") != compositionFailed {
		t.Fatalf("review instructions check = %+v", manifest.Checks)
	}
}

func enableShadowReviewFixture(t *testing.T, args *[]string) string {
	t.Helper()
	var inputRoot string
	for i, arg := range *args {
		if arg == "-review-input-root" {
			inputRoot = (*args)[i+1]
			break
		}
	}
	if inputRoot == "" {
		t.Fatal("fixture has no review input root")
	}
	setupToken := filepath.Join(inputRoot, "claude-token")
	if err := os.WriteFile(setupToken, []byte("fixture-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	image := "registry.test/claude@sha256:" + strings.Repeat("a", 64)
	*args = append(*args,
		"-shadow-review-image", image,
		"-shadow-review-auth-snapshot", setupToken,
		"-shadow-review-model", "claude-opus",
		"-shadow-review-reasoning-effort", "high",
		"-shadow-review-cost-owner", "subscription:shadow",
		"-shadow-review-workspace-size-mb", "4096",
		"-shadow-review-rate", "0.4",
	)
	return setupToken
}

// TestPreflightCredentialCheckReceivesRigAuthorizer proves the credential check
// is handed a real rig authorizer (not nil), so the observer name is bound into
// the held manifest before creation. The authorizer's binding behavior is
// proven in TestProductionRigRuntimeAuthorizerBindsCredentialObserver.
func TestPreflightCredentialCheckReceivesRigAuthorizer(t *testing.T) {
	args, environment := preflightFixture(t)
	var stdout, stderr bytes.Buffer
	if err := runPreflightCommandWithEnvironment(
		t.Context(), args, &stdout, &stderr, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23",
	); err != nil {
		t.Fatalf("preflight: %v; stderr=%s", err, stderr.String())
	}
	if environment.authVolumeCalls != 1 {
		t.Fatalf("credential check ran %d times, want 1", environment.authVolumeCalls)
	}
	if environment.authVolumeAuthorizer == nil {
		t.Fatal("credential check received a nil rig authorizer")
	}
}

func TestPreflightFailureFixtures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*[]string, *fakePreflightEnvironment)
		check  string
	}{
		{"stale image", func(_ *[]string, env *fakePreflightEnvironment) {
			env.imageErrors["claude"] = errors.New("stale")
		}, "implementer_image"},
		{"unrecorded implementer image", func(_ *[]string, env *fakePreflightEnvironment) {
			env.database.ProjectImageFound = false
		}, "implementer_image"},
		{"configured path boundary mismatch", func(args *[]string, _ *fakePreflightEnvironment) {
			for i := range *args {
				if (*args)[i] == "-allowed-paths" {
					(*args)[i+1] = "docs/**"
				}
			}
		}, "source_implementation_identity"},
		{"base absent from branch", func(_ *[]string, env *fakePreflightEnvironment) {
			env.repositoryBaseError = errors.New("not reachable")
		}, "repository_base"},
		{"publication authority unavailable", func(_ *[]string, env *fakePreflightEnvironment) {
			env.authorityError = errors.New("unavailable")
		}, "publication_authority"},
		{"daemon conflict", func(_ *[]string, env *fakePreflightEnvironment) {
			env.idleError = errors.New("held")
		}, "daemon_conflict"},
		{"invalid work-unit declaration", func(args *[]string, _ *fakePreflightEnvironment) {
			path := filepath.Join(t.TempDir(), "work-unit.json")
			if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
			*args = append(*args, "-work-unit", path)
		}, "source_implementation_identity"},
		{"invalid review instructions", func(args *[]string, _ *fakePreflightEnvironment) {
			for i := range *args {
				if (*args)[i] == "-review-instructions" {
					(*args)[i+1] = t.TempDir()
				}
			}
		}, "review_instructions"},
		{"unsupported reviewer CLI or model", func(_ *[]string, env *fakePreflightEnvironment) {
			env.imageErrors["codex"] = errors.New("unsupported")
		}, "reviewer_image"},
		{"invalid review model argument", func(args *[]string, _ *fakePreflightEnvironment) {
			for i := range *args {
				if (*args)[i] == "-review-model" {
					(*args)[i+1] = "model,readonly"
				}
			}
		}, "review_configuration"},
		{"missing topic key", func(_ *[]string, env *fakePreflightEnvironment) {
			env.topicError = errors.New("missing")
		}, "topic_key"},
		{"unavailable build egress", func(args *[]string, _ *fakePreflightEnvironment) {
			*args = append(*args, "-build-proxy", "https://proxy.example.test")
		}, "build_egress_configuration"},
		{"profile review mismatch", func(_ *[]string, env *fakePreflightEnvironment) {
			env.database.ReviewError = errors.New("mismatch")
		}, "review_configuration"},
		{"invalid exporter image", func(_ *[]string, env *fakePreflightEnvironment) {
			env.imageErrors["exporter"] = errors.New("unsupported")
		}, "exporter_image"},
		{"invalid Claude auth identity", func(_ *[]string, env *fakePreflightEnvironment) {
			env.database.CredentialError = errors.New("lease not declared")
		}, "claude_credentials"},
		{"invalid Claude credential manifest", func(_ *[]string, env *fakePreflightEnvironment) {
			env.authVolumeError = errors.New("invalid setup token")
		}, "claude_credentials"},
		{"invalid Codex auth identity", func(_ *[]string, env *fakePreflightEnvironment) {
			env.database.ReviewCredentialError = errors.New("wrong provider")
		}, "codex_credentials"},
		{"Codex re-enrollment hold", func(_ *[]string, env *fakePreflightEnvironment) {
			env.database.ReviewReenrollmentError = errors.New("needs re-enrollment")
		}, "codex_credentials"},
		{"mismatched Codex auth store", func(_ *[]string, env *fakePreflightEnvironment) {
			env.database.ReviewAuthStoreVolume = "/different/auth.json"
		}, "codex_credentials"},
		{"dirty seed root", func(_ *[]string, env *fakePreflightEnvironment) {
			env.seedError = errors.New("dirty")
		}, "seed_root"},
		{"wrong server URL", func(args *[]string, _ *fakePreflightEnvironment) {
			for i := range *args {
				if (*args)[i] == "-server-url" {
					(*args)[i+1] = "http://127.0.0.1:9999"
				}
			}
		}, "listener_server_url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, environment := preflightFixture(t)
			tt.mutate(&args, environment)
			var stdout bytes.Buffer
			err := runPreflightCommandWithEnvironment(
				t.Context(), args, &stdout, &bytes.Buffer{}, environment,
				time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23",
			)
			if !errors.Is(err, errCompositionPreflight) {
				t.Fatalf("error = %v, want composition refusal", err)
			}
			var manifest compositionManifest
			if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
				t.Fatalf("decode manifest: %v", err)
			}
			if checkStatus(manifest, tt.check) != compositionFailed {
				t.Fatalf("%s status = %s, want failed", tt.check, checkStatus(manifest, tt.check))
			}
			if tt.name == "unrecorded implementer image" && environment.imageCalls["claude"] != 0 {
				t.Fatalf("unrecorded implementer image executed %d probes", environment.imageCalls["claude"])
			}
			if tt.name == "daemon conflict" {
				if environment.repositoryCalls != 0 {
					t.Fatalf("repository authority inspection ran %d times while daemon was active", environment.repositoryCalls)
				}
				for _, name := range []string{"repository_base", "publication_authority"} {
					if status := checkStatus(manifest, name); status != compositionNotRun {
						t.Fatalf("%s status = %s, want not_run", name, status)
					}
				}
			}
		})
	}
}

func TestPreflightRejectsDirtyDaemonBuildIdentity(t *testing.T) {
	args, environment := preflightFixture(t)
	var stdout bytes.Buffer
	err := runPreflightCommandWithEnvironment(
		t.Context(), args, &stdout, &bytes.Buffer{}, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23-dirty",
	)
	if !errors.Is(err, errCompositionPreflight) {
		t.Fatalf("error = %v, want composition refusal", err)
	}
	var manifest compositionManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if status := checkStatus(manifest, "daemon_build"); status != compositionFailed {
		t.Fatalf("daemon_build status = %s, want failed", status)
	}
}

func TestPreflightRejectsInvalidBaseBeforeAuthenticatedObservation(t *testing.T) {
	args, environment := preflightFixture(t)
	for i := range args {
		if args[i] == "-base-sha" {
			args[i+1] = strings.Repeat("c", 39)
		}
	}
	var stdout bytes.Buffer
	err := runPreflightCommandWithEnvironment(
		t.Context(), args, &stdout, &bytes.Buffer{}, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23",
	)
	if !errors.Is(err, errCompositionPreflight) {
		t.Fatalf("error = %v, want composition refusal", err)
	}
	var manifest compositionManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"repository_base", "publication_authority"} {
		if status := checkStatus(manifest, name); status != compositionFailed {
			t.Fatalf("%s status = %s, want failed", name, status)
		}
	}
	if environment.repositoryCalls != 0 {
		t.Fatalf("repository authority inspection ran %d times for an invalid base", environment.repositoryCalls)
	}
}

func TestCompositionStatusRegistration(t *testing.T) {
	want := []compositionStatus{compositionPassed, compositionFailed, compositionNotRun}
	if len(AllCompositionStatuses) != len(want) {
		t.Fatalf("statuses = %v, want %v", AllCompositionStatuses, want)
	}
	for i, status := range AllCompositionStatuses {
		if status != want[i] || !status.valid() {
			t.Fatalf("status[%d] = %q, want valid %q", i, status, want[i])
		}
	}
	if compositionStatus("").valid() || compositionStatus("unknown").valid() {
		t.Fatal("invalid composition status passed validation")
	}
}

func TestGitHubRemoteMatchesRepository(t *testing.T) {
	tests := []struct {
		remote string
		want   bool
	}{
		{"git@github.com:owner/repo.git", true},
		{"git@studio.github.com:owner/repo.git", true},
		{"https://github.com/owner/repo.git", true},
		{"ssh://git@github.com/owner/repo", true},
		{"https://user:secret@github.com/owner/repo.git", false},
		{"git@example.com:owner/repo.git", false},
		{"git@github.com:owner/other.git", false},
	}
	for _, tt := range tests {
		t.Run(tt.remote, func(t *testing.T) {
			if got := githubRemoteMatchesRepository(tt.remote, "owner/repo"); got != tt.want {
				t.Fatalf("githubRemoteMatchesRepository(%q) = %t, want %t", tt.remote, got, tt.want)
			}
		})
	}
}

func TestPreflightManifestExcludesCredentialAndProxySecrets(t *testing.T) {
	args, environment := preflightFixture(t)
	args = append(args, "-build-proxy", "http://operator:do-not-print@proxy.example.test")
	environment.codexError = errors.New("access_token=do-not-print")
	var stdout bytes.Buffer
	_ = runPreflightCommandWithEnvironment(
		t.Context(), args, &stdout, &bytes.Buffer{}, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23",
	)
	if strings.Contains(stdout.String(), "do-not-print") || strings.Contains(stdout.String(), "access_token") {
		t.Fatalf("manifest leaked credential material: %s", stdout.String())
	}
}

func TestPreflightDatabaseFailureDoesNotPassDependentChecks(t *testing.T) {
	args, environment := preflightFixture(t)
	environment.database.OpenError = errors.New("unreadable")
	var stdout bytes.Buffer
	_ = runPreflightCommandWithEnvironment(
		t.Context(), args, &stdout, &bytes.Buffer{}, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23",
	)
	var manifest compositionManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	for _, name := range []string{
		"trust_profile", "review_configuration", "repository_base", "publication_authority",
		"claude_credentials", "codex_credentials",
	} {
		if status := checkStatus(manifest, name); status != compositionNotRun {
			t.Errorf("%s status = %s, want not_run", name, status)
		}
	}
	if environment.repositoryCalls != 0 {
		t.Fatalf("repository authority inspection ran %d times without an auditable database", environment.repositoryCalls)
	}
}

func preflightFixture(t *testing.T) ([]string, *fakePreflightEnvironment) {
	t.Helper()
	root := t.TempDir()
	stateRoot := "/var/lib/freeside-test/state"
	seedRoot := "/var/lib/freeside-test/seed"
	workItemPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	rigPath := filepath.Join(root, "rig.json")
	rig := rigHoldOutput{
		Token: strings.Repeat("t", 32),
		Manifest: daemonlock.RigManifest{
			Version:    3,
			Owner:      daemonlock.RigOwner{User: "operator", Host: "studio", PID: 42},
			AcquiredAt: time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC),
			Resources: daemonlock.RigResources{
				StateRoot: stateRoot, DatabasePath: filepath.Join(stateRoot, "freeside.db"),
				ListenAddress: "127.0.0.1:8677", SeedRoot: seedRoot,
				LeaseRoot: "/var/lib/freeside-test/leases",
			},
			TokenDigest: strings.Repeat("d", 64), AmendmentPublicKey: "test-public-key",
		},
	}
	body, err := json.Marshal(rig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	reviewDigest := domain.Digest("sha256:" + strings.Repeat("b", 64))
	projectImage, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "owner/repo", RepositoryID: 42, CommitSHA: strings.Repeat("c", 40),
		RecipeDigest: reviewDigest, PreparationCommand: []string{projectimage.PreparationPath},
		BaseImageRef: domain.ImageRef("registry.test/base@" + digest),
		ImageRef:     domain.ImageRef("registry.test/agent@" + digest),
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewInstructions := filepath.Join(root, "review-instructions.md")
	if err := os.WriteFile(reviewInstructions, []byte("Review the change."), 0o600); err != nil {
		t.Fatal(err)
	}
	reviewInputRoot := filepath.Join(root, "review")
	if err := os.MkdirAll(reviewInputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	reviewSnapshot := filepath.Join(reviewInputRoot, "auth.json")
	if err := os.WriteFile(reviewSnapshot, []byte(`{"access_token":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	environment := &fakePreflightEnvironment{
		rig: rig.Manifest, imageErrors: map[string]error{}, imageCalls: map[string]int{}, codexExpiresAt: &expiresAt,
		database: databaseInspection{
			SchemaVersion: 81, ExpectedSchemaVersion: 81,
			ProfileDigest: reviewDigest, ProfileRepo: "owner/repo", ProfileRepositoryID: 42,
			ReviewAuthStoreVolume: reviewSnapshot, ReviewRefreshStrategy: domain.RefreshOnDemand,
			ProjectImage: projectImage, ProjectImageFound: true,
		},
	}
	args := []string{
		"-rig-token-file", rigPath, "-server-url", "http://127.0.0.1:8677",
		"-agent-image", "registry.test/agent@" + digest,
		"-exporter-image", "registry.test/exporter@" + digest,
		"-review-image", "registry.test/reviewer@" + digest,
		"-repo", "owner/repo", "-repository-checkout", root,
		"-repository-id", "42", "-base-ref", "main",
		"-base-sha", strings.Repeat("c", 40), "-approved-recipe", string(reviewDigest),
		"-auth-identity", "claude-1", "-auth-volume", "claude-auth-volume",
		"-review-input-root", reviewInputRoot,
		"-review-auth-mode", "subscription", "-review-auth-identity", "codex-1",
		"-review-auth-snapshot", reviewSnapshot, "-review-model", "gpt-5.6-codex",
		"-review-instructions", reviewInstructions,
		"-publication-state-dir", filepath.Join(root, "app-state"),
		"-publication-credentials-dir", filepath.Join(root, "app-creds"),
		"-review-reasoning-effort", "high", "-review-cost-owner", "subscription:operator",
		"-work-item", workItemPath, "-policy", policyPath, "-publication", publicationPath,
		"-project", "project-test", "-allowed-paths", "daemon/**",
	}
	return args, environment
}

func checkStatus(manifest compositionManifest, name string) compositionStatus {
	for _, check := range manifest.Checks {
		if check.Name == name {
			return check.Status
		}
	}
	return ""
}
