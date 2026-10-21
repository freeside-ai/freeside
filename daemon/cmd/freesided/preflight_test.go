package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
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
	prodMode             executionMode
	prodRunning          bool
	prodError            error
	prodProbed           []string
	live                 bool
	liveByAddr           map[string]bool // when set, overrides live per probed address
	probed               []string
	codexError           error
	codexExpiresAt       *time.Time
	codexCalls           int
	publicationAuthor    engine.ProductionCommitAuthor
	agentsError          error
	// reviewModel replaces the model the fixture's reviewer agent names.
	reviewModel        string
	judgmentRoles      []inference.RoleCheck
	judgmentRolesError error
	judgmentRolesCalls int
}

func (e *fakePreflightEnvironment) CheckJudgmentRoles(
	context.Context, preflightConfig, judgmentRuntime, ward.RuntimeResourceAuthorizer, time.Time,
) ([]inference.RoleCheck, error) {
	e.judgmentRolesCalls++
	return e.judgmentRoles, e.judgmentRolesError
}

// preflightFixtureAgents is what the fixture's lineup resolves to: the
// identities, cost owners, models, and efforts the removed flags used to
// carry.
var preflightFixtureAgents = preflightAgentSelection{
	LineupRevision: domain.Digest("sha256:" + strings.Repeat("9", 64)),
	AuthIdentityID: "claude-1", AuthVolume: "claude-auth-volume",
	ReviewAuthIdentityID: "codex-1", ReviewCostOwner: "subscription:operator",
	ReviewModel: "gpt-5.6-codex", ReviewReasoningEffort: "high",
	ShadowReviewAuthIdentityID: "claude-1", ShadowReviewCostOwner: "subscription:shadow",
	ShadowReviewModel: "claude-opus", ShadowReviewReasoningEffort: "high",
}

func (e *fakePreflightEnvironment) ResolveAgentSelection(
	_ context.Context, cfg preflightConfig, _ time.Time,
) (preflightAgentSelection, error) {
	if e.agentsError != nil {
		return preflightAgentSelection{}, e.agentsError
	}
	agents := preflightFixtureAgents
	if e.reviewModel != "" {
		agents.ReviewModel = e.reviewModel
	}
	if cfg.ShadowReviewImage == "" {
		agents.ShadowReviewAuthIdentityID, agents.ShadowReviewCostOwner = "", ""
		agents.ShadowReviewModel, agents.ShadowReviewReasoningEffort = "", ""
	}
	return agents, nil
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
	inspection := repositoryAuthorityInspection{
		BaseError: e.repositoryBaseError, AuthorityError: e.authorityError,
	}
	if e.authorityError == nil {
		inspection.Author = engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345}
	}
	return inspection
}

func (e *fakePreflightEnvironment) CheckAuthVolume(
	_ context.Context, _, _, _ string, authorize ward.RuntimeResourceAuthorizer,
) error {
	e.authVolumeCalls++
	e.authVolumeAuthorizer = authorize
	return e.authVolumeError
}

func (e *fakePreflightEnvironment) DatabaseIdle(string) error { return e.idleError }

func (e *fakePreflightEnvironment) ProdExecution(_ context.Context, dbPath string) (executionMode, bool, error) {
	e.prodProbed = append(e.prodProbed, dbPath)
	return e.prodMode, e.prodRunning, e.prodError
}

func (e *fakePreflightEnvironment) ProbeDaemon(
	_ context.Context, address string,
) (string, bool, error) {
	e.probed = append(e.probed, address)
	if e.liveByAddr != nil {
		return "test process", e.liveByAddr[address], nil
	}
	return "test process", e.live, nil
}

