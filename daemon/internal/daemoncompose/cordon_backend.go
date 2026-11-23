package daemoncompose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	osexec "os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// CordonBackendConfig names the cordoned host launch backend. An empty
// AgentCLI configures none.
type CordonBackendConfig struct {
	// AgentCLI is the pinned agent CLI's absolute path, and AgentCLISHA256
	// its SHA-256 in lowercase hex.
	AgentCLI, AgentCLISHA256 string
	// Projects is the directory holding the owner's other projects.
	Projects string
	// ProtectedRoots are denied to a launch besides the daemon's state
	// directory and its other configured directories, which always are
	// (cordonDaemonDirs): every other instance root, and any other
	// directory the daemon or verification trusts.
	ProtectedRoots []string
	// StallLimit, EndGrace, and EndProofTimeout bound each launch; see
	// cordon.Config.
	StallLimit, EndGrace, EndProofTimeout time.Duration
	// Database is the daemon's -db path, which Start sets. It and the
	// files beside it (blobs, local backups, the control socket) must sit
	// inside a protected root, or no backend composes.
	Database string
}

// CordonAccount is what the backend reads of the daemon's own account
// rather than from flags: its home, and its per-user temporary and cache
// directories.
type CordonAccount struct {
	Home, UserTemp, UserCache string
}

// LookupCordonAccount reads the account's home from the user database and
// its per-user directories from confstr, never from HOME or TMPDIR, which a
// test harness or the operator's shell may replace: the rules must name the
// directories cfprefsd and verification jobs actually use. Each is
// resolved.
func LookupCordonAccount() (CordonAccount, error) {
	account, err := user.Current()
	if err != nil {
		return CordonAccount{}, fmt.Errorf("cordon account: %w", err)
	}
	home, err := filepath.EvalSymlinks(account.HomeDir)
	if err != nil {
		return CordonAccount{}, fmt.Errorf("cordon account home: %w", err)
	}
	temp, err := darwinUserDir("DARWIN_USER_TEMP_DIR")
	if err != nil {
		return CordonAccount{}, err
	}
	cache, err := darwinUserDir("DARWIN_USER_CACHE_DIR")
	if err != nil {
		return CordonAccount{}, err
	}
	return CordonAccount{Home: home, UserTemp: temp, UserCache: cache}, nil
}

func darwinUserDir(name string) (string, error) {
	cmd := osexec.CommandContext(context.Background(), "/usr/bin/getconf", name) //nolint:gosec // G204: a fixed confstr name
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := procbound.Run(cmd, 0); err != nil {
		return "", fmt.Errorf("cordon account: getconf %s: %w", name, err)
	}
	dir := strings.TrimSpace(stdout.String())
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("cordon account: getconf %s gave %q", name, dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("cordon account: %s: %w", name, err)
	}
	return resolved, nil
}

// cordonRunsDir is the launches' directory under the daemon's state
// directory, which the backend protects, so no launch reaches another's
// control directory.
const cordonRunsDir = "cordon-runs"

// cordonSystemPath follows the agent CLI's own directory in a launch's
// PATH.
var cordonSystemPath = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}

// DaemonDir is one of the daemon's configured directories, which the
// cordon denies to every launch, with the flag that names it.
type DaemonDir struct{ Flag, Path string }

