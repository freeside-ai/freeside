package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrCommandReceiptArms reports a CommandReceipt that sets no arm or more than
// one.
var ErrCommandReceiptArms = errors.New("command receipt must set exactly one arm")

// CommandReceipt is the recorded result of one accepted operator command
// (refresh_usage, reassign_collector, change_task_line, set_routing_policy,
// move_parked_task, set_workflow_policy): exactly one arm is set. The accepting transaction records it with the
// command's effect, and a replay of the command_id returns it unchanged.
type CommandReceipt struct {
	UsageRefresh          *UsageRefreshReceipt
	CollectorReassignment *CollectorReassignmentReceipt
	TaskLineChange        *TaskLineChangeReceipt
	RoutingPolicyChange   *RoutingPolicyChangeReceipt
	ParkedTaskMove        *ParkedTaskMoveReceipt
	WorkflowPolicyChange  *WorkflowPolicyChangeReceipt
}

// Kind names the set arm; it is empty unless exactly one arm is set.
func (r CommandReceipt) Kind() CommandKind {
	var kinds []CommandKind
	if r.UsageRefresh != nil {
		kinds = append(kinds, CommandKindRefreshUsage)
	}
	if r.CollectorReassignment != nil {
		kinds = append(kinds, CommandKindReassignCollector)
	}
	if r.TaskLineChange != nil {
		kinds = append(kinds, CommandKindChangeTaskLine)
	}
	if r.RoutingPolicyChange != nil {
		kinds = append(kinds, CommandKindSetRoutingPolicy)
	}
	if r.ParkedTaskMove != nil {
		kinds = append(kinds, CommandKindMoveParkedTask)
	}
	if r.WorkflowPolicyChange != nil {
		kinds = append(kinds, CommandKindSetWorkflowPolicy)
	}
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

// CommandID is the set arm's command_id; empty unless exactly one arm is set.
func (r CommandReceipt) CommandID() string {
	switch r.Kind() {
	case CommandKindRefreshUsage:
		return r.UsageRefresh.CommandID
	case CommandKindReassignCollector:
		return r.CollectorReassignment.CommandID
	case CommandKindChangeTaskLine:
		return r.TaskLineChange.CommandID
	case CommandKindSetRoutingPolicy:
		return r.RoutingPolicyChange.CommandID
	case CommandKindMoveParkedTask:
		return r.ParkedTaskMove.CommandID
	case CommandKindSetWorkflowPolicy:
		return r.WorkflowPolicyChange.CommandID
	case CommandKindDecision, CommandKindSubmitTask, CommandKindStopTask:
	}
	return ""
}

// Validate reports whether exactly one arm is set and that arm is well-formed.
// The store re-runs it on every decode.
func (r CommandReceipt) Validate() error {
	switch r.Kind() {
	case CommandKindRefreshUsage:
		return r.UsageRefresh.Validate()
	case CommandKindReassignCollector:
		return r.CollectorReassignment.Validate()
	case CommandKindChangeTaskLine:
		return r.TaskLineChange.Validate()
	case CommandKindSetRoutingPolicy:
		return r.RoutingPolicyChange.Validate()
	case CommandKindMoveParkedTask:
		return r.ParkedTaskMove.Validate()
	case CommandKindSetWorkflowPolicy:
		return r.WorkflowPolicyChange.Validate()
	case CommandKindDecision, CommandKindSubmitTask, CommandKindStopTask:
	}
	return ErrCommandReceiptArms
}

// CommandIdentity is the command_id and the authenticated device every
// receipt carries.
type CommandIdentity struct {
	CommandID string   `json:"command_id"`
	DeviceID  DeviceID `json:"device_id"`
}

func (c CommandIdentity) validate() error {
	if c.CommandID == "" || c.DeviceID == "" {
		return ErrEmptyID
	}
	return nil
}

// UsageRefreshRequest is a refresh_usage command as accepted.
type UsageRefreshRequest struct {
	CommandIdentity
	Pool UsagePoolID `json:"pool"`
}

// UsageRefreshReceipt records that a refresh was requested. New readings
// arrive through the pool's usage record, never through the receipt.
type UsageRefreshReceipt struct {
	UsageRefreshRequest
	RequestedAt time.Time `json:"requested_at"`
}

func (r UsageRefreshReceipt) Validate() error {
	if err := r.validate(); err != nil {
		return fmt.Errorf("usage refresh receipt: %w", err)
	}
	if r.Pool == "" {
		return fmt.Errorf("usage refresh receipt %q pool: %w", r.CommandID, ErrEmptyID)
	}
	if r.RequestedAt.IsZero() {
		return fmt.Errorf("usage refresh receipt %q: %w", r.CommandID, ErrMissingTimestamp)
	}
	return nil
}

// CollectorReassignmentRequest is a reassign_collector command as accepted.
type CollectorReassignmentRequest struct {
	CommandIdentity
	Pool      UsagePoolID        `json:"pool"`
	Collector ClientEnrollmentID `json:"collector"`
}

// CollectorReassignmentReceipt records that the pool's collector moved.
type CollectorReassignmentReceipt struct {
	CollectorReassignmentRequest
	RecordedAt time.Time `json:"recorded_at"`
}

func (r CollectorReassignmentReceipt) Validate() error {
	if err := r.validate(); err != nil {
		return fmt.Errorf("collector reassignment receipt: %w", err)
	}
	if r.Pool == "" || r.Collector == "" {
		return fmt.Errorf("collector reassignment receipt %q: %w", r.CommandID, ErrEmptyID)
	}
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("collector reassignment receipt %q: %w", r.CommandID, ErrMissingTimestamp)
	}
	return nil
}

