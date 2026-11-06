package domain_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var (
	taskLineChangeAt = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	selectionCodex   = domain.TaskLineSelection{EnrollmentID: "codex-pro", ModelID: "gpt-5.1-codex", Effort: "xhigh"}
	selectionClaude  = domain.TaskLineSelection{EnrollmentID: "claude-max-b", ModelID: "claude-sonnet-4-5", Effort: "medium"}
)

func taskLineChangeFixture(t *testing.T) (domain.TaskLine, domain.TaskLineChange, domain.TaskLine) {
	t.Helper()
	first, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleReviewer, Selection: &selectionCodex,
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
	}, 1, nil, taskLineChangeAt)
	if err != nil {
		t.Fatal(err)
	}
	change, err := domain.NewTaskLineChange("task-1", domain.RoleReviewer, &selectionClaude, &first.ID, taskLineChangeAt)
	if err != nil {
		t.Fatal(err)
	}
	line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleReviewer, Selection: &selectionClaude,
		Source: domain.TaskLineSourceCLILineChange, SetBy: string(change.ID),
	}, 2, &first.ID, taskLineChangeAt)
	if err != nil {
		t.Fatal(err)
	}
	return first, change, line
}

// TestClearedTaskLineShape: a cleared version names no agent and selects
// nothing, supersedes a line, and only a line change writes one. A version
// that isn't cleared still names a valid agent or selects one.
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
		"clear selecting": func() (domain.TaskLine, error) {
			in := clear
			in.Selection = &selectionCodex
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
	pastedSelection := selectionCodex
	pastedSelection.ModelID = pasted
	for name, build := range map[string]func() (domain.TaskLineChange, error){
		"no task": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("", domain.RoleReviewer, &selectionCodex, nil, taskLineChangeAt)
		},
		"ineligible role": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", pasted, &selectionCodex, nil, taskLineChangeAt)
		},
		"ineligible role clearing": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", pasted, nil, &first.ID, taskLineChangeAt)
		},
		"malformed selection": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", domain.RoleReviewer, &pastedSelection, nil, taskLineChangeAt)
		},
		"empty selection": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", domain.RoleReviewer, &domain.TaskLineSelection{}, nil, taskLineChangeAt)
		},
		"no selection and no clear": func() (domain.TaskLineChange, error) {
			edited := change
			edited.Selection = nil
			edited.ID, _ = edited.ComputeID()
			return edited, edited.Validate()
		},
		"v2 naming an agent": func() (domain.TaskLineChange, error) {
			edited := change
			edited.Agent = "codex"
			edited.ID, _ = edited.ComputeID()
			return edited, edited.Validate()
		},
		"clear of nothing": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", domain.RoleReviewer, nil, nil, taskLineChangeAt)
		},
		"no instant": func() (domain.TaskLineChange, error) {
			return domain.NewTaskLineChange("task-1", domain.RoleReviewer, &selectionCodex, nil, time.Time{})
		},
		"edited record": func() (domain.TaskLineChange, error) {
			edited := change
			edited.Selection = &selectionCodex
			return edited, edited.Validate()
		},
		"edited encoding": func() (domain.TaskLineChange, error) {
			edited := change
			edited.Encoding = domain.TaskLineChangeEncodingV1
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
		"another source":    func(l *domain.TaskLine) { l.Source = domain.TaskLineSourceCLISubmit },
		"another record":    func(l *domain.TaskLine) { l.SetBy = "change-2" },
		"another task":      func(l *domain.TaskLine) { l.TaskID = "task-2" },
		"another role":      func(l *domain.TaskLine) { l.Role = domain.RoleImplementer },
		"another selection": func(l *domain.TaskLine) { l.Selection = &selectionCodex },
		"a clear":           func(l *domain.TaskLine) { l.Selection, l.Cleared = nil, true },
		"a v1 line": func(l *domain.TaskLine) {
			l.Encoding, l.Selection, l.Agent = domain.TaskLineEncodingV1, nil, "claude-b"
		},
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

// TestV1ChangeBacksOnlyAV1Line: a record stored before v2 still backs the v1
// line it set, by name, and never a v2 line that selects instead.
func TestV1ChangeBacksOnlyAV1Line(t *testing.T) {
	t.Parallel()
	first, _, v2Line := taskLineChangeFixture(t)
	change := domain.TaskLineChange{
		Encoding: domain.TaskLineChangeEncodingV1, TaskID: "task-1", Role: domain.RoleReviewer,
		Agent: "claude-b", ReplacesID: &first.ID, RecordedAt: taskLineChangeAt,
	}
	var err error
	if change.ID, err = change.ComputeID(); err != nil {
		t.Fatal(err)
	}
	if err := change.Validate(); err != nil {
		t.Fatal(err)
	}
	line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleReviewer, Agent: "claude-b",
		Source: domain.TaskLineSourceCLILineChange, SetBy: string(change.ID),
	}, 2, &first.ID, taskLineChangeAt)
	if err != nil {
		t.Fatal(err)
	}
	if line.Encoding != domain.TaskLineEncodingV1 || !change.Backs(line) {
		t.Fatalf("the v1 change does not back its v1 line %+v", line)
	}
	v2Line.SetBy = string(change.ID)
	if change.Backs(v2Line) {
		t.Fatal("a v1 change backs a v2 line")
	}
	selecting := change
	selecting.Selection = &selectionClaude
	if selecting.ID, err = selecting.ComputeID(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(selecting.Validate(), domain.ErrInvalidTaskLineEncoding) {
		t.Fatalf("a v1 change with a selection: %v", selecting.Validate())
	}
}

// TestTaskLineRecordsRoundTripTheirJSON: each encoding reads back as the
// record it was written from, and a v1 form carries no encoding.
func TestTaskLineRecordsRoundTripTheirJSON(t *testing.T) {
	t.Parallel()
	first, change, line := taskLineChangeFixture(t)
	clear, err := domain.NewTaskLineChange("task-1", domain.RoleReviewer, nil, &line.ID, taskLineChangeAt)
	if err != nil {
		t.Fatal(err)
	}
	v1Line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleReviewer, Agent: "codex",
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
	}, 1, nil, taskLineChangeAt)
	if err != nil {
		t.Fatal(err)
	}
	for name, record := range map[string]any{
		"v2 line": first, "v2 changed line": line, "v1 line": v1Line, "v2 change": change, "v2 clear": clear,
	} {
		body, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(name, "v1") == strings.Contains(string(body), `"encoding"`) {
			t.Errorf("%s: encoding in %s", name, body)
		}
		decoded := reflect.New(reflect.TypeOf(record))
		if err := json.Unmarshal(body, decoded.Interface()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(decoded.Elem().Interface(), record) {
			t.Errorf("%s: read back %+v, want %+v", name, decoded.Elem().Interface(), record)
		}
	}
	var unknown domain.TaskLine
	if err := json.Unmarshal([]byte(`{"encoding":"freeside.task_line/v3"}`), &unknown); !errors.Is(err, domain.ErrInvalidTaskLineEncoding) {
		t.Fatalf("an unknown encoding: %v", err)
	}
}
