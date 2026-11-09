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
// proves (#2879). Every field enters ConfigurationDigest, so changing any of
// them invalidates a restored proof.
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
	return nil
}

// Backend is the executor's runner backend. Construct it with New; the zero
// value declares nothing and has no configuration digest.
type Backend struct {
	cfg Config
	// daemonIdentity binds a restored proof to the exact freesided binary
	// that earned it, as the ward's does: rebuilding the gate implementation
	// requires a fresh pass.
	daemonIdentity string
	initialized    bool
	// slots is shared by every Room built over this backend, so leases and
	// quarantine hold across them: the uid absence proof assumes one job per
	// slot at a time.
	slots  *slotPool
	proven atomic.Bool
	// proofMu makes the generation check and the publication one decision,
	// so a restore cannot race a later live pass (#2879).
	proofMu         sync.Mutex
	proofGeneration uint64
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
	daemonIdentity, err := daemonConfigurationIdentity()
	if err != nil {
		return nil, fmt.Errorf("%w: daemon executable identity: %w", ErrInvalidConfig, err)
	}
	return &Backend{cfg: cfg, daemonIdentity: daemonIdentity, initialized: true, slots: newSlotPool(cfg.Slots)}, nil
}

// Name identifies the backend in policy, refusals, and audit records.
func (b *Backend) Name() string { return BackendName }

// Capabilities returns the executor's declaration: empty until a passed
// conformance record for this configuration is restored, then exactly
// supports_credential_free_host_verification. Off macOS it is always empty.
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
		Version:           "freeside.macverify.conformance-configuration/v2",
		Daemon:            b.daemonIdentity,
		ExecutorRoot:      b.cfg.ExecutorRoot,
		SandboxProfile:    contentaddr.Sum(b.cfg.SandboxProfile),
		PacketFilterRules: contentaddr.Sum(b.cfg.PacketFilterRules),
		Slots:             b.cfg.Slots,
		XcodeBuild:        b.cfg.Toolchain.XcodeBuild,
		Swift:             b.cfg.Toolchain.Swift,
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

func daemonConfigurationIdentity() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path) //nolint:gosec // G304: hashes this process's own executable
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
