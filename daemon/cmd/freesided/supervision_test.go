package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestBuildVersionPrefersStampedValue(t *testing.T) {
	previous := version
	version = "v1.2.3-test"
	t.Cleanup(func() { version = previous })
	if got := buildVersion(); got != version {
		t.Fatalf("buildVersion = %q, want %q", got, version)
	}
	version = ""
	if got := buildVersion(); got == "" {
		t.Fatal("buildVersion fallback is empty")
	}
}

func TestExitClassificationIsComplete(t *testing.T) {
	want := map[componentKind]exitDisposition{
		componentHTTP:                  exitRestartSafe,
		componentWorkflow:              exitDurableStop,
		componentLocalBackups:          exitDurableStop,
		componentScheduler:             exitDurableStop,
		componentProductionPublication: exitDurableStop,
		componentActiveResource:        exitDurableStop,
		componentPanic:                 exitInvoluntary,
	}
	if len(want) != len(AllComponentKinds) {
		t.Fatalf("classification table has %d entries, want %d", len(want), len(AllComponentKinds))
	}
	for _, kind := range AllComponentKinds {
		if !kind.valid() {
			t.Fatalf("registered component kind %q is invalid", kind)
		}
		if got := classifyComponentExit(kind); got != want[kind] {
			t.Errorf("classifyComponentExit(%s) = %s, want %s", kind, got, want[kind])
		}
	}
	for _, disposition := range AllExitDispositions {
		if !disposition.valid() {
			t.Fatalf("registered exit disposition %q is invalid", disposition)
		}
	}
}

func TestScheduledExternalFailureThresholdRecordsTheRun(t *testing.T) {
	now := time.Date(2026, 8, 10, 1, 2, 3, 0, time.UTC)
	tracker := newScheduledFailureTracker(func() time.Time {
		observed := now
		now = now.Add(time.Minute)
		return observed
	}, nil)

	for attempt := 1; attempt <= scheduledFailureThreshold; attempt++ {
		consumption, err := tracker.consumption(domain.ScheduleJanitor,
			&publish.APIError{Status: http.StatusServiceUnavailable, RequestPath: "/installations"})
		if attempt < scheduledFailureThreshold {
			if err != nil || consumption.Outcome != domain.OutcomeObserveFailed {
				t.Fatalf("attempt %d = outcome %s, err %v; want observe_failed", attempt, consumption.Outcome, err)
			}
			continue
		}
		var run *scheduledFailureRunError
		if !errors.As(err, &run) || len(run.failures) != scheduledFailureThreshold {
			t.Fatalf("attempt %d error = %#v, want %d-failure run", attempt, err, scheduledFailureThreshold)
		}
		for _, want := range []string{
			"2026-08-10T01:02:03Z",
			"2026-08-10T01:03:03Z",
			"2026-08-10T01:04:03Z",
			"status 503",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("threshold error %q does not record %q", err, want)
			}
		}
	}
}

func TestScheduledFailureClassificationFailsClosed(t *testing.T) {
	for name, failure := range map[string]error{
		"unclassified": errors.New("operational source unreadable"),
		"revoked authority": &publish.APIError{
			Status: http.StatusUnauthorized, RequestPath: "/installations",
		},
		"mixed transient and revoked": fmt.Errorf("wrapped pass: %w", errors.Join(
			&publish.APIError{Status: http.StatusServiceUnavailable, RequestPath: "/installations"},
			&publish.APIError{Status: http.StatusUnauthorized, RequestPath: "/installations"},
		)),
	} {
		t.Run(name, func(t *testing.T) {
			tracker := newScheduledFailureTracker(time.Now, nil)
			_, err := tracker.consumption(domain.ScheduleDoctor, failure)
			var run *scheduledFailureRunError
			if !errors.As(err, &run) || len(run.failures) != 1 {
				t.Fatalf("error = %#v, want immediate one-failure durable stop", err)
			}
		})
	}
}

