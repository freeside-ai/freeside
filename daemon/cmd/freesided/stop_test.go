package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestStopUsesRunningDaemon stops a submitted task through the control
// socket: the receipt names the operator's account, the task's record shows
// the Stop as a client's Stop would, and nothing opens the store directly.
func TestStopUsesRunningDaemon(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	seedTemplateStore(t, dbPath)
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral,
		DBPath:      dbPath, ListenAddr: "127.0.0.1:0",
		FakeDriverEnabled: true, SeedWalkingSkeleton: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
		t.Error("stop opened the store directly")
		return nil, errors.New("unexpected direct store open")
	}))
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	submit := func(id string) domain.TaskID {
		t.Helper()
		submitted, err := runSubmitCommand(ctx, submitCommandConfig{
			SubmissionID: id, DBPath: dbPath, TaskPath: taskPath,
			PolicyPath: policyPath, WorkUnitPath: declaredWorkUnit(t), PublicationPath: publicationPath, ProjectID: "project-control",
		})
		if err != nil {
			t.Fatal(err)
		}
		return submitted.TaskID
	}

	// Abandoning first frees the shared source for the next submission.
	abandoned := submit("stop-abandoned")
	if _, err := runAbandonCommand(ctx, abandonCommandConfig{DBPath: dbPath, TaskID: abandoned}); err != nil {
		t.Fatal(err)
	}
	_, err = runStopCommand(ctx, stopCommandConfig{DBPath: dbPath, TaskID: abandoned})
	if err == nil || !strings.Contains(err.Error(), "already "+string(domain.TaskAbandoned)) {
		t.Fatalf("stop of an abandoned task error = %v, want it to name the state", err)
	}
	if err := h.Store.Read(ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetTask(ctx, abandoned)
		if err == nil && task.Cancellation != nil {
			t.Errorf("refused stop recorded cancellation %+v", task.Cancellation)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	stopped := submit("stop-live")
	receipt, err := runStopCommand(ctx, stopCommandConfig{DBPath: dbPath, TaskID: stopped})
	if err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if want := domain.DeviceID("cli:" + account.Username); receipt.DeviceID != want {
		t.Fatalf("receipt device = %q, want %q", receipt.DeviceID, want)
	}
	if receipt.TaskID != stopped || receipt.Cancellation.State != domain.TaskCancellationRequested {
		t.Fatalf("receipt = %+v, want a requested Stop of %s", receipt, stopped)
	}
	record, err := readTaskRecordFrom(ctx, h.Store, stopped)
	if err != nil {
		t.Fatal(err)
	}
	if record.Task == nil || record.Task.Cancellation == nil || record.Task.Cancellation.RequestID != receipt.Cancellation.RequestID {
		t.Fatalf("task record cancellation = %+v, want request %s", record.Task, receipt.Cancellation.RequestID)
	}
	if record.Timeline == nil || !hasTaskEvent(record.Timeline.Events, domain.TaskEventStopRequested) {
		t.Fatalf("timeline lacks %s: %+v", domain.TaskEventStopRequested, record.Timeline)
	}
}

// TestStopAgainConvergesOnPendingStop runs the daemon side with no engine,
// so no cancellation loop can conclude the first Stop before the second.
func TestStopAgainConvergesOnPendingStop(t *testing.T) {
	t.Parallel()
	dbPath, st, parent := reattemptTaskFixture(t)
	canonical, err := canonicalDatabasePath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	daemon := daemonStoreContext{
		store: st, dbPath: canonical,
		commitTaskStop: func(_ context.Context, _ domain.TaskID, commit func() error) error { return commit() },
	}
	first, err := stopTaskInDaemon(t.Context(), daemon, parent.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := stopTaskInDaemon(t.Context(), daemon, parent.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if first.CommandID == second.CommandID {
		t.Fatalf("both stops reused command %s", first.CommandID)
	}
	if second.Cancellation.RequestID != first.Cancellation.RequestID {
		t.Fatalf("second stop cancellation = %s, want the pending %s", second.Cancellation.RequestID, first.Cancellation.RequestID)
	}
}

// TestStopRefusesWithoutDaemon checks a Stop on a database no daemon holds
// points at the daemon or abandon, and leaves the database untouched.
func TestStopRefusesWithoutDaemon(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	before := fileDigest(t, dbPath)
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
		t.Error("stop opened the store directly")
		return nil, errors.New("unexpected direct store open")
	}))
	_, err := runStopCommand(ctx, stopCommandConfig{DBPath: dbPath, TaskID: "task-1"})
	if err == nil || !strings.Contains(err.Error(), "no daemon is running") || !strings.Contains(err.Error(), "freesided abandon") {
		t.Fatalf("stop without daemon error = %v", err)
	}
	if after := fileDigest(t, dbPath); !bytes.Equal(before, after) {
		t.Fatal("stop without daemon changed the database")
	}
}

func TestParseStopRequiresFlags(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"no db":      {"-task", "task-1"},
		"no task":    {"-db", "state.db"},
		"positional": {"-db", "state.db", "-task", "task-1", "extra"},
	} {
		if _, err := parseStopCommand(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: parse accepted %v", name, args)
		}
	}
	cfg, err := parseStopCommand([]string{"-db", "state.db", "--task", "task-1"}, &bytes.Buffer{})
	if err != nil || cfg != (stopCommandConfig{DBPath: "state.db", TaskID: "task-1"}) {
		t.Fatalf("parse = %+v, %v", cfg, err)
	}
}

func hasTaskEvent(events []domain.TaskEvent, kind domain.TaskEventKind) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func fileDigest(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: test-owned database path
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return sum[:]
}
