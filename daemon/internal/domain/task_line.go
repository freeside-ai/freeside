package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// TaskLineEncoding tags the canonical serialization TaskLine.ComputeID
// digests. A change to the encoding changes every line's identity, and an
// admission cites a line by that identity, so a new shape is a new encoding
// beside the old one and a stored line keeps the encoding it was written
// under.
type TaskLineEncoding string

const (
	// TaskLineEncodingV1 names the agent by its name in the project's tree.
	// Lines written before #2597 hold it; #2599 converts them.
	TaskLineEncodingV1 TaskLineEncoding = "freeside.task_line/v1"
	// TaskLineEncodingV2 names the enrollment, model, and effort
	// (TaskLineSelection). Every writer writes it.
	TaskLineEncodingV2 TaskLineEncoding = "freeside.task_line/v2"
)

// AllTaskLineEncodings lists every valid TaskLineEncoding; it is the single
// registration point.
var AllTaskLineEncodings = []TaskLineEncoding{TaskLineEncodingV1, TaskLineEncodingV2}

func (e TaskLineEncoding) valid() bool {
	switch e {
	case TaskLineEncodingV1, TaskLineEncodingV2:
		return true
	default:
		return false
	}
}

// TaskLineSelection is the enrollment, model, and effort a task line chooses
// for its role (plan §5.4): the operator-facing names an AgentSelection
// resolves from (AgentSelectionNames). Effort is the harness's effort string,
// as a selection holds it, not an EffortLevel. It checks shape only; whether
// the enrollment exists and serves the model at that effort is admission's
// question, asked at the admitting revision.
type TaskLineSelection struct {
	EnrollmentID ClientEnrollmentID `json:"enrollment_id"`
	ModelID      string             `json:"model_id"`
	Effort       string             `json:"effort"`
}

// Validate reports whether each name is present and one launch option value.
// Like ValidateTaskLineChoices, the error names the field and never the
// refused text, which an operator typed.
func (s TaskLineSelection) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"enrollment_id", string(s.EnrollmentID)}, {"model_id", s.ModelID}, {"effort", s.Effort},
	} {
		if !ValidLaunchOptionValue(field.value) || field.value == "" {
			return fmt.Errorf("task line selection %s: %w", field.name, ErrInvalidTaskLineSelection)
		}
	}
	return nil
}

// TaskLineSource is the path that set a task line (plan §5.4, Admitted
// Agents). Every member but the router and the triager is an authenticated
// operator action; the router chooses only among agents the committed lineup
// already approved, and the triager only the escalated counterpart the
// lineup names for the specifier. Intake from an issue, a label, or repository content never sets a
// line, because choosing an agent chooses which credential runs (§5.8). There
// is deliberately no intake member, so a line claiming one fails validation.
type TaskLineSource string

const (
	// TaskLineSourceSubmitTask is a paired client's submit_task command.
	TaskLineSourceSubmitTask TaskLineSource = "submit_task"
	// TaskLineSourceCLISubmit is `freesided submit` on the daemon host.
	TaskLineSourceCLISubmit TaskLineSource = "cli_submit"
	// TaskLineSourceProposalStart is the operator's Start or "start with
	// changes" decision on a task_proposal card, bound to the item version
	// and recorded at the decision's instant.
	TaskLineSourceProposalStart TaskLineSource = "proposal_start"
	// TaskLineSourceRouter is the routing policy's choice at a task's first
	// admission for a role with no operator line (plan §5.4, routing policy;
	// revision 103). It and TaskLineSourceAlternateAgentMove are the sources
	// that carry a TaskLineProvenance.
	TaskLineSourceRouter TaskLineSource = "router"
	// TaskLineSourceTriage is the triager's escalation of a task's specifier
	// to its escalated counterpart (plan §5.13, Specifier Triage; revision
	// 106). SetBy is the line's own task id: the record it cites is that
	// task's triage record, keyed by the task. It carries no provenance.
	TaskLineSourceTriage TaskLineSource = "triage"
	// TaskLineSourceCLILineChange is `freesided task-line` on the daemon host:
	// a change to a task's line after the task exists, while no attempt of
	// the role runs. SetBy names its TaskLineChange. It and
	// TaskLineSourceClientLineChange are the only sources that may clear a
	// line.
	TaskLineSourceCLILineChange TaskLineSource = "cli_line_change"
	// TaskLineSourceClientLineChange is a paired client's change_task_line
	// command: the same change as cli_line_change, made from a client, with
	// SetBy naming its TaskLineChange too.
	TaskLineSourceClientLineChange TaskLineSource = "client_line_change"
	// TaskLineSourceAlternateAgentMove is the operator's move of a task parked
	// on an exhausted quota window to another agent (plan §4, §5.4; revision
	// 103): the chosen agent takes the parked role, and every router-sourced
	// line moves to that agent's pool as the router would have written it
	// there. SetBy names the AlternateAgentRetry record, and the provenance's
	// passed_over names the agent left on the exhausted pool.
	TaskLineSourceAlternateAgentMove TaskLineSource = "alternate_agent_move"
)

