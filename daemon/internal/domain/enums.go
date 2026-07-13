package domain

// This file holds the domain's enumerated vocabularies. Each is a named
// string type, not an iota constant: the JSON/golden token is the
// human-readable string ("spec_approval"), stable and legible on the wire for
// store and api; the zero value is the empty string, which is invalid by
// design and rejected by every Validate; and unknown values are caught by the
// per-type valid() check rather than silently accepted.
//
// Convention (Wave 0): a valid() membership check uses a switch with a
// default (it is a boolean predicate, not dispatch). A switch that *dispatches
// behaviour* on an enum omits default so the exhaustive linter forces a new
// member to be handled; see computePublishEligible and
// EligibleForEvidenceSnapshot in artifact.go for the pattern.

// AttentionType is the kind of decision an AttentionItem asks for. The ten
// Phase 1 types (plan §4), including the consolidated blocked item.
type AttentionType string

const (
	AttentionSpecApproval        AttentionType = "spec_approval"
	AttentionExecutionFailure    AttentionType = "execution_failure"
	AttentionAgentQuestion       AttentionType = "agent_question"
	AttentionReviewDiminishing   AttentionType = "review_diminishing_returns"
	AttentionReviewDispute       AttentionType = "review_dispute"
	AttentionReadyForFinalReview AttentionType = "ready_for_final_review"
	AttentionPublishBlocked      AttentionType = "publish_blocked"
	AttentionRunProposal         AttentionType = "run_proposal"
	AttentionSystemHealth        AttentionType = "system_health"
	AttentionBlocked             AttentionType = "blocked"
)

// AllAttentionTypes lists every valid AttentionType; it drives table-driven
// tests and is the single place a new type is registered.
var AllAttentionTypes = []AttentionType{
	AttentionSpecApproval,
	AttentionExecutionFailure,
	AttentionAgentQuestion,
	AttentionReviewDiminishing,
	AttentionReviewDispute,
	AttentionReadyForFinalReview,
	AttentionPublishBlocked,
	AttentionRunProposal,
	AttentionSystemHealth,
	AttentionBlocked,
}

func (t AttentionType) valid() bool {
	switch t {
	case AttentionSpecApproval, AttentionExecutionFailure, AttentionAgentQuestion,
		AttentionReviewDiminishing, AttentionReviewDispute, AttentionReadyForFinalReview,
		AttentionPublishBlocked, AttentionRunProposal, AttentionSystemHealth, AttentionBlocked:
		return true
	default:
		return false
	}
}

// SubjectType is what an AttentionItem is about (plan §4).
type SubjectType string

const (
	SubjectRun           SubjectType = "run"
	SubjectProposalBatch SubjectType = "proposal_batch"
	SubjectProject       SubjectType = "project"
	SubjectSystem        SubjectType = "system"
)

// AllSubjectTypes lists every valid SubjectType.
var AllSubjectTypes = []SubjectType{SubjectRun, SubjectProposalBatch, SubjectProject, SubjectSystem}

func (t SubjectType) valid() bool {
	switch t {
	case SubjectRun, SubjectProposalBatch, SubjectProject, SubjectSystem:
		return true
	default:
		return false
	}
}

// ProducerClass records who produced an artifact (plan §5.15 rule 2). Only
// verifier and daemon artifacts may enter evidence; agent artifacts are claims.
type ProducerClass string

const (
	ProducerVerifier ProducerClass = "verifier"
	ProducerAgent    ProducerClass = "agent"
	ProducerDaemon   ProducerClass = "daemon"
)

// AllProducerClasses lists every valid ProducerClass.
var AllProducerClasses = []ProducerClass{ProducerVerifier, ProducerAgent, ProducerDaemon}

func (c ProducerClass) valid() bool {
	switch c {
	case ProducerVerifier, ProducerAgent, ProducerDaemon:
		return true
	default:
		return false
	}
}

// DeliveryStatus is the honest lifecycle of one notification attempt (plan §4,
// decision 11). A channel provider's acceptance is never called "delivered":
// this vocabulary deliberately has no such member, and nothing maps to it.
type DeliveryStatus string

