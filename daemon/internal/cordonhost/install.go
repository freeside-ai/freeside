package cordonhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// maxSocketPath is sockaddr_un's sun_path on macOS, which holds the path
// and its NUL.
const maxSocketPath = 104

// Install is where a launcher runs from: its executable, its LaunchAgent
// plist, its socket, and the protected roots of its configuration.
type Install struct {
	Binary, Plist, Socket string
	ProtectedRoots        []string
}

// CheckInstall refuses to serve from an install the run account could
// change, or from a socket a run could reach (docs/plan.md §5.7):
//
//   - The launcher must not run as root.
//   - Its binary and plist, and every directory above each, both as named
//     and with links resolved, must be owned by root and not writable by
//     the account: otherwise the account, and so any run, could replace
//     the program that holds the token at its next start.
//   - Its socket's directory, with links resolved, must lie within a
//     protected root, which the rule file denies a run, Unix socket
//     connects included, and its path must fit a socket address.
//
// Each refusal names the path. Tier A: it is the gate between a writable
// install and a launcher that serves.
func CheckInstall(in Install) error {
	return checkInstall(in, installSystem{
		euid: os.Geteuid(),
		owner: func(path string) (int, error) {
			info, err := os.Lstat(path)
			if err != nil {
				return 0, err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, fmt.Errorf("no owner for %s", path)
			}
			return int(st.Uid), nil
		},
		writable: func(path string) bool { return unix.Access(path, unix.W_OK) == nil },
		resolve:  filepath.EvalSymlinks,
	})
}

// installSystem is what the check reads of the host, so a test can say
// what it would find.
type installSystem struct {
	euid     int
	owner    func(path string) (int, error)
	writable func(path string) bool
	resolve  func(path string) (string, error)
}

func checkInstall(in Install, sys installSystem) error {
	if sys.euid == 0 {
		return errors.New("cordonhost: the launcher runs as root; it must run as the run account")
	}
	for _, path := range []string{in.Binary, in.Plist} {
		if err := checkRootOwned(path, sys); err != nil {
			return err
		}
	}
	return checkSocket(in, sys)
}

// checkRootOwned checks path and its ancestors as named, then as
// resolved, so neither a link the account owns nor a directory it can
// write sits on the way to the file.
func checkRootOwned(path string, sys installSystem) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("cordonhost: %q is not a clean absolute path", path)
	}
	resolved, err := sys.resolve(path)
	if err != nil {
		return fmt.Errorf("cordonhost: resolve %s: %w", path, err)
	}
	for _, chain := range []string{path, resolved} {
		for p := chain; ; p = filepath.Dir(p) {
			uid, err := sys.owner(p)
			if err != nil {
				return fmt.Errorf("cordonhost: inspect %s: %w", p, err)
			}
			if uid != 0 {
				return fmt.Errorf("cordonhost: %s is owned by uid %d, not root", p, uid)
			}
			if sys.writable(p) {
				return fmt.Errorf("cordonhost: %s is writable by the run account", p)
			}
			if p == "/" {
				break
			}
		}
	}
	return nil
}

func checkSocket(in Install, sys installSystem) error {
	if !filepath.IsAbs(in.Socket) || filepath.Clean(in.Socket) != in.Socket {
		return fmt.Errorf("cordonhost: socket %q is not a clean absolute path", in.Socket)
	}
	if len(in.Socket) >= maxSocketPath {
		return fmt.Errorf("cordonhost: socket %s is longer than a socket address holds (%d bytes)", in.Socket, maxSocketPath-1)
	}
	dir, err := sys.resolve(filepath.Dir(in.Socket))
	if err != nil {
		return fmt.Errorf("cordonhost: resolve socket directory %s: %w", filepath.Dir(in.Socket), err)
	}
	for _, root := range in.ProtectedRoots {
		resolved, err := sys.resolve(root)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(resolved, dir); err == nil && filepath.IsLocal(rel) {
			return nil
		}
	}
	return fmt.Errorf("cordonhost: socket %s is outside every protected root, so a run could connect to it", in.Socket)
}
