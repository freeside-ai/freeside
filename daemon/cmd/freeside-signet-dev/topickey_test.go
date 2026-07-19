package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestLoadOrCreateTopicKeyStates exercises the fail-closed state table
// (issue #133): create only against a fresh store, load an intact private
// file, and refuse every questionable case rather than regenerate over it.
func TestLoadOrCreateTopicKeyStates(t *testing.T) {
	t.Run("creates a 0600 key against a fresh store", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "topic.key")
		key, err := loadOrCreateTopicKey(path, false)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if len(key) != topicKeyLen {
			t.Fatalf("key length = %d, want %d", len(key), topicKeyLen)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("key file mode = %04o, want 0600", info.Mode().Perm())
		}
	})

	t.Run("reloads the persisted key unchanged even once the store exists", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "topic.key")
		first, err := loadOrCreateTopicKey(path, false)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		second, err := loadOrCreateTopicKey(path, true)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Errorf("reloaded key differs from the persisted key")
		}
	})

	t.Run("absent key against an existing store fails closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "topic.key")
		if _, err := loadOrCreateTopicKey(path, true); !errors.Is(err, errTopicKeyAbsentForStore) {
			t.Fatalf("err = %v, want errTopicKeyAbsentForStore", err)
		}
	})

	t.Run("widened permissions fail closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "topic.key")
		if _, err := loadOrCreateTopicKey(path, false); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // deliberately widening perms to prove the load path rejects an exposed key
			t.Fatalf("chmod: %v", err)
		}
		if _, err := loadOrCreateTopicKey(path, true); !errors.Is(err, errTopicKeyPermissions) {
			t.Fatalf("err = %v, want errTopicKeyPermissions", err)
		}
	})

	t.Run("a symlink to a valid key fails closed", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "real.key")
		if _, err := loadOrCreateTopicKey(target, false); err != nil {
			t.Fatalf("create target: %v", err)
		}
		link := filepath.Join(dir, "link.key")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := loadOrCreateTopicKey(link, true); !errors.Is(err, errTopicKeyPermissions) {
			t.Fatalf("err = %v, want errTopicKeyPermissions", err)
		}
	})

	t.Run("a wrong-length key fails closed", func(t *testing.T) {
		for name, content := range map[string][]byte{
			"too short":                      []byte("too short"),
			"one over":                       make([]byte, topicKeyLen+1),
			"much too big (bounds the read)": make([]byte, 8<<20),
		} {
			path := filepath.Join(t.TempDir(), "topic.key")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatalf("%s: write: %v", name, err)
			}
			if _, err := loadOrCreateTopicKey(path, false); !errors.Is(err, errTopicKeyMalformed) {
				t.Errorf("%s: err = %v, want errTopicKeyMalformed", name, err)
			}
		}
	})

	t.Run("a hard-linked key fails closed", func(t *testing.T) {
		// A second name for the same inode (e.g. hard-linked into the served
		// blob tree) exposes the key bytes; the path check cannot see it.
		dir := t.TempDir()
		path := filepath.Join(dir, "topic.key")
		if _, err := loadOrCreateTopicKey(path, false); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := os.Link(path, filepath.Join(dir, "alias.key")); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		if _, err := loadOrCreateTopicKey(path, true); !errors.Is(err, errTopicKeyPermissions) {
			t.Errorf("hard-linked key: err = %v, want errTopicKeyPermissions", err)
		}
	})

	t.Run("create refuses to write through an existing file", func(t *testing.T) {
		// O_EXCL: a valid file present with a fresh-store flag is a load, never
		// an overwrite, so an intact key is never clobbered by a mis-sampled
		// store-existence flag.
		path := filepath.Join(t.TempDir(), "topic.key")
		first, err := loadOrCreateTopicKey(path, false)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		again, err := loadOrCreateTopicKey(path, false)
		if err != nil {
			t.Fatalf("second call: %v", err)
		}
		if !bytes.Equal(first, again) {
			t.Errorf("second call minted a new key instead of loading the existing one")
		}
	})
}

