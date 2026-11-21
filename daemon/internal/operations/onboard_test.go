package operations_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

type imageBuilderStub struct {
	calls int
	// onBuild runs first in Build; an error from it fails the build.
	onBuild            func() error
	image              domain.ProjectImage
	store              *store.Store
	preparationCommand []string
	afterBuild         func()
	mutateRequest      func(*projectimage.Request)
	// environment overrides the recorded environment of a stub-built image;
	// nil records this binary's, as the real builder does.
	environment func() *domain.ProjectImageEnvironment
}

func stubProjectImageEnvironment() *domain.ProjectImageEnvironment {
	return &domain.ProjectImageEnvironment{
		PackageJSONSHA256: strings.Repeat("1", 64),
		PackageLockSHA256: strings.Repeat("2", 64),
		PreparationDigest: projectimage.PreparationDigest(),
	}
}

func (b *imageBuilderStub) Build(
	ctx context.Context, req projectimage.Request,
) (domain.ProjectImage, error) {
	b.calls++
	if b.onBuild != nil {
		if err := b.onBuild(); err != nil {
			return domain.ProjectImage{}, err
		}
	}
	if b.mutateRequest != nil {
		b.mutateRequest(&req)
	}
	if b.image.ID == "" {
		command := b.preparationCommand
		if command == nil {
			command = []string{projectimage.PreparationPath}
		}
		environment := stubProjectImageEnvironment
		if b.environment != nil {
			environment = b.environment
		}
		image, err := domain.NewProjectImage(domain.ProjectImageInput{
			Repository: req.Repository, RepositoryID: req.RepositoryID,
			CommitSHA: req.CommitSHA, RecipeDigest: verify.RecipeDigest(req.Recipe),
			PreparationCommand: command,
			BaseImageRef:       req.BaseImageRef,
			ImageRef: domain.ImageRef(
				"127.0.0.1:" + fmt.Sprint(req.LocalRegistryPort) + "/" +
					req.ImageName + "@sha256:" + strings.Repeat("b", 64)),
			Environment: environment(),
		})
		if err != nil {
			return domain.ProjectImage{}, err
		}
		b.image = image
	}
	if b.store != nil {
		if err := b.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.RecordProjectImage(ctx, b.image)
		}); err != nil {
			return domain.ProjectImage{}, err
		}
	}
	if b.afterBuild != nil {
		b.afterBuild()
	}
	return b.image, nil
}

// bakedBaseInputs observes, at any commit, exactly the manifests
// stubProjectImageEnvironment bakes, as a source-only commit holds them.
func bakedBaseInputs(
	_ context.Context, commitSHA string, _ domain.ProjectImageInputLayout,
) (domain.ProjectImageBaseInputs, error) {
	environment := stubProjectImageEnvironment()
	return domain.ProjectImageBaseInputs{
		CommitSHA:         commitSHA,
		PackageJSONSHA256: environment.PackageJSONSHA256,
		PackageLockSHA256: environment.PackageLockSHA256,
	}, nil
}

// unreadableBaseInputs is a checkout that lacks the commit, so no admitted
// image is reused and every approval builds.
func unreadableBaseInputs(
	context.Context, string, domain.ProjectImageInputLayout,
) (domain.ProjectImageBaseInputs, error) {
	return domain.ProjectImageBaseInputs{}, errors.New("checkout lacks the commit")
}

// recipeAtAnyCommit reads, at any commit, the one recipe every request in
// these tests carries, as a tip whose recipe the owner already reviewed.
func recipeAtAnyCommit(context.Context, string) ([]byte, error) {
	return []byte(`{"commands":[["npm","test"]],"capture":"none"}`), nil
}

type authorityStub struct{ authority publish.InstallationAuthority }

func (a authorityStub) InstallationAuthority(
	context.Context, int64,
) (publish.InstallationAuthority, error) {
	return a.authority, nil
}

type workflowAuditorStub struct {
	audit domain.WorkflowAudit
}

func (a workflowAuditorStub) Audit(
	context.Context,
	string,
	string,
) (domain.WorkflowAudit, error) {
	return a.audit, nil
}

type pendingGateStub bool

func (g pendingGateStub) AllowsRepository(int64, int64, int64) bool {
	return bool(g)
}

func (g pendingGateStub) PendingReady(
	envelope publish.PendingInstallationEnvelope,
) (int64, bool) {
	if envelope.InstallationID == 0 {
		return 22, bool(g)
	}
	return envelope.InstallationID, bool(g)
}

type pendingGateSequence struct {
	results []bool
}

func (*pendingGateSequence) AllowsRepository(int64, int64, int64) bool {
	return true
}

func (g *pendingGateSequence) PendingReady(
	envelope publish.PendingInstallationEnvelope,
) (int64, bool) {
	result := g.results[0]
	g.results = g.results[1:]
	installationID := envelope.InstallationID
	if installationID == 0 {
		installationID = 22
	}
	return installationID, result
}

type mutableInstallationGate struct {
	allowed bool
}

func (g *mutableInstallationGate) AllowsRepository(int64, int64, int64) bool {
	return g.allowed
}

func (g *mutableInstallationGate) PendingReady(
	envelope publish.PendingInstallationEnvelope,
) (int64, bool) {
	return envelope.InstallationID, g.allowed
}

func TestOnboardRoundTripsPlanPreferredIntoActivatedProfile(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t, t.TempDir()+"/freeside.db", store.Options{})
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"example/repo","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	audit := domain.WorkflowAudit{
		Repo: "example/repo", AuditedCommitSHA: "0123456789012345678901234567890123456789",
		AuditedAt:           time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
		WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	builder := &imageBuilderStub{store: st}
	onboard := operations.Onboard{
		Store: st, Builder: builder, Auditor: workflowAuditorStub{audit: audit},
		Authority: authorityStub{publish.InstallationAuthority{
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: 11, InstallationID: 22,
				Account: "example", AccountID: 33, RepositoryIDs: []int64{44},
			}},
		}},
		Gate: pendingGateStub(true), Now: func() time.Time { return audit.AuditedAt },
		BaseInputs: bakedBaseInputs, Recipe: recipeAtAnyCommit,
	}
	req := operations.OnboardRequest{
		Repository: "example/repo", RepositoryID: 44, RegistrationID: 11,
		BaseRef: "main",
		Policy: operations.OnboardPolicy{
			PRExecution: domain.PRExecutionAuditedSameRepo,
			CommitPlan:  domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
			ReviewMode: domain.ReviewFreesideInvoked, ReviewConfig: "sha256:review",
			CandidateAutomationChanges: domain.AutomationChangesBlocked,
		},
		Image: projectimage.Request{
			Repository: "example/repo", RepositoryID: 44,
			CommitSHA: "0123456789012345678901234567890123456789",
			Recipe:    []byte(`{"commands":[["npm","test"]],"capture":"none"}`),
			BaseImageRef: domain.ImageRef(
				"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
			BaseBuildRef: "local/base:v1", LocalRegistryPort: 5100,
		},
	}
	singleCommitReview, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("single-commit review pass: %v", err)
	}
	req.Policy.CommitPlan = domain.CommitPlanPlanPreferred
	preferredReview, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("plan-preferred review pass: %v", err)
	}
	if preferredReview.Status != "review_required" ||
		preferredReview.Profile.CommitPlan != domain.CommitPlanPlanPreferred {
		t.Fatalf("plan-preferred review = %+v", preferredReview)
	}
	if preferredReview.ApprovalDigest == singleCommitReview.ApprovalDigest {
		t.Fatal("plan-preferred review reused the single-commit approval digest")
	}
	// The automation-change stance is part of what the owner approves.
	req.Policy.CandidateAutomationChanges = domain.AutomationChangesAdvisory
	advisoryReview, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("advisory review pass: %v", err)
	}
	if advisoryReview.Profile.CandidateAutomationChanges != domain.AutomationChangesAdvisory ||
		advisoryReview.ApprovalDigest == preferredReview.ApprovalDigest {
		t.Fatalf("advisory review = %+v, want its own approval digest", advisoryReview)
	}
	req.Policy.CandidateAutomationChanges = domain.AutomationChangesBlocked
	// The declared Go module and Swift package directories are part of what
	// the owner approves.
	req.Image.GoModuleDir = "daemon"
	goReview, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("Go module review pass: %v", err)
	}
	if goReview.ImageRequest.GoModuleDir != "daemon" || goReview.ApprovalDigest == preferredReview.ApprovalDigest {
		t.Fatalf("Go module review = %+v, want the module shown and its own approval digest", goReview.ImageRequest)
	}
	req.Image.GoModuleDir = ""
	req.Image.SwiftPackageDir = "app"
	swiftReview, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("Swift package review pass: %v", err)
	}
	if swiftReview.ImageRequest.SwiftPackageDir != "app" ||
		swiftReview.ApprovalDigest == preferredReview.ApprovalDigest ||
		swiftReview.ApprovalDigest == goReview.ApprovalDigest {
		t.Fatalf("Swift package review = %+v, want the package shown and its own approval digest", swiftReview.ImageRequest)
	}
	// Declaring both asks for one image that bakes both toolchains, which
	// the owner approves apart from either alone.
	req.Image.GoModuleDir = "daemon"
	goSwiftReview, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("Go and Swift review pass: %v", err)
	}
	if goSwiftReview.ImageRequest.GoModuleDir != "daemon" || goSwiftReview.ImageRequest.SwiftPackageDir != "app" ||
		slices.Contains([]domain.Digest{
			preferredReview.ApprovalDigest, goReview.ApprovalDigest, swiftReview.ApprovalDigest,
		}, goSwiftReview.ApprovalDigest) {
		t.Fatalf("Go and Swift review = %+v, want both directories shown and its own approval digest",
			goSwiftReview.ImageRequest)
	}
	req.Image.GoModuleDir = ""
	req.Image.SwiftPackageDir = ""
	req.ApprovalDigest = preferredReview.ApprovalDigest
	complete, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("plan-preferred approval pass: %v", err)
	}
	if complete.Status != "complete" || complete.Profile.CommitPlan != domain.CommitPlanPlanPreferred {
		t.Fatalf("plan-preferred completion = %+v", complete)
	}
	var active domain.AutomationTrustProfile
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		active, readErr = tx.LatestTrustProfile(ctx, req.Repository)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if active.CommitPlan != domain.CommitPlanPlanPreferred ||
		active.ProfileDigest != preferredReview.Profile.ProfileDigest {
		t.Fatalf("active profile = %+v, want plan-preferred review profile", active)
	}
}