// TaskLineChangeRequest is a change_task_line command as accepted. It holds
// at most one of Agent and Selection, and neither for a clear: a request
// accepted since #2598 carries the client's selection, and one accepted
// before it the agent name the client sent then. Selection is omitted when
// nil, so a stored receipt from before it keeps its bytes.
type TaskLineChangeRequest struct {
	CommandIdentity
	TaskID    TaskID             `json:"task_id"`
	Role      RoleName           `json:"role"`
	Agent     *string            `json:"agent"`
	Selection *TaskLineSelection `json:"selection,omitempty"`
}

// Same reports whether two requests ask for the same change.
func (r TaskLineChangeRequest) Same(other TaskLineChangeRequest) bool {
	sameAgent := (r.Agent == nil) == (other.Agent == nil) && (r.Agent == nil || *r.Agent == *other.Agent)
	sameSelection := (r.Selection == nil) == (other.Selection == nil) &&
		(r.Selection == nil || *r.Selection == *other.Selection)
	return r.CommandIdentity == other.CommandIdentity && r.TaskID == other.TaskID && r.Role == other.Role &&
		sameAgent && sameSelection
}

func (r TaskLineChangeRequest) validate() error {
	if err := r.CommandIdentity.validate(); err != nil {
		return err
	}
	if r.TaskID == "" {
		return fmt.Errorf("task: %w", ErrEmptyID)
	}
	if !r.Role.TaskLineEligible() {
		return ErrTaskLineRoleIneligible
	}
	if r.Agent != nil && !validAgentName(*r.Agent) {
		return fmt.Errorf("role %s agent: %w", r.Role, ErrInvalidAgentName)
	}
	if r.Selection != nil {
		if r.Agent != nil {
			return fmt.Errorf("role %s names an agent and a selection: %w", r.Role, ErrInvalidTaskLineSelection)
		}
		if err := r.Selection.Validate(); err != nil {
			return fmt.Errorf("role %s: %w", r.Role, err)
		}
	}
	return nil
}

// TaskLineChangeReceipt records a line change. Recorded is false when the
// role already had the line asked for. Line is the role's current line after
// the request: nil when the role has none, because it never had one or the
// change cleared it.
type TaskLineChangeReceipt struct {
	TaskLineChangeRequest
	Recorded bool      `json:"recorded"`
	Line     *TaskLine `json:"line"`
}

func (r TaskLineChangeReceipt) Validate() error {
	if err := r.validate(); err != nil {
		return fmt.Errorf("task line change receipt %q: %w", r.CommandID, err)
	}
	if r.Line == nil {
		// Only a clear leaves the role with no line.
		if r.Agent != nil || r.Selection != nil {
			return fmt.Errorf("task line change receipt %q line: %w", r.CommandID, ErrParentKeyMismatch)
		}
		return nil
	}
	if err := r.Line.Validate(); err != nil {
		return fmt.Errorf("task line change receipt %q line: %w", r.CommandID, err)
	}
	if r.Line.Cleared || r.Line.TaskID != r.TaskID || r.Line.Role != r.Role || !r.lineMatches(*r.Line) {
		return fmt.Errorf("task line change receipt %q line: %w", r.CommandID, ErrParentKeyMismatch)
	}
	if r.Recorded && r.Line.Source != TaskLineSourceClientLineChange {
		return fmt.Errorf("task line change receipt %q line source %q: %w",
			r.CommandID, r.Line.Source, ErrInvalidTaskLineSource)
	}
	return nil
}