// AllTaskLineSources lists every valid TaskLineSource; it is the single
// registration point.
var AllTaskLineSources = []TaskLineSource{
	TaskLineSourceSubmitTask, TaskLineSourceCLISubmit, TaskLineSourceProposalStart,
	TaskLineSourceRouter, TaskLineSourceCLILineChange, TaskLineSourceClientLineChange,
	TaskLineSourceTriage, TaskLineSourceAlternateAgentMove,
}

func (s TaskLineSource) valid() bool {
	switch s {
	case TaskLineSourceSubmitTask, TaskLineSourceCLISubmit, TaskLineSourceProposalStart,
		TaskLineSourceRouter, TaskLineSourceCLILineChange, TaskLineSourceClientLineChange,
		TaskLineSourceTriage, TaskLineSourceAlternateAgentMove:
		return true
	default:
		return false
	}
}

// CarriesProvenance reports whether a line of this source cites the pool
// choice that set it: the router's, and a move's from an exhausted pool.
func (s TaskLineSource) CarriesProvenance() bool {
	return s == TaskLineSourceRouter || s == TaskLineSourceAlternateAgentMove
}

// IsLineChange reports whether the source is a change to an existing task's
// line, whose SetBy names a TaskLineChange record.
func (s TaskLineSource) IsLineChange() bool {
	return s == TaskLineSourceCLILineChange || s == TaskLineSourceClientLineChange
}

// TaskLineSelectionTier is which of the router's tiers chose a routing's
// pool (plan §5.4, routing policy; revision 107). The router writes lines only
// at a task's first admission (revision 103), so the tier, not an event,
// explains the choice.
type TaskLineSelectionTier string

const (
	// TaskLineSelectionDrainFirstAccount is the policy's drain-first account,
	// which leads every other pool when the deciding role's order holds an
	// agent on it and its window is not marked.
	TaskLineSelectionDrainFirstAccount TaskLineSelectionTier = "drain_first_account"
	// TaskLineSelectionSoonestReset is the candidate pool whose known weekly
	// reset for its agent's model came soonest, among those not marked.
	TaskLineSelectionSoonestReset TaskLineSelectionTier = "soonest_reset"
	// TaskLineSelectionPolicyOrder is the first pool in the policy's order
	// among those with no known reset, when no pool with one was available.
	TaskLineSelectionPolicyOrder TaskLineSelectionTier = "policy_order"
)

// AllTaskLineSelectionTiers lists every valid TaskLineSelectionTier; it is
// the single registration point.
var AllTaskLineSelectionTiers = []TaskLineSelectionTier{
	TaskLineSelectionDrainFirstAccount, TaskLineSelectionSoonestReset, TaskLineSelectionPolicyOrder,
}

func (t TaskLineSelectionTier) valid() bool {
	switch t {
	case TaskLineSelectionDrainFirstAccount, TaskLineSelectionSoonestReset, TaskLineSelectionPolicyOrder:
		return true
	default:
		return false
	}
}

// TaskLineRankingReading is one candidate pool's known weekly reset for its
// agent's model, as the router ranked it: the window that resets soonest, the
// reset, and when the reading that reported it was observed.
type TaskLineRankingReading struct {
	Pool       UsagePoolID `json:"pool"`
	Window     string      `json:"window"`
	ResetsAt   time.Time   `json:"resets_at"`
	ObservedAt time.Time   `json:"observed_at"`
}

// TaskLinePassedOverMark is the exhausted mark that made the router pass over
// an agent ranked ahead of the chosen one: the agent, its pool, and the mark
// on that pool's window for the agent's model.
type TaskLinePassedOverMark struct {
	Agent    string              `json:"agent"`
	Pool     UsagePoolID         `json:"pool"`
	Window   string              `json:"window"`
	Source   ExhaustedMarkSource `json:"source"`
	MarkedAt time.Time           `json:"marked_at"`
	// ResetsAt is when the mark clears, nil when no reset is known.
	ResetsAt *time.Time `json:"resets_at"`
}

