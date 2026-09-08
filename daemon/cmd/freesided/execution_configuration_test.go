package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestDefaultExecutionDisabled(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	h, err := run(ctx, nil, config{
		DBPath: filepath.Join(root, "freeside.db"), ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	if h.driver != nil || h.workflow != nil {
		t.Fatal("default daemon composed an execution engine")
	}
	if _, err := os.Stat(filepath.Join(root, "freeside.db.fake-stage-driver")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default daemon created fake-driver state: %v", err)
	}
	if h.pairingCode == "" {
		t.Fatal("default daemon did not offer pairing")
	}
	response, err := http.Get("http://" + h.listener.Addr().String() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
	items, err := h.attention.ListAttentionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, snapshot := range items {
		if snapshot.Item.HealthDiagnostic != nil && snapshot.Item.HealthDiagnostic.Code == "execution_not_configured" {
			found = true
			if !strings.Contains(snapshot.Item.Reason, "does not run agents or simulate work") {
				t.Fatal("setup notice does not explain disabled execution")
			}
		}
	}
	if !found {
		t.Fatal("default daemon did not show its execution setup requirement")
	}
}

func TestSeedRequiresExplicitFakeDriver(t *testing.T) {
	_, err := run(t.Context(), nil, config{
		DBPath: filepath.Join(t.TempDir(), "freeside.db"), SeedWalkingSkeleton: true,
	})
	if err == nil || !strings.Contains(err.Error(), "requires -driver fake") {
		t.Fatalf("seed without fake driver = %v", err)
	}
}

func TestExecutionConfigurationNoticeConverges(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	for _, enabled := range []bool{false, false, true} {
		if err := convergeExecutionConfiguration(ctx, st, enabled, nil); err != nil {
			t.Fatal(err)
		}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
			if err != nil {
				return err
			}
			want := 1
			if enabled {
				want = 0
			}
			if len(items) != want {
				t.Errorf("enabled=%v: %d open notices, want %d", enabled, len(items), want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
