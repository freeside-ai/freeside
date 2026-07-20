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

// verifyBlobs audits both handoff blob stores against their manifests and
// verifies every stored blob's content. The audit is exact in both directions
// per channel: every digest a manifest stores must exist as a blob file, and
// nothing else may exist anywhere in the handoff directory — the exporter writes
// exactly manifest.json (plus evidence.json when the evidence channel is
// present) and the blobs those manifests reference, so a stray path (including a
// blob for an entry marked blob_omitted) is hostile, not noise. The two channels
// keep physically separate stores (blobs/ and evidence/, plan §5.6), so an
// evidence digest never resolves through a repo blob or vice versa. Each stored
// blob then streams once through sha256 (binding content to the manifest digest)
// and the git blob object derivation that change derivation and commit
// construction rely on later. The same stream writes a daemon-private snapshot
// into a per-channel scratch subdir, so no later stage re-resolves a handoff
// pathname and a shared digest cannot collide across channels.
func verifyBlobs(handoffDir, scratch string, m export.Manifest, em export.EvidenceManifest, emPresent bool, pol Policy) (repo, evidence map[export.Digest]blobInfo, err error) {
	repoNeeded := make(map[export.Digest]int64, len(m.Entries))
	var repoTotal int64
	for _, e := range m.Entries {
		if e.Kind != export.EntryRegular || e.BlobOmitted {
			continue
		}
		if err := accumulateNeeded(repoNeeded, &repoTotal, *e.Digest, *e.Size, pol.MaxBlobBytes, pol.MaxTotalBytes); err != nil {
			return nil, nil, err
		}
	}
	// Every evidence entry has a mandatory blob (the evidence schema has no
	// blob_omitted escape), so each contributes. emPresent need not be checked
	// here: an absent channel decodes to a zero manifest with no entries.
	evidenceNeeded := make(map[export.Digest]int64, len(em.Entries))
	var evidenceTotal int64
	for _, e := range em.Entries {
		if err := accumulateNeeded(evidenceNeeded, &evidenceTotal, e.Digest, e.Size, pol.MaxEvidenceBlobBytes, pol.MaxEvidenceTotalBytes); err != nil {
			return nil, nil, err
		}
	}
	repoSha256, evidenceSha256, err := auditHandoffLayout(handoffDir, repoNeeded, evidenceNeeded, emPresent)
	if err != nil {
		return nil, nil, err
	}
	if repoSha256 != nil {
		defer func() { _ = repoSha256.Close() }()
	}
	if evidenceSha256 != nil {
		defer func() { _ = evidenceSha256.Close() }()
	}
	repo, err = snapshotChannel(scratch, "repo", repoSha256, repoNeeded)
	if err != nil {
		return nil, nil, err
	}
	evidence, err = snapshotChannel(scratch, "evidence", evidenceSha256, evidenceNeeded)
	if err != nil {
		return nil, nil, err
	}
	return repo, evidence, nil
}

// accumulateNeeded records one stored blob into needed, enforcing the per-blob
// and running-total size caps before any byte is read. verifyBlobTo reads a
// whole file to bind its digest, so a forged manifest declaring a huge stored
// blob (or many summing past the total) would otherwise burn that much disk I/O
// at the untrusted boundary; an honest exporter never stores an over-cap blob,
// so an over-cap declaration is contract-impossible and fails closed. A repeated
// digest is counted once (dedup is free and bounds bytes actually read); a
// repeat at a different size is a forged manifest.
func accumulateNeeded(needed map[export.Digest]int64, storedTotal *int64, digest export.Digest, size, maxBlob, maxTotal int64) error {
	if maxBlob > 0 && size > maxBlob {
		return fmt.Errorf("stored blob %s declares %d bytes, over the %d-byte cap: %w", digest, size, maxBlob, ErrBlobTooLarge)
	}
	if prev, ok := needed[digest]; ok {
		if prev != size {
			return fmt.Errorf("digest %s claimed at sizes %d and %d: %w", digest, prev, size, ErrSizeMismatch)
		}
		return nil
	}
	// Overflow-safe: storedTotal never exceeds maxTotal, so the subtraction
	// cannot wrap.
	if maxTotal > 0 && size > maxTotal-*storedTotal {
		return fmt.Errorf("stored blobs exceed the %d-byte total cap: %w", maxTotal, ErrBlobTooLarge)
	}
	*storedTotal += size
	needed[digest] = size
	return nil
}

