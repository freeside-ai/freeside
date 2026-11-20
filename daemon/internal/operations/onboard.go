package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
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
	// ExternalReviewers is the owner's allowlist of reviewers outside
	// Freeside whose findings may start a round (plan §5.19). Empty admits
	// nobody and proposes the same profile as before the field existed.
	ExternalReviewers []domain.ExternalReviewer
	// CandidateAutomationChanges is the owner's stance on candidate edits to
	// automation-control paths (plan §5.5): block refuses them at import,
	// advisory publishes them with an advisory finding. The profile digest,
	// and so the approval digest, binds it.
	CandidateAutomationChanges domain.AutomationChangePolicy
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
	// GoModuleDir is the declared Go module directory. It is omitted when
	// empty so a review that declares none keeps its earlier approval digest.
	GoModuleDir string `json:"go_module_dir,omitempty"`
	// SwiftPackageDir is the declared Swift package directory, omitted when
	// empty for the same reason.
	SwiftPackageDir string `json:"swift_package_dir,omitempty"`
}

// OnboardResult is either a review-required projection or a completed,
// activated profile plus its durable project image.
//
// Carried marks a review pass that completed without an approval because
// nothing the active approval trusts changed (Onboard.Run). Changes lists,
// on a review_required result for a repository with an active profile, what
// differs from that approval.
type OnboardResult struct {
	Status         string                        `json:"status"`
	ApprovalDigest domain.Digest                 `json:"approval_digest"`
	Carried        bool                          `json:"carried,omitempty"`
	Changes        []OnboardChange               `json:"changes,omitempty"`
	Profile        domain.AutomationTrustProfile `json:"profile"`
	Review         store.WorkflowAuditReview     `json:"review"`
	Installation   InstallationReview            `json:"installation"`
	ImageRequest   ProjectImageReview            `json:"image_request"`
	ProjectImage   *domain.ProjectImage          `json:"project_image,omitempty"`
	OperatingMode  domain.OperatingMode          `json:"operating_mode"`
	IsolationClass string                        `json:"isolation_class"`
}

// OnboardChange is one reason a review cannot carry the active approval
// forward. A field change names a top-level profile field, or
// image.recipe_digest or image.base_image_ref, with the approved and proposed
// JSON values; a reason change says in prose why the admitted image or the
// installation needs the owner.
type OnboardChange struct {
	Field    string          `json:"field,omitempty"`
	Approved json.RawMessage `json:"approved,omitempty"`
	Proposed json.RawMessage `json:"proposed,omitempty"`
	Reason   string          `json:"reason,omitempty"`
}

// MovedTipError reports that the live base-ref tip, which the fresh workflow
// audit covers, is not the requested commit. Onboarding audits only the live
// tip, so the caller reruns the review at Tip.
type MovedTipError struct {
	Tip    string
	Commit string
}

func (e *MovedTipError) Error() string {
	return fmt.Sprintf(
		"onboard: the base ref's live tip is %s, not -commit %s; the workflow audit covers only the live tip",
		e.Tip, e.Commit)
}

// BaseInputsReader reads, from one exact commit, the inputs a project image
// of layout records (projectimage.ObserveBaseInputs).
type BaseInputsReader func(
	ctx context.Context, commitSHA string, layout domain.ProjectImageInputLayout,
) (domain.ProjectImageBaseInputs, error)

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
	// BaseInputs decides, with the admitted image's record, whether that
	// image serves the requested commit without a build.
	BaseInputs BaseInputsReader
	Now        func() time.Time
}