// TaskLineProvenance records how the router chose a routing's pool: the
// policy revision it applied, the tier that chose, the pool, the resets that
// ranked the candidates, and any exhausted mark it passed over (plan §5.4,
// routing policy). One routing writes one provenance onto every line it
// writes, so it names no role's agent of its own.
type TaskLineProvenance struct {
	// PolicyRevision is the routing policy revision applied, from 1.
	PolicyRevision int                   `json:"policy_revision"`
	Tier           TaskLineSelectionTier `json:"tier"`
	// Pool is the usage pool the routing chose.
	Pool UsagePoolID `json:"pool"`
	// RankedBy holds one reading per candidate pool whose weekly reset was
	// known and still ahead, in rank order: ascending ResetsAt, ties in the
	// policy's order. It is empty, never nil, when no reset was known.
	RankedBy []TaskLineRankingReading `json:"ranked_by"`
	// PassedOver is the first agent ranked ahead of the chosen pool whose
	// pool's window was marked exhausted; nil when none was.
	PassedOver *TaskLinePassedOverMark `json:"passed_over"`
}

// validate checks the provenance's consistency. It does not compare the
// passed-over agent with the line's agent: the provenance is shared by every
// role's line, and another role's line may name that agent when two agents
// on one pool run different models.
func (p TaskLineProvenance) validate() error {
	if p.PolicyRevision < 1 {
		return fmt.Errorf("policy_revision %d: %w", p.PolicyRevision, ErrNonPositive)
	}
	if !p.Tier.valid() {
		return fmt.Errorf("tier %q: %w", p.Tier, ErrInvalidTaskLineSelectionTier)
	}
	if err := validateProvenanceName("pool", string(p.Pool)); err != nil {
		return err
	}
	// ranked_by is part of the canonical encoding the line's id addresses,
	// so none is [] and never null: one value, one byte form.
	if p.RankedBy == nil {
		return fmt.Errorf("ranked_by is null: %w", ErrTaskLineProvenanceInconsistent)
	}
	for i, reading := range p.RankedBy {
		if err := reading.validate(); err != nil {
			return fmt.Errorf("ranked_by %d %w", i+1, err)
		}
		if i == 0 {
			continue
		}
		if slices.ContainsFunc(p.RankedBy[:i], func(r TaskLineRankingReading) bool { return r.Pool == reading.Pool }) {
			return fmt.Errorf("ranked_by %d repeats a pool: %w", i+1, ErrTaskLineProvenanceInconsistent)
		}
		if reading.ResetsAt.Before(p.RankedBy[i-1].ResetsAt) {
			return fmt.Errorf("ranked_by %d resets before %d: %w", i+1, i, ErrTaskLineProvenanceInconsistent)
		}
	}
	// The router takes the first ranked pool not marked, so a pool ranked
	// ahead of the chosen one was passed over for a mark. A marked pool keeps
	// its reading in ranked_by (its weekly reset is still known and ahead),
	// so the chosen pool leads ranked_by only when nothing was passed over.
	chosen := slices.IndexFunc(p.RankedBy, func(r TaskLineRankingReading) bool { return r.Pool == p.Pool })
	switch p.Tier {
	case TaskLineSelectionDrainFirstAccount:
		// The account leads every candidate, so nothing ranked ahead of it.
		if p.PassedOver != nil {
			return fmt.Errorf("tier %s passed_over: %w", p.Tier, ErrTaskLineProvenanceInconsistent)
		}
	case TaskLineSelectionSoonestReset:
		if chosen < 0 {
			return fmt.Errorf("tier %s chose a pool ranked_by lacks: %w", p.Tier, ErrTaskLineProvenanceInconsistent)
		}
		if chosen > 0 && p.PassedOver == nil {
			return fmt.Errorf("tier %s passed over ranked pools with no mark: %w",
				p.Tier, ErrTaskLineProvenanceInconsistent)
		}
	case TaskLineSelectionPolicyOrder:
		if chosen >= 0 {
			return fmt.Errorf("tier %s chose a pool with a known reset: %w", p.Tier, ErrTaskLineProvenanceInconsistent)
		}
		if len(p.RankedBy) > 0 && p.PassedOver == nil {
			return fmt.Errorf("tier %s passed over ranked pools with no mark: %w",
				p.Tier, ErrTaskLineProvenanceInconsistent)
		}
	}
	if p.PassedOver != nil {
		if err := p.PassedOver.validate(); err != nil {
			return fmt.Errorf("passed_over %w", err)
		}
	}
	return nil
}

func (r TaskLineRankingReading) validate() error {
	if err := validateProvenanceName("pool", string(r.Pool)); err != nil {
		return err
	}
	if err := validateProvenanceName("window", r.Window); err != nil {
		return err
	}
	if err := validateProvenanceInstant("resets_at", r.ResetsAt); err != nil {
		return err
	}
	return validateProvenanceInstant("observed_at", r.ObservedAt)
}

