package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var ErrProductionInputUndeliverable = errors.New("production input cannot be delivered")

// errOperatorInputsUndeliverable marks a writer's operator input files that
// can never be delivered: the combined list breaks a size, count, or name
// rule. It is permanent, like a materializer shape refusal, so each caller
// turns it into its stage's undeliverable error instead of retrying the
// intent forever. A missing blob is not one: it stays an operational fault.
var errOperatorInputsUndeliverable = errors.New("operator input files cannot be delivered")

// operatorInputRefusal reports whether a snapshot error is a refusal of its
// attachment list. Only the list rules return these errors.
func operatorInputRefusal(err error) bool {
	return errors.Is(err, domain.ErrTaskAttachmentTooLarge) ||
		errors.Is(err, domain.ErrTaskAttachmentsTooLarge) ||
		errors.Is(err, domain.ErrTooManyTaskAttachments) ||
		errors.Is(err, domain.ErrInvalidTaskAttachmentName) ||
		errors.Is(err, domain.ErrTaskAttachmentNameConflict)
}

// undeliverableWriterInputs tags an operator-input refusal with the error the
// writer stage's dispatcher records as a terminal outcome, as
// validateProductionDelivery tags a materializer refusal.
func undeliverableWriterInputs(
	run domain.Run, stage domain.Stage, invocationID domain.InvocationID, err error,
) error {
	if !errors.Is(err, errOperatorInputsUndeliverable) {
		return err
	}
	role, _ := wardRole(run, stage, invocationID)
	if role == domain.RoleSpecifier {
		return errors.Join(ErrSpecificationInputUndeliverable, err)
	}
	if _, remediation := remediationRoundForInvocation(run.ID, invocationID); remediation {
		return errors.Join(ErrRemediationInputUndeliverable, ErrProductionInputUndeliverable, err)
	}
	return errors.Join(ErrProductionInputUndeliverable, err)
}

// Admission wiring (plan §5.3, §5.7): before an attempt is dispatched, the
// engine checks the runner backend against the policy floor and records what
// admitted it, in the same transaction that appends the attempt. #39 deferred
// persisting that snapshot to exactly this point.
//
// It is configured, not implicit, and that is deliberate. The 1A.0
// conversation-turn path runs no VM, seeds no workspace, and pins no image, so
// there is nothing truthful for it to record; a default environment would
// forge the audit fact the record exists to make trustworthy. With an admitter
// configured, no attempt is appended without its audited class, because the
// two writes share one transaction.

// AdmissionEnvironment is the per-stage environment a configured admitter
// records: everything about where and how the stage runs that the daemon
// composition knows before dispatch. The per-attempt facts (identities, input
// digest) come from the attempt itself.
type AdmissionEnvironment struct {
	OperatingMode  domain.OperatingMode
	CredentialMode domain.CredentialMode
	EgressProfile  domain.EgressProfile
	// EnforceableEgressProfiles is the closed set this composition can
	// truthfully materialize. Capability retries may select only from it.
	EnforceableEgressProfiles []domain.EgressProfile
	ImageRef                  domain.ImageRef
	// PromptPackageDigest is the trusted default-branch prompt artifact for
	// every stage this environment admits. It is configuration because the
	// prompt package is control-plane authority, not invocation-owned input.
	PromptPackageDigest domain.Digest
	// ReviewConfigurationDigest is the effective Freeside-invoked reviewer
	// configuration. Unattended admission re-gates the active profile against it
	// before recording or starting an attempt.
	ReviewConfigurationDigest domain.Digest
	// VendorInstructions names the host file whose dereferenced regular-file
	// bytes are snapshotted at admission. Missing is an explicit admitted
	// absence; every other source failure refuses admission.
	VendorInstructions VendorInstructionConfig
	Base               domain.BaseRevision
	Workspace          string
	// AuthIdentityID is the provider identity the stage runs under; nil only
	// for a clean-verification stage, which reaches no provider.
	AuthIdentityID *domain.AuthIdentityID
	// Agents, when set, admits every ward stage through the lineup (§5.4):
	// the identity, credential mode, and agent binding come from the role's
	// resolved agent, and AuthIdentityID must be nil. Without it the
	// environment's one identity admits every stage, as before the cutover.
	Agents *AgentSelection
	// BackupEncryptionWaiver records the §5.7 Phase 1A.2 exception when the
	// operator has one; the store re-gates it against their configuration.
	BackupEncryptionWaiver *domain.BackupEncryptionWaiver
}

// clone detaches the environment from the caller's pointers.
func (e AdmissionEnvironment) clone() AdmissionEnvironment {
	e.EnforceableEgressProfiles = slices.Clone(e.EnforceableEgressProfiles)
	if e.AuthIdentityID != nil {
		identity := *e.AuthIdentityID
		e.AuthIdentityID = &identity
	}
	if e.Agents != nil {
		agents := *e.Agents
		agents.EffectiveEgress = slices.Clone(agents.EffectiveEgress)
		e.Agents = &agents
	}
	if e.BackupEncryptionWaiver != nil {
		waiver := *e.BackupEncryptionWaiver
		e.BackupEncryptionWaiver = &waiver
	}
	return e
}

// waivedPostureItemID is the deterministic identity of the degraded-posture
// notice for one waived admission. Deterministic so a replayed dispatch
// converges on the same item instead of raising a second one.
func waivedPostureItemID(invocationID domain.InvocationID) domain.ItemID {
	return domain.ItemID("system-health-backup-waiver-" + string(invocationID))
}

