package macverify

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// BackendName is the executor's declared identity, derived from the domain
// class so the name the engine admits under and the class the store holds
// conformance records for cannot drift.
const BackendName = string(domain.BackendCredentialFreeHostVerification)

// ErrInvalidConfig refuses a configuration the executor cannot be proven
// against.
var ErrInvalidConfig = errors.New("macverify: invalid configuration")

// ErrHostUnsupported refuses publishing the declaration on a host that is not
// macOS: the confinements are macOS mechanisms, so no record can vouch for
// them elsewhere.
var ErrHostUnsupported = errors.New("macverify: host verification executor requires macOS")

// Config is the normalized configuration the executor's conformance pass
// (Suite.Full) proves. Every field enters ConfigurationDigest, so changing
// any of them invalidates a restored proof.
type Config struct {
	// ExecutorRoot is the absolute directory the executor's job directories
	// and slot templates live under.
	ExecutorRoot string
	// SandboxProfile is the exact profile applied to a job's process tree.
	SandboxProfile []byte
	// PacketFilterRules are the exact anchor rules keyed on the slot uids.
	PacketFilterRules []byte
	// Slots are the unprivileged account names, one per concurrent job,
	// sorted and unique.
	Slots []string
	// Toolchain pins the operator-installed Xcode and Swift the recipe runs
	// under. The room refuses a job when the host's differ (#2880).
	Toolchain Toolchain
	// PortBase is the first of the slots' reserved ports (ports.go): slot i
	// in sorted order holds PortBase+4i through PortBase+4i+3. It must match
	// the base install wrote into bin/ports.conf, or the room refuses jobs.
	PortBase int
}

func (c Config) validate() error {
	if c.ExecutorRoot == "" || !filepath.IsAbs(c.ExecutorRoot) ||
		filepath.Clean(c.ExecutorRoot) != c.ExecutorRoot {
		return fmt.Errorf("%w: ExecutorRoot %q must be a clean absolute path", ErrInvalidConfig, c.ExecutorRoot)
	}
	if len(c.SandboxProfile) == 0 {
		return fmt.Errorf("%w: SandboxProfile is required", ErrInvalidConfig)
	}
	if len(c.PacketFilterRules) == 0 {
		return fmt.Errorf("%w: PacketFilterRules is required", ErrInvalidConfig)
	}
	if len(c.Slots) == 0 {
		return fmt.Errorf("%w: at least one slot is required", ErrInvalidConfig)
	}
	for i, slot := range c.Slots {
		if slot == "" {
			return fmt.Errorf("%w: slot %d is empty", ErrInvalidConfig, i)
		}
		if i > 0 && slot <= c.Slots[i-1] {
			return fmt.Errorf("%w: slots must be sorted and unique, %q after %q",
				ErrInvalidConfig, slot, c.Slots[i-1])
		}
	}
	if err := c.Toolchain.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	ephemeralFirst, err := ephemeralPortFirst()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := validatePortBase(c.PortBase, len(c.Slots), ephemeralFirst); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return nil
}

// Backend is the executor's runner backend. Construct it with New; the zero
// value declares nothing and has no configuration digest.
type Backend struct {
	cfg Config
	// daemonIdentity binds a restored proof to the exact freesided binary
	// that earned it, as the ward's does: rebuilding the gate implementation
	// requires a fresh pass. daemonPath and daemonDigest are its parts:
	// stageProbe copies that path into the probe workspace and checks the
	// copy against that digest, so the probe is the binary the digest names.
	daemonIdentity string
	daemonPath     string
	daemonDigest   string
	initialized    bool
	// slots is shared by every Room built over this backend, so leases and
	// quarantine hold across them: the uid absence proof assumes one job per
	// slot at a time.
	slots  *slotPool
	proven atomic.Bool
	// proofMu makes the generation check and the publication one decision,
	// so a restore cannot race a later live pass.
	proofMu         sync.Mutex
	proofGeneration uint64
	// probeMu guards probeDir, the probe workspace stageProbe keeps.
	probeMu  sync.Mutex
	probeDir string
}

type conformanceConfiguration struct {
	Version           string   `json:"version"`
	Daemon            string   `json:"daemon"`
	ExecutorRoot      string   `json:"executor_root"`
	SandboxProfile    string   `json:"sandbox_profile"`
	PacketFilterRules string   `json:"packet_filter_rules"`
	Slots             []string `json:"slots"`
	XcodeBuild        string   `json:"xcode_build"`
	Swift             string   `json:"swift"`
	// Go names the Go toolchain pin slots run. The daemon's identity already
	// covers it, since the pin is compiled in; naming it makes the record
	// say which Go a pass proved.
	Go string `json:"go"`
	// PortBase and PortsPerSlot fix every slot's reserved ports, which the
	// probe proves reachable and the profile admits.
	PortBase     int `json:"port_base"`
	PortsPerSlot int `json:"ports_per_slot"`
}

