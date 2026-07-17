package importer

import (
	"crypto/sha1" //nolint:gosec // G505: git object identity under the sha1 object format, not cryptographic integrity
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// blobHasher accumulates a sha256 digest and byte count from a stream,
// for hashing content that must not buffer in memory.
type blobHasher struct {
	h    hash.Hash
	size int64
}

func newBlobHasher() *blobHasher {
	return &blobHasher{h: sha256.New()}
}

func (b *blobHasher) Write(p []byte) (int, error) {
	b.size += int64(len(p))
	return b.h.Write(p)
}

func (b *blobHasher) digest() export.Digest {
	return export.Digest("sha256:" + hex.EncodeToString(b.h.Sum(nil)))
}

// blobInfo is what content verification proved about one stored blob:
// its size and its git blob object name, both derived from the same
// single stream that verified the manifest's sha256 digest.
type blobInfo struct {
	size         int64
	gitOID       string
	verifiedPath string
}

// verifyBlobs audits the handoff blob store against the manifest and
// verifies every stored blob's content. The audit is exact in both
// directions: every digest the manifest stores must exist as a blob
// file, and nothing else may exist anywhere in the handoff directory —
// the exporter writes exactly manifest.json plus the blobs the manifest
// references, so a stray path (including a blob for an entry marked
// blob_omitted) is hostile, not noise. Each stored blob then streams
// once through sha256 (binding content to the manifest digest) and the
// git blob object derivation that change derivation and commit
// construction rely on later. The same stream writes a daemon-private
// snapshot, so no later stage re-resolves a handoff pathname.
func verifyBlobs(handoffDir, scratch string, m export.Manifest, pol Policy) (map[export.Digest]blobInfo, error) {
	needed := make(map[export.Digest]int64, len(m.Entries))
	var storedTotal int64
	for _, e := range m.Entries {
		if e.Kind != export.EntryRegular || e.BlobOmitted {
			continue
		}
		// Enforce the stored-blob size caps before opening or hashing any
		// blob: verifyBlob reads the whole file to bind its digest, so a
		// forged manifest declaring a huge stored blob (or many blobs
		// summing past the total) would otherwise burn that much disk I/O
		// at the untrusted boundary. An honest exporter never stores a
		// blob past these caps (it omits it), so an over-cap stored blob
		// is contract-impossible; fail closed. Deduplicated blobs are
		// counted once (the total bounds bytes actually read).
		if pol.MaxBlobBytes > 0 && *e.Size > pol.MaxBlobBytes {
			return nil, fmt.Errorf("stored blob %s declares %d bytes, over the %d-byte cap: %w", *e.Digest, *e.Size, pol.MaxBlobBytes, ErrBlobTooLarge)
		}
		if prev, ok := needed[*e.Digest]; ok {
			if prev != *e.Size {
				return nil, fmt.Errorf("digest %s claimed at sizes %d and %d: %w", *e.Digest, prev, *e.Size, ErrSizeMismatch)
			}
			continue // already counted; dedup is free
		}
		// Overflow-safe: storedTotal never exceeds MaxTotalBytes, so the
		// subtraction cannot wrap.
		if pol.MaxTotalBytes > 0 && *e.Size > pol.MaxTotalBytes-storedTotal {
			return nil, fmt.Errorf("stored blobs exceed the %d-byte total cap: %w", pol.MaxTotalBytes, ErrBlobTooLarge)
		}
		storedTotal += *e.Size
		needed[*e.Digest] = *e.Size
	}
	sha256Dir, err := auditHandoffLayout(handoffDir, needed)
	if err != nil {
		return nil, err
	}
	if sha256Dir != nil {
		defer func() { _ = sha256Dir.Close() }()
	}
	verifiedDir := filepath.Join(scratch, "verified-blobs")
	if err := os.Mkdir(verifiedDir, 0o700); err != nil {
		return nil, fmt.Errorf("create verified-blob scratch: %w", err)
	}
	blobs := make(map[export.Digest]blobInfo, len(needed))
	for digest, size := range needed {
		if sha256Dir == nil {
			return nil, fmt.Errorf("manifest stores %s but the handoff has no blob directory: %w", digest, ErrMissingBlob)
		}
		verifiedPath := filepath.Join(verifiedDir, strings.TrimPrefix(string(digest), "sha256:"))
		info, err := snapshotVerifiedBlob(sha256Dir, verifiedPath, digest, size)
		if err != nil {
			return nil, err
		}
		blobs[digest] = info
	}
	return blobs, nil
}

// auditHandoffLayout enforces the exact handoff shape: the root holds
// manifest.json and at most a blobs/ directory, blobs/ holds at most a
// sha256/ directory, and sha256/ holds exactly the needed digests as
// regular files (a symlink or subdirectory there is hostile). Every
// level is enumerated in bounded batches and aborts on the first
// unexpected entry, so a hostile handoff that stuffs a directory with
// millions of names cannot force the whole listing into memory before
// the audit rejects it.
func auditHandoffLayout(dir string, needed map[export.Digest]int64) (*os.File, error) {
	root, err := openDirectory(dir, ErrHandoffUnreadable)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w: %w", dir, ErrHandoffUnreadable, err)
	}
	defer func() { _ = root.Close() }()
	blobsDir := false
	if err := scanOpenDirBatched(root, dir, ErrHandoffUnreadable, func(de os.DirEntry) error {
		switch {
		case de.Name() == export.ManifestFilename && de.Type().IsRegular():
			return nil
		case de.Name() == "blobs" && de.IsDir():
			blobsDir = true
			return nil
		default:
			return fmt.Errorf("unexpected handoff entry %q: %w", de.Name(), ErrOrphanBlob)
		}
	}); err != nil {
		return nil, err
	}
	if !blobsDir {
		if len(needed) > 0 {
			return nil, fmt.Errorf("manifest references blobs but the handoff has no blob store: %w", ErrMissingBlob)
		}
		return nil, nil
	}
	blobsPath := filepath.Join(dir, "blobs")
	blobs, err := openDirectoryAt(root, "blobs", ErrHandoffUnreadable)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w: %w", blobsPath, ErrHandoffUnreadable, err)
	}
	defer func() { _ = blobs.Close() }()
	sha256Dir := false
	if err := scanOpenDirBatched(blobs, blobsPath, ErrHandoffUnreadable, func(de os.DirEntry) error {
		if de.Name() != "sha256" || !de.IsDir() {
			return fmt.Errorf("unexpected blob-store entry %q: %w", de.Name(), ErrOrphanBlob)
		}
		sha256Dir = true
		return nil
	}); err != nil {
		return nil, err
	}
	found := make(map[string]struct{}, len(needed))
	if sha256Dir {
		sha256Path := filepath.Join(blobsPath, "sha256")
		sha256, err := openDirectoryAt(blobs, "sha256", ErrHandoffUnreadable)
		if err != nil {
			return nil, fmt.Errorf("open %q: %w: %w", sha256Path, ErrHandoffUnreadable, err)
		}
		if err := scanOpenDirBatched(sha256, sha256Path, ErrHandoffUnreadable, func(bf os.DirEntry) error {
			digest := export.Digest("sha256:" + bf.Name())
			if _, ok := needed[digest]; !ok || !bf.Type().IsRegular() {
				return fmt.Errorf("blob-store entry %q is unreferenced or not a regular file: %w", bf.Name(), ErrOrphanBlob)
			}
			found[bf.Name()] = struct{}{}
			return nil
		}); err != nil {
			_ = sha256.Close()
			return nil, err
		}
		for digest := range needed {
			if _, ok := found[strings.TrimPrefix(string(digest), "sha256:")]; !ok {
				_ = sha256.Close()
				return nil, fmt.Errorf("manifest stores %s but the handoff does not hold it: %w", digest, ErrMissingBlob)
			}
		}
		return sha256, nil
	}
	for digest := range needed {
		if _, ok := found[strings.TrimPrefix(string(digest), "sha256:")]; !ok {
			return nil, fmt.Errorf("manifest stores %s but the handoff does not hold it: %w", digest, ErrMissingBlob)
		}
	}
	return nil, nil
}

