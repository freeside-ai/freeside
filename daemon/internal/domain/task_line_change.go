package domain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// TaskLineChangeEncoding tags the canonical serialization
// TaskLineChange.ComputeID digests. A change to the encoding changes every
// record's identity, and a line's SetBy cites a change by that identity, so a
// stored record keeps the encoding it was written under.
type TaskLineChangeEncoding string

const (
	// TaskLineChangeEncodingV1 names the agent; records written before #2597
	// hold it.
	TaskLineChangeEncodingV1 TaskLineChangeEncoding = "freeside.task_line_change/v1"
	// TaskLineChangeEncodingV2 holds the selection; every change written
	// since does, clears included.
	TaskLineChangeEncodingV2 TaskLineChangeEncoding = "freeside.task_line_change/v2"
)

// AllTaskLineChangeEncodings lists every valid TaskLineChangeEncoding; it is
// the single registration point.
var AllTaskLineChangeEncodings = []TaskLineChangeEncoding{TaskLineChangeEncodingV1, TaskLineChangeEncodingV2}

func (e TaskLineChangeEncoding) valid() bool {
	switch e {
	case TaskLineChangeEncodingV1, TaskLineChangeEncodingV2:
		return true
	default:
		return false
	}
}

// TaskLineChange is an operator's recorded change to a task's line for one
// role (plan §5.4, Admitted Agents): the record a cli_line_change or
// client_line_change line cites as its SetBy. Unlike a submission record, it
// stores the choice and the version it replaced, so admission can hold the
// line to both. A row in task_lines alone proves nothing, because its id is
// an unkeyed hash of its own columns.
//
// Its JSON form depends on the encoding (MarshalJSON), as TaskLine's does: a
// v1 record keeps the body it was stored under.
type TaskLineChange struct {
	// ID is the content address of every other field.
	ID       Digest
	Encoding TaskLineChangeEncoding
	TaskID   TaskID
	Role     RoleName
	// Agent is a v1 record's agent, empty when Cleared.
	Agent string
	// Selection is a v2 record's enrollment, model, and effort, nil when
	// Cleared.
	Selection *TaskLineSelection
	// Cleared takes the role back to the lineup.
	Cleared bool
	// ReplacesID is the line version this change supersedes: nil only when
	// the role had no line.
	ReplacesID *Digest
	RecordedAt time.Time
}

// NewTaskLineChange builds a validated v2 change in canonical byte-form: a
// nil selection clears the role, the replaced id is detached, the instant is
// normalized to UTC, and the ID is computed last from the bound facts.
func NewTaskLineChange(
	taskID TaskID, role RoleName, selection *TaskLineSelection, replaces *Digest, recordedAt time.Time,
) (TaskLineChange, error) {
	change := TaskLineChange{
		Encoding: TaskLineChangeEncodingV2, TaskID: taskID, Role: role,
		Selection: clonePtr(selection), Cleared: selection == nil,
		ReplacesID: clonePtr(replaces), RecordedAt: recordedAt.UTC(),
	}
	id, err := change.ComputeID()
	if err != nil {
		return TaskLineChange{}, err
	}
	change.ID = id
	if err := change.Validate(); err != nil {
		return TaskLineChange{}, err
	}
	return change, nil
}

// canonicalTaskLineChangeV1 is the v1 serialization ComputeID digests: every
// bound fact and nothing derived (ID is this value).
type canonicalTaskLineChangeV1 struct {
	Version    TaskLineChangeEncoding `json:"version"`
	TaskID     TaskID                 `json:"task_id"`
	Role       RoleName               `json:"role"`
	Agent      string                 `json:"agent"`
	Cleared    bool                   `json:"cleared"`
	ReplacesID *Digest                `json:"replaces_id"`
	RecordedAt time.Time              `json:"recorded_at"`
}

// canonicalTaskLineChangeV2 is the v2 serialization: the selection in place
// of the agent, null on a clear.
type canonicalTaskLineChangeV2 struct {
	Version    TaskLineChangeEncoding `json:"version"`
	TaskID     TaskID                 `json:"task_id"`
	Role       RoleName               `json:"role"`
	Selection  *TaskLineSelection     `json:"selection"`
	Cleared    bool                   `json:"cleared"`
	ReplacesID *Digest                `json:"replaces_id"`
	RecordedAt time.Time              `json:"recorded_at"`
}