// waivedPostureItem is the §5.7 notice that an admission ran under the Phase
// 1A.2 encryption exception. The plan requires every waived admission to
// surface its degraded posture, not merely record the waiver in the audit
// record: an operator who cannot see that unattended work is proceeding on the
// temporary exception has no way to know the exception is still in use.
//
// It is raised as an ordinary open system_health item carrying the typed §4
// supersession condition: the validated waiver configuration overrides the
// item's blocking state, so the notice stays visible without blocking the
// admissions the waiver exists to permit. The condition names the waived
// repository; whether it holds is re-derived against live policy at every
// admission (store.RequireUnattendedAdmissible), so clearing the waiver makes
// this still-open notice blocking again with no write.
func waivedPostureItem(
	run domain.Run, invocationID domain.InvocationID, waiver domain.BackupEncryptionWaiver,
	createdAt time.Time, names ...*domain.DisplayNames,
) (domain.AttentionItem, error) {
	runID := run.ID
	posture := domain.HealthPostureBlocking
	var displayNames *domain.DisplayNames
	if len(names) != 0 {
		displayNames = names[0]
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: waivedPostureItemID(invocationID), ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason: fmt.Sprintf(
			"Unattended execution admitted under the Phase 1A.2 backup-encryption waiver "+
				"for repository %d (%s). Backup encryption is not verified for this run.",
			waiver.RepositoryID, waiver.Reason),
		// stop_unattended is offered now that accepting it is a durable
		// operating transition the admission gate honours (#319); before
		// that existed, offering an action the system could not honour was
		// judged worse than an absent one.
		RequestedDecision: []domain.Action{domain.ActionAcknowledge, domain.ActionStopUnattended},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: "backup_encryption_waived", Impairs: domain.ImpairedCapabilityUnattendedAdmission,
		},
		DisplayNames: displayNames,
		ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt,
		Posture:   &posture,
		BlockingSupersession: &domain.BlockingSupersession{
			Kind:         domain.SupersessionBackupEncryptionWaiver,
			RepositoryID: waiver.RepositoryID,
		},
		Status: domain.StatusOpen,
	}, nil)
}

// admitter holds the configured admission inputs.
type admitter struct {
	backend exec.RunnerBackend
	// backendConfigurationDigest is read once, for a backend whose proved
	// image is fixed; a conformanceIdentityBackend's is read per attempt.
	backendConfigurationDigest domain.Digest
	floor                      []exec.Capability
	environment                AdmissionEnvironment
	now                        func() time.Time
}

// WithAdmission makes the engine admit every dispatched attempt against
// backend, requiring at least floor, and record the resulting snapshot as the
// attempt's durable admission. A backend below the floor fails the dispatch:
// an unmet minimum is a typed refusal, never a silent downgrade (§5.7), so no
// attempt is appended and nothing is started.
//
// An empty floor is a policy state, not a mistake: WithAdmission itself is
// what says admission is configured, so a mode that requires no minimum
// capability is expressible here exactly as it is at the persistence boundary,
// where a present-but-empty floor differs from a missing one.
//
// now supplies the admission instant, since the engine takes its clock from
// its composition rather than reading one here.
func WithAdmission(backend exec.RunnerBackend, floor []exec.Capability, env AdmissionEnvironment, now func() time.Time) Option {
	return func(e *Engine) error {
		if backend == nil {
			return errors.New("with admission: nil runner backend")
		}
		// Every stage this admitter places is a ward role, which the host
		// verification executor never runs (§5.7); it is admitted only
		// through WithHostVerificationAdmission.
		if backend.Name() == string(domain.BackendCredentialFreeHostVerification) {
			return errors.New("with admission: host verification backend cannot admit ward stages")
		}
		if now == nil {
			return errors.New("with admission: nil clock")
		}
		var backendConfigurationDigest domain.Digest
		if env.OperatingMode == domain.ModeUnattended {
			bound, ok := backend.(exec.ConfigurationBoundBackend)
			if !ok {
				return errors.New("with admission: unattended backend has no configuration digest")
			}
			backendConfigurationDigest = bound.ConfigurationDigest()
			if !contentaddr.Valid(string(backendConfigurationDigest)) ||
				backendConfigurationDigest == domain.UnboundBackendConfigurationDigest {
				return fmt.Errorf("with admission: unattended backend configuration digest %q is not bound",
					backendConfigurationDigest)
			}
			if env.ReviewConfigurationDigest == "" {
				return errors.New("with admission: unattended review configuration digest is empty")
			}
		}
		if !contentaddr.Valid(string(env.PromptPackageDigest)) {
			return fmt.Errorf("with admission: prompt package digest %q is not canonical",
				env.PromptPackageDigest)
		}
		if err := env.VendorInstructions.validate(); err != nil {
			return fmt.Errorf("with admission: %w", err)
		}
		if env.Agents != nil {
			if err := env.Agents.validate(); err != nil {
				return fmt.Errorf("with admission: %w", err)
			}
			// One source of identity: a lineup admission derives it from the
			// agent's enrollment, so a configured one could only disagree.
			if env.AuthIdentityID != nil {
				return errors.New("with admission: agent selection and a configured auth identity are exclusive")
			}
		}
		if len(env.EnforceableEgressProfiles) == 0 &&
			slices.Contains(domain.AllEgressProfiles, env.EgressProfile) {
			env.EnforceableEgressProfiles = []domain.EgressProfile{env.EgressProfile}
		}
		seenEgress := make(map[domain.EgressProfile]struct{}, len(env.EnforceableEgressProfiles))
		for _, profile := range env.EnforceableEgressProfiles {
			if !slices.Contains(domain.AllEgressProfiles, profile) {
				return fmt.Errorf("with admission: enforceable egress profile %q is invalid", profile)
			}
			if _, duplicate := seenEgress[profile]; duplicate {
				return fmt.Errorf("with admission: enforceable egress profile %q is duplicated", profile)
			}
			seenEgress[profile] = struct{}{}
			// A declaration is authority only when this exact composition can
			// construct a valid admission for the profile. Provider-reaching
			// profiles retain the configured identity; clean verification must
			// have none. A single admitter cannot truthfully promise both shapes.
			switch profile {
			case domain.EgressProviderOnly, domain.EgressProviderRegistry, domain.EgressProviderWebRead:
				if env.Agents == nil && (env.AuthIdentityID == nil || *env.AuthIdentityID == "") {
					return fmt.Errorf(
						"with admission: enforceable egress profile %q has no auth identity", profile)
				}
			case domain.EgressCleanVerification:
				if env.AuthIdentityID != nil {
					return fmt.Errorf(
						"with admission: enforceable egress profile %q retains an auth identity", profile)
				}
			}
		}
		if len(env.EnforceableEgressProfiles) != 0 {
			if _, ok := seenEgress[env.EgressProfile]; !ok {
				return fmt.Errorf("with admission: default egress profile %q is not enforceable", env.EgressProfile)
			}
		}
		// Detached from the caller's values before they become live
		// configuration: an environment or floor that followed later edits
		// could weaken the gate, or retarget the credential and waiver
		// bindings a record attests to, long after engine.New returned.
		e.admission = &admitter{
			backend:                    backend,
			backendConfigurationDigest: backendConfigurationDigest,
			floor:                      slices.Clone(floor),
			environment:                env.clone(),
			now:                        now,
		}
		return nil
	}
}

