package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// taskLineEncodingVersion tags the canonical serialization
// TaskLine.ComputeID digests. A change to the encoding changes every line's
// identity, and an admission cites a line by that identity.
const taskLineEncodingVersion = "freeside.task_line/v1"

// TaskLineSource is the path that set a task line (plan §5.4, Admitted
// Agents). Every member but the router is an authenticated operator action,
// and the router chooses only among agents the committed lineup already
// approved. Intake from an issue, a label, or repository content never sets a
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
	// revision 103). It is the only source that carries a TaskLineProvenance.
	TaskLineSourceRouter TaskLineSource = "router"
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
)

// AllTaskLineSources lists every valid TaskLineSource; it is the single
// registration point.
var AllTaskLineSources = []TaskLineSource{
	TaskLineSourceSubmitTask, TaskLineSourceCLISubmit, TaskLineSourceProposalStart,
	TaskLineSourceRouter, TaskLineSourceCLILineChange, TaskLineSourceClientLineChange,
}

func (s TaskLineSource) valid() bool {
	switch s {
	case TaskLineSourceSubmitTask, TaskLineSourceCLISubmit, TaskLineSourceProposalStart,
		TaskLineSourceRouter, TaskLineSourceCLILineChange, TaskLineSourceClientLineChange:
		return true
	default:
		return false
	}
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
		RoleAttentionDiscussion, RoleBriefer:
		return false
	}
	return false
}

// TaskLineChoice is one role's requested agent as an operator submits it:
// the caller-supplied half of a line, before any task exists to hold it.
type TaskLineChoice struct {
	Role  RoleName `json:"role"`
	Agent string   `json:"agent"`
}

// ValidateTaskLineChoices reports whether the choices are a well-formed set:
// each role eligible and named at most once, each agent a well-formed agent
// name. It checks shape only. Whether the agent resolves in the project's
// tree is admission's question, asked at the admitting revision.
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
		if !validAgentName(choice.Agent) {
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
	if len(lines) == 0 || !slices.Equal(lines, canonical) {
		return ErrTaskLinesNotCanonical
	}
	return nil
}

// TaskLineInput carries the caller-supplied fields of a TaskLine. It has no
// ID, Version, or PredecessorID: the store assigns the version and the
// predecessor inside the write transaction and the identity is a content
// address, so no input path can set them.
type TaskLineInput struct {
	TaskID TaskID
	Role   RoleName
	Agent  string
	Source TaskLineSource
	// SetBy is the command or submission identity that set the line.
	SetBy string
	// Provenance is set exactly for a router line.
	Provenance *TaskLineProvenance
	// Cleared records a version that names no agent; Agent stays empty.
	Cleared bool
}

// TaskLine is one version of a task's line for one role (plan §5.4, Admitted
// Agents): the operator's choice of agent for that role in that task. A
// change is a new version that names the one it supersedes; a version is
// never edited, so the line an admission cites stays the choice that
// authorized that attempt. It selects an agent only: the role's prompt stays
// the one its lineup line names.
type TaskLine struct {
	// ID is the content address of every other field.
	ID     Digest   `json:"id"`
	TaskID TaskID   `json:"task_id"`
	Role   RoleName `json:"role"`
	// Agent is the agent's name in the project's tree, empty exactly when
	// Cleared. It is not resolved here: admission resolves it at the
	// admitting revision.
	Agent string `json:"agent"`
	// Version counts the role's lines in this task from 1.
	Version int `json:"version"`
	// PredecessorID is the line this version supersedes: nil for version 1
	// and set for every later one.
	PredecessorID *Digest        `json:"predecessor_id"`
	Source        TaskLineSource `json:"source"`
	SetBy         string         `json:"set_by"`
	SetAt         time.Time      `json:"set_at"`
	// Provenance is how the router chose the agent: set exactly when Source
	// is router, nil for every operator source.
	Provenance *TaskLineProvenance `json:"provenance"`
	// Cleared is a version that takes the role back to the lineup. It is
	// omitted when false because the API's TaskLine has no such field: the
	// synced task carries only each role's current line, and a cleared role
	// has none.
	Cleared bool `json:"cleared,omitempty"`
}

// NewTaskLine builds a validated line version in canonical byte-form: the
// predecessor and provenance are detached, the timestamps are normalized to
// UTC, and the ID is computed last from the bound facts.
func NewTaskLine(in TaskLineInput, version int, predecessor *Digest, setAt time.Time) (TaskLine, error) {
	line := TaskLine{
		TaskID:        in.TaskID,
		Role:          in.Role,
		Agent:         in.Agent,
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

// canonicalTaskLine is the versioned serialization ComputeID digests: every
// bound fact and nothing derived (ID is this value).
type canonicalTaskLine struct {
	Version       string         `json:"version"`
	TaskID        TaskID         `json:"task_id"`
	Role          RoleName       `json:"role"`
	Agent         string         `json:"agent"`
	LineVersion   int            `json:"line_version"`
	PredecessorID *Digest        `json:"predecessor_id"`
	Source        TaskLineSource `json:"source"`
	SetBy         string         `json:"set_by"`
	SetAt         time.Time      `json:"set_at"`
	// Provenance is omitted when nil, so an operator line keeps the v1 byte
	// form and the id it was stored under before router lines existed.
	Provenance *TaskLineProvenance `json:"provenance,omitempty"`
	// Cleared is omitted when false, so every line that isn't a clear keeps
	// its id.
	Cleared bool `json:"cleared,omitempty"`
}

// ComputeID returns the content address of the line: a sha256 over its
// versioned canonical serialization.
func (l TaskLine) ComputeID() (Digest, error) {
	body, err := json.Marshal(canonicalTaskLine{
		Version:       taskLineEncodingVersion,
		TaskID:        l.TaskID,
		Role:          l.Role,
		Agent:         l.Agent,
		LineVersion:   l.Version,
		PredecessorID: l.PredecessorID,
		Source:        l.Source,
		SetBy:         l.SetBy,
		SetAt:         l.SetAt,
		Provenance:    l.Provenance,
		Cleared:       l.Cleared,
	})
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
	if l.Cleared {
		// A clear supersedes a line, so it never starts a chain.
		if l.Agent != "" || !l.Source.IsLineChange() || l.Version == 1 {
			return fmt.Errorf("task line %s %s version %d clear: %w", l.TaskID, l.Role, l.Version, ErrTaskLineInconsistent)
		}
	} else if !validAgentName(l.Agent) {
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
	if (l.Provenance != nil) != (l.Source == TaskLineSourceRouter) {
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