// Run returns the complete digest-bound review until the operator supplies
// that proposed profile's exact digest. The applying pass builds and records
// the project image before activating the profile, so a failed build cannot
// leave a repository trusted without its runtime artifact.
//
// Two shortcuts skip work the active approval already covers. The applying
// pass reuses the admitted image instead of building when that image serves
// the commit. The review pass carries the approval forward, re-activating
// the active profile with no owner approval, when the installation is
// already trusted, the proposed profile's digest equals the active one, and
// the admitted image serves the commit: the profile digest binds every
// workflow, setting, and policy fact the owner approved, and the image check
// binds the recipe, base, destination, toolchain layout, and dependency
// manifests. Anything else needs the owner, and the result's Changes say why.
func (o Onboard) Run(ctx context.Context, req OnboardRequest) (OnboardResult, error) {
	if o.Store == nil || o.Builder == nil || o.Auditor == nil ||
		o.Authority == nil || o.BaseInputs == nil || o.Now == nil {
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
		return OnboardResult{}, &MovedTipError{
			Tip: audit.AuditedCommitSHA, Commit: approvedImageRequest.CommitSHA,
		}
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
		CandidateAutomationChanges: req.Policy.CandidateAutomationChanges,
		PRGitHubTokenPermissions:   audit.EffectiveTokenPerms,
		AllowOIDC:                  audit.OIDCAvailable,
		AllowEnvironmentSecrets:    audit.EnvironmentSecrets,
		AllowSecretBearingPRJobs:   audit.SecretBearingPRJobs,
		AllowSelfHostedCI:          audit.SelfHostedRunners,
		AllowPullRequestTarget:     audit.PullRequestTarget,
		AllowReusableWorkflows:     audit.ReusableWorkflows,
		AllowPackagePublishing:     audit.PackagePublishing,
		AllowArtifactConsumers:     audit.ArtifactConsumers,
		// The issue_comment set mirrors the audit like every other observed
		// privilege. Approving this profile's digest is the owner's acceptance
		// of the comment operation over exactly these workflows, none
		// included; declining it accepts no comment.
		AllowIssueCommentWorkflows: true,
		IssueCommentWorkflows:      audit.IssueCommentWorkflows,
		CommitPlan:                 req.Policy.CommitPlan,
		MessageRuleset:             req.Policy.MessageRuleset,
		WorkflowAuditDigest:        audit.WorkflowAuditDigest,
		Review: domain.ReviewSettings{
			Mode: req.Policy.ReviewMode, ConfigDigest: req.Policy.ReviewConfig,
		},
		ProtectedPaths:    req.Policy.ProtectedPaths,
		ExternalReviewers: req.Policy.ExternalReviewers,
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
	reusable, imageChanges, err := o.servingImage(ctx, approvedImageRequest)
	if err != nil {
		return OnboardResult{}, err
	}
	if req.ApprovalDigest == "" {
		active, found, activeChange, err := o.activeProfile(ctx, req.Repository)
		if err != nil {
			return OnboardResult{}, err
		}
		if !found {
			return result, nil
		}
		if !pending && activeChange == nil &&
			active.ProfileDigest == profile.ProfileDigest && reusable != nil {
			if err := o.recheckTrustedInstallation(ctx, req.RegistrationID, installation); err != nil {
				return OnboardResult{}, err
			}
			if err := o.activate(ctx, profile); err != nil {
				return OnboardResult{}, err
			}
			result.Status = "complete"
			result.Carried = true
			result.ProjectImage = reusable
			return result, nil
		}
		if activeChange != nil {
			result.Changes = append(result.Changes, *activeChange)
		} else {
			result.Changes, err = profileChanges(active, profile)
			if err != nil {
				return OnboardResult{}, err
			}
		}
		if pending {
			result.Changes = append(result.Changes, OnboardChange{
				Reason: "the repository's installation is a pending intent, which only an approval promotes",
			})
		}
		result.Changes = append(result.Changes, imageChanges...)
		return result, nil
	}
	if req.ApprovalDigest != result.ApprovalDigest {
		return OnboardResult{}, fmt.Errorf(
			"onboard: approval digest %s does not match proposed review %s",
			req.ApprovalDigest, result.ApprovalDigest)
	}
	var image domain.ProjectImage
	if reusable != nil {
		// The admitted image already serves the approved request, so the
		// pointer stays where it is and only the profile is recorded.
		image = *reusable
		if err := o.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.RecordInactiveTrustProfile(ctx, profile, o.Now().UTC())
		}); err != nil {
			return OnboardResult{}, fmt.Errorf("onboard: record inactive trust profile: %w", err)
		}
	} else {
		image, err = o.buildAndAdmit(ctx, req, approvedImageRequest, profile)
		if err != nil {
			return OnboardResult{}, err
		}
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
	} else if err := o.recheckTrustedInstallation(ctx, req.RegistrationID, installation); err != nil {
		return OnboardResult{}, err
	}
	if err := o.activate(ctx, profile); err != nil {
		return OnboardResult{}, err
	}
	result.Status = "complete"
	result.ProjectImage = &image
	return result, nil
}