// Compile-time contract assertion (exec package convention).
var _ exec.ConfigurationBoundBackend = (*Backend)(nil)

// New builds a Backend over cfg, refusing an invalid configuration. The
// backend declares nothing until RestoreConformance accepts a passed record.
func New(cfg Config) (*Backend, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	// Config is caller-owned; freeze its reference fields before they become
	// the identity a proof is bound to.
	cfg.SandboxProfile = slices.Clone(cfg.SandboxProfile)
	cfg.PacketFilterRules = slices.Clone(cfg.PacketFilterRules)
	cfg.Slots = slices.Clone(cfg.Slots)
	daemonPath, daemonDigest, err := daemonExecutable()
	if err != nil {
		return nil, fmt.Errorf("%w: daemon executable identity: %w", ErrInvalidConfig, err)
	}
	return &Backend{
		cfg: cfg, daemonIdentity: daemonPath + "@" + daemonDigest, daemonPath: daemonPath, daemonDigest: daemonDigest,
		initialized: true, slots: newSlotPool(cfg.ExecutorRoot, cfg.Slots),
	}, nil
}

// Name identifies the backend in policy, refusals, and audit records.
func (b *Backend) Name() string { return BackendName }

// Capabilities returns the executor's declaration: empty until a recorded,
// passing Suite.Full or a restored passed record for this configuration,
// then exactly supports_credential_free_host_verification. A newer pass
// beginning or a pre-job probe failure empties it again. Off macOS it is
// always empty.
func (b *Backend) Capabilities() exec.CapabilitySet {
	caps := exec.NewCapabilitySet()
	if b == nil || !b.initialized || !hostSupported {
		return caps
	}
	if b.proven.Load() {
		caps[exec.CapCredentialFreeHostVerification] = struct{}{}
	}
	return caps
}

