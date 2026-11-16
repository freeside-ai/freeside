package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// TestProdServesDriverlessBesideADeputy pins prod's side of the start order
// through run: a Claude-mode prod that finds the App authority writer lock
// held, or a rig lease present, starts anyway (no exit status 2), composes no
// driver, files the card that names why, and reports execution disabled on
// GET /execution, which is what a deputy reads before it opens the authority.
func TestProdServesDriverlessBesideADeputy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		lockHeld bool
		lease    bool
		wantCode string
	}{
		{name: "deputy holds the lock", lockHeld: true, wantCode: "app_authority_in_use"},
		{name: "a lease is present", lease: true, wantCode: "rig_lease_present"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			stateDir := filepath.Join(root, "app-state")
			if err := os.Mkdir(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			leaseRoot := t.TempDir()
			if tc.lease {
				_, leaseRoot, _ = acquireTestRig(t)
			}
			if tc.lockHeld {
				lock, err := publish.AcquireAuthorityWriterLock(stateDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
			}
			dbPath := filepath.Join(root, "freeside.db")
			seedTemplateStore(t, dbPath)
			h, err := run(t.Context(), nil, config{
				Environment: environmentProd, DBPath: dbPath, ListenAddr: "127.0.0.1:0",
				Claude:        &daemoncompose.ClaudeDriverConfig{StateRoot: stateDir},
				ProdAuthority: &daemoncompose.ProdAuthorityGate{LeaseRoot: leaseRoot},
			})
			if err != nil {
				t.Fatalf("prod beside a deputy did not start: %v", err)
			}
			t.Cleanup(func() {
				if err := h.Close(); err != nil {
					t.Error(err)
				}
			})
			if h.Workflow != nil {
				t.Fatal("prod composed a driver beside a deputy")
			}
			if !tc.lockHeld {
				// prod took the lock, found the lease, and released it
				// before serving, so the deputy can take it.
				lock, err := publish.AcquireAuthorityWriterLock(stateDir)
				if err != nil {
					t.Fatalf("driverless prod still holds the lock: %v", err)
				}
				_ = lock.Close()
			}
			items, err := h.Attention.ListAttentionItems(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, snapshot := range items {
				if d := snapshot.Item.HealthDiagnostic; d != nil {
					codes = append(codes, d.Code)
				}
			}
			found := false
			for _, code := range codes {
				switch code {
				case tc.wantCode:
					found = true
				case "execution_not_configured", "app_authority_in_use", "rig_lease_present":
					t.Errorf("open card %s beside %s", code, tc.wantCode)
				}
			}
			if !found {
				t.Fatalf("cards %v lack %s", codes, tc.wantCode)
			}
			mode, running, err := newProductionRigHost().ProdExecution(t.Context(), dbPath)
			if err != nil || !running || mode != executionDisabled {
				t.Fatalf("probe = %q, running %t, %v; want disabled, running", mode, running, err)
			}
		})
	}
}

// TestDeputyRefusesBesideAnExecutingProd pins the deputy's side of prod-first:
// the deputy takes its lease, is refused the lock an executing prod holds,
// with the refusal naming execution pause, and its clean release leaves no
// manifest to keep a restarted prod driverless.
//
//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
func TestDeputyRefusesBesideAnExecutingProd(t *testing.T) {
	host, paths, settings := deputyLeaseFixture(t)
	appState := filepath.Join(t.TempDir(), "app-state")
	if err := os.Mkdir(appState, 0o700); err != nil {
		t.Fatal(err)
	}
	prod, err := publish.AcquireAuthorityWriterLock(appState)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prod.Close() }()
	lease, _, err := acquireDeputyLease(t.Context(), host, paths, settings, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireAppAuthority(appState); !errors.Is(err, publish.ErrAuthorityWriterLockHeld) ||
		!strings.Contains(err.Error(), "freesided execution pause") {
		t.Fatalf("deputy beside an executing prod: %v, want a held-lock refusal naming execution pause", err)
	}
	if err := releaseDeputyLease(t.Context(), host.rig, paths, lease, true); err != nil {
		t.Fatal(err)
	}
	if present, err := daemonlock.RigManifestPresent(host.leaseRoot); err != nil || present {
		t.Fatalf("manifest present = %v, %v after the refused deputy released its lease", present, err)
	}
}
