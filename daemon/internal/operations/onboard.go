package operations

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// ProjectImageBuilder is the exact reusable #334 primitive consumed by
// onboarding. The interface exists only to make orchestration testable.
type ProjectImageBuilder interface {
	Build(context.Context, projectimage.Request) (domain.ProjectImage, error)
}

// InstallationGate carries the janitor's latest complete observation of
// trusted and pending remote grants. Onboarding rechecks it at the final
// activation boundary because the project-image build may be long-running.
type InstallationGate interface {
	AllowsRepository(registrationID, installationID, repositoryID int64) bool
	PendingReady(publish.PendingInstallationEnvelope) (int64, bool)
}

// OnboardPolicy carries the owner choices that cannot be inferred from a
// workflow audit. Effective authority is copied from the retained audit into
// the proposed profile so the one review sees every privilege it approves.
type OnboardPolicy struct {
	PRExecution    domain.PRExecutionMode
	CommitPlan     domain.CommitPlanMode
	MessageRuleset domain.MessageRuleset
	ReviewMode     domain.ReviewMode
	ReviewConfig   domain.Digest
	ProtectedPaths domain.ProtectedPathConfig
}

// OnboardRequest binds the review and image build to one repository identity.
// ApprovalDigest is empty on the review pass and must exactly match the
// returned review digest on the applying pass.
type OnboardRequest struct {
	Repository     string
	RepositoryID   int64
	RegistrationID int64
	BaseRef        string
	Policy         OnboardPolicy
	ApprovalDigest domain.Digest
	Image          projectimage.Request
}

// InstallationReview is the non-secret selected-installation identity bound
// to the onboarding result.
type InstallationReview struct {
	RegistrationID int64                      `json:"registration_id"`
	InstallationID int64                      `json:"installation_id"`
	Account        string                     `json:"account"`
	AccountID      int64                      `json:"account_id"`
	RepositoryID   int64                      `json:"repository_id"`
	Pending        *PendingInstallationReview `json:"pending,omitempty"`
}

// PendingInstallationReview binds approval to the exact authority frontier
// that received the janitor's temporary reconciliation exception.
type PendingInstallationReview struct {
	ActiveEpoch            int64     `json:"active_epoch"`
	DurableIntentRevision  int64     `json:"durable_intent_revision"`
	AuthoredInstallationID int64     `json:"authored_installation_id"`
	CurrentRepositoryIDs   []int64   `json:"current_repository_ids"`
	ExpectedRepositoryIDs  []int64   `json:"expected_repository_ids"`
	RequiredRepositoryMode string    `json:"required_repository_mode"`
	ExpiresAt              time.Time `json:"expires_at"`
}

// ProjectImageReview is the complete security-relevant image request rendered
// for approval. RecipeDigest binds the exact recipe bytes without embedding
// the operator-supplied document in every result.
type ProjectImageReview struct {
	Repository        string          `json:"repository"`
	RepositoryID      int64           `json:"repository_id"`
	CommitSHA         string          `json:"commit_sha"`
	RecipeDigest      domain.Digest   `json:"recipe_digest"`
	BaseImageRef      domain.ImageRef `json:"base_image_ref"`
	BaseBuildRef      string          `json:"base_build_ref"`
	Registry          string          `json:"registry"`
	LocalRegistryPort int             `json:"local_registry_port"`
	ImageName         string          `json:"image_name"`
	RefTag            string          `json:"ref_tag"`
	DNS               []string        `json:"dns"`
	BuildProxy        string          `json:"build_proxy"`
}

// OnboardResult is either a review-required projection or a completed,
// activated profile plus its durable project image.
type OnboardResult struct {
	Status         string                        `json:"status"`
	ApprovalDigest domain.Digest                 `json:"approval_digest"`
	Profile        domain.AutomationTrustProfile `json:"profile"`
	Review         store.WorkflowAuditReview     `json:"review"`
	Installation   InstallationReview            `json:"installation"`
	ImageRequest   ProjectImageReview            `json:"image_request"`
	ProjectImage   *domain.ProjectImage          `json:"project_image,omitempty"`
	OperatingMode  domain.OperatingMode          `json:"operating_mode"`
	IsolationClass string                        `json:"isolation_class"`
}

// Onboard composes a fresh workflow audit, owner policy, and the reusable
// project-image builder. It deliberately carries no second audit or image
// implementation.
type Onboard struct {
	Store     *store.Store
	Builder   ProjectImageBuilder
	Auditor   publish.WorkflowAuditor
	Authority publish.InstallationAuthoritySource
	Documents AuthorityDocumentStore
	Gate      InstallationGate
	Now       func() time.Time
}