func (m TaskLinePassedOverMark) validate() error {
	if !validAgentName(m.Agent) {
		return fmt.Errorf("agent %q: %w", m.Agent, ErrInvalidAgentName)
	}
	if err := validateProvenanceName("pool", string(m.Pool)); err != nil {
		return err
	}
	if err := validateProvenanceName("window", m.Window); err != nil {
		return err
	}
	if !m.Source.valid() {
		return fmt.Errorf("source %q: %w", m.Source, ErrInvalidExhaustedMarkSource)
	}
	if err := validateProvenanceInstant("marked_at", m.MarkedAt); err != nil {
		return err
	}
	if m.ResetsAt != nil {
		return validateProvenanceInstant("resets_at", *m.ResetsAt)
	}
	return nil
}

func validateProvenanceName(field, name string) error {
	if name == "" {
		return fmt.Errorf("%s: %w", field, ErrEmptyField)
	}
	// JSON encoding replaces invalid UTF-8, so a name holding it would be
	// digested in one form and decoded in another: the stored line could
	// never read back.
	if !utf8.ValidString(name) {
		return fmt.Errorf("%s: %w", field, ErrTaskLineReadingNotUTF8)
	}
	return nil
}

// validateProvenanceInstant requires a set UTC instant: the provenance is part
// of the canonical encoding the line's id addresses, so one instant must have
// one byte form.
func validateProvenanceInstant(field string, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("%s: %w", field, ErrMissingTimestamp)
	}
	if at.Location() != time.UTC {
		return fmt.Errorf("%s: %w", field, ErrTimestampNotUTC)
	}
	return nil
}

// clone returns a deep copy in canonical byte form, the one the line's id
// digests: every instant in UTC and no ranking as [], not null.
func (p *TaskLineProvenance) clone() *TaskLineProvenance {
	if p == nil {
		return nil
	}
	out := *p
	out.RankedBy = make([]TaskLineRankingReading, len(p.RankedBy))
	for i, reading := range p.RankedBy {
		reading.ResetsAt = reading.ResetsAt.UTC()
		reading.ObservedAt = reading.ObservedAt.UTC()
		out.RankedBy[i] = reading
	}
	if p.PassedOver != nil {
		mark := *p.PassedOver
		mark.MarkedAt = mark.MarkedAt.UTC()
		if mark.ResetsAt != nil {
			mark.ResetsAt = new(mark.ResetsAt.UTC())
		}
		out.PassedOver = &mark
	}
	return &out
}

// TaskLineRoles lists the roles a task line may choose an agent for, in the
// order the API schema's TaskLineRole enum lists them.
var TaskLineRoles = []RoleName{RoleSpecifier, RoleImplementer, RoleRemediator, RoleReviewer}

// TaskLineEligible reports whether a task line may choose the role's agent.
// The shadow reviewer and every wardless role stay on the lineup, because
// their comparisons key on the lineup line (§5.13) and a per-task agent
// would split them. The switch omits default so a new role's author decides.
func (r RoleName) TaskLineEligible() bool {
	switch r {
	case RoleSpecifier, RoleImplementer, RoleRemediator, RoleReviewer:
		return true
	case RoleShadowReviewer, RoleDiagnostic, RoleTaskNamer, RolePublicationAuthor,
		RoleFindingClassifier, RoleFindingAdjudicator, RoleDriftAuditor,
		RoleAttentionDiscussion, RoleBriefer, RoleTriager:
		return false
	}
	return false
}

// TaskLineChoice is one role's requested agent as an operator submits it:
// the caller-supplied half of a line, before any task exists to hold it. It
// holds exactly one of Agent and Selection. Every writer since #2597 records
// a Selection: a client command and `freesided submit` both carry the tuple
// (#2598). Agent remains for records written before then. It is omitted
// when empty, so a name choice keeps the byte form every recorded command
// and submission digested before Selection existed.
type TaskLineChoice struct {
	Role      RoleName           `json:"role"`
	Agent     string             `json:"agent,omitempty"`
	Selection *TaskLineSelection `json:"selection,omitempty"`
}

