// Package intake holds the label-initiator intake contract surface: the typed
// view over the run-level WIP-cap and initiator-mode resolved-policy keys, and
// the pure gates a start decision composes (issue #720, plan §5.12). The
// reconciliation loop that counts live runs, calls these gates under the store
// write lock, and authors the start command or the durable refusal is start
// execution and lands with #659; this package is only the decidable contract
// those calls stand on.
package intake

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	// PolicyRunWIPCap is the resolved-policy key whose value is the run-level
	// WIP cap: the maximum number of concurrently active, non-terminal runs a
	// project's label intake may drive at once. A distinct axis from
	// AuthIdentity.MaxParallelExecutions, which caps an inference identity's
	// parallel executions, not a project's runs (issue #720 non-goal). Value: a
	// positive integer.
	PolicyRunWIPCap = "budgets.run_wip_cap"
	// PolicyInitiatorMode is the resolved-policy key whose value is the
	// label-intake admission posture (propose | auto_start). auto_start is
	// authorized only when this key's per-key provenance is an explicit
	// override; a preset-sourced auto_start downgrades to propose (see
	// AutoStartAuthorized and Downgraded).
	PolicyInitiatorMode = "initiator.mode"

	// MaxRunWIPCap is a defensive sanity ceiling on the parsed WIP cap. A real
	// run cap is small; a value this large is almost certainly a
	// misconfiguration, and a WIP cap is a safety limit on concurrent
	// autonomous runs, so failing closed on an implausible value beats silently
	// treating it as effectively unlimited.
	MaxRunWIPCap = 1024
)

var (
	// ErrIntakePolicyMissing marks a resolved policy that omits a required
	// intake key. Parsing fails closed: an intake decision never runs on a
	// half-specified policy.
	ErrIntakePolicyMissing = errors.New("intake policy key is missing")
	// ErrIntakePolicyMalformed marks a present intake key whose value is not a
	// valid setting (a non-positive or out-of-range cap, an unknown mode).
	ErrIntakePolicyMalformed = errors.New("intake policy key is malformed")
)

// IntakePolicy is the typed engine-side view over the two label-intake keys.
// It carries the WIP cap, the configured initiator mode, and that mode key's
// provenance, from which the authorization predicates below derive. It holds no
// live state: the WIP-cap gate takes the active-run count as an argument so the
// whole contract is a pure function of policy plus count, testable without a
// store.
type IntakePolicy struct {
	WIPCap         int
	Mode           domain.InitiatorMode
	ModeProvenance domain.ProvenanceSource
}

// ParseIntakePolicy reads the two intake keys from an authenticated resolved
// policy, failing closed when the policy is invalid, a key is missing, or a
// value is malformed. It validates the policy first, so a caller-tampered bag
// (bad digest, unattributed key) is rejected before any value is read.
func ParseIntakePolicy(resolved domain.ResolvedPolicy) (IntakePolicy, error) {
	if err := resolved.Validate(); err != nil {
		return IntakePolicy{}, fmt.Errorf("intake policy: %w", err)
	}
	keys := make(map[string]domain.PolicyKey, len(resolved.Keys))
	for _, key := range resolved.Keys {
		keys[key.Key] = key
	}
	capKey, ok := keys[PolicyRunWIPCap]
	if !ok {
		return IntakePolicy{}, fmt.Errorf("%w: %s", ErrIntakePolicyMissing, PolicyRunWIPCap)
	}
	wipCap, err := strconv.Atoi(capKey.Value)
	if err != nil || wipCap < 1 || wipCap > MaxRunWIPCap {
		return IntakePolicy{}, fmt.Errorf("%w: %s must be an integer in [1, %d]",
			ErrIntakePolicyMalformed, PolicyRunWIPCap, MaxRunWIPCap)
	}
	modeKey, ok := keys[PolicyInitiatorMode]
	if !ok {
		return IntakePolicy{}, fmt.Errorf("%w: %s", ErrIntakePolicyMissing, PolicyInitiatorMode)
	}
	mode := domain.InitiatorMode(modeKey.Value)
	if !slices.Contains(domain.AllInitiatorModes, mode) {
		return IntakePolicy{}, fmt.Errorf("%w: %s value %q",
			ErrIntakePolicyMalformed, PolicyInitiatorMode, modeKey.Value)
	}
	return IntakePolicy{
		WIPCap:         wipCap,
		Mode:           mode,
		ModeProvenance: modeKey.Provenance.Source,
	}, nil
}

// AutoStartAuthorized is the provenance predicate: auto_start is eligible only
// when the mode key's value is auto_start and its provenance is an explicit
// override. A preset-sourced auto_start is never authorized, so a shipped
// preset can never silently start runs.
func (p IntakePolicy) AutoStartAuthorized() bool {
	return p.Mode == domain.InitiatorModeAutoStart && p.ModeProvenance == domain.ProvenanceOverride
}

// EffectiveMode is the posture actually taken: auto_start only when authorized,
// otherwise the conservative propose fallback.
func (p IntakePolicy) EffectiveMode() domain.InitiatorMode {
	if p.AutoStartAuthorized() {
		return domain.InitiatorModeAutoStart
	}
	return domain.InitiatorModePropose
}

// Downgraded reports a configured auto_start that lacked override provenance
// and was reduced to propose. The reconciliation loop records
// IntakeRefusalModeNotAuthorized on the occurrence when this holds, so the
// downgrade is durable, not silent.
func (p IntakePolicy) Downgraded() bool {
	return p.Mode == domain.InitiatorModeAutoStart && !p.AutoStartAuthorized()
}

// WIPCapExhausted reports whether an active-run count is at or over the cap, so
// an authorized auto_start must be refused (IntakeRefusalWIPCapExhausted) and
// the admitted item left an ordinary proposal. The caller derives activeRuns
// under the store write lock, in the same decision that records the refusal or
// authors the start (#659), so the count and its consequence serialize.
func (p IntakePolicy) WIPCapExhausted(activeRuns int) bool {
	return activeRuns >= p.WIPCap
}
