package publish

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// authorityWriterLockFileName is the lock every process holds for as long as
// it uses the App authority. It sits in the authority state directory beside
// the janitor journal's lock, whose job is narrower: that one serializes a
// single read-modify-write, this one excludes a second holder for the whole
// life of a keystore or authority store.
const authorityWriterLockFileName = "app-authority-writer.lock"

// ErrAuthorityWriterLockHeld reports that another open file description holds
// the App authority writer lock: another process, or another acquisition in
// this one. The caller must not open the App authority.
var ErrAuthorityWriterLockHeld = errors.New("app authority writer lock is held by another holder")

// errAuthorityWriterLockUnusable is the refusal NewKeystore and
// NewInstallationAuthorityStore give a lock that cannot prove its holder still
// excludes every other writer.
var errAuthorityWriterLockUnusable = errors.New("unusable app authority writer lock")

// AuthorityWriterLock is a held, non-blocking exclusive flock on the App
// authority state directory. Its holder is the only process that may open the
// keystore or the installation authority store over that directory: the two
// constructors take the lock in place of the directory, so a caller cannot
// open the authority without holding it.
//
// The lock belongs to the open file description, so the kernel drops it when
// the holder exits however it exits, and a second acquisition in the same
// process conflicts with the first. Go opens the file close-on-exec, so a
// child process (a git publish, a container runtime) does not keep it past
// its exec. Between fork and exec the child does share the description, so
// Close unlocks it explicitly rather than relying on the close alone.
type AuthorityWriterLock struct {
	dir string

	mu   sync.Mutex
	file *os.File
}

// AcquireAuthorityWriterLock takes the writer lock on stateDir or fails at
// once with ErrAuthorityWriterLockHeld. The directory must exist and is
// resolved the way the authority store resolves it, so a lock and a store
// built from different spellings of one directory still match. A symlink at
// the lock file, or at any component below the resolved directory, is
// refused: following one would lock a file other holders never see.
func AcquireAuthorityWriterLock(stateDir string) (*AuthorityWriterLock, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("app authority writer lock: empty state directory")
	}
	resolved, err := resolveExisting(stateDir)
	if err != nil {
		return nil, fmt.Errorf("app authority writer lock: %w", err)
	}
	info, err := os.Lstat(resolved) //nolint:gosec // G703: the caller's own state directory, resolved
	if err != nil {
		return nil, fmt.Errorf("app authority writer lock: state directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("app authority writer lock: %s is not a directory", resolved)
	}
	path := filepath.Join(resolved, authorityWriterLockFileName)
	if err := rejectSymlinkedPath(path); err != nil {
		return nil, fmt.Errorf("app authority writer lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // G304: fixed name under the resolved state directory
	if err != nil {
		return nil, fmt.Errorf("app authority writer lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrAuthorityWriterLockHeld, path)
		}
		return nil, fmt.Errorf("app authority writer lock: %w", err)
	}
	return &AuthorityWriterLock{dir: resolved, file: f}, nil
}

// Dir is the resolved state directory the lock covers.
func (l *AuthorityWriterLock) Dir() string { return l.dir }

// Close releases the lock. It is idempotent, and a nil lock is a no-op so a
// deferred Close needs no guard. A keystore or store opened under the lock
// keeps working after Close, so the holder closes it only once it has stopped
// using them.
//
// Closing the descriptor alone is not enough: a child another goroutine has
// forked but not yet exec'd shares the open file description, and the lock
// lives as long as the description does. An explicit LOCK_UN through any
// descriptor releases it for every sharer, so the lock is free when Close
// returns.
func (l *AuthorityWriterLock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	if unlockErr != nil {
		unlockErr = fmt.Errorf("app authority writer lock: unlock: %w", unlockErr)
	}
	err := errors.Join(unlockErr, l.file.Close())
	l.file = nil
	return err
}

// Covers refuses a lock that is nil, released, or held on a directory other
// than stateDir. A composer that carries the state directory beside the lock
// calls it, so a lock taken on one directory never stands in for another.
func (l *AuthorityWriterLock) Covers(stateDir string) error {
	dir, err := l.requireHeld()
	if err != nil {
		return err
	}
	resolved, err := resolveExisting(stateDir)
	if err != nil {
		return fmt.Errorf("app authority writer lock: %w", err)
	}
	if resolved != dir {
		return fmt.Errorf("%w: held on %s, not %s", errAuthorityWriterLockUnusable, dir, resolved)
	}
	return nil
}

// requireHeld returns the state directory of a lock still held, or the
// constructors' refusal for a nil or released one.
func (l *AuthorityWriterLock) requireHeld() (string, error) {
	if l == nil {
		return "", fmt.Errorf("%w: nil lock", errAuthorityWriterLockUnusable)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return "", fmt.Errorf("%w: released", errAuthorityWriterLockUnusable)
	}
	return l.dir, nil
}
