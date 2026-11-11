package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestReleaseVerificationMemoryNeedsADaemon: the release runs inside the
// daemon, so with none running the command refuses rather than releasing from
// the store it could open.
func TestReleaseVerificationMemoryNeedsADaemon(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	err := runReleaseVerificationMemoryCommand(t.Context(), []string{"-db", dbPath, "-task", "task-1"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no daemon is running") {
		t.Fatalf("release with no daemon = %v, want a refusal", err)
	}
}

// TestReleaseVerificationMemoryReportsABusyLane: inside the daemon, a lane
// the engine reports busy is reported as busy, and the flags reach the
// engine as given.
func TestReleaseVerificationMemoryReportsABusyLane(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	canonical, err := canonicalDatabasePath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var gotTask domain.TaskID
	var gotGone bool
	ctx := context.WithValue(t.Context(), daemonStoreContextKey{}, daemonStoreContext{
		dbPath: canonical,
		releaseVerificationMemory: func(_ context.Context, task domain.TaskID, gone bool) (engine.VerificationMemoryRelease, error) {
			gotTask, gotGone = task, gone
			return engine.VerificationMemoryRelease{}, engine.ErrPublicationLaneBusy
		},
	})
	err = runReleaseVerificationMemoryCommand(ctx, []string{"-db", dbPath, "-task", "task-1", "-command-gone"}, io.Discard, io.Discard)
	if !errors.Is(err, engine.ErrPublicationLaneBusy) || gotTask != "task-1" || !gotGone {
		t.Fatalf("release = %v for task %q, command gone %v; want busy for task-1 with -command-gone", err, gotTask, gotGone)
	}
}

// TestReleaseVerificationMemoryThroughARunningDaemon: the command reaches
// the running daemon over the control socket instead of opening the store.
// An ephemeral daemon composes no publication lane, so the daemon's own
// refusal comes back rather than the no-daemon one.
func TestReleaseVerificationMemoryThroughARunningDaemon(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
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
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("unexpected direct store open")
		}))
	err = runReleaseVerificationMemoryCommand(ctx, []string{"-db", dbPath, "-task", "task-1"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no publication lane") {
		t.Fatalf("release through an ephemeral daemon = %v, want its no-lane refusal", err)
	}
}

func TestReleaseVerificationMemoryFlags(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"-task", "task-1"},
		{"-db", "freeside.db"},
		{"-db", "freeside.db", "-task", "task-1", "extra"},
	} {
		if _, err := parseReleaseVerificationMemoryConfig(args, io.Discard); err == nil {
			t.Errorf("%v parsed, want a refusal", args)
		}
	}
}