// ComputeID returns the content address of the change: a sha256 over the
// canonical serialization of the encoding the record declares.
func (c TaskLineChange) ComputeID() (Digest, error) {
	var canonical any
	switch c.Encoding {
	case TaskLineChangeEncodingV1:
		canonical = canonicalTaskLineChangeV1{
			Version: c.Encoding, TaskID: c.TaskID, Role: c.Role,
			Agent: c.Agent, Cleared: c.Cleared, ReplacesID: c.ReplacesID, RecordedAt: c.RecordedAt,
		}
	case TaskLineChangeEncodingV2:
		canonical = canonicalTaskLineChangeV2{
			Version: c.Encoding, TaskID: c.TaskID, Role: c.Role,
			Selection: c.Selection, Cleared: c.Cleared, ReplacesID: c.ReplacesID, RecordedAt: c.RecordedAt,
		}
	default:
		return "", fmt.Errorf("task line change id: encoding %q: %w", c.Encoding, ErrInvalidTaskLineEncoding)
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("task line change id: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Replaces reports whether the change supersedes exactly the given version;
// nil means none.
func (c TaskLineChange) Replaces(predecessor *Digest) bool {
	if c.ReplacesID == nil || predecessor == nil {
		return c.ReplacesID == nil && predecessor == nil
	}
	return *c.ReplacesID == *predecessor
}

// Backs reports whether this is the change that set the line: the line names
// this record, and they agree on the task, role, agent or selection or clear,
// the version replaced, and the instant. A change writes a line of its own
// generation, so a v1 record never backs a v2 line or the reverse.
func (c TaskLineChange) Backs(line TaskLine) bool {
	sameEncoding := (c.Encoding == TaskLineChangeEncodingV1) == (line.Encoding == TaskLineEncodingV1)
	sameSelection := (c.Selection == nil) == (line.Selection == nil) &&
		(c.Selection == nil || *c.Selection == *line.Selection)
	return line.Source.IsLineChange() && line.SetBy == string(c.ID) && sameEncoding &&
		c.TaskID == line.TaskID && c.Role == line.Role && c.Agent == line.Agent && sameSelection &&
		c.Cleared == line.Cleared && c.Replaces(line.PredecessorID) && c.RecordedAt.Equal(line.SetAt)
}

// Validate reports whether the change is well-formed and its identity
// authentic. The store re-runs it on every decode. Like
// ValidateTaskLineChoices, it never repeats a refused role or agent, which
// is request text.
func (c TaskLineChange) Validate() error {
	if c.TaskID == "" {
		return fmt.Errorf("task line change task_id: %w", ErrEmptyID)
	}
	if !c.Encoding.valid() {
		return fmt.Errorf("task line change encoding: %w", ErrInvalidTaskLineEncoding)
	}
	if (c.Encoding == TaskLineChangeEncodingV1 && c.Selection != nil) ||
		(c.Encoding == TaskLineChangeEncodingV2 && c.Agent != "") {
		return fmt.Errorf("task line change %s: %w", c.Encoding, ErrInvalidTaskLineEncoding)
	}
	if c.Cleared {
		switch {
		case !c.Role.TaskLineEligible():
			return fmt.Errorf("task line change role: %w", ErrTaskLineRoleIneligible)
		case c.Agent != "" || c.Selection != nil:
			return fmt.Errorf("task line change %s clear names an agent: %w", c.Role, ErrTaskLineInconsistent)
		case c.ReplacesID == nil:
			return fmt.Errorf("task line change %s clear replaces no line: %w", c.Role, ErrTaskLineInconsistent)
		}
	} else if err := ValidateTaskLineChoices([]TaskLineChoice{{Role: c.Role, Agent: c.Agent, Selection: c.Selection}}); err != nil {
		return err
	}
	if c.Encoding == TaskLineChangeEncodingV2 && !c.Cleared && c.Selection == nil {
		return fmt.Errorf("task line change %s selects nothing: %w", c.Role, ErrInvalidTaskLineSelection)
	}
	if c.ReplacesID != nil && !contentaddr.Valid(string(*c.ReplacesID)) {
		return fmt.Errorf("task line change %s replaces_id: %w", c.Role, ErrInvalidDigest)
	}
	if c.RecordedAt.IsZero() {
		return fmt.Errorf("task line change %s recorded_at: %w", c.Role, ErrMissingTimestamp)
	}
	// recorded_at is part of the canonical encoding the id addresses, so one
	// instant must have one byte form.
	if c.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("task line change %s recorded_at: %w", c.Role, ErrTimestampNotUTC)
	}
	computed, err := c.ComputeID()
	if err != nil {
		return err
	}
	if c.ID != computed {
		return fmt.Errorf("task line change %s, content resolves to %s: %w", c.ID, computed, ErrTaskLineInconsistent)
	}
	return nil
}

// taskLineChangeV1JSON is a v1 record's stored body, unchanged since before
// v2.
type taskLineChangeV1JSON struct {
	ID         Digest    `json:"id"`
	TaskID     TaskID    `json:"task_id"`
	Role       RoleName  `json:"role"`
	Agent      string    `json:"agent"`
	Cleared    bool      `json:"cleared"`
	ReplacesID *Digest   `json:"replaces_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

// taskLineChangeV2JSON is a v2 record's stored body: it names its encoding,
// which a v1 body never carries, and the selection in place of the agent.
type taskLineChangeV2JSON struct {
	ID         Digest                 `json:"id"`
	Encoding   TaskLineChangeEncoding `json:"encoding"`
	TaskID     TaskID                 `json:"task_id"`
	Role       RoleName               `json:"role"`
	Selection  *TaskLineSelection     `json:"selection"`
	Cleared    bool                   `json:"cleared"`
	ReplacesID *Digest                `json:"replaces_id"`
	RecordedAt time.Time              `json:"recorded_at"`
}

// MarshalJSON writes the body of the record's encoding.
func (c TaskLineChange) MarshalJSON() ([]byte, error) {
	switch c.Encoding {
	case TaskLineChangeEncodingV1:
		return json.Marshal(taskLineChangeV1JSON{
			ID: c.ID, TaskID: c.TaskID, Role: c.Role, Agent: c.Agent, Cleared: c.Cleared,
			ReplacesID: c.ReplacesID, RecordedAt: c.RecordedAt,
		})
	case TaskLineChangeEncodingV2:
		return json.Marshal(taskLineChangeV2JSON{
			ID: c.ID, Encoding: c.Encoding, TaskID: c.TaskID, Role: c.Role, Selection: c.Selection,
			Cleared: c.Cleared, ReplacesID: c.ReplacesID, RecordedAt: c.RecordedAt,
		})
	}
	return nil, fmt.Errorf("task line change %s encoding %q: %w", c.ID, c.Encoding, ErrInvalidTaskLineEncoding)
}

// UnmarshalJSON reads either body: one with no encoding is v1. It decodes
// only; the caller validates.
func (c *TaskLineChange) UnmarshalJSON(data []byte) error {
	var probe struct {
		Encoding *TaskLineChangeEncoding `json:"encoding"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if probe.Encoding == nil {
		var v1 taskLineChangeV1JSON
		if err := json.Unmarshal(data, &v1); err != nil {
			return err
		}
		*c = TaskLineChange{
			ID: v1.ID, Encoding: TaskLineChangeEncodingV1, TaskID: v1.TaskID, Role: v1.Role, Agent: v1.Agent,
			Cleared: v1.Cleared, ReplacesID: v1.ReplacesID, RecordedAt: v1.RecordedAt,
		}
		return nil
	}
	if *probe.Encoding != TaskLineChangeEncodingV2 {
		return fmt.Errorf("task line change encoding %q: %w", *probe.Encoding, ErrInvalidTaskLineEncoding)
	}
	var v2 taskLineChangeV2JSON
	if err := json.Unmarshal(data, &v2); err != nil {
		return err
	}
	*c = TaskLineChange{
		ID: v2.ID, Encoding: v2.Encoding, TaskID: v2.TaskID, Role: v2.Role, Selection: v2.Selection,
		Cleared: v2.Cleared, ReplacesID: v2.ReplacesID, RecordedAt: v2.RecordedAt,
	}
	return nil
}
