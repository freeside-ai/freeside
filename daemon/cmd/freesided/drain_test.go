package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	drainTestOldBuild domain.Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	drainTestNewBuild domain.Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// TestDrainUsesRunningDaemonAndLiftsOnNewBuild: freesided drain reaches the
// daemon through the control socket and never opens the store itself. A
// restart on the same build keeps the drain in force; the first start of a
// different build lifts it; -cancel lifts one by hand.
func TestDrainUsesRunningDaemonAndLiftsOnNewBuild(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
		t.Error("drain opened the store directly")
		return nil, errors.New("unexpected direct store open")
	}))
	start := func(build domain.Digest) *daemon {
		t.Helper()
		h, err := run(t.Context(), nil, config{
			Environment: environmentEphemeral,
			DBPath:      dbPath, ListenAddr: "127.0.0.1:0",
			FakeDriverEnabled: true,
			seams:             daemoncompose.StartSeams{Build: build},
		})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	stopDaemon := func(h *daemon) {
		t.Helper()
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
	drained := func(label string, action drainAction, want bool) {
		t.Helper()
		readiness, err := runDrainCommand(ctx, drainCommandConfig{DBPath: dbPath, Action: action})
		if err != nil {
			t.Fatalf("%s: drain %s = %v", label, action, err)
		}
		if got := readiness.Drain != nil; got != want {
			t.Fatalf("%s: drain %s readiness = %+v, want drain in force %t", label, action, readiness, want)
		}
	}

	h := start(drainTestOldBuild)
	drained("fresh daemon", drainStatus, false)
	drained("request", drainRequest, true)
	drained("request again", drainRequest, true)
	stopDaemon(h)

	h = start(drainTestOldBuild)
	drained("same build", drainStatus, true)
	stopDaemon(h)

	h = start(drainTestNewBuild)
	drained("new build", drainStatus, false)
	drained("request on the new build", drainRequest, true)
	drained("cancel", drainCancel, false)
	stopDaemon(h)
}

// TestDrainRefusesWithoutDaemon: with no daemon there is nothing to drain,
// and the database is left untouched.
func TestDrainRefusesWithoutDaemon(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	before := fileDigest(t, dbPath)
	for _, action := range allDrainActions {
		_, err := runDrainCommand(t.Context(), drainCommandConfig{DBPath: dbPath, Action: action})
		if err == nil || !strings.Contains(err.Error(), "no daemon is running") {
			t.Fatalf("drain %s without a daemon = %v", action, err)
		}
	}
	if after := fileDigest(t, dbPath); !bytes.Equal(before, after) {
		t.Fatal("drain without a daemon changed the database")
	}
}

func TestParseDrainCommand(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"no db":         {"-cancel"},
		"both actions":  {"-db", "state.db", "-cancel", "-status"},
		"positional":    {"-db", "state.db", "extra"},
		"unknown flags": {"-db", "state.db", "-force"},
	} {
		if _, err := parseDrainCommand(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: parse accepted %v", name, args)
		}
	}
	for args, want := range map[string]drainAction{
		"":        drainRequest,
		"-cancel": drainCancel,
		"-status": drainStatus,
	} {
		argv := []string{"-db", "state.db"}
		if args != "" {
			argv = append(argv, args)
		}
		cfg, err := parseDrainCommand(argv, &bytes.Buffer{})
		if err != nil || cfg != (drainCommandConfig{DBPath: "state.db", Action: want}) {
			t.Fatalf("parse %v = %+v, %v; want %s", argv, cfg, err, want)
		}
	}
	for _, action := range allDrainActions {
		if !action.valid() {
			t.Errorf("registered action %q is invalid", action)
		}
	}
	if drainAction("").valid() {
		t.Error("the zero drain action is valid")
	}
}
