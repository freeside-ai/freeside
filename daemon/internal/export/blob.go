package export

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// blobWriter stores digest-addressed content blobs under
// <out>/blobs/sha256/<hex>. Content is read exactly once: the same stream
// feeds the hash and, when the blob is wanted, a temporary file renamed to
// its digest name, so the recorded digest always describes the stored
// bytes. Stored-ness is tracked in written, never read back from the
// filesystem: a pre-existing path at a digest name (a stale or corrupt
// leftover) is not this export's blob and must never satisfy a manifest
// entry.
type blobWriter struct {
	dir     string
	written map[Digest]bool
}

// newBlobWriter creates the sha256 store for one channel under
// <outDir>/<channelDir>/sha256/. channelDir is "blobs" for the repo channel and
// EvidenceBlobsDirname for the evidence channel, which keep physically separate
// stores so a digest never resolves across channels (plan §5.6).
func newBlobWriter(outDir, channelDir string) (*blobWriter, error) {
	dir := filepath.Join(outDir, channelDir, "sha256")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create blob directory: %w", err)
	}
	return &blobWriter{dir: dir, written: map[Digest]bool{}}, nil
}

// blobResult reports one digestAndStore pass: the content address, the
// bytes observed in the stream, whether the blob is present under the
// store after the call, and how many new bytes the call wrote (zero on a
// dedup hit or a skipped store; Export charges its aggregate budget with
// this, so only bytes actually landing on the exporter rootfs count).
type blobResult struct {
	digest       Digest
	size         int64
	stored       bool
	bytesWritten int64
}

// digestAndStore streams one workspace file exactly once. With store false
// (a file over the per-file cap or past the aggregate budget) it hashes
// without writing, then still reports the blob as stored if identical
// content already landed it. Identical content dedups onto one blob.
func (w *blobWriter) digestAndStore(fsys fs.FS, p string, store bool) (blobResult, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return blobResult{}, fmt.Errorf("open %q: %w", p, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if !store {
		n, err := io.Copy(h, f)
		if err != nil {
			return blobResult{}, fmt.Errorf("hash %q: %w", p, err)
		}
		d := Digest(fmt.Sprintf("sha256:%x", h.Sum(nil)))
		return blobResult{digest: d, size: n, stored: w.written[d]}, nil
	}

	tmp, err := os.CreateTemp(w.dir, ".partial-*")
	if err != nil {
		return blobResult{}, fmt.Errorf("stage blob for %q: %w", p, err)
	}
	tmpName := tmp.Name()
	n, err := io.Copy(io.MultiWriter(h, tmp), f)
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return blobResult{}, fmt.Errorf("write blob for %q: %w", p, err)
	}

	d := Digest(fmt.Sprintf("sha256:%x", h.Sum(nil)))
	if w.written[d] {
		// Already stored by an identical file this export hashed itself;
		// content addressing makes the duplicate byte-identical.
		_ = os.Remove(tmpName)
		return blobResult{digest: d, size: n, stored: true}, nil
	}
	final := filepath.Join(w.dir, strings.TrimPrefix(string(d), "sha256:"))
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return blobResult{}, fmt.Errorf("commit blob for %q: %w", p, err)
	}
	w.written[d] = true
	return blobResult{digest: d, size: n, stored: true, bytesWritten: n}, nil
}
