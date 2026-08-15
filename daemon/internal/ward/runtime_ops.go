package ward

import (
	"context"
	"time"
)

// runtimeOps owns the runtime-and-configuration primitives shared by the
// handoff gate and the Codex review lifecycle. Domain-specific state remains
// on those two owners and is passed in only where a primitive needs it.
type runtimeOps struct {
	rt  Runtime
	cfg runtimeOpsConfig
}

type runtimeOpsConfig struct {
	ExporterImage   string
	WorkspaceTarget string
	SeedRoot        string
	SeedStageDir    string
	SeedReadyDir    string
	BaseProofPath   string
	SeedTimeout     time.Duration
	TeardownTimeout time.Duration
	PollInterval    time.Duration
	MaxSeedBytes    int64
	MaxSeedEntries  int
	MaxArchiveBytes int64
	Sleep           func(context.Context, time.Duration) error
	checkWorkspace  func(context.Context, string, int) error
}

func newRuntimeOps(rt Runtime, cfg Config) runtimeOps {
	checkWorkspace := cfg.checkSeedWorkspace
	if checkWorkspace == nil {
		checkWorkspace = checkCanonicalSeedWorkspaceClean
	}
	return runtimeOps{rt: rt, cfg: runtimeOpsConfig{
		ExporterImage:   cfg.ExporterImage,
		WorkspaceTarget: cfg.WorkspaceTarget,
		SeedRoot:        cfg.SeedRoot,
		SeedStageDir:    cfg.SeedStageDir,
		SeedReadyDir:    cfg.SeedReadyDir,
		BaseProofPath:   cfg.BaseProofPath,
		SeedTimeout:     cfg.SeedTimeout,
		TeardownTimeout: cfg.TeardownTimeout,
		PollInterval:    cfg.PollInterval,
		MaxSeedBytes:    cfg.MaxSeedBytes,
		MaxSeedEntries:  cfg.MaxSeedEntries,
		MaxArchiveBytes: cfg.MaxArchiveBytes,
		Sleep:           cfg.Sleep,
		checkWorkspace:  checkWorkspace,
	}}
}

func (c runtimeOpsConfig) seedConfig() Config {
	return Config{
		ExporterImage:   c.ExporterImage,
		WorkspaceTarget: c.WorkspaceTarget,
		SeedRoot:        c.SeedRoot,
		SeedStageDir:    c.SeedStageDir,
		SeedReadyDir:    c.SeedReadyDir,
		BaseProofPath:   c.BaseProofPath,
		SeedTimeout:     c.SeedTimeout,
		MaxSeedBytes:    c.MaxSeedBytes,
		MaxSeedEntries:  c.MaxSeedEntries,
	}
}

type runtimeTeardownHooks struct {
	freeLeaseSlot         func(*runState)
	releaseAuthStoreLease func(context.Context, *runState) []string
	teardownNetwork       func(context.Context, string, objectClaim, Label) error
	reapUnlistedContainer func(context.Context, string, objectClaim, Label) error
}

func (h runtimeTeardownHooks) free(st *runState) {
	if h.freeLeaseSlot != nil {
		h.freeLeaseSlot(st)
	}
}

func (h runtimeTeardownHooks) release(ctx context.Context, st *runState) []string {
	if h.releaseAuthStoreLease == nil {
		return nil
	}
	return h.releaseAuthStoreLease(ctx, st)
}

func (h runtimeTeardownHooks) teardownNet(
	ctx context.Context, fallback runtimeOps, name string, claim objectClaim, owner Label,
) error {
	if h.teardownNetwork != nil {
		return h.teardownNetwork(ctx, name, claim, owner)
	}
	return fallback.teardownNetwork(ctx, name, claim, owner)
}

