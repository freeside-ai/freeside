package ward

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// Backend realizes the fresh_vm_read_only_volume_handoff isolation class on
// a Runtime. Construct it with New; the zero value declares nothing and
// gates nothing.
type Backend struct {
	rt          Runtime
	cfg         Config
	initialized bool
	// networkless and providerEgress are the two suite-earned capability
	// flags. Both proofs come from the same Full pass, so one generation
	// guards them jointly: a pass is all-or-nothing, and a split publication
	// would declare a capability the pass did not finish proving.
	networkless    atomic.Bool
	providerEgress atomic.Bool
	// proofMu makes the generation check and capability publication one
	// atomic decision across overlapping Full passes.
	proofMu         sync.Mutex
	proofGeneration uint64
	// leaseMu guards activeLeases: the identities whose §5.4 mutation
	// window a handoff in this process currently holds. The store
	// serializes distinct holders; this closes the residual same-holder
	// hole, where a caller reusing one holder ID for two concurrent
	// handoffs would converge on one window (see acquireAuthStoreLease).
	leaseMu      sync.Mutex
	activeLeases map[domain.AuthIdentityID]bool
}

// Compile-time contract assertion (exec package convention).
var _ exec.RunnerBackend = (*Backend)(nil)

// New builds a Backend over rt, applying cfg defaults and refusing an
// invalid configuration.
func New(rt Runtime, cfg Config) (*Backend, error) {
	if rt == nil {
		return nil, fmt.Errorf("%w: Runtime is required", ErrInvalidConfig)
	}
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	// Config is caller-owned. Freeze every reference field before it becomes
	// the expected allowlist that runtime-observed state is compared against.
	cfg.ExporterCommand = slices.Clone(cfg.ExporterCommand)
	cfg.ProviderEndpoints = slices.Clone(cfg.ProviderEndpoints)
	return &Backend{rt: rt, cfg: cfg, initialized: true, activeLeases: map[domain.AuthIdentityID]bool{}}, nil
}

// Name identifies the backend in policy, refusals, and audit records.
func (b *Backend) Name() string { return BackendName }

// beginConformanceProof starts a new proof generation: it clears the
// declaration and, still under the guard, runs the durable superseding step
// (nil when no recorder is configured). Durable-first mirrors
// concludeConformanceProof's persist-then-publish: the previous pass's row
// must stop admitting before this recheck runs a single probe, or an
// admission whose snapshot froze just before this begin would ride the old
// row for the recheck's whole duration. A failed superseding append aborts
// the begin (the declaration stays cleared, which is the safe direction).
func (b *Backend) beginConformanceProof(record func() error) (uint64, error) {
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	b.proofGeneration++
	b.networkless.Store(false)
	b.providerEgress.Store(false)
	if record != nil {
		if err := record(); err != nil {
			return 0, err
		}
	}
	return b.proofGeneration, nil
}

// concludeConformanceProof runs a pass's durable record step and then
// publishes its result, all while holding the currency judgment. A
// superseded generation publishes and records nothing, so an older
// overlapping pass that finishes last cannot resurrect a superseded proof.
// The record callback runs under proofMu on purpose: releasing the mutex
// between the currency check and the append would let a newer pass begin,
// fail, and append its row first, leaving the older pass's stale success as
// durable-latest and inverting the append-only log's supersession order. The
// cost is that a concurrent begin blocks for one store append, which is
// nothing against a suite pass.
//
// Persist-then-publish ordering is load-bearing: the flags have been clear
// since beginConformanceProof, and they must stay unobservable until the
// pass's own row is durable-latest. Publishing first would open a window
// where a concurrent admission is enabled by the fresh declaration yet gated
// against the previous pass's stale row; if the append then failed, that
// admission would stand on evidence this pass never persisted. A failed
// append therefore publishes false: an unpersisted proof is never declared.
func (b *Backend) concludeConformanceProof(
	generation uint64, proved bool, record func() error,
) (published bool, recordErr error) {
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	if generation != b.proofGeneration {
		return false, nil
	}
	if record != nil {
		recordErr = record()
	}
	proved = proved && recordErr == nil
	b.networkless.Store(proved)
	b.providerEgress.Store(proved)
	return true, recordErr
}

// finishConformanceProof is concludeConformanceProof with no durable record
// step, for callers and tests that only publish.
func (b *Backend) finishConformanceProof(generation uint64, proved bool) bool {
	published, _ := b.concludeConformanceProof(generation, proved, nil)
	return published
}

// Capabilities returns the backend's declared capability set, freshly built
// on every call so no caller can mutate it. exec.CheckCapabilities freezes
// the result at admission.
//
// The base declaration is exactly what the handoff spike proved on Apple
// container 1.1.0:
//
//   - supports_detachable_workspace: a named volume survives writer exit and
//     deletion and reattaches to a different VM.
//   - supports_post_exit_export: composed, narrow reading only — the runtime
//     exports a stopped root filesystem, never a mounted workspace volume
//     directly; the exporter copies the read-only workspace into its rootfs
//     first (the accepted workspace-copy cost).
//   - supports_read_only_remount: the same volume mounts rw in the writer
//     and ro in the exporter.
//
// supports_networkless_export and supports_enforced_provider_egress are
// added only after Suite.Full has passed: the runtime-observed empty-network
// check with its DNS/direct-connect probe, and the in-writer egress probes
// (declared-provider CONNECT success, undeclared-authority refusal, DNS and
// direct-IP refusal). A new or
// failed backend therefore refuses an unattended policy minimum until the
// proof has run. supports_credential_volume_detach and
// supports_workspace_snapshot are refuted on this runtime and are never
// declared: the spike proved a guest
// unmount is not a credential-device detach (the refuted same-VM fallback
// class), and volume snapshotting has no public support.
func (b *Backend) Capabilities() exec.CapabilitySet {
	if b == nil || !b.initialized {
		return exec.NewCapabilitySet()
	}
	caps := exec.NewCapabilitySet(
		exec.CapDetachableWorkspace,
		exec.CapPostExitExport,
		exec.CapReadOnlyRemount,
	)
	if b.networkless.Load() {
		caps[exec.CapNetworklessExport] = struct{}{}
	}
	if b.providerEgress.Load() {
		caps[exec.CapEnforcedProviderEgress] = struct{}{}
	}
	return caps
}

// provenCapabilities is the exact set a green Full pass proves: the frozen
// base declaration plus both suite-earned capabilities. It is spelled out
// rather than read back from the live declaration so the durable conformance
// record states what the pass proved, not whatever the flags happen to say
// when the record is built; a drift test binds it to the domain's class
// ceiling.
func provenCapabilities() []exec.Capability {
	return []exec.Capability{
		exec.CapDetachableWorkspace,
		exec.CapPostExitExport,
		exec.CapReadOnlyRemount,
		exec.CapNetworklessExport,
		exec.CapEnforcedProviderEgress,
	}
}
