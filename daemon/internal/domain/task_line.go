package domain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// taskLineEncodingVersion tags the canonical serialization
// TaskLine.ComputeID digests. A change to the encoding changes every line's
// identity, and an admission cites a line by that identity.
const taskLineEncodingVersion = "freeside.task_line/v1"

// TaskLineSource is the operator path that set a task line (plan §5.4,
// Admitted Agents). Every member is an authenticated operator action: intake
// from an issue, a label, or repository content never sets a line, because
// choosing an agent chooses which credential runs (§5.8). There is
// deliberately no intake member, so a line claiming one fails validation.
type TaskLineSource string

const (
	// TaskLineSourceSubmitTask is a paired client's submit_task command.
	TaskLineSourceSubmitTask TaskLineSource = "submit_task"
	// TaskLineSourceCLISubmit is `freesided submit` on the daemon host.
	TaskLineSourceCLISubmit TaskLineSource = "cli_submit"
	// TaskLineSourceProposalStart is the operator's Start or "start with
	// changes" decision on a task_proposal card. Nothing writes it until
	// #1641 binds the lines to the item version.
	TaskLineSourceProposalStart TaskLineSource = "proposal_start"
)

// AllTaskLineSources lists every valid TaskLineSource; it is the single
// registration point.
var AllTaskLineSources = []TaskLineSource{
	TaskLineSourceSubmitTask, TaskLineSourceCLISubmit, TaskLineSourceProposalStart,
}

func (s TaskLineSource) valid() bool {
	switch s {
	case TaskLineSourceSubmitTask, TaskLineSourceCLISubmit, TaskLineSourceProposalStart:
		return true
	default:
		return false
	}
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
}

// NewTaskLine builds a validated line version in canonical byte-form: the
// predecessor pointer is detached, the timestamp is normalized to UTC, and
// the ID is computed last from the bound facts.
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
