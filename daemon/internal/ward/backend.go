package ward

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// Backend realizes the fresh_vm_read_only_volume_handoff isolation class on
// a Runtime. Construct it with New; the zero value declares nothing and
// gates nothing.
type Backend struct {
	rt         Runtime
	cfg        Config
	runtimeOps runtimeOps
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
	// proofMu guards proofs and proofCounter, and makes each image's
	// generation check and capability publication one atomic decision
	// across overlapping Full passes.
	proofMu sync.Mutex
	// proofs holds, per writer image, the proof state of the configuration
	// that image yields (#3211). An image with no entry has never been
	// proved or restored in this process.
	proofs map[string]*imageProof
	// proofCounter stamps every begin, whatever its image, so a
	// configuration retired and proved again never reuses a generation an
	// older pass still holds.
	proofCounter uint64
	// leaseMu guards activeLeases and activeReads: the identities whose
	// §5.4 mutation window, and the identity-and-holder pairs whose shared
	// read window, a handoff in this process currently holds. The store
	// serializes distinct holders; this closes the residual same-holder
	// hole, where a caller reusing one holder ID for two concurrent
	// handoffs would converge on one window (see acquireAuthStoreLease).
	leaseMu      sync.Mutex
	activeLeases map[domain.AuthIdentityID]bool
	activeReads  map[domain.AuthIdentityID]map[domain.InvocationID]bool
	// live registers each running writer for the live transcript read
	// (live_transcript.go). Its zero value is ready.
	live liveWriters
}