// buildAndAdmit builds the approved image, proves the result is bound to the
// approved request, and records the profile inactive beside the admitted-image
// pointer advanced to that image.
func (o Onboard) buildAndAdmit(
	ctx context.Context,
	req OnboardRequest,
	approvedImageRequest projectimage.Request,
	profile domain.AutomationTrustProfile,
) (domain.ProjectImage, error) {
	image, err := o.Builder.Build(ctx, cloneProjectImageRequest(approvedImageRequest))
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf("onboard: build project image: %w", err)
	}
	if err := image.Validate(); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("onboard: validate project image result: %w", err)
	}
	if err := projectimage.ValidatePublishedRef(approvedImageRequest, image.ImageRef); err != nil {
		return domain.ProjectImage{}, fmt.Errorf(
			"onboard: project image result is not bound to the approved destination: %w", err,
		)
	}
	if image.Repository != req.Repository ||
		image.RepositoryID != req.RepositoryID ||
		image.CommitSHA != approvedImageRequest.CommitSHA ||
		image.RecipeDigest != verify.RecipeDigest(approvedImageRequest.Recipe) ||
		!slices.Equal(image.PreparationCommand, []string{projectimage.PreparationPath}) ||
		image.BaseImageRef != approvedImageRequest.BaseImageRef {
		return domain.ProjectImage{}, errors.New(
			"onboard: project image result is not bound to the approved request")
	}
	// A new build always records its environment, under this binary's own
	// preparation implementation. A result without one could never be reused
	// past its build commit, and one naming another implementation would be
	// reused on evidence this binary did not produce.
	if image.Environment == nil ||
		image.Environment.PreparationDigest != projectimage.PreparationDigest() {
		return domain.ProjectImage{}, errors.New(
			"onboard: project image result does not record this builder's environment")
	}
	var recorded domain.ProjectImage
	if err := o.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		recorded, err = tx.GetProjectImage(ctx, image.ID)
		return err
	}); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("onboard: read recorded project image: %w", err)
	}
	if !sameProjectImage(recorded, image) {
		return domain.ProjectImage{}, errors.New(
			"onboard: recorded project image differs from the builder result")
	}
	// The image just built seeds the admitted-image pointer in the same
	// transaction as the trust profile (plan §5.7, Project-image upkeep).
	// Onboarding builds from the Claude agent base only, so it seeds that
	// harness's pointer; a re-onboard advances it to the newer image.
	pointer := domain.AdmittedImagePointer{
		RepositoryID: image.RepositoryID, Repository: image.Repository,
		Harness: domain.HarnessClientClaudeCode, ImageID: image.ID,
		ImageRef: image.ImageRef, AdvancedAt: o.Now().UTC(),
	}
	if err := o.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveTrustProfile(ctx, profile, o.Now().UTC()); err != nil {
			return err
		}
		return tx.AdvanceAdmittedImagePointer(ctx, pointer)
	}); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("onboard: record inactive trust profile: %w", err)
	}
	return image, nil
}

