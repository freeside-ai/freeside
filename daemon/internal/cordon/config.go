package cordon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ErrInvalidConfig refuses a configuration a launch can't be held to.
var ErrInvalidConfig = errors.New("cordon: invalid configuration")

// Config is the normalized configuration every launch runs under. Every
// field enters the backend's ConfigurationDigest, so a proof bound to one
// configuration says nothing about another.
//
// Every path is absolute, clean, and already resolved (filepath.EvalSymlinks
// of its deepest existing ancestor leaves it unchanged), because Seatbelt
// matches resolved paths: a rule on /tmp never matches, since the kernel
// sees /private/tmp (#3463, Method).
type Config struct {
	// RunsRoot is where each launch's directory lives. It must sit inside a
	// protected root, so a launch can't read or write any launch's control
	// directory, its own included.
	RunsRoot string
	// Home is the owner's home directory, the base of the fixed secret
	// denials and the launch's HOME. It must be the account's own home,
	// because cfprefsd writes the global preferences there for the launch.
	Home string
	// OwnerClaudeConfig is the owner's own Claude configuration directory,
	// denied to the launch, which runs with a Freeside-owned one.
	OwnerClaudeConfig string
	// Projects is the directory holding the owner's other projects, whose
	// .env files are denied.
	Projects string
	// ProtectedRoots are denied to the launch for listing, contents, and
	// writes, and for connecting to a Unix socket under them: the daemon's
	// root, every other configured instance root, and any other directory
	// the daemon or verification trusts. Metadata stays readable, because
	// freesided resolves its default root with lstat. Sorted and unique.
	ProtectedRoots []string
	// UserTemp and UserCache are the owner's per-user temporary and cache
	// directories (confstr's DARWIN_USER_TEMP_DIR and DARWIN_USER_CACHE_DIR),
	// under which each verification job has its own freeside-verify-
	// directory the launch can't write.
	UserTemp  string
	UserCache string
	// ReservedPorts are the ports verification jobs reserve, which the
	// launch can't bind. Sorted and unique.
	ReservedPorts []int
	// AgentCLI is the absolute path of the pinned agent CLI.
	AgentCLI string
	// AgentCLISHA256 pins the agent CLI's exact build: its SHA-256, in
	// lowercase hex. A proof binds it, and the pre-job probe refuses a CLI
	// that no longer hashes to it.
	AgentCLISHA256 string
	// Path is the launch's PATH: absolute directories, in search order.
	Path []string
	// StallLimit ends a launch whose output has been silent this long.
	StallLimit time.Duration
	// EndGrace is how long a launch being ended has between SIGTERM and
	// SIGKILL.
	EndGrace time.Duration
	// EndProofTimeout bounds the run-end proof. A proof that doesn't finish
	// in time leaves the launch blocked.
	EndProofTimeout time.Duration
}

func (c Config) validate() error {
	single := []struct{ name, path string }{
		{"RunsRoot", c.RunsRoot},
		{"Home", c.Home},
		{"OwnerClaudeConfig", c.OwnerClaudeConfig},
		{"Projects", c.Projects},
		{"UserTemp", c.UserTemp},
		{"UserCache", c.UserCache},
		{"AgentCLI", c.AgentCLI},
	}
	for _, f := range single {
		if err := resolvedPath(f.path); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrInvalidConfig, f.name, err)
		}
	}
	if !sha256Hex.MatchString(c.AgentCLISHA256) {
		return fmt.Errorf("%w: AgentCLISHA256 must be 64 lowercase hex digits", ErrInvalidConfig)
	}
	if len(c.ProtectedRoots) == 0 {
		return fmt.Errorf("%w: at least one protected root is required", ErrInvalidConfig)
	}
	for i, root := range c.ProtectedRoots {
		if err := resolvedPath(root); err != nil {
			return fmt.Errorf("%w: ProtectedRoots[%d]: %w", ErrInvalidConfig, i, err)
		}
		if i > 0 && root <= c.ProtectedRoots[i-1] {
			return fmt.Errorf("%w: protected roots must be sorted and unique, %q after %q",
				ErrInvalidConfig, root, c.ProtectedRoots[i-1])
		}
		// The run root's allow follows the root denials, so a protected
		// root inside a launch's directory would be reopened to it.
		if within(root, c.RunsRoot) {
			return fmt.Errorf("%w: protected root %q is inside RunsRoot", ErrInvalidConfig, root)
		}
	}
	if !slices.ContainsFunc(c.ProtectedRoots, func(root string) bool {
		return c.RunsRoot != root && within(c.RunsRoot, root)
	}) {
		return fmt.Errorf("%w: RunsRoot %q must be inside a protected root", ErrInvalidConfig, c.RunsRoot)
	}
	for i, port := range c.ReservedPorts {
		if port < 1 || port > 65535 || (i > 0 && port <= c.ReservedPorts[i-1]) {
			return fmt.Errorf("%w: reserved ports must be sorted, unique, and in 1-65535, %d at %d",
				ErrInvalidConfig, port, i)
		}
	}
	if len(c.Path) == 0 {
		return fmt.Errorf("%w: Path is required", ErrInvalidConfig)
	}
	for i, dir := range c.Path {
		if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || strings.Contains(dir, ":") {
			return fmt.Errorf("%w: Path[%d] %q must be a clean absolute directory", ErrInvalidConfig, i, dir)
		}
	}
	durations := []struct {
		name  string
		value time.Duration
	}{
		{"StallLimit", c.StallLimit}, {"EndGrace", c.EndGrace}, {"EndProofTimeout", c.EndProofTimeout},
	}
	for _, d := range durations {
		if d.value <= 0 {
			return fmt.Errorf("%w: %s must be positive", ErrInvalidConfig, d.name)
		}
	}
	return nil
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// clone copies the configuration's reference fields, so a caller can't
// change a backend's configuration after New validated it.
func (c Config) clone() Config {
	c.ProtectedRoots = slices.Clone(c.ProtectedRoots)
	c.ReservedPorts = slices.Clone(c.ReservedPorts)
	c.Path = slices.Clone(c.Path)
	return c
}

// resolvedPath refuses a path Seatbelt would never match: one that isn't
// absolute and clean, or that passes through a symlink. A path that doesn't
// exist yet is checked through its deepest existing ancestor, so a
// configured instance root that isn't created yet is still accepted.
func resolvedPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%q must be a clean absolute path", path)
	}
	existing, rest := path, ""
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resolve %q: %w", path, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return fmt.Errorf("resolve %q: no existing ancestor", path)
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", path, err)
	}
	if filepath.Join(resolved, rest) != path {
		return fmt.Errorf("%q resolves to %q; give the resolved path", path, filepath.Join(resolved, rest))
	}
	return nil
}

// within reports whether path is root or lies under it.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}
