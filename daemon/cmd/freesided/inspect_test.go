package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/observe"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// TestInspectIndexGolden pins the index's shape. A change to these bytes is a
// change to inspectIndexVersion.
func TestInspectIndexGolden(t *testing.T) {
	latest := domain.RunID("run-freeside-656")
	got, err := json.MarshalIndent(inspectIndex{
		Version: inspectIndexVersion, AsOfRevision: 22, Reads: inspectReads(),
		Tasks: []inspectTask{
			{
				TaskID: "task-with-runs", ProjectID: "freeside",
				CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), LatestRunID: &latest,
			},
			{TaskID: "task-without-runs", ProjectID: "freeside", CreatedAt: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)},
		},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "inspect_index", append(got, '\n'))
}

// TestInspectIndexNamesItsReads holds the index to the versions the reads
// print, so the list cannot name a shape the command no longer writes.
func TestInspectIndexNamesItsReads(t *testing.T) {
	versions := map[string]string{}
	for _, read := range inspectReads() {
		if read.Name == "" || read.Command == "" || read.IDs == nil {
			t.Errorf("read %+v is incomplete", read)
		}
		if read.Version != nil {
			versions[read.Name] = *read.Version
		}
	}
	if versions["inspect"] != inspectIndexVersion || versions["inspect task"] != taskRecordVersion {
		t.Fatalf("versions = %v", versions)
	}
}

func startSeededDaemon(t *testing.T) (*daemon, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h := startDaemon(t, config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0", SeedFixture: "representative", Environment: environmentEphemeral,
	})
	return h, dbPath
}

func inspect[T any](t *testing.T, ctx context.Context, args ...string) T {
	t.Helper()
	var out bytes.Buffer
	if err := runInspectCommand(ctx, args, &out, io.Discard); err != nil {
		t.Fatalf("inspect %v: %v", args, err)
	}
	if bytes.Count(out.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("inspect %v printed more than one line", args)
	}
	var value T
	if err := strictjson.Decode(out.Bytes(), &value, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		t.Fatalf("inspect %v: %v", args, err)
	}
	return value
}

// TestInspectReadsThroughRunningDaemon fails any attempt to open the database
// while a daemon holds it: both reads must come over the control socket.
func TestInspectReadsThroughRunningDaemon(t *testing.T) {
	h, dbPath := startSeededDaemon(t)
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("inspect opened the database a daemon holds")
		}))
	index := inspect[inspectIndex](t, ctx, "-db", dbPath)
	if index.Version != inspectIndexVersion || index.AsOfRevision == 0 || len(index.Tasks) != 6 ||
		!slices.EqualFunc(index.Reads, inspectReads(), equalInspectReads) {
		t.Fatalf("index = %+v", index)
	}
	for _, task := range index.Tasks {
		record := inspect[taskRecord](t, ctx, "task", "-db", dbPath, "-task", string(task.TaskID))
		if record.Version != taskRecordVersion || record.TaskID != task.TaskID || record.AsOfRevision < index.AsOfRevision {
			t.Errorf("task %s: version %q, id %q, revision %d", task.TaskID, record.Version, record.TaskID, record.AsOfRevision)
		}
		if record.Timeline == nil || record.Items == nil || len(record.Runs) == 0 ||
			!equalPointers(task.LatestRunID, &record.Runs[len(record.Runs)-1].RunID) {
			t.Errorf("task %s: timeline error %+v, items error %+v, %d runs, latest run %v",
				task.TaskID, record.TimelineError, record.ItemsError, len(record.Runs), task.LatestRunID)
		}
	}
	err := runInspectCommand(ctx, []string{"task", "-db", dbPath, "-task", "task-unknown"}, io.Discard, io.Discard)
	if !errors.Is(err, store.ErrNotFound) || errors.Is(err, observe.ErrUsage) {
		t.Fatalf("unknown task over the socket: %v", err)
	}
}

func equalInspectReads(a, b inspectRead) bool {
	return a.Name == b.Name && a.Command == b.Command && slices.Equal(a.IDs, b.IDs) && equalPointers(a.Version, b.Version)
}

// TestInspectReadsStoreWithNoDaemon reads each record through a daemon,
// stops it, and expects the command to read the same record from the file.
func TestInspectReadsStoreWithNoDaemon(t *testing.T) {
	h, dbPath := startSeededDaemon(t)
	ctx := t.Context()
	index := inspect[inspectIndex](t, ctx, "-db", dbPath)
	served := map[domain.TaskID]taskRecord{}
	for _, task := range index.Tasks {
		served[task.TaskID] = inspect[taskRecord](t, ctx, "task", "-db", dbPath, "-task", string(task.TaskID))
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	direct := inspect[inspectIndex](t, ctx, "-db", dbPath)
	if !slices.EqualFunc(direct.Tasks, index.Tasks, func(a, b inspectTask) bool {
		return a.TaskID == b.TaskID && a.CreatedAt.Equal(b.CreatedAt) && equalPointers(a.LatestRunID, b.LatestRunID)
	}) {
		t.Fatalf("direct index tasks = %+v, served = %+v", direct.Tasks, index.Tasks)
	}
	for id, want := range served {
		got := inspect[taskRecord](t, ctx, "task", "-db", dbPath, "-task", string(id))
		if !bytes.Equal(comparableRecord(t, got), comparableRecord(t, want)) {
			t.Errorf("task %s: direct record differs from the served one\ndirect: %s\nserved: %s",
				id, comparableRecord(t, got), comparableRecord(t, want))
		}
	}
	err := runInspectCommand(ctx, []string{"task", "-db", dbPath, "-task", "task-unknown"}, io.Discard, io.Discard)
	if !errors.Is(err, store.ErrNotFound) || errors.Is(err, observe.ErrUsage) {
		t.Fatalf("unknown task with no daemon: %v", err)
	}
	// The reads above moved nothing.
	if after := inspect[inspectIndex](t, ctx, "-db", dbPath); after.AsOfRevision != direct.AsOfRevision {
		t.Fatalf("revision moved from %d to %d across reads", direct.AsOfRevision, after.AsOfRevision)
	}
}

// comparableRecord is the record without the read's wall-clock stamps and
// revision, which differ between two reads of unchanged task state.
func comparableRecord(t *testing.T, record taskRecord) []byte {
	t.Helper()
	record.AsOf, record.AsOfRevision = time.Time{}, 0
	if record.Timeline != nil {
		timeline := *record.Timeline
		timeline.AsOf, timeline.AsOfRevision = time.Time{}, 0
		record.Timeline = &timeline
	}
	out, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInspectUsageErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	for name, args := range map[string][]string{
		"no database":        {},
		"task with no id":    {"task", "-db", dbPath},
		"task flag on index": {"-db", dbPath, "-task", "task-1"},
		"unknown verb":       {"run", "-db", dbPath},
		"positional":         {"-db", dbPath, "task"},
		"unknown flag":       {"-db", dbPath, "-follow"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := runInspectCommand(t.Context(), args, &out, io.Discard)
			if !errors.Is(err, observe.ErrUsage) || out.Len() != 0 {
				t.Fatalf("err = %v, stdout = %q", err, out.String())
			}
		})
	}
	// A read never creates the database it is pointed at.
	err := runInspectCommand(t.Context(), []string{"-db", dbPath}, io.Discard, io.Discard)
	if err == nil || errors.Is(err, observe.ErrUsage) {
		t.Fatalf("missing database: %v", err)
	}
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("inspect created the database: %v", statErr)
	}
}