const (
	DeliverySubmitted       DeliveryStatus = "submitted"
	DeliveryChannelAccepted DeliveryStatus = "channel_accepted"
	DeliveryOpened          DeliveryStatus = "opened"
)

// AllDeliveryStatuses lists every valid DeliveryStatus.
var AllDeliveryStatuses = []DeliveryStatus{DeliverySubmitted, DeliveryChannelAccepted, DeliveryOpened}

func (s DeliveryStatus) valid() bool {
	switch s {
	case DeliverySubmitted, DeliveryChannelAccepted, DeliveryOpened:
		return true
	default:
		return false
	}
}

// InterruptionClass tags an AttentionItem for the interruption budget: a
// planned gate versus an exceptional interruption whose rate is a tracked
// health metric (plan §3.2).
type InterruptionClass string

const (
	InterruptionPlannedGate InterruptionClass = "planned_gate"
	InterruptionExceptional InterruptionClass = "exceptional"
)

// AllInterruptionClasses lists every valid InterruptionClass.
var AllInterruptionClasses = []InterruptionClass{InterruptionPlannedGate, InterruptionExceptional}

func (c InterruptionClass) valid() bool {
	switch c {
	case InterruptionPlannedGate, InterruptionExceptional:
		return true
	default:
		return false
	}
}

// Action is one decision a user can be offered on an item; an item's
// requested_decision is the set of actions offered (plan §4 Actions). The
// per-type default action set is signet policy, not domain vocabulary, so it
// lives with the attention service; this enum is only the union of tokens.
type Action string

const (
	ActionApprove              Action = "approve"
	ActionRequestChanges       Action = "request_changes"
	ActionDiscuss              Action = "discuss"
	ActionStop                 Action = "stop"
	ActionFinishNow            Action = "finish_now"
	ActionApplyThenFinish      Action = "apply_then_finish"
	ActionContinueUnderPolicy  Action = "continue_under_policy"
	ActionConvertToPolicy      Action = "convert_to_policy"
	ActionAdjudicate           Action = "adjudicate"
	ActionRetry                Action = "retry"
	ActionRetryWithCapability  Action = "retry_with_capabilities"
	ActionAnswerAndRetry       Action = "answer_and_retry"
	ActionAnswerWithoutRetry   Action = "answer_without_retry"
	ActionRerunTrustEvaluation Action = "rerun_trust_evaluation"
	ActionChooseAlternate      Action = "choose_alternate_profile"
	ActionInspectTrustFailure  Action = "inspect_trust_failure"
	ActionOpenPR               Action = "open_pr"
	ActionReturnToAgent        Action = "return_to_agent"
	ActionMarkSeen             Action = "mark_seen"
	ActionDismiss              Action = "dismiss"
	ActionStart                Action = "start"
	ActionStartWithChanges     Action = "start_with_changes"
	ActionDecline              Action = "decline"
	ActionSnooze               Action = "snooze"
	ActionAcknowledge          Action = "acknowledge"
	ActionRunDoctor            Action = "run_doctor"
	ActionStopUnattended       Action = "stop_unattended"
)

// AllActions lists every valid Action.
var AllActions = []Action{
	ActionApprove, ActionRequestChanges, ActionDiscuss, ActionStop,
	ActionFinishNow, ActionApplyThenFinish, ActionContinueUnderPolicy, ActionConvertToPolicy,
	ActionAdjudicate, ActionRetry, ActionRetryWithCapability,
	ActionAnswerAndRetry, ActionAnswerWithoutRetry,
	ActionRerunTrustEvaluation, ActionChooseAlternate, ActionInspectTrustFailure,
	ActionOpenPR, ActionReturnToAgent, ActionMarkSeen, ActionDismiss,
	ActionStart, ActionStartWithChanges, ActionDecline, ActionSnooze,
	ActionAcknowledge, ActionRunDoctor, ActionStopUnattended,
}