// Run returns the complete digest-bound review until the operator supplies
// that proposed profile's exact digest. The applying pass builds and records
// the project image before activating the profile, so a failed build cannot
// leave a repository trusted without its runtime artifact.
func (o Onboard) Run(ctx context.Context, req OnboardRequest) (OnboardResult, error) {
	if o.Store == nil || o.Builder == nil || o.Auditor == nil ||
		o.Authority == nil || o.Now == nil {
		return OnboardResult{}, errors.New("onboard: nil dependency")
	}
	if req.Repository == "" || req.RepositoryID <= 0 ||
		req.RegistrationID <= 0 || req.BaseRef == "" {
		return OnboardResult{}, errors.New(
			"onboard: repository, repository ID, registration ID, and base ref are required")
	}
	approvedImageRequest := projectimage.NormalizeRequest(cloneProjectImageRequest(req.Image))
	if approvedImageRequest.Repository != req.Repository ||
		approvedImageRequest.RepositoryID != req.RepositoryID {
		return OnboardResult{}, errors.New("onboard: project image repository binding differs")
	}
	owner, _, ok := strings.Cut(req.Repository, "/")
	if !ok || owner == "" {
		return OnboardResult{}, errors.New("onboard: repository must be owner/name")
	}
	authority, err := o.Authority.InstallationAuthority(ctx, req.RegistrationID)
	if err != nil {
		return OnboardResult{}, fmt.Errorf("onboard: resolve installation authority: %w", err)
	}
	var bindings []publish.TrustedInstallation
	for _, binding := range authority.TrustedInstallations {
		if slices.Contains(binding.RepositoryIDs, req.RepositoryID) {
			bindings = append(bindings, binding)
		}
	}
	if len(bindings) > 1 {
		return OnboardResult{}, fmt.Errorf(
			"onboard: repository %d resolves to %d trusted installations, want exactly one",
			req.RepositoryID, len(bindings))
	}
	var (
		installation  InstallationReview
		pending       bool
		pendingReview *publish.PendingInstallationEnvelope
	)
	if len(bindings) == 1 {
		binding := bindings[0]
		if binding.RegistrationID != req.RegistrationID ||
			!strings.EqualFold(binding.Account, owner) {
			return OnboardResult{}, errors.New("onboard: selected installation identity differs")
		}
		installation = InstallationReview{
			RegistrationID: binding.RegistrationID,
			InstallationID: binding.InstallationID,
			Account:        binding.Account,
			AccountID:      binding.AccountID,
			RepositoryID:   req.RepositoryID,
		}
	} else {
		envelope := authority.Pending
		added := pendingRepositoryDelta(envelope)
		if envelope == nil ||
			envelope.ActiveEpoch != authority.ActiveEpoch ||
			envelope.DurableIntentRevision != authority.DurableIntentRevision ||
			!strings.EqualFold(envelope.ExpectedAccount, owner) ||
			len(added) != 1 || added[0] != req.RepositoryID ||
			envelope.RequiredRepositoryMode != "selected" ||
			!envelope.ExpiresAt.After(o.Now().UTC()) {
			return OnboardResult{}, fmt.Errorf(
				"onboard: repository %d has no current exact pending installation",
				req.RepositoryID)
		}
		if o.Gate == nil {
			return OnboardResult{}, errors.New(
				"onboard: pending installation has not passed canonical grant reconciliation")
		}
		installationID, ready := o.Gate.PendingReady(*envelope)
		if !ready {
			return OnboardResult{}, errors.New(
				"onboard: pending installation has not passed canonical grant reconciliation")
		}
		pendingReview = cloneResolvedPending(envelope, installationID)
		installation = InstallationReview{
			RegistrationID: req.RegistrationID,
			InstallationID: installationID,
			Account:        pendingReview.ExpectedAccount,
			AccountID:      pendingReview.ExpectedAccountID,
			RepositoryID:   req.RepositoryID,
			Pending: &PendingInstallationReview{
				ActiveEpoch:            envelope.ActiveEpoch,
				DurableIntentRevision:  envelope.DurableIntentRevision,
				AuthoredInstallationID: envelope.InstallationID,
				CurrentRepositoryIDs:   slices.Clone(envelope.CurrentRepositoryIDs),
				ExpectedRepositoryIDs:  slices.Clone(envelope.ExpectedRepositoryIDs),
				RequiredRepositoryMode: envelope.RequiredRepositoryMode,
				ExpiresAt:              envelope.ExpiresAt,
			},
		}
		pending = true
	}
	audit, err := o.Auditor.Audit(ctx, req.Repository, req.BaseRef)
	if err != nil {
		return OnboardResult{}, fmt.Errorf("onboard: audit workflow authority: %w", err)
	}
	if audit.AuditedCommitSHA != approvedImageRequest.CommitSHA {
		return OnboardResult{}, fmt.Errorf(
			"onboard: fresh workflow audit covers commit %s, not project-image commit %s",
			audit.AuditedCommitSHA, approvedImageRequest.CommitSHA)
	}
	if err := o.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordWorkflowAudit(ctx, audit)
		return err
	}); err != nil {
		return OnboardResult{}, fmt.Errorf("onboard: record fresh workflow audit: %w", err)
	}
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       req.Repository,
		RepositoryID:               req.RepositoryID,
		PRExecution:                req.Policy.PRExecution,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   audit.EffectiveTokenPerms,
		AllowOIDC:                  audit.OIDCAvailable,
		AllowEnvironmentSecrets:    audit.EnvironmentSecrets,
		AllowSecretBearingPRJobs:   audit.SecretBearingPRJobs,
		AllowSelfHostedCI:          audit.SelfHostedRunners,
		AllowPullRequestTarget:     audit.PullRequestTarget,
		AllowReusableWorkflows:     audit.ReusableWorkflows,
		AllowPackagePublishing:     audit.PackagePublishing,
		AllowArtifactConsumers:     audit.ArtifactConsumers,
		CommitPlan:                 req.Policy.CommitPlan,
		MessageRuleset:             req.Policy.MessageRuleset,
		WorkflowAuditDigest:        audit.WorkflowAuditDigest,
		Review: domain.ReviewSettings{
			Mode: req.Policy.ReviewMode, ConfigDigest: req.Policy.ReviewConfig,
		},
		ProtectedPaths: req.Policy.ProtectedPaths,
	})
	if err != nil {
		return OnboardResult{}, fmt.Errorf("onboard: construct trust profile: %w", err)
	}
	var review store.WorkflowAuditReview
	if err := o.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		review, err = tx.WorkflowAuditReviewForProfile(ctx, profile)
		return err
	}); err != nil {
		return OnboardResult{}, fmt.Errorf("onboard: load workflow-audit review: %w", err)
	}
	result := OnboardResult{
		Status: "review_required", Profile: profile, Review: review,
		Installation:   installation,
		ImageRequest:   projectImageReview(approvedImageRequest),
		OperatingMode:  domain.ModeAttendedDev,
		IsolationClass: "process_local",
	}
	result.ApprovalDigest, err = onboardApprovalDigest(
		profile.ProfileDigest, installation, result.ImageRequest,
	)
	if err != nil {
		return OnboardResult{}, err
	}
	if req.ApprovalDigest == "" {
		return result, nil
	}
	if req.ApprovalDigest != result.ApprovalDigest {
		return OnboardResult{}, fmt.Errorf(
			"onboard: approval digest %s does not match proposed review %s",
			req.ApprovalDigest, result.ApprovalDigest)
	}
	image, err := o.Builder.Build(ctx, cloneProjectImageRequest(approvedImageRequest))
	if err != nil {
		return OnboardResult{}, fmt.Errorf("onboard: build project image: %w", err)
	}
	if err := image.Validate(); err != nil {
		return OnboardResult{}, fmt.Errorf("onboard: validate project image result: %w", err)
	}
	if err := projectimage.ValidatePublishedRef(approvedImageRequest, image.ImageRef); err != nil {
		return OnboardResult{}, fmt.Errorf(
			"onboard: project image result is not bound to the approved destination: %w", err,
		)
	}
	if image.Repository != req.Repository ||
		image.RepositoryID != req.RepositoryID ||
		image.CommitSHA != approvedImageRequest.CommitSHA ||
		image.RecipeDigest != verify.RecipeDigest(approvedImageRequest.Recipe) ||
		!slices.Equal(image.PreparationCommand, []string{projectimage.PreparationPath}) ||
		image.BaseImageRef != approvedImageRequest.BaseImageRef {
		return OnboardResult{}, errors.New(
			"onboard: project image result is not bound to the approved request")
	}
	var recorded domain.ProjectImage
	if err := o.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		recorded, err = tx.GetProjectImage(ctx, image.ID)
		return err
	}); err != nil {
		return OnboardResult{}, fmt.Errorf("onboard: read recorded project image: %w", err)
	}
	if !sameProjectImage(recorded, image) {
		return OnboardResult{}, errors.New(
			"onboard: recorded project image differs from the builder result")
	}
	if err := o.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordInactiveTrustProfile(ctx, profile, o.Now().UTC())
	}); err != nil {
		return OnboardResult{}, fmt.Errorf("onboard: record inactive trust profile: %w", err)
	}
	if pending {
		if o.Documents == nil {
			return OnboardResult{}, errors.New("onboard: nil installation authority writer")
		}
		if _, ready := o.Gate.PendingReady(*pendingReview); !ready {
			return OnboardResult{}, errors.New(
				"onboard: pending installation changed before approved promotion")
		}
		if err := PromoteInstallation(
			ctx, o.Documents, *pendingReview, req.RepositoryID, o.Now,
		); err != nil {
			return OnboardResult{}, fmt.Errorf("onboard: promote approved installation: %w", err)
		}
	} else {
		if o.Gate == nil {
			return OnboardResult{}, errors.New(
				"onboard: trusted installation has no canonical grant reconciliation")
		}
		current, authorityErr := o.Authority.InstallationAuthority(
			ctx, req.RegistrationID,
		)
		if authorityErr != nil {
			return OnboardResult{}, fmt.Errorf(
				"onboard: re-resolve installation authority before activation: %w",
				authorityErr)
		}
		var matches []publish.TrustedInstallation
		for _, binding := range current.TrustedInstallations {
			if slices.Contains(binding.RepositoryIDs, req.RepositoryID) {
				matches = append(matches, binding)
			}
		}
		if len(matches) != 1 {
			return OnboardResult{}, errors.New(
				"onboard: trusted installation changed before profile activation")
		}
		binding := matches[0]
		if binding.RegistrationID != installation.RegistrationID ||
			binding.InstallationID != installation.InstallationID ||
			binding.Account != installation.Account ||
			binding.AccountID != installation.AccountID ||
			!o.Gate.AllowsRepository(
				installation.RegistrationID,
				installation.InstallationID,
				installation.RepositoryID,
			) {
			return OnboardResult{}, errors.New(
				"onboard: trusted installation changed before profile activation")
		}
	}
	if err := o.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ActivateTrustProfile(
			ctx, profile.Repo, profile.ProfileDigest, o.Now().UTC())
	}); err != nil {
		return OnboardResult{}, fmt.Errorf("onboard: activate trust profile: %w", err)
	}
	result.Status = "complete"
	result.ProjectImage = &image
	return result, nil
}

