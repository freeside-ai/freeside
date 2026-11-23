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
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func runTaskWriterClass(t *testing.T, ctx context.Context, args ...string) (taskWriterClassResult, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runTaskWriterClassCommand(ctx, args, &stdout, io.Discard)
	var result taskWriterClassResult
	if err == nil {
		if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
			t.Fatalf("decode %q: %v", stdout.String(), decodeErr)
		}
	}
	return result, err
}

func taskRecordWriterClassAt(t *testing.T, dbPath string, task domain.TaskID) (*taskRecordWriterClass, *taskRecordError) {
	t.Helper()
	st, err := store.Open(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var record taskRecord
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		record, err = readTaskRecord(t.Context(), tx, task, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record.WriterClass, record.WriterClassError
}

// TestTaskWriterClassCommandSetsAndClears: the command sets the warded
// requirement, the task's record shows it, and a clear removes it.
func TestTaskWriterClassCommandSetsAndClears(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	run := seedReviewedRun(t, st, "run-writer-class")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	task := string(run.TaskID)
	if shown, shownErr := taskRecordWriterClassAt(t, dbPath, run.TaskID); shown != nil || shownErr != nil {
		t.Fatalf("record before any set: %+v, %+v", shown, shownErr)
	}

	set, err := runTaskWriterClass(t, t.Context(), "-db", dbPath, "-task", task, "-class", "fresh_vm_read_only_volume_handoff")
	if err != nil {
		t.Fatal(err)
	}
	if set.TaskID != run.TaskID || set.WriterClass == nil || set.WriterClass.Class != domain.BackendFreshVMReadOnlyVolumeHandoff ||
		!strings.HasPrefix(set.WriterClass.SetBy, "cli:") || set.Removed {
		t.Fatalf("set = %+v", set)
	}
	shown, shownErr := taskRecordWriterClassAt(t, dbPath, run.TaskID)
	if shownErr != nil || shown == nil || shown.Class != domain.BackendFreshVMReadOnlyVolumeHandoff ||
		shown.SetBy != set.WriterClass.SetBy || !shown.SetAt.Equal(set.WriterClass.SetAt) {
		t.Fatalf("record after set: %+v, %+v; want %+v", shown, shownErr, set.WriterClass)
	}

	for _, want := range []bool{true, false} {
		cleared, err := runTaskWriterClass(t, t.Context(), "-db", dbPath, "-task", task, "-clear")
		if err != nil {
			t.Fatal(err)
		}
		if cleared.WriterClass != nil || cleared.Removed != want {
			t.Fatalf("clear = %+v, want removed %t", cleared, want)
		}
	}
	if shown, shownErr := taskRecordWriterClassAt(t, dbPath, run.TaskID); shown != nil || shownErr != nil {
		t.Fatalf("record after clear: %+v, %+v", shown, shownErr)
	}
}

func TestTaskWriterClassCommandRefusals(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	task := string(seedReviewedRun(t, st, "run-writer-class").TaskID)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args []string
		want error
	}{
		"cordoned class": {[]string{"-db", dbPath, "-task", task, "-class", "cordoned_host_process"}, domain.ErrTaskWriterClassNotWarded},
		"unknown class":  {[]string{"-db", dbPath, "-task", task, "-class", "warded"}, domain.ErrTaskWriterClassNotWarded},
		"missing task": {
			[]string{"-db", dbPath, "-task", "task-missing", "-class", "fresh_vm_read_only_volume_handoff"}, store.ErrNotFound,
		},
		"clear on a missing task": {[]string{"-db", dbPath, "-task", "task-missing", "-clear"}, store.ErrNotFound},
		"no db":                   {[]string{"-task", task, "-clear"}, nil},
		"no task":                 {[]string{"-db", dbPath, "-clear"}, nil},
		"neither class nor clear": {[]string{"-db", dbPath, "-task", task}, nil},
		"both class and clear":    {[]string{"-db", dbPath, "-task", task, "-clear", "-class", "fresh_vm_read_only_volume_handoff"}, nil},
	} {
		_, err := runTaskWriterClass(t, t.Context(), tc.args...)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if shown, shownErr := taskRecordWriterClassAt(t, dbPath, domain.TaskID(task)); shown != nil || shownErr != nil {
		t.Fatalf("record after refusals: %+v, %+v", shown, shownErr)
	}
}
