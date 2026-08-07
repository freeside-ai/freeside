package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunDrainsBackgroundWorkersBeforeClosingStoreOnStartupFailure(t *testing.T) {
	root := t.TempDir()
	var logs lockedBuffer
	logger, err := newLogger(&logs, defaultLogLevel)
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	startupErr := errors.New("injected post-background-start failure")
	const signalRestoredLog = "signal disposition restored"
	var logsAtStoreClose string
	cfg := config{
		DBPath:            filepath.Join(root, "freeside.db"),
		FakeDriverDir:     filepath.Join(root, "driver"),
		ListenAddr:        "127.0.0.1:0",
		ReconcileInterval: time.Millisecond,
		Logger:            logger,
		afterBackgroundStart: func() error {
			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				if strings.Contains(logs.String(), "reconcile loop started") {
					return startupErr
				}
				select {
				case <-deadline.C:
					return errors.New("reconcile loop did not start")
				case <-ticker.C:
				}
			}
		},
		beforeStoreClose: func() {
			logsAtStoreClose = logs.String()
		},
	}

	h, err := run(t.Context(), func() {
		if _, err := fmt.Fprintln(&logs, signalRestoredLog); err != nil {
			t.Errorf("record signal restoration: %v", err)
		}
	}, cfg)
	if h != nil {
		_ = h.Close()
		t.Fatal("run returned a daemon after the injected startup failure")
	}
	if !errors.Is(err, startupErr) {
		t.Fatalf("run error = %v, want %v", err, startupErr)
	}
	if !strings.Contains(logsAtStoreClose, "reconcile loop stopped") {
		t.Fatalf("store close began before the reconcile loop stopped:\n%s", logsAtStoreClose)
	}
	signalRestoredAt := strings.Index(logsAtStoreClose, signalRestoredLog)
	reconcileStoppedAt := strings.Index(logsAtStoreClose, "reconcile loop stopped")
	if signalRestoredAt == -1 || signalRestoredAt > reconcileStoppedAt {
		t.Fatalf("signal disposition was not restored before the worker drain:\n%s", logsAtStoreClose)
	}
	gotLogs := logs.String()
	for _, unexpected := range []string{"reconcile pass failed", "sql: database is closed"} {
		if strings.Contains(gotLogs, unexpected) {
			t.Fatalf("background worker used the closed store (%q):\n%s", unexpected, gotLogs)
		}
	}
}

func TestRunReturnsAfterPostBackgroundStartFailure(t *testing.T) {
	root := t.TempDir()
	startupErr := errors.New("injected post-background-start failure")
	result := make(chan struct {
		h   *daemon
		err error
	}, 1)
	go func() {
		h, err := run(t.Context(), nil, config{
			DBPath:               filepath.Join(root, "freeside.db"),
			FakeDriverDir:        filepath.Join(root, "driver"),
			ListenAddr:           "127.0.0.1:0",
			ReconcileInterval:    time.Millisecond,
			afterBackgroundStart: func() error { return startupErr },
		})
		result <- struct {
			h   *daemon
			err error
		}{h: h, err: err}
	}()

	select {
	case got := <-result:
		if got.h != nil {
			_ = got.h.Close()
			t.Fatal("run returned a daemon after the injected startup failure")
		}
		if !errors.Is(got.err, startupErr) {
			t.Fatalf("run error = %v, want %v", got.err, startupErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run hung while draining a failed startup")
	}
}

func TestRunServesSignetAndStops(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := run(ctx, nil, config{
		DBPath:        filepath.Join(root, "freeside.db"),
		FakeDriverDir: filepath.Join(root, "driver"),
		ListenAddr:    "127.0.0.1:0", ReconcileInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(h.readiness().APIURL + "/sync/revision")
	if err != nil {
		t.Fatalf("GET /sync/revision: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /sync/revision status = %d, want 401 from the live authorizer", response.StatusCode)
	}

	cancel()
	if err := h.Wait(ctx); err != nil {
		t.Fatalf("Wait after cancellation: %v", err)
	}
}

func TestRunConvergesLegacyFakePublicationBeforeStartingScheduler(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	cfg := config{
		DBPath: dbPath, FakeDriverDir: filepath.Join(root, "driver"),
		ListenAddr: "127.0.0.1:0", ReconcileInterval: 10 * time.Millisecond,
	}
	cleanCtx, stopClean := context.WithCancel(context.Background())
	clean, err := run(cleanCtx, nil, cfg)
	if err != nil {
		stopClean()
		t.Fatal(err)
	}
	stopClean()
	if err := errors.Join(clean.Wait(context.Background()), clean.Close()); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	const key = "engine.fake_publication/legacy-invalid"
	seedErr := st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		if _, _, err := tx.EnqueueOutbox(
			context.Background(), key, engine.FakePublicationTaskKind, []byte(`{}`),
		); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(context.Background(), key)
	})
	if err := errors.Join(seedErr, st.Close()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := run(ctx, nil, cfg)
	if err == nil {
		_ = h.Close()
		t.Fatal("run started scheduler before rejecting malformed legacy publication history")
	}
	if !strings.Contains(err.Error(), "converge legacy fake-publication policies") {
		t.Fatalf("run error = %v, want synchronous legacy convergence", err)
	}
}

// A durable row this binary cannot reconstruct, the shape a downgrade leaves
// behind, must not stop the daemon from starting: startup is the one pass an
// operator cannot retry past, and a daemon that will not start cannot be
// upgraded in place (#430). Every attempt starts, and backup health carries
// the refusal that holds unattended work.
func TestRunStartsWithADurableRowItCannotReconstruct(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	cfg := config{
		DBPath:        dbPath,
		FakeDriverDir: filepath.Join(root, "driver"),
		ListenAddr:    "127.0.0.1:0", ReconcileInterval: 10 * time.Millisecond,
	}
	start := func(t *testing.T, attempt string) (*daemon, context.CancelFunc) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		h, err := run(ctx, nil, cfg)
		if err != nil {
			cancel()
			t.Fatalf("%s: run: %v", attempt, err)
		}
		return h, cancel
	}
	stop := func(t *testing.T, attempt string, h *daemon, cancel context.CancelFunc) {
		t.Helper()
		cancel()
		if err := h.Wait(context.Background()); err != nil {
			t.Fatalf("%s: Wait after cancellation: %v", attempt, err)
		}
		if err := h.Close(); err != nil {
			t.Fatalf("%s: Close: %v", attempt, err)
		}
	}

	// Establish the state a newer daemon leaves behind: a complete store, a
	// proven checkpoint, and then a durable row of a kind this binary has no
	// extractor for.
	h, cancel := start(t, "clean start")
	stop(t, "clean start", h, cancel)
	seed, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatalf("open store for seeding: %v", err)
	}
	seedErr := seed.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			context.Background(), "future-1", "backup.kind-this-binary-lacks", []byte("payload"))
		return err
	})
	if err := errors.Join(seedErr, seed.Close()); err != nil {
		t.Fatalf("seed unreadable durable row: %v", err)
	}

	for _, attempt := range []string{"first start", "restart"} {
		h, cancel := start(t, attempt)
		health, err := h.store.BackupHealth(context.Background())
		if err != nil {
			cancel()
			t.Fatalf("%s: BackupHealth: %v", attempt, err)
		}
		if health.ArtifactClosure != domain.BackupHealthUnhealthy {
			cancel()
			t.Fatalf("%s: closure = %q, want unhealthy", attempt, health.ArtifactClosure)
		}
		if err := health.RequireHealthy(); err == nil {
			cancel()
			t.Fatalf("%s: unattended admission stayed open", attempt)
		}
		stop(t, attempt, h, cancel)
	}
}

