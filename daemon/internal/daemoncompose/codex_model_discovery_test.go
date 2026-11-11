package daemoncompose

import (
	"bytes"
	"context"
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
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

const codexModelDiscoveryTestImage = "example.test/agent-codex@sha256:" +
	"2222222222222222222222222222222222222222222222222222222222222222"

func codexDiscoveredReport(ids ...string) ward.ModelReport {
	report := ward.ModelReport{CLIBuild: "codex-cli 0.162.1"}
	for _, id := range ids {
		report.Models = append(report.Models,
			domain.DiscoveredModel{ID: id, DisplayName: id, EffortLevels: []string{"low", "max"}})
	}
	return report
}

// TestModelCatalogRefreshDiscoversCodexEnrollmentsForACodexReviewer: with a
// Codex reviewer, the refresh reads each enabled Codex OAuth enrollment's
// credential, discovers it in the review image after the Claude
// enrollments, and records the catalog; a failed read keeps the last one.
func TestModelCatalogRefreshDiscoversCodexEnrollmentsForACodexReviewer(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	at := time.Now().UTC().Add(-time.Hour)
	seedUsageEnrollment(t, st, "auth-claude", "enroll-claude", "pool-claude", domain.HarnessClientClaudeCode, at)
	seedUsageEnrollment(t, st, "auth-codex", "enroll-codex", "pool-codex", domain.HarnessClientCodexCLI, at)
	token, err := ward.NewSetupToken([]byte(authAddTestToken))
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	readErr := error(nil)
	var logs bytes.Buffer
	observedAt := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	refresh := modelCatalogRefresh{
		store: st,
		readToken: func(_ context.Context, id domain.ClientEnrollmentID, _ int) (ward.SetupToken, error) {
			order = append(order, string(id))
			return token, nil
		},
		discover: func(context.Context, string, ward.SetupToken) (ward.ModelReport, error) {
			return discoveredReport("opus"), nil
		},
		image:      modelDiscoveryTestImage,
		codexImage: codexModelDiscoveryTestImage,
		readCodex: func(_ context.Context, id domain.ClientEnrollmentID) (ward.LineupProbeCredential, error) {
			order = append(order, string(id))
			return ward.LineupProbeCredential{}, readErr
		},
		discoverCodex: func(_ context.Context, image string, _ ward.LineupProbeCredential) (ward.ModelReport, error) {
			if image != codexModelDiscoveryTestImage {
				t.Errorf("Codex discovery ran in %q, want the review image", image)
			}
			return codexDiscoveredReport("gpt-6.1-sol", "gpt-6-astra"), nil
		},
		now: func() time.Time { return observedAt }, logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	refresh.run(t.Context())
	if !slices.Equal(order, []string{"enroll-claude", "enroll-codex"}) {
		t.Fatalf("read order = %v, want Claude then Codex", order)
	}
	catalog, err := readModelCatalog(t, st, "enroll-codex")
	if err != nil {
		t.Fatal(err)
	}
	if catalog.HarnessBuild != "codex-cli 0.162.1" || !catalog.ObservedAt.Equal(observedAt) || len(catalog.Models) != 2 {
		t.Fatalf("catalog = %+v", catalog)
	}

	readErr = wardstore.ErrCredentialReadMutationLeaseLive
	refresh.now = func() time.Time { return observedAt.Add(time.Hour) }
	refresh.run(t.Context())
	kept, err := readModelCatalog(t, st, "enroll-codex")
	if err != nil || !kept.ObservedAt.Equal(observedAt) {
		t.Fatalf("after a refused read: catalog = %+v, err = %v; want the last one kept", kept, err)
	}
	if !strings.Contains(logs.String(), "model discovery failed") || !strings.Contains(logs.String(), "mutation lease is live") {
		t.Fatalf("logs = %s", logs.String())
	}
}

// TestModelCatalogRefreshSkipsCodexWithoutACodexReviewer: a Claude reviewer,
// an API-key Codex reviewer, whose input root holds no OAuth store, or an
// attended daemon reads no Codex credential and logs one line.
func TestModelCatalogRefreshSkipsCodexWithoutACodexReviewer(t *testing.T) {
	t.Parallel()
	codexReviewer := ClaudeDriverConfig{
		OperatingMode: domain.ModeUnattended, ReviewImage: codexModelDiscoveryTestImage,
		ReviewInputRoot: "/private/review", ReviewAuthMode: ward.CodexAuthSubscription,
	}
	claudeReviewer := codexReviewer
	claudeReviewer.ReviewAuthMode = ward.CodexAuthSetupToken
	apiKeyReviewer := codexReviewer
	apiKeyReviewer.ReviewAuthMode = ward.CodexAuthAPIKey
	attended := codexReviewer
	attended.OperatingMode = domain.ModeAttendedDev
	if !codexReviewerConfigured(codexReviewer) {
		t.Fatal("a Codex reviewer is not recognised")
	}
	for name, cfg := range map[string]ClaudeDriverConfig{
		"Claude reviewer": claudeReviewer, "API-key Codex reviewer": apiKeyReviewer, "attended": attended,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if codexReviewerConfigured(cfg) {
				t.Fatal("Codex discovery would run")
			}
			st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
			seedUsageEnrollment(t, st, "auth-codex", "enroll-codex", "pool-codex", domain.HarnessClientCodexCLI,
				time.Now().UTC().Add(-time.Hour))
			var logs bytes.Buffer
			refresh := newModelCatalogRefresh(st, cfg, "container", nil,
				func(context.Context, string, ward.LineupProbeCredential) (ward.ModelReport, error) {
					t.Error("Codex discovery ran")
					return ward.ModelReport{}, nil
				}, slog.New(slog.NewTextHandler(&logs, nil)))
			refresh.run(t.Context())
			if strings.Count(logs.String(), "Codex model discovery skipped") != 1 {
				t.Fatalf("logs = %s", logs.String())
			}
		})
	}
}
