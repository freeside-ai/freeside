package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// storedProdFixture is an installed daemon's root: prod's database and the
// App authority's directories under one supervised root, laid out as main
// lays them out, and a lease root no deputy has used.
type storedProdFixture struct {
	root, dbPath, appState, leaseRoot string
}

func newStoredProdFixture(t *testing.T) storedProdFixture {
	t.Helper()
	root := t.TempDir()
	f := storedProdFixture{
		root: root, dbPath: filepath.Join(root, "daemon", "freeside.db"), appState: filepath.Join(root, "daemon"),
		leaseRoot: t.TempDir(),
	}
	for _, dir := range []string{f.appState, filepath.Join(root, "credentials")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	seedTemplateStore(t, f.dbPath)
	return f
}

// importRunSettings records a complete deputy settings file for
// project-widgets through settings import, as an operator would.
func (f storedProdFixture) importRunSettings(t *testing.T) {
	t.Helper()
	settingsPath, _ := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	if _, err := runSettings(t, t.Context(), "import", "-db", f.dbPath, "-settings", settingsPath,
		"-container-bin", settingsTestContainer); err != nil {
		t.Fatal(err)
	}
}

// set records one section through settings set; an empty project sets the
// instance's.
func (f storedProdFixture) set(t *testing.T, project domain.ProjectID, section, value string) {
	t.Helper()
	args := []string{"set", "-db", f.dbPath, "-section", section, "-value", value}
	if project != "" {
		args = append(args, "-project", string(project))
	}
	if _, err := runSettings(t, t.Context(), args...); err != nil {
		t.Fatal(err)
	}
}

// start runs the installed daemon's start through the retry main runs, so a
// failed composition serves driverless as it does under launchd.
func (f storedProdFixture) start(t *testing.T) *daemon {
	t.Helper()
	h, _, _, err := runWithDriverlessRetry(func() (context.Context, func()) { return t.Context(), func() {} }, config{
		Environment: environmentProd, DBPath: f.dbPath, ListenAddr: "127.0.0.1:0",
		ProdAuthority:   &daemoncompose.ProdAuthorityGate{LeaseRoot: f.leaseRoot},
		StoredExecution: storedExecution(f.root, f.appState, "127.0.0.1:0", nil),
	})
	if err != nil {
		t.Fatalf("the installed daemon did not start: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

// executionCard is the open execution card's reason under want, failing on
// any other open execution card.
func executionCard(t *testing.T, h *daemon, want string) string {
	t.Helper()
	items, err := h.Attention.ListAttentionItems(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	for _, snapshot := range items {
		d := snapshot.Item.HealthDiagnostic
		if d == nil || snapshot.Item.Status != domain.StatusOpen ||
			!strings.HasPrefix(string(snapshot.Item.ID), "system-health-execution-configuration-") {
			continue
		}
		if d.Code != want {
			t.Errorf("open execution card %s, want %s", d.Code, want)
			continue
		}
		reason = snapshot.Item.Reason
	}
	if reason == "" {
		t.Fatalf("no open %s card", want)
	}
	return reason
}

// TestInstalledDaemonServesDriverlessWhenItsSettingsStopIt pins the stored
// half of the start order: each way the settings stop execution starts the
// daemon anyway, composes no driver, raises the card that names what to
// fix, takes and keeps no App authority writer lock, and reports execution
// disabled to a deputy.
func TestInstalledDaemonServesDriverlessWhenItsSettingsStopIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		seed     func(*testing.T, storedProdFixture)
		wantCode string
		wantText []string
	}{
		{name: "no settings", seed: func(*testing.T, storedProdFixture) {}, wantCode: "execution_not_configured"},
		{name: "execution off", seed: func(t *testing.T, f storedProdFixture) {
			f.importRunSettings(t)
			f.set(t, "project-widgets", "enabled", `{"enabled":true}`)
			f.set(t, "", "execution", `{"enabled":false}`)
		}, wantCode: "execution_not_configured"},
		{name: "no project enabled", seed: func(t *testing.T, f storedProdFixture) {
			f.importRunSettings(t)
			f.set(t, "", "execution", `{"enabled":true}`)
		}, wantCode: "no_enabled_project", wantText: []string{"-section enabled"}},
		{name: "two projects enabled", seed: func(t *testing.T, f storedProdFixture) {
			f.importRunSettings(t)
			f.set(t, "", "execution", `{"enabled":true}`)
			f.set(t, "project-widgets", "enabled", `{"enabled":true}`)
			f.set(t, "project-gadgets", "enabled", `{"enabled":true}`)
		}, wantCode: "several_projects_enabled", wantText: []string{"project-gadgets, project-widgets"}},
		{name: "settings the loader refuses", seed: func(t *testing.T, f storedProdFixture) {
			f.set(t, "", "execution", `{"enabled":true}`)
			f.set(t, "project-widgets", "enabled", `{"enabled":true}`)
		}, wantCode: "settings_refused", wantText: []string{"host_tools"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newStoredProdFixture(t)
			tc.seed(t, f)
			h := f.start(t)
			if h.Workflow != nil {
				t.Fatal("the installed daemon composed a driver")
			}
			reason := executionCard(t, h, tc.wantCode)
			for _, want := range tc.wantText {
				if !strings.Contains(reason, want) {
					t.Errorf("card %q lacks %q", reason, want)
				}
			}
			requireLockFree(t, f.appState)
			mode, running, err := newProductionRigHost().ProdExecution(t.Context(), f.dbPath)
			if err != nil || !running || mode != executionDisabled {
				t.Fatalf("probe = %q, running %t, %v; want disabled, running", mode, running, err)
			}
		})
	}
}

// TestInstalledDaemonRunsTheGatedOrderOnItsSettings pins that settings which
// choose a project reach #3204's order: a lost lock and a found lease each
// serve driverless with that order's card and run no start check (the pause
// has its own test), and a start the order lets through runs the start
// checks, where a missing container tool serves driverless with a card
// naming the check, the path, and the fix, with the lock released before it
// serves.
func TestInstalledDaemonRunsTheGatedOrderOnItsSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		lockHeld bool
		lease    bool
		wantCode string
	}{
		{name: "a deputy holds the lock", lockHeld: true, wantCode: "app_authority_in_use"},
		{name: "a lease is present", lease: true, wantCode: "rig_lease_present"},
		{name: "the container tool is missing", wantCode: "host_check_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newStoredProdFixture(t)
			f.importRunSettings(t)
			f.set(t, "", "execution", `{"enabled":true}`)
			f.set(t, "project-widgets", "enabled", `{"enabled":true}`)
			// No container tool exists here, so a start the order lets
			// through fails its first start check before it launches
			// anything; a declined start never reaches that check.
			f.set(t, "", "host_tools", `{"container":"/nonexistent/freeside-test/container"}`)
			if tc.lease {
				_, f.leaseRoot, _ = acquireTestRig(t)
			}
			if tc.lockHeld {
				lock, err := publish.AcquireAuthorityWriterLock(f.appState)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
			}
			h := f.start(t)
			if h.Workflow != nil {
				t.Fatal("the installed daemon composed a driver")
			}
			reason := executionCard(t, h, tc.wantCode)
			if checked := strings.Contains(reason, "host_tools.container"); checked != (tc.wantCode == "host_check_failed") {
				t.Errorf("card %q: names the container check %t, want only when the order let the start through", reason, checked)
			}
			if tc.wantCode == "host_check_failed" && (!strings.Contains(reason, "/nonexistent/freeside-test/container") ||
				!strings.Contains(reason, "freesided settings set -section host_tools")) {
				t.Errorf("card %q does not name the path and the fix", reason)
			}
			if !tc.lockHeld {
				requireLockFree(t, f.appState)
			}
			mode, running, err := newProductionRigHost().ProdExecution(t.Context(), f.dbPath)
			if err != nil || !running || mode != executionDisabled {
				t.Fatalf("probe = %q, running %t, %v; want disabled, running", mode, running, err)
			}
		})
	}
}

