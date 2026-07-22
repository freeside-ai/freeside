package integration_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// This is the composition the issue #168 evidence found missing: no merged
// path ran the importer's findings through the publication gate. The test
// imports a workspace that adds a §5.8 control-plane path, lifts the real
// import findings into a candidate authorization alongside a verification
// finding, and drives the real publisher — asserting it refuses before any
// branch or pull request, i.e. before the fake forge receives a single
// request.

const (
	testRepo = "freeside-ai/evidence-repo"
	// A recipe the evidence artifact is verified under and the candidate
	// binds; the exact value only has to be internally consistent here.
	testRecipe = domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
)

var fixtureTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// runGit runs git over a test-owned repo with global and system config
// neutralized and a fixed identity, so commits are deterministic and no
// developer git config leaks in. Mirrors the importer suite's helper.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test running git over test-owned repos with test-chosen args
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@test.invalid",
		"GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@test.invalid",
		"GIT_COMMITTER_DATE=1700000000 +0000",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// importControlPlaneCandidate runs the real importer over a workspace that
// adds a control-plane path (.github/workflows/ci.yml, a mandatory §5.8
// automation-control pattern). It returns the imported candidate head and the
// real import findings.
func importControlPlaneCandidate(t *testing.T) (headSHA string, findings []importer.Finding) {
	t.Helper()
	base := t.TempDir()
	runGit(t, base, "init", "-q")
	writeFile(t, base, "README.md", "hi\n")
	runGit(t, base, "add", "-A")
	runGit(t, base, "commit", "-q", "-m", "base")
	baseSHA := runGit(t, base, "rev-parse", "HEAD")

	ws := t.TempDir()
	writeFile(t, ws, "README.md", "hi\n") // unchanged
	writeFile(t, ws, ".github/workflows/ci.yml", "on: push\njobs: {}\n")

	handoff := filepath.Join(t.TempDir(), "handoff")
	if _, err := export.Export(os.DirFS(ws), handoff, export.Options{}); err != nil {
		t.Fatalf("export.Export: %v", err)
	}

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, base, "clone", "-q", "--no-hardlinks", ".", clone)

	res, err := importer.Import(context.Background(), handoff, clone, importer.Options{
		BaseSHA:    baseSHA,
		CommitDate: fixtureTime,
	})
	if err != nil {
		t.Fatalf("importer.Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("control-plane import withheld the commit; expected a policy-only finding")
	}
	var sawControlPlane bool
	for _, f := range res.Findings {
		if f.Kind == importer.FindingAutomationControlPath {
			sawControlPlane = true
		}
	}
	if !sawControlPlane {
		t.Fatalf("import produced no automation-control finding: %+v", res.Findings)
	}
	return res.CommitSHA, res.Findings
}

// conformantTrust builds a trust profile and matching audit the candidate
// passes the drift gate (#169) against, so a refusal must come from the
// authorization gate, not the earlier trust gate.
func conformantTrust(t *testing.T, headSHA string) (domain.AutomationTrustProfile, domain.WorkflowAudit) {
	t.Helper()
	p, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       testRepo,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit-fixture",
		Review:                     domain.ReviewSettings{Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config"},
	})
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	audit := domain.WorkflowAudit{
		Repo:                testRepo,
		AuditedCommitSHA:    headSHA,
		AuditedAt:           fixtureTime,
		WorkflowAuditDigest: "sha256:workflow-audit-fixture",
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	if err := domain.EvaluateTrustDrift(p, audit); err != nil {
		t.Fatalf("fixture trust drifts: %v", err)
	}
	return p, audit
}

// evidenceArtifact builds a publish-eligible, head-bound evidence artifact so
// the candidate clears the artifact re-gate before reaching the authorization
// gate.
func evidenceArtifact(t *testing.T, headSHA string) domain.Artifact {
	t.Helper()
	r := testRecipe
	a, err := domain.NewArtifact(domain.ArtifactInput{
		ID:     "artifact-1",
		Type:   "verification-evidence",
		Digest: domain.Digest("sha256:" + strings.Repeat("b", 64)),
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-producer",
			HeadBinding:              domain.HeadBound,
			SourceHeadSHA:            headSHA,
			VerificationRecipeDigest: &r,
			SensitivityClass:         domain.SensitivityNormal,
		},
	}, map[domain.Digest]bool{testRecipe: true})
	if err != nil {
		t.Fatalf("NewArtifact: %v", err)
	}
	return a
}

// In-package fakes for the publisher's store-backed ports. The gate under
// test fails before recordIntent and before any forge call, so the ledger and
// token source must never be reached; reaching them is itself a failure.