// AdmissionDerivation derives the per-attempt half of the admission
// environment: the workspace reference and the exact base revision this
// attempt runs against. The static AdmissionEnvironment cannot carry either
// once more than one run flows through a daemon (each handoff owns its
// workspace volume, and the trusted base tip moves between submissions), and
// the composition, not the engine, knows the ward's workspace naming scheme
// and how the operator resolves a base (#237). The engine replaces the
// derived base commit in three cases: a maintenance task runs at its
// refusal's tip (repairTaskRefusal), a handoff conflict's or hand-back
// cycle's remediation at the base its sealed authority names
// (handoffConflictAdmission), and a later return to the agent or remediation
// at that base when its candidate was written there
// (handoffConflictTipAdmission). Each tip is read from an
// immutable record, so every attempt derives the same base without the Run
// carrying one.
type AdmissionDerivation func(ctx context.Context, invocationID domain.InvocationID) (workspace string, base domain.BaseRevision, err error)

// WithAdmissionDerivation configures per-attempt workspace and base
// derivation. The derived values replace the environment's Workspace and
// Base for every admission this engine records; everything else stays the
// configured static environment.
func WithAdmissionDerivation(derive AdmissionDerivation) Option {
	return func(e *Engine) error {
		if derive == nil {
			return errors.New("with admission derivation: nil derivation")
		}
		e.derive = derive
		return nil
	}
}

// ImageSelection selects the writer image one attempt runs in. It returns
// the image, or an error that holds the attempt: an ImageUpkeepPendingError
// while upkeep may still make an image serve the base, or
// ErrProjectImageUnserved when nothing pending would.
//
// repair is the refusal a maintenance task's run repairs, nil for every
// other run. A repair run takes the image the refusal was measured from
// while the admitted-image pointer still names it, instead of an image that
// serves the daemon's base, and holds as unserved once the pointer moves.
type ImageSelection func(
	ctx context.Context, invocationID domain.InvocationID, repair *domain.ImageUpkeepRefusal,
) (domain.ImageRef, error)

// WithAdmissionImageSelection makes every admission name the image
// selection returns instead of the environment's static ImageRef.
func WithAdmissionImageSelection(selectImage ImageSelection) Option {
	return func(e *Engine) error {
		if selectImage == nil {
			return errors.New("with admission image selection: nil selection")
		}
		e.selectImage = selectImage
		return nil
	}
}

// ErrImageNotProved refuses an unattended attempt whose image is not the one
// the runner backend's conformance identity proves. It is joined with
// domain.ErrAdmissionConfigurationMismatch, so it holds under
// backend_not_conformant until the backend proves the image.
var ErrImageNotProved = errors.New("selected image is not the image the runner backend proves")

// conformanceIdentityBackend is a runner backend whose proved image can
// change while the daemon runs. Its identity is read per attempt so an
// admission records the digest of the image it names, never another's.
type conformanceIdentityBackend interface {
	ConformanceIdentity() (image string, digest domain.Digest)
}

// ErrEgressProfileNotEnforceable refuses an attempt whose requested egress
// profile is outside the composition's EnforceableEgressProfiles.
var ErrEgressProfileNotEnforceable = errors.New("egress profile is not enforceable by this composition")

// ErrEgressPolicyRefused refuses an attempt whose run policy does not name an
// egress request the writer can be admitted under: a profile the policy key
// cannot select, or provider_registry without a valid declared registry set.
// It marks the refusal as the run's own policy content, which holds that run
// and no other. The domain error joined to it says which rule refused.
var ErrEgressPolicyRefused = errors.New("run policy egress request is refused")

// writerEgressPolicy reads the resolved policy the writer's egress request
// comes from. A run without a readable policy, or one whose stored policy is
// not the one the run names, has no request to honor and is refused: the
// admission records the run's policy digest as what bound the profile and the
// declared registry set. Both are binding failures, not policy verdicts, so
// they carry no hold sentinel.
func (e *Engine) writerEgressPolicy(ctx context.Context, run domain.Run) (domain.ResolvedPolicy, error) {
	var policy domain.ResolvedPolicy
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		policy, err = runResolvedPolicy(ctx, tx, run)
		return err
	}); err != nil {
		return domain.ResolvedPolicy{}, err
	}
	return policy, nil
}

// runResolvedPolicy reads the policy resolved for run, refusing one whose
// digest is not the run's.
func runResolvedPolicy(ctx context.Context, tx *store.ReadTx, run domain.Run) (domain.ResolvedPolicy, error) {
	policy, err := tx.GetResolvedPolicy(ctx, run.ID)
	if err != nil {
		return domain.ResolvedPolicy{}, err
	}
	if policy.Digest != run.PolicyDigest {
		return domain.ResolvedPolicy{}, fmt.Errorf("resolved policy digest %q is not the run's %q: %w",
			policy.Digest, run.PolicyDigest, domain.ErrPolicyDigestMismatch)
	}
	return policy, nil
}

// writerEgressRefusal is the gate on the profile a writer attempt was
// selected to run under, whichever of policy or a capability manifest
// selected it. Recording the profile promises the writer runs under it, so
// one this composition cannot materialize is refused: a policy request alone
// never widens egress. A manifest names a profile, not a set, so the registry
// profile still requires a valid declared set. The driver adds the run's
// repository's adopted hosts at writer start; adoptions alone never opt in.
func writerEgressRefusal(
	enforceable []domain.EgressProfile, policy domain.ResolvedPolicy, profile domain.EgressProfile,
) error {
	if !slices.Contains(enforceable, profile) {
		return fmt.Errorf("egress profile %q: %w", profile, ErrEgressProfileNotEnforceable)
	}
	if profile == domain.EgressProviderRegistry {
		if _, err := domain.DeclaredRegistrySet(policy); err != nil {
			return fmt.Errorf("egress profile %q: %w", profile, errors.Join(ErrEgressPolicyRefused, err))
		}
	}
	return nil
}