// dirBatch is how many entries scanDirBatched pulls per syscall: enough
// to keep enumeration cheap, small enough that a hostile directory of
// millions of names never lands in memory at once.
const dirBatch = 4096

// scanDirBatched enumerates a directory in bounded batches, calling fn
// on each entry and stopping at the first fn error. Unlike os.ReadDir it
// never reads the whole listing into memory, so it fails closed on a
// hostile directory without an unbounded allocation first. A directory
// that cannot be opened or read wraps unreadableErr.
func scanDirBatched(path string, unreadableErr error, fn func(os.DirEntry) error) error {
	d, err := openDirectory(path, unreadableErr)
	if err != nil {
		return fmt.Errorf("open %q: %w: %w", path, unreadableErr, err)
	}
	defer func() { _ = d.Close() }()
	return scanOpenDirBatched(d, path, unreadableErr, fn)
}

// scanOpenDirBatched scans a directory descriptor the caller has pinned.
// It does not close d, so an audit can use that same inode as the parent
// for a descriptor-relative child open after inspecting its entries.
func scanOpenDirBatched(d *os.File, path string, unreadableErr error, fn func(os.DirEntry) error) error {
	for {
		entries, err := d.ReadDir(dirBatch)
		for _, de := range entries {
			if ferr := fn(de); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %q: %w: %w", path, unreadableErr, err)
		}
	}
}

