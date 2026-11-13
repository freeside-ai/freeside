package signet

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ClientCommand is the in-process form of the API contract's ClientCommand
// (api/openapi.yaml): a client-prepared decision submission. The provisional
// expected_bindings map is deliberately not carried: for a decision command
// the authoritative binding set is the payload's item_version, pr_head_sha,
// and artifact_digests, which the acceptance boundary cross-checks against
// the live item (the spec's own note on expected_bindings).
type ClientCommand struct {
	// CommandID is the client-generated idempotency key: a retry with the same
	// CommandID returns the original recorded result, never a second effect.
	CommandID string
	DeviceID  domain.DeviceID
	// Kind discriminates the payload (plan §5.14). A decision command decides an
	// attention item and carries ExpectedEntityVersion and Payload; a
	// submit_task command creates a task from source text (plan §5.11) and
	// carries SubmitTask; stop_task carries StopTask and the observed version.
	// The empty zero value is treated as a decision so existing decision callers
	// stay unchanged; the HTTP boundary sets it from the payload discriminator.
	Kind domain.CommandKind
	// ExpectedEntityVersion is the store's per-row entity_version the command
	// was prepared against; distinct from the payload's domain ItemVersion. A
	// mismatch rejects the command with replacement state. Stop uses the public
	// task projection version (currently the global revision), not a row version.
	ExpectedEntityVersion int64
	Payload               DecisionPayload
	// StopTask carries the stop_task payload; zero for other command kinds.
	StopTask StopTaskPayload
	// SubmitTask carries the submit_task payload; zero for other command kinds.
	SubmitTask SubmitTaskPayload
	// The operator commands' payloads; each is zero for every other kind, and
	// an operator command carries no ExpectedEntityVersion.
	RefreshUsage      RefreshUsagePayload
	ReassignCollector ReassignCollectorPayload
	ChangeTaskLine    ChangeTaskLinePayload
	SetRoutingPolicy  SetRoutingPolicyPayload
	MoveParkedTask    MoveParkedTaskPayload
	SetWorkflowPolicy SetWorkflowPolicyPayload
}

// SubmitTaskPayload mirrors the API contract's SubmitTaskPayload: the project
// to submit into, the task's source text, an optional operator-chosen name,
// and optional task lines. It binds to no attention item and no other entity,
// so a submit_task command carries none of the decision envelope.
//
// The command's request digest is taken over this struct's JSON, so the
// field names and their order are part of every recorded submission's replay
// identity.
type SubmitTaskPayload struct {
	ProjectID domain.ProjectID
	Source    string
	// Name is an optional operator-chosen task name; empty means none, and the
	// daemon falls back to the source's heading or an identifier.
	Name string
	// TaskLines are the operator's per-role agent choices for the new task
	// (plan §5.4). Nil means none; a present, empty list is malformed. The tag
	// omits the field when there are none, so a submission without lines
	// keeps the request digest it had before the field existed and a
	// recorded one still replays.
	TaskLines []domain.TaskLineChoice `json:",omitempty"`
	// Attachments are the files the submission carries, in submitted order:
	// each names a digest the client uploaded first. Nil means none; a
	// present, empty list is malformed. Order is part of the request, so a
	// reordered list is a different submission. The tag omits the field when
	// there are none, so a submission without attachments keeps the request
	// digest it had before the field existed.
	Attachments []TaskAttachmentInput `json:",omitempty"`
}

// TaskAttachmentInput mirrors the API contract's TaskAttachmentInput: an
// uploaded digest, the relative-path name to bind it to, and the client's
// claimed media type. The daemon determines the recorded media type and size
// from the stored bytes (ResolveTaskAttachments).
type TaskAttachmentInput struct {
	Digest    domain.Digest             `json:"digest"`
	Name      domain.TaskAttachmentName `json:"name"`
	MediaType string                    `json:"media_type"`
}

