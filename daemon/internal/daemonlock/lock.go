// Package daemonlock owns the process lifetime lock for one Freeside database.
package daemonlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var (
	ErrAlreadyRunning        = errors.New("a freesided daemon already holds this database lock")
	ErrAmbiguousDatabasePath = errors.New("database has multiple hard links")
)

// Lock is acquired before a daemon opens its database and released with the
// process (including when it dies). It is deliberately not a PID-file lease.
type Lock struct {
	mu sync.Mutex
	f  *os.File
}

func Acquire(databasePath string) (*Lock, error) {
	path, err := canonicalPath(databasePath)
	if err != nil {
		return nil, err
	}
	if err := rejectMultipleLinks(path); err != nil {
		return nil, err
	}
	// #nosec G304 -- canonicalPath binds this beside the caller's database.
	f, err := os.OpenFile(path+".daemon.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("acquire daemon lock: %w", err)
	}
	return &Lock{f: f}, nil
}

func rejectMultipleLinks(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat database path: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("inspect database link count")
	}
	if stat.Nlink > 1 {
		return ErrAmbiguousDatabasePath
	}
	return nil
}

func (l *Lock) Held() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.f != nil }

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := errors.Join(syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN), l.f.Close())
	l.f = nil
	return err
}

func canonicalPath(path string) (string, error) {
	return canonicalPathDepth(path, 0)
}

func canonicalPathDepth(path string, depth int) (string, error) {
	if depth >= 40 {
		return "", errors.New("canonicalize database path: too many symlinks")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("canonicalize database path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("canonicalize database parent: %w", err)
	}
	candidate := filepath.Join(parent, filepath.Base(abs))
	info, err := os.Lstat(candidate)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", fmt.Errorf("read database symlink: %w", err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(parent, target)
		}
		return canonicalPathDepth(target, depth+1)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat database path: %w", err)
	}
	return candidate, nil
}