// TestOnboardProposesAuditedIssueCommentWorkflows: onboarding proposes the
// comment operation over the issue_comment workflows the audit observed, none
// included, so the one review shows them; approving that digest activates a
// profile that accepts exactly that set.
func TestOnboardProposesAuditedIssueCommentWorkflows(t *testing.T) {
	for name, listed := range map[string][]domain.IssueCommentWorkflow{
		"one workflow": {{Path: ".github/workflows/triage.yml", Digest: "sha256:triage-closure"}},
		"none":         nil,
	} {
		t.Run(name, func(t *testing.T) { onboardProposesIssueCommentWorkflows(t, listed) })
	}
}

func onboardProposesIssueCommentWorkflows(t *testing.T, listed []domain.IssueCommentWorkflow) {
	t.Helper()
	ctx := t.Context()
	st := storetest.Open(t, t.TempDir()+"/freeside.db", store.Options{})
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"example/repo","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	audit := domain.WorkflowAudit{
		Repo: "example/repo", AuditedCommitSHA: "0123456789012345678901234567890123456789",
		AuditedAt:           time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
		WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
		EffectiveTokenPerms:   domain.TokenPermissionsReadOnly,
		IssueCommentWorkflows: listed,
	}
	onboard := operations.Onboard{
		Store: st, Builder: &imageBuilderStub{store: st}, Auditor: workflowAuditorStub{audit: audit},
		Authority: authorityStub{publish.InstallationAuthority{
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: 11, InstallationID: 22,
				Account: "example", AccountID: 33, RepositoryIDs: []int64{44},
			}},
		}},
		Gate: pendingGateStub(true), Now: func() time.Time { return audit.AuditedAt },
		BaseInputs: bakedBaseInputs, Recipe: recipeAtAnyCommit,
	}
	req := operations.OnboardRequest{
		Repository: "example/repo", RepositoryID: 44, RegistrationID: 11,
		BaseRef: "main",
		Policy: operations.OnboardPolicy{
			PRExecution: domain.PRExecutionAuditedSameRepo,
			CommitPlan:  domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
			ReviewMode: domain.ReviewFreesideInvoked, ReviewConfig: "sha256:review",
			CandidateAutomationChanges: domain.AutomationChangesBlocked,
		},
		Image: projectimage.Request{
			Repository: "example/repo", RepositoryID: 44,
			CommitSHA: "0123456789012345678901234567890123456789",
			Recipe:    []byte(`{"commands":[["npm","test"]],"capture":"none"}`),
			BaseImageRef: domain.ImageRef(
				"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
			BaseBuildRef: "local/base:v1", LocalRegistryPort: 5100,
		},
	}
	review, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("review pass: %v", err)
	}
	if !review.Profile.AllowIssueCommentWorkflows || !reflect.DeepEqual(review.Profile.IssueCommentWorkflows, listed) {
		t.Fatalf("proposed profile issue_comment = %t %+v, want true %+v",
			review.Profile.AllowIssueCommentWorkflows, review.Profile.IssueCommentWorkflows, listed)
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	if listed != nil && !strings.Contains(string(body), `"issue_comment_workflows":[{"path":".github/workflows/triage.yml"`) {
		t.Fatalf("review JSON does not show the audited set under profile.issue_comment_workflows: %s", body)
	}

	req.ApprovalDigest = review.ApprovalDigest
	complete, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("approval pass: %v", err)
	}
	if complete.Status != "complete" {
		t.Fatalf("approval pass status = %q", complete.Status)
	}
	var active domain.AutomationTrustProfile
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		active, readErr = tx.LatestTrustProfile(ctx, req.Repository)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if active.ProfileDigest != review.Profile.ProfileDigest ||
		domain.EvaluateIssueCommentWorkflowDrift(active, listed) != nil {
		t.Fatalf("active profile = %+v, want the reviewed profile accepting %+v", active, listed)
	}
}