// DecisionPayload mirrors the API contract's DecisionPayload: the decision and
// the exact bindings it was rendered against.
type DecisionPayload struct {
	ItemID      domain.ItemID
	Action      domain.Action
	ItemVersion int
	PRHeadSHA   string
	// ArtifactDigests is the binding set exactly as rendered to the user;
	// acceptance canonicalizes it (domain.NewCommand), so order and duplicates
	// do not affect the recorded command.
	ArtifactDigests []domain.Digest
	// Message and Attachments carry conversation content for the actions that
	// ride the conversation channel (discuss, plan §5.14). Which actions
	// require or forbid them is acceptance policy (validateCommandContent);
	// attachment order is authored content, preserved as sent.
	Message     string
	Attachments []domain.Digest
	// TaskProposalRevision, EffectProposalRevision, SnoozeUntil,
	// AlternativeChoices, FindingRoutes, CapabilityManifestDigest, and
	// AlternateAgentChoice are the typed wire inputs for the parameterized decisions
	// (start_with_changes, approve_with_changes, snooze,
	// choose_alternative_route, adjudicate_findings, retry_with_capabilities,
	// and retry_with_alternate_agent, in that order). decisionMessage
	// canonicalizes exactly the one each action names into the durable
	// Command.Message representation so retries retain the existing
	// write-once command identity.
	TaskProposalRevision     *TaskProposalRevisionInput
	EffectProposalRevision   *EffectProposalRevisionInput
	SnoozeUntil              *time.Time
	AlternativeChoices       []AlternativeChoice
	FindingRoutes            []FindingRouteChoice `json:",omitempty"`
	CapabilityManifestDigest *domain.Digest
	// AlternateAgentChoice is the enrollment, and any model and effort, a
	// retry_with_alternate_agent decision picks from the card's offers, and
	// forbidden on every other action.
	AlternateAgentChoice *AlternateAgentChoice
	// TaskLines are per-role agent choices for the task a task_proposal start
	// names (plan §5.4). Nil means absent; a present, empty list is malformed.
	// Only start and start_with_changes apply them, in the decision's
	// transaction and bound to the same item version.
	TaskLines []domain.TaskLineChoice
	// AnswerRoute is required for answer_and_retry on an implementation-stage
	// agent_question and forbidden everywhere else (validateAnswerRoute).
	AnswerRoute *domain.AnswerRoute
	// DecisionActionSurfaceDigest optionally references the daemon-derived
	// DecisionActionSurface the client rendered the decision from (plan §8). It
	// is telemetry evidence: acceptance revalidates it and stamps it onto the
	// command, but it never widens the offered actions. Absent for a client
	// build that does not adopt the action surface.
	DecisionActionSurfaceDigest *domain.Digest
}

// AlternativeChoice replaces one finding's recommended route with an offered
// route. Findings omitted from the list retain their recommendations.
type AlternativeChoice struct {
	FindingID domain.FindingID         `json:"finding_id"`
	Route     domain.AdjudicationRoute `json:"route"`
}

// FindingRouteChoice is one disputed finding's route in an adjudicate_findings
// decision. Reason is the person's own words for declining or deferring an
// external finding, which the reply in the reviewer's thread posts; every
// other choice carries none (validateReviewDisputeDecision). omitempty keeps
// a reasonless choice's canonical message byte-identical to one stored before
// reasons existed, which the store gate re-marshals.
type FindingRouteChoice struct {
	FindingID domain.FindingID         `json:"finding_id"`
	Route     domain.AdjudicationRoute `json:"route"`
	Reason    string                   `json:"reason,omitempty"`
}

// TaskProposalRevisionInput deliberately omits SubjectHandle. The store keeps
// that opaque authority binding fixed to the proposal the operator reviewed.
type TaskProposalRevisionInput struct {
	Intent            domain.TaskProposalIntent `json:"intent"`
	ExpectedCostUnits int                       `json:"expected_cost_units"`
	Scope             domain.TaskProposalScope  `json:"scope"`
}

