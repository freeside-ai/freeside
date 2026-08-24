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
	ImageRef       domain.ImageRef
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
	// BackupEncryptionWaiver records the §5.7 Phase 1A.2 exception when the
	// operator has one; the store re-gates it against their configuration.
	BackupEncryptionWaiver *domain.BackupEncryptionWaiver
}

// clone detaches the environment from the caller's pointers.
func (e AdmissionEnvironment) clone() AdmissionEnvironment {
	if e.AuthIdentityID != nil {
		identity := *e.AuthIdentityID
		e.AuthIdentityID = &identity
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
	createdAt time.Time,
) (domain.AttentionItem, error) {
	runID := run.ID
	posture := domain.HealthPostureBlocking
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
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
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
	backend                    exec.RunnerBackend
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
// and how the operator resolves a base (#237).
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
	snapshot, err := exec.CheckCapabilities(e.admission.backend, e.admission.floor)
	if err != nil {
		return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: %w", invocationID, err)
	}
	inputDigest, err := binding.invocation.ComputeInputDigest()
	if err != nil {
		return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: %w", invocationID, err)
	}
	env := e.admission.environment
	if e.derive != nil {
		workspace, base, err := e.derive(ctx, invocationID)
		if err != nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: derive environment: %w", invocationID, err)
		}
		env.Workspace, env.Base = workspace, base
	}
	promptPackageDigest := e.admission.environment.PromptPackageDigest
	isElaboration := stage.ID == elaborationStageID(binding.run.ID) && stage.Name == elaborationStageName
	isProduction := invocationID == productionInvocationID(binding.run.ID) &&
		stage.ID == productionStageID(binding.run.ID) && stage.Name == productionStageName
	if isElaboration {
		if e.elaboration == nil {
			return domain.ExecutionAdmission{}, false, fmt.Errorf(
				"admit invocation %q: elaboration stage has no elaboration workflow", invocationID)
		}
		promptPackageDigest = e.elaboration.promptPackage
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
	stageInputs, err := e.stageInputSnapshot(
		ctx, binding, inputDigest, promptPackageDigest, isElaboration,
	)
	if err != nil {
		return domain.ExecutionAdmission{}, false, fmt.Errorf(
			"admit invocation %q stage inputs: %w", invocationID, err)
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
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID:               invocationID,
		RunID:                      binding.run.ID,
		StageID:                    stage.ID,
		AttemptID:                  attemptIDFor(invocationID),
		Backend:                    snapshot.Backend,
		Capabilities:               snapshot.Declared.Snapshot(),
		BackendConfigurationDigest: e.admission.backendConfigurationDigest,
		OperatingMode:              env.OperatingMode,
		CredentialMode:             env.CredentialMode,
		EgressProfile:              env.EgressProfile,
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
		AdmittedAt:                 e.admission.now(),
	})
	if err != nil {
		return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: %w", invocationID, err)
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
	initial, remediation := productionDeliveryInvocation(invocationID, admission)
	if !initial && !remediation {
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
) (initial, remediation bool) {
	initial = invocationID == productionInvocationID(admission.RunID) &&
		admission.StageID == productionStageID(admission.RunID)
	round, remediation := remediationRoundForInvocation(admission.RunID, invocationID)
	remediation = remediation && admission.StageID == remediationStageID(admission.RunID, round)
	return initial, remediation
}

func (e *Engine) validateProductionReplayDelivery(
	ctx context.Context,
	invocationID domain.InvocationID,
	admission domain.ExecutionAdmission,
) error {
	initial, remediation := productionDeliveryInvocation(invocationID, admission)
	if !initial && !remediation {
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

// imageInputArtifactType is the Phase 1 artifact vocabulary already used for
// agent-produced image inputs. Other artifact types are prior artifacts.
const imageInputArtifactType = domain.ArtifactKindImage

func (e *Engine) stageInputSnapshot(
	ctx context.Context, binding invocationBinding, inputDigest, promptPackageDigest domain.Digest,
	isElaboration bool,
) (domain.StageInputSnapshot, error) {
	return e.stageInputSnapshotWithArtifacts(ctx, binding, inputDigest, promptPackageDigest, isElaboration, nil)
}

func (e *Engine) stageInputSnapshotWithArtifacts(
	ctx context.Context, binding invocationBinding, inputDigest, promptPackageDigest domain.Digest,
	isElaboration bool, prospective map[domain.ArtifactID]domain.Artifact,
) (domain.StageInputSnapshot, error) {
	vendorInstructions, vendorBody, err := snapshotVendorInstructions(
		ctx, e.admission.environment.VendorInstructions,
	)
	if err != nil {
		return domain.StageInputSnapshot{}, err
	}
	if isElaboration {
		separator := ""
		if len(vendorBody) > 0 && vendorBody[len(vendorBody)-1] != '\n' {
			separator = "\n"
		}
		vendorBody = append(vendorBody, separator+"\n"+elaborationSystemContract...)
		if int64(len(vendorBody)) > domain.MaxVendorInstructionBytes {
			return domain.StageInputSnapshot{}, fmt.Errorf(
				"%w: elaboration vendor instructions exceed %d bytes",
				ErrElaborationInputUndeliverable, domain.MaxVendorInstructionBytes)
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
	if isElaboration && len(priorArtifactRecords) > 0 {
		priorArtifacts = make([]domain.Digest, 0, len(priorArtifactRecords))
		for _, artifact := range priorArtifactRecords {
			envelope, err := e.encodeElaborationPriorArtifact(ctx, artifact)
			if err != nil {
				return domain.StageInputSnapshot{}, err
			}
			digest := domain.Digest(contentaddr.Sum(envelope))
			if err := e.signet.PutStageInput(ctx, digest, envelope); err != nil {
				return domain.StageInputSnapshot{}, fmt.Errorf(
					"store elaboration prior artifact envelope %s: %w", digest, err)
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
		if !isElaboration {
			for _, message := range binding.conversation.Messages[:binding.invocation.ThroughSequence] {
				// The provider does not yet have an authenticated image-delivery path.
				// Preserve the existing opaque role outside elaboration; elaboration
				// admits only envelopes constructed from typed durable artifacts above.
				priorArtifacts = append(priorArtifacts, message.Attachments...)
			}
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
	})
	if err != nil {
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