// TestOnboardReRunAdmitsExternalReviewers: the allowlist is an owner choice
// carried by the reviewed profile, so adding a reviewer to an onboarded
// repository is a second onboard pass with its own approval digest, and the
// approved profile becomes the active one the admission check reads.
func TestOnboardReRunAdmitsExternalReviewers(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t, t.TempDir()+"/freeside.db", store.Options{})
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"example/repo","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	audit := domain.WorkflowAudit{
		Repo: "example/repo", AuditedCommitSHA: "0123456789012345678901234567890123456789",
		AuditedAt:           time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
		WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	onboard := operations.Onboard{
		Store: st, Builder: &imageBuilderStub{store: st}, Auditor: workflowAuditorStub{audit: audit},
		Authority: authorityStub{publish.InstallationAuthority{
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: 11, InstallationID: 22,
				Account: "example", AccountID: 33, RepositoryIDs: []int64{44},
			}},
		}},
		Gate: pendingGateStub(true), Now: func() time.Time { return audit.AuditedAt },
		BaseInputs: bakedBaseInputs, Recipe: recipeAtAnyCommit,
	}
	req := operations.OnboardRequest{
		Repository: "example/repo", RepositoryID: 44, RegistrationID: 11,
		BaseRef: "main",
		Policy: operations.OnboardPolicy{
			PRExecution: domain.PRExecutionAuditedSameRepo,
			CommitPlan:  domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
			ReviewMode: domain.ReviewFreesideInvoked, ReviewConfig: "sha256:review",
			CandidateAutomationChanges: domain.AutomationChangesBlocked,
		},
		Image: projectimage.Request{
			Repository: "example/repo", RepositoryID: 44,
			CommitSHA: "0123456789012345678901234567890123456789",
			Recipe:    []byte(`{"commands":[["npm","test"]],"capture":"none"}`),
			BaseImageRef: domain.ImageRef(
				"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
			BaseBuildRef: "local/base:v1", LocalRegistryPort: 5100,
		},
	}
	approve := func(t *testing.T) operations.OnboardResult {
		t.Helper()
		req.ApprovalDigest = ""
		review, err := onboard.Run(ctx, req)
		if err != nil {
			t.Fatalf("review pass: %v", err)
		}
		req.ApprovalDigest = review.ApprovalDigest
		complete, err := onboard.Run(ctx, req)
		if err != nil {
			t.Fatalf("approval pass: %v", err)
		}
		if complete.Status != "complete" {
			t.Fatalf("completion = %+v", complete)
		}
		return complete
	}
	first := approve(t)
	if first.Profile.ExternalReviewers != nil {
		t.Fatalf("profile without the choice carries reviewers %v", first.Profile.ExternalReviewers)
	}

	reviewer := domain.ExternalReviewer{
		Forge: domain.ExternalReviewForgeGitHub, AccountID: 41, Login: "codex[bot]",
		Authority: domain.ExternalReviewDriveRound,
	}
	req.Policy.ExternalReviewers = []domain.ExternalReviewer{reviewer}
	// The first pass's approval does not cover the new reviewer.
	if _, err := onboard.Run(ctx, req); err == nil {
		t.Fatal("the earlier approval digest admitted a new external reviewer")
	}
	second := approve(t)
	if second.ApprovalDigest == first.ApprovalDigest {
		t.Fatal("adding an external reviewer reused the earlier approval digest")
	}
	if !slices.Equal(second.Profile.ExternalReviewers, []domain.ExternalReviewer{reviewer}) {
		t.Fatalf("reviewed profile reviewers = %v, want %v", second.Profile.ExternalReviewers, reviewer)
	}
	// The one-time review output is the profile itself, so the owner sees
	// the entry they approve.
	body, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"external_reviewers":[{"forge":"github","account_id":41,"login":"codex[bot]","authority":"drive_round"}]`) {
		t.Fatalf("review output omits the allowlist: %s", body)
	}
	var active domain.AutomationTrustProfile
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		active, readErr = tx.LatestTrustProfile(ctx, req.Repository)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if active.ProfileDigest != second.Profile.ProfileDigest {
		t.Fatalf("active profile = %s, want the re-approved %s", active.ProfileDigest, second.Profile.ProfileDigest)
	}
	if _, ok := active.ExternalReviewAuthorityFor(reviewer.Forge, reviewer.AccountID, reviewer.Login); !ok {
		t.Fatal("active profile does not admit the approved reviewer")
	}
}

// TestOnboardReRunBindsProtectedPaths pins that a protected-path widening
// is part of what the owner reviews and approves: the review output shows
// it, and an approval digest issued without it does not admit it.
func TestOnboardReRunBindsProtectedPaths(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t, t.TempDir()+"/freeside.db", store.Options{})
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"example/repo","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	audit := domain.WorkflowAudit{
		Repo: "example/repo", AuditedCommitSHA: "0123456789012345678901234567890123456789",
		AuditedAt:           time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
		WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	onboard := operations.Onboard{
		Store: st, Builder: &imageBuilderStub{store: st}, Auditor: workflowAuditorStub{audit: audit},
		Authority: authorityStub{publish.InstallationAuthority{
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: 11, InstallationID: 22,
				Account: "example", AccountID: 33, RepositoryIDs: []int64{44},
			}},
		}},
		Gate: pendingGateStub(true), Now: func() time.Time { return audit.AuditedAt },
		BaseInputs: bakedBaseInputs, Recipe: recipeAtAnyCommit,
	}
	req := operations.OnboardRequest{
		Repository: "example/repo", RepositoryID: 44, RegistrationID: 11,
		BaseRef: "main",
		Policy: operations.OnboardPolicy{
			PRExecution: domain.PRExecutionAuditedSameRepo,
			CommitPlan:  domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
			ReviewMode: domain.ReviewFreesideInvoked, ReviewConfig: "sha256:review",
			CandidateAutomationChanges: domain.AutomationChangesBlocked,
		},
		Image: projectimage.Request{
			Repository: "example/repo", RepositoryID: 44,
			CommitSHA: "0123456789012345678901234567890123456789",
			Recipe:    []byte(`{"commands":[["npm","test"]],"capture":"none"}`),
			BaseImageRef: domain.ImageRef(
				"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
			BaseBuildRef: "local/base:v1", LocalRegistryPort: 5100,
		},
	}
	review := func(t *testing.T) operations.OnboardResult {
		t.Helper()
		req.ApprovalDigest = ""
		result, err := onboard.Run(ctx, req)
		if err != nil {
			t.Fatalf("review pass: %v", err)
		}
		return result
	}
	approve := func(t *testing.T, reviewed operations.OnboardResult) operations.OnboardResult {
		t.Helper()
		req.ApprovalDigest = reviewed.ApprovalDigest
		complete, err := onboard.Run(ctx, req)
		if err != nil {
			t.Fatalf("approval pass: %v", err)
		}
		if complete.Status != "complete" {
			t.Fatalf("completion = %+v", complete)
		}
		return complete
	}
	first := approve(t, review(t))
	if !reflect.DeepEqual(first.Profile.ProtectedPaths, domain.ProtectedPathConfig{}) {
		t.Fatalf("profile without the choice widens protected paths: %+v", first.Profile.ProtectedPaths)
	}

	protected := domain.ProtectedPathConfig{
		ExtraVerificationControlPatterns: []string{".freeside/**", "scripts/check.sh"},
		ExtraMaterialityRulesPatterns:    []string{"docs/plan.md"},
	}
	req.Policy.ProtectedPaths = protected
	// The first pass's approval does not cover the widening.
	req.ApprovalDigest = first.ApprovalDigest
	if _, err := onboard.Run(ctx, req); err == nil ||
		!strings.Contains(err.Error(), "does not match proposed review") {
		t.Fatalf("earlier approval digest with a protected-path widening: err = %v, want a digest mismatch", err)
	}
	reviewed := review(t)
	if reviewed.ApprovalDigest == first.ApprovalDigest {
		t.Fatal("adding protected paths reused the earlier approval digest")
	}
	// The one-time review output is the profile itself, so the owner sees
	// the patterns they approve.
	body, err := json.Marshal(reviewed)
	if err != nil {
		t.Fatal(err)
	}
	var shown struct {
		Profile struct {
			ProtectedPaths domain.ProtectedPathConfig `json:"protected_paths"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(body, &shown); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shown.Profile.ProtectedPaths, protected) {
		t.Fatalf("review output profile.protected_paths = %+v, want %+v", shown.Profile.ProtectedPaths, protected)
	}
	second := approve(t, reviewed)
	if second.ApprovalDigest != reviewed.ApprovalDigest {
		t.Fatalf("approval digest = %s, want the reviewed %s", second.ApprovalDigest, reviewed.ApprovalDigest)
	}
	var active domain.AutomationTrustProfile
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		active, readErr = tx.LatestTrustProfile(ctx, req.Repository)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(active.ProtectedPaths, protected) {
		t.Fatalf("active profile protected paths = %+v, want %+v", active.ProtectedPaths, protected)
	}
}

func TestOnboardRequiresOneDigestBoundReviewBeforeBuildAndActivation(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, t.TempDir()+"/freeside.db", store.Options{})
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"example/repo","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	audit := domain.WorkflowAudit{
		Repo: "example/repo", AuditedCommitSHA: "0123456789012345678901234567890123456789",
		AuditedAt:           time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordWorkflowAudit(ctx, audit)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	builder := &imageBuilderStub{store: st}
	authority := &authorityStub{authority: publish.InstallationAuthority{
		TrustedInstallations: []publish.TrustedInstallation{{
			RegistrationID: 11, InstallationID: 22,
			Account: "example", AccountID: 33, RepositoryIDs: []int64{44},
		}},
	}}
	gate := &mutableInstallationGate{allowed: true}
	onboard := operations.Onboard{
		Store: st, Builder: builder, Auditor: workflowAuditorStub{audit: audit},
		Now: func() time.Time { return audit.AuditedAt }, Gate: gate,
		Authority: authority, BaseInputs: unreadableBaseInputs, Recipe: recipeAtAnyCommit,
	}
	req := operations.OnboardRequest{
		Repository: "example/repo", RepositoryID: 44, RegistrationID: 11,
		BaseRef: "main",
		Policy: operations.OnboardPolicy{
			PRExecution: domain.PRExecutionAuditedSameRepo,
			CommitPlan:  domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
			ReviewMode: domain.ReviewFreesideInvoked, ReviewConfig: "sha256:review",
			CandidateAutomationChanges: domain.AutomationChangesBlocked,
		},
		Image: projectimage.Request{
			Repository: "example/repo", RepositoryID: 44,
			CommitSHA: "0123456789012345678901234567890123456789",
			Recipe:    []byte(`{"commands":[["npm","test"]],"capture":"none"}`),
			BaseImageRef: domain.ImageRef(
				"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
			BaseBuildRef: "local/base:v1", LocalRegistryPort: 5100,
			DNS: []string{"1.1.1.1"},
		},
	}
	review, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("review pass: %v", err)
	}
	if review.Status != "review_required" || builder.calls != 0 {
		t.Fatalf("review result = %+v, builder calls = %d", review, builder.calls)
	}
	if review.ImageRequest.ImageName != "freeside-project-example-repo" ||
		review.ImageRequest.RefTag != "v1" {
		t.Fatalf("review image destination = %s:%s, want normalized default",
			review.ImageRequest.ImageName, review.ImageRequest.RefTag)
	}
	stale := req
	stale.Image.CommitSHA = "1123456789012345678901234567890123456789"
	if _, err := onboard.Run(ctx, stale); err == nil {
		t.Fatal("onboard accepted an audit from a different project-image commit")
	}
	req.ApprovalDigest = review.ApprovalDigest
	for _, tc := range []struct {
		name   string
		mutate func(*projectimage.Request)
	}{
		{"commit", func(image *projectimage.Request) {
			image.CommitSHA = "1123456789012345678901234567890123456789"
		}},
		{"recipe", func(image *projectimage.Request) {
			image.Recipe = []byte(`{"commands":[["go","test","./..."]],"capture":"none"}`)
		}},
		{"base image", func(image *projectimage.Request) {
			image.BaseImageRef = domain.ImageRef(
				"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("c", 64),
			)
		}},
		{"base build ref", func(image *projectimage.Request) {
			image.BaseBuildRef = "local/base:changed"
		}},
		{"registry", func(image *projectimage.Request) {
			image.Registry = "registry.example.test/project"
			image.LocalRegistryPort = 0
		}},
		{"registry port", func(image *projectimage.Request) {
			image.LocalRegistryPort = 5101
		}},
		{"image name", func(image *projectimage.Request) {
			image.ImageName = "changed-image"
		}},
		{"ref tag", func(image *projectimage.Request) {
			image.RefTag = "changed"
		}},
		{"dns", func(image *projectimage.Request) {
			image.DNS = []string{"8.8.8.8"}
		}},
		{"build proxy", func(image *projectimage.Request) {
			image.BuildProxy = "http://proxy.example.test"
		}},
	} {
		t.Run("stale approval changed "+tc.name, func(t *testing.T) {
			candidate := req
			tc.mutate(&candidate.Image)
			candidateAudit := audit
			candidateAudit.AuditedCommitSHA = candidate.Image.CommitSHA
			onboard.Auditor = workflowAuditorStub{audit: candidateAudit}
			calls := builder.calls
			if _, err := onboard.Run(ctx, candidate); err == nil ||
				!strings.Contains(err.Error(), "approval digest") {
				t.Fatalf("changed %s stale approval error = %v", tc.name, err)
			}
			if builder.calls != calls {
				t.Fatalf("changed %s reached image build before approval", tc.name)
			}
		})
	}
	onboard.Auditor = workflowAuditorStub{audit: audit}
	builder.mutateRequest = func(image *projectimage.Request) {
		image.Recipe[0] = '['
		image.DNS[0] = "9.9.9.9"
	}
	builder.store = nil
	builder.image = domain.ProjectImage{}
	if _, err := onboard.Run(ctx, req); err == nil ||
		!strings.Contains(err.Error(), "project image result is not bound") {
		t.Fatalf("builder-mutated image request error = %v", err)
	}
	if req.Image.Recipe[0] != '{' || req.Image.DNS[0] != "1.1.1.1" ||
		review.ImageRequest.DNS[0] != "1.1.1.1" {
		t.Fatal("builder mutation escaped its owned image-request snapshot")
	}
	builder.mutateRequest = nil
	builder.image = domain.ProjectImage{}
	builder.preparationCommand = []string{"/tmp/not-the-approved-preparation-command"}
	if _, err := onboard.Run(ctx, req); err == nil {
		t.Fatal("onboard accepted a substituted project-image preparation command")
	}
	builder.preparationCommand = nil
	// A result must record this binary's environment: one with none could
	// never be reused, and one naming another builder's preparation sources
	// would be reused on evidence this binary did not produce.
	builder.image = domain.ProjectImage{}
	builder.environment = func() *domain.ProjectImageEnvironment { return nil }
	if _, err := onboard.Run(ctx, req); err == nil ||
		!strings.Contains(err.Error(), "does not record this builder's environment") {
		t.Fatalf("project image without an environment error = %v", err)
	}
	builder.image = domain.ProjectImage{}
	builder.environment = func() *domain.ProjectImageEnvironment {
		environment := stubProjectImageEnvironment()
		environment.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("9", 64))
		return environment
	}
	if _, err := onboard.Run(ctx, req); err == nil ||
		!strings.Contains(err.Error(), "does not record this builder's environment") {
		t.Fatalf("project image from another builder error = %v", err)
	}
	builder.image = domain.ProjectImage{}
	builder.environment = nil
	builder.image, err = domain.NewProjectImage(domain.ProjectImageInput{
		Repository: req.Repository, RepositoryID: req.RepositoryID,
		CommitSHA:          req.Image.CommitSHA,
		RecipeDigest:       verify.RecipeDigest(req.Image.Recipe),
		PreparationCommand: []string{projectimage.PreparationPath},
		BaseImageRef:       req.Image.BaseImageRef,
		ImageRef: domain.ImageRef(
			"registry.example.test/other-image@sha256:" + strings.Repeat("b", 64)),
		Environment: stubProjectImageEnvironment(),
	})
	if err != nil {
		t.Fatal(err)
	}
	builder.store = st
	if _, err := onboard.Run(ctx, req); err == nil ||
		!strings.Contains(err.Error(), "approved destination") {
		t.Fatalf("foreign project-image destination error = %v", err)
	}
	builder.image = domain.ProjectImage{}
	builder.store = nil
	if _, err := onboard.Run(ctx, req); err == nil {
		t.Fatal("onboard activated trust without a durably recorded project image")
	}
	builder.store = st
	builder.afterBuild = func() {
		authority.authority.TrustedInstallations = nil
	}
	if _, err := onboard.Run(ctx, req); err == nil {
		t.Fatal("onboard activated trust after authority changed during image build")
	}
	authority.authority.TrustedInstallations = []publish.TrustedInstallation{{
		RegistrationID: 11, InstallationID: 22,
		Account: "example", AccountID: 33, RepositoryIDs: []int64{44},
	}}
	builder.afterBuild = func() {
		gate.allowed = false
	}
	if _, err := onboard.Run(ctx, req); err == nil {
		t.Fatal("onboard activated trust after reconciled grant changed during image build")
	}
	gate.allowed = true
	builder.afterBuild = nil
	complete, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatalf("approval pass: %v", err)
	}
	if complete.Status != "complete" || builder.calls != 9 {
		t.Fatalf("complete result = %+v, builder calls = %d", complete, builder.calls)
	}
	var active domain.AutomationTrustProfile
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		active, err = tx.LatestTrustProfile(ctx, req.Repository)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if active.ProfileDigest != review.Profile.ProfileDigest {
		t.Fatalf("active profile = %s, want %s", active.ProfileDigest, review.Profile.ProfileDigest)
	}
	// The built image seeds the Claude agent base's admitted-image pointer.
	var (
		pointer domain.AdmittedImagePointer
		found   bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pointer, _, found, err = tx.GetAdmittedImagePointer(ctx, req.RepositoryID, domain.HarnessClientClaudeCode)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !found || complete.ProjectImage == nil || pointer.ImageID != complete.ProjectImage.ID ||
		pointer.ImageRef != complete.ProjectImage.ImageRef {
		t.Fatalf("admitted image pointer = %+v found=%v, want the built image %+v",
			pointer, found, complete.ProjectImage)
	}
}