// ValidateTaskLineChoices reports whether the choices are a well-formed set:
// each role eligible and named at most once, each choosing a well-formed
// agent name or a well-formed selection, never both. It checks shape only.
// Whether the agent or the selection resolves in the project's tree is
// admission's question, asked at the admitting revision.
//
// The error names the choice by its position and by its role once the role
// is known to be eligible. It never carries text that failed a check: a
// refused role or agent is request text, and a credential pasted into either
// must not come back in a response or a log line.
func ValidateTaskLineChoices(choices []TaskLineChoice) error {
	seen := make(map[RoleName]bool, len(choices))
	for i, choice := range choices {
		if !choice.Role.TaskLineEligible() {
			return fmt.Errorf("task line %d role: %w", i+1, ErrTaskLineRoleIneligible)
		}
		if seen[choice.Role] {
			return fmt.Errorf("task line %d role %s: %w", i+1, choice.Role, ErrDuplicateTaskLineRole)
		}
		seen[choice.Role] = true
		if choice.Selection != nil {
			if choice.Agent != "" {
				return fmt.Errorf("task line %d (%s) names an agent and a selection: %w",
					i+1, choice.Role, ErrInvalidTaskLineSelection)
			}
			if err := choice.Selection.Validate(); err != nil {
				return fmt.Errorf("task line %d (%s): %w", i+1, choice.Role, err)
			}
		} else if !validAgentName(choice.Agent) {
			return fmt.Errorf("task line %d (%s) agent: %w", i+1, choice.Role, ErrInvalidAgentName)
		}
	}
	return nil
}

// CanonicalTaskLineChoices validates the choices and returns them in
// TaskLineRoles order. A submission's replay identity digests this form, so
// the set of lines is the identity and the order they were sent in is not.
// No choices canonicalize to nil.
func CanonicalTaskLineChoices(choices []TaskLineChoice) ([]TaskLineChoice, error) {
	if err := ValidateTaskLineChoices(choices); err != nil {
		return nil, err
	}
	if len(choices) == 0 {
		return nil, nil
	}
	byRole := make(map[RoleName]TaskLineChoice, len(choices))
	for _, choice := range choices {
		choice.Selection = clonePtr(choice.Selection)
		byRole[choice.Role] = choice
	}
	canonical := make([]TaskLineChoice, 0, len(choices))
	for _, role := range TaskLineRoles {
		if choice, ok := byRole[role]; ok {
			canonical = append(canonical, choice)
		}
	}
	return canonical, nil
}

// ValidateRecordedTaskLines reports whether a submission record's lines are
// the form a writer stores: absent, or a non-empty set in canonical order. An
// out-of-order list is refused rather than reordered, because no writer
// stores one. Like ValidateTaskLineChoices, the error carries no refused text.
func ValidateRecordedTaskLines(lines []TaskLineChoice) error {
	if lines == nil {
		return nil
	}
	canonical, err := CanonicalTaskLineChoices(lines)
	if err != nil {
		return err
	}
	if len(lines) == 0 || !slices.EqualFunc(lines, canonical, TaskLineChoice.Same) {
		return ErrTaskLinesNotCanonical
	}
	return nil
}

// Same reports whether two choices choose the same thing for the same role:
// the same name, or equal selections.
func (c TaskLineChoice) Same(other TaskLineChoice) bool {
	if (c.Selection == nil) != (other.Selection == nil) {
		return false
	}
	return c.Role == other.Role && c.Agent == other.Agent && (c.Selection == nil || *c.Selection == *other.Selection)
}

// Choice is the role and the agent or selection the line holds, as a
// submission records them. A cleared line's choice holds neither.
func (l TaskLine) Choice() TaskLineChoice {
	return TaskLineChoice{Role: l.Role, Agent: l.Agent, Selection: l.Selection}
}

// TaskLineInput carries the caller-supplied fields of a TaskLine. It has no
// ID, Version, or PredecessorID: the store assigns the version and the
// predecessor inside the write transaction and the identity is a content
// address, so no input path can set them.
//
// The input sets the line's encoding: Agent writes a v1 line, which only
// records from before #2597 and their test fixtures hold; Selection, or a
// clear, writes v2. Every writer in the daemon passes Selection.
type TaskLineInput struct {
	TaskID    TaskID
	Role      RoleName
	Agent     string
	Selection *TaskLineSelection
	Source    TaskLineSource
	// SetBy is the command or submission identity that set the line.
	SetBy string
	// Provenance is set exactly for a router or alternate-agent-move line.
	Provenance *TaskLineProvenance
	// Cleared records a version that names no agent; Agent and Selection
	// stay empty.
	Cleared bool
}

