package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestOpenStoreWithTopicKey(t *testing.T) {
	t.Parallel()
	t.Run("fresh store mints key before opening", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		st, key, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
		if err != nil {
			t.Fatalf("open store with topic key: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if len(key) != sha256.Size {
			t.Fatalf("open store returned a %d-byte topic key, want %d", len(key), sha256.Size)
		}
		if _, err := os.Stat(dbPath + topicKeySuffix); err != nil {
			t.Fatalf("stat topic key: %v", err)
		}
	})

	t.Run("preexisting store without key fails closed", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		seed, err := store.Open(t.Context(), dbPath, store.Options{})
		if err != nil {
			t.Fatalf("seed store: %v", err)
		}
		if err := seed.Close(); err != nil {
			t.Fatalf("close seed store: %v", err)
		}
		if _, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{}); !errors.Is(err, errTopicKeyMissing) {
			t.Fatalf("open error = %v, want errTopicKeyMissing", err)
		}
	})

	t.Run("failed store open leaves reusable key before database", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		if st, _, err := openStoreWithTopicKey(
			t.Context(), dbPath, store.Options{BusyTimeout: -time.Second},
		); err == nil {
			_ = st.Close()
			t.Fatal("open store accepted a negative busy timeout")
		}
		if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("database stat error = %v, want ErrNotExist", err)
		}
		path := dbPath + topicKeySuffix
		want, err := os.ReadFile(path) //nolint:gosec // test-owned credential path
		if err != nil {
			t.Fatalf("read topic key after failed open: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat topic key after failed open: %v", err)
		}
		if len(want) != sha256.Size || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("topic key length/mode = %d/%v, want %d/0600 regular", len(want), info.Mode(), sha256.Size)
		}
		st, got, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
		if err != nil {
			t.Fatalf("retry open store: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if !bytes.Equal(got, want) {
			t.Fatal("retry replaced the orphan topic key")
		}
	})

	t.Run("existing key is loaded byte-identical", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		want, err := loadOrCreateTopicKey(dbPath, false)
		if err != nil {
			t.Fatalf("create topic key: %v", err)
		}
		st, got, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
		if err != nil {
			t.Fatalf("open store with existing topic key: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if !bytes.Equal(got, want) {
			t.Fatal("open store loaded different topic key bytes")
		}
	})
}