func TestHoldRetryableClaudeRecovery(t *testing.T) {
	t.Parallel()
	for _, held := range []error{
		fmt.Errorf("journal unavailable: %w", claude.ErrRecoveryRetryable),
		fmt.Errorf("encrypted checkpoint pending: %w", domain.ErrCheckpointNotEncrypted),
		fmt.Errorf("checkpoint drift: %w", domain.ErrCheckpointNotCurrent),
		fmt.Errorf("conformance drift: %w", domain.ErrAdmissionConfigurationMismatch),
	} {
		if err := holdRetryableClaudeRecovery(held); err != nil {
			t.Fatalf("held recovery remained startup-fatal: %v", err)
		}
	}
	permanent := errors.New("intent authentication failed")
	if err := holdRetryableClaudeRecovery(permanent); !errors.Is(err, permanent) {
		t.Fatalf("permanent recovery error = %v, want %v", err, permanent)
	}
}

func TestScheduledDoctorPassRefreshesConformanceBeforeReporting(t *testing.T) {
	t.Parallel()
	t.Run("reports failures", func(t *testing.T) {
		var calls []string
		conformanceErr := errors.New("conformance failed")
		doctorErr := errors.New("doctor failed")
		err := runScheduledDoctorPass(
			context.Background(),
			func(context.Context) error {
				calls = append(calls, "conformance")
				return conformanceErr
			},
			func(context.Context) error {
				calls = append(calls, "doctor")
				return doctorErr
			},
		)
		if !slices.Equal(calls, []string{"conformance", "doctor"}) {
			t.Fatalf("calls = %v, want conformance then doctor", calls)
		}
		if !errors.Is(err, conformanceErr) || !errors.Is(err, doctorErr) {
			t.Fatalf("error = %v, want both conformance and doctor failures", err)
		}
	})
	t.Run("cancellation is shutdown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		err := runScheduledDoctorPass(
			ctx,
			func(context.Context) error {
				cancel()
				return context.Canceled
			},
			func(context.Context) error {
				return context.Canceled
			},
		)
		if err != nil {
			t.Fatalf("canceled pass = %v, want graceful shutdown", err)
		}
	})
}