// lineMatches reports whether line holds what the request asked for. A
// request since #2598 records its own selection as a v2 line. One from
// before it named an agent: a v1 line holds that name, and a v2 line the
// selection the daemon's tree translated it to then, which is the tree's
// question, so such a line is not compared here.
func (r TaskLineChangeReceipt) lineMatches(line TaskLine) bool {
	switch {
	case r.Selection != nil:
		return line.Encoding == TaskLineEncodingV2 && line.Selection != nil && *line.Selection == *r.Selection
	case r.Agent != nil:
		return line.Encoding != TaskLineEncodingV1 || line.Agent == *r.Agent
	default:
		return false
	}
}

// RoutingPolicyChangeRequest is a set_routing_policy command as accepted,
// less its roles and drain-first account: the receipt's policy holds them,
// the roles in canonical form.
// ExpectedRevision is nil when the client saw no policy.
type RoutingPolicyChangeRequest struct {
	CommandIdentity
	ExpectedRevision *int `json:"expected_revision"`
}

// RoutingPolicyChangeReceipt records a routing policy change. Recorded is
// false when the policy already held the rule asked for; either way Policy
// is the revision current after the request and holds that rule.
type RoutingPolicyChangeReceipt struct {
	RoutingPolicyChangeRequest
	Recorded bool          `json:"recorded"`
	Policy   RoutingPolicy `json:"policy"`
}

// SameRequest reports whether a request with these canonical roles and this
// drain-first account is the one this receipt records.
func (r RoutingPolicyChangeReceipt) SameRequest(
	request RoutingPolicyChangeRequest, roles []RoutingPolicyRole, drainFirst *UsagePoolID,
) bool {
	sameExpected := (r.ExpectedRevision == nil) == (request.ExpectedRevision == nil) &&
		(r.ExpectedRevision == nil || *r.ExpectedRevision == *request.ExpectedRevision)
	return r.CommandIdentity == request.CommandIdentity && sameExpected && r.Policy.SameRule(roles, drainFirst)
}

func (r RoutingPolicyChangeReceipt) Validate() error {
	if err := r.validate(); err != nil {
		return fmt.Errorf("routing policy change receipt: %w", err)
	}
	if r.ExpectedRevision != nil && *r.ExpectedRevision < 1 {
		return fmt.Errorf("routing policy change receipt %q expected revision: %w", r.CommandID, ErrNonPositive)
	}
	if err := r.Policy.Validate(); err != nil {
		return fmt.Errorf("routing policy change receipt %q: %w", r.CommandID, err)
	}
	if r.Recorded && r.Policy.Source != RoutingPolicySourceClientSet {
		return fmt.Errorf("routing policy change receipt %q policy source %q: %w",
			r.CommandID, r.Policy.Source, ErrInvalidRoutingPolicySource)
	}
	// A change is accepted only against the current revision, so a recorded
	// one follows it and an unrecorded one is it.
	expected := 0
	if r.ExpectedRevision != nil {
		expected = *r.ExpectedRevision
	}
	if r.Recorded && r.Policy.Revision != expected+1 || !r.Recorded && r.Policy.Revision != expected {
		return fmt.Errorf("routing policy change receipt %q revision %d after %d: %w",
			r.CommandID, r.Policy.Revision, expected, ErrParentKeyMismatch)
	}
	return nil
}

// ParkedTaskMoveRequest is a move_parked_task command as accepted: the task
// parked on an exhausted quota window and the choice it moves to.
//
// Choice is what the operator sent since #2808, which the engine resolves
// against the lineup and the enrollment's catalog. Agent and Selection are
// the older encodings, kept only so their receipts still decode: a request
// recorded under #2735 holds the tree agent the wire named and the selection
// it reduced to, and one recorded before #2735 holds only the agent, which
// its record names. Exactly one of Choice and Agent is set.
type ParkedTaskMoveRequest struct {
	CommandIdentity
	TaskID    TaskID                `json:"task_id"`
	Choice    *AlternateAgentChoice `json:"choice,omitempty"`
	Agent     string                `json:"agent,omitempty"`
	Selection *TaskLineSelection    `json:"selection,omitempty"`
}

