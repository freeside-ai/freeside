package fake

import "github.com/freeside-ai/freeside/daemon/internal/exec"

// Compile-time contract assertion, as for the driver fakes.
var _ exec.RunnerBackend = RunnerBackend{}

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