// recheckTrustedInstallation re-resolves the repository's trusted installation
// and the janitor's reconciled grant just before activation: an image build
// can run for most of an hour, and a carried review activates without one.
func (o Onboard) recheckTrustedInstallation(
	ctx context.Context, registrationID int64, installation InstallationReview,
) error {
	if o.Gate == nil {
		return errors.New(
			"onboard: trusted installation has no canonical grant reconciliation")
	}
	current, err := o.Authority.InstallationAuthority(ctx, registrationID)
	if err != nil {
		return fmt.Errorf(
			"onboard: re-resolve installation authority before activation: %w", err)
	}
	var matches []publish.TrustedInstallation
	for _, binding := range current.TrustedInstallations {
		if slices.Contains(binding.RepositoryIDs, installation.RepositoryID) {
			matches = append(matches, binding)
		}
	}
	if len(matches) != 1 {
		return errors.New(
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
		return errors.New(
			"onboard: trusted installation changed before profile activation")
	}
	return nil
}

func (o Onboard) activate(ctx context.Context, profile domain.AutomationTrustProfile) error {
	if err := o.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ActivateTrustProfile(
			ctx, profile.Repo, profile.ProfileDigest, o.Now().UTC())
	}); err != nil {
		return fmt.Errorf("onboard: activate trust profile: %w", err)
	}
	return nil
}

// activeProfile reads the repository's active trust profile. A repository
// with none returns found false. One whose stored profile no longer
// reconstructs under this build returns found true with a change naming
// that, so the owner reviews it again instead of being locked out of the
// re-approval that repairs it.
func (o Onboard) activeProfile(
	ctx context.Context, repository string,
) (domain.AutomationTrustProfile, bool, *OnboardChange, error) {
	var active domain.AutomationTrustProfile
	err := o.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		active, err = tx.LatestTrustProfile(ctx, repository)
		return err
	})
	switch {
	case err == nil:
		return active, true, nil, nil
	case errors.Is(err, store.ErrNotFound):
		return domain.AutomationTrustProfile{}, false, nil, nil
	case errors.Is(err, store.ErrTrustProfileUnreadable):
		return domain.AutomationTrustProfile{}, true, &OnboardChange{
			Reason: fmt.Sprintf("the active trust profile is unreadable: %v", err),
		}, nil
	}
	return domain.AutomationTrustProfile{}, false, nil,
		fmt.Errorf("onboard: read active trust profile: %w", err)
}

// servingImage returns the Claude harness's admitted image when it serves
// request's commit, so onboarding needs no build: the image bakes the
// requested recipe on the requested base, sits at the requested destination,
// bakes the requested toolchain directories, and is admissible at the commit
// (domain.ProjectImage.AdmissibleAt, over the commit's inputs). Otherwise it
// returns the changes that say why not. Only a store failure is an error; an
// unreadable commit is a reason, and the caller builds as it would without
// an admitted image.
func (o Onboard) servingImage(
	ctx context.Context, request projectimage.Request,
) (*domain.ProjectImage, []OnboardChange, error) {
	var (
		image domain.ProjectImage
		found bool
	)
	if err := o.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		_, image, found, err = tx.GetAdmittedImagePointer(
			ctx, request.RepositoryID, domain.HarnessClientClaudeCode)
		return err
	}); err != nil {
		return nil, nil, fmt.Errorf("onboard: read admitted project image: %w", err)
	}
	if !found {
		return nil, []OnboardChange{{
			Reason: "the repository has no admitted project image; approval builds one",
		}}, nil
	}
	var changes []OnboardChange
	if recipe := verify.RecipeDigest(request.Recipe); image.RecipeDigest != recipe {
		change, err := fieldChange("image.recipe_digest", image.RecipeDigest, recipe)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
	}
	if image.BaseImageRef != request.BaseImageRef {
		change, err := fieldChange("image.base_image_ref", image.BaseImageRef, request.BaseImageRef)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, change)
	}
	if len(changes) != 0 {
		return nil, changes, nil
	}
	notServed := func(format string, args ...any) (*domain.ProjectImage, []OnboardChange, error) {
		return nil, []OnboardChange{{Reason: fmt.Sprintf(format, args...)}}, nil
	}
	if image.Repository != request.Repository {
		return notServed("the admitted image %s was built for repository %s, not %s",
			image.ImageRef, image.Repository, request.Repository)
	}
	if !slices.Equal(image.PreparationCommand, []string{projectimage.PreparationPath}) {
		return notServed("the admitted image %s runs preparation command %q, not %q",
			image.ImageRef, image.PreparationCommand, projectimage.PreparationPath)
	}
	layout := image.InputLayout()
	if layout.GoModuleDir != request.GoModuleDir || layout.SwiftPackageDir != request.SwiftPackageDir {
		return notServed("the admitted image %s bakes Go module directory %q and Swift package directory %q; "+
			"the request declares %q and %q",
			image.ImageRef, layout.GoModuleDir, layout.SwiftPackageDir,
			request.GoModuleDir, request.SwiftPackageDir)
	}
	// AdmissibleAt accepts any image at its own build commit, so a same-commit
	// review would otherwise reuse an image built by another binary's
	// toolchain and preparation sources, which a build here never does.
	if image.Environment == nil || image.Environment.PreparationDigest != projectimage.PreparationDigest() {
		return notServed("the admitted image %s was not built with this binary's toolchain and "+
			"preparation sources", image.ImageRef)
	}
	if err := projectimage.ValidatePublishedRef(request, image.ImageRef); err != nil {
		return notServed("the admitted image is not at the requested destination: %v", err)
	}
	inputs, err := o.BaseInputs(ctx, request.CommitSHA, layout)
	if err != nil {
		return notServed("cannot read commit %s's image inputs to reuse the admitted image %s: %v",
			request.CommitSHA, image.ImageRef, err)
	}
	if err := image.AdmissibleAt(inputs, projectimage.PreparationDigest()); err != nil {
		return notServed("%v", err)
	}
	return &image, nil, nil
}

