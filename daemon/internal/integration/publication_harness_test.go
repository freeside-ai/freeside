package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	fakeforge "github.com/freeside-ai/freeside/daemon/internal/exec/fake/forge"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/repotemplate"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/strayrequest"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	fakePublicationRepo       = "freeside-ai/evidence-repo"
	fakePublicationIssueTitle = "Preserve retry results"
	fakePublicationIssueBody  = "Keep recorded results when a publication retries after restart."
)

var fakePublicationTime = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func integrationTokenSource() fakeforge.TokenSource {
	return fakeforge.TokenSource{
		Secret: "integration-token", Repo: fakePublicationRepo,
		Now: func() time.Time { return fakePublicationTime },
	}
}

type inactiveIntegrationTokenSource struct{}

func (inactiveIntegrationTokenSource) Token(
	context.Context,
	string,
) (publish.InstallationToken, error) {
	return publish.InstallationToken{}, publish.ErrJanitorInactive
}

// newIntegrationForge serves the forge double for fakePublicationRepo, whose
// one work-unit issue is #82, and fails the test on any request it does not
// expect.
func newIntegrationForge(t *testing.T) (*fakeforge.Forge, *httptest.Server) {
	t.Helper()
	forge := fakeforge.New(fakeforge.Config{
		Repo: fakePublicationRepo, Token: "integration-token",
		Issue: fakeforge.Issue{
			Number: 82, Title: fakePublicationIssueTitle, Body: fakePublicationIssueBody,
		},
		Report: t.Errorf,
	})
	server := httptest.NewServer(strayrequest.Ignore(forge))
	t.Cleanup(server.Close)
	return forge, server
}

// TestIntegrationForgeIgnoresStrayRootProbe: a local tool's GET / to the
// forge double is not an unexpected forge request, and the double still
// answers the requests a publication sends (#2465).
func TestIntegrationForgeIgnoresStrayRootProbe(t *testing.T) {
	t.Parallel()
	forge, server := newIntegrationForge(t)
	strayrequest.Probe(t, server.URL)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/repos/"+fakePublicationRepo+"/issues/82", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer integration-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	forge.Mu.Lock()
	reads := forge.IssueReads
	forge.Mu.Unlock()
	if resp.StatusCode != http.StatusOK || reads != 1 {
		t.Errorf("issue read status %d, reads %d; want 200 and 1", resp.StatusCode, reads)
	}
}