func (in TaskProposalRevisionInput) validate() error {
	// Reuse the domain validator with a non-empty sentinel handle; the real
	// trusted handle is overlaid from the current proposal at decision time.
	return (domain.TaskProposalParameters{
		SubjectHandle: "server-bound", Intent: in.Intent,
		ExpectedCostUnits: in.ExpectedCostUnits, Scope: in.Scope,
	}).Validate()
}

// EffectProposalRevisionInput is the typed wire input for approve_with_changes
// on a source_issue_closure effect proposal. It carries only the resolve flag:
// the daemon owns the target, provenance, and origin, and rebuilds the revised
// proposal from the prior one at decision time (store.ReviseClosureProposal), so
// no other field is client-authored. A revision whose resolves equals the prior
// proposal's is rejected there because it changes no digest.
type EffectProposalRevisionInput struct {
	Resolves bool `json:"resolves"`
}

func decisionMessage(payload DecisionPayload) (string, error) {
	if payload.Action != domain.ActionAdjudicateFindings && payload.FindingRoutes != nil {
		return "", ErrInvalidReviewDisputeDecisionPayload
	}
	if payload.Action != domain.ActionRetryWithAlternateAgent && payload.AlternateAgentChoice != nil {
		return "", ErrInvalidAlternateAgentDecisionPayload
	}
	switch payload.Action {
	case domain.ActionAdjudicateFindings:
		if len(payload.FindingRoutes) == 0 || payload.AlternativeChoices != nil ||
			payload.TaskProposalRevision != nil || payload.EffectProposalRevision != nil ||
			payload.SnoozeUntil != nil || payload.Message != "" || len(payload.Attachments) != 0 ||
			payload.CapabilityManifestDigest != nil {
			return "", ErrInvalidReviewDisputeDecisionPayload
		}
		message, err := canonicalFindingRoutes(payload.FindingRoutes)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidReviewDisputeDecisionPayload, err)
		}
		return message, nil
	case domain.ActionStartWithChanges:
		if payload.TaskProposalRevision == nil || payload.EffectProposalRevision != nil ||
			payload.SnoozeUntil != nil || payload.Message != "" ||
			payload.AlternativeChoices != nil || payload.CapabilityManifestDigest != nil {
			return "", ErrInvalidProposalDecisionPayload
		}
		if err := payload.TaskProposalRevision.validate(); err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
		}
		body, err := json.Marshal(payload.TaskProposalRevision)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
		}
		return string(body), nil
	case domain.ActionApproveWithChanges:
		if payload.EffectProposalRevision == nil || payload.TaskProposalRevision != nil ||
			payload.SnoozeUntil != nil || payload.Message != "" ||
			payload.AlternativeChoices != nil || payload.CapabilityManifestDigest != nil {
			return "", ErrInvalidProposalDecisionPayload
		}
		body, err := json.Marshal(payload.EffectProposalRevision)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
		}
		return string(body), nil
	case domain.ActionSnooze:
		if payload.SnoozeUntil == nil || payload.TaskProposalRevision != nil ||
			payload.EffectProposalRevision != nil || payload.Message != "" ||
			payload.SnoozeUntil.Location() != time.UTC || payload.AlternativeChoices != nil ||
			payload.CapabilityManifestDigest != nil {
			return "", ErrInvalidProposalDecisionPayload
		}
		return payload.SnoozeUntil.Format(time.RFC3339Nano), nil
	case domain.ActionChooseAlternativeRoute:
		if payload.TaskProposalRevision != nil || payload.EffectProposalRevision != nil ||
			payload.SnoozeUntil != nil ||
			payload.Message != "" || len(payload.AlternativeChoices) == 0 ||
			payload.CapabilityManifestDigest != nil {
			return "", ErrInvalidFindingAdjudicationDecisionPayload
		}
		return canonicalAlternativeChoices(payload.AlternativeChoices)
	case domain.ActionAcceptRecommendedRoute:
		if payload.TaskProposalRevision != nil || payload.EffectProposalRevision != nil ||
			payload.SnoozeUntil != nil ||
			payload.Message != "" || payload.AlternativeChoices != nil ||
			payload.CapabilityManifestDigest != nil {
			return "", ErrInvalidFindingAdjudicationDecisionPayload
		}
		return "", nil
	case domain.ActionRetryWithCapability:
		if payload.TaskProposalRevision != nil || payload.EffectProposalRevision != nil ||
			payload.SnoozeUntil != nil ||
			payload.Message != "" || payload.AlternativeChoices != nil ||
			payload.CapabilityManifestDigest == nil ||
			!contentaddr.Valid(string(*payload.CapabilityManifestDigest)) {
			return "", ErrInvalidCapabilityRetryDecisionPayload
		}
		return string(*payload.CapabilityManifestDigest), nil
	case domain.ActionRetryWithAlternateAgent:
		// The choice's canonical message is the durable message, as the
		// manifest digest is for a capability retry, so a replay compares the
		// same choice.
		if payload.TaskProposalRevision != nil || payload.EffectProposalRevision != nil ||
			payload.SnoozeUntil != nil || payload.Message != "" || len(payload.Attachments) != 0 ||
			payload.AlternativeChoices != nil || payload.CapabilityManifestDigest != nil ||
			payload.AlternateAgentChoice == nil {
			return "", ErrInvalidAlternateAgentDecisionPayload
		}
		choice, err := payload.AlternateAgentChoice.choice()
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidAlternateAgentDecisionPayload, err)
		}
		return choice.Message(), nil
	default:
		if payload.TaskProposalRevision != nil || payload.EffectProposalRevision != nil ||
			payload.SnoozeUntil != nil ||
			payload.AlternativeChoices != nil || payload.CapabilityManifestDigest != nil {
			return "", ErrInvalidProposalDecisionPayload
		}
		return payload.Message, nil
	}
}