// TestResolveTopicKeyPolicy exercises the composition policy layered over the
// state table: persist when a path is given, stay ephemeral only against a
// fresh store, and refuse a reused store that carries no persisted key.
func TestResolveTopicKeyPolicy(t *testing.T) {
	t.Run("no file + fresh store mints an ephemeral key", func(t *testing.T) {
		key, err := resolveTopicKey("", filepath.Join(t.TempDir(), "signet.db"), false)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(key) != topicKeyLen {
			t.Errorf("key length = %d, want %d", len(key), topicKeyLen)
		}
	})

	t.Run("no file + existing store fails closed", func(t *testing.T) {
		if _, err := resolveTopicKey("", filepath.Join(t.TempDir(), "signet.db"), true); !errors.Is(err, errTopicKeyAbsentForStore) {
			t.Fatalf("err = %v, want errTopicKeyAbsentForStore", err)
		}
	})

	t.Run("a file path persists across calls", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "signet.db")
		path := filepath.Join(t.TempDir(), "topic.key")
		first, err := resolveTopicKey(path, dbPath, false)
		if err != nil {
			t.Fatalf("first resolve: %v", err)
		}
		second, err := resolveTopicKey(path, dbPath, true)
		if err != nil {
			t.Fatalf("second resolve: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Errorf("persisted key changed across resolve calls")
		}
	})
}

// TestResolveTopicKeyRejectsStorePaths covers the custody boundary (Codex P1):
// a key path that coincides with the store file or lands inside its blob tree
// is refused before any key is written, so the derive-all-topics secret never
// enters the backup/workspace surface or the attachment-served blob store.
func TestResolveTopicKeyRejectsStorePaths(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "signet.db")
	for name, keyPath := range map[string]string{
		"the store file itself":     dbPath,
		"a SQLite WAL sidecar":      dbPath + "-wal",
		"the blob store dir":        dbPath + ".blobs",
		"a file in the blobtree":    filepath.Join(dbPath+".blobs", "ab", "cdef"),
		"the checkpoint dir":        dbPath + ".checkpoints",
		"a file in the checkpoints": filepath.Join(dbPath+".checkpoints", "topic.key"),
	} {
		if _, err := resolveTopicKey(keyPath, dbPath, false); !errors.Is(err, errTopicKeyInStore) {
			t.Errorf("%s: err = %v, want errTopicKeyInStore", name, err)
		}
	}
	// A disjoint sibling directory is accepted.
	if _, err := resolveTopicKey(filepath.Join(t.TempDir(), "topic.key"), dbPath, false); err != nil {
		t.Errorf("disjoint key path rejected: %v", err)
	}
}

// TestResolveTopicKeyRejectsAliasedStorePaths widens Codex's second P1: a
// lexical check misses a key path that reaches the blob tree through a
// symlinked parent, because filepath.Abs does not resolve the alias. Resolving
// the deepest existing ancestor before comparison closes it.
func TestResolveTopicKeyRejectsAliasedStorePaths(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "signet.db")
	blobs := dbPath + ".blobs"
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	// A symlinked parent pointing into the blob store: the key path is
	// lexically outside .blobs but physically lands inside it.
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(blobs, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := resolveTopicKey(filepath.Join(alias, "topic.key"), dbPath, false); !errors.Is(err, errTopicKeyInStore) {
		t.Errorf("aliased-into-blobs key path: err = %v, want errTopicKeyInStore", err)
	}
}

