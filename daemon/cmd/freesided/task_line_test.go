package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func runTaskLine(t *testing.T, ctx context.Context, args ...string) (taskLineResult, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runTaskLineCommand(ctx, args, &stdout, io.Discard)
	var result taskLineResult
	if err == nil {
		if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
			t.Fatalf("decode %q: %v", stdout.String(), decodeErr)
		}
	}
	return result, err
}

// TestTaskLineCommandChangesAndClears: the command reports what it wrote, a
// repeat reports that it wrote nothing, and a clear ends the role's line.
func TestTaskLineCommandChangesAndClears(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	run := seedReviewedRun(t, st, "run-line")
	first := setReviewedRunLine(t, st, run, domain.RoleReviewer, "claude-a", "cmd-run-line", 0)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	task := string(run.TaskID)

	changed, err := runTaskLine(t, t.Context(), "-db", dbPath, "-task", task, "-role", "reviewer", "-agent", "codex")
	if err != nil {
		t.Fatal(err)
	}
	line := changed.Line
	if !changed.Recorded || changed.TaskID != run.TaskID || changed.Role != domain.RoleReviewer || line == nil ||
		line.Agent != "codex" || line.Version != 2 || *line.PredecessorID != first.ID {
		t.Fatalf("change = %+v, line %+v", changed, line)
	}
	repeat, err := runTaskLine(t, t.Context(), "-db", dbPath, "-task", task, "-role", "reviewer", "-agent", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Recorded || repeat.Line == nil || repeat.Line.ID != line.ID {
		t.Fatalf("repeat = %+v, want the current line and nothing recorded", repeat)
	}
	cleared, err := runTaskLine(t, t.Context(), "-db", dbPath, "-task", task, "-role", "reviewer", "-clear")
	if err != nil {
		t.Fatal(err)
	}
	if !cleared.Recorded || cleared.Line == nil || !cleared.Line.Cleared || cleared.Line.Agent != "" {
		t.Fatalf("clear = %+v, line %+v", cleared, cleared.Line)
	}
	// A role with no line has none to report.
	none, err := runTaskLine(t, t.Context(), "-db", dbPath, "-task", task, "-role", "specifier", "-clear")
	if err != nil {
		t.Fatal(err)
	}
	if none.Recorded || none.Line != nil {
		t.Fatalf("clear of a lineless role = %+v", none)
	}
}

func TestTaskLineCommandRefusals(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	task := string(seedReviewedRun(t, st, "run-line").TaskID)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Fails the agent grammar and reads like a pasted credential.
	const pasted = "sk-ant-oat01 fixture not a token"
	for name, args := range map[string][]string{
		"no db":             {"-task", task, "-role", "reviewer", "-clear"},
		"no task":           {"-db", dbPath, "-role", "reviewer", "-clear"},
		"no role":           {"-db", dbPath, "-task", task, "-clear"},
		"neither":           {"-db", dbPath, "-task", task, "-role", "reviewer"},
		"both":              {"-db", dbPath, "-task", task, "-role", "reviewer", "-agent", "codex", "-clear"},
		"positional":        {"-db", dbPath, "-task", task, "-role", "reviewer", "-clear", pasted},
		"ineligible role":   {"-db", dbPath, "-task", task, "-role", "shadow_reviewer", "-agent", "codex"},
		"unknown role":      {"-db", dbPath, "-task", task, "-role", pasted, "-agent", "codex"},
		"malformed agent":   {"-db", dbPath, "-task", task, "-role", "reviewer", "-agent", pasted},
		"unknown task":      {"-db", dbPath, "-task", "task-missing", "-role", "reviewer", "-agent", "codex"},
		"a missing db file": {"-db", dbPath + ".missing", "-task", task, "-role", "reviewer", "-clear"},
	} {
		_, err := runTaskLine(t, t.Context(), args...)
		if err == nil {
			t.Errorf("%s: task-line succeeded", name)
		} else if strings.Contains(err.Error(), pasted) {
			t.Errorf("%s: the refusal repeats the request text: %v", name, err)
		}
	}
}

func TestTaskLineUsesRunningDaemonStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	task := seedReviewedRun(t, h.store, "run-line").TaskID
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("unexpected direct store open")
		}))

	result, err := runTaskLine(t, ctx, "-db", dbPath, "-task", string(task), "-role", "implementer", "-agent", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Recorded || result.Line == nil || result.Line.Agent != "codex" {
		t.Fatalf("result = %+v", result)
	}
	const pasted = "sk-ant-oat01 fixture not a token"
	_, err = runTaskLine(t, ctx, "-db", dbPath, "-task", string(task), "-role", "implementer", "-agent", pasted)
	if err == nil || strings.Contains(err.Error(), pasted) {
		t.Fatalf("forwarded refusal = %v", err)
	}
	if err := h.store.Read(t.Context(), func(tx *store.ReadTx) error {
		current, err := tx.CurrentTaskLines(t.Context(), task)
		if err == nil && current[domain.RoleImplementer].ID != result.Line.ID {
			t.Errorf("running daemon store line = %+v", current[domain.RoleImplementer])
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