// imageProof is one writer image's in-memory proof state.
type imageProof struct {
	// generation is the stamp of the newest pass begun for the image, or
	// zero when none has begun in this process. Only that pass may
	// conclude.
	generation uint64
	// restored is the durable generation a restart republished, zero when
	// none was.
	restored uint64
	// published is whether the image's two suite-earned capabilities are
	// declared. Both proofs come from the same Full pass, so one flag
	// covers them: a pass is all-or-nothing, and a split publication would
	// declare a capability the pass did not finish proving.
	published bool
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
var _ exec.ImageBoundBackend = (*Backend)(nil)

// New builds a Backend over rt, applying cfg defaults and refusing an
// invalid configuration.
func New(rt Runtime, cfg Config) (*Backend, error) {
	if rt == nil {
		return nil, fmt.Errorf("%w: Runtime is required", ErrInvalidConfig)
	}
	var err error
	cfg, err = prepareConfig(cfg)
	if err != nil {
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
	return &Backend{
		rt:              rt,
		cfg:             cfg,
		runtimeOps:      newRuntimeOps(rt, cfg),
		daemonIdentity:  daemonIdentity,
		runtimeIdentity: runtimeIdentity,
		initialized:     true,
		proofs:          map[string]*imageProof{},
		activeLeases:    map[domain.AuthIdentityID]bool{},
		activeReads:     map[domain.AuthIdentityID]map[domain.InvocationID]bool{},
	}, nil
}

func prepareConfig(cfg Config) (Config, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	// Config is caller-owned. Freeze every reference field before it becomes
	// runtime policy or an allowlist compared with observed state.
	cfg.ExporterCommand = slices.Clone(cfg.ExporterCommand)
	cfg.ProviderEndpoints = slices.Clone(cfg.ProviderEndpoints)
	return cfg, nil
}

// RestoreConformance republishes image's suite-earned capabilities from the
// durable proof of the configuration image yields, after a daemon restart.
// The record is accepted only before this backend has begun a live proof of
// image, or restored one: once a recheck of image starts, its generation and
// cleared declaration are authoritative, and an older persisted success must
// never resurrect them. A live proof of another image does not matter. The
// caller must pass the newest record for the configuration
// (store.ReadTx.LatestConfigurationConformance).
func (b *Backend) RestoreConformance(image string, record domain.BackendConformance) error {
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
	if !digestPinnedImagePattern.MatchString(image) {
		return fmt.Errorf("%w: AgentImage %q is not digest-pinned", ErrInvalidConfig, image)
	}
	if want := b.ConfigurationDigestFor(image); record.ConfigurationDigest != want {
		return fmt.Errorf(
			"restore backend conformance generation %d for configuration %s on image %s's %s",
			record.Generation, record.ConfigurationDigest, image, want)
	}

	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	if proof, ok := b.proofs[image]; ok {
		if proof.generation != 0 {
			return fmt.Errorf("restore backend conformance generation %d for image %s after live generation %d began",
				record.Generation, image, proof.generation)
		}
		return fmt.Errorf("restore backend conformance generation %d for image %s after generation %d was restored",
			record.Generation, image, proof.restored)
	}
	b.proofs[image] = &imageProof{
		restored: record.Generation,
		published: record.Capabilities.Has(domain.CapNetworklessExport) &&
			record.Capabilities.Has(domain.CapEnforcedProviderEgress),
	}
	return nil
}

// HandoffTimeout is the wall-clock budget of one Handoff after defaults: the
// longest a writer attempt can hold its credential, which admission uses as
// the attempt's deadline.
func (b *Backend) HandoffTimeout() time.Duration { return b.cfg.HandoffTimeout }

// conformanceConfigurationVersion names the shape and the rule of a ward
// configuration digest. v5 is the per-configuration rule (#3211): a passed
// row stands for its own configuration until that configuration's next row.
// Rows written under v4 were judged per backend, where a newer row for
// another image cleared them, so no v4 digest may count again: none matches
// a v5 digest, each configuration proves once on first use, and the old rows
// stay as audit history.
const conformanceConfigurationVersion = "freeside.ward.conformance-configuration/v5"

// ConfigurationDigest is the configuration digest of Config.AgentImage, the
// attended composition's static writer image. It deliberately excludes
// collaborators and operational budgets that do not change the runtime
// topology under proof. An unattended attempt's digest is its own image's
// (ConfigurationDigestFor).
func (b *Backend) ConfigurationDigest() domain.Digest {
	if b == nil || !b.initialized {
		return ""
	}
	return b.ConfigurationDigestFor(b.cfg.AgentImage)
}

// ConfigurationDigestFor is the stable identity of the runtime configuration
// a Full conformance pass on image proves. Ward holds one proof per such
// configuration.
func (b *Backend) ConfigurationDigestFor(image string) domain.Digest {
	if b == nil || !b.initialized {
		return ""
	}
	return b.configurationDigest(conformanceConfigurationVersion, image)
}

func (b *Backend) configurationDigest(version, image string) domain.Digest {
	endpoints := slices.Clone(b.cfg.ProviderEndpoints)
	slices.Sort(endpoints)
	body, err := json.Marshal(conformanceConfiguration{
		Version:           version,
		Daemon:            b.daemonIdentity,
		Runtime:           b.runtimeIdentity,
		AgentImage:        image,
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
	return domain.Digest(contentaddr.Sum(body))
}

// ConformanceFor returns the configuration digest image yields, the
// capabilities this backend declares for image (the base declaration, plus
// the two suite-earned capabilities while image's proof is published), and
// whether that proof is published. The set is an independent copy.
// Admission reads it per attempt (exec.ImageBoundBackend), so an attempt is
// admitted against its own image's proof, never another image's.
func (b *Backend) ConformanceFor(image string) (domain.Digest, exec.CapabilitySet, bool) {
	if b == nil || !b.initialized {
		return "", exec.NewCapabilitySet(), false
	}
	caps := b.Capabilities()
	b.proofMu.Lock()
	proof := b.proofs[image]
	published := proof != nil && proof.published
	b.proofMu.Unlock()
	if published {
		caps[exec.CapNetworklessExport] = struct{}{}
		caps[exec.CapEnforcedProviderEgress] = struct{}{}
	}
	return b.ConfigurationDigestFor(image), caps, published
}

// ProvenImages lists the writer images whose proof is published, sorted.
func (b *Backend) ProvenImages() []string {
	if b == nil || !b.initialized {
		return nil
	}
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	var images []string
	for image, proof := range b.proofs {
		if proof.published {
			images = append(images, image)
		}
	}
	slices.Sort(images)
	return images
}

// TrackedImages lists, sorted, every image ward holds proof state for: a
// published proof, a pass in flight, or a pass that failed. RetireConformance
// is what stops tracking one.
func (b *Backend) TrackedImages() []string {
	if b == nil || !b.initialized {
		return nil
	}
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	return slices.Sorted(maps.Keys(b.proofs))
}

// RetireConformance withdraws image's proof without running a pass: it is
// the begin step with no pass after it. The superseding marker for image's
// configuration is appended through recorder first, so the store stops
// admitting under that configuration and a restart does not restore it;
// then ward stops tracking image, so it is unproved until an attempt
// selects it and a pass proves it again. A failed append still leaves image
// unpublished, the fail-closed side, and is returned.
func (b *Backend) RetireConformance(ctx context.Context, image string, recorder ConformanceRecorder) error {
	if b == nil || !b.initialized {
		return fmt.Errorf("%w: backend is not initialized", ErrInvalidConfig)
	}
	if recorder == nil {
		return fmt.Errorf("retire conformance for image %s: no recorder", image)
	}
	_, err := b.beginConformanceProof(image, conformanceRecordStep(
		ctx, recorder, domain.ConformanceSuperseded, b.ConfigurationDigestFor(image)))
	if err != nil {
		return fmt.Errorf("retire conformance for image %s: %w", image, err)
	}
	b.proofMu.Lock()
	delete(b.proofs, image)
	b.proofMu.Unlock()
	return nil
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
	return path + "@" + contentaddr.Format(h.Sum(nil)), nil
}

// Name identifies the backend in policy, refusals, and audit records.
func (b *Backend) Name() string { return BackendName }

// beginConformanceProof starts a new proof generation for image: it clears
// image's declaration and, still under the guard, runs the durable
// superseding step (nil when no recorder is configured). Other images'
// proofs are untouched. Durable-first mirrors concludeConformanceProof's
// persist-then-publish: the previous pass's row for image's configuration
// must stop admitting before this recheck runs a single probe, or an
// admission whose snapshot froze just before this begin would ride the old
// row for the recheck's whole duration. A failed superseding append aborts
// the begin (the declaration stays cleared, which is the safe direction).
//
// A begin with no pass after it retires image's proof: the configuration
// stops admitting, durably, until a later pass proves it again.
func (b *Backend) beginConformanceProof(image string, record func() error) (uint64, error) {
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	b.proofCounter++
	proof, ok := b.proofs[image]
	if !ok {
		proof = &imageProof{}
		b.proofs[image] = proof
	}
	proof.generation = b.proofCounter
	proof.published = false
	if record != nil {
		if err := record(); err != nil {
			return 0, err
		}
	}
	return proof.generation, nil
}

// concludeConformanceProof runs a pass's durable record step and then
// publishes its result for image, all while holding the currency judgment.
// A superseded generation publishes and records nothing, so an older
// overlapping pass on image that finishes last cannot resurrect a superseded
// proof. The record callback runs under proofMu on purpose: releasing the
// mutex between the currency check and the append would let a newer pass on
// image begin, fail, and append its row first, leaving the older pass's
// stale success as the configuration's newest row and inverting the
// append-only log's supersession order. The cost is that a concurrent begin
// of any image blocks for one store append, which is nothing against a suite
// pass.
//
// Persist-then-publish ordering is load-bearing: image's capabilities have
// been clear since beginConformanceProof, and they must stay unobservable
// until the pass's own row is its configuration's newest. Publishing first
// would open a window where a concurrent admission is enabled by the fresh
// declaration yet gated against the previous pass's stale row; if the
// append then failed, that admission would stand on evidence this pass never
// persisted. A failed append therefore publishes false: an unpersisted proof
// is never declared.
func (b *Backend) concludeConformanceProof(
	image string, generation uint64, proved bool, record func() error,
) (published bool, recordErr error) {
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	proof, ok := b.proofs[image]
	if !ok || generation == 0 || generation != proof.generation {
		return false, nil
	}
	if record != nil {
		recordErr = record()
	}
	proof.published = proved && recordErr == nil
	return true, recordErr
}

// finishConformanceProof is concludeConformanceProof with no durable record
// step, for callers and tests that only publish.
func (b *Backend) finishConformanceProof(image string, generation uint64, proved bool) bool {
	published, _ := b.concludeConformanceProof(image, generation, proved, nil)
	return published
}

// Capabilities returns the backend's base capability set, freshly built on
// every call so no caller can mutate it. The two suite-earned capabilities
// belong to one image's configuration, so they are declared only through
// ConformanceFor; the handoff and recovery need only the base set, and a
// caller left on this method loses the earned ones, which is the safe
// direction.
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
// added for an image only after Suite.Full has passed on it: the
// runtime-observed empty-network check with its DNS/direct-connect probe,
// and the in-writer egress probes (declared-provider CONNECT success,
// undeclared-authority refusal, DNS and direct-IP refusal). A new or failed
// image therefore refuses an unattended policy minimum until its proof has
// run. supports_credential_volume_detach and supports_workspace_snapshot are
// refuted on this runtime and are never declared: the spike proved a guest
// unmount is not a credential-device detach (the refuted same-VM fallback
// class), and volume snapshotting has no public support.
func (b *Backend) Capabilities() exec.CapabilitySet {
	if b == nil || !b.initialized {
		return exec.NewCapabilitySet()
	}
	return exec.NewCapabilitySet(
		exec.CapDetachableWorkspace,
		exec.CapPostExitExport,
		exec.CapReadOnlyRemount,
	)
}

// provenCapabilities is the exact set a green Full pass proves: the frozen
// base declaration plus both suite-earned capabilities.
// supports_enforced_provider_egress covers provider_only and
// provider_registry alike (plan §5.7), so Full earns it only when its
// synthetic handoff passes under each. The set is spelled out rather than
// read back from the live declaration so the durable conformance
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