// TaskLine is one version of a task's line for one role (plan §5.4, Admitted
// Agents): the operator's choice of agent for that role in that task. A
// change is a new version that names the one it supersedes; a version is
// never edited, so the line an admission cites stays the choice that
// authorized that attempt. It selects an agent only: the role's prompt stays
// the one its lineup line names.
//
// Its JSON form depends on the encoding (MarshalJSON): a v1 line keeps the
// form it was stored and synced under, and a v2 line names its encoding.
type TaskLine struct {
	// ID is the content address of every other field.
	ID Digest
	// Encoding is the canonical serialization ID addresses.
	Encoding TaskLineEncoding
	TaskID   TaskID
	Role     RoleName
	// Agent is a v1 line's agent name in the project's tree, empty when
	// Cleared and on every v2 line. It is not resolved here: admission
	// resolves it at the admitting revision.
	Agent string
	// Selection is a v2 line's enrollment, model, and effort, nil when
	// Cleared and on every v1 line. Admission resolves it at the admitting
	// revision.
	Selection *TaskLineSelection
	// Version counts the role's lines in this task from 1.
	Version int
	// PredecessorID is the line this version supersedes: nil for version 1
	// and set for every later one.
	PredecessorID *Digest
	Source        TaskLineSource
	SetBy         string
	SetAt         time.Time
	// Provenance is how the router, or a move from an exhausted pool, chose
	// the agent: set exactly when Source is router or alternate_agent_move
	// (CarriesProvenance), nil for every other source.
	Provenance *TaskLineProvenance
	// Cleared is a version that takes the role back to the lineup. Its JSON
	// is omitted when false because the API's TaskLine has no such field: the
	// synced task carries only each role's current line, and a cleared role
	// has none.
	Cleared bool
}

// NewTaskLine builds a validated line version in canonical byte-form: the
// predecessor and provenance are detached, the timestamps are normalized to
// UTC, and the ID is computed last from the bound facts.
func NewTaskLine(in TaskLineInput, version int, predecessor *Digest, setAt time.Time) (TaskLine, error) {
	encoding := TaskLineEncodingV2
	if in.Agent != "" {
		encoding = TaskLineEncodingV1
	}
	line := TaskLine{
		Encoding:      encoding,
		TaskID:        in.TaskID,
		Role:          in.Role,
		Agent:         in.Agent,
		Selection:     clonePtr(in.Selection),
		Version:       version,
		PredecessorID: clonePtr(predecessor),
		Source:        in.Source,
		SetBy:         in.SetBy,
		SetAt:         setAt.UTC(),
		Provenance:    in.Provenance.clone(),
		Cleared:       in.Cleared,
	}
	id, err := line.ComputeID()
	if err != nil {
		return TaskLine{}, err
	}
	line.ID = id
	if err := line.Validate(); err != nil {
		return TaskLine{}, err
	}
	return line, nil
}

// canonicalTaskLineV1 is the v1 serialization ComputeID digests: every bound
// fact and nothing derived (ID is this value).
type canonicalTaskLineV1 struct {
	Version       TaskLineEncoding `json:"version"`
	TaskID        TaskID           `json:"task_id"`
	Role          RoleName         `json:"role"`
	Agent         string           `json:"agent"`
	LineVersion   int              `json:"line_version"`
	PredecessorID *Digest          `json:"predecessor_id"`
	Source        TaskLineSource   `json:"source"`
	SetBy         string           `json:"set_by"`
	SetAt         time.Time        `json:"set_at"`
	// Provenance is omitted when nil, so an operator line keeps the v1 byte
	// form and the id it was stored under before router lines existed.
	Provenance *TaskLineProvenance `json:"provenance,omitempty"`
	// Cleared is omitted when false, so every line that isn't a clear keeps
	// its id.
	Cleared bool `json:"cleared,omitempty"`
}

// canonicalTaskLineV2 is the v2 serialization: the selection in place of the
// agent, null on a clear.
type canonicalTaskLineV2 struct {
	Version       TaskLineEncoding    `json:"version"`
	TaskID        TaskID              `json:"task_id"`
	Role          RoleName            `json:"role"`
	Selection     *TaskLineSelection  `json:"selection"`
	LineVersion   int                 `json:"line_version"`
	PredecessorID *Digest             `json:"predecessor_id"`
	Source        TaskLineSource      `json:"source"`
	SetBy         string              `json:"set_by"`
	SetAt         time.Time           `json:"set_at"`
	Provenance    *TaskLineProvenance `json:"provenance"`
	Cleared       bool                `json:"cleared"`
}