func TestStoreConformanceRecorderPersistsConfigurationBoundPass(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome: domain.ConformancePassed,
		Capabilities: domain.NewCapabilitySnapshot(
			exec.CapDetachableWorkspace,
			exec.CapPostExitExport,
			exec.CapReadOnlyRemount,
			exec.CapNetworklessExport,
			exec.CapEnforcedProviderEgress,
		),
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		ProvedAt:            time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	if err := (storeConformanceRecorder{store: st}).RecordBackendConformance(ctx, record); err != nil {
		t.Fatalf("RecordBackendConformance: %v", err)
	}

	var (
		got   domain.BackendConformance
		found bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, found, err = tx.LatestBackendConformance(
			ctx, domain.BackendFreshVMReadOnlyVolumeHandoff,
		)
		return err
	}); err != nil {
		t.Fatalf("LatestBackendConformance: %v", err)
	}
	if !found || got.Generation == 0 || got.ConfigurationDigest != record.ConfigurationDigest ||
		got.Outcome != domain.ConformancePassed {
		t.Fatalf("stored conformance = (%+v, found=%v), want configuration-bound pass", got, found)
	}
}

func TestResolvedPathAllowlistBindsTheAdmittedPolicy(t *testing.T) {
	t.Parallel()
	newPolicy := func(t *testing.T, key, value string) domain.ResolvedPolicy {
		t.Helper()
		policy, err := domain.NewResolvedPolicy("run-policy", []domain.PolicyKey{{
			Key: key, Value: value,
			Provenance: domain.KeyProvenance{
				Source: domain.ProvenanceOverride,
				Digest: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
			},
		}})
		if err != nil {
			t.Fatalf("NewResolvedPolicy: %v", err)
		}
		return policy
	}

	canonical := newPolicy(t, "paths", "daemon/**, docs/**")
	got, err := resolvedPathAllowlist(
		canonical, canonical.Digest, []string{"daemon/**", "docs/**"},
	)
	if err != nil {
		t.Fatalf("canonical allowlist: %v", err)
	}
	if !slices.Equal(got, []string{"daemon/**", "docs/**"}) {
		t.Fatalf("canonical allowlist = %q", got)
	}
	missingPaths := newPolicy(t, "egress", "provider_only")
	matchEverything := newPolicy(t, "paths", "**/*")

	// A digest substitution is record corruption and must stay fatal. Every
	// other refusal compares the durable policy against this daemon's own
	// -allowed-paths, so it must be a mutable policy verdict the engine holds
	// on: dispatch returns the error up to Engine.Run, and a fatal one there
	// ends the reconcile loop, so a single mismatched work item would exit
	// the daemon on every restart with no way to reconfigure out of it.
	tests := []struct {
		name       string
		policy     domain.ResolvedPolicy
		digest     domain.Digest
		configured []string
		want       error
		holds      bool
	}{
		{"digest substitution", canonical, domain.Digest("sha256:" + strings.Repeat("cd", 32)), []string{"daemon/**", "docs/**"}, domain.ErrParentKeyMismatch, false},
		{"broader daemon configuration", canonical, canonical.Digest, []string{"daemon/**", "docs/**", "scripts/**"}, domain.ErrPathBoundaryMismatch, true},
		{"different run policy", canonical, canonical.Digest, []string{"daemon/**"}, domain.ErrPathBoundaryMismatch, true},
		{"reordered run policy", canonical, canonical.Digest, []string{"docs/**", "daemon/**"}, domain.ErrPathBoundaryMismatch, true},
		{"missing paths", missingPaths, missingPaths.Digest, []string{"daemon/**", "docs/**"}, domain.ErrPathBoundaryMismatch, true},
		{"match everything", matchEverything, matchEverything.Digest, []string{"**/*"}, domain.ErrPathBoundaryMismatch, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolvedPathAllowlist(tc.policy, tc.digest, tc.configured)
			if !errors.Is(err, tc.want) {
				t.Fatalf("resolvedPathAllowlist error = %v, want %v", err, tc.want)
			}
			if got := engine.MutableAdmissionPolicyRefusal(err); got != tc.holds {
				t.Errorf("engine holds dispatch = %v, want %v (fatal here exits the daemon)", got, tc.holds)
			}
		})
	}
}

// A policy carrying no enforceable boundary is refused at the door. The start
// gate can only hold on it, and no -allowed-paths value satisfies a policy
// with no paths key, so a durable run built from one would be held forever.
func TestSubmittedPathBoundaryRefusesAnUnenforceablePolicy(t *testing.T) {
	t.Parallel()
	key := func(t *testing.T, name, value string) domain.ResolvedPolicy {
		t.Helper()
		policy, err := domain.NewResolvedPolicy("run-boundary", []domain.PolicyKey{{
			Key: name, Value: value,
			Provenance: domain.KeyProvenance{
				Source: domain.ProvenanceOverride,
				Digest: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
			},
		}})
		if err != nil {
			t.Fatalf("NewResolvedPolicy: %v", err)
		}
		return policy
	}
	if err := submittedPathBoundary(key(t, "paths", "daemon/**, docs/**")); err != nil {
		t.Fatalf("explicit declared paths: %v", err)
	}
	for _, tc := range []struct{ name, keyName, value string }{
		{"no paths key", "egress", "provider_only"},
		{"match everything", "paths", "**/*"},
		{"leading glob segment", "paths", "*/**"},
		{"empty", "paths", ""},
		{"malformed glob", "paths", "daemon/[abc"},
		{"parent escape", "paths", "../outside/**"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := submittedPathBoundary(
				key(t, tc.keyName, tc.value),
			); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
				t.Errorf("submittedPathBoundary(%q) = %v, want ErrPathBoundaryMismatch",
					tc.value, err)
			}
		})
	}
}