func TestOnboardPromotesPendingInstallationOnlyAfterApproval(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, t.TempDir()+"/freeside.db", store.Options{})
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"example/repo","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	auditedAt := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	audit := domain.WorkflowAudit{
		Repo: "example/repo", AuditedCommitSHA: "0123456789012345678901234567890123456789",
		AuditedAt: auditedAt, WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordWorkflowAudit(ctx, audit)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	installationID := int64(22)
	unassignedInstallationID := int64(0)
	documents := &documentStoreStub{document: publish.InstallationAuthorityDocument{
		Version: 1,
		Registrations: []publish.InstallationAuthorityEntry{{
			RegistrationID: 11, ActiveEpoch: 3, DurableIntentRevision: 7,
			TrustedOwners:        []publish.TrustedOwnerRecord{},
			TrustedInstallations: []publish.TrustedInstallationRecord{},
			Pending: &publish.PendingEnvelopeRecord{
				ActiveEpoch: 3, DurableIntentRevision: 7,
				ExpectedAccount: "example", ExpectedAccountID: 33,
				InstallationID:       &unassignedInstallationID,
				CurrentRepositoryIDs: []int64{}, ExpectedRepositoryIDs: []int64{44},
				RequiredRepositoryMode: "selected", ExpiresAt: auditedAt.Add(time.Hour),
			},
		}},
	}}
	builder := &imageBuilderStub{store: st}
	onboard := operations.Onboard{
		Store: st, Builder: builder, Documents: documents,
		Auditor: workflowAuditorStub{audit: audit},
		Now:     func() time.Time { return auditedAt }, Gate: pendingGateStub(true),
		BaseInputs: unreadableBaseInputs, Recipe: recipeAtAnyCommit,
		Authority: authorityStub{publish.InstallationAuthority{
			ActiveEpoch: 3, DurableIntentRevision: 7,
			Pending: &publish.PendingInstallationEnvelope{
				ActiveEpoch: 3, DurableIntentRevision: 7, RegistrationID: 11,
				ExpectedAccount: "example", ExpectedAccountID: 33,
				InstallationID: 0, CurrentRepositoryIDs: []int64{},
				ExpectedRepositoryIDs:  []int64{44},
				RequiredRepositoryMode: "selected", ExpiresAt: auditedAt.Add(time.Hour),
			},
		}},
	}
	req := operations.OnboardRequest{
		Repository: "example/repo", RepositoryID: 44, RegistrationID: 11,
		BaseRef: "main",
		Policy: operations.OnboardPolicy{
			PRExecution: domain.PRExecutionAuditedSameRepo,
			CommitPlan:  domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
			ReviewMode: domain.ReviewFreesideInvoked, ReviewConfig: "sha256:review",
			CandidateAutomationChanges: domain.AutomationChangesBlocked,
		},
		Image: projectimage.Request{
			Repository: "example/repo", RepositoryID: 44,
			CommitSHA: "0123456789012345678901234567890123456789",
			Recipe:    []byte(`{"commands":[["npm","test"]],"capture":"none"}`),
			BaseImageRef: domain.ImageRef(
				"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
			BaseBuildRef: "local/base:v1", LocalRegistryPort: 5100,
			ImageName: "test-image", RefTag: "v1",
		},
	}
	review, err := onboard.Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if documents.document.Registrations[0].Pending == nil ||
		len(documents.document.Registrations[0].TrustedInstallations) != 0 {
		t.Fatal("review pass promoted the non-authorizing pending installation")
	}
	req.ApprovalDigest = review.ApprovalDigest
	onboard.Authority = authorityStub{publish.InstallationAuthority{
		ActiveEpoch: 3, DurableIntentRevision: 8,
		Pending: &publish.PendingInstallationEnvelope{
			ActiveEpoch: 3, DurableIntentRevision: 8, RegistrationID: 11,
			ExpectedAccount: "example", ExpectedAccountID: 33,
			InstallationID: 0, CurrentRepositoryIDs: []int64{},
			ExpectedRepositoryIDs:  []int64{44},
			RequiredRepositoryMode: "selected", ExpiresAt: auditedAt.Add(time.Hour),
		},
	}}
	if _, err := onboard.Run(ctx, req); err == nil {
		t.Fatal("onboard accepted approval for a replacement pending revision")
	}
	onboard.Gate = &pendingGateSequence{results: []bool{true, false}}
	// The applying pass rechecks the promoted binding before activation, so
	// from here the authority reads what promotion writes.
	onboard.Authority = documents
	if _, err := onboard.Run(ctx, req); err == nil {
		t.Fatal("onboard accepted readiness that changed before promotion")
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.LatestTrustProfile(ctx, req.Repository)
		return err
	}); err == nil {
		t.Fatal("failed pending promotion left an active trust profile")
	}
	onboard.Gate = pendingGateStub(true)
	if _, err := onboard.Run(ctx, req); err != nil {
		t.Fatal(err)
	}
	entry := documents.document.Registrations[0]
	if entry.Pending != nil || len(entry.TrustedInstallations) != 1 ||
		entry.TrustedInstallations[0].InstallationID != installationID {
		t.Fatalf("approved authority = %+v", entry)
	}
}