func TestReadinessFileIsPrivateAndUsable(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h, err := run(ctx, nil, config{
		DBPath:            filepath.Join(root, "freeside.db"),
		FakeDriverDir:     filepath.Join(root, "driver"),
		StateDir:          stateDir,
		ListenAddr:        "127.0.0.1:0",
		ReconcileInterval: 10 * time.Millisecond,
	})
	if err != nil {
		cancel()
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	path := filepath.Join(stateDir, readinessFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat readiness file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("readiness mode = %04o, want 0600", got)
	}
	body, err := os.ReadFile(path) //nolint:gosec // test-owned temporary path
	if err != nil {
		t.Fatalf("read readiness: %v", err)
	}
	var ready readiness
	if err := json.Unmarshal(body, &ready); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if ready != h.readiness() || ready.APIURL == "" || ready.PairingCode == "" {
		t.Fatalf("readiness = %+v, want %+v", ready, h.readiness())
	}
	response, err := http.Get(ready.APIURL + "/health")
	if err != nil {
		t.Fatalf("GET readiness api_url health: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", response.StatusCode)
	}
	pairingBody, err := json.Marshal(map[string]string{
		"pairing_code": ready.PairingCode,
		"display_name": "Readiness file fixture",
	})
	if err != nil {
		t.Fatalf("encode pairing request: %v", err)
	}
	pairingResponse, err := http.Post(ready.APIURL+"/pairing", "application/json", bytes.NewReader(pairingBody))
	if err != nil {
		t.Fatalf("pair through readiness api_url: %v", err)
	}
	defer func() { _ = pairingResponse.Body.Close() }()
	if pairingResponse.StatusCode != http.StatusCreated {
		t.Fatalf("pair through readiness file = %d, want 201", pairingResponse.StatusCode)
	}
}

func TestDurableStopKeepsHTTPAndSurvivesReopen(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	h, err := run(lifetime, nil, config{
		DBPath:            dbPath,
		FakeDriverDir:     filepath.Join(root, "driver"),
		ListenAddr:        "127.0.0.1:0",
		ReconcileInterval: 10 * time.Millisecond,
	})
	if err != nil {
		cancelLifetime()
		t.Fatalf("run: %v", err)
	}
	h.componentExited(lifetime, context.Background(), componentWorkflow,
		errors.New("injected durable store failure"))

	response, err := http.Get(h.readiness().APIURL + "/health")
	if err != nil {
		t.Fatalf("GET /health after durable stop: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /health after durable stop = %d, want 200", response.StatusCode)
	}
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		return tx.RequireUnattendedOperationOpen(context.Background())
	}); !errors.Is(err, domain.ErrBlockingSystemHealth) {
		t.Fatalf("admission after durable stop = %v, want blocking system health", err)
	}
	var durableStop domain.AttentionItem
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		for _, item := range items {
			if strings.HasPrefix(string(item.ID), durableStopItemPrefix) {
				durableStop = item
				if item.Offers(domain.ActionResumeUnattended) {
					t.Errorf("fresh durable-stop item actions = %v, must not offer resume_unattended", item.RequestedDecision)
				}
				return nil
			}
		}
		t.Error("durable-stop item was not recorded")
		return nil
	}); err != nil {
		t.Fatalf("read durable-stop item: %v", err)
	}
	select {
	case err := <-h.errs:
		t.Fatalf("durable stop reached fatal channel: %v", err)
	default:
	}

	cancelLifetime()
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Read(context.Background(), func(tx *store.ReadTx) error {
		return tx.RequireUnattendedOperationOpen(context.Background())
	}); !errors.Is(err, domain.ErrBlockingSystemHealth) {
		t.Fatalf("admission after reopen = %v, want blocking system health", err)
	}
	restarted := &daemon{store: reopened}
	if err := restarted.enableDurableStopRecovery(context.Background()); err != nil {
		t.Fatalf("enable durable-stop recovery after reopen: %v", err)
	}
	if err := reopened.Read(context.Background(), func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(context.Background(), durableStop.ID)
		if err != nil {
			return err
		}
		if !item.Offers(domain.ActionResumeUnattended) {
			t.Errorf("restarted durable-stop item actions = %v, want resume_unattended", item.RequestedDecision)
		}
		return nil
	}); err != nil {
		t.Fatalf("read recovered durable-stop item: %v", err)
	}
	if err := restarted.fileDurableStop(context.Background(), errors.New("second durable-stop cause")); err != nil {
		t.Fatalf("record second durable stop: %v", err)
	}
	if err := reopened.Read(context.Background(), func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(context.Background(), durableStop.ID)
		if err != nil {
			return err
		}
		if item.Offers(domain.ActionResumeUnattended) {
			t.Errorf("recurrent durable-stop item actions = %v, must not offer resume_unattended", item.RequestedDecision)
		}
		if !strings.Contains(item.Reason, "second durable-stop cause") {
			t.Errorf("recurrent durable-stop reason = %q, want latest cause", item.Reason)
		}
		return nil
	}); err != nil {
		t.Fatalf("read recurrent durable-stop item: %v", err)
	}
}
