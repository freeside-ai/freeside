package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestSeedFixtureRefusedBeforeStoreOpens(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config
		want string
	}{
		{"prod", config{SeedFixture: "representative", Environment: environmentProd}, "-seed-fixture requires -environment ephemeral"},
		{"dev", config{SeedFixture: "representative", Environment: environmentDev}, "-seed-fixture requires -environment ephemeral"},
		// run rejects the zero tier before parsing the fixture (#1562), so
		// this refusal comes from the tier check, still before the store opens.
		{"unset tier", config{SeedFixture: "representative"}, `environment "" is not prod, dev, or ephemeral`},
		{"fake driver", config{SeedFixture: "representative", Environment: environmentEphemeral, FakeDriverEnabled: true}, "-seed-fixture requires -driver disabled"},
		{"claude driver", config{SeedFixture: "representative", Environment: environmentEphemeral, Claude: &claudeDriverConfig{}}, "-seed-fixture requires -driver disabled"},
		{"unknown name", config{SeedFixture: "nope", Environment: environmentEphemeral}, "-seed-fixture: unknown fixture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "freeside.db")
			tc.cfg.DBPath = dbPath
			tc.cfg.ListenAddr = "127.0.0.1:0"
			_, err := run(t.Context(), nil, tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run = %v, want an error containing %q", err, tc.want)
			}
			if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused start touched the store: %v", err)
			}
		})
	}
}

func TestSeedFixtureServesRepresentativeState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	cfg := config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0",
		SeedFixture: "representative", Environment: environmentEphemeral,
	}
	h := startDaemon(t, cfg)
	assertFixtureServed(t, h)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// A second seed into the now non-empty store is refused, not merged.
	if _, err := run(t.Context(), nil, cfg); err == nil || !strings.Contains(err.Error(), "already holds runs") {
		t.Fatalf("reseed = %v, want refusal", err)
	}

	// Restarting the seeded store without the flag still reads its admitted
	// runs: the ephemeral driverless floor does not depend on -seed-fixture.
	cfg.SeedFixture = ""
	h = startDaemon(t, cfg)
	assertFixtureServed(t, h)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func startDaemon(t *testing.T, cfg config) *daemon {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	h, err := run(ctx, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// assertFixtureServed checks the representative counts through the served
// projections, plus the daemon's own execution-configuration notice.
func assertFixtureServed(t *testing.T, h *daemon) {
	t.Helper()
	ctx := t.Context()
	runs, err := h.attention.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 8 {
		t.Errorf("runs = %d, want 8", len(runs))
	}
	items, err := h.attention.ListAttentionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, snapshot := range items {
		if snapshot.Item.Type == domain.AttentionSystemHealth && snapshot.Item.HealthDiagnostic != nil &&
			snapshot.Item.HealthDiagnostic.Code == "execution_not_configured" {
			notices++
		}
	}
	// 14 fixture items plus the daemon's execution-configuration notice.
	if len(items) != 15 || notices != 1 {
		t.Errorf("attention items = %d with %d execution notices, want 15 with 1", len(items), notices)
	}
}

func TestStoreOptionsEphemeralDriverlessFloor(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config
		want bool
	}{
		{"ephemeral disabled", config{Environment: environmentEphemeral}, true},
		{"ephemeral fake", config{Environment: environmentEphemeral, FakeDriverEnabled: true}, false},
		{"dev disabled", config{Environment: environmentDev}, false},
		{"prod disabled", config{Environment: environmentProd}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := tc.cfg.storeOptions()
			if err != nil {
				t.Fatal(err)
			}
			_, got := opts.AdmissionFloors[domain.ModeAttendedDev]
			if got != tc.want || len(opts.AdmissionFloors) > 1 {
				t.Fatalf("floors = %v, want attended_dev floor: %v", opts.AdmissionFloors, tc.want)
			}
		})
	}
}