// CordonConfig builds the backend's configuration from cfg, the account,
// the daemon's state directory and its other configured directories, and
// the macOS verification executor's reserved ports. Paths are resolved
// here, so the rules and the configuration digest name what a launch would
// really reach; a directory that doesn't exist is refused by its flag.
func CordonConfig(
	cfg CordonBackendConfig, account CordonAccount, stateDir string, daemonDirs []DaemonDir, reservedPorts []int,
) (cordon.Config, error) {
	state, err := resolveExisting("the daemon's state directory", stateDir)
	if err != nil {
		return cordon.Config{}, err
	}
	projects, err := resolveExisting("-cordon-projects", cfg.Projects)
	if err != nil {
		return cordon.Config{}, err
	}
	cli, err := resolveExisting("-cordon-agent-cli", cfg.AgentCLI)
	if err != nil {
		return cordon.Config{}, err
	}
	roots := []string{state}
	for _, dir := range daemonDirs {
		resolved, err := resolveExisting(dir.Flag, dir.Path)
		if err != nil {
			return cordon.Config{}, err
		}
		roots = append(roots, resolved)
	}
	for _, root := range cfg.ProtectedRoots {
		resolved, err := resolveExisting("-cordon-protected-root", root)
		if err != nil {
			return cordon.Config{}, err
		}
		roots = append(roots, resolved)
	}
	slices.Sort(roots)
	if cfg.Database != "" {
		dbDir, err := resolveExisting("-db's directory", filepath.Dir(cfg.Database))
		if err != nil {
			return cordon.Config{}, err
		}
		if !slices.ContainsFunc(roots, func(root string) bool { return inside(dbDir, root) }) {
			return cordon.Config{}, fmt.Errorf(
				"configure the cordon: -db %s must sit inside -state-dir or a -cordon-protected-root, "+
					"or a launch could read the database", cfg.Database)
		}
	}
	path := []string{filepath.Dir(cli)}
	for _, dir := range cordonSystemPath {
		if !slices.Contains(path, dir) {
			path = append(path, dir)
		}
	}
	ports := slices.Clone(reservedPorts)
	slices.Sort(ports)
	return cordon.Config{
		RunsRoot:          filepath.Join(state, cordonRunsDir),
		Home:              account.Home,
		OwnerClaudeConfig: filepath.Join(account.Home, ".claude"),
		Projects:          projects,
		ProtectedRoots:    slices.Compact(roots),
		UserTemp:          account.UserTemp,
		UserCache:         account.UserCache,
		ReservedPorts:     slices.Compact(ports),
		AgentCLI:          cli,
		AgentCLISHA256:    cfg.AgentCLISHA256,
		Path:              path,
		StallLimit:        cfg.StallLimit,
		EndGrace:          cfg.EndGrace,
		EndProofTimeout:   cfg.EndProofTimeout,
	}, nil
}