type recordingSessionCloser struct {
	calls int
	err   error
}

func (c *recordingSessionCloser) Close(context.Context) error {
	c.calls++
	return c.err
}

func TestStartupSessionCleanupTransfersOnlyAfterSuccess(t *testing.T) {
	t.Parallel()
	closeErr := errors.New("session close failed")
	failed := &recordingSessionCloser{err: closeErr}
	stopped := 0
	stop := func() { stopped++ }
	if err := closeStartupSessions(false, failed, stop); !errors.Is(err, closeErr) {
		t.Fatalf("failed-start cleanup = %v, want %v", err, closeErr)
	}
	if failed.calls != 1 {
		t.Fatalf("failed-start close calls = %d, want 1", failed.calls)
	}
	// A failed startup tears down a credential-bearing session while
	// signal.NotifyContext is still registered; restoring signal disposition
	// first is what keeps a second SIGTERM able to end a wedged lease cleanup.
	if stopped != 1 {
		t.Fatalf("failed-start signal restore calls = %d, want 1", stopped)
	}

	succeeded := &recordingSessionCloser{}
	if err := closeStartupSessions(true, succeeded, stop); err != nil {
		t.Fatalf("successful-start cleanup = %v", err)
	}
	if succeeded.calls != 0 {
		t.Fatalf("successful-start close calls = %d, want daemon ownership", succeeded.calls)
	}
	// A successful startup leaves signal handling in place: serve owns the
	// stop-before-close sequence for the running daemon's teardown.
	if stopped != 1 {
		t.Fatalf("successful-start signal restore calls = %d, want no additional restore", stopped)
	}

	// With no credential-bearing closer (the fake-driver lane), there is
	// nothing that can wedge, so signal disposition is left untouched.
	if err := closeStartupSessions(false, nil, stop); err != nil {
		t.Fatalf("failed-start with no closer = %v", err)
	}
	if stopped != 1 {
		t.Fatalf("no-closer signal restore calls = %d, want no restore", stopped)
	}
}

func TestRunPairsFreshDevice(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := run(ctx, nil, config{
		DBPath: filepath.Join(root, "freeside.db"), ListenAddr: "127.0.0.1:0",
		ReconcileInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ready := h.readiness()
	if ready.PairingCode == "" {
		t.Fatal("readiness carried no startup pairing code")
	}
	payload, err := json.Marshal(map[string]string{
		"pairing_code": ready.PairingCode, "display_name": "Fresh device",
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	response, err := http.Post(ready.APIURL+"/pairing", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /pairing: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("POST /pairing status = %d body=%s, want 201", response.StatusCode, body)
	}
	var grant struct {
		DeviceToken      string `json:"device_token"`
		NtfySubscription struct {
			ServerURL string `json:"server_url"`
			Topic     string `json:"topic"`
		} `json:"ntfy_subscription"`
	}
	if err := json.NewDecoder(response.Body).Decode(&grant); err != nil {
		t.Fatalf("decode pairing grant: %v", err)
	}
	if grant.DeviceToken == "" || grant.NtfySubscription.Topic == "" ||
		grant.NtfySubscription.ServerURL != defaultNtfyURL {
		t.Fatalf("pairing grant = %#v, want token and hosted ntfy capability", grant)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ready.APIURL+"/sync/revision", nil)
	if err != nil {
		t.Fatalf("new authenticated request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+grant.DeviceToken)
	authorized, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("authenticated GET /sync/revision: %v", err)
	}
	defer func() { _ = authorized.Body.Close() }()
	if authorized.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /sync/revision status = %d, want 200", authorized.StatusCode)
	}
}

func TestTopicKeyPersistsAndRejectsUntrustedFiles(t *testing.T) {
	t.Run("persists privately", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		first, err := loadOrCreateTopicKey(dbPath, false)
		if err != nil {
			t.Fatalf("create topic key: %v", err)
		}
		second, err := loadOrCreateTopicKey(dbPath, true)
		if err != nil {
			t.Fatalf("load topic key: %v", err)
		}
		if !slices.Equal(first, second) {
			t.Fatal("reloaded topic key differs")
		}
		info, err := os.Stat(dbPath + topicKeySuffix)
		if err != nil {
			t.Fatalf("stat topic key: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("topic key mode = %04o, want 0600", info.Mode().Perm())
		}
	})

	t.Run("missing for existing store", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		if _, err := loadOrCreateTopicKey(dbPath, true); !errors.Is(err, errTopicKeyMissing) {
			t.Fatalf("load error = %v, want errTopicKeyMissing", err)
		}
	})

	t.Run("widened permissions", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		path := dbPath + topicKeySuffix
		if err := os.WriteFile(path, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
			t.Fatalf("write topic key: %v", err)
		}
		if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // intentionally widened adversarial fixture
			t.Fatalf("chmod topic key: %v", err)
		}
		if _, err := loadOrCreateTopicKey(dbPath, true); !errors.Is(err, errTopicKeyPermissions) {
			t.Fatalf("load error = %v, want errTopicKeyPermissions", err)
		}
	})

	t.Run("malformed length", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		if err := os.WriteFile(dbPath+topicKeySuffix, []byte("short"), 0o600); err != nil {
			t.Fatalf("write topic key: %v", err)
		}
		if _, err := loadOrCreateTopicKey(dbPath, true); !errors.Is(err, errTopicKeyMalformed) {
			t.Fatalf("load error = %v, want errTopicKeyMalformed", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "freeside.db")
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.Symlink(target, dbPath+topicKeySuffix); err != nil {
			t.Fatalf("symlink topic key: %v", err)
		}
		if _, err := loadOrCreateTopicKey(dbPath, true); !errors.Is(err, errTopicKeyPermissions) {
			t.Fatalf("load error = %v, want errTopicKeyPermissions", err)
		}
	})

	t.Run("hard link", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "freeside.db")
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.Link(target, dbPath+topicKeySuffix); err != nil {
			t.Fatalf("hard link topic key: %v", err)
		}
		if _, err := loadOrCreateTopicKey(dbPath, true); !errors.Is(err, errTopicKeyPermissions) {
			t.Fatalf("load error = %v, want errTopicKeyPermissions", err)
		}
	})
}

func TestRunValidatesConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := run(ctx, nil, config{ListenAddr: "127.0.0.1:0"}); err == nil {
		t.Fatal("run accepted an empty database path")
	}
	if _, err := run(ctx, nil, config{
		DBPath:     filepath.Join(t.TempDir(), "freeside.db"),
		ListenAddr: "127.0.0.1:0", ReconcileInterval: -time.Second,
	}); err == nil {
		t.Fatal("run accepted a negative reconcile interval")
	}
}

func TestConfigStoreOptions(t *testing.T) {
	t.Parallel()
	options, err := (config{}).storeOptions()
	if err != nil {
		t.Fatalf("storeOptions with no waiver: %v", err)
	}
	if options.BackupEncryptionWaiverRepositoryID != nil {
		t.Fatalf("no-waiver options configured repository %d", *options.BackupEncryptionWaiverRepositoryID)
	}

	repositoryID := int64(424242)
	if _, err := (config{BackupEncryptionWaiverRepositoryID: &repositoryID}).storeOptions(); !errors.Is(err, domain.ErrBackupEncryptionWaiverUnsupported) {
		t.Fatalf("storeOptions with retired waiver = %v, want %v",
			err, domain.ErrBackupEncryptionWaiverUnsupported)
	}
}

func TestConfigStoreOptionsCarryDoctorRecipePolicy(t *testing.T) {
	ctx := context.Background()
	recipe := domain.Digest("sha256:" + strings.Repeat("a", 64))
	cfg := config{ApprovedRecipes: map[domain.Digest]bool{recipe: true}}
	options, err := cfg.storeOptions()
	if err != nil {
		t.Fatal(err)
	}
	if !options.ApprovedRecipes[recipe] {
		t.Fatal("production store options dropped the approved recipe")
	}
	delete(options.ApprovedRecipes, recipe)
	if !cfg.ApprovedRecipes[recipe] {
		t.Fatal("store options retained the caller's mutable recipe map")
	}
	options, err = cfg.storeOptions()
	if err != nil {
		t.Fatal(err)
	}
	artifactBody := "approved verifier artifact"
	artifactDigest := (domain.ClaimText{Content: artifactBody}).ComputeDigest()
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "artifact-doctor-policy", Type: "verify_log",
		Digest: artifactDigest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "invocation-doctor-policy",
			HeadBinding: domain.HeadBound, SourceHeadSHA: strings.Repeat("c", 40),
			VerificationRecipeDigest: &recipe, SensitivityClass: domain.SensitivityNormal,
		},
	}, options.ApprovedRecipes)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "freeside.db")
	blobs, err := signet.NewBlobStore(path + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put(artifactDigest, strings.NewReader(artifactBody)); err != nil {
		t.Fatal(err)
	}
	backupFiles, err := store.NewDefaultLocalBackupFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	healthSource, err := backupFiles.NewCheckpointHealthSource(
		blobs, options.ApprovedRecipes, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	options.BackupHealthSource = healthSource
	st, err := store.Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutArtifact(ctx, artifact)
	}); err != nil {
		t.Fatal(err)
	}
	producer, err := backupFiles.NewProducer(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("produce checkpoint under approved recipe policy: %v", err)
	}
	if _, err := st.BackupHealth(ctx); err != nil {
		t.Fatalf("inspect checkpoint under approved recipe policy: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopenOptions, err := cfg.storeOptions()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, path, reopenOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close() //nolint:errcheck // Test cleanup cannot affect the assertion.
	if err := reopened.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetArtifact(ctx, artifact.ID)
		return err
	}); err != nil {
		t.Fatalf("reconstruct approved persisted artifact: %v", err)
	}
}