// snapshotVerifiedBlob opens a digest relative to the pinned, audited
// sha256 directory and copies exactly the bounded, verified stream into
// daemon-private scratch. sha1 here is git's object identity (this
// package requires the sha1 object format), not an integrity claim; the
// sha256 verified beside it is the content authority.
func snapshotVerifiedBlob(dir *os.File, dst string, digest export.Digest, size int64) (blobInfo, error) {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: daemon-private scratch path derived from a validated digest
	if err != nil {
		return blobInfo{}, fmt.Errorf("create verified snapshot for %s: %w", digest, err)
	}
	info, verifyErr := verifyBlobTo(dir, digest, size, f)
	closeErr := f.Close()
	if verifyErr != nil {
		_ = os.Remove(dst)
		return blobInfo{}, verifyErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return blobInfo{}, fmt.Errorf("close verified snapshot for %s: %w", digest, closeErr)
	}
	info.verifiedPath = dst
	return info, nil
}

// verifyBlobTo performs the blob verification stream and copies the same
// bytes to dst. The digest, git object name, and daemon-private snapshot
// therefore all derive from one bounded read of the pinned audited store.
func verifyBlobTo(dir *os.File, digest export.Digest, size int64, dst io.Writer) (blobInfo, error) {
	hexName := strings.TrimPrefix(string(digest), "sha256:")
	f, err := openRegularAt(dir, hexName, ErrOrphanBlob)
	if err != nil {
		return blobInfo{}, fmt.Errorf("open blob %s: %w: %w", digest, ErrMissingBlob, err)
	}
	defer func() { _ = f.Close() }()
	content := sha256.New()
	object := sha1.New() //nolint:gosec // G401: git object identity under the sha1 object format, not cryptographic integrity (the sha256 beside it is)
	_, _ = fmt.Fprintf(object, "blob %s\x00", strconv.FormatInt(size, 10))
	writers := []io.Writer{content, object, dst}
	// Read at most one byte past the claimed size: a hostile blob file
	// much larger than its manifest size must not stream in full before
	// the length check rejects it (an oversized read makes n exceed
	// size and fails closed). Undersized files read short and fail the
	// same check.
	n, err := io.Copy(io.MultiWriter(writers...), io.LimitReader(f, size+1))
	if err != nil {
		return blobInfo{}, fmt.Errorf("stream blob %s: %w: %w", digest, ErrHandoffUnreadable, err)
	}
	if n != size {
		return blobInfo{}, fmt.Errorf("blob %s does not hold exactly the manifest's %d bytes: %w", digest, size, ErrSizeMismatch)
	}
	if got := "sha256:" + hex.EncodeToString(content.Sum(nil)); got != string(digest) {
		return blobInfo{}, fmt.Errorf("blob content hashes to %s, manifest claims %s: %w", got, digest, ErrDigestMismatch)
	}
	return blobInfo{size: size, gitOID: hex.EncodeToString(object.Sum(nil))}, nil
}
