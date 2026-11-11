package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

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
