package daemoncompose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

type refusingSeeder struct{}

func (refusingSeeder) FetchBaseWorktree(context.Context, string, string, string, string) error {
	return errors.New("no seed in a composition test")
}

// cordonFixture is a configuration whose every path is a resolved
// directory under the test's own, so no composition test names the
// account's home.
func cordonFixture(t *testing.T) (CordonBackendConfig, CordonAccount, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cli := filepath.Join(dir("bin"), "claude")
	if err := os.WriteFile(cli, []byte("cli"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := CordonBackendConfig{
		AgentCLI: cli, AgentCLISHA256: strings.Repeat("a", 64), Projects: dir("projects"),
		ProtectedRoots: []string{dir("other-root")},
		StallLimit:     time.Minute, EndGrace: time.Second, EndProofTimeout: time.Minute,
	}
	account := CordonAccount{Home: dir("home"), UserTemp: dir("temp"), UserCache: dir("cache")}
	return cfg, account, dir("state")
}

func TestCordonUnconfiguredComposesNothing(t *testing.T) {
	t.Parallel()
	composed, err := composeConfiguredCordon(t.Context(), nil, ClaudeDriverConfig{}, refusingSeeder{}, nil)
	if err != nil || composed != nil {
		t.Fatalf("composeConfiguredCordon = %v, %v, want none", composed, err)
	}
	if err := composed.RunScheduled(t.Context()); err != nil {
		t.Fatalf("an unconfigured cordon's scheduled pass = %v", err)
	}
	if digest := composed.ConfigurationDigest(); digest != "" {
		t.Fatalf("an unconfigured cordon's digest = %q", digest)
	}
}

// TestCordonConfigProtectsTheStateDirectory shows the launches' directory
// sits in the daemon's state directory, which is always protected, and
// that the operator's paths are resolved.
func TestCordonConfigProtectsTheStateDirectory(t *testing.T) {
	t.Parallel()
	cfg, account, state := cordonFixture(t)
	link := filepath.Join(filepath.Dir(state), "state-link")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	cfg.ProtectedRoots = append(cfg.ProtectedRoots, cfg.ProtectedRoots[0])
	got, err := CordonConfig(cfg, account, link, nil, []int{47004, 47000, 47004})
	if err != nil {
		t.Fatal(err)
	}
	if got.RunsRoot != filepath.Join(state, cordonRunsDir) {
		t.Errorf("RunsRoot = %q, want it under the resolved state directory", got.RunsRoot)
	}
	if want := slices.Sorted(slices.Values([]string{state, cfg.ProtectedRoots[0]})); !slices.Equal(got.ProtectedRoots, want) {
		t.Errorf("ProtectedRoots = %q, want %q", got.ProtectedRoots, want)
	}
	if !slices.Equal(got.ReservedPorts, []int{47000, 47004}) {
		t.Errorf("ReservedPorts = %v, want sorted and unique", got.ReservedPorts)
	}
	if got.Path[0] != filepath.Dir(cfg.AgentCLI) {
		t.Errorf("Path = %q, want the agent CLI's directory first", got.Path)
	}
	if _, err := cordon.New(got, refusingSeeder{}); err != nil {
		t.Fatalf("the composed configuration doesn't validate: %v", err)
	}
	cfg.Projects = filepath.Join(state, "absent")
	if _, err := CordonConfig(cfg, account, state, nil, nil); err == nil || !strings.Contains(err.Error(), "-cordon-projects") {
		t.Fatalf("a missing projects directory = %v, want a refusal naming the flag", err)
	}
}

// TestCordonConfigProtectsTheDaemonsDirectories shows every directory the
// daemon is configured with is denied to a launch without the operator
// naming it, a missing one is refused by its own flag, and a database
// outside every protected root is refused.
func TestCordonConfigProtectsTheDaemonsDirectories(t *testing.T) {
	t.Parallel()
	cfg, account, state := cordonFixture(t)
	cfg.ProtectedRoots = nil
	base := filepath.Dir(state)
	dir := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	driver := ClaudeDriverConfig{
		SeedRoot: dir("seed"), StateRoot: dir("app"), CredentialsDir: dir("app-credentials"),
		ReviewInputRoot: dir("review"), HostVerification: MacverifyExecutorConfig{Root: dir("macverify")},
	}
	daemonDirs := cordonDaemonDirs(driver)
	got, err := CordonConfig(cfg, account, state, daemonDirs, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{state, driver.SeedRoot, driver.StateRoot, driver.CredentialsDir, driver.ReviewInputRoot, driver.HostVerification.Root}
	slices.Sort(want)
	if !slices.Equal(got.ProtectedRoots, want) {
		t.Fatalf("ProtectedRoots = %q, want %q", got.ProtectedRoots, want)
	}
	cfg.Database = filepath.Join(state, "freeside.db")
	if _, err := CordonConfig(cfg, account, state, daemonDirs, nil); err != nil {
		t.Fatalf("a database in the state directory = %v", err)
	}
	cfg.Database = filepath.Join(filepath.Dir(cfg.Projects), "freeside.db")
	if _, err := CordonConfig(cfg, account, state, daemonDirs, nil); err == nil || !strings.Contains(err.Error(), "-db") {
		t.Fatalf("a database outside every protected root = %v, want a refusal naming -db", err)
	}
	cfg.Database = ""
	if len(cordonDaemonDirs(ClaudeDriverConfig{})) != 0 {
		t.Fatal("an unset directory is protected")
	}
	if err := os.Remove(driver.CredentialsDir); err != nil {
		t.Fatal(err)
	}
	if _, err := CordonConfig(cfg, account, state, daemonDirs, nil); err == nil ||
		!strings.Contains(err.Error(), "-publication-credentials-dir") {
		t.Fatalf("a missing credentials directory = %v, want a refusal naming its flag", err)
	}
}

func TestComposeCordonBackendOnlyStopsOnAnUnwrittenRecord(t *testing.T) {
	t.Parallel()
	cfg, account, state := cordonFixture(t)
	config, err := CordonConfig(cfg, account, state, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	failed := &cordon.ConformanceFailure{Check: "read", Reason: "the probe read it"}
	unwritten := errors.Join(failed, cordon.ErrConformanceRecord)

	var calls int
	outcome := error(failed)
	prove := func(context.Context, *cordon.Suite) error { calls++; return outcome }
	composed, err := ComposeCordonBackend(t.Context(), st, config, refusingSeeder{}, nil, prove)
	if err != nil {
		t.Fatalf("a failed pass stopped startup: %v", err)
	}
	if len(composed.Backend.Capabilities()) != 0 {
		t.Fatal("a backend whose pass failed declares")
	}
	if composed.ConfigurationDigest() != composed.Backend.ConfigurationDigest() || composed.ConfigurationDigest() == "" {
		t.Fatal("the composition doesn't report its backend's digest")
	}
	if err := composed.RunScheduled(t.Context()); err != nil {
		t.Fatalf("a failed scheduled pass = %v, want nil", err)
	}
	outcome = unwritten
	if err := composed.RunScheduled(t.Context()); !errors.Is(err, cordon.ErrConformanceRecord) {
		t.Fatalf("an unwritten scheduled record = %v, want ErrConformanceRecord", err)
	}
	if calls != 3 {
		t.Fatalf("the pass ran %d times, want once at start and once per scheduled pass", calls)
	}

	// At start, an unwritten record fails the composition.
	if _, err := ComposeCordonBackend(t.Context(), st, config, refusingSeeder{}, nil, prove); !errors.Is(err, cordon.ErrConformanceRecord) {
		t.Fatalf("an unwritten record at start = %v, want ErrConformanceRecord", err)
	}
}
