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
	"strings"
	"syscall"
)

// topicKeyLen is the ntfy topic key's fixed length. It must be at least
// sha256.Size because signet derives per-device topics as HMAC-SHA256 under
// this key (daemon/internal/signet/ntfy.go), and a topic is an unguessable
// capability URL; a short key would let one paired device brute-force another
// device's topic. We pin exactly 32 rather than "at least 32" so a truncated
// file is a corruption signal, not a silently weaker key.
const topicKeyLen = sha256.Size

// Topic key custody mirrors the publish keystore's hardened credential-file
// pattern (daemon/internal/publish/keystore.go) at small scale rather than
// importing it: the redaction/containment discipline is shared, the lane is
// not (see daemon/internal/signet/secret.go). The key is a daemon-held secret
// that derives every device's private topic, so it lives in its own 0600 file
// on a path disjoint from the SQLite store, never in the store itself: the
// store is the checkpoint/backup and workspace-mount surface (plan §5.10,
// §5.4), which this key must stay out of.
var (
	// errTopicKeyPermissions rejects a key file that is a symlink, a
	// directory, or group/other-accessible: any of those means the secret
	// must be treated as exposed, exactly as the keystore treats a widened
	// App-credential file.
	errTopicKeyPermissions = errors.New("topic key file is not a private (0600) regular file")
	// errTopicKeyMalformed rejects a key file that is not exactly
	// topicKeyLen bytes; regenerating over it would silently rekey devices.
	errTopicKeyMalformed = errors.New("topic key file is corrupt")
	// errTopicKeyInStore rejects a key path that lands on the store's own
	// files or inside its blob tree: those are the checkpoint/backup and
	// workspace-mount surface the derive-all-topics secret must stay out of,
	// and a digest-shaped name under the blob tree could even be served by the
	// attachment handler.
	errTopicKeyInStore = errors.New("topic key file must be disjoint from the store and its blob tree")
	// errTopicKeyAbsentForStore is the fail-closed core of issue #133: the
	// key file is gone but the store may already hold paired devices, so
	// minting a fresh key would strand every prior device on a topic the
	// daemon no longer publishes to. Creating a key is only safe against a
	// store that has never been opened.
	errTopicKeyAbsentForStore = errors.New("topic key file is absent but the store already exists")
)

// resolveTopicKey applies the dev harness's composition policy for the ntfy
// topic key (issue #133):
//
//   - -topic-key-file is set: persist across restarts (loadOrCreateTopicKey),
//     so a device paired before a restart keeps its subscription;
//   - no file + fresh store: keep the historical per-process key, meaningful
//     only to this process (the §5.14 convergence suite's posture, which
//     never restarts a reused store);
//   - no file + pre-existing store: fail closed, since the store's devices
//     were paired under a key this process cannot reproduce, and a fresh key
//     would strand every one of them.
func resolveTopicKey(path, dbPath string, storePreexisting bool) ([]byte, error) {
	if path != "" {
		if err := ensureTopicKeyDisjoint(path, dbPath); err != nil {
			return nil, err
		}
		return loadOrCreateTopicKey(path, storePreexisting)
	}
	if storePreexisting {
		return nil, fmt.Errorf(
			"%w: pass -topic-key-file to persist device topics across restarts, "+
				"or start against a fresh -db",
			errTopicKeyAbsentForStore)
	}
	key := make([]byte, topicKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("topic key: generate ephemeral: %w", err)
	}
	return key, nil
}