func (a Action) valid() bool {
	switch a {
	case ActionApprove, ActionRequestChanges, ActionDiscuss, ActionStop,
		ActionFinishNow, ActionApplyThenFinish, ActionContinueUnderPolicy, ActionConvertToPolicy,
		ActionAdjudicate, ActionRetry, ActionRetryWithCapability,
		ActionAnswerAndRetry, ActionAnswerWithoutRetry,
		ActionRerunTrustEvaluation, ActionChooseAlternate, ActionInspectTrustFailure,
		ActionOpenPR, ActionReturnToAgent, ActionMarkSeen, ActionDismiss,
		ActionStart, ActionStartWithChanges, ActionDecline, ActionSnooze,
		ActionAcknowledge, ActionRunDoctor, ActionStopUnattended:
		return true
	default:
		return false
	}
}

// Priority orders competing items. Provisional (plan §4 names the field but
// enumerates no members); flagged for spine review, tightened by a later
// kind:contract change.
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

// AllPriorities lists every valid Priority.
var AllPriorities = []Priority{PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent}

func (p Priority) valid() bool {
	switch p {
	case PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent:
		return true
	default:
		return false
	}
}

// ItemStatus is an AttentionItem's lifecycle state (plan §4 lifecycle:
// approvals supersede, resolutions transition). Provisional member set;
// flagged for spine review.
type ItemStatus string

const (
	StatusOpen       ItemStatus = "open"
	StatusResolved   ItemStatus = "resolved"
	StatusSuperseded ItemStatus = "superseded"
	StatusDismissed  ItemStatus = "dismissed"
	StatusExpired    ItemStatus = "expired"
)

// AllItemStatuses lists every valid ItemStatus.
var AllItemStatuses = []ItemStatus{StatusOpen, StatusResolved, StatusSuperseded, StatusDismissed, StatusExpired}

func (s ItemStatus) valid() bool {
	switch s {
	case StatusOpen, StatusResolved, StatusSuperseded, StatusDismissed, StatusExpired:
		return true
	default:
		return false
	}
}

// SensitivityClass is an artifact's confidentiality tier (plan §5.15 provenance,
// §5.10/§5.14 high-sensitivity handling). Provisional member set; flagged for
// spine review.
type SensitivityClass string

const (
	SensitivityNormal    SensitivityClass = "normal"
	SensitivitySensitive SensitivityClass = "sensitive"
	SensitivityHigh      SensitivityClass = "high_sensitivity"
)

// AllSensitivityClasses lists every valid SensitivityClass.
var AllSensitivityClasses = []SensitivityClass{SensitivityNormal, SensitivitySensitive, SensitivityHigh}

func (c SensitivityClass) valid() bool {
	switch c {
	case SensitivityNormal, SensitivitySensitive, SensitivityHigh:
		return true
	default:
		return false
	}
}

// Author is who wrote a conversation Message (plan §5.14). Provisional member
// set; flagged for spine review.
type Author string

const (
	AuthorUser   Author = "user"
	AuthorAgent  Author = "agent"
	AuthorDaemon Author = "daemon"
)

// AllAuthors lists every valid Author.
var AllAuthors = []Author{AuthorUser, AuthorAgent, AuthorDaemon}

func (a Author) valid() bool {
	switch a {
	case AuthorUser, AuthorAgent, AuthorDaemon:
		return true
	default:
		return false
	}
}

// ProvenanceSource records whether a resolved-policy key came from a preset
// default or an explicit override (plan §3.2, §5.12 per-key provenance).
type ProvenanceSource string

const (
	ProvenancePreset   ProvenanceSource = "preset"
	ProvenanceOverride ProvenanceSource = "override"
)

// AllProvenanceSources lists every valid ProvenanceSource.
var AllProvenanceSources = []ProvenanceSource{ProvenancePreset, ProvenanceOverride}

func (s ProvenanceSource) valid() bool {
	switch s {
	case ProvenancePreset, ProvenanceOverride:
		return true
	default:
		return false
	}
}