// TestInstalledDaemonReadsItsClientPolicyFromItsSettings pins that the
// installed daemon reads the client-task policy file its
// manual_submission_config section names, as the deputy reads its settings
// file's: a file that does not read serves driverless with a card naming the
// section and its fix, before any host check runs, and one that reads lets the
// start go on to the host checks.
func TestInstalledDaemonReadsItsClientPolicyFromItsSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, wantCheck string
	}{
		{name: "the file is missing", wantCheck: "manual_submission_config"},
		{name: "the file reads", body: `{"version":1,"projects":[]}`, wantCheck: "host_tools.container"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newStoredProdFixture(t)
			f.importRunSettings(t)
			f.set(t, "", "execution", `{"enabled":true}`)
			f.set(t, "project-widgets", "enabled", `{"enabled":true}`)
			f.set(t, "", "host_tools", `{"container":"/nonexistent/freeside-test/container"}`)
			path := filepath.Join(t.TempDir(), "manual.json")
			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f.set(t, "", "manual_submission_config", strconv.Quote(path))
			h := f.start(t)
			if h.Workflow != nil {
				t.Fatal("the installed daemon composed a driver")
			}
			reason := executionCard(t, h, "host_check_failed")
			if !strings.Contains(reason, tc.wantCheck) {
				t.Errorf("card %q does not name %s", reason, tc.wantCheck)
			}
			if tc.body == "" && (!strings.Contains(reason, path) ||
				!strings.Contains(reason, "freesided settings set -section manual_submission_config")) {
				t.Errorf("card %q does not name the file and the fix", reason)
			}
		})
	}
}