type trustSource struct {
	profile domain.AutomationTrustProfile
	audit   domain.WorkflowAudit
}

func (s trustSource) CurrentTrust(context.Context, string) (publish.CurrentTrust, error) {
	return publish.CurrentTrust{Profile: &s.profile, Audit: &s.audit}, nil
}

func (s trustSource) Audit(context.Context, string, string) (domain.WorkflowAudit, error) {
	return s.audit, nil
}

type authzSource struct {
	auths map[domain.Digest]domain.CandidateAuthorization
}

func (s authzSource) Authorization(_ context.Context, id domain.Digest) (domain.CandidateAuthorization, bool, error) {
	a, ok := s.auths[id]
	return a, ok, nil
}

type failLedger struct{ t *testing.T }

func (l failLedger) Record(context.Context, string, string, []byte) ([]byte, bool, error) {
	l.t.Error("outbox intent recorded: the authorization gate must fail before recordIntent")
	return nil, false, errors.New("ledger must not be reached")
}

type zeroTokenSource struct{}

func (zeroTokenSource) Token(context.Context, string) (publish.InstallationToken, error) {
	return publish.InstallationToken{}, nil
}

// TestImportFindingsBlockPublication composes the real importer and the real
// publisher: a candidate whose authorization records a control-plane import
// finding (plus a verification-control finding) is refused before any branch
// or pull request. The forge server fails the test on any request, so the
// assertion is that no external effect occurred at all.
func TestImportFindingsBlockPublication(t *testing.T) {
	ctx := context.Background()

	headSHA, importFindings := importControlPlaneCandidate(t)

	// Lift the real import findings and add a verification-origin finding, so
	// the authorization carries both stages' publish-blocking findings.
	var findings []domain.CandidateFinding
	for _, f := range importFindings {
		findings = append(findings, f.Candidate())
	}
	verificationCat := domain.ControlPlaneVerificationRecipes
	findings = append(findings, domain.CandidateFinding{
		Class:       domain.FindingClassControlPlane,
		Category:    &verificationCat,
		Origin:      domain.FindingOriginVerification,
		Kind:        "verification_control_path",
		Path:        ".freeside/recipe.yml",
		Disposition: domain.DispositionBlocking,
	})

	profile, audit := conformantTrust(t, headSHA)

	auth, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo:                     testRepo,
		BaseSHA:                  strings.Repeat("1", 40),
		HeadSHA:                  headSHA,
		ImportResultDigest:       domain.Digest("sha256:" + strings.Repeat("f", 64)),
		VerificationRecipeDigest: testRecipe,
		VerificationOutcome:      domain.VerificationPassed,
		Findings:                 findings,
		TrustProfileDigest:       profile.ProfileDigest,
		InvocationID:             "inv-verify-1",
		CreatedAt:                fixtureTime,
	})
	if err != nil {
		t.Fatalf("NewCandidateAuthorization: %v", err)
	}
	// Precondition: the real blocking findings make the record non-authorizing.
	if auth.AuthorizesPublication {
		t.Fatal("authorization with blocking findings unexpectedly authorizes publication")
	}

	// A forge that fails the test on any request: an authorized-refusal must
	// touch nothing.
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("forge received %s %s: unauthorized candidate reached an external effect", r.Method, r.URL.Path)
		http.Error(w, "must not be called", http.StatusTeapot)
	}))
	t.Cleanup(forge.Close)

	pub := publish.NewPublisher(
		zeroTokenSource{}, forge.Client(), forge.URL,
		trustSource{profile: profile, audit: audit},
		failLedger{t},
		trustSource{profile: profile, audit: audit},
		authzSource{auths: map[domain.Digest]domain.CandidateAuthorization{auth.ID: auth}},
	)

	recipe := testRecipe
	authID := auth.ID
	profileDigest := profile.ProfileDigest
	c := publish.Candidate{
		Repo:               testRepo,
		BaseRef:            "main",
		HeadSHA:            headSHA,
		Title:              "Candidate: control-plane change",
		Body:               "Should never publish.",
		Artifacts:          []domain.Artifact{evidenceArtifact(t, headSHA)},
		RecipeDigest:       &recipe,
		InvocationID:       "inv-publish-1",
		AuthorizationID:    &authID,
		TrustProfileDigest: &profileDigest,
	}

	_, err = pub.Publish(ctx, c, map[domain.Digest]bool{testRecipe: true})
	if !errors.Is(err, publish.ErrUnauthorizedPublication) {
		t.Fatalf("Publish err = %v, want ErrUnauthorizedPublication", err)
	}
}