// TestResolveTopicKeyResolvesSymlinkedDB guards the divergent-blob-path
// blocker: when -db is a symlink, the real blob store is `<dbPath>.blobs`
// beside the symlink, not `<resolved-db>.blobs`. Resolving dbPath and then
// appending ".blobs" would compute the wrong tree and accept a key inside the
// real one. The symlink target exists so the resolved db differs from the raw
// path (a dangling link would rejoin lexically and mask the bug).
func TestResolveTopicKeyResolvesSymlinkedDB(t *testing.T) {
	target := filepath.Join(t.TempDir(), "realdb")
	if err := os.WriteFile(target, []byte("db"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db") // a symlink resolving elsewhere
	if err := os.Symlink(target, dbPath); err != nil {
		t.Fatalf("symlink -db: %v", err)
	}
	blobs := dbPath + ".blobs" // the real blob tree beside the symlink
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	if _, err := resolveTopicKey(filepath.Join(blobs, "topic.key"), dbPath, false); !errors.Is(err, errTopicKeyInStore) {
		t.Errorf("key in the symlinked db's real blob tree: err = %v, want errTopicKeyInStore", err)
	}
}

// TestResolveTopicKeyRejectsResolvedSidecars closes the raw-vs-resolved
// sidecar gap: SQLite names -wal/-shm/-journal beside the resolved database
// target, so a key at `<resolved-db>-journal` sits on the store surface even
// though the raw `<dbPath>-journal` name differs. The store surface is derived
// from both the raw and the resolved db path, so either is refused.
func TestResolveTopicKeyRejectsResolvedSidecars(t *testing.T) {
	realDir := t.TempDir()
	realDB := filepath.Join(realDir, "real.db")
	if err := os.WriteFile(realDB, []byte("db"), 0o600); err != nil {
		t.Fatalf("write db target: %v", err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db")
	if err := os.Symlink(realDB, dbPath); err != nil {
		t.Fatalf("symlink -db: %v", err)
	}
	for _, sidecar := range []string{"-wal", "-shm", "-journal"} {
		keyPath := realDB + sidecar // beside the resolved target, in the backup dir
		if _, err := resolveTopicKey(keyPath, dbPath, false); !errors.Is(err, errTopicKeyInStore) {
			t.Errorf("key at resolved sidecar %s: err = %v, want errTopicKeyInStore", keyPath, err)
		}
	}
}

// TestResolveTopicKeyRejectsSidecarSymlink closes Codex round-5's alias gap:
// a sidecar beside the resolved db that is itself a symlink onto the key file
// is caught because each resolved-db-derived store path is itself resolved.
func TestResolveTopicKeyRejectsSidecarSymlink(t *testing.T) {
	realDir := t.TempDir()
	realDB := filepath.Join(realDir, "real.db")
	if err := os.WriteFile(realDB, []byte("db"), 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "db")
	if err := os.Symlink(realDB, dbPath); err != nil {
		t.Fatalf("symlink -db: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "topic.key")
	if err := os.WriteFile(keyFile, make([]byte, topicKeyLen), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	// A sidecar beside the resolved db aliased onto the key file.
	if err := os.Symlink(keyFile, realDB+"-journal"); err != nil {
		t.Fatalf("symlink sidecar: %v", err)
	}
	if _, err := resolveTopicKey(keyFile, dbPath, false); !errors.Is(err, errTopicKeyInStore) {
		t.Errorf("key aliased by a resolved-db sidecar: err = %v, want errTopicKeyInStore", err)
	}
}

// TestResolveTopicKeyRejectsDanglingStoreSymlink closes Codex round-6: a
// dangling store-surface symlink (e.g. a sidecar pointing at the not-yet-created
// key) must fail closed, not be rejoined lexically, or createTopicKey would
// materialize the target and alias the store file onto the key.
func TestResolveTopicKeyRejectsDanglingStoreSymlink(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db")                  // fresh, never opened
	keyFile := filepath.Join(dir, "creds", "topic.key") // does not exist yet
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		t.Fatalf("mkdir creds: %v", err)
	}
	// A store sidecar dangling onto the intended key path.
	if err := os.Symlink(keyFile, dbPath+"-wal"); err != nil {
		t.Fatalf("dangling symlink: %v", err)
	}
	if _, err := resolveTopicKey(keyFile, dbPath, false); err == nil {
		t.Error("dangling store-surface symlink accepted, want a fail-closed refusal")
	}
	if _, statErr := os.Stat(keyFile); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("key was created despite the dangling store symlink: stat err = %v", statErr)
	}
}

// TestLoadOrCreateTopicKeyRejectsFifo covers Codex P2: a FIFO at the key path
// must fail closed as a non-regular file rather than block the open forever
// waiting for a writer. The bounded goroutine proves the call returns.
func TestLoadOrCreateTopicKeyRejectsFifo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topic.key")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := loadOrCreateTopicKey(path, true)
		done <- result{err}
	}()
	select {
	case r := <-done:
		if !errors.Is(r.err, errTopicKeyPermissions) {
			t.Fatalf("err = %v, want errTopicKeyPermissions for a non-regular file", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loadOrCreateTopicKey blocked on a FIFO key path instead of failing closed")
	}
}