// admitAttempt runs the capability gate and builds the durable record for one
// attempt, or reports that no admitter is configured. The input digest is the
// content address of the invocation's own binding, so the record names the
// inputs the stage ran against rather than a second notion of them.
func (e *Engine) admitAttempt(
	ctx context.Context, binding invocationBinding, stage domain.Stage, invocationID domain.InvocationID,
) (domain.ExecutionAdmission, bool, error) {
	if e.admission == nil {
		return domain.ExecutionAdmission{}, false, nil
	}
	env := e.admission.environment
	// The image is decided before the capability gate, so an attempt waiting
	// on upkeep, or on a proof of its image, holds under that cause rather
	// than under the capabilities a repoint cleared. A maintenance task's
	// exception applies only where images are selected per attempt: with a
	// static image there is no pointer for its refusal to name.
	var repair *domain.ImageUpkeepRefusal
	if e.selectImage != nil {
		var err error
		repair, err = readRepairTaskRefusal(ctx, e.store, binding.run)
		if err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: read repair refusal: %w", invocationID, err)
		}
		image, err := e.selectImage(ctx, invocationID, repair)
		if err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: select image: %w", invocationID, err)
		}
		env.ImageRef = image
	}
	backendConfigurationDigest := e.admission.backendConfigurationDigest
	if identity, ok := e.admission.backend.(conformanceIdentityBackend); ok &&
		env.OperatingMode == domain.ModeUnattended {
		image, digest := identity.ConformanceIdentity()
		if image != string(env.ImageRef) {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: image %s, backend proves %q: %w", invocationID, env.ImageRef, image,
				errors.Join(ErrImageNotProved, domain.ErrAdmissionConfigurationMismatch))
		}
		if !contentaddr.Valid(string(digest)) || digest == domain.UnboundBackendConfigurationDigest {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: backend configuration digest %q: %w",
				invocationID, digest, domain.ErrConformanceConfigurationUnbound)
		}
		backendConfigurationDigest = digest
	}
	snapshot, err := exec.CheckCapabilities(e.admission.backend, e.admission.floor)
	if err != nil {
		return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: %w", invocationID, err)
	}
	inputDigest, err := binding.invocation.ComputeInputDigest()
	if err != nil {
		return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: %w", invocationID, err)
	}
	if e.derive != nil {
		workspace, base, err := e.derive(ctx, invocationID)
		if err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: derive environment: %w", invocationID, err)
		}
		env.Workspace, env.Base = workspace, base
	}
	// A maintenance task runs at its refusal's tip and starts without
	// hydration: the image's baked manifests disagree with that tip's, so the
	// hydration helper would fail the attempt before the agent starts.
	var hydration domain.WorkspaceHydration
	if repair != nil {
		if env.Base.RepositoryID != repair.RepositoryID {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: base repository %d, repair refusal %s names %d: %w",
				invocationID, env.Base.RepositoryID, repair.ID, repair.RepositoryID,
				domain.ErrRepositoryIdentityMismatch)
		}
		env.Base.BaseSHA = repair.TipSHA
		hydration = domain.WorkspaceHydrationNone
	}
	// A remediation under a handoff conflict's authority runs at the tip that
	// authority sealed (issue #2646), so its writer is seeded there and its
	// export, verification, and review follow that base. A later cycle whose
	// candidate was written on that tip, such as a return to the agent on
	// the item the conflict's cycle published, runs there too (issue #2699).
	tip, atTip, err := handoffConflictAdmission(ctx, e.store, binding.run.ID, invocationID)
	if err != nil {
		return domain.ExecutionAdmission{}, false, fmt.Errorf(
			"admit invocation %q: read handoff conflict authority: %w", invocationID, err)
	}
	if !atTip {
		if tip, atTip, err = handoffConflictTipAdmission(ctx, e.store, binding.run.ID, stage, invocationID); err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: read handoff conflict tip: %w", invocationID, err)
		}
	}
	if atTip {
		if env.Base.Repo != tip.Repo || env.Base.RepositoryID != tip.RepositoryID || env.Base.BaseRef != tip.BaseRef {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: base %s#%d@%s, the run was admitted at %s#%d@%s: %w",
				invocationID, env.Base.Repo, env.Base.RepositoryID, env.Base.BaseRef,
				tip.Repo, tip.RepositoryID, tip.BaseRef, domain.ErrRepositoryIdentityMismatch)
		}
		env.Base.BaseSHA = tip.BaseSHA
	}
	promptPackageDigest := e.admission.environment.PromptPackageDigest
	isSpecification := stage.ID == specificationStageID(binding.run.ID) && stage.Name == specificationStageName
	isProduction := invocationID == productionInvocationID(binding.run.ID) &&
		stage.ID == productionStageID(binding.run.ID) && stage.Name == productionStageName
	if isSpecification {
		if e.specification == nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: specification stage has no specification workflow", invocationID)
		}
		promptPackageDigest = e.specification.promptPackage
	}
	if round, ok := remediationRoundForInvocation(binding.run.ID, invocationID); ok {
		if stage.ID != remediationStageID(binding.run.ID, round) || stage.Name != productionStageName ||
			e.productionPublication == nil ||
			!contentaddr.Valid(string(e.productionPublication.remediationPromptPackage)) {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: remediation prompt package is unavailable", invocationID)
		}
		isProduction = true
		promptPackageDigest = e.productionPublication.remediationPromptPackage
	}
	if stage.ID == operatorFeedbackStageID(invocationID) && stage.Name == productionStageName {
		if e.productionPublication == nil ||
			!contentaddr.Valid(string(e.productionPublication.remediationPromptPackage)) {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: operator-feedback prompt package is unavailable", invocationID)
		}
		isProduction = true
		promptPackageDigest = e.productionPublication.remediationPromptPackage
	}
	var capabilityManifestDigest *domain.Digest
	if stage.Name == productionStageName {
		// Project policy, not the composition, requests the writer's profile
		// (plan §5.4). A capability manifest selected for a retry still wins
		// below, because the operator chose it for this attempt.
		policy, err := e.writerEgressPolicy(ctx, binding.run)
		if err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q egress policy: %w", invocationID, err)
		}
		requested, err := domain.EgressProfileFromPolicy(policy)
		if err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q egress policy: %w",
				invocationID, errors.Join(ErrEgressPolicyRefused, err))
		}
		env.EgressProfile = requested
		var attempt domain.ProductionAttempt
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			attempt, err = tx.GetProductionAttemptByRun(ctx, binding.run.ID)
			return err
		}); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return domain.ExecutionAdmission{}, false, fmt.Errorf(
					"admit invocation %q production attempt: %w", invocationID, err)
			}
		} else if attempt.CapabilityManifestDigest != nil {
			manifest, err := e.capabilityManifestForRun(ctx, binding.run, *attempt.CapabilityManifestDigest)
			if err != nil {
				return domain.ExecutionAdmission{}, false, fmt.Errorf(
					"admit invocation %q capability manifest: %w", invocationID, err)
			}
			env.EgressProfile = manifest.EgressProfile
			digest := manifest.Digest
			capabilityManifestDigest = &digest
		}
		if err := writerEgressRefusal(
			e.admission.environment.EnforceableEgressProfiles, policy, env.EgressProfile,
		); err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: %w", invocationID, err)
		}
	}
	_, wardWriter := wardRole(binding.run, stage, invocationID)
	stageInputs, err := e.stageInputSnapshot(
		ctx, binding, inputDigest, promptPackageDigest, isSpecification, wardWriter,
	)
	if err != nil {
		return domain.ExecutionAdmission{}, false, fmt.Errorf(
			"admit invocation %q stage inputs: %w", invocationID,
			undeliverableWriterInputs(binding.run, stage, invocationID, err))
	}
	// A mode that must be anchored to an approved trust profile records the
	// exact revision it was admitted under, read here rather than configured:
	// the operator activates revisions at runtime, and a configured digest
	// would name whatever was current when the daemon started. The store
	// re-checks it against the activation on write, so a revision landing
	// between this read and the commit fails closed rather than being
	// recorded stale.
	var profileDigest *domain.Digest
	if env.OperatingMode == domain.ModeUnattended || env.BackupEncryptionWaiver != nil {
		var profile domain.AutomationTrustProfile
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			profile, err = tx.LatestTrustProfile(ctx, env.Base.Repo)
			if err != nil {
				return err
			}
			digest := profile.ProfileDigest
			profileDigest = &digest
			return nil
		}); err != nil {
			return domain.ExecutionAdmission{}, false,
				fmt.Errorf("admit invocation %q: trusted profile for %q: %w", invocationID, env.Base.Repo, err)
		}
		if env.OperatingMode == domain.ModeUnattended &&
			profile.Review.ConfigDigest != env.ReviewConfigurationDigest {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: review configuration for %q: %w",
				invocationID, env.Base.Repo,
				reviewConfigurationUnapprovedError(
					profile.Review.ConfigDigest, env.ReviewConfigurationDigest,
				),
			)
		}
	}
	var agent *agentAdmission
	// admittedAt is read only after triage, so the lines the router writes
	// and the admission's anchor follow the triage record in time.
	var admittedAt time.Time
	if env.Agents != nil {
		role, ok := wardRole(binding.run, stage, invocationID)
		if !ok {
			// A lineup admitter runs ward stages only; a stage no role runs
			// has no agent to bind and no identity to run under.
			return domain.ExecutionAdmission{}, false, &AgentAdmissionRefusal{
				Refusal: domain.AdmissionRefusal{Check: domain.AdmissionCheckAgentRole},
				err: fmt.Errorf(
					"admit invocation %q: stage %q has no ward role: %w", invocationID, stage.Name, ErrAgentNotAdmissible),
			}
		}
		if env.Agents.Gate != nil {
			if err := env.Agents.Gate(ctx); err != nil {
				return domain.ExecutionAdmission{}, false, &AgentAdmissionRefusal{
					Refusal: domain.AdmissionRefusal{Check: domain.AdmissionCheckAgentGate},
					err: fmt.Errorf(
						"admit invocation %q: role %s: %w", invocationID, role, errors.Join(ErrAgentNotAdmissible, err)),
				}
			}
		}
		// The triager chooses the specifier's tier before its first
		// admission and before the router (plan §5.13, Specifier Triage).
		if role == domain.RoleSpecifier {
			if err := e.triageSpecifier(ctx, *env.Agents, binding.run.ID, e.admission.now); err != nil {
				return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: triage: %w", invocationID, err)
			}
		}
		admittedAt = e.admission.now()
		// The router writes the task's lines at its first routable
		// admission, and a routed line's marked pool holds the attempt
		// (plan §5.4, routing policy). Admission then reads the line.
		if err := env.Agents.routeBeforeAdmission(ctx, e.store, binding.run.ID, role, admittedAt); err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: %w", invocationID, err)
		}
		resolved, err := resolveAgentAdmission(
			ctx, e.store, *env.Agents, binding.run.ID, role, promptPackageDigest, admittedAt,
		)
		if err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: %w", invocationID, err)
		}
		agent = &resolved
		identity := resolved.enrollment.AuthIdentityID
		env.AuthIdentityID, env.CredentialMode = &identity, resolved.enrollment.CredentialMode
	} else {
		admittedAt = e.admission.now()
	}
	var agentBinding *domain.AdmissionAgentBinding
	if agent != nil {
		agentBinding = &agent.binding
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID:               invocationID,
		RunID:                      binding.run.ID,
		StageID:                    stage.ID,
		AttemptID:                  attemptIDFor(invocationID),
		Backend:                    snapshot.Backend,
		Capabilities:               snapshot.Declared.Snapshot(),
		BackendConfigurationDigest: backendConfigurationDigest,
		OperatingMode:              env.OperatingMode,
		CredentialMode:             env.CredentialMode,
		EgressProfile:              env.EgressProfile,
		CapabilityManifestDigest:   capabilityManifestDigest,
		ImageRef:                   env.ImageRef,
		SpecDigest:                 binding.run.SpecDigest,
		PolicyDigest:               binding.run.PolicyDigest,
		InputDigest:                inputDigest,
		StageInputs:                &stageInputs,
		Base:                       env.Base,
		Workspace:                  env.Workspace,
		AuthIdentityID:             env.AuthIdentityID,
		TrustProfileDigest:         profileDigest,
		BackupEncryptionWaiver:     env.BackupEncryptionWaiver,
		AgentBinding:               agentBinding,
		WorkspaceHydration:         hydration,
		AdmittedAt:                 admittedAt,
	})
	if err != nil {
		return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: %w", invocationID, err)
	}
	if agent != nil {
		// The record is rechecked against the closure it names before it is
		// stored, so a resolver bug cannot persist a binding reconstruction
		// would refuse.
		if err := domain.ValidateAdmissionSelectionDerivations(
			admission, agent.selection, agent.adapter, agent.route,
			agent.launch, agent.stage, agent.enrollment, agent.generation, agent.identity,
		); err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: %w", invocationID, err)
		}
	}
	if isProduction {
		if err := e.validateProductionDelivery(ctx, invocationID, admission); err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q production delivery: %w", invocationID, err,
			)
		}
	}
	return admission, true, nil
}

