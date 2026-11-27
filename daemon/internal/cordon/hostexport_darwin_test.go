//go:build darwin && cgo

package cordon

import "testing"

// HostConfig is a real launch's configuration: the test binary is its
// agent CLI. It skips the test where no launch can start.
func HostConfig(t *testing.T) Config {
	t.Helper()
	return newHostFixture(t).cfg
}

// SuiteConfig is a real full pass's configuration.
func SuiteConfig(t *testing.T) Config {
	t.Helper()
	return suiteFixture(t)
}

// HostSuite is a suite over b whose launchd stand-in jobs are fakes, so a
// full pass submits nothing to the session's launchd.
func HostSuite(t *testing.T, b *Backend, opts ...SuiteOption) *Suite {
	t.Helper()
	s := testSuite(t, b, opts...)
	s.outside = &fakeOutside{}
	return s
}

// FixtureAgentArg is the first argument of every fake agent launch.
const FixtureAgentArg = fakeAgentArg

// HostRequest is a launch of the fake agent with args.
func HostRequest(runID, prompt string, args ...string) LaunchRequest {
	return hostRequest(runID, prompt, args...)
}

// HostCheck is a check whose command is the fake agent in mode args.
func HostCheck(cfg Config, runID string, args ...string) CheckRequest {
	return hostCheck(cfg, runID, args...)
}

// GitSeeder seeds by cloning base at the requested commit.
func GitSeeder(base string) Seeder { return gitSeeder{base: base} }

// Git runs git in dir with a scrubbed environment and fails t on error.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return mustGit(t, dir, args...)
}

// ProcessLive reports whether pid names a live process.
func ProcessLive(t *testing.T, pid int) bool {
	t.Helper()
	return processLive(t, pid)
}