// CommandResult is the committed outcome of an accepted command: the durable
// record and the server revision of the transaction that applied it. A retry of
// the same CommandID returns this exact value (§5.14 test 4). The record is a
// tagged union: Record holds a decision, Submission a task submission, or Stop
// an immutable task cancellation receipt. MarshalJSON renders the wire record
// with its kind discriminator so the client can decode the matching arm.
type CommandResult struct {
	Record domain.Command
	// Stop is set only for an accepted stop_task command.
	Stop *domain.StopTaskReceipt
	// Submission is set only for an accepted submit_task command; when non-nil
	// the wire record is the submission with kind submit_task instead of the
	// decision Record.
	Submission *domain.TaskSubmission
	// Receipt is set only for an accepted operator command; the wire record is
	// its arm with the arm's kind.
	Receipt  *domain.CommandReceipt
	Revision int64
}

// kindedCommandRecord and kindedSubmissionRecord embed the domain record so its
// json-tagged fields flatten alongside the kind discriminator, producing the
// wire CommandRecord and TaskSubmissionRecord shapes (api/openapi.yaml).
type kindedCommandRecord struct {
	Kind domain.CommandKind `json:"kind"`
	domain.Command
}

// A wire field at the outer level shadows the embedded record's field of the
// same JSON name: the record's choices go out as the wire's agent and
// selection pair.
type kindedSubmissionRecord struct {
	Kind domain.CommandKind `json:"kind"`
	domain.TaskSubmission
	TaskLines []TaskLineChoice `json:"task_lines,omitempty"`
}