func TestRepositoryIDFlag(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "0", "-1", "repo", "9223372036854775808"} {
		var flagValue repositoryIDFlag
		if err := flagValue.Set(value); err == nil {
			t.Errorf("Set(%q) succeeded", value)
		}
		if flagValue.Value() != nil {
			t.Errorf("Set(%q) retained repository ID %d after rejection", value, *flagValue.Value())
		}
	}

	var flagValue repositoryIDFlag
	if err := flagValue.Set("424242"); err != nil {
		t.Fatalf("Set(valid): %v", err)
	}
	got := flagValue.Value()
	if got == nil || *got != 424242 {
		t.Fatalf("Value() = %v, want 424242", got)
	}
	*got = 7
	if reread := flagValue.Value(); reread == nil || *reread != 424242 {
		t.Fatalf("Value() followed caller mutation: %v", reread)
	}
}

func TestRunDrivesFakeWorkflow(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := run(ctx, nil, config{
		DBPath:        filepath.Join(root, "freeside.db"),
		FakeDriverDir: filepath.Join(root, "driver"),
		ListenAddr:    "127.0.0.1:0", ReconcileInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	health, err := h.store.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("production backup-health source: %v", err)
	}
	if health.Encryption != domain.BackupHealthHealthy ||
		health.CheckpointCurrency != domain.BackupHealthHealthy ||
		health.ArtifactClosure != domain.BackupHealthHealthy ||
		health.RestoreTestAge != domain.BackupHealthHealthy {
		t.Fatalf("produced local checkpoint health = %+v, want every dimension healthy", health)
	}

	pairedAt := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{
			ID: "device-1", DisplayName: "Device 1",
			Status: domain.DeviceActive, PairedAt: pairedAt,
		})
	}); err != nil {
		t.Fatalf("put device: %v", err)
	}
	approval, err := h.attention.GetAttentionItem(ctx, "approval-"+domain.ItemID(defaultFakeRunID))
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if _, err := h.attention.Submit(ctx, signet.ClientCommand{
		CommandID: "approve-main", DeviceID: "device-1", ExpectedEntityVersion: approval.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: approval.Item.ID, Action: domain.ActionApprove,
			ItemVersion: approval.Item.ItemVersion,
		},
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	feedback := waitForItem(t, h.attention, "feedback-"+domain.ItemID(defaultFakeRunID))
	if _, err := h.attention.Submit(ctx, signet.ClientCommand{
		CommandID: "discuss-main", DeviceID: "device-1", ExpectedEntityVersion: feedback.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: feedback.Item.ID, Action: domain.ActionDiscuss,
			ItemVersion: feedback.Item.ItemVersion, Message: "advance the workflow",
		},
	}); err != nil {
		t.Fatalf("discuss: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := h.attention.GetRun(ctx, defaultFakeRunID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if len(run.Run.Stages) == 1 && len(run.Run.Stages[0].Attempts) == 1 {
			item, err := h.attention.GetAttentionItem(ctx, feedback.Item.ID)
			if err != nil {
				t.Fatalf("get feedback: %v", err)
			}
			conversation, err := h.attention.GetConversation(ctx, *item.Item.ConversationID)
			if err != nil {
				t.Fatalf("get conversation: %v", err)
			}
			if len(conversation.Conversation.Messages) == 2 &&
				conversation.Conversation.Status == domain.ConversationIdle {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("freesided did not accept the fake result within 2s")
}

func waitForItem(t *testing.T, attention *signet.Service, id domain.ItemID) signet.AttentionItemSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		item, err := attention.GetAttentionItem(context.Background(), id)
		if err == nil {
			return item
		}
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get item %q: %v", id, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("item %q did not appear within 2s", id)
	return signet.AttentionItemSnapshot{}
}

func TestListenPrivileged(t *testing.T) {
	listener, err := listenPrivileged("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenPrivileged(loopback): %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close loopback listener: %v", err)
	}

	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "192.0.2.1:0"} {
		t.Run(addr, func(t *testing.T) {
			if listener, err := listenPrivileged(addr); err == nil {
				_ = listener.Close()
				t.Fatalf("listenPrivileged(%q) accepted a wildcard or arbitrary non-loopback address", addr)
			}
		})
	}
}

func TestListenPrivilegedRejectsBeforeBind(t *testing.T) {
	bindCalled := false
	_, err := listenPrivilegedWith(
		"0.0.0.0:0",
		func(string, *net.TCPAddr) (net.Listener, error) {
			bindCalled = true
			return nil, errors.New("unsafe bind was attempted")
		},
		func() ([]netip.Addr, error) {
			t.Fatal("wildcard validation queried Tailscale")
			return nil, nil
		},
	)
	if err == nil {
		t.Fatal("listenPrivilegedWith accepted a wildcard address")
	}
	if bindCalled {
		t.Fatal("listenPrivilegedWith called the binder before rejecting a wildcard address")
	}
}

