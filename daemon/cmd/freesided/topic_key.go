package main

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const topicKeySuffix = ".ntfy-topic.key"

var (
	errTopicKeyPermissions = errors.New("topic key is not a private (0600) regular file")
	errTopicKeyMalformed   = errors.New("topic key is corrupt")
	errTopicKeyMissing     = errors.New("topic key is absent for an existing store")
)

// loadOrCreateTopicKey keeps per-device capability topics stable across
// daemon restarts. The path is derived from the database path rather than
// accepted from an operator, so it cannot be redirected into the database or
// blob tree. An existing store without its key fails closed instead of
// silently re-keying every paired device.
func loadOrCreateTopicKey(dbPath string, storePreexisting bool) ([]byte, error) {
	path := dbPath + topicKeySuffix
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // fixed sibling credential path
	switch {
	case err == nil:
		defer f.Close() //nolint:errcheck // the read result is the useful signal
		return readTopicKey(path, f)
	case errors.Is(err, fs.ErrNotExist):
		if storePreexisting {
			return nil, fmt.Errorf("%w: restore %s or deliberately replace the store and re-pair", errTopicKeyMissing, path)
		}
		return createTopicKey(path)
	case errors.Is(err, syscall.ELOOP):
		return nil, fmt.Errorf("topic key %s is a symlink: %w", path, errTopicKeyPermissions)
	default:
		return nil, fmt.Errorf("open topic key %s: %w", path, err)
	}
}

func readTopicKey(path string, f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat topic key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("topic key %s has mode %04o: %w",
			path, info.Mode().Perm(), errTopicKeyPermissions)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink != 1 {
		return nil, fmt.Errorf("topic key %s has %d hard links, want 1: %w",
			path, st.Nlink, errTopicKeyPermissions)
	}
	if info.Size() != sha256.Size {
		return nil, fmt.Errorf("topic key %s is %d bytes, want %d: %w",
			path, info.Size(), sha256.Size, errTopicKeyMalformed)
	}
	key := make([]byte, sha256.Size)
	if _, err := io.ReadFull(f, key); err != nil {
		return nil, fmt.Errorf("read topic key %s: %w", path, err)
	}
	return key, nil
}

func createTopicKey(path string) ([]byte, error) {
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate topic key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // fixed sibling credential path
	if err != nil {
		return nil, fmt.Errorf("create topic key %s: %w", path, err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write topic key %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sync topic key %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close topic key %s: %w", path, err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open topic key directory: %w", err)
	}
	defer dir.Close() //nolint:errcheck // Sync below is the durability signal
	if err := dir.Sync(); err != nil {
		return nil, fmt.Errorf("sync topic key directory: %w", err)
	}
	return key, nil
}