// TestInstalledDaemonPausesAndResumesThroughItsControlSocket pins that the
// installed daemon serves execution pause and resume like a prod started
// with -driver claude, and that resume reruns the start order: a pause taken
// beside a lease restarts it paused once the lease is gone, and resume
// restarts it into the order, which here reaches a start check that fails.
func TestInstalledDaemonPausesAndResumesThroughItsControlSocket(t *testing.T) {
	t.Parallel()
	f := newStoredProdFixture(t)
	f.importRunSettings(t)
	f.set(t, "", "execution", `{"enabled":true}`)
	f.set(t, "project-widgets", "enabled", `{"enabled":true}`)
	f.set(t, "", "host_tools", `{"container":"/nonexistent/freeside-test/container"}`)
	lease, leaseRoot, _ := acquireTestRig(t)
	f.leaseRoot = leaseRoot

	h := f.start(t)
	executionCard(t, h, "rig_lease_present")
	if stdout, _ := execution(t, f.dbPath, executionPauseRequest); !strings.Contains(stdout, `"state":"released"`) {
		t.Fatalf("pause output %q never reported the release", stdout)
	}
	requireRestartRequested(t, h)
	closeDaemon(t, h)
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	h = f.start(t)
	executionCard(t, h, "execution_paused")
	requireLockFree(t, f.appState)
	execution(t, f.dbPath, executionPauseResume)
	requireRestartRequested(t, h)
	closeDaemon(t, h)

	h = f.start(t)
	if reason := executionCard(t, h, "host_check_failed"); !strings.Contains(reason, "host_tools.container") {
		t.Errorf("card %q does not name the failed check", reason)
	}
	requireLockFree(t, f.appState)
}

// TestInstalledDaemonReadsItsEvidenceWithoutADriver pins the store's
// open-time policy: evidence a run recorded under a project's approved
// recipe stays readable on a start that runs no driver, even with the
// project turned off, because the recipe comes from the settings and not
// from a driver.
func TestInstalledDaemonReadsItsEvidenceWithoutADriver(t *testing.T) {
	t.Parallel()
	f := newStoredProdFixture(t)
	f.importRunSettings(t)
	recipe := domain.Digest("sha256:" + strings.Repeat("b", 64)) // deputySettingsFixture's approved_recipe
	approved := map[domain.Digest]bool{recipe: true}
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "artifact-stored-recipe", Type: domain.ArtifactKindVerifyLog,
		Digest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "inv-stored-recipe",
			HeadBinding: domain.HeadIndependent, VerificationRecipeDigest: &recipe,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: testRunEvidenceMetadata(1),
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	if !artifact.PublishEligible {
		t.Fatal("fixture artifact is not publish-eligible, so it proves nothing about the recipe")
	}
	seed, _, err := openStoreWithTopicKey(t.Context(), f.dbPath, store.Options{ApprovedRecipes: approved})
	if err != nil {
		t.Fatal(err)
	}
	writeErr := seed.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutArtifact(t.Context(), artifact) })
	if err := errors.Join(writeErr, seed.Close()); err != nil {
		t.Fatal(err)
	}

	h := f.start(t)
	executionCard(t, h, "execution_not_configured")
	if err := h.Store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetArtifact(t.Context(), artifact.ID)
		return err
	}); err != nil {
		t.Fatalf("a driverless start cannot read evidence under the project's recipe: %v", err)
	}
}