// headRepo is the transport's remote side of the pull request branches.
func headRepo(t *testing.T, transport *fakeforge.Transport) string {
	t.Helper()
	dir, err := transport.HeadRepo()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

type publicationHarness struct {
	t                 *testing.T
	ctx               context.Context
	dbPath            string
	blobDir           string
	workDir           string
	baseDir           string
	baseSHA           string
	baseFiles         map[string]string
	projectImageInput func(*domain.ProjectImageInput)
	cliSubmission     *cliPublicationSubmission
	recipe            []byte
	recipeD           domain.Digest
	profile           domain.AutomationTrustProfile
	audit             domain.WorkflowAudit
	store             *store.Store
	attention         *signet.Service
	blobs             *signet.BlobStore
	transport         *fakeforge.Transport
	forge             *fakeforge.Forge
	server            *httptest.Server
	tokens            publish.TokenSource
	now               time.Time

	// replayStanceClaim, when set, is the candidate_automation_changes
	// stance the next built replay's import options claim in place of the
	// profile's, as a forged task payload would.
	replayStanceClaim domain.AutomationChangePolicy

	// repairRefusal, when set, makes a client submission the maintenance
	// task image upkeep submits for the refusal it returns, given the
	// harness's project image, in place of a device's task.
	repairRefusal func(domain.ProjectImage) domain.ImageUpkeepRefusalInput
}

// publicationBaseTemplates builds each distinct base file set once per test
// binary; every harness gets its own copy, so a test that commits to its
// baseDir changes only that copy. The fixed fixture identity and dates make
// the copy the same commit a fresh build would make.
var publicationBaseTemplates repotemplate.Cache[string]

func newPublicationHarness(t *testing.T) *publicationHarness {
	t.Helper()
	return newPublicationHarnessWithRecipe(t, []byte(`{"commands":[["/usr/bin/true"]],"capture":"none"}`))
}

func newPublicationHarnessWithRecipe(t *testing.T, recipe []byte) *publicationHarness {
	t.Helper()
	return newPublicationHarnessWithBaseFiles(t, recipe, nil)
}

func newPublicationHarnessWithBaseFiles(t *testing.T, recipe []byte, files map[string]string) *publicationHarness {
	t.Helper()
	return newPublicationHarnessWithStance(t, recipe, files, domain.AutomationChangesBlocked)
}

// newPublicationHarnessWithStance records a trust profile whose
// candidate_automation_changes stance is stance, so a test can opt the
// repository into publishing automation-control edits as advisory.
func newPublicationHarnessWithStance(
	t *testing.T, recipe []byte, files map[string]string, stance domain.AutomationChangePolicy,
) *publicationHarness {
	t.Helper()
	ctx := t.Context()
	root := t.TempDir()
	base, baseSHA := publicationBaseTemplates.Copy(t, repotemplate.FilesKey(files), func() (string, string) {
		dir := t.TempDir()
		runGit(t, dir, "init", "-q", "-b", "main")
		writeFile(t, dir, "README.md", "base\n")
		for path, body := range files {
			writeFile(t, dir, path, body)
		}
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-q", "-m", "base")
		return dir, runGit(t, dir, "rev-parse", "HEAD")
	})

	recipeDigest := verify.RecipeDigest(recipe)
	auditEvidence := integrationWorkflowAuditEvidence(t, fakePublicationRepo, "publish")
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: fakePublicationRepo, RepositoryID: 123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: stance,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanPlanPreferred, MessageRuleset: domain.MessageRulesetGitHub1,
		WorkflowAuditDigest: auditEvidence.Digest(),
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: fake.DefaultReviewConfigurationDigest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	audit := domain.WorkflowAudit{
		Repo: fakePublicationRepo, AuditedCommitSHA: baseSHA,
		AuditedAt: fakePublicationTime, WorkflowAuditDigest: profile.WorkflowAuditDigest,
		Evidence:            &auditEvidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	if err := domain.EvaluateTrustDrift(profile, audit); err != nil {
		t.Fatalf("test trust profile drifts: %v", err)
	}
	dbPath := filepath.Join(root, "state", "freeside.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, dbPath, store.Options{
		ApprovedRecipes: map[domain.Digest]bool{recipeDigest: true},
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: {},
			domain.ModeUnattended:  {},
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource: store.BackupHealthSourceFunc(func(
			context.Context, store.BackupHealthContext,
		) (domain.BackupHealth, error) {
			return domain.BackupHealth{
				Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
				ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
			}, nil
		}),
	})
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, profile, fakePublicationTime)
	}); err != nil {
		t.Fatal(err)
	}
	forge, server := newIntegrationForge(t)
	blobDir := filepath.Join(root, "blobs")
	blobs, err := signet.NewBlobStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	return &publicationHarness{
		t: t, ctx: ctx, dbPath: dbPath, blobDir: blobDir,
		workDir: filepath.Join(root, "publication"),
		baseDir: base, baseSHA: baseSHA, baseFiles: files, recipe: recipe, recipeD: recipeDigest,
		profile: profile, audit: audit, store: st,
		attention: signet.NewService(st, signet.WithBlobStore(blobs)), blobs: blobs,
		transport: &fakeforge.Transport{BaseDir: base, BaseRef: "main", Forge: forge, HeadsDir: t.TempDir()},
		forge:     forge, server: server, now: fakePublicationTime,
	}
}