// carryFixture is a repository onboarded through one trusted installation,
// built by newReviewFixture or newCarryFixture.
type carryFixture struct {
	st      *store.Store
	dbPath  string
	builder *imageBuilderStub
	gate    *mutableInstallationGate
	onboard operations.Onboard
	req     operations.OnboardRequest
	audit   domain.WorkflowAudit
	first   operations.OnboardResult
}

const (
	carryCommitA = "0123456789012345678901234567890123456789"
	carryCommitB = "2223456789012345678901234567890123456789"
	carryCommitC = "3333456789012345678901234567890123456789"
)

// newReviewFixture is a repository with no active profile or admitted image,
// whose base ref's tip is carryCommitA, before its first review.
func newReviewFixture(t *testing.T) *carryFixture {
	t.Helper()
	dbPath := t.TempDir() + "/freeside.db"
	st := storetest.Open(t, dbPath, store.Options{})
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"example/repo","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	audit := domain.WorkflowAudit{
		Repo: "example/repo", AuditedCommitSHA: carryCommitA,
		AuditedAt:           time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
		WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	f := &carryFixture{
		st: st, dbPath: dbPath, builder: &imageBuilderStub{store: st},
		gate: &mutableInstallationGate{allowed: true}, audit: audit,
	}
	f.onboard = operations.Onboard{
		Store: st, Builder: f.builder, Auditor: workflowAuditorStub{audit: audit},
		Authority: authorityStub{publish.InstallationAuthority{
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: 11, InstallationID: 22,
				Account: "example", AccountID: 33, RepositoryIDs: []int64{44},
			}},
		}},
		Gate: f.gate, BaseInputs: bakedBaseInputs, Recipe: recipeAtAnyCommit,
		Now: func() time.Time { return audit.AuditedAt },
	}
	f.req = operations.OnboardRequest{
		Repository: "example/repo", RepositoryID: 44, RegistrationID: 11,
		BaseRef: "main",
		Policy: operations.OnboardPolicy{
			PRExecution: domain.PRExecutionAuditedSameRepo,
			CommitPlan:  domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
			ReviewMode: domain.ReviewFreesideInvoked, ReviewConfig: "sha256:review",
			CandidateAutomationChanges: domain.AutomationChangesBlocked,
		},
		Image: projectimage.Request{
			Repository: "example/repo", RepositoryID: 44, CommitSHA: carryCommitA,
			Recipe: []byte(`{"commands":[["npm","test"]],"capture":"none"}`),
			BaseImageRef: domain.ImageRef(
				"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
			BaseBuildRef: "local/base:v1", LocalRegistryPort: 5100,
		},
	}
	return f
}

// moveTip moves the base ref's tip to commit with the workflows unchanged.
func (f *carryFixture) moveTip(commit string) {
	f.audit.AuditedCommitSHA = commit
	f.onboard.Auditor = workflowAuditorStub{audit: f.audit}
}

// newCarryFixture is a repository onboarded and activated at carryCommitA,
// whose base ref has since moved to carryCommitB with no change to its
// workflows, before its review there.
func newCarryFixture(t *testing.T) *carryFixture {
	t.Helper()
	ctx := t.Context()
	f := newReviewFixture(t)
	review, err := f.onboard.Run(ctx, f.req)
	if err != nil {
		t.Fatalf("first review: %v", err)
	}
	if review.Changes != nil {
		t.Fatalf("first review of a repository with no active profile shows changes %+v", review.Changes)
	}
	f.req.ApprovalDigest = review.ApprovalDigest
	f.first, err = f.onboard.Run(ctx, f.req)
	if err != nil || f.first.Status != "complete" || f.builder.calls != 1 {
		t.Fatalf("first approval = %+v, %v; builder calls %d", f.first, err, f.builder.calls)
	}
	// The base moves to a source-only commit, and the operator reviews again.
	f.req.ApprovalDigest = ""
	f.req.Image.CommitSHA = carryCommitB
	f.moveTip(carryCommitB)
	// A new build is only ever of the new commit.
	f.builder.image = domain.ProjectImage{}
	return f
}

func (f *carryFixture) activations(t *testing.T) int {
	t.Helper()
	raw, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var count int
	if err := raw.QueryRowContext(t.Context(),
		`SELECT count(*) FROM trust_profile_activations WHERE repo = ?`, f.req.Repository,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *carryFixture) pointerImage(t *testing.T) domain.Digest {
	t.Helper()
	var pointer domain.AdmittedImagePointer
	if err := f.st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		pointer, _, _, err = tx.GetAdmittedImagePointer(t.Context(), f.req.RepositoryID, domain.HarnessClientClaudeCode)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pointer.ImageID
}

func (f *carryFixture) activeDigest(t *testing.T) domain.Digest {
	t.Helper()
	var active domain.AutomationTrustProfile
	if err := f.st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		active, err = tx.LatestTrustProfile(t.Context(), f.req.Repository)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return active.ProfileDigest
}

// TestOnboardCarriesApprovalAcrossSourceOnlyCommit: a review after the base
// moved, with the same workflows and policy and an admitted image that
// serves the new commit, completes with no approval and no build, and
// re-activates the approved profile.
func TestOnboardCarriesApprovalAcrossSourceOnlyCommit(t *testing.T) {
	f := newCarryFixture(t)
	before := f.activations(t)
	carried, err := f.onboard.Run(t.Context(), f.req)
	if err != nil {
		t.Fatalf("carried review: %v", err)
	}
	if carried.Status != "complete" || !carried.Carried || carried.Changes != nil {
		t.Fatalf("carried review = status %q carried %t changes %+v", carried.Status, carried.Carried, carried.Changes)
	}
	if f.builder.calls != 1 {
		t.Fatalf("carried review built an image; builder calls = %d", f.builder.calls)
	}
	if carried.ProjectImage == nil || carried.ProjectImage.ID != f.first.ProjectImage.ID ||
		f.pointerImage(t) != f.first.ProjectImage.ID {
		t.Fatalf("carried image = %+v, want the admitted %s", carried.ProjectImage, f.first.ProjectImage.ID)
	}
	if carried.ImageRequest.CommitSHA != carryCommitB ||
		carried.Profile.ProfileDigest != f.first.Profile.ProfileDigest {
		t.Fatalf("carried review binds commit %s profile %s", carried.ImageRequest.CommitSHA, carried.Profile.ProfileDigest)
	}
	if got := f.activations(t); got != before+1 {
		t.Fatalf("activation rows = %d, want %d", got, before+1)
	}
}

// TestOnboardReviewNamesWhatTheApprovalDoesNotCover: each change the owner
// has not approved keeps the review required, appends no activation, and
// names the change.
func TestOnboardReviewNamesWhatTheApprovalDoesNotCover(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*carryFixture)
		want   func(operations.OnboardChange) bool
	}{
		{"audited privilege", func(f *carryFixture) {
			f.audit.OIDCAvailable = true
			f.onboard.Auditor = workflowAuditorStub{audit: f.audit}
		}, func(c operations.OnboardChange) bool {
			return c.Field == "allow_oidc" && string(c.Approved) == "false" && string(c.Proposed) == "true"
		}},
		{"recipe", func(f *carryFixture) {
			f.req.Image.Recipe = []byte(`{"commands":[["npm","run","check"]],"capture":"none"}`)
		}, func(c operations.OnboardChange) bool { return c.Field == "image.recipe_digest" }},
		{"manifests", func(f *carryFixture) {
			f.onboard.BaseInputs = func(
				ctx context.Context, commit string, layout domain.ProjectImageInputLayout,
			) (domain.ProjectImageBaseInputs, error) {
				inputs, err := bakedBaseInputs(ctx, commit, layout)
				inputs.PackageLockSHA256 = strings.Repeat("3", 64)
				return inputs, err
			}
		}, func(c operations.OnboardChange) bool {
			return strings.Contains(c.Reason, "package-lock.json at the base differs")
		}},
		{"unreadable commit", func(f *carryFixture) {
			f.onboard.BaseInputs = unreadableBaseInputs
		}, func(c operations.OnboardChange) bool {
			return strings.Contains(c.Reason, "checkout lacks the commit")
		}},
		{"toolchain directory", func(f *carryFixture) {
			f.req.Image.GoModuleDir = "daemon"
		}, func(c operations.OnboardChange) bool {
			return strings.Contains(c.Reason, `the request declares "daemon"`)
		}},
		{"pending installation", func(f *carryFixture) {
			f.onboard.Authority = authorityStub{publish.InstallationAuthority{
				ActiveEpoch: 3, DurableIntentRevision: 7,
				Pending: &publish.PendingInstallationEnvelope{
					ActiveEpoch: 3, DurableIntentRevision: 7, RegistrationID: 11,
					ExpectedAccount: "example", ExpectedAccountID: 33,
					CurrentRepositoryIDs: []int64{}, ExpectedRepositoryIDs: []int64{44},
					RequiredRepositoryMode: "selected", ExpiresAt: f.audit.AuditedAt.Add(time.Hour),
				},
			}}
			f.onboard.Gate = pendingGateStub(true)
		}, func(c operations.OnboardChange) bool { return strings.Contains(c.Reason, "pending intent") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCarryFixture(t)
			tc.mutate(f)
			before := f.activations(t)
			review, err := f.onboard.Run(t.Context(), f.req)
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			if review.Status != "review_required" || review.Carried || !slices.ContainsFunc(review.Changes, tc.want) {
				t.Fatalf("review = status %q carried %t changes %+v", review.Status, review.Carried, review.Changes)
			}
			if got := f.activations(t); got != before || f.builder.calls != 1 {
				t.Fatalf("activation rows %d (want %d), builder calls %d (want 1)", got, before, f.builder.calls)
			}
		})
	}
}