func (r CommandResult) MarshalJSON() ([]byte, error) {
	wire := struct {
		Record   any   `json:"record"`
		Revision int64 `json:"revision"`
	}{Revision: r.Revision}
	if r.Stop != nil {
		wire.Record = struct {
			Kind domain.CommandKind `json:"kind"`
			domain.StopTaskReceipt
		}{domain.CommandKindStopTask, *r.Stop}
	} else if r.Receipt != nil {
		record, err := kindedReceiptRecord(*r.Receipt)
		if err != nil {
			return nil, err
		}
		wire.Record = record
	} else if r.Submission != nil {
		wire.Record = kindedSubmissionRecord{
			Kind: domain.CommandKindSubmitTask, TaskSubmission: *r.Submission,
			TaskLines: wireTaskLineChoices(r.Submission.TaskLines),
		}
	} else {
		wire.Record = kindedCommandRecord{Kind: domain.CommandKindDecision, Command: r.Record}
	}
	return json.Marshal(wire)
}

// kindedReceiptRecord flattens a receipt's arm beside its kind, producing the
// wire UsageRefreshRecord, CollectorReassignmentRecord, TaskLineChangeRecord,
// RoutingPolicyChangeRecord, ParkedTaskMoveRecord, or
// WorkflowPolicyChangeRecord.
func kindedReceiptRecord(receipt domain.CommandReceipt) (any, error) {
	kind := receipt.Kind()
	switch kind {
	case domain.CommandKindRefreshUsage:
		return struct {
			Kind domain.CommandKind `json:"kind"`
			*domain.UsageRefreshReceipt
		}{kind, receipt.UsageRefresh}, nil
	case domain.CommandKindReassignCollector:
		return struct {
			Kind domain.CommandKind `json:"kind"`
			*domain.CollectorReassignmentReceipt
		}{kind, receipt.CollectorReassignment}, nil
	case domain.CommandKindChangeTaskLine:
		var line *TaskLine
		if receipt.TaskLineChange.Line != nil {
			projected := wireTaskLine(*receipt.TaskLineChange.Line)
			line = &projected
		}
		// The stored request omits a nil selection; the wire carries the
		// pair always, null for a clear and for a request from before #2598.
		return struct {
			Kind domain.CommandKind `json:"kind"`
			*domain.TaskLineChangeReceipt
			Selection *domain.TaskLineSelection `json:"selection"`
			Line      *TaskLine                 `json:"line"`
		}{kind, receipt.TaskLineChange, receipt.TaskLineChange.Selection, line}, nil
	case domain.CommandKindSetRoutingPolicy:
		return struct {
			Kind domain.CommandKind `json:"kind"`
			*domain.RoutingPolicyChangeReceipt
			Policy RoutingPolicyWire `json:"policy"`
		}{kind, receipt.RoutingPolicyChange, renderRoutingPolicy(receipt.RoutingPolicyChange.Policy)}, nil
	case domain.CommandKindSetWorkflowPolicy:
		return struct {
			Kind domain.CommandKind `json:"kind"`
			*domain.WorkflowPolicyChangeReceipt
		}{kind, receipt.WorkflowPolicyChange}, nil
	case domain.CommandKindMoveParkedTask:
		// The wire carries a choice and its record's selection. A receipt
		// stored before #2808 holds neither; no command renders one, since a
		// replay must send the choice its receipt stores (Same).
		move := receipt.ParkedTaskMove
		if move.Choice == nil || move.Retry.Selection == nil {
			return nil, domain.ErrCommandReceiptArms
		}
		return struct {
			Kind domain.CommandKind `json:"kind"`
			domain.CommandIdentity
			TaskID domain.TaskID              `json:"task_id"`
			Choice AlternateAgentChoice       `json:"choice"`
			Retry  domain.AlternateAgentRetry `json:"retry"`
			Lines  []TaskLine                 `json:"lines"`
		}{
			kind, move.CommandIdentity, move.TaskID, wireAlternateAgentChoice(*move.Choice), move.Retry,
			wireTaskLines(move.Lines),
		}, nil
	case domain.CommandKindDecision, domain.CommandKindSubmitTask, domain.CommandKindStopTask:
	}
	return nil, domain.ErrCommandReceiptArms
}
