package engine

import (
	"errors"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// holdAction is what the dispatch path does with an error a holdClasses row
// matches. The zero value "" is invalid.
type holdAction string

const (
	// holdActionOperatingGate is the durable operator stop or a blocking
	// system_health item: the pass holds every remaining intent, and the run
	// records the gate's reason.
	holdActionOperatingGate holdAction = "operating_gate"
	// holdActionInvocation holds the one invocation and lets the pass go on
	// to other work.
	holdActionInvocation holdAction = "invocation"
	// holdActionPass is an unattended refusal that ends the pass quietly.
	holdActionPass holdAction = "pass"
	// holdActionPassMutablePolicy ends the pass like holdActionPass, and is
	// also a current-policy verdict that can change without changing the
	// recorded attempt (MutableAdmissionPolicyRefusal).
	holdActionPassMutablePolicy holdAction = "pass_mutable_policy"
)

var allHoldActions = []holdAction{
	holdActionOperatingGate, holdActionInvocation, holdActionPass, holdActionPassMutablePolicy,
}

func (a holdAction) valid() bool {
	switch a {
	case holdActionOperatingGate, holdActionInvocation, holdActionPass, holdActionPassMutablePolicy:
		return true
	default:
		return false
	}
}

// holdClass is one sentinel the dispatch path classifies. reason is the hold
// the run records, empty only for the fenced no-more-work rows; check is the
// admission check an admission_policy_refused hold names, empty when the
// vocabulary has none for the sentinel; clearsOnStart marks a scheduling
// hold that starting the invocation clears.
type holdClass struct {
	sentinel      error
	action        holdAction
	reason        domain.RunHoldReason
	check         domain.AdmissionCheck
	clearsOnStart bool
}

// holdClasses is every sentinel the dispatch path classifies, in precedence
// order. A joined error records the reason and check of the first row that
// matches it and has a reason, while a quiet-hold predicate matches when any
// row of its action does. Backup-protection rows come before the general
// admission-policy rows because they are a distinct operator action.
var holdClasses = []holdClass{
	{
		sentinel: domain.ErrUnattendedOperationStopped, action: holdActionOperatingGate,
		reason: domain.HoldOperationStopped,
	},
	{
		sentinel: domain.ErrBlockingSystemHealth, action: holdActionOperatingGate,
		reason: domain.HoldBlockingSystemHealth,
	},
	{
		sentinel: exec.ErrInputUnavailable, action: holdActionInvocation,
		reason: domain.HoldInputUnavailable,
	},
	// An update drain holds each new launch on its own, not the pass: an
	// intent whose admission was recorded before the drain is running work
	// and must still start, or its held slot would keep the restart from
	// ever being ready. Starting the invocation clears the hold, as lifting
	// the drain does.
	{
		sentinel: store.ErrUpdateDrainInForce, action: holdActionInvocation,
		reason: domain.HoldUpdateDrain, clearsOnStart: true,
	},
	// Capacity, memory, image-upkeep, and quota-window holds are current
	// scheduling state, not history, so starting the invocation clears them.
	{
		sentinel: domain.ErrUsagePoolExhausted, action: holdActionInvocation,
		reason: domain.HoldUsagePoolExhausted, clearsOnStart: true,
	},
	{
		sentinel: domain.ErrMachineCapacity, action: holdActionInvocation,
		reason: domain.HoldMachineCapacity, clearsOnStart: true,
	},
	{
		sentinel: ErrImageUpkeepPending, action: holdActionInvocation,
		reason: domain.HoldImageUpkeepPending, clearsOnStart: true,
	},
	{
		sentinel: ErrQuotaWindowExhausted, action: holdActionInvocation,
		reason: domain.HoldQuotaWindowExhausted, clearsOnStart: true,
	},
	{
		sentinel: store.ErrBackendNotConformant, action: holdActionPassMutablePolicy,
		reason: domain.HoldBackendNotConformant,
	},
	{
		sentinel: domain.ErrConformanceConfigurationUnbound, action: holdActionPassMutablePolicy,
		reason: domain.HoldBackendNotConformant,
	},
	{
		sentinel: domain.ErrAdmissionConfigurationMismatch, action: holdActionPassMutablePolicy,
		reason: domain.HoldBackendNotConformant,
	},
	{
		sentinel: domain.ErrAdmissionExceedsConformance, action: holdActionPassMutablePolicy,
		reason: domain.HoldBackendNotConformant,
	},
	{
		sentinel: domain.ErrBackupHealthUnavailable, action: holdActionPassMutablePolicy,
		reason: domain.HoldBackupProtectionUnready,
	},
	{
		sentinel: domain.ErrCheckpointNotEncrypted, action: holdActionPassMutablePolicy,
		reason: domain.HoldBackupProtectionUnready,
	},
	{
		sentinel: domain.ErrCheckpointNotCurrent, action: holdActionPassMutablePolicy,
		reason: domain.HoldBackupProtectionUnready,
	},
	{
		sentinel: domain.ErrArtifactClosureIncomplete, action: holdActionPassMutablePolicy,
		reason: domain.HoldBackupProtectionUnready,
	},
	{
		sentinel: domain.ErrRestoreTestStale, action: holdActionPassMutablePolicy,
		reason: domain.HoldBackupProtectionUnready,
	},
	{
		sentinel: domain.ErrInvalidBackupHealthStatus, action: holdActionPassMutablePolicy,
		reason: domain.HoldBackupProtectionUnready,
	},
	{
		sentinel: store.ErrRepositoryUntrusted, action: holdActionPassMutablePolicy,
		reason: domain.HoldRepositoryUntrusted,
	},
	{
		sentinel: publish.ErrJanitorInactive, action: holdActionPassMutablePolicy,
		reason: domain.HoldProviderAuthorityUnavailable,
	},
	// The admission-policy rows share one reason, so their order decides only
	// the check a joined error names. A base no admitted image serves comes
	// first and names no check: the vocabulary has none for it, and naming
	// the agent check would blame an agent that was never asked. It holds its
	// own invocation, whether upkeep may still serve it or not: an image that
	// later serves the base resumes it without anyone acting.
	{
		sentinel: ErrProjectImageUnserved, action: holdActionInvocation,
		reason: domain.HoldAdmissionPolicyRefused,
	},
	// A marked generation is the agent-admission verdict reached in the
	// admitting transaction: the mark landed after role resolution read the
	// store, so the refusal arrives from the store without
	// ErrAgentNotAdmissible. It holds like the read-side refusal, and
	// re-enrollment clears it.
	{
		sentinel: domain.ErrGenerationIntegrityMarked, action: holdActionInvocation,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckGenerationIntegrityMarked,
	},
	// A writer attempt refused for the egress profile its own run requests.
	// The verdict is the run's: its policy content, or a profile this
	// composition does not enforce yet, says nothing about any other run, so
	// it holds the one invocation instead of ending the pass. A composition
	// that later enforces the profile clears the second kind; the first kind
	// stays held under its recorded reason until the operator stops the run,
	// because a run's policy does not change.
	{
		sentinel: ErrEgressProfileNotEnforceable, action: holdActionInvocation,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckEgressProfileNotEnforceable,
	},
	{
		sentinel: ErrEgressPolicyRefused, action: holdActionInvocation,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckEgressPolicyRefused,
	},
	{
		sentinel: exec.ErrCapabilityRefused, action: holdActionPass,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckBackendCapabilityRefused,
	},
	{
		sentinel: exec.ErrPreJobRefused, action: holdActionPass,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckBackendPreJobRefused,
	},
	{
		sentinel: domain.ErrUnknownAdmissionFloor, action: holdActionPassMutablePolicy,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckAdmissionFloorUnknown,
	},
	{
		sentinel: domain.ErrCapabilityBelowFloor, action: holdActionPassMutablePolicy,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckCapabilityBelowFloor,
	},
	{
		sentinel: domain.ErrCredentialModeNotApproved, action: holdActionPassMutablePolicy,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckCredentialModeNotApproved,
	},
	{
		sentinel: domain.ErrWaiverNotConfigured, action: holdActionPassMutablePolicy,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckWaiverNotConfigured,
	},
	{
		sentinel: domain.ErrRepositoryIdentityMismatch, action: holdActionPassMutablePolicy,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckRepositoryIdentityMismatch,
	},
	{
		sentinel: domain.ErrPathBoundaryMismatch, action: holdActionPassMutablePolicy,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckPathBoundaryMismatch,
	},
	{
		sentinel: domain.ErrTrustProfileSuperseded, action: holdActionPassMutablePolicy,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckTrustProfileSuperseded,
	},
	{
		sentinel: domain.ErrReviewConfigurationUnapproved, action: holdActionPassMutablePolicy,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckReviewConfigurationUnapproved,
	},
	// A role whose agent does not pass admission holds its own invocation:
	// the verdict reads the store and the selection gate, both of which can
	// change, and another role's work may still be admissible. It is last
	// among the admission rows so a more specific cause names the check.
	{
		sentinel: ErrAgentNotAdmissible, action: holdActionInvocation,
		reason: domain.HoldAdmissionPolicyRefused, check: domain.AdmissionCheckAgentAdmission,
	},
	// A fenced workflow is a no-more-work outcome (workflowAdvancementFenced):
	// it holds the invocation but records no reason, because nothing waits on
	// an operator. These rows come last so a reasoned cause joined with one
	// still records its reason.
	{sentinel: store.ErrTaskCancellationFenced, action: holdActionInvocation},
	{sentinel: store.ErrTaskWorkflowClosed, action: holdActionInvocation},
}

// firstHoldClass is the first row that matches err and satisfies keep.
func firstHoldClass(err error, keep func(holdClass) bool) (holdClass, bool) {
	for _, class := range holdClasses {
		if keep(class) && errors.Is(err, class.sentinel) {
			return class, true
		}
	}
	return holdClass{}, false
}

// matchesHoldAction reports whether any row of one of actions matches err.
func matchesHoldAction(err error, actions ...holdAction) bool {
	_, ok := firstHoldClass(err, func(class holdClass) bool {
		return slices.Contains(actions, class.action)
	})
	return ok
}

func hasHoldReason(class holdClass) bool { return class.reason != "" }

// dispatchHoldReason classifies a dispatch-path hold or refusal error onto
// the closed reason vocabulary: the reason of the first holdClasses row that
// matches and has one. Only the code crosses into the observation; an error
// that classifies onto no code records no hold and keeps its ordinary loud
// failure path.
func dispatchHoldReason(err error) (domain.RunHoldReason, bool) {
	class, ok := firstHoldClass(err, hasHoldReason)
	return class.reason, ok
}

// admissionRefusalDetail names the check, and when known the agent and the
// launch, behind an error dispatchHoldReason classifies as
// admission_policy_refused; any other error has no detail. A typed agent
// refusal names its own check; otherwise the row that decided the reason
// does, and a row with no check names none.
func admissionRefusalDetail(err error) *domain.AdmissionRefusal {
	class, ok := firstHoldClass(err, hasHoldReason)
	if !ok || class.reason != domain.HoldAdmissionPolicyRefused {
		return nil
	}
	var agent *AgentAdmissionRefusal
	if errors.As(err, &agent) {
		refusal := agent.Refusal
		return &refusal
	}
	if class.check == "" {
		return nil
	}
	return &domain.AdmissionRefusal{Check: class.check}
}

// operatingGateHold reports the operating-state hold err carries: the
// durable operator stop or a blocking system_health item.
func operatingGateHold(err error) (domain.RunHoldReason, bool) {
	class, ok := firstHoldClass(err, func(class holdClass) bool {
		return class.action == holdActionOperatingGate
	})
	return class.reason, ok
}

func invocationDispatchHold(err error) bool {
	return matchesHoldAction(err, holdActionInvocation)
}

func unattendedDispatchRefusal(err error) bool {
	return matchesHoldAction(err, holdActionPass, holdActionPassMutablePolicy)
}

// MutableAdmissionPolicyRefusal identifies a fail-closed current-policy
// verdict that can change without changing the recorded attempt. These
// verdicts hold work for a later reconcile pass. Immutable-record corruption,
// identity inconsistencies, and binding errors are deliberately absent: those
// remain fatal correctness failures.
func MutableAdmissionPolicyRefusal(err error) bool {
	return matchesHoldAction(err, holdActionPassMutablePolicy)
}

// holdReasonsWhere is the reasons of the rows keep selects, each once, in
// table order.
func holdReasonsWhere(keep func(holdClass) bool) []domain.RunHoldReason {
	var reasons []domain.RunHoldReason
	for _, class := range holdClasses {
		if keep(class) && class.reason != "" && !slices.Contains(reasons, class.reason) {
			reasons = append(reasons, class.reason)
		}
	}
	return reasons
}

// clearOnStartHoldReasons is the hold causes dispatchAdmittedIntent clears
// when an invocation starts. The store's predicate clears only these exact
// causes, so a newer, different hold that replaced one survives. A stored
// identity_parallelism row, which no sentinel produces any more, clears with
// the pool hold that replaced its reason.
var clearOnStartHoldReasons = append(
	holdReasonsWhere(func(class holdClass) bool { return class.clearsOnStart }),
	domain.HoldIdentityParallelism,
)

// refusalHoldReasons is the closed set of hold reasons the production and
// specification acceptance paths (acceptProductionAttempt,
// acceptSpecificationAttempt) can record: the reasons of the mutable
// admission-policy rows. A healthy acceptance pass clears every one, so a
// transient refusal that has recovered (for example backend conformance that
// briefly lapsed) stops being displayed as the run's hold.
var refusalHoldReasons = holdReasonsWhere(func(class holdClass) bool {
	return class.action == holdActionPassMutablePolicy
})
