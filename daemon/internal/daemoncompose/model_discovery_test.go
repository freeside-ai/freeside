package daemoncompose

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const modelDiscoveryTestImage = "example.test/agent-claude@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func discoveredReport(ids ...string) ward.ModelReport {
	report := ward.ModelReport{CLIBuild: "2.1.282 (Claude Code)"}
	for _, id := range ids {
		report.Models = append(report.Models, domain.DiscoveredModel{ID: id, DisplayName: id, EffortLevels: []string{}})
	}
	return report
}

func readModelCatalog(t *testing.T, st *store.Store, id domain.ClientEnrollmentID) (domain.DiscoveredModelCatalog, error) {
	t.Helper()
	var catalog domain.DiscoveredModelCatalog
	err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		catalog, err = tx.GetDiscoveredModelCatalog(t.Context(), id)
		return err
	})
	return catalog, err
}

// TestModelCatalogRefreshDiscoversEachEnabledClaudeEnrollment: the refresh
// reads each enabled Claude setup-token enrollment's current generation,
// discovers under that token in the agent image, and records the catalog;
// a disabled identity and a Codex enrollment are skipped, and a failure
// keeps the last catalog and logs.
func TestModelCatalogRefreshDiscoversEachEnabledClaudeEnrollment(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	at := time.Now().UTC().Add(-time.Hour)
	seedUsageEnrollment(t, st, "auth-claude", "enroll-claude", "pool-claude", domain.HarnessClientClaudeCode, at)
	seedUsageEnrollment(t, st, "auth-off", "enroll-off", "pool-off", domain.HarnessClientClaudeCode, at)
	seedUsageEnrollment(t, st, "auth-codex", "enroll-codex", "pool-codex", domain.HarnessClientCodexCLI, at)
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		identity, err := tx.GetAuthIdentity(t.Context(), "auth-off")
		if err != nil {
			return err
		}
		identity.Enabled = false
		return tx.RecordAuthIdentity(t.Context(), identity, at.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}

	token, err := ward.NewSetupToken([]byte(authAddTestToken))
	if err != nil {
		t.Fatal(err)
	}
	var read []string
	discoverErr := error(nil)
	var logs bytes.Buffer
	observedAt := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	refresh := modelCatalogRefresh{
		store: st,
		readToken: func(_ context.Context, id domain.ClientEnrollmentID, ordinal int) (ward.SetupToken, error) {
			read = append(read, string(id))
			if ordinal != 1 {
				t.Errorf("read %s generation %d, want the current one", id, ordinal)
			}
			return token, nil
		},
		discover: func(_ context.Context, image string, _ ward.SetupToken) (ward.ModelReport, error) {
			if image != modelDiscoveryTestImage {
				t.Errorf("discovered through %q", image)
			}
			return discoveredReport("opus", "sonnet"), discoverErr
		},
		image: modelDiscoveryTestImage, now: func() time.Time { return observedAt },
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	refresh.run(t.Context())
	if !slices.Equal(read, []string{"enroll-claude"}) {
		t.Fatalf("read tokens for %v, want only the enabled Claude enrollment", read)
	}
	catalog, err := readModelCatalog(t, st, "enroll-claude")
	if err != nil {
		t.Fatal(err)
	}
	if !catalog.ObservedAt.Equal(observedAt) || len(catalog.Models) != 2 {
		t.Fatalf("catalog = %+v", catalog)
	}

	discoverErr = errors.New("probe failed")
	refresh.now = func() time.Time { return observedAt.Add(time.Hour) }
	refresh.run(t.Context())
	kept, err := readModelCatalog(t, st, "enroll-claude")
	if err != nil || !kept.ObservedAt.Equal(observedAt) {
		t.Fatalf("after a failed refresh: catalog = %+v, err = %v; want the last one kept", kept, err)
	}
	if !strings.Contains(logs.String(), "model discovery failed") || !strings.Contains(logs.String(), "enroll-claude") {
		t.Fatalf("logs = %s", logs.String())
	}
}

// TestModelCatalogRefreshSettlesTheDispatchHold: the engine's dispatch waits
// for the startup discovery (#3447), so the hold stays open while a probe
// runs and closes on every way the pass ends: all targets done, targets
// unreadable, or its context ended. The drain wait is
// TestModelDiscoveryWaitsOutAnUpdateDrain's.
func TestModelCatalogRefreshSettlesTheDispatchHold(t *testing.T) {
	t.Parallel()
	token, err := ward.NewSetupToken([]byte(authAddTestToken))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		closeStore bool
		cancel     bool
	}{
		"all targets done":   {},
		"targets unreadable": {closeStore: true},
		"context ended":      {cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
			seedUsageEnrollment(t, st, "auth-claude", "enroll-claude", "pool-claude",
				domain.HarnessClientClaudeCode, time.Now().UTC().Add(-time.Hour))
			hold := make(chan struct{})
			held := func() bool {
				select {
				case <-hold:
					return false
				default:
					return true
				}
			}
			discovered := false
			refresh := modelCatalogRefresh{
				store: st,
				readToken: func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
					return token, nil
				},
				discover: func(context.Context, string, ward.SetupToken) (ward.ModelReport, error) {
					discovered = true
					if !held() {
						t.Error("the dispatch hold closed before the probe ran")
					}
					return discoveredReport("opus"), nil
				},
				image: modelDiscoveryTestImage, now: time.Now, logger: slog.New(slog.DiscardHandler),
				settled: sync.OnceFunc(func() { close(hold) }),
			}
			ctx := t.Context()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if tc.closeStore {
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
			}
			refresh.run(ctx)
			if held() {
				t.Fatal("the pass ended with the dispatch hold open")
			}
			if wantProbe := !tc.closeStore && !tc.cancel; discovered != wantProbe {
				t.Fatalf("probe ran = %v, want %v", discovered, wantProbe)
			}
		})
	}
}
