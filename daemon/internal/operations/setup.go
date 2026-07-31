// Package operations composes the already-proven daemon primitives into the
// operator-facing setup, onboarding, and doctor workflows.
package operations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Layout is the single-directory Phase 1A installation layout.
type Layout struct {
	ConfigDir      string `json:"config_dir"`
	DBPath         string `json:"db_path"`
	StateDir       string `json:"state_dir"`
	CredentialsDir string `json:"credentials_dir"`
	FakeDriverDir  string `json:"fake_driver_dir"`
	AuthorityPath  string `json:"authority_path"`
}

// DefaultConfigDir returns the one-directory Phase 1A default.
func DefaultConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".freeside"), nil
}

// Setup creates the owner-private state required before the first daemon run.
// Installation of the static binary and supervisor definition remains a
// one-time, narrow privileged packaging step; the stateful daemon paths are
// always owned by the invoking non-root operator.
func Setup(ctx context.Context, configDir string) (Layout, error) {
	if configDir == "" {
		var err error
		configDir, err = DefaultConfigDir()
		if err != nil {
			return Layout{}, err
		}
	}
	absolute, err := filepath.Abs(configDir)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve config directory: %w", err)
	}
	if absolute == filepath.VolumeName(absolute)+string(os.PathSeparator) {
		return Layout{}, errors.New("configuration directory cannot be a filesystem root")
	}
	layout := Layout{
		ConfigDir:      absolute,
		DBPath:         filepath.Join(absolute, "state", "freeside.db"),
		StateDir:       filepath.Join(absolute, "state"),
		CredentialsDir: filepath.Join(absolute, "credentials"),
	}
	layout.FakeDriverDir = layout.DBPath + ".fake-stage-driver"
	layout.AuthorityPath = filepath.Join(layout.StateDir, "installation-authority.json")
	for _, dir := range []string{
		layout.ConfigDir,
		layout.StateDir,
		layout.CredentialsDir,
		layout.FakeDriverDir,
	} {
		if err := ensurePrivateDir(dir); err != nil {
			return Layout{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Layout{}, err
	}
	return layout, nil
}

func ensurePrivateDir(path string) error {
	clean := filepath.Clean(path)
	root := filepath.VolumeName(clean) + string(os.PathSeparator)
	relative := strings.TrimPrefix(clean, root)
	components := strings.Split(relative, string(os.PathSeparator))
	current := root
	for index, component := range components {
		if component == "" {
			continue
		}
		candidate := filepath.Join(current, component)
		info, err := os.Lstat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			err = os.Mkdir(candidate, 0o700)
			if errors.Is(err, fs.ErrExist) {
				info, err = os.Lstat(candidate)
			} else if err == nil {
				info, err = os.Lstat(candidate)
			}
		}
		if err != nil {
			return fmt.Errorf("inspect or create setup directory %s: %w", candidate, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// macOS exposes root-owned compatibility links such as /var ->
			// /private/var. Resolve that one platform prefix, then reject every
			// symlink in the caller-controlled remainder.
			if index == 0 && index < len(components)-1 {
				resolved, err := filepath.EvalSymlinks(candidate)
				if err != nil {
					return fmt.Errorf("resolve setup prefix %s: %w", candidate, err)
				}
				current = resolved
				continue
			}
			return fmt.Errorf("setup directory ancestor %s is a symbolic link", candidate)
		}
		if !info.IsDir() {
			return fmt.Errorf("setup path %s is not a directory", candidate)
		}
		if index == len(components)-1 && info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("setup directory %s has mode %04o, want owner-only",
				candidate, info.Mode().Perm())
		}
		current = candidate
	}
	return nil
}
