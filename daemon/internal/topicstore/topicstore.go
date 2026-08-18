// Package topicstore opens daemon stores through the ntfy topic-key boundary.
package topicstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// KeySuffix is the fixed sibling suffix for a store's ntfy topic key.
const KeySuffix = ".ntfy-topic.key"

var (
	// ErrKeyPermissions reports a topic key exposed through its type, mode, or links.
	ErrKeyPermissions = errors.New("topic key is not a private (0600) regular file")
	// ErrKeyMalformed reports a topic key with the wrong byte length.
	ErrKeyMalformed = errors.New("topic key is corrupt")
	// ErrKeyMissing reports an existing store whose topic key has been lost.
	ErrKeyMissing = errors.New("topic key is absent for an existing store")
)

type writeKeyFile func(string, []byte, fs.FileMode) error

// Open makes key creation part of store birth. The key must be durable before
// store.Open can create the database: an orphan key is safe to reuse, while an
// existing database without its key must fail closed.
func Open(
	ctx context.Context, dbPath string, opts store.Options,
) (*store.Store, []byte, error) {
	return open(ctx, dbPath, opts, atomicfile.WriteFileNoReplace)
}

func open(
	ctx context.Context, dbPath string, opts store.Options, writeKey writeKeyFile,
) (*store.Store, []byte, error) {
	_, statErr := os.Stat(dbPath)
	storePreexisting := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("stat store path: %w", statErr)
	}
	topicKey, err := loadOrCreateKey(dbPath, storePreexisting, writeKey)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(ctx, dbPath, opts)
	if err != nil {
		return nil, nil, err
	}
	return st, topicKey, nil
}

// LoadOrCreateKey keeps per-device capability topics stable across daemon
// restarts. An existing store without its key fails closed instead of silently
// re-keying every paired device.
func LoadOrCreateKey(dbPath string, storePreexisting bool) ([]byte, error) {
	return loadOrCreateKey(dbPath, storePreexisting, atomicfile.WriteFileNoReplace)
}

// InspectKey validates an existing topic key without creating or replacing
// any state. It returns only readiness, never the credential bytes.
func InspectKey(dbPath string) error {
	path := dbPath + KeySuffix
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // fixed sibling credential path
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: restore %s or deliberately replace the store and re-pair", ErrKeyMissing, path)
		}
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("topic key %s is a symlink: %w", path, ErrKeyPermissions)
		}
		return fmt.Errorf("open topic key %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // the validation result is the useful signal
	_, err = readKey(path, f)
	return err
}

func loadOrCreateKey(
	dbPath string, storePreexisting bool, writeKey writeKeyFile,
) ([]byte, error) {
	path := dbPath + KeySuffix
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // fixed sibling credential path
	switch {
	case err == nil:
		defer f.Close() //nolint:errcheck // the read result is the useful signal
		return readKey(path, f)
	case errors.Is(err, fs.ErrNotExist):
		if storePreexisting {
			return nil, fmt.Errorf("%w: restore %s or deliberately replace the store and re-pair", ErrKeyMissing, path)
		}
		return createKey(path, writeKey)
	case errors.Is(err, syscall.ELOOP):
		return nil, fmt.Errorf("topic key %s is a symlink: %w", path, ErrKeyPermissions)
	default:
		return nil, fmt.Errorf("open topic key %s: %w", path, err)
	}
}

func readKey(path string, f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat topic key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("topic key %s has mode %04o: %w",
			path, info.Mode().Perm(), ErrKeyPermissions)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink != 1 {
		return nil, fmt.Errorf("topic key %s has %d hard links, want 1: %w",
			path, st.Nlink, ErrKeyPermissions)
	}
	if info.Size() != sha256.Size {
		return nil, fmt.Errorf("topic key %s is %d bytes, want %d: %w",
			path, info.Size(), sha256.Size, ErrKeyMalformed)
	}
	key := make([]byte, sha256.Size)
	if _, err := io.ReadFull(f, key); err != nil {
		return nil, fmt.Errorf("read topic key %s: %w", path, err)
	}
	return key, nil
}

func createKey(path string, writeKey writeKeyFile) ([]byte, error) {
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate topic key: %w", err)
	}
	if err := writeKey(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("publish topic key %s: %w", path, err)
	}
	return key, nil
}