// snapshotChannel verifies and snapshots every needed blob for one channel into
// a private scratch subdir named for the channel. Separate subdirs keep the
// channels' verified content physically apart even when one content digest
// appears in both, so neither channel's snapshot can be read as the other's.
func snapshotChannel(scratch, name string, sha256Dir *os.File, needed map[export.Digest]int64) (map[export.Digest]blobInfo, error) {
	blobs := make(map[export.Digest]blobInfo, len(needed))
	if len(needed) == 0 {
		return blobs, nil
	}
	if sha256Dir == nil {
		// auditHandoffLayout returns a nil directory only for a channel that
		// stored nothing; a non-empty needed set with no store is already
		// ErrMissingBlob there, so this is an internal invariant guard.
		for digest := range needed {
			return nil, fmt.Errorf("channel %s stores %s but has no blob directory: %w", name, digest, ErrMissingBlob)
		}
	}
	verifiedDir := filepath.Join(scratch, "verified-blobs", name)
	if err := os.MkdirAll(verifiedDir, 0o700); err != nil {
		return nil, fmt.Errorf("create verified-blob scratch for %s: %w", name, err)
	}
	for digest, size := range needed {
		verifiedPath := filepath.Join(verifiedDir, strings.TrimPrefix(string(digest), "sha256:"))
		info, err := snapshotVerifiedBlob(sha256Dir, verifiedPath, digest, size)
		if err != nil {
			return nil, err
		}
		blobs[digest] = info
	}
	return blobs, nil
}

// auditHandoffLayout enforces the exact handoff shape in a single root scan and
// returns each channel's pinned sha256 directory for content verification. The
// root holds manifest.json and at most blobs/, evidence.json, and evidence/;
// every other entry is an orphan. The root is scanned once (a second pass per
// channel would reject the other channel's files as orphans), then each blob
// store (blobs/, evidence/) is audited by the shared auditBlobStore: it holds at
// most a sha256/ directory holding exactly that channel's needed digests as
// regular files. Every level is enumerated in bounded batches and aborts on the
// first unexpected entry, so a hostile handoff that stuffs a directory with
// millions of names cannot force the whole listing into memory before the audit
// rejects it. A returned directory is non-nil only for a channel that stored
// blobs; the caller closes each after verification. When the evidence channel is
// absent (evidencePresent false, i.e. no evidence.json), evidence.json and
// evidence/ are NOT permitted at the root: a stale or planted second-channel
// entry is an orphan, so an absent evidence channel is exactly the pre-evidence
// layout.
func auditHandoffLayout(dir string, repoNeeded, evidenceNeeded map[export.Digest]int64, evidencePresent bool) (repoSha256, evidenceSha256 *os.File, err error) {
	root, err := openDirectory(dir, ErrHandoffUnreadable)
	if err != nil {
		return nil, nil, fmt.Errorf("open %q: %w: %w", dir, ErrHandoffUnreadable, err)
	}
	defer func() { _ = root.Close() }()
	blobsDir, evidenceDir := false, false
	if err := scanOpenDirBatched(root, dir, ErrHandoffUnreadable, func(de os.DirEntry) error {
		switch {
		case de.Name() == export.ManifestFilename && de.Type().IsRegular():
			return nil
		case de.Name() == "blobs" && de.IsDir():
			blobsDir = true
			return nil
		case evidencePresent && de.Name() == export.EvidenceFilename && de.Type().IsRegular():
			return nil
		case evidencePresent && de.Name() == export.EvidenceBlobsDirname && de.IsDir():
			evidenceDir = true
			return nil
		default:
			return fmt.Errorf("unexpected handoff entry %q: %w", de.Name(), ErrOrphanBlob)
		}
	}); err != nil {
		return nil, nil, err
	}
	repoSha256, err = auditBlobStore(root, dir, "blobs", blobsDir, repoNeeded)
	if err != nil {
		return nil, nil, err
	}
	evidenceSha256, err = auditBlobStore(root, dir, export.EvidenceBlobsDirname, evidenceDir, evidenceNeeded)
	if err != nil {
		if repoSha256 != nil {
			_ = repoSha256.Close()
		}
		return nil, nil, err
	}
	return repoSha256, evidenceSha256, nil
}