func (r ParkedTaskMoveRequest) validate() error {
	if err := r.CommandIdentity.validate(); err != nil {
		return err
	}
	if r.TaskID == "" {
		return fmt.Errorf("task: %w", ErrEmptyID)
	}
	if r.Choice != nil {
		if r.Agent != "" || r.Selection != nil {
			return fmt.Errorf("choice beside an agent: %w", ErrInvalidTaskLineSelection)
		}
		return r.Choice.Validate()
	}
	if !validAgentName(r.Agent) {
		return fmt.Errorf("agent: %w", ErrInvalidAgentName)
	}
	if r.Selection != nil {
		if err := r.Selection.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Same reports whether two requests are one: a replay of a command must
// carry the request its receipt records.
func (r ParkedTaskMoveRequest) Same(other ParkedTaskMoveRequest) bool {
	return r.CommandIdentity == other.CommandIdentity && r.TaskID == other.TaskID && r.Agent == other.Agent &&
		samePtr(r.Choice, other.Choice) && samePtr(r.Selection, other.Selection)
}

// samePtr reports whether two optional values are both absent or equal.
func samePtr[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// ParkedTaskMoveReceipt records a move: the card record it wrote and the
// task lines it set, each citing that record. The parked attempt's hold is
// released in the same transaction.
type ParkedTaskMoveReceipt struct {
	ParkedTaskMoveRequest
	Retry AlternateAgentRetry `json:"retry"`
	Lines []TaskLine          `json:"lines"`
}

func (r ParkedTaskMoveReceipt) Validate() error {
	if err := r.validate(); err != nil {
		return fmt.Errorf("parked task move receipt %q: %w", r.CommandID, err)
	}
	if err := r.Retry.Validate(); err != nil {
		return fmt.Errorf("parked task move receipt %q: %w", r.CommandID, err)
	}
	// A v2 record holds the selection the request's choice resolved to, or
	// under #2735 the selection its agent reduced to; a v1 record the agent
	// it named.
	chose := r.Retry.Agent == r.Agent && r.Selection == nil && r.Choice == nil
	switch {
	case r.Retry.Selection != nil && r.Choice != nil:
		chose = r.Choice.Chose(*r.Retry.Selection)
	case r.Retry.Selection != nil:
		chose = r.Selection != nil && *r.Selection == *r.Retry.Selection
	}
	if !r.Retry.ParkedMove() || r.Retry.CommandID != r.CommandID || r.Retry.TaskID != r.TaskID || !chose {
		return fmt.Errorf("parked task move receipt %q retry: %w", r.CommandID, ErrParentKeyMismatch)
	}
	// The parked role's line is always among them: under a v2 record it
	// holds the record's selection; under a v1 record it names the chosen
	// agent, or on a v2 line the selection the tree translated it to, which
	// admission holds to the record (anchorAlternateAgentMoveLine).
	moved := false
	for _, line := range r.Lines {
		if err := line.Validate(); err != nil {
			return fmt.Errorf("parked task move receipt %q line: %w", r.CommandID, err)
		}
		if line.TaskID != r.TaskID || line.Source != TaskLineSourceAlternateAgentMove ||
			line.SetBy != string(r.Retry.ID) {
			return fmt.Errorf("parked task move receipt %q line %s: %w", r.CommandID, line.ID, ErrParentKeyMismatch)
		}
		moved = moved || line.Role == r.Retry.Role && r.movesTo(line)
	}
	if !moved {
		return fmt.Errorf("parked task move receipt %q moves no %s line: %w",
			r.CommandID, r.Retry.Role, ErrParentKeyMismatch)
	}
	return nil
}

// movesTo reports whether line runs what the receipt's record chose.
func (r ParkedTaskMoveReceipt) movesTo(line TaskLine) bool {
	if r.Retry.Selection != nil {
		return line.Selection != nil && *line.Selection == *r.Retry.Selection
	}
	return line.Encoding == TaskLineEncodingV2 || line.Agent == r.Agent
}