// inside reports whether path is root or lies under it; both are resolved.
func inside(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func resolveExisting(name, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("configure the cordon: %s is required", name)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("configure the cordon: %s: %w", name, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("configure the cordon: %s: %w", name, err)
	}
	return resolved, nil
}

// proveCordon runs the suite's full pass.
func proveCordon(ctx context.Context, suite *cordon.Suite) error { return suite.Full(ctx) }

// ComposedCordon is the cordoned launch backend with its attached suite.
type ComposedCordon struct {
	Backend *cordon.Backend
	suite   *cordon.Suite
	prove   func(context.Context, *cordon.Suite) error
	logger  *slog.Logger
}

// ComposeCordonBackend builds the backend from config, attaches its suite
// with a store-backed recorder, and runs the full pass through prove
// (proveCordon outside tests). There is no restore: every start
// proves the class again, because the pass is cheap and a record from an
// earlier start says nothing about this host's state now. A failed pass
// leaves the backend declaring nothing and logs why; startup goes on, and
// unattended admission refuses the class. A record the store didn't take
// fails startup, since the durable state is then unknown.
//
// Tier A: it chooses which proof the backend declares from. The only
// declaration it allows is the one the suite's own recorded pass publishes.
func ComposeCordonBackend(
	ctx context.Context, st *store.Store, config cordon.Config, seeder cordon.Seeder, logger *slog.Logger,
	prove func(context.Context, *cordon.Suite) error,
) (*ComposedCordon, error) {
	backend, err := cordon.New(config, seeder)
	if err != nil {
		return nil, fmt.Errorf("configure the cordon: %w", err)
	}
	suite, err := cordon.NewSuite(backend, cordon.WithConformanceRecorder(StoreConformanceRecorder{Store: st}))
	if err != nil {
		return nil, fmt.Errorf("configure the cordon: %w", err)
	}
	composed := &ComposedCordon{Backend: backend, suite: suite, prove: prove, logger: logger}
	if err := composed.runPass(ctx); err != nil {
		return nil, fmt.Errorf("run the cordon's conformance pass: %w", err)
	}
	return composed, nil
}

// RunScheduled runs the full pass again, as the scheduled doctor pass
// does after the ward's. It returns only a record the store didn't take;
// a failed pass leaves the class undeclared without stopping the daemon.
func (c *ComposedCordon) RunScheduled(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.runPass(ctx)
}

// ConfigurationDigest is the composed backend's, or empty when no cordon
// is configured.
func (c *ComposedCordon) ConfigurationDigest() domain.Digest {
	if c == nil {
		return ""
	}
	return c.Backend.ConfigurationDigest()
}

func (c *ComposedCordon) runPass(ctx context.Context) error {
	err := c.prove(ctx, c.suite)
	switch {
	case err == nil:
		// The digest is what `freesided doctor -cordon-configuration-digest`
		// takes, so the log is where an operator finds it.
		if c.logger != nil {
			c.logger.Info("the cordoned class is proven", "configuration_digest", c.Backend.ConfigurationDigest())
		}
		return nil
	case errors.Is(err, cordon.ErrConformanceRecord):
		return err
	case ctx.Err() != nil:
		return ctx.Err()
	}
	if c.logger != nil {
		c.logger.Warn("the cordoned class declares nothing; unattended admission refuses it until a pass succeeds",
			"configuration_digest", c.Backend.ConfigurationDigest(), "reason", err.Error())
	}
	return nil
}

// composeConfiguredCordon composes the cordon cfg.Cordon names, or none
// when it names no agent CLI. Its reserved ports are the macOS
// verification executor's, when one is configured.
func composeConfiguredCordon(
	ctx context.Context, st *store.Store, cfg ClaudeDriverConfig, seeder cordon.Seeder, logger *slog.Logger,
) (*ComposedCordon, error) {
	if cfg.Cordon.AgentCLI == "" {
		return nil, nil
	}
	account, err := LookupCordonAccount()
	if err != nil {
		return nil, fmt.Errorf("configure the cordon: %w", err)
	}
	var ports []int
	if cfg.HostVerification.Root != "" {
		ports = macverify.Config{Slots: cfg.HostVerification.Slots, PortBase: cfg.HostVerification.PortBase}.ReservedPortList()
	}
	config, err := CordonConfig(cfg.Cordon, account, cfg.StateDir, cordonDaemonDirs(cfg), ports)
	if err != nil {
		return nil, err
	}
	return ComposeCordonBackend(ctx, st, config, seeder, logger, proveCordon)
}

// cordonDaemonDirs are the daemon's configured directories besides its
// state directory, each denied to a launch whatever the operator passes as
// -cordon-protected-root: the exact-base checkouts, the GitHub App
// authority and its credentials, the reviewer's credential snapshots, and
// the macOS verification executor's root. The rule file allows by default,
// so a directory left off would be readable to the writer. An unset one is
// skipped.
func cordonDaemonDirs(cfg ClaudeDriverConfig) []DaemonDir {
	var dirs []DaemonDir
	for _, dir := range []DaemonDir{
		{Flag: "-seed-root", Path: cfg.SeedRoot},
		{Flag: "-publication-state-dir", Path: cfg.StateRoot},
		{Flag: "-publication-credentials-dir", Path: cfg.CredentialsDir},
		{Flag: "-review-input-root", Path: cfg.ReviewInputRoot},
		{Flag: "-macverify-root", Path: cfg.HostVerification.Root},
	} {
		if dir.Path != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
