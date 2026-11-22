package verify

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ScratchPrefix starts the name of every verification's scratch directory
// under Options.ScratchParent. PrepareScratchParent removes only entries
// with this prefix.
const ScratchPrefix = "freeside-verify-"

// validScratchParent refuses a scratch parent Verify must not stage under:
// empty (which os.MkdirTemp would read as the temporary directory),
// relative (which would follow the process's working directory), or
// unclean (so the path a caller names is the path staged under).
func validScratchParent(p string) error {
	if p == "" {
		return errors.New("is unset")
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%q is not absolute", p)
	}
	if filepath.Clean(p) != p {
		return fmt.Errorf("%q is not clean", p)
	}
	return nil
}

// PrepareScratchParent makes parent ready to be Options.ScratchParent: it
// creates the directory (mode 0700) if it is missing, refuses one that is
// a symlink or not a directory, and removes every direct entry whose name
// starts with ScratchPrefix, which is scratch a stopped daemon left
// behind. Any other entry stays, so a parent named by mistake loses
// nothing but verification scratch. A prefixed symlink is removed itself,
// never followed.
//
// The sweep is best effort: an entry it can't remove (one a container
// the stopped daemon left running still writes, say) is returned in
// unremoved for the caller to report, and never fails the call. Leftover
// scratch costs only disk, because every verification stages in a fresh
// directory of its own. err reports a parent that can't be used.
//
// It deletes scratch a running verification in this process would own,
// so the caller holds the daemon lock and calls it before any
// verification in the process starts.
func PrepareScratchParent(parent string) (unremoved []error, err error) {
	if err := validScratchParent(parent); err != nil {
		return nil, fmt.Errorf("verify scratch parent: %w", err)
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create verify scratch parent: %w", err)
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("inspect verify scratch parent: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("verify scratch parent %q is not a directory (mode %s)", parent, info.Mode().Type())
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, fmt.Errorf("list verify scratch parent: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ScratchPrefix) {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		if err := removeScratch(path); err != nil {
			unremoved = append(unremoved, fmt.Errorf("remove stale verify scratch %q: %w", path, err))
		}
	}
	return unremoved, nil
}

// removeScratch removes path and everything under it. A room runs
// candidate code in a workspace it mounts read-write, and that code (or a
// Go module cache it keeps there) can leave a directory without its
// owner's write or search bit, which stops os.RemoveAll. So on failure it
// restores the owner's bits on every real directory under path, never
// through a symlink, and tries once more.
func removeScratch(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	// WalkDir reports each directory before it reads it, so a directory
	// with no read bit is opened after its chmod. Symlinks aren't followed.
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // G302: private scratch being removed
		}
		return nil
	})
	return os.RemoveAll(path)
}