// TestOnboardDoesNotCarryAnotherBinarysImage: an image at the review's own
// commit passes AdmissibleAt whatever built it, so the carry checks the
// preparation sources itself.
func TestOnboardDoesNotCarryAnotherBinarysImage(t *testing.T) {
	f := newCarryFixture(t)
	ctx := t.Context()
	environment := stubProjectImageEnvironment()
	environment.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("9", 64))
	var pointed domain.ProjectImage
	if err := f.st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		_, pointed, _, err = tx.GetAdmittedImagePointer(ctx, f.req.RepositoryID, domain.HarnessClientClaudeCode)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: pointed.Repository, RepositoryID: pointed.RepositoryID,
		CommitSHA: f.req.Image.CommitSHA, RecipeDigest: pointed.RecipeDigest,
		PreparationCommand: pointed.PreparationCommand, BaseImageRef: pointed.BaseImageRef,
		ImageRef: domain.ImageRef(strings.Replace(
			string(pointed.ImageRef), strings.Repeat("b", 64), strings.Repeat("c", 64), 1)),
		Environment: environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordProjectImage(ctx, stale); err != nil {
			return err
		}
		return tx.AdvanceAdmittedImagePointer(ctx, domain.AdmittedImagePointer{
			RepositoryID: stale.RepositoryID, Repository: stale.Repository,
			Harness: domain.HarnessClientClaudeCode, ImageID: stale.ID, ImageRef: stale.ImageRef,
			AdvancedAt: f.audit.AuditedAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	review, err := f.onboard.Run(ctx, f.req)
	if err != nil || review.Status != "review_required" || review.Carried ||
		!slices.ContainsFunc(review.Changes, func(c operations.OnboardChange) bool {
			return strings.Contains(c.Reason, "this binary's toolchain")
		}) {
		t.Fatalf("review over another binary's image = %+v, %v", review, err)
	}
}

// TestOnboardCarryRechecksTheTrustedInstallation: a carried review activates
// only after the same installation re-check an approval runs.
func TestOnboardCarryRechecksTheTrustedInstallation(t *testing.T) {
	f := newCarryFixture(t)
	f.gate.allowed = false
	before := f.activations(t)
	if _, err := f.onboard.Run(t.Context(), f.req); err == nil ||
		!strings.Contains(err.Error(), "trusted installation changed before profile activation") {
		t.Fatalf("carry with a revoked grant: err = %v", err)
	}
	if got := f.activations(t); got != before {
		t.Fatalf("activation rows = %d, want %d", got, before)
	}
}

// TestOnboardApprovalReusesAServingImage: an approval of a changed policy
// whose admitted image still serves the commit activates without a build and
// leaves the pointer alone; one whose image does not serve builds as before.
func TestOnboardApprovalReusesAServingImage(t *testing.T) {
	approve := func(t *testing.T, f *carryFixture) operations.OnboardResult {
		t.Helper()
		review, err := f.onboard.Run(t.Context(), f.req)
		if err != nil || review.Status != "review_required" {
			t.Fatalf("review = %+v, %v", review, err)
		}
		f.req.ApprovalDigest = review.ApprovalDigest
		complete, err := f.onboard.Run(t.Context(), f.req)
		if err != nil || complete.Status != "complete" || complete.Carried {
			t.Fatalf("approval = %+v, %v", complete, err)
		}
		if f.activeDigest(t) != review.Profile.ProfileDigest {
			t.Fatalf("active profile = %s, want the approved %s", f.activeDigest(t), review.Profile.ProfileDigest)
		}
		return complete
	}
	t.Run("serving image", func(t *testing.T) {
		f := newCarryFixture(t)
		f.req.Policy.CommitPlan = domain.CommitPlanPlanPreferred
		complete := approve(t, f)
		if f.builder.calls != 1 || complete.ProjectImage.ID != f.first.ProjectImage.ID ||
			f.pointerImage(t) != f.first.ProjectImage.ID {
			t.Fatalf("approval built %d image(s) and returned %s, want the admitted %s",
				f.builder.calls-1, complete.ProjectImage.ID, f.first.ProjectImage.ID)
		}
	})
	t.Run("moved destination", func(t *testing.T) {
		f := newCarryFixture(t)
		f.req.Image.ImageName = "moved-image"
		complete := approve(t, f)
		if f.builder.calls != 2 || complete.ProjectImage.CommitSHA != carryCommitB ||
			f.pointerImage(t) != complete.ProjectImage.ID {
			t.Fatalf("approval at a moved destination: builder calls %d, image %+v, pointer %s",
				f.builder.calls, complete.ProjectImage, f.pointerImage(t))
		}
	})
}

// TestOnboardNamesTheMovedTip: a review pinned to a commit the live tip has
// left reports the tip, so the caller can review it.
func TestOnboardNamesTheMovedTip(t *testing.T) {
	f := newCarryFixture(t)
	f.req.Image.CommitSHA = carryCommitA
	_, err := f.onboard.Run(t.Context(), f.req)
	var moved *operations.MovedTipError
	if !errors.As(err, &moved) || moved.Tip != carryCommitB || moved.Commit != carryCommitA {
		t.Fatalf("stale commit error = %v, want MovedTipError{%s, %s}", err, carryCommitB, carryCommitA)
	}
}

// TestOnboardApprovalAppliesAtAMovedTip: an approval of a review whose
// commit the base ref has since left applies at the live tip when the tip's
// workflow audit and recipe are the reviewed ones. The image is built or
// reused at the tip, and the result keeps the owner's digest and names the
// reviewed commit.
func TestOnboardApprovalAppliesAtAMovedTip(t *testing.T) {
	t.Run("first onboarding builds at the tip", func(t *testing.T) {
		f := newReviewFixture(t)
		review, err := f.onboard.Run(t.Context(), f.req)
		if err != nil || review.Status != "review_required" {
			t.Fatalf("review = %+v, %v", review, err)
		}
		f.req.ApprovalDigest = review.ApprovalDigest
		f.moveTip(carryCommitB)
		complete, err := f.onboard.Run(t.Context(), f.req)
		if err != nil || complete.Status != "complete" || complete.Carried {
			t.Fatalf("approval at a moved tip = %+v, %v", complete, err)
		}
		if f.builder.calls != 1 || complete.ProjectImage == nil ||
			complete.ProjectImage.CommitSHA != carryCommitB ||
			f.pointerImage(t) != complete.ProjectImage.ID {
			t.Fatalf("approval built %d image(s), image %+v, pointer %s; want one build at the tip",
				f.builder.calls, complete.ProjectImage, f.pointerImage(t))
		}
		assertAppliedAtTip(t, f, review, complete, carryCommitA, carryCommitB)
	})
	t.Run("an onboarded repository reuses the serving image", func(t *testing.T) {
		f := newCarryFixture(t)
		f.req.Policy.CommitPlan = domain.CommitPlanPlanPreferred
		review, err := f.onboard.Run(t.Context(), f.req)
		if err != nil || review.Status != "review_required" {
			t.Fatalf("review = %+v, %v", review, err)
		}
		f.req.ApprovalDigest = review.ApprovalDigest
		f.moveTip(carryCommitC)
		complete, err := f.onboard.Run(t.Context(), f.req)
		if err != nil || complete.Status != "complete" || complete.Carried {
			t.Fatalf("approval at a moved tip = %+v, %v", complete, err)
		}
		// The reused image keeps its own build commit; it serves the tip.
		if f.builder.calls != 1 || complete.ProjectImage == nil ||
			complete.ProjectImage.ID != f.first.ProjectImage.ID ||
			f.pointerImage(t) != f.first.ProjectImage.ID {
			t.Fatalf("approval built %d image(s) and returned %+v, want the admitted %s",
				f.builder.calls-1, complete.ProjectImage, f.first.ProjectImage.ID)
		}
		assertAppliedAtTip(t, f, review, complete, carryCommitB, carryCommitC)
	})
}

func assertAppliedAtTip(
	t *testing.T, f *carryFixture, review, complete operations.OnboardResult, reviewed, tip string,
) {
	t.Helper()
	if complete.ImageRequest.CommitSHA != tip || complete.ReviewedCommitSHA != reviewed ||
		complete.ApprovalDigest != review.ApprovalDigest {
		t.Fatalf("approval at a moved tip names image commit %s, reviewed commit %s, digest %s; "+
			"want %s, %s, %s", complete.ImageRequest.CommitSHA, complete.ReviewedCommitSHA,
			complete.ApprovalDigest, tip, reviewed, review.ApprovalDigest)
	}
	if review.ReviewedCommitSHA != "" {
		t.Fatalf("review names reviewed commit %s, want none", review.ReviewedCommitSHA)
	}
	if got := f.activeDigest(t); got != review.Profile.ProfileDigest {
		t.Fatalf("active profile = %s, want the approved %s", got, review.Profile.ProfileDigest)
	}
}

// TestOnboardApprovalRefusesAMovedTipThatChangedWhatItCovers: an approval
// at a moved tip whose workflow audit, recipe, or onboarding flags differ
// from the review refuses with the tip, names what differs, and builds,
// promotes, and activates nothing.
func TestOnboardApprovalRefusesAMovedTipThatChangedWhatItCovers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *carryFixture)
		want   func(operations.OnboardChange) bool
	}{
		{"workflow audit", func(t *testing.T, f *carryFixture) {
			evidence, err := domain.NewWorkflowAuditEvidence([]byte(
				`{"version":"freeside-workflow-audit/v2","repo":"example/repo","workflows":["ci.yml"]}`,
			))
			if err != nil {
				t.Fatal(err)
			}
			f.audit.WorkflowAuditDigest, f.audit.Evidence = evidence.Digest(), &evidence
			f.onboard.Auditor = workflowAuditorStub{audit: f.audit}
		}, func(c operations.OnboardChange) bool { return c.Field == "workflow_audit_digest" }},
		{"recipe", func(_ *testing.T, f *carryFixture) {
			f.onboard.Recipe = func(context.Context, string) ([]byte, error) {
				return []byte(`{"commands":[["npm","run","check"]],"capture":"none"}`), nil
			}
		}, func(c operations.OnboardChange) bool { return c.Field == "image.recipe_digest" }},
		{"unreadable recipe", func(_ *testing.T, f *carryFixture) {
			f.onboard.Recipe = func(context.Context, string) ([]byte, error) {
				return nil, errors.New("checkout lacks the commit")
			}
		}, func(c operations.OnboardChange) bool {
			return strings.Contains(c.Reason, "recipe at the tip "+carryCommitB) &&
				strings.Contains(c.Reason, "checkout lacks the commit")
		}},
		{"onboarding flag", func(_ *testing.T, f *carryFixture) {
			f.req.Policy.CommitPlan = domain.CommitPlanPlanPreferred
		}, func(c operations.OnboardChange) bool {
			return strings.Contains(c.Reason, "does not match the review recomputed at the tip")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t)
			review, err := f.onboard.Run(t.Context(), f.req)
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			f.req.ApprovalDigest = review.ApprovalDigest
			f.moveTip(carryCommitB)
			tc.mutate(t, f)
			_, err = f.onboard.Run(t.Context(), f.req)
			var moved *operations.MovedTipError
			if !errors.As(err, &moved) || moved.Tip != carryCommitB || moved.Commit != carryCommitA ||
				!slices.ContainsFunc(moved.Changes, tc.want) {
				t.Fatalf("approval at a changed tip: err = %v, changes %+v", err, changesOf(moved))
			}
			if f.builder.calls != 0 || f.activations(t) != 0 {
				t.Fatalf("refused approval built %d image(s) and left %d activation(s)",
					f.builder.calls, f.activations(t))
			}
		})
	}
}

