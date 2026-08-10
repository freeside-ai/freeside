package ward

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type codexReviewLifecycleConfig struct {
	EgressDialContext  dialContextFunc
	EgressProxyTimeout time.Duration
	ExportRoot         string
	SeedReadyDir       string
	SeedStageDir       string
	SeedTimeout        time.Duration
	TeardownTimeout    time.Duration
}

func newCodexReviewLifecycleConfig(cfg Config) codexReviewLifecycleConfig {
	return codexReviewLifecycleConfig{
		EgressDialContext:  cfg.EgressDialContext,
		EgressProxyTimeout: cfg.EgressProxyTimeout,
		ExportRoot:         cfg.ExportRoot,
		SeedReadyDir:       cfg.SeedReadyDir,
		SeedStageDir:       cfg.SeedStageDir,
		SeedTimeout:        cfg.SeedTimeout,
		TeardownTimeout:    cfg.TeardownTimeout,
	}
}

func (c codexReviewLifecycleConfig) seedConfig() Config {
	return Config{
		SeedReadyDir: c.SeedReadyDir,
		SeedStageDir: c.SeedStageDir,
		SeedTimeout:  c.SeedTimeout,
	}
}

// CodexReviewLifecycle owns the runtime topology, durable reconstruction, and
// in-process serialization for Codex reviews. Construct one shared instance
// for every source and recovery path that can address the same run IDs.
type CodexReviewLifecycle struct {
	runtimeOps
	cfg codexReviewLifecycleConfig

	// codexReviewMu guards per-run lifecycle gates. A rejected request must not
	// recover a preparing intent while the in-process launch still creates and
	// journals resources under that same durable owner.
	codexReviewMu   sync.Mutex
	codexReviewRuns map[string]chan struct{}
}

// NewCodexReviewLifecycle builds the Codex review runtime owner. Config is
// defaulted, validated, and frozen independently of Backend so the lifecycle
// cannot depend on handoff conformance or lease state.
func NewCodexReviewLifecycle(rt Runtime, cfg Config) (*CodexReviewLifecycle, error) {
	if rt == nil {
		return nil, fmt.Errorf("%w: Runtime is required", ErrInvalidConfig)
	}
	var err error
	cfg, err = prepareConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &CodexReviewLifecycle{
		runtimeOps:      newRuntimeOps(rt, cfg),
		cfg:             newCodexReviewLifecycleConfig(cfg),
		codexReviewRuns: map[string]chan struct{}{},
	}, nil
}

func (l *CodexReviewLifecycle) valid() bool {
	return l != nil && l.rt != nil && l.codexReviewRuns != nil
}

// Codex workspaces never acquire the handoff gate's auth-store lease. Passing
// empty hooks keeps that capability outside the review lifecycle.
func (l *CodexReviewLifecycle) teardown(
	ctx context.Context, names handoffNames, st *runState,
) error {
	return l.runtimeOps.teardown(ctx, names, st, runtimeTeardownHooks{})
}

func (l *CodexReviewLifecycle) seedWorkspace(
	ctx context.Context, hs HandoffSpec, names handoffNames, st *runState,
) error {
	return l.runtimeOps.seedWorkspace(ctx, hs, names, st, runtimeSeedHooks{})
}

func (l *CodexReviewLifecycle) observeSeededBase(
	ctx context.Context, hs HandoffSpec, names handoffNames, st *runState,
) (string, error) {
	return l.runtimeOps.observeSeededBase(ctx, hs, names, st, runtimeSeedHooks{})
}