func TestEvaluateDaemonConflictProbesLoopbackTwin(t *testing.T) {
	for _, tc := range []struct {
		name         string
		listen       string
		liveByAddr   map[string]bool
		wantPass     bool
		wantProbed   []string
		wantEvidence string
	}{
		{
			name: "tailscale idle twin passes", listen: "100.64.0.1:8677",
			wantPass: true, wantProbed: []string{"100.64.0.1:8677", "127.0.0.1:8677"},
		},
		{
			name: "tailscale occupied twin fails", listen: "100.64.0.1:8677",
			liveByAddr: map[string]bool{"127.0.0.1:8677": true},
			wantPass:   false, wantProbed: []string{"100.64.0.1:8677", "127.0.0.1:8677"},
			wantEvidence: "leased loopback listener is occupied",
		},
		{
			name: "loopback lease probes once", listen: "127.0.0.1:8677",
			wantPass: true, wantProbed: []string{"127.0.0.1:8677"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := &fakePreflightEnvironment{liveByAddr: tc.liveByAddr}
			manifest := &compositionManifest{
				Checks: []compositionCheck{{Name: "daemon_conflict", Status: compositionNotRun}},
			}
			manifest.Rig.Resources.ListenAddress = tc.listen
			got := evaluateDaemonConflict(context.Background(), manifest, preflightConfig{}, env)
			if got != tc.wantPass {
				t.Fatalf("evaluateDaemonConflict = %v, want %v", got, tc.wantPass)
			}
			if !slices.Equal(env.probed, tc.wantProbed) {
				t.Fatalf("probed = %v, want %v", env.probed, tc.wantProbed)
			}
			status := compositionPassed
			if !tc.wantPass {
				status = compositionFailed
			}
			if checkStatus(*manifest, "daemon_conflict") != status {
				t.Fatalf("daemon_conflict status = %s, want %s",
					checkStatus(*manifest, "daemon_conflict"), status)
			}
			if tc.wantEvidence != "" {
				var evidence string
				for _, check := range manifest.Checks {
					if check.Name == "daemon_conflict" {
						evidence = check.Evidence
					}
				}
				if !strings.Contains(evidence, tc.wantEvidence) {
					t.Fatalf("daemon_conflict evidence = %q, want it to contain %q", evidence, tc.wantEvidence)
				}
			}
		})
	}
}

func TestEvaluateProdExecution(t *testing.T) {
	for _, tc := range []struct {
		name            string
		env             fakePreflightEnvironment
		wantPass        bool
		wantEvidence    string
		wantRemediation string
	}{
		{name: "not running", wantPass: true, wantEvidence: "prod is not running"},
		{
			name: "driverless", env: fakePreflightEnvironment{prodRunning: true, prodMode: executionDisabled},
			wantPass: true, wantEvidence: "prod runs without a driver",
		},
		{
			name: "fake driver", env: fakePreflightEnvironment{prodRunning: true, prodMode: executionFake},
			wantEvidence: "prod runs with -driver fake", wantRemediation: "restart it without -driver",
		},
		{
			name: "claude driver", env: fakePreflightEnvironment{prodRunning: true, prodMode: executionClaude},
			wantEvidence: "prod runs with -driver claude", wantRemediation: "restart it without -driver",
		},
		{
			name: "unanswering advertisement", env: fakePreflightEnvironment{prodError: errors.New("connection refused")},
			wantEvidence:    "did not report its execution mode",
			wantRemediation: "rewrites its control advertisement",
		},
		{
			name: "older build", env: fakePreflightEnvironment{prodRunning: true, prodError: errors.New("404")},
			wantEvidence:    "did not report its execution mode",
			wantRemediation: "update the installed build",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			manifest := &compositionManifest{
				Checks: []compositionCheck{{Name: "prod_execution", Status: compositionNotRun}},
			}
			appState := filepath.Join(t.TempDir(), "prod", "daemon")
			cfg := preflightConfig{PublicationStateDir: appState}
			if got := evaluateProdExecution(context.Background(), manifest, cfg, &env); got != tc.wantPass {
				t.Fatalf("evaluateProdExecution = %v, want %v", got, tc.wantPass)
			}
			if want := []string{filepath.Join(appState, "freeside.db")}; !slices.Equal(env.prodProbed, want) {
				t.Fatalf("probed = %v, want prod's database %v", env.prodProbed, want)
			}
			check := manifest.Checks[0]
			wantStatus := compositionPassed
			if !tc.wantPass {
				wantStatus = compositionFailed
			}
			if check.Status != wantStatus || (manifest.Status == compositionFailed) == tc.wantPass {
				t.Fatalf("prod_execution status = %s, manifest %s; want %s", check.Status, manifest.Status, wantStatus)
			}
			if !strings.Contains(check.Evidence, tc.wantEvidence) || !strings.Contains(check.Remediation, tc.wantRemediation) {
				t.Fatalf("prod_execution evidence %q, remediation %q; want %q and %q",
					check.Evidence, check.Remediation, tc.wantEvidence, tc.wantRemediation)
			}
		})
	}
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

