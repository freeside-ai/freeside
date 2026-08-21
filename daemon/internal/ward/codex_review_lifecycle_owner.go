package ward

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

type codexReviewLifecycleConfig struct {
	EgressDialContext  dialContextFunc
	EgressProxyTimeout time.Duration
	ExportRoot         string
	HandoffTimeout     time.Duration
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
		HandoffTimeout:     cfg.HandoffTimeout,
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
	cfg                       codexReviewLifecycleConfig
	authorizeRuntimeResources RuntimeResourceAuthorizer

	// provider supplies the vendor-varying launch decisions (review command,
	// topology-version tag). It is unexported so external callers cannot set it;
	// NewCodexReviewLifecycle defaults it to the Codex provider, and the
	// same-package Claude runtime (#865) injects its own.
	provider reviewProvider

	// codexReviewMu guards per-run lifecycle gates. A rejected request must not
	// recover a preparing intent while the in-process launch still creates and
	// journals resources under that same durable owner.
	codexReviewMu   sync.Mutex
	codexReviewRuns map[string]chan struct{}
}

// NewCodexReviewLifecycle builds the Codex review runtime owner. Config is
// defaulted, validated, and frozen independently of Backend. Review-specific
// host auth dependencies arrive through CodexReviewConfig at launch.
func NewCodexReviewLifecycle(
	rt Runtime, cfg Config, authorizeRuntimeResources RuntimeResourceAuthorizer,
) (*CodexReviewLifecycle, error) {
	return newReviewLifecycle(codexReviewProvider{}, rt, cfg, authorizeRuntimeResources)
}

func newReviewLifecycle(
	provider reviewProvider, rt Runtime, cfg Config, authorizeRuntimeResources RuntimeResourceAuthorizer,
) (*CodexReviewLifecycle, error) {
	if rt == nil {
		return nil, fmt.Errorf("%w: Runtime is required", ErrInvalidConfig)
	}
	var err error
	cfg, err = prepareConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &CodexReviewLifecycle{
		runtimeOps:                newRuntimeOps(rt, cfg),
		cfg:                       newCodexReviewLifecycleConfig(cfg),
		authorizeRuntimeResources: authorizeRuntimeResources,
		provider:                  provider,
		codexReviewRuns:           map[string]chan struct{}{},
	}, nil
}

func (l *CodexReviewLifecycle) valid() bool {
	return l != nil && l.rt != nil && l.codexReviewRuns != nil
}

// reviewProvider returns the lifecycle's vendor seam, defaulting to Codex when
// a direct struct construction (e.g. a focused test) left it unset.
// NewCodexReviewLifecycle always populates it.
func (l *CodexReviewLifecycle) reviewProvider() reviewProvider {
	if l.provider != nil {
		return l.provider
	}
	return codexReviewProvider{}
}

func (l *CodexReviewLifecycle) authorizeRuntime(
	ctx context.Context, resources RuntimeResourceNames,
) error {
	if l.authorizeRuntimeResources == nil {
		return nil
	}
	resources.Containers = slices.Clone(resources.Containers)
	resources.Volumes = slices.Clone(resources.Volumes)
	resources.Networks = slices.Clone(resources.Networks)
	return l.authorizeRuntimeResources(ctx, resources)
}

// Running Codex workspaces never hold the handoff gate's auth-store lease.
// Review launch holds its separate host-auth lease through container start,
// then releases it before returning; teardown therefore receives no auth hooks.
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