// auditBlobStore enforces one channel's blob-store shape under the pinned
// handoff root and returns its open sha256 directory. present reports whether
// the store's top directory (storeName) was seen at the root scan. The store
// holds at most a sha256/ directory, which holds exactly the needed digests as
// regular files: a stray, non-regular, or unreferenced entry is an orphan, a
// missing needed digest is ErrMissingBlob, and an absent store with a non-empty
// needed set is ErrMissingBlob. Both channels share this routine, so neither can
// hold content the other's manifest references.
func auditBlobStore(root *os.File, dir, storeName string, present bool, needed map[export.Digest]int64) (*os.File, error) {
	if !present {
		if len(needed) > 0 {
			return nil, fmt.Errorf("manifest references blobs but the handoff has no %s store: %w", storeName, ErrMissingBlob)
		}
		return nil, nil
	}
	storePath := filepath.Join(dir, storeName)
	store, err := openDirectoryAt(root, storeName, ErrHandoffUnreadable)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w: %w", storePath, ErrHandoffUnreadable, err)
	}
	defer func() { _ = store.Close() }()
	sha256Present := false
	if err := scanOpenDirBatched(store, storePath, ErrHandoffUnreadable, func(de os.DirEntry) error {
		if de.Name() != "sha256" || !de.IsDir() {
			return fmt.Errorf("unexpected %s-store entry %q: %w", storeName, de.Name(), ErrOrphanBlob)
		}
		sha256Present = true
		return nil
	}); err != nil {
		return nil, err
	}
	if !sha256Present {
		// An empty store with nothing needed is benign; a needed digest with no
		// sha256 directory is missing.
		for digest := range needed {
			return nil, fmt.Errorf("%s manifest stores %s but the handoff does not hold it: %w", storeName, digest, ErrMissingBlob)
		}
		return nil, nil
	}
	sha256Path := filepath.Join(storePath, "sha256")
	sha256, err := openDirectoryAt(store, "sha256", ErrHandoffUnreadable)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w: %w", sha256Path, ErrHandoffUnreadable, err)
	}
	found := make(map[string]struct{}, len(needed))
	if err := scanOpenDirBatched(sha256, sha256Path, ErrHandoffUnreadable, func(bf os.DirEntry) error {
		digest := export.Digest("sha256:" + bf.Name())
		if _, ok := needed[digest]; !ok || !bf.Type().IsRegular() {
			return fmt.Errorf("%s-store entry %q is unreferenced or not a regular file: %w", storeName, bf.Name(), ErrOrphanBlob)
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
			return nil, fmt.Errorf("%s manifest stores %s but the handoff does not hold it: %w", storeName, digest, ErrMissingBlob)
		}
	}
	return sha256, nil
}

// dirBatch is how many entries scanOpenDirBatched pulls per syscall:
// enough to keep enumeration cheap, small enough that a hostile
// directory of millions of names never lands in memory at once.
const dirBatch = 4096

// scanOpenDirBatched enumerates a directory descriptor the caller has
// pinned in bounded batches, calling fn on each entry and stopping at
// the first fn error. Unlike os.ReadDir it never reads the whole listing
// into memory, so it fails closed on a hostile directory without an
// unbounded allocation first; a directory that cannot be read wraps
// unreadableErr. It does not close d, so an audit can use that same
// inode as the parent for a descriptor-relative child open after
// inspecting its entries.
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
