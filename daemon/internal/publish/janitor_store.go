package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const (
	// installationAuthorityFileName is the operator-authored snapshot. The
	// daemon only ever reads it: authoring and promotion belong to onboarding
	// (#238), and a daemon that rewrote it could not be told apart from one
	// whose authority had been tampered with.
	installationAuthorityFileName = "installation-authority.json"

	// installationAuthorityMaxBytes bounds the operator's file. Exceeding it
	// denies the pass rather than truncating: a partial authority is a narrower
	// one, and a narrower authority is destructive.
	installationAuthorityMaxBytes = 1 << 20
)

// InstallationAuthorityStore serves the janitor's installation authority from a
// daemon state directory on a standalone host. It is the port's first non-test
// implementation: #263 deferred its persistence until a consumer existed, which
// left every registration inoperable behind the janitor gate.
//
// The operator's file is a reconstruction trust boundary, so every field is
// re-validated on load and any failure denies the pass. "Fails closed" is
// bounded, though, and the bounds are these. It covers structural tamper:
// truncation, a partial write, a widened mode, a symlinked component, unknown
// or duplicated fields, an over-cap file. It does not cover authorship: the
// file is plaintext under a directory the operator controls, and the
// directory's ownership is not checked, only its mode, as the keystore checks
// its own.
//
// The composing caller (#236) must pass the same state directory the keystore
// was constructed against, which keeps App credentials structurally outside it.
type InstallationAuthorityStore struct {
	dir string
	mu  sync.Mutex
}

var _ InstallationAuthoritySource = (*InstallationAuthorityStore)(nil)

// NewInstallationAuthorityStore resolves the state directory once, the way the
// keystore resolves its roots: a symlink at the directory itself is the
// operator's own choice, but nothing that appears inside it afterwards is
// followed.
func NewInstallationAuthorityStore(stateDir string) (*InstallationAuthorityStore, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("installation authority: empty state directory")
	}
	resolved, err := resolveExisting(stateDir)
	if err != nil {
		return nil, fmt.Errorf("installation authority: %w", err)
	}
	if err := rejectNonDir(resolved); err != nil {
		return nil, fmt.Errorf("installation authority: %w", err)
	}
	return &InstallationAuthorityStore{dir: resolved}, nil
}

// InstallationAuthority serves one registration's authored authority.
//
// The file is re-read on every call. A registration is reconciled against
// the authority as it stands when its turn comes, and an operator's correction
// takes effect on the next pass without a restart.
func (s *InstallationAuthorityStore) InstallationAuthority(
	ctx context.Context,
	registrationID int64,
) (InstallationAuthority, error) {
	if s == nil {
		return InstallationAuthority{}, errors.New("installation authority: nil store")
	}
	if err := ctx.Err(); err != nil {
		return InstallationAuthority{}, err
	}
	if registrationID <= 0 {
		return InstallationAuthority{}, fmt.Errorf(
			"installation authority: registration id %d is not positive: %w",
			registrationID, ErrInstallationAuthoritySnapshot,
		)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	payload, err := s.readFile(installationAuthorityFileName, installationAuthorityMaxBytes)
	if err != nil {
		return InstallationAuthority{}, fmt.Errorf(
			"installation authority: %w: %w", err, ErrInstallationAuthoritySnapshot,
		)
	}
	document, err := DecodeInstallationAuthorityDocument(payload)
	if err != nil {
		return InstallationAuthority{}, err
	}
	entry, err := document.entry(registrationID)
	if err != nil {
		return InstallationAuthority{}, err
	}
	return entry.authority(), nil
}

// readFile reads one state file under the store's directory, refusing anything
// that is not a regular, owner-only file reached without following a symlink.
// The descriptor, not the path, answers every question after the open, so a
// swap between the checks and the read cannot change what was read.
func (s *InstallationAuthorityStore) readFile(name string, maxBytes int64) ([]byte, error) {
	if err := s.assertDirectory(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, name)
	if err := rejectSymlinkedPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: hardened open of a state path derived from the resolved state directory; the descriptor's kind and mode are verified below
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only descriptor; the read result is the outcome
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %s)", path, info.Mode().Type())
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %04o, which is not owner-only", path, info.Mode().Perm())
	}
	// Size the file before reading it so an over-cap file is reported as one,
	// rather than as the unexpected EOF a truncated file produces.
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d byte limit", path, info.Size(), maxBytes)
	}
	payload, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maxBytes {
		return nil, fmt.Errorf("%s grew past the %d byte limit while being read", path, maxBytes)
	}
	return payload, nil
}

// assertDirectory refuses a state directory another account can write to.
// Directory write permission is what lets a second account swap the authority
// file; read permission does not, since the files themselves are owner-only.
// A directory that does not exist yet is not a failure: no state has been
// written, and the caller's own open or mkdir decides what happens next.
func (s *InstallationAuthorityStore) assertDirectory() error {
	if err := rejectSymlinkedPath(s.dir); err != nil {
		return err
	}
	info, err := os.Lstat(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", s.dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf(
			"%s is mode %04o, which lets another account replace its contents",
			s.dir, info.Mode().Perm(),
		)
	}
	return nil
}

// rejectSymlinkedPath proves every component of path is a real entry. O_NOFOLLOW
// only refuses a symlink at the final component, so without this walk a link at
// any parent would still redirect the read or the rename. It mirrors the
// component check mkdirAllSync runs before creating the keystore's directories.
func rejectSymlinkedPath(path string) error {
	prefix := string(filepath.Separator)
	trimmed := strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	for part := range strings.SplitSeq(trimmed, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		info, err := os.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // the rest of the path does not exist yet
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", prefix)
		}
	}
	return nil
}