type listenerStub struct {
	addr   net.Addr
	closed bool
}

func (*listenerStub) Accept() (net.Conn, error) {
	return nil, errors.New("stub listener does not accept")
}
func (l *listenerStub) Close() error   { l.closed = true; return nil }
func (l *listenerStub) Addr() net.Addr { return l.addr }

func TestListenPrivilegedAcceptsOnlyTailscaleOwnedAddresses(t *testing.T) {
	for _, addr := range []string{"100.64.0.7:8443", "[fd7a:115c:a1e0::7]:8443"} {
		t.Run(addr, func(t *testing.T) {
			resolved, err := net.ResolveTCPAddr("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			tailscaleIP, ok := netip.AddrFromSlice(resolved.IP)
			if !ok {
				t.Fatalf("parse test Tailscale address %q", resolved.IP)
			}
			stub := &listenerStub{addr: &net.TCPAddr{IP: resolved.IP, Port: resolved.Port}}
			listener, err := listenPrivilegedWith(
				addr,
				func(_ string, _ *net.TCPAddr) (net.Listener, error) { return stub, nil },
				func() ([]netip.Addr, error) { return []netip.Addr{tailscaleIP}, nil },
			)
			if err != nil {
				t.Fatalf("listenPrivilegedWith(%q): %v", addr, err)
			}
			if listener != stub || stub.closed {
				t.Fatal("accepted Tailscale listener was replaced or closed")
			}
		})
	}

	for _, tc := range []struct {
		name      string
		addr      string
		tailscale []netip.Addr
		queryErr  error
	}{
		{name: "unreported Tailscale", addr: "100.64.0.7:8443"},
		{name: "different reported Tailscale", addr: "100.64.0.7:8443", tailscale: []netip.Addr{
			netip.MustParseAddr("100.64.0.8"),
		}},
		{name: "Tailscale query failure", addr: "100.64.0.7:8443", queryErr: errors.New("tailscaled unavailable")},
		{name: "arbitrary non-loopback", addr: "192.0.2.7:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bindCalled := false
			_, err := listenPrivilegedWith(
				tc.addr,
				func(string, *net.TCPAddr) (net.Listener, error) {
					bindCalled = true
					return nil, errors.New("unsafe bind was attempted")
				},
				func() ([]netip.Addr, error) { return tc.tailscale, tc.queryErr },
			)
			if err == nil {
				t.Fatalf("listenPrivilegedWith(%q) accepted unsupported address", tc.addr)
			}
			if bindCalled {
				t.Fatalf("listenPrivilegedWith(%q) attempted a bind", tc.addr)
			}
		})
	}
}

func TestParseTailscaleIPs(t *testing.T) {
	got, err := parseTailscaleIPs("100.64.0.7\nfd7a:115c:a1e0::7\n")
	if err != nil {
		t.Fatalf("parseTailscaleIPs: %v", err)
	}
	want := []netip.Addr{
		netip.MustParseAddr("100.64.0.7"),
		netip.MustParseAddr("fd7a:115c:a1e0::7"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseTailscaleIPs = %v, want %v", got, want)
	}

	for _, output := range []string{
		"",
		"not-an-ip",
		"192.0.2.7",
		"fe80::1%utun4",
		"100.64.0.7\n192.0.2.7",
	} {
		t.Run(output, func(t *testing.T) {
			if _, err := parseTailscaleIPs(output); err == nil {
				t.Fatalf("parseTailscaleIPs(%q) accepted unsupported output", output)
			}
		})
	}
}

func TestRunRefusesNonLoopbackBeforeCreatingState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	driverDir := filepath.Join(t.TempDir(), "driver")
	h, err := run(context.Background(), nil, config{
		DBPath:        dbPath,
		ListenAddr:    "0.0.0.0:0",
		FakeDriverDir: driverDir,
	})
	if err == nil {
		_ = h.Close()
		t.Fatal("run accepted a wildcard listener")
	}
	for _, path := range []string{dbPath, dbPath + topicKeySuffix, dbPath + ".blobs", driverDir} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("unsafe listener startup created %q: stat error = %v", path, statErr)
		}
	}
}

func TestProductionInputSourceClassifiesOperationalErrorsOnly(t *testing.T) {
	t.Parallel()
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := productionInputSource{blobs: blobs}
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))

	if _, err := source.OpenContext(t.Context(), digest); !errors.Is(err, signet.ErrBlobNotFound) ||
		errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("missing input = %v, want permanent blob-not-found only", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := source.OpenContext(ctx, digest); !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("canceled input open = %v, want retryable input class", err)
	}

	readFailure := retryableInputReadCloser{ReadCloser: failingInputReadCloser{}}
	if _, err := readFailure.Read(make([]byte, 1)); !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("input read = %v, want retryable input class", err)
	}
	if err := readFailure.Close(); !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("input close = %v, want retryable input class", err)
	}
}