// TestOnboardRefusedMovedTipApprovalLeavesTheIntentPending: a refused
// approval at a moved tip decides before promotion, so a pending intent
// stays pending and nothing is built or activated.
func TestOnboardRefusedMovedTipApprovalLeavesTheIntentPending(t *testing.T) {
	f := newPendingFixture(t)
	audit := f.onboard.Auditor.(workflowAuditorStub).audit
	audit.AuditedCommitSHA = carryCommitB
	f.onboard.Auditor = workflowAuditorStub{audit: audit}
	f.onboard.Recipe = func(context.Context, string) ([]byte, error) {
		return []byte(`{"commands":[["npm","run","check"]],"capture":"none"}`), nil
	}
	_, err := f.onboard.Run(t.Context(), f.req)
	var moved *operations.MovedTipError
	if !errors.As(err, &moved) || len(moved.Changes) == 0 {
		t.Fatalf("approval at a tip with a changed recipe: err = %v", err)
	}
	if f.trusted() || f.builder.calls != 0 || f.activeProfile(t) {
		t.Fatalf("refused approval: trusted %t, builds %d, active profile %t",
			f.trusted(), f.builder.calls, f.activeProfile(t))
	}
}

func changesOf(moved *operations.MovedTipError) []operations.OnboardChange {
	if moved == nil {
		return nil
	}
	return moved.Changes
}

// InstallationAuthority serves the stub's document the way the authority
// store does, so an onboarding run reads back the binding its own promotion
// wrote.
func (s *documentStoreStub) InstallationAuthority(
	_ context.Context, registrationID int64,
) (publish.InstallationAuthority, error) {
	for _, entry := range s.document.Registrations {
		if entry.RegistrationID != registrationID {
			continue
		}
		authority := publish.InstallationAuthority{
			ActiveEpoch: entry.ActiveEpoch, DurableIntentRevision: entry.DurableIntentRevision,
		}
		for _, binding := range entry.TrustedInstallations {
			authority.TrustedInstallations = append(authority.TrustedInstallations,
				publish.TrustedInstallation{
					RegistrationID: registrationID, InstallationID: binding.InstallationID,
					Account: binding.Account, AccountID: binding.AccountID,
					RepositoryIDs: slices.Clone(binding.RepositoryIDs),
				})
		}
		if pending := entry.Pending; pending != nil {
			authority.Pending = &publish.PendingInstallationEnvelope{
				ActiveEpoch: pending.ActiveEpoch, DurableIntentRevision: pending.DurableIntentRevision,
				RegistrationID: registrationID, ExpectedAccount: pending.ExpectedAccount,
				ExpectedAccountID: pending.ExpectedAccountID, InstallationID: *pending.InstallationID,
				CurrentRepositoryIDs:   slices.Clone(pending.CurrentRepositoryIDs),
				ExpectedRepositoryIDs:  slices.Clone(pending.ExpectedRepositoryIDs),
				RequiredRepositoryMode: pending.RequiredRepositoryMode, ExpiresAt: pending.ExpiresAt,
			}
		}
		return authority, nil
	}
	return publish.InstallationAuthority{}, fmt.Errorf("registration %d is not in the document", registrationID)
}

// coverageGate reports a pending intent ready and covers the promoted binding
// only once covered is set, counting each coverage read.
type coverageGate struct {
	covered func() bool
	reads   int
}

func (g *coverageGate) AllowsRepository(int64, int64, int64) bool {
	g.reads++
	return g.covered()
}

func (*coverageGate) PendingReady(envelope publish.PendingInstallationEnvelope) (int64, bool) {
	if envelope.InstallationID == 0 {
		return 22, true
	}
	return envelope.InstallationID, true
}

// hookedAuditor runs during the workflow audit, between onboarding's start
// check and its promotion.
type hookedAuditor struct {
	audit  domain.WorkflowAudit
	during func()
}

func (a hookedAuditor) Audit(context.Context, string, string) (domain.WorkflowAudit, error) {
	if a.during != nil {
		a.during()
	}
	return a.audit, nil
}

// pendingFixture is a repository awaiting its first installation: a pending
// intent that expires an hour after the audit, read and written through one
// document, with a clock the test moves.
type pendingFixture struct {
	st        *store.Store
	documents *documentStoreStub
	builder   *imageBuilderStub
	now       time.Time
	expiresAt time.Time
	onboard   operations.Onboard
	req       operations.OnboardRequest
}

