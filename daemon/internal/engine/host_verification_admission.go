package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// HostVerificationAdmission is what the host verification gate admitted: the
// credential-free macOS verification executor (§5.7, macOS Execution), named
// by its class, with the declaration and configuration it was admitted
// under, for one run's verification job. It is an in-memory result room
// selection logs and then drops: no durable record of it exists, because a
// restart reruns verification from its checkpoint and admits again.
type HostVerificationAdmission struct {
	InvocationID               domain.InvocationID
	RunID                      domain.RunID
	Backend                    string
	Capabilities               domain.CapabilitySnapshot
	BackendConfigurationDigest domain.Digest
	OperatingMode              domain.OperatingMode
	AdmittedAt                 time.Time
}

// ErrHostVerificationNotAdmissible is the class sentinel for every host
// verification refusal; HostVerificationRefusal unwraps to it.
var ErrHostVerificationNotAdmissible = errors.New("host verification is not admissible")

// HostVerificationRefusal is the typed refusal of the host verification gate.
// Missing names the capabilities the executor failed to declare; a
// conformance refusal instead unwraps to the store's or domain's sentinel
// (ErrBackendNotConformant, ErrAdmissionConfigurationMismatch,
// ErrAdmissionExceedsConformance, ErrConformanceConfigurationUnbound), so a
// caller reads the reason without parsing the message.
type HostVerificationRefusal struct {
	InvocationID domain.InvocationID
	Reason       string
	Missing      []exec.Capability
	cause        error
}

func (r *HostVerificationRefusal) Error() string {
	if r.cause != nil {
		return fmt.Sprintf("host verification %s: %s: %v", r.InvocationID, r.Reason, r.cause)
	}
	return fmt.Sprintf("host verification %s: %s", r.InvocationID, r.Reason)
}

func (r *HostVerificationRefusal) Unwrap() []error {
	if r.cause == nil {
		return []error{ErrHostVerificationNotAdmissible}
	}
	return []error{ErrHostVerificationNotAdmissible, r.cause}
}

// WithHostVerificationAdmission configures the credential-free macOS
// verification executor as the engine's host verification backend, and
// newRoom as the constructor of the room its admitted jobs run in. Only a
// backend named by that class is accepted, and the ward admitter refuses
// one (WithAdmission), so the executor serves verification and nothing else.
// The gate also needs WithAdmission, for the operating mode and clock; it
// checks that on each call, since option order is not fixed.
func WithHostVerificationAdmission(
	backend exec.ConfigurationBoundBackend, newRoom func() (verify.Room, error),
) Option {
	return func(e *Engine) error {
		if backend == nil {
			return errors.New("with host verification admission: nil backend")
		}
		if newRoom == nil {
			return errors.New("with host verification admission: nil room constructor")
		}
		if backend.Name() != string(domain.BackendCredentialFreeHostVerification) {
			return fmt.Errorf("with host verification admission: backend %q is not the %q class",
				backend.Name(), domain.BackendCredentialFreeHostVerification)
		}
		e.hostVerification = backend
		e.newHostVerificationRoom = newRoom
		return nil
	}
}