func TestClassifyTransportSeedError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		err       error
		retryable bool
		refused   bool
	}{
		{"missing base", publish.ErrRemoteMissingBase, false, true},
		{"materialization refusal", publish.ErrMaterializationRefused, false, true},
		{"git auth", &publish.TransportGitError{Refusal: publish.RefusalAuth}, false, true},
		{"API unauthorized", &publish.APIError{Status: http.StatusUnauthorized}, false, true},
		{"API forbidden", &publish.APIError{Status: http.StatusForbidden}, false, true},
		{"API missing", &publish.APIError{Status: http.StatusNotFound}, false, true},
		{"API rate limit", &publish.APIError{Status: http.StatusTooManyRequests}, true, false},
		{"API server failure", &publish.APIError{Status: http.StatusInternalServerError}, true, false},
		{"ambiguous transport failure", errors.New("temporary network failure"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyTransportSeedError(tc.err)
			if got := errors.Is(err, claude.ErrSeedRetryable); got != tc.retryable {
				t.Fatalf("classifyTransportSeedError(%v) retryable = %v, want %v",
					tc.err, got, tc.retryable)
			}
			if got := errors.Is(err, claude.ErrSeedRefused); got != tc.refused {
				t.Fatalf("classifyTransportSeedError(%v) refused = %v, want %v",
					tc.err, got, tc.refused)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("classified error %v does not retain %v", err, tc.err)
			}
		})
	}
	for _, permanent := range []error{
		publish.ErrNoAppCredentials,
		publish.ErrCredentialPermissions,
		publish.ErrNoInstallation,
		publish.ErrInstallationGrantUntrusted,
		publish.ErrGrantMismatch,
	} {
		if err := classifyTransportSeedError(permanent); !errors.Is(err, claude.ErrSeedRefused) {
			t.Errorf("credential error %v classified as %v, want definitive refusal",
				permanent, err)
		}
	}
}

type failingInputReadCloser struct{}

func (failingInputReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("fixture read failure")
}

func (failingInputReadCloser) Close() error {
	return errors.New("fixture close failure")
}

type stubJanitorRunner struct {
	active         bool
	passes         int
	runErr         error
	stableCoverage int
	faults         []publish.JanitorRegistrationFault
}

func (j *stubJanitorRunner) RunScheduledPass(context.Context) error {
	if j.runErr != nil {
		return j.runErr
	}
	j.passes++
	j.active = true
	return nil
}

func (j *stubJanitorRunner) ActiveFor(int64) bool { return j.active }

func (j *stubJanitorRunner) WithStableCoverage(fn func() error) error {
	j.stableCoverage++
	return fn()
}

func (j *stubJanitorRunner) RegistrationFaults() []publish.JanitorRegistrationFault {
	return j.faults
}

func (j *stubJanitorRunner) PendingReady(publish.PendingInstallationEnvelope) (int64, bool) {
	return 0, false
}

func TestJanitorSessionPrimesCoverageSynchronously(t *testing.T) {
	janitor := &stubJanitorRunner{}
	session, err := startJanitorSession(t.Context(), janitor, []int64{4385298})
	if err != nil {
		t.Fatalf("startJanitorSession: %v", err)
	}
	if janitor.passes != 1 || !janitor.ActiveFor(4385298) {
		t.Fatalf("startup pass = %d, active = %v", janitor.passes, janitor.active)
	}
	if err := session.WithStableCoverage(func() error { return nil }); err != nil {
		t.Fatalf("WithStableCoverage: %v", err)
	}
	if janitor.stableCoverage != 1 {
		t.Fatalf("stable coverage calls = %d, want 1", janitor.stableCoverage)
	}
	// The scheduler-fired pass reaches the same lifecycle.
	if err := session.RunScheduledPass(t.Context()); err != nil {
		t.Fatalf("RunScheduledPass: %v", err)
	}
	if janitor.passes != 2 {
		t.Fatalf("passes = %d, want 2", janitor.passes)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestJanitorSessionReturnsStartupFailure(t *testing.T) {
	want := errors.New("janitor failed")
	_, err := startJanitorSession(
		t.Context(), &stubJanitorRunner{runErr: want}, []int64{4385298},
	)
	if !errors.Is(err, want) {
		t.Fatalf("startJanitorSession error = %v, want %v", err, want)
	}
}

func TestJanitorSessionReportsMissingCoverageWithFaults(t *testing.T) {
	fault := errors.New("registration denied")
	janitor := &stubJanitorRunner{
		faults: []publish.JanitorRegistrationFault{{RegistrationID: 4385298, Err: fault}},
	}
	// The pass succeeds but publishes no coverage for the registration: the
	// startup gate fails with the pass's fault attached.
	janitor.runErr = nil
	_, err := startJanitorSession(t.Context(), &faultyCoverageRunner{janitor}, []int64{4385298})
	if err == nil || !errors.Is(err, fault) {
		t.Fatalf("startJanitorSession error = %v, want fault %v", err, fault)
	}
}

// faultyCoverageRunner completes its pass without activating coverage,
// modeling a per-registration fault.
type faultyCoverageRunner struct{ *stubJanitorRunner }

func (j *faultyCoverageRunner) RunScheduledPass(context.Context) error { return nil }
func (j *faultyCoverageRunner) ActiveFor(int64) bool                   { return false }