func (e *Engine) validateProductionDelivery(
	ctx context.Context,
	invocationID domain.InvocationID,
	admission domain.ExecutionAdmission,
) error {
	production, remediation := productionDeliveryInvocation(invocationID, admission)
	if !production && !remediation {
		return nil
	}
	if e.productionDeliveryValidator == nil {
		return nil
	}
	err := e.productionDeliveryValidator(
		ctx, exec.StartSpecFromAdmission(admission),
	)
	if err == nil {
		return nil
	}
	// The materializer owns the distinction between a permanent shape refusal
	// and an operational failure. Only the former is safe to turn into a
	// terminal outcome; an unavailable blob or other transient fault must stay
	// retryable instead of being laundered into a deterministic refusal here.
	if !errors.Is(err, ErrProductionInputUndeliverable) {
		return err
	}
	if remediation {
		err = errors.Join(ErrRemediationInputUndeliverable, err)
	}
	return err
}

func productionDeliveryInvocation(
	invocationID domain.InvocationID,
	admission domain.ExecutionAdmission,
) (production, remediation bool) {
	production = invocationID == productionInvocationID(admission.RunID) &&
		admission.StageID == productionStageID(admission.RunID)
	production = production || (domain.HasOperatorFeedbackInvocationPrefix(invocationID) &&
		admission.StageID == operatorFeedbackStageID(invocationID))
	round, remediation := remediationRoundForInvocation(admission.RunID, invocationID)
	remediation = remediation && admission.StageID == remediationStageID(admission.RunID, round)
	return production, remediation
}