// admitHostVerification is the Tier A gate that places a run's verification
// job on the host executor. It re-checks, on every call:
//
//   - the operating mode is attended_dev or unattended;
//   - the job is the task's own verification invocation, derived from its
//     producing invocation, from its sealed successor (a re-entry), or from
//     its own publication (the handoff merge check), and never an
//     invocation a ward role (writer, remediator, specifier) runs under;
//   - the executor's live declaration clears the class-aware minimum for the
//     operating mode (domain.RequiredCapabilities): nothing under
//     attended_dev, exactly supports_credential_free_host_verification under
//     unattended;
//   - under unattended, the executor's configuration digest is bound and the
//     executor class's current durable conformance record is a pass for that
//     digest covering the declaration (store.RequireHostVerificationConformant).
//
// Any failure is a HostVerificationRefusal.
func (e *Engine) admitHostVerification(
	ctx context.Context, task productionPublicationTask,
) (HostVerificationAdmission, error) {
	invocationID := task.verificationInvocationID()
	refuse := func(reason string, cause error) (HostVerificationAdmission, error) {
		return HostVerificationAdmission{}, &HostVerificationRefusal{
			InvocationID: invocationID, Reason: reason, cause: cause,
		}
	}
	if e.hostVerification == nil {
		return refuse("no host verification backend is configured", nil)
	}
	if e.admission == nil {
		return refuse("admission is not configured", nil)
	}
	if role, ok := wardInvocationRole(task.RunID, invocationID); ok {
		return refuse(fmt.Sprintf("invocation runs the ward role %s", role), nil)
	}
	if !ownVerificationInvocation(task) {
		return refuse(fmt.Sprintf("invocation is not one of run %s's verifications", task.RunID),
			domain.ErrParentKeyMismatch)
	}

	backend := e.hostVerification
	mode := e.admission.environment.OperatingMode
	// The conformance checks below run only under unattended, so a mode
	// outside the vocabulary must refuse here rather than skip them: no
	// persisted record re-validates this admission the way
	// ExecutionAdmission.Validate does for a ward stage.
	switch mode {
	case domain.ModeAttendedDev, domain.ModeUnattended:
	default:
		return refuse(fmt.Sprintf("operating mode %q", mode), domain.ErrInvalidOperatingMode)
	}
	digest := backend.ConfigurationDigest()
	floor, err := domain.RequiredCapabilities(mode, domain.BackendCredentialFreeHostVerification, nil)
	if err != nil {
		return refuse("operating mode has no host verification minimum", err)
	}
	admitted, err := exec.CheckCapabilities(backend, floor)
	if err != nil {
		var below *exec.CapabilityRefusal
		if errors.As(err, &below) {
			return HostVerificationAdmission{}, &HostVerificationRefusal{
				InvocationID: invocationID,
				Reason:       "executor declaration is below the minimum",
				Missing:      below.Missing,
				cause:        err,
			}
		}
		return refuse("capability check", err)
	}
	capabilities := admitted.Declared.Snapshot()
	if mode == domain.ModeUnattended {
		if !contentaddr.Valid(string(digest)) || digest == domain.UnboundBackendConfigurationDigest {
			return refuse(fmt.Sprintf("executor configuration digest %q is not bound", digest),
				domain.ErrConformanceConfigurationUnbound)
		}
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			return tx.RequireHostVerificationConformant(ctx, digest, capabilities)
		}); err != nil {
			return refuse("executor conformance", err)
		}
	}
	return HostVerificationAdmission{
		InvocationID:               invocationID,
		RunID:                      task.RunID,
		Backend:                    backend.Name(),
		Capabilities:               capabilities,
		BackendConfigurationDigest: digest,
		OperatingMode:              mode,
		AdmittedAt:                 e.admission.now().UTC(),
	}, nil
}

// ownVerificationInvocation reports whether task.VerificationID is one the
// task's own authority derives: its producer's verification (a first cycle or
// a remediation round), its sealed successor's re-entry verification, or the
// handoff merge check of its own publication. A successor of another run
// derives nothing here.
func ownVerificationInvocation(task productionPublicationTask) bool {
	if task.VerificationID == "" {
		return false
	}
	if task.VerificationID == productionVerificationInvocationIDForProducer(task.RunID, task.ProducingInvocationID) {
		return true
	}
	publication := domain.ProductionPublicationInvocationID(task.RunID)
	if successor := task.Successor; successor != nil {
		if successor.RunID != task.RunID {
			return false
		}
		if task.VerificationID == reentryVerificationInvocationID(*successor) &&
			task.PublicationID == successor.PublicationID() {
			return true
		}
		publication = successor.PublicationID()
	}
	return task.PublicationID == publication && task.VerificationID == handoffVerificationInvocationID(task)
}

// admitHostVerificationRoom admits task's verification job onto the host
// executor (admitHostVerification) and builds the room it runs in. A room the
// constructor cannot build is a refusal too: the configuration that failed it
// fails it again on every call.
func (e *Engine) admitHostVerificationRoom(
	ctx context.Context, task productionPublicationTask,
) (verify.Room, HostVerificationAdmission, error) {
	admission, err := e.admitHostVerification(ctx, task)
	if err != nil {
		return nil, HostVerificationAdmission{}, err
	}
	room, err := e.newHostVerificationRoom()
	if err != nil {
		return nil, HostVerificationAdmission{}, &HostVerificationRefusal{
			InvocationID: admission.InvocationID, Reason: "construct the executor's room", cause: err,
		}
	}
	return room, admission, nil
}

// wardInvocationRole reports the ward role an invocation of run would run
// under, if any: the production writer, a remediation round or operator
// feedback return, or a specification iteration. The host executor never
// runs one.
func wardInvocationRole(runID domain.RunID, id domain.InvocationID) (domain.RoleName, bool) {
	switch {
	case id == productionInvocationID(runID):
		return domain.RoleImplementer, true
	case domain.HasOperatorFeedbackInvocationPrefix(id):
		return domain.RoleRemediator, true
	}
	if _, ok := remediationRoundForInvocation(runID, id); ok {
		return domain.RoleRemediator, true
	}
	if specRun, ok := domain.SpecificationRunIDFromInvocationID(id); ok && specRun == runID {
		return domain.RoleSpecifier, true
	}
	return "", false
}
