package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// pausableProd is one prod started with a driver: its database, App state
// directory, and the lease root its gated order reads.
type pausableProd struct {
	dbPath, stateDir, leaseRoot string
}

func newPausableProd(t *testing.T, leaseRoot string) pausableProd {
	t.Helper()
	root := t.TempDir()
	p := pausableProd{
		dbPath: filepath.Join(root, "freeside.db"), stateDir: filepath.Join(root, "app-state"), leaseRoot: leaseRoot,
	}
	if err := os.Mkdir(p.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedTemplateStore(t, p.dbPath)
	return p
}

// start runs prod as main does in Claude mode. A test can't compose a real
// driver, so every start here is one the gated order or the pause declines.
func (p pausableProd) start(t *testing.T) *daemon {
	t.Helper()
	h, err := run(t.Context(), nil, config{
		Environment: environmentProd, DBPath: p.dbPath, ListenAddr: "127.0.0.1:0",
		Claude:        &daemoncompose.ClaudeDriverConfig{StateRoot: p.stateDir},
		ProdAuthority: &daemoncompose.ProdAuthorityGate{LeaseRoot: p.leaseRoot},
	})
	if err != nil {
		t.Fatalf("prod did not start: %v", err)
	}
	if h.Workflow != nil {
		t.Fatal("prod composed a driver")
	}
	return h
}

// openCodes returns the codes of the open system_health cards.
func openCodes(t *testing.T, h *daemon) []string {
	t.Helper()
	items, err := h.Attention.ListAttentionItems(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, snapshot := range items {
		if d := snapshot.Item.HealthDiagnostic; d != nil && snapshot.Item.Status == domain.StatusOpen {
			codes = append(codes, d.Code)
		}
	}
	return codes
}

// requireOnlyDriverlessCard checks the one driverless card open is want, and
// whether the pause card is open.
func requireOnlyDriverlessCard(t *testing.T, h *daemon, want string, pauseCard bool) {
	t.Helper()
	codes := openCodes(t, h)
	found, pause := false, false
	for _, code := range codes {
		switch code {
		case want:
			found = true
		case "execution_pause":
			pause = true
		case "execution_not_configured", "app_authority_in_use", "rig_lease_present", "execution_paused":
			t.Errorf("open card %s beside %s", code, want)
		}
	}
	if !found || pause != pauseCard {
		t.Fatalf("cards %v: want %s, pause card open %t", codes, want, pauseCard)
	}
}

func requireRestartRequested(t *testing.T, h *daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := h.Wait(ctx); !errors.Is(err, daemonshell.ErrRestartRequested) {
		t.Fatalf("daemon wait = %v, want a requested restart", err)
	}
}

func closeDaemon(t *testing.T, h *daemon) {
	t.Helper()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func execution(t *testing.T, dbPath string, action executionPauseAction) (stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := runExecutionCommand(t.Context(), executionPauseCommandConfig{DBPath: dbPath, Action: action},
		&out, &errOut); err != nil {
		t.Fatalf("execution %s: %v (stderr %q)", action, err, errOut.String())
	}
	return out.String(), errOut.String()
}

// TestExecutionPauseHandsTheAuthorityToADeputy walks one pause through the
// CLI: prod, here driverless beside a lease, takes the request, releases it
// at once with nothing running, and asks for a restart; the restarted prod
// serves driverless as paused even with the lease gone, holding no lock;
// resume clears the pause online and asks for a restart again; and the next
// start runs the gated order, which now finds the lock free.
func TestExecutionPauseHandsTheAuthorityToADeputy(t *testing.T) {
	t.Parallel()
	lease, leaseRoot, _ := acquireTestRig(t)
	p := newPausableProd(t, leaseRoot)

	h := p.start(t)
	requireOnlyDriverlessCard(t, h, "rig_lease_present", false)
	stdout, _ := execution(t, p.dbPath, executionPauseRequest)
	if !strings.Contains(stdout, `"state":"released"`) {
		t.Fatalf("pause output %q never reported the release", stdout)
	}
	requireRestartRequested(t, h)
	closeDaemon(t, h)

	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	h = p.start(t)
	requireOnlyDriverlessCard(t, h, "execution_paused", true)
	requireLockFree(t, p.stateDir)
	mode, running, err := newProductionRigHost().ProdExecution(t.Context(), p.dbPath)
	if err != nil || !running || mode != executionDisabled {
		t.Fatalf("probe = %q, running %t, %v; want disabled, running", mode, running, err)
	}

	stdout, _ = execution(t, p.dbPath, executionPauseResume)
	var status daemoncompose.ExecutionPauseStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil || status.Pause != nil || !status.Restarting {
		t.Fatalf("resume = %+v, %v; want no pause, restarting", status, err)
	}
	requireRestartRequested(t, h)
	closeDaemon(t, h)

	// The gated order runs again; a "deputy" holding the lock keeps this
	// start driverless, which is as far as a test without a driver goes.
	deputy, err := publish.AcquireAuthorityWriterLock(p.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deputy.Close() }()
	h = p.start(t)
	defer closeDaemon(t, h)
	requireOnlyDriverlessCard(t, h, "app_authority_in_use", false)
}

// TestExecutionPauseResumesOffline: with no daemon running, resume clears a
// released pause and concludes its card, and the next start, still beside a
// lease, comes back driverless for the lease rather than the pause. pause
// and status need a running prod.
func TestExecutionPauseResumesOffline(t *testing.T) {
	t.Parallel()
	_, leaseRoot, _ := acquireTestRig(t)
	p := newPausableProd(t, leaseRoot)
	ctx := t.Context()
	st, err := store.OpenExisting(ctx, p.dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = daemoncompose.RequestExecutionPause(ctx, st, "cli:operator", time.Now())
	if err == nil {
		err = st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.AppendInstanceSettingsRevision(ctx, domain.InstanceSettingsInput{
				Section: domain.InstanceSectionExecutionPause, Value: json.RawMessage(`{"state":"released"}`),
				SetBy: "daemon:execution-pause",
			}, time.Now().UTC())
			return err
		})
	}
	if err := errors.Join(err, st.Close()); err != nil {
		t.Fatal(err)
	}
	for _, action := range []executionPauseAction{executionPauseRequest, executionPauseStatus} {
		if err := runExecutionCommand(ctx, executionPauseCommandConfig{DBPath: p.dbPath, Action: action},
			&bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no daemon is running") {
			t.Fatalf("offline %s = %v, want a refusal", action, err)
		}
	}
	if stdout, _ := execution(t, p.dbPath, executionPauseResume); !strings.Contains(stdout, "cleared the pause") {
		t.Fatalf("offline resume output %q", stdout)
	}
	h := p.start(t)
	defer closeDaemon(t, h)
	requireOnlyDriverlessCard(t, h, "rig_lease_present", false)
}

// TestExecutionPauseRouteServesOnlyAGatedProd: a prod started without a
// driver answers that it has nothing to pause.
func TestExecutionPauseRouteServesOnlyAGatedProd(t *testing.T) {
	t.Parallel()
	_, err := executionPauseInDaemon(t.Context(), nil, executionPauseRequest)
	if err == nil || !strings.Contains(err.Error(), "no execution to pause") {
		t.Fatalf("pause without a driver = %v", err)
	}
	if _, err := executionPauseInDaemon(t.Context(), nil, "halt"); err == nil {
		t.Fatal("unknown action accepted")
	}
	for _, action := range allExecutionPauseActions {
		if !action.valid() {
			t.Fatalf("%s is not valid", action)
		}
	}
	if executionPauseAction("").valid() {
		t.Fatal("empty action is valid")
	}
}

// TestExecutionPauseHandsOverOnlyFromADriverlessProd: the pause command's
// wait ends only when a prod without execution, which holds no writer lock,
// reports the release, never when the prod that released it does while it
// tears down.
func TestExecutionPauseHandsOverOnlyFromADriverlessProd(t *testing.T) {
	t.Parallel()
	released := &domain.ExecutionPauseSettings{State: domain.ExecutionPauseReleased}
	requested := &domain.ExecutionPauseSettings{State: domain.ExecutionPauseRequested}
	for _, tc := range []struct {
		status daemoncompose.ExecutionPauseStatus
		want   bool
	}{
		{daemoncompose.ExecutionPauseStatus{Pause: released}, true},
		{daemoncompose.ExecutionPauseStatus{Pause: released, Executes: true}, false},
		{daemoncompose.ExecutionPauseStatus{Pause: requested}, false},
		{daemoncompose.ExecutionPauseStatus{}, false},
	} {
		if got := executionPauseHandedOver(tc.status); got != tc.want {
			t.Errorf("handed over %+v = %t, want %t", tc.status, got, tc.want)
		}
	}
}

func requireLockFree(t *testing.T, stateDir string) {
	t.Helper()
	lock, err := publish.AcquireAuthorityWriterLock(stateDir)
	if err != nil {
		t.Fatalf("the App authority writer lock is held: %v", err)
	}
	_ = lock.Close()
}

// TestExecutionPauseFailedStartReleasesTheLock: a prod whose gated order
// takes the writer lock but whose start then fails, here because a test
// can't compose a driver, leaves the lock free for a deputy.
func TestExecutionPauseFailedStartReleasesTheLock(t *testing.T) {
	t.Parallel()
	p := newPausableProd(t, t.TempDir())
	h, err := run(t.Context(), nil, config{
		Environment: environmentProd, DBPath: p.dbPath, ListenAddr: "127.0.0.1:0",
		Claude:        &daemoncompose.ClaudeDriverConfig{StateRoot: p.stateDir},
		ProdAuthority: &daemoncompose.ProdAuthorityGate{LeaseRoot: p.leaseRoot},
	})
	if err == nil {
		closeDaemon(t, h)
		t.Fatal("prod composed a driver from a bare state root")
	}
	requireLockFree(t, p.stateDir)
}