func (e *Engine) validateProductionReplayDelivery(
	ctx context.Context,
	invocationID domain.InvocationID,
	admission domain.ExecutionAdmission,
) error {
	production, remediation := productionDeliveryInvocation(invocationID, admission)
	if !production && !remediation {
		return nil
	}
	inspection, err := e.driver.Inspect(ctx, invocationID)
	if errors.Is(err, exec.ErrUnknownInvocation) {
		return e.validateProductionDelivery(ctx, invocationID, admission)
	}
	if err != nil {
		return fmt.Errorf("inspect admitted invocation: %w", err)
	}
	if err := inspection.Validate(); err != nil {
		return fmt.Errorf("inspect admitted invocation: %w", err)
	}
	return nil
}

// taskInputFiles maps a task's attachments, in submitted order, to the stage
// inputs every agent that receives them sees as task/<name>. Writers and the
// review agents share it, so both read the same names. Task attachments are
// written once with the task, so the list is stable for the task's life.
func taskInputFiles(
	ctx context.Context, tx *store.ReadTx, taskID domain.TaskID,
) ([]domain.StageInputAttachment, error) {
	task, err := tx.TaskAttachments(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("read task attachments: %w", err)
	}
	files := make([]domain.StageInputAttachment, 0, len(task))
	for _, a := range task {
		files = append(files, domain.StageInputAttachment{
			Digest: a.Digest, Name: "task/" + a.Name, MediaType: a.MediaType, SizeBytes: a.SizeBytes,
		})
	}
	return files, nil
}

// operatorInputFiles lists the files a Ward writer receives, in order: the
// task's attachments as task/<name>, then each user-authored message's
// attachments in the bound conversation prefix as
// discussion/<sequence>/<index>.<ext>, its media type sniffed from the stored
// bytes. Agent- and daemon-authored attachments are never delivered. The
// snapshot enforces names, counts, and byte budgets, so a breach fails
// admission; a missing blob fails here.
func (e *Engine) operatorInputFiles(
	ctx context.Context, binding invocationBinding,
) ([]domain.StageInputAttachment, error) {
	// Every stored run is bound to a task, and a prospective run carries the
	// task the store will bind it to, so an empty TaskID is a caller bug;
	// skipping would silently drop the task's files.
	if binding.run.TaskID == "" {
		return nil, fmt.Errorf("run %q names no task: %w", binding.run.ID, domain.ErrEmptyID)
	}
	var files []domain.StageInputAttachment
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		files, err = taskInputFiles(ctx, tx, binding.run.TaskID)
		return err
	}); err != nil {
		return nil, err
	}
	if binding.invocation.ConversationID == nil {
		return files, nil
	}
	for _, message := range binding.conversation.Messages[:binding.invocation.ThroughSequence] {
		if message.Author != domain.AuthorUser {
			continue
		}
		for index, digest := range message.Attachments {
			size, head, err := e.signet.DescribeAttachment(ctx, digest)
			if err != nil {
				return nil, fmt.Errorf("describe message %d attachment %d: %w", message.Sequence, index+1, err)
			}
			mediaType := domain.SniffTaskAttachmentMediaType(head)
			files = append(files, domain.StageInputAttachment{
				Digest: digest, MediaType: mediaType, SizeBytes: size,
				Name: domain.TaskAttachmentName(fmt.Sprintf(
					"discussion/%d/%d.%s", message.Sequence, index+1, discussionExtension(mediaType))),
			})
		}
	}
	return files, nil
}

