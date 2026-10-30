package domain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// taskLineChangeEncodingVersion tags the canonical serialization
// TaskLineChange.ComputeID digests. A change to the encoding changes every
// record's identity, and a line's SetBy cites a change by that identity.
const taskLineChangeEncodingVersion = "freeside.task_line_change/v1"

// TaskLineChange is an operator's recorded change to a task's line for one
// role (plan §5.4, Admitted Agents): the record a cli_line_change line cites
// as its SetBy. Unlike a submission record, it stores the agent it chose and
// the version it replaced, so admission can hold the line to both. A row in
// task_lines alone proves nothing, because its id is an unkeyed hash of its
// own columns.
type TaskLineChange struct {
	// ID is the content address of every other field.
	ID     Digest   `json:"id"`
	TaskID TaskID   `json:"task_id"`
	Role   RoleName `json:"role"`
	// Agent is the agent the role moves to, empty exactly when Cleared.
	Agent string `json:"agent"`
	// Cleared takes the role back to the lineup.
	Cleared bool `json:"cleared"`
	// ReplacesID is the line version this change supersedes: nil only when
	// the role had no line.
	ReplacesID *Digest   `json:"replaces_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

// NewTaskLineChange builds a validated change in canonical byte-form: the
// replaced id is detached, the instant is normalized to UTC, and the ID is
// computed last from the bound facts.
func NewTaskLineChange(
	taskID TaskID, role RoleName, agent string, cleared bool, replaces *Digest, recordedAt time.Time,
) (TaskLineChange, error) {
	change := TaskLineChange{
		TaskID: taskID, Role: role, Agent: agent, Cleared: cleared,
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

// canonicalTaskLineChange is the versioned serialization ComputeID digests:
// every bound fact and nothing derived (ID is this value).
type canonicalTaskLineChange struct {
	Version    string    `json:"version"`
	TaskID     TaskID    `json:"task_id"`
	Role       RoleName  `json:"role"`
	Agent      string    `json:"agent"`
	Cleared    bool      `json:"cleared"`
	ReplacesID *Digest   `json:"replaces_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

// ComputeID returns the content address of the change: a sha256 over its
// versioned canonical serialization.
func (c TaskLineChange) ComputeID() (Digest, error) {
	body, err := json.Marshal(canonicalTaskLineChange{
		Version: taskLineChangeEncodingVersion, TaskID: c.TaskID, Role: c.Role,
		Agent: c.Agent, Cleared: c.Cleared, ReplacesID: c.ReplacesID, RecordedAt: c.RecordedAt,
	})
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
// this record, and they agree on the task, role, agent or clear, the version
// replaced, and the instant.
func (c TaskLineChange) Backs(line TaskLine) bool {
	return line.Source == TaskLineSourceCLILineChange && line.SetBy == string(c.ID) &&
		c.TaskID == line.TaskID && c.Role == line.Role && c.Agent == line.Agent &&
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
	if c.Cleared {
		switch {
		case !c.Role.TaskLineEligible():
			return fmt.Errorf("task line change role: %w", ErrTaskLineRoleIneligible)
		case c.Agent != "":
			return fmt.Errorf("task line change %s clear names an agent: %w", c.Role, ErrTaskLineInconsistent)
		case c.ReplacesID == nil:
			return fmt.Errorf("task line change %s clear replaces no line: %w", c.Role, ErrTaskLineInconsistent)
		}
	} else if err := ValidateTaskLineChoices([]TaskLineChoice{{Role: c.Role, Agent: c.Agent}}); err != nil {
		return err
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