// ConfigurationDigest is the stable identity of the configuration a
// conformance pass proves: the daemon binary and every Config field, with the
// profile and rule bytes entered by digest.
func (b *Backend) ConfigurationDigest() domain.Digest {
	if b == nil || !b.initialized {
		return ""
	}
	body, err := json.Marshal(conformanceConfiguration{
		Version:           "freeside.macverify.conformance-configuration/v3",
		Daemon:            b.daemonIdentity,
		ExecutorRoot:      b.cfg.ExecutorRoot,
		SandboxProfile:    contentaddr.Sum(b.cfg.SandboxProfile),
		PacketFilterRules: contentaddr.Sum(b.cfg.PacketFilterRules),
		Slots:             b.cfg.Slots,
		XcodeBuild:        b.cfg.Toolchain.XcodeBuild,
		Swift:             b.cfg.Toolchain.Swift,
		Go:                "go" + goToolchainVersion,
		PortBase:          b.cfg.PortBase,
		PortsPerSlot:      portsPerSlot,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal macverify conformance configuration: %v", err))
	}
	return domain.Digest(contentaddr.Sum(body))
}

// RestoreConformance republishes the declaration from the durable latest
// proof after a daemon restart. It accepts only a passed, persisted record
// for this class and this backend's current configuration, only on macOS,
// and only before any proof generation has been taken, so an older record
// can never overwrite a newer one.
func (b *Backend) RestoreConformance(record domain.BackendConformance) error {
	if b == nil || !b.initialized {
		return fmt.Errorf("%w: backend is not initialized", ErrInvalidConfig)
	}
	if err := record.Validate(); err != nil {
		return fmt.Errorf("restore host verification conformance: %w", err)
	}
	if record.Backend != domain.BackendCredentialFreeHostVerification {
		return fmt.Errorf("restore backend conformance for %q on %q", record.Backend, BackendName)
	}
	if record.Outcome != domain.ConformancePassed {
		return fmt.Errorf("restore backend conformance generation %d with outcome %q",
			record.Generation, record.Outcome)
	}
	if record.Generation == 0 {
		return errors.New("restore unpersisted backend conformance generation")
	}
	if record.ConfigurationDigest != b.ConfigurationDigest() {
		return fmt.Errorf(
			"restore backend conformance generation %d for configuration %s on active %s",
			record.Generation, record.ConfigurationDigest, b.ConfigurationDigest())
	}
	if !hostSupported {
		return ErrHostUnsupported
	}

	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	if b.proofGeneration != 0 {
		return fmt.Errorf("restore backend conformance generation %d after generation %d",
			record.Generation, b.proofGeneration)
	}
	b.proofGeneration = record.Generation
	b.proven.Store(record.Capabilities.Has(domain.CapCredentialFreeHostVerification))
	return nil
}

// beginConformanceProof starts a new proof generation: it clears the
// declaration and, still under the guard, runs the durable superseding step
// (nil when no recorder is configured), as the ward's does. A failed
// superseding append aborts the begin with the declaration cleared, the safe
// direction.
func (b *Backend) beginConformanceProof(record func() error) (uint64, error) {
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	b.proofGeneration++
	b.proven.Store(false)
	if record != nil {
		if err := record(); err != nil {
			return 0, err
		}
	}
	return b.proofGeneration, nil
}

// concludeConformanceProof runs a pass's durable record step and then
// publishes its result, both under the guard and only while generation is
// still current, so an older pass that finishes last records and publishes
// nothing. The record runs first and a failed record publishes false: the
// declaration is never observable before the pass's own row is
// durable-latest. published reports whether generation was still current.
func (b *Backend) concludeConformanceProof(generation uint64, proved bool, record func() error) (published bool, recordErr error) {
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	if generation != b.proofGeneration {
		return false, nil
	}
	if record != nil {
		recordErr = record()
	}
	b.proven.Store(proved && recordErr == nil)
	return true, recordErr
}

// withdrawConformance clears the declaration after a pre-job probe found a
// confinement broken. It takes a generation too, so a pass that began
// before the failure cannot publish over it. The failed slot stays
// quarantined and a pass refuses it, so in practice the declaration returns
// only when a restart restores the durable record; the pre-job probe then
// runs again before every command. The withdrawal is in memory only.
func (b *Backend) withdrawConformance() {
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	b.proofGeneration++
	b.proven.Store(false)
}

// daemonExecutable returns this process's executable path and its digest.
func daemonExecutable() (path, digest string, err error) {
	path, err = os.Executable()
	if err != nil {
		return "", "", err
	}
	digest, err = fileDigest(path)
	if err != nil {
		return "", "", err
	}
	return path, digest, nil
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: hashes this process's own executable or the room's copy of it
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return contentaddr.Format(h.Sum(nil)), nil
}

// stageProbe returns the probe workspace: a daemon-owned directory holding a
// copy of the daemon's executable named probe, which prepare copies into the
// job like any workspace. Every room over the backend shares the one copy.
// It stages the copy on first use, and again when the copy is gone or
// differs (the system's sweep of temporary files can remove it under a
// long-running daemon). The copy must hash to the digest the configuration
// digest names, so the probe is the binary the proof is bound to; a binary
// replaced on disk since New is refused until the daemon restarts.
func (b *Backend) stageProbe() (string, error) {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	if b.probeDir != "" {
		digest, err := fileDigest(filepath.Join(b.probeDir, probeExecutable))
		if err == nil && digest == b.daemonDigest {
			return b.probeDir, nil
		}
		_ = os.RemoveAll(b.probeDir)
		b.probeDir = ""
	}
	dir, err := os.MkdirTemp("", "freeside-macverify-probe-")
	if err != nil {
		return "", fmt.Errorf("stage probe: %w", err)
	}
	// The workspace path the helper reads is the canonical one, not one
	// through the /var symlink.
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("stage probe: %w", err)
	}
	dir = canonical
	if err := stageProbeIn(dir, b.daemonPath, b.daemonDigest); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	b.probeDir = dir
	return dir, nil
}

// stageProbeIn copies the daemon executable at path into dir as the probe
// and refuses a copy whose digest is not want.
func stageProbeIn(dir, path, want string) error {
	dst := filepath.Join(dir, probeExecutable)
	if err := copyExecutable(path, dst); err != nil {
		return fmt.Errorf("stage probe: %w", err)
	}
	digest, err := fileDigest(dst)
	if err != nil {
		return fmt.Errorf("stage probe: %w", err)
	}
	if digest != want {
		return fmt.Errorf("stage probe: the daemon executable %s is %s on disk, but the daemon started as %s; restart the daemon",
			path, digest, want)
	}
	return nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: the daemon's own executable
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755) //nolint:gosec // G302,G304: the probe must be executable by the slot that runs its copy
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
