package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
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

// TestDiscoverAfterEnrollment: auth add records the new enrollment's catalog,
// which auth list then prints; a failed discovery only warns.
func TestDiscoverAfterEnrollment(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	// A failed add leaves the identity and enrollment recorded.
	_ = runAuthAddCommand(context.Background(), claudeAuthAddArgs(dbPath),
		strings.NewReader(authAddTestToken+"\n"), &bytes.Buffer{}, &bytes.Buffer{}, authAddTestDeps())
	if listed := runAuthListForTest(t, dbPath); listed.Identities[0].Enrollments[0].ModelCatalog != nil {
		t.Fatalf("an undiscovered enrollment lists a catalog: %+v", listed.Identities[0].Enrollments[0].ModelCatalog)
	}
	token, err := ward.NewSetupToken([]byte(authAddTestToken))
	if err != nil {
		t.Fatal(err)
	}
	const enrollment = domain.ClientEnrollmentID("claude-main/claude_code")

	st := storetest.Open(t, dbPath, store.Options{})
	var stderr bytes.Buffer
	discoverAfterEnrollment(t.Context(), st, func(context.Context, string, ward.SetupToken) (ward.ModelReport, error) {
		return ward.ModelReport{}, errors.New("probe failed")
	}, modelDiscoveryTestImage, token, enrollment, &stderr)
	if !strings.Contains(stderr.String(), "warning: model discovery for "+string(enrollment)) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	discoverAfterEnrollment(t.Context(), st, func(context.Context, string, ward.SetupToken) (ward.ModelReport, error) {
		return discoveredReport("opus"), nil
	}, modelDiscoveryTestImage, token, enrollment, &stderr)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	listed := runAuthListForTest(t, dbPath)
	catalog := listed.Identities[0].Enrollments[0].ModelCatalog
	if catalog == nil || catalog.HarnessBuild != "2.1.282 (Claude Code)" || len(catalog.Models) != 1 ||
		catalog.Models[0].ID != "opus" {
		t.Fatalf("listed catalog = %+v", catalog)
	}
}