// profileChanges lists each top-level field of the proposed profile whose
// JSON value differs from the active one, in field order. The profile digest
// is left out: it differs whenever any listed field does.
func profileChanges(active, proposed domain.AutomationTrustProfile) ([]OnboardChange, error) {
	approvedFields, err := profileFields(active)
	if err != nil {
		return nil, err
	}
	proposedFields, err := profileFields(proposed)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(approvedFields)+len(proposedFields))
	for name := range approvedFields {
		names = append(names, name)
	}
	for name := range proposedFields {
		if _, ok := approvedFields[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var changes []OnboardChange
	for _, name := range names {
		if name == "profile_digest" ||
			string(approvedFields[name]) == string(proposedFields[name]) {
			continue
		}
		changes = append(changes, OnboardChange{
			Field: name, Approved: approvedFields[name], Proposed: proposedFields[name],
		})
	}
	return changes, nil
}

func profileFields(profile domain.AutomationTrustProfile) (map[string]json.RawMessage, error) {
	body, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("onboard: encode trust profile: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("onboard: decode trust profile fields: %w", err)
	}
	return fields, nil
}

func fieldChange(field string, approved, proposed any) (OnboardChange, error) {
	approvedJSON, err := json.Marshal(approved)
	if err != nil {
		return OnboardChange{}, fmt.Errorf("onboard: encode approved %s: %w", field, err)
	}
	proposedJSON, err := json.Marshal(proposed)
	if err != nil {
		return OnboardChange{}, fmt.Errorf("onboard: encode proposed %s: %w", field, err)
	}
	return OnboardChange{Field: field, Approved: approvedJSON, Proposed: proposedJSON}, nil
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
	return domain.Digest(contentaddr.Sum(body)), nil
}

func projectImageReview(request projectimage.Request) ProjectImageReview {
	return ProjectImageReview{
		Repository: request.Repository, RepositoryID: request.RepositoryID,
		CommitSHA: request.CommitSHA, RecipeDigest: verify.RecipeDigest(request.Recipe),
		BaseImageRef: request.BaseImageRef, BaseBuildRef: request.BaseBuildRef,
		Registry: request.Registry, LocalRegistryPort: request.LocalRegistryPort,
		ImageName: request.ImageName, RefTag: request.RefTag,
		DNS: slices.Clone(request.DNS), BuildProxy: request.BuildProxy,
		GoModuleDir: request.GoModuleDir, SwiftPackageDir: request.SwiftPackageDir,
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
		a.ImageRef == b.ImageRef &&
		sameProjectImageEnvironment(a.Environment, b.Environment)
}

func sameProjectImageEnvironment(a, b *domain.ProjectImageEnvironment) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
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