// loadOrCreateTopicKey returns the ntfy topic key persisted at path, so a
// device paired before a restart keeps the same subscription after it. The
// four states and their dispositions (issue #133):
//
//   - present, private, exactly topicKeyLen bytes -> load and use;
//   - absent + store never opened (storePreexisting == false) -> mint and
//     persist a fresh key (the genuine first run);
//   - absent + store already exists -> fail closed, because a fresh key here
//     would silently rekey existing devices;
//   - present but a symlink/dir, widened, or the wrong length -> fail closed,
//     never regenerate over questionable material.
//
// The caller passes storePreexisting from an os.Stat of the store path taken
// before store.Open (which creates the file), so a pre-existing store is the
// conservative proxy for "may hold paired devices"; it over-refuses a
// pre-existing-but-empty store, which fails safe.
func loadOrCreateTopicKey(path string, storePreexisting bool) ([]byte, error) {
	// O_NOFOLLOW closes the symlink-swap race a separate Lstat-then-read would
	// leave open: the final path component is never followed, and the mode
	// check below runs against the opened descriptor (fstat), not a path that
	// could be repointed between two syscalls. This is stricter than the
	// keystore's assertMode it otherwise mirrors. O_NONBLOCK keeps a FIFO or
	// device at the key path from blocking the open forever before fstat can
	// reject the non-regular file; it is a no-op on the regular file we expect.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // operator-supplied dev-harness credential path
	switch {
	case err == nil:
		defer f.Close() //nolint:errcheck // read-only descriptor; the read result is the durability signal
		return loadTopicKey(path, f)
	case errors.Is(err, fs.ErrNotExist):
		if storePreexisting {
			return nil, fmt.Errorf(
				"%w (%s): refusing to mint a new key against an existing store, "+
					"which would rekey every paired device; restore the original "+
					"key file, or remove the store to re-pair and rotate deliberately",
				errTopicKeyAbsentForStore, path)
		}
		return createTopicKey(path)
	case errors.Is(err, syscall.ELOOP):
		// A symlink at the key path: O_NOFOLLOW refuses to follow it, so the
		// key material's real location is not what the operator configured.
		return nil, fmt.Errorf("topic key %s is a symlink: %w", path, errTopicKeyPermissions)
	default:
		return nil, fmt.Errorf("topic key: open %s: %w", path, err)
	}
}

// loadTopicKey validates the opened key file and returns its bytes. It asserts
// kind and permissions from the descriptor's own fstat on every load, so a
// key file that has become non-regular or group-readable since it was written
// fails closed rather than being trusted.
func loadTopicKey(path string, f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("topic key: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("topic key %s is not a regular file: %w", path, errTopicKeyPermissions)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("topic key %s is mode %04o: %w", path, info.Mode().Perm(), errTopicKeyPermissions)
	}
	// A credential file must have exactly one name. A second hard link (e.g. a
	// digest-shaped name under the blob tree that the attachment handler could
	// serve) aliases the same key bytes into the store surface, which the
	// path-based disjointness check cannot see because a hard link is not a
	// symlink. Fail closed on any extra link.
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return nil, fmt.Errorf("topic key %s has %d hard links, want 1: %w", path, st.Nlink, errTopicKeyPermissions)
	}
	// Reject the wrong size from the descriptor's own fstat before reading, so
	// a misplaced or replaced oversized file fails closed instead of being
	// slurped whole; the read is then bounded to exactly topicKeyLen bytes.
	if info.Size() != topicKeyLen {
		return nil, fmt.Errorf("topic key %s is %d bytes, want %d: %w", path, info.Size(), topicKeyLen, errTopicKeyMalformed)
	}
	key := make([]byte, topicKeyLen)
	if _, err := io.ReadFull(f, key); err != nil {
		// ErrUnexpectedEOF means the file shrank since the size check; any
		// short/failed read fails closed rather than yielding a partial key.
		return nil, fmt.Errorf("topic key: read %s: %w", path, err)
	}
	return key, nil
}

