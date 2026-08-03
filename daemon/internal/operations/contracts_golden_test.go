package operations_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestOperationalContractGoldens(t *testing.T) {
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"example/repo","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	auditedAt := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	audit := domain.WorkflowAudit{
		Repo: "example/repo", AuditedCommitSHA: strings.Repeat("1", 40),
		AuditedAt: auditedAt, WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	if err := audit.Validate(); err != nil {
		t.Fatal(err)
	}
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "example/repo", RepositoryID: 44,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        audit.WorkflowAuditDigest,
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
		},
		ProtectedPaths: domain.ProtectedPathConfig{
			ExtraAutomationControlPatterns:   []string{".github/workflows/**"},
			ExtraReviewerInstructionPatterns: []string{"AGENTS.md"},
			ExtraPromptsAndPolicyPatterns:    []string{"prompts/**"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "example/repo", RepositoryID: 44,
		CommitSHA:          strings.Repeat("1", 40),
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("2", 64)),
		PreparationCommand: []string{"/usr/local/bin/freeside-project-prepare"},
		BaseImageRef: domain.ImageRef(
			"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("3", 64)),
		ImageRef: domain.ImageRef(
			"ghcr.io/freeside-ai/example-repo@sha256:" + strings.Repeat("4", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewSide := store.WorkflowAuditReviewSide{
		Audit:    store.WorkflowAuditRecord{ID: 7, Audit: audit},
		Evidence: evidence,
	}
	review := store.WorkflowAuditReview{
		Profile: profile, Approved: reviewSide, Observed: reviewSide,
		ChangedFields: []string{},
	}
	installation := operations.InstallationReview{
		RegistrationID: 11, InstallationID: 22,
		Account: "example", AccountID: 33, RepositoryID: 44,
		Pending: &operations.PendingInstallationReview{
			ActiveEpoch: 3, DurableIntentRevision: 7,
			AuthoredInstallationID: 0,
			CurrentRepositoryIDs:   []int64{},
			ExpectedRepositoryIDs:  []int64{44},
			RequiredRepositoryMode: "selected",
			ExpiresAt:              auditedAt.Add(time.Hour),
		},
	}
	approvalDigest := domain.Digest("sha256:" + strings.Repeat("5", 64))
	imageRequest := operations.ProjectImageReview{
		Repository: "example/repo", RepositoryID: 44,
		CommitSHA:    strings.Repeat("1", 40),
		RecipeDigest: domain.Digest("sha256:" + strings.Repeat("2", 64)),
		BaseImageRef: domain.ImageRef(
			"ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("3", 64)),
		BaseBuildRef: "localhost/freeside-agent:build",
		Registry:     "", LocalRegistryPort: 5100,
		ImageName: "example-repo", RefTag: "v1",
		DNS: []string{"1.1.1.1"}, BuildProxy: "http://proxy.example.test",
	}
	cases := []struct {
		name  string
		value any
	}{
		{
			name: "layout",
			value: operations.Layout{
				ConfigDir:      "/home/operator/.freeside",
				DBPath:         "/home/operator/.freeside/state/freeside.db",
				StateDir:       "/home/operator/.freeside/state",
				CredentialsDir: "/home/operator/.freeside/credentials",
				FakeDriverDir:  "/home/operator/.freeside/state/freeside.db.fake-stage-driver",
				AuthorityPath:  "/home/operator/.freeside/state/installation-authority.json",
			},
		},
		{
			name: "doctor-report",
			value: operations.DoctorReport{
				Healthy: false, OperatingMode: domain.ModeUnattended,
				IsolationClass: string(domain.BackendFreshVMReadOnlyVolumeHandoff),
				Findings: []operations.DoctorFinding{
					{Code: "conformance", Healthy: true, Detail: "generation 7 passed"},
					{Code: "artifact_closure", Healthy: false, Detail: "unhealthy"},
				},
			},
		},
		{
			name: "onboard-review-required",
			value: operations.OnboardResult{
				Status: "review_required", ApprovalDigest: approvalDigest,
				Profile: profile, Review: review,
				Installation: installation, ImageRequest: imageRequest,
				OperatingMode:  domain.ModeAttendedDev,
				IsolationClass: "process_local",
			},
		},
		{
			name: "onboard-complete",
			value: operations.OnboardResult{
				Status: "complete", ApprovalDigest: approvalDigest,
				Profile: profile, Review: review,
				Installation: installation, ImageRequest: imageRequest, ProjectImage: &image,
				OperatingMode: domain.ModeAttendedDev, IsolationClass: "process_local",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.MarshalIndent(tc.value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, tc.name, append(got, '\n'))
		})
	}
}