func (h runtimeTeardownHooks) reapUnlisted(
	ctx context.Context, fallback runtimeOps, id string, claim objectClaim, owner Label,
) error {
	if h.reapUnlistedContainer != nil {
		return h.reapUnlistedContainer(ctx, id, claim, owner)
	}
	return fallback.reapUnlistedContainer(ctx, id, claim, owner)
}

type runtimeSeedHooks struct {
	copyIntoSeeder func(context.Context, string, string, string) error
	readBaseProof  func(context.Context, string, string, *runState) (string, error)
}

// Backend delegates the shared primitives through runtimeOps so its existing
// handoff surface and method count remain stable while the implementation is
// reusable by the review lifecycle.
func (b *Backend) materializeRootFS(ctx context.Context, id, tarPath string, c Check) error {
	return b.runtimeOps.materializeRootFS(ctx, id, tarPath, c)
}

func (b *Backend) waitStopped(
	ctx context.Context, id string, claim objectClaim, ownershipLabel Label, timeout time.Duration,
) error {
	return b.runtimeOps.waitStopped(ctx, id, claim, ownershipLabel, timeout)
}

func (b *Backend) verifyContainerAbsent(
	ctx context.Context, id string, claim objectClaim, ownershipLabel Label, c Check,
) error {
	return b.runtimeOps.verifyContainerAbsent(ctx, id, claim, ownershipLabel, c)
}

func (b *Backend) teardown(ctx context.Context, names handoffNames, st *runState) error {
	return b.runtimeOps.teardown(ctx, names, st, runtimeTeardownHooks{
		freeLeaseSlot:         b.freeLeaseSlot,
		releaseAuthStoreLease: b.releaseAuthStoreLease,
		teardownNetwork:       b.teardownNetwork,
		reapUnlistedContainer: b.reapUnlistedContainer,
	})
}

func (b *Backend) teardownNetwork(
	ctx context.Context, name string, claim objectClaim, ownershipLabel Label,
) error {
	return b.runtimeOps.teardownNetwork(ctx, name, claim, ownershipLabel)
}

func (b *Backend) containerEvidence(
	ctx context.Context, candidate ContainerSummary, claim objectClaim, ownershipLabel Label,
) (objectEvidence, error) {
	return b.runtimeOps.containerEvidence(ctx, candidate, claim, ownershipLabel)
}

func (b *Backend) volumeEvidence(
	ctx context.Context, candidate VolumeSummary, claim objectClaim, ownershipLabel Label,
) (objectEvidence, error) {
	return b.runtimeOps.volumeEvidence(ctx, candidate, claim, ownershipLabel)
}

func (b *Backend) reapUnlistedContainer(
	ctx context.Context, id string, claim objectClaim, ownershipLabel Label,
) error {
	return b.runtimeOps.reapUnlistedContainer(ctx, id, claim, ownershipLabel)
}

func (b *Backend) reapContainer(ctx context.Context, cs ContainerSummary) error {
	return b.runtimeOps.reapContainer(ctx, cs)
}

func (b *Backend) seedWorkspace(
	ctx context.Context, hs HandoffSpec, names handoffNames, st *runState,
) error {
	return b.runtimeOps.seedWorkspace(ctx, hs, names, st, runtimeSeedHooks{
		copyIntoSeeder: b.copyIntoSeeder,
	})
}

func (b *Backend) observeSeededBase(
	ctx context.Context, hs HandoffSpec, names handoffNames, st *runState,
) (string, error) {
	return b.runtimeOps.observeSeededBase(ctx, hs, names, st, runtimeSeedHooks{
		readBaseProof: b.readBaseProof,
	})
}

func (b *Backend) readBaseProof(
	ctx context.Context, runID, id string, st *runState,
) (string, error) {
	return b.runtimeOps.readBaseProof(ctx, runID, id, st)
}

func (b *Backend) copyIntoSeeder(ctx context.Context, id, hostDir, targetDir string) error {
	return b.runtimeOps.copyIntoSeeder(ctx, id, hostDir, targetDir)
}
