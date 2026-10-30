package domain

import (
	"encoding/json"
	"fmt"
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
)

// AllTaskLineSources lists every valid TaskLineSource; it is the single
// registration point.
var AllTaskLineSources = []TaskLineSource{
	TaskLineSourceSubmitTask, TaskLineSourceCLISubmit, TaskLineSourceProposalStart,
	TaskLineSourceRouter,
}

func (s TaskLineSource) valid() bool {
	switch s {
	case TaskLineSourceSubmitTask, TaskLineSourceCLISubmit, TaskLineSourceProposalStart,
		TaskLineSourceRouter:
		return true
	default:
		return false
	}
}

// TaskLineSelectionTrigger is what made the router write a line (plan §5.4,
// routing policy). There is no quota-refusal member: under revision 103 the
// router writes lines only at a task's first admission, and a quota refusal
// parks the task instead of rerouting it.
type TaskLineSelectionTrigger string

const (
	// TaskLineSelectionFirstAdmission is a first admission with no window
	// marked exhausted ahead of the chosen agent: the policy order's first
	// agent took the task, so no reading was acted on and none passed over.
	TaskLineSelectionFirstAdmission TaskLineSelectionTrigger = "first_admission"
	// TaskLineSelectionExhaustedMark is a first admission that passed over an
	// agent whose pool's window was marked exhausted.
	TaskLineSelectionExhaustedMark TaskLineSelectionTrigger = "exhausted_mark"
)

// AllTaskLineSelectionTriggers lists every valid TaskLineSelectionTrigger; it
// is the single registration point.
var AllTaskLineSelectionTriggers = []TaskLineSelectionTrigger{
	TaskLineSelectionFirstAdmission, TaskLineSelectionExhaustedMark,
}

func (t TaskLineSelectionTrigger) valid() bool {
	switch t {
	case TaskLineSelectionFirstAdmission, TaskLineSelectionExhaustedMark:
		return true
	default:
		return false
	}
}

// TaskLineSelectionReading cites the usage mark the router acted on. The
// usage observation record does not exist yet (#1715), so the citation is an
// opaque reference: the pool and window by name and the observation's
// instant.
type TaskLineSelectionReading struct {
	Pool       string    `json:"pool"`
	Window     string    `json:"window"`
	ObservedAt time.Time `json:"observed_at"`
}

// TaskLineProvenance records how the router came to write a line: the policy
// revision it applied, what triggered the selection, the reading it acted on,
// and the agent it passed over (plan §5.4, routing policy).
type TaskLineProvenance struct {
	// PolicyRevision is the routing policy revision applied, from 1.
	PolicyRevision int                      `json:"policy_revision"`
	Trigger        TaskLineSelectionTrigger `json:"trigger"`
	// Reading is the exhausted mark acted on: set exactly when Trigger is
	// exhausted_mark.
	Reading *TaskLineSelectionReading `json:"reading"`
	// PassedOver is the agent the policy order put ahead of the chosen one:
	// set exactly when Trigger is exhausted_mark.
	PassedOver *string `json:"passed_over"`
}

// validate checks the provenance against the line's agent, which it may
// not pass over.
func (p TaskLineProvenance) validate(agent string) error {
	if p.PolicyRevision < 1 {
		return fmt.Errorf("policy_revision %d: %w", p.PolicyRevision, ErrNonPositive)
	}
	if !p.Trigger.valid() {
		return fmt.Errorf("trigger %q: %w", p.Trigger, ErrInvalidTaskLineSelectionTrigger)
	}
	acted := p.Trigger == TaskLineSelectionExhaustedMark
	if (p.Reading != nil) != acted || (p.PassedOver != nil) != acted {
		return fmt.Errorf("trigger %s reading and passed_over: %w", p.Trigger, ErrTaskLineProvenanceInconsistent)
	}
	if p.Reading != nil {
		switch {
		case p.Reading.Pool == "":
			return fmt.Errorf("reading pool: %w", ErrEmptyField)
		case p.Reading.Window == "":
			return fmt.Errorf("reading window: %w", ErrEmptyField)
		// JSON encoding replaces invalid UTF-8, so a name holding it would
		// be digested in one form and decoded in another: the stored line
		// could never read back.
		case !utf8.ValidString(p.Reading.Pool) || !utf8.ValidString(p.Reading.Window):
			return fmt.Errorf("reading pool or window: %w", ErrTaskLineReadingNotUTF8)
		case p.Reading.ObservedAt.IsZero():
			return fmt.Errorf("reading observed_at: %w", ErrMissingTimestamp)
		// observed_at is part of the canonical encoding the line's id
		// addresses, so one instant must have one byte form.
		case p.Reading.ObservedAt.Location() != time.UTC:
			return fmt.Errorf("reading observed_at: %w", ErrTimestampNotUTC)
		}
	}
	if p.PassedOver != nil {
		if !validAgentName(*p.PassedOver) {
			return fmt.Errorf("passed_over %q: %w", *p.PassedOver, ErrInvalidAgentName)
		}
		if *p.PassedOver == agent {
			return fmt.Errorf("passed_over is the chosen agent: %w", ErrTaskLineProvenanceInconsistent)
		}
	}
	return nil
}

// clone returns a deep copy with the reading's instant in UTC, the canonical
// byte form the line's id digests.
func (p *TaskLineProvenance) clone() *TaskLineProvenance {
	if p == nil {
		return nil
	}
	out := *p
	if p.Reading != nil {
		reading := *p.Reading
		reading.ObservedAt = reading.ObservedAt.UTC()
		out.Reading = &reading
	}
	out.PassedOver = clonePtr(p.PassedOver)
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
	// Agent is the agent's name in the project's tree. It is not resolved
	// here: admission resolves it at the admitting revision.
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
	if !validAgentName(l.Agent) {
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
		if err := l.Provenance.validate(l.Agent); err != nil {
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