func onboardApprovalDigest(
	profileDigest domain.Digest,
	installation InstallationReview,
	imageRequest ProjectImageReview,
) (domain.Digest, error) {
	body, err := json.Marshal(struct {
		Version       string             `json:"version"`
		ProfileDigest domain.Digest      `json:"profile_digest"`
		Installation  InstallationReview `json:"installation"`
		ImageRequest  ProjectImageReview `json:"image_request"`
	}{
		Version:       "freeside.onboard-approval/v2",
		ProfileDigest: profileDigest,
		Installation:  installation,
		ImageRequest:  imageRequest,
	})
	if err != nil {
		return "", fmt.Errorf("onboard: encode approval review: %w", err)
	}
	return domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(body))), nil
}

func projectImageReview(request projectimage.Request) ProjectImageReview {
	return ProjectImageReview{
		Repository: request.Repository, RepositoryID: request.RepositoryID,
		CommitSHA: request.CommitSHA, RecipeDigest: verify.RecipeDigest(request.Recipe),
		BaseImageRef: request.BaseImageRef, BaseBuildRef: request.BaseBuildRef,
		Registry: request.Registry, LocalRegistryPort: request.LocalRegistryPort,
		ImageName: request.ImageName, RefTag: request.RefTag,
		DNS: slices.Clone(request.DNS), BuildProxy: request.BuildProxy,
	}
}

