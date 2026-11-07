package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// holdClassificationSentinels is every sentinel the dispatch path
// classifies: the operating gate, the quiet invocation and pass holds, the
// mutable admission-policy refusals, and the fenced no-more-work outcomes.
var holdClassificationSentinels = []error{
	domain.ErrUnattendedOperationStopped,
	domain.ErrBlockingSystemHealth,
	exec.ErrInputUnavailable,
	domain.ErrUsagePoolExhausted,
	domain.ErrMachineCapacity,
	ErrImageUpkeepPending,
	ErrQuotaWindowExhausted,
	store.ErrBackendNotConformant,
	domain.ErrConformanceConfigurationUnbound,
	domain.ErrAdmissionConfigurationMismatch,
	domain.ErrAdmissionExceedsConformance,
	domain.ErrBackupHealthUnavailable,
	domain.ErrCheckpointNotEncrypted,
	domain.ErrCheckpointNotCurrent,
	domain.ErrArtifactClosureIncomplete,
	domain.ErrRestoreTestStale,
	domain.ErrInvalidBackupHealthStatus,
	store.ErrRepositoryUntrusted,
	publish.ErrJanitorInactive,
	ErrProjectImageUnserved,
	domain.ErrGenerationIntegrityMarked,
	ErrEgressProfileNotEnforceable,
	ErrEgressPolicyRefused,
	exec.ErrCapabilityRefused,
	exec.ErrPreJobRefused,
	domain.ErrUnknownAdmissionFloor,
	domain.ErrCapabilityBelowFloor,
	domain.ErrCredentialModeNotApproved,
	domain.ErrWaiverNotConfigured,
	domain.ErrRepositoryIdentityMismatch,
	domain.ErrPathBoundaryMismatch,
	domain.ErrTrustProfileSuperseded,
	domain.ErrReviewConfigurationUnapproved,
	ErrAgentNotAdmissible,
	store.ErrTaskCancellationFenced,
	store.ErrTaskWorkflowClosed,
}

type holdClassificationCase struct {
	name string
	err  error
}

// joinedHoldClassificationCases are the joined errors whose precedence a
// classifier decides: which cause's reason and check win, and whether any
// cause makes a quiet predicate match.
func joinedHoldClassificationCases() []holdClassificationCase {
	return []holdClassificationCase{
		{
			"fenced joined with machine capacity",
			errors.Join(store.ErrTaskWorkflowClosed, domain.ErrMachineCapacity),
		},
		{
			"agent not admissible joined with project image unserved",
			errors.Join(ErrAgentNotAdmissible, ErrProjectImageUnserved),
		},
		{
			"agent not admissible joined with generation integrity marked",
			errors.Join(ErrAgentNotAdmissible, domain.ErrGenerationIntegrityMarked),
		},
		{
			"review configuration joined with egress policy",
			errors.Join(domain.ErrReviewConfigurationUnapproved, ErrEgressPolicyRefused),
		},
		{
			"input unavailable joined with capability refused",
			errors.Join(exec.ErrInputUnavailable, exec.ErrCapabilityRefused),
		},
		{
			"janitor inactive joined with backup health unavailable",
			errors.Join(publish.ErrJanitorInactive, domain.ErrBackupHealthUnavailable),
		},
		{
			"agent not admissible joined with blocking system health",
			errors.Join(ErrAgentNotAdmissible, domain.ErrBlockingSystemHealth),
		},
		{"typed agent gate refusal on blocking system health", &AgentAdmissionRefusal{
			Refusal: domain.AdmissionRefusal{Check: domain.AdmissionCheckAgentGate},
			err:     errors.Join(ErrAgentNotAdmissible, domain.ErrBlockingSystemHealth),
		}},
	}
}

// holdClassification is every classifier's answer for one error, as the
// dispatch lanes see it.
type holdClassification struct {
	Case                          string                   `json:"case"`
	HoldReason                    domain.RunHoldReason     `json:"hold_reason"`
	AdmissionRefusal              *domain.AdmissionRefusal `json:"admission_refusal"`
	InvocationDispatchHold        bool                     `json:"invocation_dispatch_hold"`
	UnattendedDispatchRefusal     bool                     `json:"unattended_dispatch_refusal"`
	MutableAdmissionPolicyRefusal bool                     `json:"mutable_admission_policy_refusal"`
	OperatingGateHold             domain.RunHoldReason     `json:"operating_gate_hold"`
	ClearedOnStart                bool                     `json:"cleared_on_start"`
	RefusalHoldReason             bool                     `json:"refusal_hold_reason"`
}

// classifyHold renders every classifier's answer for err, wrapped as the
// dispatch lanes wrap it.
func classifyHold(name string, err error) holdClassification {
	wrapped := fmt.Errorf("intent %q: dispatch: %w", "inv-1", err)
	reason, _ := dispatchHoldReason(wrapped)
	gate, _ := operatingGateHoldToday(wrapped)
	return holdClassification{
		Case:                          name,
		HoldReason:                    reason,
		AdmissionRefusal:              admissionRefusalDetail(wrapped),
		InvocationDispatchHold:        invocationDispatchHold(wrapped),
		UnattendedDispatchRefusal:     unattendedDispatchRefusal(wrapped),
		MutableAdmissionPolicyRefusal: MutableAdmissionPolicyRefusal(wrapped),
		OperatingGateHold:             gate,
		ClearedOnStart:                reason != "" && slices.Contains(clearOnStartHoldReasonsToday, reason),
		RefusalHoldReason:             reason != "" && slices.Contains(refusalHoldReasons, reason),
	}
}

// operatingGateHoldToday mirrors the operating-state switch in
// dispatchPendingInvocations' pre-check and dispatchAdmittedIntent's gate,
// which are inline today.
func operatingGateHoldToday(err error) (domain.RunHoldReason, bool) {
	switch {
	case errors.Is(err, domain.ErrUnattendedOperationStopped):
		return domain.HoldOperationStopped, true
	case errors.Is(err, domain.ErrBlockingSystemHealth):
		return domain.HoldBlockingSystemHealth, true
	}
	return "", false
}

// clearOnStartHoldReasonsToday mirrors the causes dispatchAdmittedIntent
// clears when an invocation starts, which are inline today.
var clearOnStartHoldReasonsToday = []domain.RunHoldReason{
	domain.HoldUsagePoolExhausted, domain.HoldIdentityParallelism,
	domain.HoldImageUpkeepPending, domain.HoldQuotaWindowExhausted, domain.HoldMachineCapacity,
}

// TestHoldClassificationGolden pins every classifier's answer for every
// classified sentinel and for the joined errors whose precedence matters, so
// a change to how the dispatch path holds, refuses, or records an error
// shows up as a golden diff.
func TestHoldClassificationGolden(t *testing.T) {
	t.Parallel()
	var rows []holdClassification
	seen := map[string]bool{}
	for _, sentinel := range holdClassificationSentinels {
		name := sentinel.Error()
		if seen[name] {
			t.Fatalf("two classified sentinels share the text %q", name)
		}
		seen[name] = true
		rows = append(rows, classifyHold(name, sentinel))
	}
	for _, tc := range joinedHoldClassificationCases() {
		if seen[tc.name] {
			t.Fatalf("two classification cases share the name %q", tc.name)
		}
		seen[tc.name] = true
		rows = append(rows, classifyHold(tc.name, tc.err))
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("marshal classifications: %v", err)
	}
	golden.Assert(t, "hold_classification", append(got, '\n'))
}
