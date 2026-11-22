package cordon

import (
	"encoding/json"
	"errors"
	"fmt"

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
	// newHost and start are the run-end proof's kernel queries and the
	// process starter; tests replace them.
	newHost func() (hostProcesses, error)
	start   func(processSpec) (process, error)
}

// Compile-time contract assertion (exec package convention).
var _ exec.ConfigurationBoundBackend = (*Backend)(nil)

// New validates cfg and renders its rule file once, so every launch the
// backend runs uses the same bytes.
func New(cfg Config, seeder Seeder) (*Backend, error) {
	if seeder == nil {
		return nil, errors.New("cordon: a seeder is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.clone()
	return &Backend{
		cfg: cfg, rules: RenderRules(cfg), seeder: seeder,
		newHost: newHostProcesses, start: startExec,
	}, nil
}

// Name identifies the backend by its runner class, so the name admission
// uses and the class conformance records name can't drift.
func (b *Backend) Name() string { return string(domain.BackendCordonedHostProcess) }

// Capabilities returns the cordoned class's declaration. Declaring it
// admits nothing by itself: unattended admission also needs a passed
// conformance record for this ConfigurationDigest (#3470, #3472).
func (b *Backend) Capabilities() exec.CapabilitySet {
	if b == nil || b.rules == nil {
		return exec.NewCapabilitySet()
	}
	return exec.NewCapabilitySet(domain.CordonedCapabilities()...)
}

type conformanceConfiguration struct {
	Version           string   `json:"version"`
	Rules             string   `json:"rules"`
	RunsRoot          string   `json:"runs_root"`
	Home              string   `json:"home"`
	OwnerClaudeConfig string   `json:"owner_claude_config"`
	Projects          string   `json:"projects"`
	ProtectedRoots    []string `json:"protected_roots"`
	AgentCLI          string   `json:"agent_cli"`
	Path              []string `json:"path"`
	StallLimit        int64    `json:"stall_limit_ns"`
	EndGrace          int64    `json:"end_grace_ns"`
	EndProofTimeout   int64    `json:"end_proof_timeout_ns"`
}

// ConfigurationDigest is the stable identity of the configuration a
// conformance pass proves: every Config field, with the rule file entered
// by its digest.
func (b *Backend) ConfigurationDigest() domain.Digest {
	if b == nil || b.rules == nil {
		return ""
	}
	body, err := json.Marshal(conformanceConfiguration{
		Version:           "freeside.cordon.conformance-configuration/v1",
		Rules:             contentaddr.Sum(b.rules),
		RunsRoot:          b.cfg.RunsRoot,
		Home:              b.cfg.Home,
		OwnerClaudeConfig: b.cfg.OwnerClaudeConfig,
		Projects:          b.cfg.Projects,
		ProtectedRoots:    b.cfg.ProtectedRoots,
		AgentCLI:          b.cfg.AgentCLI,
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