func cloneProjectImageRequest(request projectimage.Request) projectimage.Request {
	request.Recipe = slices.Clone(request.Recipe)
	request.DNS = slices.Clone(request.DNS)
	return request
}

func pendingRepositoryDelta(
	envelope *publish.PendingInstallationEnvelope,
) []int64 {
	if envelope == nil {
		return nil
	}
	var added []int64
	for _, repositoryID := range envelope.ExpectedRepositoryIDs {
		if !slices.Contains(envelope.CurrentRepositoryIDs, repositoryID) {
			added = append(added, repositoryID)
		}
	}
	return added
}

func sameProjectImage(a, b domain.ProjectImage) bool {
	return a.ID == b.ID &&
		a.Repository == b.Repository &&
		a.RepositoryID == b.RepositoryID &&
		a.CommitSHA == b.CommitSHA &&
		a.RecipeDigest == b.RecipeDigest &&
		slices.Equal(a.PreparationCommand, b.PreparationCommand) &&
		a.BaseImageRef == b.BaseImageRef &&
		a.ImageRef == b.ImageRef
}

func cloneResolvedPending(
	envelope *publish.PendingInstallationEnvelope,
	installationID int64,
) *publish.PendingInstallationEnvelope {
	resolved := *envelope
	resolved.InstallationID = installationID
	resolved.CurrentRepositoryIDs = slices.Clone(envelope.CurrentRepositoryIDs)
	resolved.ExpectedRepositoryIDs = slices.Clone(envelope.ExpectedRepositoryIDs)
	return &resolved
}
