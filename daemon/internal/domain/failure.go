package domain

import "fmt"

// The cross-lane failure model (plan §4, alternate-agent retry) keeps three
// axes apart so no one value has to answer two questions:
//
//   - the terminal outcome, ExecutionOutcomeStatus, says how an attempt
//     ended;
//   - the cause, FailureCause, says what went wrong and nothing about what
//     to do next;
//   - the disposition, FailureDisposition, says what policy allows next. It
//     is computed by DecideFailureDisposition from the cause plus recorded
//     facts, never stored as a property of the cause, and never inferred by
//     a diagnostic model.

// FailureCause is what went wrong in a failed invocation, in a vocabulary
// every invocation lane shares. Unknown is a real member: it is what a writer
// records when nothing positively classified the failure, so a missing
// classifier never reads as a positive cause.
type FailureCause string

const (
	FailureCauseQuota                 FailureCause = "quota"
	FailureCauseCredentialExpired     FailureCause = "credential_expired" //nolint:gosec // G101 false positive: a cause name, not a credential.
	FailureCauseCredentialRevoked     FailureCause = "credential_revoked" //nolint:gosec // G101 false positive: a cause name, not a credential.
	FailureCauseServiceCapacity       FailureCause = "service_capacity"
	FailureCauseProviderUnavailable   FailureCause = "provider_unavailable"
	FailureCauseInvalidConfiguration  FailureCause = "invalid_configuration"
	FailureCauseContractContradiction FailureCause = "contract_contradiction"
	FailureCauseUnknown               FailureCause = "unknown"
)

// AllFailureCauses lists every valid FailureCause; it is the single
// registration point.
var AllFailureCauses = []FailureCause{
	FailureCauseQuota,
	FailureCauseCredentialExpired,
	FailureCauseCredentialRevoked,
	FailureCauseServiceCapacity,
	FailureCauseProviderUnavailable,
	FailureCauseInvalidConfiguration,
	FailureCauseContractContradiction,
	FailureCauseUnknown,
}

func (c FailureCause) valid() bool {
	switch c {
	case FailureCauseQuota, FailureCauseCredentialExpired, FailureCauseCredentialRevoked,
		FailureCauseServiceCapacity, FailureCauseProviderUnavailable,
		FailureCauseInvalidConfiguration, FailureCauseContractContradiction,
		FailureCauseUnknown:
		return true
	default:
		return false
	}
}

// Validate reports whether the cause is a registered member, for packages
// outside domain that carry one (exec.StageResult).
func (c FailureCause) Validate() error {
	if !c.valid() {
		return fmt.Errorf("failure cause %q: %w", c, ErrInvalidFailureCause)
	}
	return nil
}

// CanonicalFailureCause is the cause an outcome with this status records: a
// failed outcome with no cause records unknown, so a writer that classifies
// nothing and a row written before causes existed read the same. Any other
// pairing is returned unchanged for Validate to judge.
func CanonicalFailureCause(status ExecutionOutcomeStatus, cause FailureCause) FailureCause {
	if status == ExecutionOutcomeFailed && cause == "" {
		return FailureCauseUnknown
	}
	return cause
}

// FailureDisposition is what policy allows after a failure. It is derived,
// never recorded beside the cause: the same cause can allow different next
// steps depending on what the lineup offers.
type FailureDisposition string

const (
	FailureDispositionRetrySameAgent         FailureDisposition = "retry_same_agent"
	FailureDispositionAlternateAgentEligible FailureDisposition = "alternate_agent_eligible"
	FailureDispositionWait                   FailureDisposition = "wait"
	FailureDispositionAttention              FailureDisposition = "attention"
	FailureDispositionFailLoud               FailureDisposition = "fail_loud"
)

// AllFailureDispositions lists every valid FailureDisposition; it is the
// single registration point.
var AllFailureDispositions = []FailureDisposition{
	FailureDispositionRetrySameAgent,
	FailureDispositionAlternateAgentEligible,
	FailureDispositionWait,
	FailureDispositionAttention,
	FailureDispositionFailLoud,
}

func (d FailureDisposition) valid() bool {
	switch d {
	case FailureDispositionRetrySameAgent, FailureDispositionAlternateAgentEligible,
		FailureDispositionWait, FailureDispositionAttention, FailureDispositionFailLoud:
		return true
	default:
		return false
	}
}

// FailureFacts are the recorded facts a disposition is computed from: the
// failed attempt's service route and usage pool, and the alternates the
// current lineup offers for the same role.
type FailureFacts struct {
	Route      string
	UsagePool  string
	Alternates []FailureAlternate
}

// FailureAlternate is one agent the lineup offers in place of the failed
// one. GenerationValid says whether its enrollment currently has a valid
// credential generation.
type FailureAlternate struct {
	Agent           Digest
	Route           string
	UsagePool       string
	GenerationValid bool
}

// DecideFailureDisposition computes what policy allows after a failure with
// this cause, from recorded facts only. It is deterministic and total: an
// invalid cause gets attention. An alternate counts only when it escapes what
// failed: a quota failure needs a different usage pool (two enrollments
// drawing on one pool share its quota), a capacity failure a different
// route, and a credential failure an enrollment with a valid generation. A
// failed attempt with no recorded pool or route proves no alternate escapes
// it, so it waits.
//
// Which alternate to offer, and the lineup's per-failure-class switch (plan
// §4), belong to the alternate-agent card (#869), not here.
func DecideFailureDisposition(cause FailureCause, facts FailureFacts) FailureDisposition {
	switch cause {
	case FailureCauseQuota:
		if facts.hasAlternate(func(a FailureAlternate) bool {
			return facts.UsagePool != "" && a.UsagePool != "" && a.UsagePool != facts.UsagePool
		}) {
			return FailureDispositionAlternateAgentEligible
		}
		return FailureDispositionWait
	case FailureCauseCredentialExpired, FailureCauseCredentialRevoked:
		if facts.hasAlternate(func(a FailureAlternate) bool { return a.GenerationValid }) {
			return FailureDispositionAlternateAgentEligible
		}
		return FailureDispositionAttention
	case FailureCauseServiceCapacity:
		if facts.hasAlternate(func(a FailureAlternate) bool {
			return facts.Route != "" && a.Route != "" && a.Route != facts.Route
		}) {
			return FailureDispositionAlternateAgentEligible
		}
		return FailureDispositionWait
	case FailureCauseProviderUnavailable:
		return FailureDispositionRetrySameAgent
	case FailureCauseInvalidConfiguration, FailureCauseUnknown:
		return FailureDispositionAttention
	case FailureCauseContractContradiction:
		return FailureDispositionFailLoud
	}
	return FailureDispositionAttention
}

func (f FailureFacts) hasAlternate(eligible func(FailureAlternate) bool) bool {
	for _, a := range f.Alternates {
		if eligible(a) {
			return true
		}
	}
	return false
}