// TestPreflightLaunchManifestGolden pins the manifest deputy writes for a
// run: no submission identity, the settings revision, and the App bot
// identity the registration resolved, recorded rather than compared.
func TestPreflightLaunchManifestGolden(t *testing.T) {
	args, environment := preflightFixture(t)
	cfg, err := parsePreflightConfig(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TaskPath, cfg.PolicyPath, cfg.PublicationPath = "", "", ""
	cfg.ProjectID = "proj-launch"
	cfg.Launch = true
	cfg.SettingsDigest = domain.Digest("sha256:" + strings.Repeat("5", 64))
	manifest, err := composePreflight(t.Context(), cfg, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != compositionPassed {
		t.Fatalf("launch manifest status %s: %+v", manifest.Status, manifest.Checks)
	}
	if environment.publicationAuthor != (engine.ProductionCommitAuthor{}) {
		t.Fatalf("launch preflight claimed author %+v", environment.publicationAuthor)
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "preflight-launch-manifest", append(body, '\n'))
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
	judgments, _ := judgmentFixture(t)
	args = append(args, "-judgment-claude-bin", judgments.CLI.Binary,
		"-judgment-claude-sha256", judgments.CLI.SHA256)
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
	// The judgment role check reads a stored credential too.
	if checkStatus(manifest, "judgment_configuration") != compositionPassed ||
		checkStatus(manifest, "judgment_roles") != compositionNotRun || environment.judgmentRolesCalls != 0 {
		t.Fatalf("judgment roles before shadow approval: calls=%d checks=%+v",
			environment.judgmentRolesCalls, manifest.Checks)
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
	st := storetest.Open(t, dbPath, store.Options{})
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
		ReviewAuthMode: ward.CodexAuthSubscription,
		Agents: preflightAgentSelection{
			AuthIdentityID: "missing-claude", ReviewAuthIdentityID: "missing-codex",
			ShadowReviewAuthIdentityID: "missing-claude", ShadowReviewCostOwner: "subscription:shadow",
			ShadowReviewModel: "claude-opus", ShadowReviewReasoningEffort: "high",
		},
		ShadowReviewImage:           "ghcr.io/x/claude@sha256:" + strings.Repeat("6", 64),
		ExporterImage:               "ghcr.io/x/exporter@sha256:" + strings.Repeat("5", 64),
		ReviewInputRoot:             "/var/freeside/review-inputs",
		ShadowReviewWorkspaceSizeMB: 4096,
		ShadowReviewRate:            0.2,
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

func TestPreflightRejectsOversizedComposedAuthorInstructions(t *testing.T) {
	args, environment := preflightFixture(t)
	repo, base := authorPreflightRepository(t, map[string]string{"AGENTS.md": "trusted repository rules\n"})
	for i := range args {
		switch args[i] {
		case "-repository-checkout":
			args[i+1] = repo
		case "-base-sha":
			args[i+1] = base
		case "-review-instructions":
			if err := os.WriteFile(args[i+1], []byte(strings.Repeat("h", 256<<10)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	args = append(args, "-judgment-publication-author-prompt", filepath.Join(t.TempDir(), "author-prompt.md"))
	var stdout bytes.Buffer
	err := runPreflightCommandWithEnvironment(t.Context(), args, &stdout, &bytes.Buffer{}, environment,
		time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23")
	if !errors.Is(err, errCompositionPreflight) {
		t.Fatalf("preflight error = %v, want refusal", err)
	}
	var manifest compositionManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, check := range manifest.Checks {
		if check.Name != "publication_author_inputs" {
			continue
		}
		if check.Status != compositionFailed || !strings.Contains(check.Evidence, "instruction_snapshot:") ||
			!strings.Contains(check.Evidence, "limit 262144") || strings.Contains(check.Evidence, "hhhh") ||
			!strings.Contains(check.Remediation, "host rules") {
			t.Fatalf("author input check = %+v", check)
		}
		return
	}
	t.Fatal("preflight omitted publication_author_inputs check")
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
		{"prod with a driver", func(_ *[]string, env *fakePreflightEnvironment) {
			env.prodRunning, env.prodMode = true, executionClaude
		}, "prod_execution"},
		{"prod that does not answer", func(_ *[]string, env *fakePreflightEnvironment) {
			env.prodError = errors.New("connection refused")
		}, "prod_execution"},
		{"role with no resolving line", func(_ *[]string, env *fakePreflightEnvironment) {
			env.agentsError = &roleAdmissionError{Role: domain.RoleReviewer, Err: engine.ErrAgentNotAdmissible}
		}, "agent_selection"},
		{"unreadable agent tree", func(_ *[]string, env *fakePreflightEnvironment) {
			env.agentsError = errors.New("commit not found")
		}, "agent_selection"},
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
		{"a reviewer agent whose model is no single argument", func(_ *[]string, env *fakePreflightEnvironment) {
			env.reviewModel = "model,readonly"
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
		{"expired Codex credential", func(_ *[]string, env *fakePreflightEnvironment) {
			env.codexError = errors.New("below lifetime floor")
		}, "codex_credentials"},
		{"invalid Codex snapshot", func(_ *[]string, env *fakePreflightEnvironment) {
			env.codexError = errors.New("invalid credential JSON")
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
			if tt.check == "codex_credentials" {
				want := "enroll-codex"
				if tt.name == "expired Codex credential" {
					want = "renew-codex"
				}
				if tt.name == "Codex re-enrollment hold" {
					want = "Recover Expired Reviewer Credentials"
				}
				for _, check := range manifest.Checks {
					if check.Name == tt.check && !strings.Contains(check.Remediation, want) {
						t.Fatalf("wrong recovery hint: %s", check.Remediation)
					}
					if check.Name == tt.check && tt.name == "invalid Codex snapshot" && !strings.Contains(check.Remediation, "codex login") {
						t.Fatal("invalid subscription snapshot lacks the login and enrollment fallback")
					}
					if check.Name == tt.check && tt.name == "expired Codex credential" && !strings.Contains(check.Evidence, "lifetime floor") {
						t.Fatal("expiry condition missing")
					}
					if check.Name == tt.check && tt.name == "Codex re-enrollment hold" && !strings.Contains(check.Evidence, "hold") {
						t.Fatal("hold condition missing")
					}
				}
			}
			if tt.name == "unrecorded implementer image" && environment.imageCalls["claude"] != 0 {
				t.Fatalf("unrecorded implementer image executed %d probes", environment.imageCalls["claude"])
			}
			if tt.name == "daemon conflict" || tt.name == "prod with a driver" || tt.name == "prod that does not answer" {
				if environment.repositoryCalls != 0 {
					t.Fatalf("repository authority inspection ran %d times while a daemon could write", environment.repositoryCalls)
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
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
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
		"-agent-tree", root, "-agent-tree-commit", strings.Repeat("e", 40),
		"-auth-volume", "claude-auth-volume",
		"-review-input-root", reviewInputRoot,
		"-review-auth-mode", "subscription",
		"-review-auth-snapshot", reviewSnapshot,
		"-review-instructions", reviewInstructions,
		"-publication-state-dir", filepath.Join(root, "app-state"),
		"-publication-credentials-dir", filepath.Join(root, "app-creds"),
		"-task", taskPath, "-policy", policyPath, "-publication", publicationPath,
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

// TestPreflightRefusesProdAppDirectoriesWithoutAuthority: a run pointed at
// prod's App directories (#1583) can't start when they hold no credentials,
// or credentials with no authority entry for the registration.
func TestPreflightRefusesProdAppDirectoriesWithoutAuthority(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, state, credentials string)
		want  string
	}{
		{"no credentials", func(*testing.T, string, string) {}, "no usable GitHub App credentials"},
		{"credentials without matching authority", func(t *testing.T, state, credentials string) {
			t.Helper()
			keystore, err := publish.NewKeystore(credentials, state)
			if err != nil {
				t.Fatal(err)
			}
			if err := keystore.SaveApp(publish.AppCredentials{
				Owner: "example", OwnerID: 42, Visibility: publish.AppVisibilityPublic,
				AppID: 91, Name: "Freeside Example", Slug: "freeside-example",
				ClientID: "Iv1.example", Key: setupTestKey(t),
			}); err != nil {
				t.Fatal(err)
			}
			authority, err := publish.NewInstallationAuthorityStore(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := authority.InitializeDocument(t.Context()); err != nil {
				t.Fatal(err)
			}
		}, "registration 91 has no authority entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state, credentials := prodAppDirs(t, t.TempDir())
			tc.setup(t, state, credentials)
			inspection := inspectRepositoryAuthority(t.Context(), preflightConfig{
				PublicationStateDir: state, PublicationCredentialsDir: credentials, RepositoryID: 7,
			}, time.Now())
			if inspection.AuthorityError == nil || !strings.Contains(inspection.AuthorityError.Error(), tc.want) {
				t.Fatalf("AuthorityError = %v, want %q", inspection.AuthorityError, tc.want)
			}
		})
	}
}
