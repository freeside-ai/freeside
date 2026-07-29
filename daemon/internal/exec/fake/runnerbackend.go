package fake

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// Compile-time contract assertion, as for the driver fakes.
var (
	_ exec.RunnerBackend             = RunnerBackend{}
	_ exec.ConfigurationBoundBackend = RunnerBackend{}
)

// RunnerBackend is the permanent declaring-side fake of exec.RunnerBackend:
// a value that declares exactly the capabilities a test gives it, for
// exercising policy minimums against declared capability sets (§5.7).
type RunnerBackend struct {
	BackendName string
	Caps        exec.CapabilitySet
}

// Name identifies the fake backend in refusals and test output.
func (b RunnerBackend) Name() string { return b.BackendName }

// Capabilities returns the declared capability set as an independent copy, so
// a caller that mutates the returned map cannot alter the fake's declaration
// (the aliasing boundary issue #39 closes).
func (b RunnerBackend) Capabilities() exec.CapabilitySet { return b.Caps.Clone() }

// ConfigurationDigest supplies a deterministic configuration identity for
// tests that exercise unattended admission. The fake has no runtime topology,
// so its complete configuration is its name plus canonical capability list.
func (b RunnerBackend) ConfigurationDigest() domain.Digest {
	sum := sha256.Sum256([]byte(b.BackendName + "\x00" +
		strings.Join(capabilityNames(b.Caps.Snapshot()), "\x00")))
	return domain.Digest(fmt.Sprintf("sha256:%x", sum))
}

func capabilityNames(capabilities domain.CapabilitySnapshot) []string {
	names := make([]string, len(capabilities))
	for i, capability := range capabilities {
		names[i] = string(capability)
	}
	return names
}