// discussionExtension names a discussion file by its sniffed type, so the
// writer's tools recognize an image without a client-chosen name.
func discussionExtension(mediaType domain.TaskAttachmentMediaType) string {
	switch mediaType {
	case domain.TaskAttachmentMediaPNG:
		return "png"
	case domain.TaskAttachmentMediaJPEG:
		return "jpg"
	case domain.TaskAttachmentMediaGIF:
		return "gif"
	case domain.TaskAttachmentMediaWebP:
		return "webp"
	case domain.TaskAttachmentMediaOctetStream:
		return "bin"
	}
	return "bin"
}

// imageInputArtifactType is the Phase 1 artifact vocabulary already used for
// agent-produced image inputs. Other artifact types are prior artifacts.
const imageInputArtifactType = domain.ArtifactKindImage

func (e *Engine) stageInputSnapshot(
	ctx context.Context, binding invocationBinding, inputDigest, promptPackageDigest domain.Digest,
	isSpecification, wardWriter bool,
) (domain.StageInputSnapshot, error) {
	return e.stageInputSnapshotWithArtifacts(
		ctx, binding, inputDigest, promptPackageDigest, isSpecification, wardWriter, nil)
}

// stageInputSnapshotWithArtifacts freezes one start's inputs. wardWriter says
// the stage runs a Ward writer (specifier, implementer, or remediator), the
// only stages that receive operator input files: the task's attachments and
// the attachments on user-authored discussion messages.
func (e *Engine) stageInputSnapshotWithArtifacts(
	ctx context.Context, binding invocationBinding, inputDigest, promptPackageDigest domain.Digest,
	isSpecification, wardWriter bool, prospective map[domain.ArtifactID]domain.Artifact,
) (domain.StageInputSnapshot, error) {
	vendorInstructions, vendorBody, err := snapshotVendorInstructions(
		ctx, e.admission.environment.VendorInstructions,
	)
	if err != nil {
		return domain.StageInputSnapshot{}, err
	}
	if isSpecification {
		separator := ""
		if len(vendorBody) > 0 && vendorBody[len(vendorBody)-1] != '\n' {
			separator = "\n"
		}
		vendorBody = append(vendorBody, separator+"\n"+specificationSystemContract...)
		if int64(len(vendorBody)) > domain.MaxVendorInstructionBytes {
			return domain.StageInputSnapshot{}, fmt.Errorf(
				"%w: specification vendor instructions exceed %d bytes",
				ErrSpecificationInputUndeliverable, domain.MaxVendorInstructionBytes)
		}
		digest := domain.Digest(contentaddr.Sum(vendorBody))
		vendorInstructions.Digest = &digest
	}
	priorArtifacts := make([]domain.Digest, 0, len(binding.invocation.InputIDs))
	priorArtifactRecords := make([]domain.Artifact, 0, len(binding.invocation.InputIDs))
	imageInputs := make([]domain.Digest, 0, len(binding.invocation.InputIDs))
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		for index, id := range binding.invocation.InputIDs {
			artifact, ok := prospective[id]
			if !ok {
				var err error
				artifact, err = tx.GetArtifact(ctx, id)
				if err != nil {
					return fmt.Errorf("resolve input artifact %q: %w", id, err)
				}
			}
			if artifact.Type == imageInputArtifactType {
				imageInputs = append(imageInputs, artifact.Digest)
				continue
			}
			// The primary specification already has its own prompt role. An
			// invocation commonly names that artifact in InputIDs as part of its
			// immutable input binding; repeating it as a prior artifact changes
			// provider semantics and consumes the prompt budget twice.
			if index == 0 && artifact.Digest == binding.run.SpecDigest {
				continue
			}
			priorArtifacts = append(priorArtifacts, artifact.Digest)
			priorArtifactRecords = append(priorArtifactRecords, artifact)
		}
		return nil
	}); err != nil {
		return domain.StageInputSnapshot{}, err
	}
	if isSpecification && len(priorArtifactRecords) > 0 {
		priorArtifacts = make([]domain.Digest, 0, len(priorArtifactRecords))
		for _, artifact := range priorArtifactRecords {
			envelope, err := e.encodeSpecificationPriorArtifact(ctx, artifact)
			if err != nil {
				return domain.StageInputSnapshot{}, err
			}
			digest := domain.Digest(contentaddr.Sum(envelope))
			if err := e.signet.PutStageInput(ctx, digest, envelope); err != nil {
				return domain.StageInputSnapshot{}, fmt.Errorf(
					"store specification prior artifact envelope %s: %w", digest, err)
			}
			priorArtifacts = append(priorArtifacts, digest)
		}
	}
	var conversationDigest *domain.Digest
	var conversationBody []byte
	if binding.invocation.ConversationID != nil {
		if binding.conversation.ID != *binding.invocation.ConversationID {
			return domain.StageInputSnapshot{}, fmt.Errorf(
				"conversation %q, invocation binds %q: %w",
				binding.conversation.ID, *binding.invocation.ConversationID,
				domain.ErrParentKeyMismatch)
		}
		digest, body, err := binding.conversation.PrefixContent(
			binding.invocation.ThroughSequence)
		if err != nil {
			return domain.StageInputSnapshot{}, err
		}
		conversationDigest, conversationBody = &digest, body
	}
	var attachments []domain.StageInputAttachment
	if wardWriter {
		attachments, err = e.operatorInputFiles(ctx, binding)
		if err != nil {
			return domain.StageInputSnapshot{}, err
		}
	}
	snapshot, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:          inputDigest,
		SpecificationDigest:  binding.run.SpecDigest,
		PromptPackageDigest:  promptPackageDigest,
		PolicyDigest:         binding.run.PolicyDigest,
		VendorInstructions:   &vendorInstructions,
		ConversationDigest:   conversationDigest,
		PriorArtifactDigests: priorArtifacts,
		ImageInputDigests:    imageInputs,
		Attachments:          attachments,
	})
	if err != nil {
		if len(attachments) > 0 && operatorInputRefusal(err) {
			return domain.StageInputSnapshot{}, fmt.Errorf("%w: %w", errOperatorInputsUndeliverable, err)
		}
		return domain.StageInputSnapshot{}, err
	}
	if vendorInstructions.Digest != nil {
		if err := e.signet.PutStageInput(ctx, *vendorInstructions.Digest, vendorBody); err != nil {
			return domain.StageInputSnapshot{}, fmt.Errorf(
				"store vendor instructions %s: %w", *vendorInstructions.Digest, err)
		}
	}
	if conversationDigest != nil {
		if err := e.signet.PutStageInput(ctx, *conversationDigest, conversationBody); err != nil {
			return domain.StageInputSnapshot{}, fmt.Errorf(
				"store conversation prefix %s: %w", *conversationDigest, err)
		}
	}
	return snapshot, nil
}

