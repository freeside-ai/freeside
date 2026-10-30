package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var taskLineChangeAt = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

func taskLineChangeFixture(t *testing.T) (domain.TaskLine, domain.TaskLineChange, domain.TaskLine) {
	t.Helper()
	first, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleReviewer, Agent: "codex",
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
	}, 1, nil, taskLineChangeAt)
	if err != nil {
		t.Fatal(err)
	}
	change, err := domain.NewTaskLineChange("task-1", domain.RoleReviewer, "claude-b", false, &first.ID, taskLineChangeAt)
	if err != nil {
		t.Fatal(err)
	}
	line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleReviewer, Agent: "claude-b",
		Source: domain.TaskLineSourceCLILineChange, SetBy: string(change.ID),
	}, 2, &first.ID, taskLineChangeAt)
	if err != nil {
		t.Fatal(err)
	}
	return first, change, line
}

// TestClearedTaskLineShape: a cleared version names no agent, supersedes a
// line, and only a line change writes one. A version that isn't cleared still
// names a valid agent.
func TestClearedTaskLineShape(t *testing.T) {
	t.Parallel()
	first, _, _ := taskLineChangeFixture(t)
	clear := domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleReviewer, Cleared: true,
		Source: domain.TaskLineSourceCLILineChange, SetBy: "change-1",
	}
	if _, err := domain.NewTaskLine(clear, 2, &first.ID, taskLineChangeAt); err != nil {
		t.Fatalf("a clear of a line: %v", err)
	}
	for name, build := range map[string]func() (domain.TaskLine, error){
		"clear naming an agent": func() (domain.TaskLine, error) {
			in := clear
			in.Agent = "codex"
			return domain.NewTaskLine(in, 2, &first.ID, taskLineChangeAt)
		},
		"clear as version 1": func() (domain.TaskLine, error) {
			return domain.NewTaskLine(clear, 1, nil, taskLineChangeAt)
		},
		"clear by a submission": func() (domain.TaskLine, error) {
			in := clear
			in.Source = domain.TaskLineSourceCLISubmit
			return domain.NewTaskLine(in, 2, &first.ID, taskLineChangeAt)
		},
		"no agent and no clear": func() (domain.TaskLine, error) {
			in := clear
			in.Cleared = false
			return domain.NewTaskLine(in, 2, &first.ID, taskLineChangeAt)
		},
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestTaskLineChangeValidate: an edited record resolves to another address,
// and no refusal repeats a refused role or agent.
func TestTaskLineChangeValidate(t *testing.T) {
	t.Parallel()
	first, change, _ := taskLineChangeFixture(t)
	const pasted = "sk-ant-oat01 not a name"
	for name, build := range map[string]func() (domain.TaskLineChange, error){
		"no task": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("", domain.RoleReviewer, "codex", false, nil, taskLineChangeAt)
		},
		"ineligible role": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", pasted, "codex", false, nil, taskLineChangeAt)
		},
		"ineligible role clearing": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", pasted, "", true, &first.ID, taskLineChangeAt)
		},
		"malformed agent": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", domain.RoleReviewer, pasted, false, nil, taskLineChangeAt)
		},
		"no agent": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", domain.RoleReviewer, "", false, nil, taskLineChangeAt)
		},
		"clear naming an agent": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", domain.RoleReviewer, pasted, true, &first.ID, taskLineChangeAt)
		},
		"clear of nothing": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", domain.RoleReviewer, "", true, nil, taskLineChangeAt)
		},
		"no instant": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", domain.RoleReviewer, "codex", false, nil, time.Time{})
		},
		"edited record": func() (domain.TaskLineChange, error) {
			edited := change
			edited.Agent = "codex"
			return edited, edited.Validate()
		},
	} {
		_, err := build()
		if err == nil {
			t.Errorf("%s was accepted", name)
		} else if strings.Contains(err.Error(), pasted) {
			t.Errorf("%s: the refusal repeats the request text: %v", name, err)
		}
	}
	if err := change.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskLineChangeBacksOnlyItsOwnLine(t *testing.T) {
	t.Parallel()
	first, change, line := taskLineChangeFixture(t)
	if !change.Backs(line) {
		t.Fatal("the change does not back the line it set")
	}
	for name, edit := range map[string]func(*domain.TaskLine){
		"another source": func(l *domain.TaskLine) { l.Source = domain.TaskLineSourceCLISubmit },
		"another record": func(l *domain.TaskLine) { l.SetBy = "change-2" },
		"another task":   func(l *domain.TaskLine) { l.TaskID = "task-2" },
		"another role":   func(l *domain.TaskLine) { l.Role = domain.RoleImplementer },
		"another agent":  func(l *domain.TaskLine) { l.Agent = "codex" },
		"a clear":        func(l *domain.TaskLine) { l.Agent, l.Cleared = "", true },
		"no predecessor": func(l *domain.TaskLine) { l.PredecessorID = nil },
		"another predecessor": func(l *domain.TaskLine) {
			other := domain.Digest("sha256:" + strings.Repeat("b", 64))
			l.PredecessorID = &other
		},
		"another instant": func(l *domain.TaskLine) { l.SetAt = l.SetAt.Add(time.Second) },
	} {
		edited := line
		edit(&edited)
		if change.Backs(edited) {
			t.Errorf("%s: the change backs the line", name)
		}
	}
	if change.Replaces(nil) || !change.Replaces(&first.ID) {
		t.Error("Replaces does not match exactly the version the change names")
	}
}

func TestAClearKeepsItsOwnDefaultOutOfOtherLines(t *testing.T) {
	t.Parallel()
	first, _, _ := taskLineChangeFixture(t)
	withoutClear, err := first.ComputeID()
	if err != nil {
		t.Fatal(err)
	}
	if withoutClear != first.ID {
		t.Fatalf("id = %s, want %s", withoutClear, first.ID)
	}
	// The golden fixtures pin the bytes; here, the bit moves the address.
	cleared := first
	cleared.Cleared = true
	if moved, err := cleared.ComputeID(); err != nil || moved == first.ID {
		t.Fatalf("a cleared bit left the id at %s, %v", moved, err)
	}
	if !errors.Is(cleared.Validate(), domain.ErrTaskLineInconsistent) {
		t.Fatalf("an edited cleared bit: %v", cleared.Validate())
	}
}
