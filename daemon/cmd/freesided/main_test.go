package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// testEvidenceTime is the fixed UTC creation time for the package's
// evidence-metadata fixtures.
var testEvidenceTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// testRunEvidenceMetadata is valid run-sourced evidence metadata.
func testRunEvidenceMetadata(sizeBytes int64) domain.EvidenceMetadata {
	return domain.EvidenceMetadata{
		MediaType:    domain.EvidenceMediaApplicationJSON,
		SizeBytes:    sizeBytes,
		CreatedAt:    testEvidenceTime,
		Source:       domain.EvidenceSourceRun,
		Availability: domain.EvidenceAvailable,
	}
}

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
	t.Parallel()
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
		Environment:       environmentEphemeral,
		FakeDriverEnabled: true,
		DBPath:            filepath.Join(root, "freeside.db"),
		FakeDriverDir:     filepath.Join(root, "driver"),
		StateDir:          filepath.Join(root, "state"),
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

	seedTemplateStore(t, cfg.DBPath)
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
	if _, statErr := os.Stat(filepath.Join(cfg.StateDir, daemonshell.ReadinessFileName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("readiness after failed startup = %v, want absent", statErr)
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
	retry, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral,
		DBPath:      filepath.Join(root, "freeside.db"), FakeDriverDir: filepath.Join(root, "retry-driver"),
		StateDir: root, ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("retry after failed startup: %v", err)
	}
	if err := retry.Close(); err != nil {
		t.Fatal(err)
	}
}