// ComputeID returns the content address of the line: a sha256 over the
// canonical serialization of the encoding the line declares.
func (l TaskLine) ComputeID() (Digest, error) {
	var canonical any
	switch l.Encoding {
	case TaskLineEncodingV1:
		canonical = canonicalTaskLineV1{
			Version: l.Encoding, TaskID: l.TaskID, Role: l.Role, Agent: l.Agent,
			LineVersion: l.Version, PredecessorID: l.PredecessorID, Source: l.Source,
			SetBy: l.SetBy, SetAt: l.SetAt, Provenance: l.Provenance, Cleared: l.Cleared,
		}
	case TaskLineEncodingV2:
		canonical = canonicalTaskLineV2{
			Version: l.Encoding, TaskID: l.TaskID, Role: l.Role, Selection: l.Selection,
			LineVersion: l.Version, PredecessorID: l.PredecessorID, Source: l.Source,
			SetBy: l.SetBy, SetAt: l.SetAt, Provenance: l.Provenance, Cleared: l.Cleared,
		}
	default:
		return "", fmt.Errorf("task line id: encoding %q: %w", l.Encoding, ErrInvalidTaskLineEncoding)
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("task line id: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the line is well-formed and its identity
// authentic. The store re-runs it on every decode, so a row edited in place
// resolves to a different content address and fails here.
func (l TaskLine) Validate() error {
	if l.TaskID == "" {
		return fmt.Errorf("task line task_id: %w", ErrEmptyID)
	}
	if !l.Role.TaskLineEligible() {
		return fmt.Errorf("task line %s role %q: %w", l.TaskID, l.Role, ErrTaskLineRoleIneligible)
	}
	if !l.Encoding.valid() {
		return fmt.Errorf("task line %s %s encoding %q: %w", l.TaskID, l.Role, l.Encoding, ErrInvalidTaskLineEncoding)
	}
	// A v1 line names an agent and a v2 line selects one; neither holds the
	// other's field.
	if (l.Encoding == TaskLineEncodingV1 && l.Selection != nil) || (l.Encoding == TaskLineEncodingV2 && l.Agent != "") {
		return fmt.Errorf("task line %s %s %s: %w", l.TaskID, l.Role, l.Encoding, ErrInvalidTaskLineEncoding)
	}
	switch {
	case l.Cleared:
		// A clear supersedes a line, so it never starts a chain.
		if l.Agent != "" || l.Selection != nil || !l.Source.IsLineChange() || l.Version == 1 {
			return fmt.Errorf("task line %s %s version %d clear: %w", l.TaskID, l.Role, l.Version, ErrTaskLineInconsistent)
		}
	case l.Encoding == TaskLineEncodingV2:
		if l.Selection == nil {
			return fmt.Errorf("task line %s %s selects nothing: %w", l.TaskID, l.Role, ErrInvalidTaskLineSelection)
		}
		if err := l.Selection.Validate(); err != nil {
			return fmt.Errorf("task line %s %s: %w", l.TaskID, l.Role, err)
		}
	case !validAgentName(l.Agent):
		return fmt.Errorf("task line %s %s agent %q: %w", l.TaskID, l.Role, l.Agent, ErrInvalidAgentName)
	}
	if l.Version < 1 {
		return fmt.Errorf("task line %s %s version %d: %w", l.TaskID, l.Role, l.Version, ErrNonPositive)
	}
	if (l.Version == 1) != (l.PredecessorID == nil) {
		return fmt.Errorf("task line %s %s version %d predecessor: %w",
			l.TaskID, l.Role, l.Version, ErrTaskLineInconsistent)
	}
	if l.PredecessorID != nil && !contentaddr.Valid(string(*l.PredecessorID)) {
		return fmt.Errorf("task line %s %s predecessor_id %q: %w",
			l.TaskID, l.Role, *l.PredecessorID, ErrInvalidDigest)
	}
	if !l.Source.valid() {
		return fmt.Errorf("task line %s %s source %q: %w", l.TaskID, l.Role, l.Source, ErrInvalidTaskLineSource)
	}
	if l.SetBy == "" {
		return fmt.Errorf("task line %s %s set_by: %w", l.TaskID, l.Role, ErrEmptyField)
	}
	if (l.Provenance != nil) != l.Source.CarriesProvenance() {
		return fmt.Errorf("task line %s %s source %s provenance: %w",
			l.TaskID, l.Role, l.Source, ErrTaskLineProvenanceInconsistent)
	}
	if l.Provenance != nil {
		if err := l.Provenance.validate(); err != nil {
			return fmt.Errorf("task line %s %s provenance %w", l.TaskID, l.Role, err)
		}
	}
	if l.SetAt.IsZero() {
		return fmt.Errorf("task line %s %s set_at: %w", l.TaskID, l.Role, ErrMissingTimestamp)
	}
	// set_at is part of the canonical encoding the id addresses, so one
	// instant must have one byte form.
	if l.SetAt.Location() != time.UTC {
		return fmt.Errorf("task line %s %s set_at: %w", l.TaskID, l.Role, ErrTimestampNotUTC)
	}
	if l.ID == "" {
		return fmt.Errorf("task line %s %s id: %w", l.TaskID, l.Role, ErrEmptyID)
	}
	computed, err := l.ComputeID()
	if err != nil {
		return err
	}
	if l.ID != computed {
		return fmt.Errorf("task line %s, content resolves to %s: %w", l.ID, computed, ErrTaskLineInconsistent)
	}
	return nil
}

// taskLineV1JSON is a v1 line's JSON form, unchanged since before v2: the
// form change receipts stored, so a stored receipt still decodes and a v1
// fixture keeps its bytes.
type taskLineV1JSON struct {
	ID            Digest              `json:"id"`
	TaskID        TaskID              `json:"task_id"`
	Role          RoleName            `json:"role"`
	Agent         string              `json:"agent"`
	Version       int                 `json:"version"`
	PredecessorID *Digest             `json:"predecessor_id"`
	Source        TaskLineSource      `json:"source"`
	SetBy         string              `json:"set_by"`
	SetAt         time.Time           `json:"set_at"`
	Provenance    *TaskLineProvenance `json:"provenance"`
	Cleared       bool                `json:"cleared,omitempty"`
}

// taskLineV2JSON is a v2 line's JSON form: it names its encoding, which a v1
// form never carries, and the selection in place of the agent.
type taskLineV2JSON struct {
	ID            Digest              `json:"id"`
	Encoding      TaskLineEncoding    `json:"encoding"`
	TaskID        TaskID              `json:"task_id"`
	Role          RoleName            `json:"role"`
	Selection     *TaskLineSelection  `json:"selection"`
	Version       int                 `json:"version"`
	PredecessorID *Digest             `json:"predecessor_id"`
	Source        TaskLineSource      `json:"source"`
	SetBy         string              `json:"set_by"`
	SetAt         time.Time           `json:"set_at"`
	Provenance    *TaskLineProvenance `json:"provenance"`
	Cleared       bool                `json:"cleared,omitempty"`
}

// MarshalJSON writes the form of the line's encoding.
func (l TaskLine) MarshalJSON() ([]byte, error) {
	switch l.Encoding {
	case TaskLineEncodingV1:
		return json.Marshal(taskLineV1JSON{
			ID: l.ID, TaskID: l.TaskID, Role: l.Role, Agent: l.Agent, Version: l.Version,
			PredecessorID: l.PredecessorID, Source: l.Source, SetBy: l.SetBy, SetAt: l.SetAt,
			Provenance: l.Provenance, Cleared: l.Cleared,
		})
	case TaskLineEncodingV2:
		return json.Marshal(taskLineV2JSON{
			ID: l.ID, Encoding: l.Encoding, TaskID: l.TaskID, Role: l.Role, Selection: l.Selection,
			Version: l.Version, PredecessorID: l.PredecessorID, Source: l.Source, SetBy: l.SetBy,
			SetAt: l.SetAt, Provenance: l.Provenance, Cleared: l.Cleared,
		})
	}
	return nil, fmt.Errorf("task line %s encoding %q: %w", l.ID, l.Encoding, ErrInvalidTaskLineEncoding)
}

// UnmarshalJSON reads either form: one with no encoding is v1, the form
// written before v2 existed. It decodes only; the caller validates.
func (l *TaskLine) UnmarshalJSON(data []byte) error {
	var probe struct {
		Encoding *TaskLineEncoding `json:"encoding"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if probe.Encoding == nil {
		var v1 taskLineV1JSON
		if err := json.Unmarshal(data, &v1); err != nil {
			return err
		}
		*l = TaskLine{
			ID: v1.ID, Encoding: TaskLineEncodingV1, TaskID: v1.TaskID, Role: v1.Role, Agent: v1.Agent,
			Version: v1.Version, PredecessorID: v1.PredecessorID, Source: v1.Source, SetBy: v1.SetBy,
			SetAt: v1.SetAt, Provenance: v1.Provenance, Cleared: v1.Cleared,
		}
		return nil
	}
	if *probe.Encoding != TaskLineEncodingV2 {
		return fmt.Errorf("task line encoding %q: %w", *probe.Encoding, ErrInvalidTaskLineEncoding)
	}
	var v2 taskLineV2JSON
	if err := json.Unmarshal(data, &v2); err != nil {
		return err
	}
	*l = TaskLine{
		ID: v2.ID, Encoding: v2.Encoding, TaskID: v2.TaskID, Role: v2.Role, Selection: v2.Selection,
		Version: v2.Version, PredecessorID: v2.PredecessorID, Source: v2.Source, SetBy: v2.SetBy,
		SetAt: v2.SetAt, Provenance: v2.Provenance, Cleared: v2.Cleared,
	}
	return nil
}