// TestInstalledDaemonInspectsARunAsTheDeputyDoes: a run's record on the
// installed daemon's root reads as it does on a deputy's (#2083). The record
// names no composition input, so what could differ is the store's recipe
// policy. Served, the daemon approves the recipe the project's settings
// record, as a deputy approves its settings file's; stopped, the direct read
// approves what -approved-recipe names, as on a deputy root, and its note
// says where the installed root keeps the recipe. A test cannot compose a
// real driver, so the run and its evidence are written directly.
func TestInstalledDaemonInspectsARunAsTheDeputyDoes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newStoredProdFixture(t)
	recipe := domain.Digest("sha256:" + strings.Repeat("b", 64)) // deputySettingsFixture's approved_recipe
	seed, _, err := openStoreWithTopicKey(ctx, f.dbPath, store.Options{ApprovedRecipes: map[domain.Digest]bool{recipe: true}})
	if err != nil {
		t.Fatal(err)
	}
	const own, runID = domain.ItemID("execution-failure-evidence-installed"), domain.RunID("run-installed-evidence")
	putRecipeEvidenceItem(t, seed, recipe, runID, own)
	id := taskOfRun(t, seed, runID)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	f.importRunSettings(t)

	h, _, _, err := runWithDriverlessRetry(func() (context.Context, func()) { return ctx, func() {} }, config{
		Environment: environmentProd, DBPath: f.dbPath, ListenAddr: "127.0.0.1:0",
		ProdAuthority:   &daemoncompose.ProdAuthorityGate{LeaseRoot: f.leaseRoot},
		StoredExecution: storedExecution(f.root, f.appState, "127.0.0.1:0", nil),
	})
	if err != nil {
		t.Fatalf("the installed daemon did not start: %v", err)
	}
	args := []string{"task", "-db", f.dbPath, "-task", string(id)}
	served := inspect[taskRecord](t, ctx, args...)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if item := recordItem(served, own); item == nil || item.Error != nil {
		t.Fatalf("served item = %+v, want the evidence read under the settings' recipe", item)
	}

	var stdout, stderr bytes.Buffer
	if err := runInspectCommand(ctx, args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if note := stderr.String(); !strings.Contains(note, "freesided settings show -project") {
		t.Fatalf("stopped-root stderr = %q, want the note to name the installed root's recipe", note)
	}
	stdout.Reset()
	stderr.Reset()
	if err := runInspectCommand(ctx, append(args, "-approved-recipe", string(recipe)), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var direct taskRecord
	if err := strictjson.Decode(stdout.Bytes(), &direct, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(comparableRecord(t, direct), comparableRecord(t, served)) || stderr.Len() != 0 {
		t.Fatalf("stopped-root record differs from the served one, or stderr = %q\ndirect: %s\nserved: %s",
			stderr.String(), comparableRecord(t, direct), comparableRecord(t, served))
	}
}

// TestInstalledDaemonServesLoopbackWhenTailscaleIsLate: an installed daemon
// whose -listen names a Tailscale address Tailscale does not report starts
// anyway, on loopback at the same port, with the advisory card that says the
// phone cannot reach it; a later start that binds its address concludes the
// card. The wait itself is TestListenWaitingWaitsForTailscaleThenServesLoopback's.
func TestInstalledDaemonServesLoopbackWhenTailscaleIsLate(t *testing.T) {
	t.Parallel()
	f := newStoredProdFixture(t)
	start := func(listen string) *daemon {
		t.Helper()
		h, _, _, err := runWithDriverlessRetry(func() (context.Context, func()) { return t.Context(), func() {} }, config{
			Environment: environmentProd, DBPath: f.dbPath, ListenAddr: listen,
			ProdAuthority:   &daemoncompose.ProdAuthorityGate{LeaseRoot: f.leaseRoot},
			StoredExecution: storedExecution(f.root, f.appState, listen, nil),
			ListenWait: &daemoncompose.ListenWait{TailscaleIPs: func() ([]netip.Addr, error) {
				return nil, errors.New("tailscale ip: not connected")
			}},
		})
		if err != nil {
			t.Fatalf("the installed daemon did not start on %s: %v", listen, err)
		}
		return h
	}
	listenerCards := func(h *daemon) (open []string, concluded int) {
		t.Helper()
		items, err := h.Attention.ListAttentionItems(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, snapshot := range items {
			d := snapshot.Item.HealthDiagnostic
			if d == nil || d.Code != daemoncompose.ListenerFallbackCode {
				continue
			}
			if snapshot.Item.Status == domain.StatusOpen {
				open = append(open, snapshot.Item.Reason)
			} else {
				concluded++
			}
		}
		return open, concluded
	}

	late := start("100.64.0.7:0")
	bound := strings.TrimPrefix(late.Readiness().APIURL, "http://")
	open, _ := listenerCards(late)
	if err := late.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(bound, "127.0.0.1:") {
		t.Fatalf("bound %s, want loopback", bound)
	}
	if len(open) != 1 || !strings.Contains(open[0], bound) || !strings.Contains(open[0], "100.64.0.7:0") ||
		!strings.Contains(open[0], "launchctl kickstart") {
		t.Fatalf("open listener cards = %q, want one naming the bound and configured addresses and the restart", open)
	}

	onTime := start("127.0.0.1:0")
	open, concluded := listenerCards(onTime)
	if err := onTime.Close(); err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 || concluded != 1 {
		t.Fatalf("after a start on its address: open %q, concluded %d; want none open and one concluded", open, concluded)
	}
}

// TestInstalledDaemonAdvertisesExecutionOnlyWhileItsDriverRuns pins what a
// deputy reads from the installed daemon: GET /execution answers claude
// while a driver composed from the settings runs, which checkProdDriverless
// and the preflight's prod_execution check both refuse, and disabled when
// the start ran none. A test cannot compose a real driver, so this serves the
// installed daemon's control routes over each state directly.
func TestInstalledDaemonAdvertisesExecutionOnlyWhileItsDriverRuns(t *testing.T) {
	t.Parallel()
	for _, state := range []daemoncompose.ExecutionState{
		daemoncompose.ExecutionConfigured, daemoncompose.ExecutionCompositionFailed,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			f := newStoredProdFixture(t)
			st, _, err := openStoreWithTopicKey(t.Context(), f.dbPath, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			control, err := newPairingControl(f.dbPath, f.appState)
			if err != nil {
				t.Fatal(err)
			}
			installed := config{StoredExecution: storedExecution(f.root, f.appState, "127.0.0.1:0", nil)}
			socket := pairingControlSocket{pairing: control, environment: environmentProd, execution: installed.execution()}
			socket.Configure("http://127.0.0.1:7331", nil)
			socket.RegisterRoutes(daemoncompose.ControlRouteInputs{Store: st, Execution: state})
			served := make(chan error, 1)
			go func() { served <- socket.Serve() }()
			t.Cleanup(func() {
				_ = socket.Close()
				if err := <-served; !errors.Is(err, http.ErrServerClosed) {
					t.Error(err)
				}
			})
			if err := socket.Publish(); err != nil {
				t.Fatal(err)
			}

			probe := newProductionRigHost().ProdExecution
			want := executionDisabled
			if state.Runs() {
				want = executionClaude
			}
			if mode, running, err := probe(t.Context(), f.dbPath); err != nil || !running || mode != want {
				t.Fatalf("probe = %q, running %t, %v; want %q, running", mode, running, err, want)
			}
			err = checkProdDriverless(t.Context(), probe, f.appState)
			manifest := compositionManifest{Checks: []compositionCheck{{Name: "prod_execution", Status: compositionNotRun}}}
			passed := evaluateProdExecution(t.Context(), &manifest, preflightConfig{PublicationStateDir: f.appState},
				newProductionPreflightEnvironment())
			if state.Runs() {
				if err == nil || !strings.Contains(err.Error(), "-driver claude") || passed {
					t.Fatalf("beside an executing installed daemon: checkProdDriverless %v, prod_execution passed %t",
						err, passed)
				}
				return
			}
			if err != nil || !passed {
				t.Fatalf("beside a driverless installed daemon: checkProdDriverless %v, prod_execution passed %t",
					err, passed)
			}
		})
	}
}