//nolint:paralleltest // Serial: its 30s budget covers a startup drain that takes up to 16s alone under -race; parallel load overruns it.
func TestRunReturnsAfterPostBackgroundStartFailure(t *testing.T) {
	root := t.TempDir()
	seedTemplateStore(t, filepath.Join(root, "freeside.db"))
	startupErr := errors.New("injected post-background-start failure")
	result := make(chan struct {
		h   *daemon
		err error
	}, 1)
	go func() {
		h, err := run(t.Context(), nil, config{
			Environment:          environmentEphemeral,
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
	t.Parallel()
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	seedTemplateStore(t, filepath.Join(root, "freeside.db"))
	h, err := run(ctx, nil, config{
		Environment:   environmentEphemeral,
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
	response, err := client.Get(h.Readiness().APIURL + "/sync/revision")
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
	t.Parallel()
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	cfg := config{
		Environment: environmentEphemeral,
		DBPath:      dbPath, FakeDriverDir: filepath.Join(root, "driver"),
		FakeDriverEnabled: true,
		ListenAddr:        "127.0.0.1:0", ReconcileInterval: 10 * time.Millisecond,
	}
	cleanCtx, stopClean := context.WithCancel(context.Background())
	seedTemplateStore(t, dbPath)
	clean, err := run(cleanCtx, nil, cfg)
	if err != nil {
		stopClean()
		t.Fatal(err)
	}
	stopClean()
	if err := errors.Join(clean.Wait(cleanCtx), clean.Close()); err != nil {
		t.Fatal(err)
	}

	st := storetest.Open(t, dbPath, store.Options{})
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
	t.Parallel()
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	seedTemplateStore(t, dbPath)
	cfg := config{
		Environment:   environmentEphemeral,
		DBPath:        dbPath,
		FakeDriverDir: filepath.Join(root, "driver"),
		ListenAddr:    "127.0.0.1:0", ReconcileInterval: 10 * time.Millisecond,
	}
	start := func(t *testing.T, attempt string) (*daemon, context.Context, context.CancelFunc) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		h, err := run(ctx, nil, cfg)
		if err != nil {
			cancel()
			t.Fatalf("%s: run: %v", attempt, err)
		}
		return h, ctx, cancel
	}
	stop := func(t *testing.T, attempt string, h *daemon, ctx context.Context, cancel context.CancelFunc) {
		t.Helper()
		cancel()
		if err := h.Wait(ctx); err != nil {
			t.Fatalf("%s: Wait after cancellation: %v", attempt, err)
		}
		if err := h.Close(); err != nil {
			t.Fatalf("%s: Close: %v", attempt, err)
		}
	}

	// Establish the state a newer daemon leaves behind: a complete store, a
	// proven checkpoint, and then a durable row of a kind this binary has no
	// extractor for.
	h, ctx, cancel := start(t, "clean start")
	stop(t, "clean start", h, ctx, cancel)
	seed := storetest.Open(t, dbPath, store.Options{})
	seedErr := seed.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			context.Background(), "future-1", "backup.kind-this-binary-lacks", []byte("payload"))
		return err
	})
	if err := errors.Join(seedErr, seed.Close()); err != nil {
		t.Fatalf("seed unreadable durable row: %v", err)
	}

	for _, attempt := range []string{"first start", "restart"} {
		h, ctx, cancel := start(t, attempt)
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
		stop(t, attempt, h, ctx, cancel)
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
	if err := engine.SubmittedPathBoundary(key(t, "paths", "daemon/**, docs/**")); err != nil {
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
			if err := engine.SubmittedPathBoundary(
				key(t, tc.keyName, tc.value),
			); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
				t.Errorf("engine.SubmittedPathBoundary(%q) = %v, want ErrPathBoundaryMismatch",
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
	t.Parallel()
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	seedTemplateStore(t, filepath.Join(root, "freeside.db"))
	h, err := run(ctx, nil, config{
		Environment: environmentEphemeral,
		DBPath:      filepath.Join(root, "freeside.db"), ListenAddr: "127.0.0.1:0",
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

	ready := h.Readiness()
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
	t.Parallel()
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
	if _, err := run(ctx, nil, config{Environment: environmentEphemeral, ListenAddr: "127.0.0.1:0"}); err == nil {
		t.Fatal("run accepted an empty database path")
	}
	if _, err := run(ctx, nil, config{
		Environment: environmentEphemeral,
		DBPath:      filepath.Join(t.TempDir(), "freeside.db"),
		ListenAddr:  "127.0.0.1:0", ReconcileInterval: -time.Second,
	}); err == nil {
		t.Fatal("run accepted a negative reconcile interval")
	}
	if _, err := run(ctx, nil, config{
		Environment: environmentEphemeral,
		DBPath:      filepath.Join(t.TempDir(), "freeside.db"),
		ListenAddr:  "127.0.0.1:0", AttentionDeliveryInterval: -time.Second,
	}); err == nil {
		t.Fatal("run accepted a negative attention delivery interval")
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
	t.Parallel()
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
		ID: "artifact-doctor-policy", Type: domain.ArtifactKindVerifyLog,
		Digest: artifactDigest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "invocation-doctor-policy",
			HeadBinding: domain.HeadBound, SourceHeadSHA: strings.Repeat("c", 40),
			VerificationRecipeDigest: &recipe, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: testRunEvidenceMetadata(int64(len(artifactBody))),
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
	storetest.Copy(t, path)
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

//nolint:paralleltest // Serial: waitForItem and the fake-result poll give the live daemon 2s, which parallel load overruns.
func TestRunDrivesFakeWorkflow(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	// Uncheckpointed: the health read below proves the checkpoint this start
	// produces.
	seedUncheckpointedTemplateStore(t, filepath.Join(root, "freeside.db"))
	h, err := run(ctx, nil, config{
		Environment:   environmentEphemeral,
		DBPath:        filepath.Join(root, "freeside.db"),
		FakeDriverDir: filepath.Join(root, "driver"),
		ListenAddr:    "127.0.0.1:0", ReconcileInterval: 5 * time.Millisecond,
		SeedWalkingSkeleton: true, FakeDriverEnabled: true,
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
	approval, err := h.attention.GetAttentionItem(ctx, "approval-"+domain.ItemID(daemoncompose.DefaultFakeRunID))
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

	feedback := waitForItem(t, h.attention, "feedback-"+domain.ItemID(daemoncompose.DefaultFakeRunID))
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
		run, err := h.attention.GetRun(ctx, daemoncompose.DefaultFakeRunID)
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

// TestRunWithoutSeedFlagLeavesTheStoreEmpty is the #1127 half of the
// walking-skeleton seed gate: an explicitly selected fake driver seeds no demo
// work without the seed flag. The default disabled mode is tested separately.
func TestRunWithoutSeedFlagLeavesTheStoreEmpty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	seedTemplateStore(t, filepath.Join(root, "freeside.db"))
	h, err := run(ctx, nil, config{
		Environment:       environmentEphemeral,
		FakeDriverEnabled: true,
		DBPath:            filepath.Join(root, "freeside.db"),
		FakeDriverDir:     filepath.Join(root, "driver"),
		ListenAddr:        "127.0.0.1:0", ReconcileInterval: 5 * time.Millisecond,
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

	if err := h.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetRun(ctx, daemoncompose.DefaultFakeRunID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("get walking-skeleton run = %v, want ErrNotFound", err)
		}
		if _, err := tx.GetAttentionItem(ctx, "approval-"+domain.ItemID(daemoncompose.DefaultFakeRunID)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("get walking-skeleton approval = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("read store: %v", err)
	}
}

// daemonWaitBound bounds a whole-daemon test's wait for the daemon's own
// work, so it catches a hang, not a slow run. That work is CPU-bound in the
// engine and the pure-Go SQLite store, which the race detector slows several
// times: the discuss reply in
// TestDaemonAnswersADiscussAskedWhileItsNotificationIsInFlight takes about
// 0.1s without -race, 0.9-2.2s with it, and up to 5.3s with it on a heavily
// loaded host.
const daemonWaitBound = 60 * time.Second

func waitForItem(t *testing.T, attention *signet.Service, id domain.ItemID) signet.AttentionItemSnapshot {
	t.Helper()
	deadline := time.Now().Add(daemonWaitBound)
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
	t.Fatalf("item %q did not appear within %s", id, daemonWaitBound)
	return signet.AttentionItemSnapshot{}
}

func TestRunRefusesNonLoopbackBeforeCreatingState(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	driverDir := filepath.Join(t.TempDir(), "driver")
	h, err := run(context.Background(), nil, config{
		Environment:   environmentEphemeral,
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
