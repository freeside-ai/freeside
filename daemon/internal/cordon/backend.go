package cordon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// Backend runs cordoned writer launches on the daemon's host. Construct it
// with New; the zero value declares nothing and has no configuration digest.
type Backend struct {
	cfg    Config
	rules  []byte
	seeder Seeder
	// daemonPath and daemonDigest are the executable this process started
	// as. The digest enters ConfigurationDigest, and a probe launch runs a
	// copy of that path only once the copy hashes to it, so the probe is the
	// binary a proof is bound to.
	daemonPath, daemonDigest string
	// newHost, start, and startSupervisor are the run-end proof's kernel
	// queries, the process starter recovery's canary uses, and the
	// supervisor starter; tests replace them.
	newHost         func() (hostProcesses, error)
	start           func(processSpec) (process, error)
	startSupervisor func(supervisorSpec) (supervisorProcess, error)
	// facts reads the approvals the program that starts runs holds
	// (hostfacts.go). New sets the real reader, and only this package's
	// tests replace it.
	facts func() (HostFacts, error)

	// detached closes once Detach runs.
	detached   chan struct{}
	detachOnce sync.Once

	// proven is the declaration: set only by a recorded, generation-current
	// passing Suite.Full.
	proven atomic.Bool
	// proofMu makes the generation check and the publication one decision,
	// so a pass that a newer pass or a pre-job failure superseded never
	// publishes over it.
	proofMu         sync.Mutex
	proofGeneration uint64
	// suite is the conformance suite NewSuite attached, which runs the
	// pre-job probe of a launch that requires proof.
	suite atomic.Pointer[Suite]
}

// Compile-time contract assertion (exec package convention).
var _ exec.ConfigurationBoundBackend = (*Backend)(nil)

// New validates cfg and renders its rule file once, so every launch the
// backend runs uses the same bytes. The backend declares nothing until a
// Suite's Full pass is recorded.
func New(cfg Config, seeder Seeder) (*Backend, error) {
	if seeder == nil {
		return nil, errors.New("cordon: a seeder is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.clone()
	daemonPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("%w: daemon executable: %w", ErrInvalidConfig, err)
	}
	daemonDigest, err := fileSHA256(daemonPath)
	if err != nil {
		return nil, fmt.Errorf("%w: daemon executable: %w", ErrInvalidConfig, err)
	}
	return &Backend{
		cfg: cfg, rules: RenderRules(cfg), seeder: seeder,
		daemonPath: daemonPath, daemonDigest: daemonDigest,
		newHost: newHostProcesses, start: startExec, startSupervisor: startSupervisorExec,
		facts: hostFactsReader, detached: make(chan struct{}),
	}, nil
}

// Name identifies the backend by its runner class, so the name admission
// uses and the class conformance records name can't drift.
func (b *Backend) Name() string { return string(domain.BackendCordonedHostProcess) }

// Capabilities returns the cordoned class's declaration: empty until a
// recorded, passing Suite.Full for this configuration, then the class's four
// capabilities. A newer pass beginning or a failed pre-job probe empties it
// again. Declaring admits nothing by itself: unattended admission also
// needs the durable passed record for this ConfigurationDigest (#3472).
func (b *Backend) Capabilities() exec.CapabilitySet {
	if b == nil || b.rules == nil || !b.proven.Load() {
		return exec.NewCapabilitySet()
	}
	return exec.NewCapabilitySet(domain.CordonedCapabilities()...)
}

type conformanceConfiguration struct {
	Version string `json:"version"`
	// Daemon is the probe binary: the executable this process started as,
	// by path and digest.
	Daemon            string   `json:"daemon"`
	Rules             string   `json:"rules"`
	RunsRoot          string   `json:"runs_root"`
	Home              string   `json:"home"`
	OwnerClaudeConfig string   `json:"owner_claude_config"`
	Projects          string   `json:"projects"`
	ProtectedRoots    []string `json:"protected_roots"`
	UserTemp          string   `json:"user_temp"`
	UserCache         string   `json:"user_cache"`
	ReservedPorts     []int    `json:"reserved_ports"`
	AgentCLI          string   `json:"agent_cli"`
	AgentCLISHA256    string   `json:"agent_cli_sha256"`
	AgentCLISettings  []string `json:"agent_cli_settings"`
	Path              []string `json:"path"`
	StallLimit        int64    `json:"stall_limit_ns"`
	EndGrace          int64    `json:"end_grace_ns"`
	EndProofTimeout   int64    `json:"end_proof_timeout_ns"`
}

// ConfigurationDigest is the stable identity of the configuration a
// conformance pass proves: the daemon executable, the agent CLI's fixed
// settings, and every Config field, with the rule file entered by its
// digest.
func (b *Backend) ConfigurationDigest() domain.Digest {
	if b == nil || b.rules == nil {
		return ""
	}
	body, err := json.Marshal(conformanceConfiguration{
		Version:           "freeside.cordon.conformance-configuration/v2",
		Daemon:            b.daemonPath + "@" + b.daemonDigest,
		Rules:             contentaddr.Sum(b.rules),
		RunsRoot:          b.cfg.RunsRoot,
		Home:              b.cfg.Home,
		OwnerClaudeConfig: b.cfg.OwnerClaudeConfig,
		Projects:          b.cfg.Projects,
		ProtectedRoots:    b.cfg.ProtectedRoots,
		UserTemp:          b.cfg.UserTemp,
		UserCache:         b.cfg.UserCache,
		ReservedPorts:     b.cfg.ReservedPorts,
		AgentCLI:          b.cfg.AgentCLI,
		AgentCLISHA256:    b.cfg.AgentCLISHA256,
		AgentCLISettings:  cliSettings,
		Path:              b.cfg.Path,
		StallLimit:        int64(b.cfg.StallLimit),
		EndGrace:          int64(b.cfg.EndGrace),
		EndProofTimeout:   int64(b.cfg.EndProofTimeout),
	})
	if err != nil {
		panic(fmt.Sprintf("marshal cordon conformance configuration: %v", err))
	}
	return domain.Digest(contentaddr.Sum(body))
}

// beginConformanceProof starts a new proof generation: it clears the
// declaration and, still under the guard, runs the durable superseding step
// (nil with no recorder). A failed superseding append aborts the begin with
// the declaration cleared, the safe direction.
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

// declaredGeneration reports the current proof generation and whether it
// declares the class, read together under the guard.
func (b *Backend) declaredGeneration() (uint64, bool) {
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	return b.proofGeneration, b.proven.Load()
}

// withdrawConformance clears the declaration after a pre-job probe failed,
// and runs the durable failure step under the guard. It moves to a new
// generation, so a pass that began before the failure can't publish over
// it. Only a new Full pass declares again.
func (b *Backend) withdrawConformance(record func() error) error {
	b.proofMu.Lock()
	defer b.proofMu.Unlock()
	b.proofGeneration++
	b.proven.Store(false)
	if record == nil {
		return nil
	}
	return record()
}

// fileSHA256 is the SHA-256 of the file at path, in lowercase hex.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the daemon's own executable, the pinned agent CLI, or a probe launch's copy
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
