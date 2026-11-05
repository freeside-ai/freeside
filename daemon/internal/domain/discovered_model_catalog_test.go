package domain_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// validDiscoveredModelCatalog is three models as a Claude CLI reports them:
// an alias that resolves to a dated model, a full id, and a model without
// effort support. It is both the golden and the validation-positive case.
func validDiscoveredModelCatalog() domain.DiscoveredModelCatalog {
	return domain.DiscoveredModelCatalog{
		EnrollmentID: "claude-a/claude_code", HarnessBuild: "2.1.282 (Claude Code)",
		ObservedAt: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		Models: []domain.DiscoveredModel{
			{
				ID: "opus", ResolvedModel: new("claude-opus-5-5"), DisplayName: "Opus 5.5",
				EffortLevels: []string{"low", "medium", "high", "xhigh", "max"},
			},
			{
				ID: "claude-sonnet-4-6", ResolvedModel: new("claude-sonnet-4-6"), DisplayName: "Sonnet 4.6",
				EffortLevels: []string{"low", "medium", "high", "max"},
			},
			{ID: "claude-haiku-4-5-20251001", DisplayName: "Haiku 4.5", EffortLevels: []string{}},
		},
	}
}

func TestDiscoveredModelCatalogGolden(t *testing.T) {
	catalog := validDiscoveredModelCatalog()
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "discovered_model_catalog", append(body, '\n'))
}

func TestDiscoveredModelCatalogValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   error
		mutate func(*domain.DiscoveredModelCatalog)
	}{
		{"no enrollment", domain.ErrEmptyID, func(c *domain.DiscoveredModelCatalog) { c.EnrollmentID = "" }},
		{"no build", domain.ErrEmptyField, func(c *domain.DiscoveredModelCatalog) { c.HarnessBuild = "" }},
		{"no time", domain.ErrMissingTimestamp, func(c *domain.DiscoveredModelCatalog) { c.ObservedAt = time.Time{} }},
		{"local time", domain.ErrTimestampNotUTC, func(c *domain.DiscoveredModelCatalog) {
			c.ObservedAt = c.ObservedAt.In(time.FixedZone("x", 3600))
		}},
		{"no models", domain.ErrNoDiscoveredModels, func(c *domain.DiscoveredModelCatalog) { c.Models = nil }},
		{"duplicate model", domain.ErrDuplicate, func(c *domain.DiscoveredModelCatalog) {
			c.Models[1].ID = c.Models[0].ID
		}},
		{"empty model id", domain.ErrEmptyField, func(c *domain.DiscoveredModelCatalog) { c.Models[0].ID = "" }},
		{"empty resolved model", domain.ErrEmptyField, func(c *domain.DiscoveredModelCatalog) {
			c.Models[0].ResolvedModel = new("")
		}},
		{"empty display name", domain.ErrEmptyField, func(c *domain.DiscoveredModelCatalog) { c.Models[0].DisplayName = "" }},
		{"null efforts", domain.ErrEmptyField, func(c *domain.DiscoveredModelCatalog) { c.Models[2].EffortLevels = nil }},
		{"empty effort", domain.ErrEmptyField, func(c *domain.DiscoveredModelCatalog) {
			c.Models[0].EffortLevels = []string{"low", ""}
		}},
		{"duplicate effort", domain.ErrDuplicate, func(c *domain.DiscoveredModelCatalog) {
			c.Models[0].EffortLevels = []string{"high", "high"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := validDiscoveredModelCatalog()
			tc.mutate(&catalog)
			if err := catalog.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}