// createTopicKey mints a fresh key and writes it owner-only, syncing the file
// and its directory entry so the key survives a crash right after creation
// (mirroring the keystore's writeFileExclSync). O_EXCL guarantees no
// pre-existing inode is written through, so a concurrent racer cannot pre-seed
// the file.
func createTopicKey(path string) ([]byte, error) {
	key := make([]byte, topicKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("topic key: generate: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // operator-supplied dev-harness credential path
	if err != nil {
		return nil, fmt.Errorf("topic key: create %s: %w", path, err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("topic key: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("topic key: sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("topic key: close %s: %w", path, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("topic key: sync dir %s: %w", path, err)
	}
	return key, nil
}

// ensureTopicKeyDisjoint refuses a topic-key path that coincides with the
// store's own files or lands inside its blob tree. Both are the
// checkpoint/backup and workspace-mount surface (plan §5.10, §5.4) this
// credential must stay out of, and a digest-shaped name under the blob tree
// could be served by the attachment handler. Both paths are resolved through
// their deepest existing ancestor (so a symlinked parent that reaches the blob
// store cannot slip past a purely lexical check) and case-folded before
// comparison, mirroring the publish keystore's disjointness gate.
func ensureTopicKeyDisjoint(keyPath, dbPath string) error {
	key, err := resolveExisting(keyPath)
	if err != nil {
		return fmt.Errorf("topic key: resolve %s: %w", keyPath, err)
	}
	// Case-fold before comparing: filepath.Rel is case-sensitive, so on a
	// case-insensitive filesystem (macOS APFS default) a key path differing
	// from the store only in case would pass the lexical check while
	// physically nesting. Folding over-rejects on case-sensitive volumes,
	// which is the fail-closed direction.
	key = strings.ToLower(key)
	files, dirs, err := storeSurface(dbPath)
	if err != nil {
		return err
	}
	for _, f := range files {
		if key == strings.ToLower(f) {
			return fmt.Errorf("%w: %s coincides with the store file %s", errTopicKeyInStore, keyPath, f)
		}
	}
	for _, d := range dirs {
		if within(strings.ToLower(d), key) {
			return fmt.Errorf("%w: %s is inside the store directory %s", errTopicKeyInStore, keyPath, d)
		}
	}
	return nil
}

// storeSurface returns the physical files and directories the store occupies,
// which the topic key must stay clear of. Each path is derived two ways and
// the union taken, because a symlinked -db makes the two diverge: from the raw
// strings the composition passes (store.Open(dbPath), NewBlobStore(dbPath +
// ".blobs")), and from the symlink-resolved database target (SQLite names its
// -wal/-shm/-journal sidecars beside the resolved file, not the link). Covering
// both derivations for every store path closes the raw-vs-resolved gap by
// construction; over-inclusion is the fail-closed direction.
func storeSurface(dbPath string) (files, dirs []string, err error) {
	resolvedDB, err := resolveExisting(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("topic key: resolve store path %s: %w", dbPath, err)
	}
	seenFile, seenDir := map[string]bool{}, map[string]bool{}
	// Every candidate is itself resolved through resolveExisting before it is
	// recorded: the resolved-db-derived names (resolvedDB + suffix) could each
	// be a symlink of their own (e.g. a sidecar aliased onto the key file), so
	// comparing against the raw joined string would miss it.
	add := func(candidate string, seen map[string]bool, out *[]string) error {
		resolved, err := resolveExisting(candidate)
		if err != nil {
			return fmt.Errorf("topic key: resolve store path %s: %w", candidate, err)
		}
		if !seen[resolved] {
			seen[resolved] = true
			*out = append(*out, resolved)
		}
		return nil
	}
	// The db file and SQLite's sidecars, derived from both the raw path the
	// composition passes and the symlink-resolved database target.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := add(dbPath+suffix, seenFile, &files); err != nil {
			return nil, nil, err
		}
		if err := add(resolvedDB+suffix, seenFile, &files); err != nil {
			return nil, nil, err
		}
	}
	// The blob store directory NewBlobStore opens beside the db, and the
	// checkpoint directory the control surface writes store snapshots into:
	// both are backup/workspace surfaces the derive-all topic key must stay
	// out of, or copying an artifact would carry the live credential.
	for _, suffix := range []string{".blobs", ".checkpoints"} {
		if err := add(dbPath+suffix, seenDir, &dirs); err != nil {
			return nil, nil, err
		}
		if err := add(resolvedDB+suffix, seenDir, &dirs); err != nil {
			return nil, nil, err
		}
	}
	return files, dirs, nil
}

// resolveExisting makes path absolute and resolves symlinks through its
// deepest existing ancestor, rejoining any not-yet-created remainder, so a
// containment comparison sees the real filesystem location even before the
// key file (or the store) exists. Any error other than non-existence fails
// closed. Mirrors the publish keystore's helper of the same name.
func resolveExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for cur := filepath.Clean(abs); ; {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		// EvalSymlinks reports ErrNotExist both for a genuinely absent
		// component and for an existing but dangling symlink. Lstat
		// distinguishes them: an existing entry that will not resolve (a
		// dangling symlink) must fail closed, not be rejoined lexically — a
		// store-surface path dangling onto the key would otherwise slip the
		// disjointness gate, then createTopicKey would materialize the target
		// and alias the store file onto the key.
		if _, lerr := os.Lstat(cur); lerr == nil {
			return "", fmt.Errorf("path component %s exists but does not resolve (dangling symlink?): %w", cur, fs.ErrInvalid)
		} else if !errors.Is(lerr, fs.ErrNotExist) {
			return "", lerr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no existing ancestor for %s", abs)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within reports whether target is base itself or a descendant of it. Both
// arguments must already be absolute and resolved.
func within(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// syncDir fsyncs a directory so a newly created entry inside it is durable:
// syncing only the file does not persist the entry on POSIX filesystems.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dev-harness directory path only
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // Sync is the durability barrier; close only releases the descriptor
	return d.Sync()
}