func newPendingFixture(t *testing.T) *pendingFixture {
	t.Helper()
	ctx := t.Context()
	st := storetest.Open(t, t.TempDir()+"/freeside.db", store.Options{})
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"example/repo","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	auditedAt := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	audit := domain.WorkflowAudit{
		Repo: "example/repo", AuditedCommitSHA: "0123456789012345678901234567890123456789",
		AuditedAt: auditedAt, WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordWorkflowAudit(ctx, audit)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	unassigned := int64(0)
	f := &pendingFixture{
		st: st, now: auditedAt, expiresAt: auditedAt.Add(time.Hour),
		builder: &imageBuilderStub{store: st},
	}
	f.documents = &documentStoreStub{document: publish.InstallationAuthorityDocument{
		Version: 1,
		Registrations: []publish.InstallationAuthorityEntry{{
			RegistrationID: 11, ActiveEpoch: 3, DurableIntentRevision: 7,
			TrustedOwners:        []publish.TrustedOwnerRecord{},
			TrustedInstallations: []publish.TrustedInstallationRecord{},
			Pending: &publish.PendingEnvelopeRecord{
				ActiveEpoch: 3, DurableIntentRevision: 7,
				ExpectedAccount: "example", ExpectedAccountID: 33,
				InstallationID:       &unassigned,
				CurrentRepositoryIDs: []int64{}, ExpectedRepositoryIDs: []int64{44},
				RequiredRepositoryMode: "selected", ExpiresAt: f.expiresAt,
			},
		}},
	}}
	f.onboard = operations.Onboard{
		Store: st, Builder: f.builder, Documents: f.documents, Authority: f.documents,
		Auditor: workflowAuditorStub{audit: audit},
		Now:     func() time.Time { return f.now }, Gate: pendingGateStub(true),
		BaseInputs: unreadableBaseInputs, Recipe: recipeAtAnyCommit,
	}
	f.req = operations.OnboardRequest{
		Repository: "example/repo", RepositoryID: 44, RegistrationID: 11,
		BaseRef: "main",
		Policy: operations.OnboardPolicy{
			PRExecution: domain.PRExecutionAuditedSameRepo,
			CommitPlan:  domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
			ReviewMode: domain.ReviewFreesideInvoked, ReviewConfig: "sha256:review",
			CandidateAutomationChanges: domain.AutomationChangesBlocked,
		},
		Image: projectimage.Request{
			Repository: "example/repo", RepositoryID: 44,
			CommitSHA: "0123456789012345678901234567890123456789",
			Recipe:    []byte(`{"commands":[["npm","test"]],"capture":"none"}`),
			BaseImageRef: domain.ImageRef(
				"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
			BaseBuildRef: "local/base:v1", LocalRegistryPort: 5100,
			ImageName: "test-image", RefTag: "v1",
		},
	}
	review, err := f.onboard.Run(ctx, f.req)
	if err != nil {
		t.Fatalf("review pass: %v", err)
	}
	if review.Installation.Pending == nil {
		t.Fatalf("review installation = %+v, want a pending intent", review.Installation)
	}
	f.req.ApprovalDigest = review.ApprovalDigest
	return f
}

// trusted reports whether the document holds the promoted binding and no
// pending intent.
func (f *pendingFixture) trusted() bool {
	entry := f.documents.document.Registrations[0]
	return entry.Pending == nil && len(entry.TrustedInstallations) == 1 &&
		entry.TrustedInstallations[0].InstallationID == 22 &&
		slices.Equal(entry.TrustedInstallations[0].RepositoryIDs, []int64{44})
}

func (f *pendingFixture) activeProfile(t *testing.T) bool {
	t.Helper()
	err := f.st.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.LatestTrustProfile(t.Context(), f.req.Repository)
		return err
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// TestOnboardPromotesBeforeBuildSoTheBuildCanOutlastTheIntent proves an
// approval promotes the pending installation before the image build starts,
// so a build that runs past the intent's expiry still completes (#3437 F19).
func TestOnboardPromotesBeforeBuildSoTheBuildCanOutlastTheIntent(t *testing.T) {
	f := newPendingFixture(t)
	f.builder.onBuild = func() error {
		if !f.trusted() {
			t.Error("the build started before the installation was promoted")
		}
		f.now = f.expiresAt.Add(13 * time.Minute)
		return nil
	}
	result, err := f.onboard.Run(t.Context(), f.req)
	if err != nil {
		t.Fatalf("approval whose build outlasted the intent: %v", err)
	}
	if result.Status != "complete" || f.builder.calls != 1 || !f.trusted() || !f.activeProfile(t) {
		t.Fatalf("status %q, builds %d, trusted %t, active %t",
			result.Status, f.builder.calls, f.trusted(), f.activeProfile(t))
	}
}

// TestOnboardWithAProfilePromotesAfterTheImage proves a repository that
// already has a trust profile keeps the old order: the binding stays pending
// through the build, so the runtime minter can never pair the old profile with
// the new binding, and a failed build leaves the intent pending and the old
// profile in place. A profile recorded under another spelling of the name
// still counts, because the minter checks coverage by repository ID.
func TestOnboardWithAProfilePromotesAfterTheImage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seedRepo  string
		buildErr  error
		wantTrust bool
	}{
		{name: "build succeeds", wantTrust: true},
		{name: "build fails", buildErr: errors.New("build failed")},
		{name: "profile under another spelling", seedRepo: "Example/Repo", buildErr: errors.New("build failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			ctx := t.Context()
			older := f.req
			older.ApprovalDigest = ""
			older.Policy.CommitPlan = domain.CommitPlanPlanPreferred
			previous, err := f.onboard.Run(ctx, older)
			if err != nil {
				t.Fatal(err)
			}
			if tc.seedRepo != "" {
				previous.Profile.Repo = tc.seedRepo
				if previous.Profile.ProfileDigest, err = previous.Profile.ComputeDigest(); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				if err := tx.RecordInactiveTrustProfile(ctx, previous.Profile, f.now); err != nil {
					return err
				}
				return tx.ActivateTrustProfile(ctx, previous.Profile.Repo, previous.Profile.ProfileDigest, f.now)
			}); err != nil {
				t.Fatal(err)
			}
			current := f.req
			current.ApprovalDigest = ""
			review, err := f.onboard.Run(ctx, current)
			if err != nil {
				t.Fatal(err)
			}
			f.req.ApprovalDigest = review.ApprovalDigest
			f.builder.onBuild = func() error {
				if f.trusted() {
					t.Error("a repository with a profile promoted before its build")
				}
				return tc.buildErr
			}
			_, err = f.onboard.Run(ctx, f.req)
			if (err == nil) != (tc.buildErr == nil) {
				t.Fatalf("approval error = %v, want build error %v", err, tc.buildErr)
			}
			if f.trusted() != tc.wantTrust {
				t.Fatalf("trusted = %t, want %t", f.trusted(), tc.wantTrust)
			}
			var active domain.AutomationTrustProfile
			if err := f.st.Read(ctx, func(tx *store.ReadTx) error {
				active, err = tx.LatestTrustProfile(ctx, previous.Profile.Repo)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			want := previous.Profile.ProfileDigest
			if tc.buildErr == nil {
				want = review.Profile.ProfileDigest
			}
			if active.ProfileDigest != want {
				t.Fatalf("active profile = %s, want %s", active.ProfileDigest, want)
			}
		})
	}
}

// TestOnboardFailedBuildAfterPromotionLeavesNoActiveProfile proves a build
// that fails after promotion leaves the installation trusted but no active
// trust profile, which the runtime minter requires before it mints.
func TestOnboardFailedBuildAfterPromotionLeavesNoActiveProfile(t *testing.T) {
	f := newPendingFixture(t)
	f.builder.onBuild = func() error { return errors.New("build failed") }
	if _, err := f.onboard.Run(t.Context(), f.req); err == nil {
		t.Fatal("approval succeeded with a failed build")
	}
	if !f.trusted() {
		t.Fatalf("promotion did not survive the failed build: %+v", f.documents.document.Registrations[0])
	}
	if f.activeProfile(t) {
		t.Fatal("a failed build left an active trust profile")
	}
}

// TestOnboardWaitsBoundedForThePromotedBindingsCoverage proves the approval
// waits for a janitor pass to cover the promoted binding before it builds,
// and gives up after the bound without building.
func TestOnboardWaitsBoundedForThePromotedBindingsCoverage(t *testing.T) {
	t.Run("covered after a few passes", func(t *testing.T) {
		f := newPendingFixture(t)
		gate := &coverageGate{}
		gate.covered = func() bool { return gate.reads > 3 }
		f.onboard.Gate = gate
		f.builder.onBuild = func() error {
			if gate.reads <= 3 {
				t.Error("the build started before the janitor covered the promoted binding")
			}
			return nil
		}
		synctest.Test(t, func(t *testing.T) {
			if _, err := f.onboard.Run(t.Context(), f.req); err != nil {
				t.Fatal(err)
			}
		})
		if f.builder.calls != 1 || !f.activeProfile(t) {
			t.Fatalf("builds %d, active %t", f.builder.calls, f.activeProfile(t))
		}
	})
	t.Run("never covered", func(t *testing.T) {
		f := newPendingFixture(t)
		f.onboard.Gate = &coverageGate{covered: func() bool { return false }}
		var err error
		synctest.Test(t, func(t *testing.T) {
			started := time.Now()
			_, err = f.onboard.Run(t.Context(), f.req)
			if waited := time.Since(started); waited < time.Minute {
				t.Errorf("gave up after %s, want the one-minute bound", waited)
			}
		})
		if err == nil || !strings.Contains(err.Error(), "did not cover repository 44") {
			t.Fatalf("error = %v, want the coverage bound", err)
		}
		if f.builder.calls != 0 || !f.trusted() || f.activeProfile(t) {
			t.Fatalf("builds %d, trusted %t, active %t", f.builder.calls, f.trusted(), f.activeProfile(t))
		}
	})
}

// TestOnboardNamesTheExpiredIntent proves an intent that expired before
// promotion fails with ExpiredIntentError, at the start check and at the
// pre-promotion check, and is never promoted.
func TestOnboardNamesTheExpiredIntent(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*pendingFixture)
	}{
		{"before the start check", func(f *pendingFixture) { f.now = f.expiresAt }},
		{"during the workflow audit", func(f *pendingFixture) {
			auditor, ok := f.onboard.Auditor.(workflowAuditorStub)
			if !ok {
				t.Fatalf("auditor = %T", f.onboard.Auditor)
			}
			f.onboard.Auditor = hookedAuditor{
				audit: auditor.audit, during: func() { f.now = f.expiresAt.Add(time.Second) },
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPendingFixture(t)
			test.setup(f)
			_, err := f.onboard.Run(t.Context(), f.req)
			var expired *operations.ExpiredIntentError
			if !errors.As(err, &expired) {
				t.Fatalf("error = %v, want ExpiredIntentError", err)
			}
			want := operations.ExpiredIntentError{
				RegistrationID: 11, DurableIntentRevision: 7, ExpectedAccount: "example",
				ExpectedRepositoryIDs: []int64{44}, ExpiresAt: f.expiresAt,
			}
			if !reflect.DeepEqual(*expired, want) {
				t.Fatalf("expired = %+v, want %+v", *expired, want)
			}
			if f.trusted() || f.builder.calls != 0 || f.activeProfile(t) {
				t.Fatalf("expired intent: trusted %t, builds %d, active %t",
					f.trusted(), f.builder.calls, f.activeProfile(t))
			}
		})
	}
}
