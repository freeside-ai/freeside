package ward

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	rt  Runtime
	cfg Config
	// daemonIdentity binds restored conformance to the exact freesided/ward
	// executable that earned it. Rebuilding any gate implementation changes
	// the binary identity and requires a fresh Full pass.
	daemonIdentity string
	// runtimeIdentity binds conformance to the executable bytes observed at
	// backend construction. PreJob recomputes it before every real dispatch,
	// so replacing a CLI in place clears the practical admission path even
	// though its filesystem path is unchanged.
	runtimeIdentity string
	initialized     bool
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
	// codexReviewMu guards per-run lifecycle gates. A rejected request must not
	// recover a preparing intent while the in-process launch still creates and
	// journals resources under that same durable owner.
	codexReviewMu   sync.Mutex
	codexReviewRuns map[string]chan struct{}
}

type conformanceConfiguration struct {
	Version           string   `json:"version"`
	Daemon            string   `json:"daemon"`
	Runtime           string   `json:"runtime"`
	AgentImage        string   `json:"agent_image"`
	ProviderEndpoints []string `json:"provider_endpoints"`
	ExporterImage     string   `json:"exporter_image"`
	ExporterCommand   []string `json:"exporter_command"`
	SeedRoot          string   `json:"seed_root"`
	ExportRoot        string   `json:"export_root"`
	WorkspaceTarget   string   `json:"workspace_target"`
	HandoffDir        string   `json:"handoff_dir"`
	ProofPath         string   `json:"proof_path"`
	SeedStageDir      string   `json:"seed_stage_dir"`
	SeedReadyDir      string   `json:"seed_ready_dir"`
	BaseProofPath     string   `json:"base_proof_path"`
	CredProofPath     string   `json:"cred_proof_path"`
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
	runtimeIdentity, err := runtimeConfigurationIdentity(rt)
	if err != nil {
		return nil, fmt.Errorf("%w: Runtime executable identity: %w", ErrInvalidConfig, err)
	}
	daemonIdentity, err := daemonConfigurationIdentity()
	if err != nil {
		return nil, fmt.Errorf("%w: daemon executable identity: %w", ErrInvalidConfig, err)
	}
	// Config is caller-owned. Freeze every reference field before it becomes
	// the expected allowlist that runtime-observed state is compared against.
	cfg.ExporterCommand = slices.Clone(cfg.ExporterCommand)
	cfg.ProviderEndpoints = slices.Clone(cfg.ProviderEndpoints)
	return &Backend{
		rt: rt, cfg: cfg,
		daemonIdentity:  daemonIdentity,
		runtimeIdentity: runtimeIdentity,
		initialized:     true,
		activeLeases:    map[domain.AuthIdentityID]bool{},
		codexReviewRuns: map[string]chan struct{}{},
	}, nil
}

// RestoreConformance republishes the suite-earned capabilities from the
// durable latest proof after a daemon restart. The record is accepted only
// before this backend has begun a live proof: once a recheck starts, its
// generation and cleared declaration are authoritative, and an older
// persisted success must never resurrect them.
func (b *Backend) RestoreConformance(record domain.BackendConformance) error {
	if b == nil || !b.initialized {
		return fmt.Errorf("%w: backend is not initialized", ErrInvalidConfig)
	}
	if err := record.Validate(); err != nil {
		return fmt.Errorf("restore backend conformance: %w", err)
	}
	if record.Backend != domain.BackendFreshVMReadOnlyVolumeHandoff {
		return fmt.Errorf("restore backend conformance for %q on %q",
			record.Backend, BackendName)
	}
	if record.Outcome != domain.ConformancePassed {
		return fmt.Errorf("restore backend conformance generation %d with outcome %q",
			record.Generation, record.Outcome)
	}
	if record.Generation == 0 {
		return fmt.Errorf("restore unpersisted backend conformance generation")
	}
	if record.ConfigurationDigest != b.ConfigurationDigest() {
		return fmt.Errorf(
			"restore backend conformance generation %d for configuration %s on active %s",
			record.Generation, record.ConfigurationDigest, b.ConfigurationDigest())
	}

	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	if b.proofGeneration != 0 {
		return fmt.Errorf("restore backend conformance generation %d after live generation %d began",
			record.Generation, b.proofGeneration)
	}
	b.proofGeneration = record.Generation
	b.networkless.Store(record.Capabilities.Has(domain.CapNetworklessExport))
	b.providerEgress.Store(record.Capabilities.Has(domain.CapEnforcedProviderEgress))
	return nil
}

// ConfigurationDigest is the stable identity of the runtime configuration a
// Full conformance pass proves. It deliberately excludes collaborators and
// operational budgets that do not change the runtime topology under proof.
func (b *Backend) ConfigurationDigest() domain.Digest {
	if b == nil || !b.initialized {
		return ""
	}
	endpoints := slices.Clone(b.cfg.ProviderEndpoints)
	slices.Sort(endpoints)
	body, err := json.Marshal(conformanceConfiguration{
		Version:           "freeside.ward.conformance-configuration/v4",
		Daemon:            b.daemonIdentity,
		Runtime:           b.runtimeIdentity,
		AgentImage:        b.cfg.AgentImage,
		ProviderEndpoints: endpoints,
		ExporterImage:     b.cfg.ExporterImage,
		ExporterCommand:   b.cfg.ExporterCommand,
		SeedRoot:          b.cfg.SeedRoot,
		ExportRoot:        b.cfg.ExportRoot,
		WorkspaceTarget:   b.cfg.WorkspaceTarget,
		HandoffDir:        b.cfg.HandoffDir,
		ProofPath:         b.cfg.ProofPath,
		SeedStageDir:      b.cfg.SeedStageDir,
		SeedReadyDir:      b.cfg.SeedReadyDir,
		BaseProofPath:     b.cfg.BaseProofPath,
		CredProofPath:     b.cfg.CredProofPath,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal ward conformance configuration: %v", err))
	}
	return domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(body)))
}

func runtimeConfigurationIdentity(rt Runtime) (string, error) {
	cli, ok := rt.(*CLIRuntime)
	if !ok {
		return fmt.Sprintf("%T", rt), nil
	}
	return executableIdentity(cli.bin)
}

func daemonConfigurationIdentity() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return executableIdentity(path)
}

func executableIdentity(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: hashes the validated runtime path or this process's executable
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s@sha256:%x", path, h.Sum(nil)), nil
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