// handoffConflictAdmission returns the base a remediation under a handoff
// conflict's authority is admitted at, and false for every other invocation.
// The commit is the tip the authority sealed, read through the authority's
// gate, which proved it against the predecessor's sealed handoff check; the
// request's own base is never the source.
//
// A remediation under a hand_back authority is admitted at that authority's
// base too (issue #3160): the adopted head's merge base with the target, which
// the cycle proved reachable from the target branch, with the head descending
// from it, before its review routed a finding to a fix. The repository coordinates are the
// run's production admission's, the ones the handoff audited, so the caller
// refuses a derived base of another repository or branch.
func handoffConflictAdmission(
	ctx context.Context, st *store.Store, runID domain.RunID, invocationID domain.InvocationID,
) (domain.BaseRevision, bool, error) {
	if _, ok := remediationRoundForInvocation(runID, invocationID); !ok {
		return domain.BaseRevision{}, false, nil
	}
	var (
		base     domain.BaseRevision
		conflict bool
	)
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(ctx, string(invocationID))
		// The dispatch lane admits a remediation only from its intent, so one
		// without an intent is under no handoff conflict authority.
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		request, err := decodeRemediationRequest(entry)
		if err != nil || request.SuccessorPublicationID == "" {
			return err
		}
		successor, err := tx.GetPublicationSuccessor(ctx, runID, request.SuccessorPublicationID)
		if err != nil || (!successor.RemediatesHandoffConflict() && !successor.HandsBack()) {
			return err
		}
		if !successor.AllowsRemediation(domain.RemediationInvocationIntent(request)) {
			return domain.ErrParentKeyMismatch
		}
		production, err := tx.GetExecutionAdmissionRecord(ctx, productionInvocationID(runID))
		if err != nil {
			return err
		}
		base = production.Base
		base.BaseSHA = successor.Reentry.BaseSHA
		conflict = true
		return nil
	})
	return base, conflict, err
}

// handoffConflictTipAdmission returns the base an operator-feedback
// invocation or a remediation not under a handoff conflict's authority is
// admitted at when its candidate was written on a tip such an authority
// sealed, and false for every other invocation (issue #2699). The commit is
// the one the invocation's request names: for feedback, the returned
// published candidate's base, read through FeedbackPublicationParent's
// retry and answer ancestry; for a remediation, its request's base. The
// request only selects the tip. What admits it is a handoff_conflict
// authority in the run's successor chain that names that same tip, read
// through the store's gate (PublicationSuccessorChain), which re-proves each
// authority against its sealed handoff check. A request naming any other
// commit keeps the derived base. The repository coordinates are the run's
// production admission's, as for handoffConflictAdmission. A hand_back
// authority in the chain admits its base the same way (issue #3160).
//
// Every attempt derives the same base: the request is immutable, and a
// candidate is written on a sealed tip only after the authority naming it
// sealed, so the chain already holds that authority when the request is
// written and keeps it.
func handoffConflictTipAdmission(
	ctx context.Context, st *store.Store, runID domain.RunID, stage domain.Stage, invocationID domain.InvocationID,
) (domain.BaseRevision, bool, error) {
	_, remediation := remediationRoundForInvocation(runID, invocationID)
	// The stage, not the prefix alone, makes an operator-feedback writer: a
	// specification discussion's ID can carry the same prefix.
	feedback := domain.HasOperatorFeedbackInvocationPrefix(invocationID) &&
		stage.ID == operatorFeedbackStageID(invocationID) && stage.Name == productionStageName
	if !remediation && !feedback {
		return domain.BaseRevision{}, false, nil
	}
	var (
		base  domain.BaseRevision
		atTip bool
	)
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var requested string
		if feedback {
			// As for a remediation below, a feedback without an intent names
			// no candidate.
			if _, err := tx.GetOutbox(ctx, string(invocationID)); errors.Is(err, store.ErrNotFound) {
				return nil
			} else if err != nil {
				return err
			}
			returned, _, found, err := tx.FeedbackPublicationParent(ctx, invocationID)
			// A question answered before publication returns no candidate.
			if err != nil || !found {
				return err
			}
			if returned.RunID != runID {
				return domain.ErrParentKeyMismatch
			}
			requested = returned.BaseSHA
		} else {
			entry, err := tx.GetOutbox(ctx, string(invocationID))
			// The dispatch lane admits a remediation only from its intent,
			// so, as in handoffConflictAdmission, one without an intent
			// names no candidate.
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			request, err := decodeRemediationRequest(entry)
			if err != nil {
				return err
			}
			requested = request.BaseSHA
		}
		if requested == "" {
			return nil
		}
		chain, err := tx.PublicationSuccessorChain(ctx, runID)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(chain, func(s domain.PublicationSuccessor) bool {
			return (s.RemediatesHandoffConflict() || s.HandsBack()) && s.Reentry.BaseSHA == requested
		}) {
			return nil
		}
		production, err := tx.GetExecutionAdmissionRecord(ctx, productionInvocationID(runID))
		if err != nil {
			return err
		}
		base = production.Base
		base.BaseSHA = requested
		atTip = true
		return nil
	})
	return base, atTip, err
}
