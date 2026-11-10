package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

type stubClaudeUsageSink struct{}

func (stubClaudeUsageSink) ClaudeUsageCaptured(
	context.Context, domain.UsagePoolID, domain.ClientEnrollmentID, int, exec.ClaudeUsageCapture,
) error {
	return nil
}

// TestProductionEngineOptionsInstallOptionalPartsOnlyWhenSet pins the
// builder's nil contract: required parts always become an option (here
// every one is zero, so the engine's own checks would refuse them), and
// each optional part adds exactly one option only when set.
func TestProductionEngineOptionsInstallOptionalPartsOnlyWhenSet(t *testing.T) {
	t.Parallel()
	// Lock, capacity, admission, derivation, delivery validation,
	// specification, cancellation, publication.
	const required = 8
	if got := len(ProductionEngine{}.Options()); got != required {
		t.Fatalf("options with no optional part = %d, want %d", got, required)
	}
	for name, set := range map[string]func(*ProductionEngine){
		"image selection": func(p *ProductionEngine) {
			p.SelectImage = func(context.Context, domain.InvocationID, *domain.ImageUpkeepRefusal) (domain.ImageRef, error) {
				return "", nil
			}
		},
		"inference":      func(p *ProductionEngine) { p.Inference = &inference.Client{} },
		"health refresh": func(p *ProductionEngine) { p.HealthRefresh = func(context.Context) error { return nil } },
		"logger":         func(p *ProductionEngine) { p.Logger = slog.New(slog.DiscardHandler) },
		"usage sink":     func(p *ProductionEngine) { p.UsageSink = stubClaudeUsageSink{} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var p ProductionEngine
			set(&p)
			if got := len(p.Options()); got != required+1 {
				t.Fatalf("options with %s set = %d, want %d", name, got, required+1)
			}
		})
	}
}
