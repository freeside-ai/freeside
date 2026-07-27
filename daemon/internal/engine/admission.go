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
	Base                domain.BaseRevision
	Workspace           string
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
) (domain.AttentionItem, error) {
	runID := run.ID
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
		BlockingSupersession: &domain.BlockingSupersession{
			Kind:         domain.SupersessionBackupEncryptionWaiver,
			RepositoryID: waiver.RepositoryID,
		},
		Status: domain.StatusOpen,
	}, nil)
}

// admitter holds the configured admission inputs.
type admitter struct {
	backend     exec.RunnerBackend
	floor       []exec.Capability
	environment AdmissionEnvironment
	now         func() time.Time
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
		if !contentaddr.Valid(string(env.PromptPackageDigest)) {
			return fmt.Errorf("with admission: prompt package digest %q is not canonical",
				env.PromptPackageDigest)
		}
		// Detached from the caller's values before they become live
		// configuration: an environment or floor that followed later edits
		// could weaken the gate, or retarget the credential and waiver
		// bindings a record attests to, long after engine.New returned.
		e.admission = &admitter{
			backend:     backend,
			floor:       slices.Clone(floor),
			environment: env.clone(),
			now:         now,
		}
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
	stageInputs, err := e.stageInputSnapshot(ctx, binding, inputDigest)
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
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			profile, err := tx.LatestTrustProfile(ctx, env.Base.Repo)
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
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID:           invocationID,
		RunID:                  binding.run.ID,
		StageID:                stage.ID,
		AttemptID:              attemptIDFor(invocationID),
		Backend:                snapshot.Backend,
		Capabilities:           snapshot.Declared.Snapshot(),
		OperatingMode:          env.OperatingMode,
		CredentialMode:         env.CredentialMode,
		EgressProfile:          env.EgressProfile,
		ImageRef:               env.ImageRef,
		SpecDigest:             binding.run.SpecDigest,
		PolicyDigest:           binding.run.PolicyDigest,
		InputDigest:            inputDigest,
		StageInputs:            &stageInputs,
		Base:                   env.Base,
		Workspace:              env.Workspace,
		AuthIdentityID:         env.AuthIdentityID,
		TrustProfileDigest:     profileDigest,
		BackupEncryptionWaiver: env.BackupEncryptionWaiver,
		AdmittedAt:             e.admission.now(),
	})
	if err != nil {
		return domain.ExecutionAdmission{}, false, fmt.Errorf("admit invocation %q: %w", invocationID, err)
	}
	return admission, true, nil
}

// imageInputArtifactType is the Phase 1 artifact vocabulary already used for
// agent-produced image inputs. Other artifact types are prior artifacts.
const imageInputArtifactType = "image"

func (e *Engine) stageInputSnapshot(
	ctx context.Context, binding invocationBinding, inputDigest domain.Digest,
) (domain.StageInputSnapshot, error) {
	priorArtifacts := make([]domain.Digest, 0, len(binding.invocation.InputIDs))
	imageInputs := make([]domain.Digest, 0, len(binding.invocation.InputIDs))
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		for _, id := range binding.invocation.InputIDs {
			artifact, err := tx.GetArtifact(ctx, id)
			if err != nil {
				return fmt.Errorf("resolve input artifact %q: %w", id, err)
			}
			if artifact.Type == imageInputArtifactType {
				imageInputs = append(imageInputs, artifact.Digest)
				continue
			}
			priorArtifacts = append(priorArtifacts, artifact.Digest)
		}
		return nil
	}); err != nil {
		return domain.StageInputSnapshot{}, err
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
		for _, message := range binding.conversation.Messages[:binding.invocation.ThroughSequence] {
			priorArtifacts = append(priorArtifacts, message.Attachments...)
		}
	}
	snapshot, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:          inputDigest,
		SpecificationDigest:  binding.run.SpecDigest,
		PromptPackageDigest:  e.admission.environment.PromptPackageDigest,
		PolicyDigest:         binding.run.PolicyDigest,
		ConversationDigest:   conversationDigest,
		PriorArtifactDigests: priorArtifacts,
		ImageInputDigests:    imageInputs,
	})
	if err != nil {
		return domain.StageInputSnapshot{}, err
	}
	if conversationDigest != nil {
		if err := e.signet.PutStageInput(ctx, *conversationDigest, conversationBody); err != nil {
			return domain.StageInputSnapshot{}, fmt.Errorf(
				"store conversation prefix %s: %w", *conversationDigest, err)
		}
	}
	return snapshot, nil
}
