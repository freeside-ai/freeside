package operations_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

type imageBuilderStub struct {
	calls              int
	image              domain.ProjectImage
	store              *store.Store
	preparationCommand []string
	afterBuild         func()
	mutateRequest      func(*projectimage.Request)
}

func (b *imageBuilderStub) Build(
	ctx context.Context, req projectimage.Request,
) (domain.ProjectImage, error) {
	b.calls++
	if b.mutateRequest != nil {
		b.mutateRequest(&req)
	}
	if b.image.ID == "" {
		command := b.preparationCommand
		if command == nil {
			command = []string{projectimage.PreparationPath}
		}
		image, err := domain.NewProjectImage(domain.ProjectImageInput{
			Repository: req.Repository, RepositoryID: req.RepositoryID,
			CommitSHA: req.CommitSHA, RecipeDigest: verify.RecipeDigest(req.Recipe),
			PreparationCommand: command,
			BaseImageRef:       req.BaseImageRef,
			ImageRef: domain.ImageRef(
				"127.0.0.1:" + fmt.Sprint(req.LocalRegistryPort) + "/" +
					req.ImageName + "@sha256:" + strings.Repeat("b", 64)),
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

func TestOnboardRequiresOneDigestBoundReviewBeforeBuildAndActivation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/freeside.db", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // Test cleanup cannot affect the assertion.
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
		Authority: authority,
	}
	req := operations.OnboardRequest{
		Repository: "example/repo", RepositoryID: 44, RegistrationID: 11,
		BaseRef: "main",
		Policy: operations.OnboardPolicy{
			PRExecution: domain.PRExecutionAuditedSameRepo,
			CommitPlan:  domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
			ReviewMode: domain.ReviewAuto, ReviewConfig: "sha256:review",
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
	builder.image, err = domain.NewProjectImage(domain.ProjectImageInput{
		Repository: req.Repository, RepositoryID: req.RepositoryID,
		CommitSHA:          req.Image.CommitSHA,
		RecipeDigest:       verify.RecipeDigest(req.Image.Recipe),
		PreparationCommand: []string{projectimage.PreparationPath},
		BaseImageRef:       req.Image.BaseImageRef,
		ImageRef: domain.ImageRef(
			"registry.example.test/other-image@sha256:" + strings.Repeat("b", 64)),
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
	if complete.Status != "complete" || builder.calls != 7 {
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
}

func TestOnboardPromotesPendingInstallationOnlyAfterApproval(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/freeside.db", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // Test cleanup cannot affect the assertion.
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
			ReviewMode: domain.ReviewAuto, ReviewConfig: "sha256:review",
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
	changedAuthority := onboard.Authority
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
	onboard.Authority = changedAuthority
	onboard.Gate = &pendingGateSequence{results: []bool{true, false}}
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
