package cordon

import "testing"

// This file lends the external cordon_test package, which runs the host
// launcher's contract suite, what it needs from inside the package.

// testVerbs are subprocess entry points the external test package adds:
// TestMain runs one when the test binary starts with its name.
var testVerbs = map[string]func(args []string) int{}

// RegisterTestVerb adds a subprocess entry point from a test file's init.
func RegisterTestVerb(name string, verb func(args []string) int) { testVerbs[name] = verb }

// CheckApprovals is the check a refusal over a held approval names.
const CheckApprovals = checkApprovals

// FixtureToken is the setup token every test request carries.
const FixtureToken = fixtureToken

// PinnedConfig is a configuration whose agent CLI is a stand-in file, for
// a backend that starts no launch.
func PinnedConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig(t)
	pinnedCLI(t, &cfg)
	return cfg
}

// WithHostFacts makes b read facts in place of the host's approvals.
func WithHostFacts(b *Backend, facts HostFacts) {
	b.facts = func() (HostFacts, error) { return facts, nil }
}

// ExportRequestFor is a run's export request with fixed evidence sources.
func ExportRequestFor(runID string) ExportRequest { return exportRequest(runID) }

// Worktree is runID's worktree under cfg's runs root.
func Worktree(cfg Config, runID string) string { return layoutFor(cfg.RunsRoot, runID).worktree }

// Transcript is where runID's launch writes the agent's stdout. A client
// of a launcher reports it only for a failed run.
func Transcript(cfg Config, runID string) string { return layoutFor(cfg.RunsRoot, runID).transcript }
