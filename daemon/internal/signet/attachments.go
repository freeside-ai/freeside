package signet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// BlobStore is the digest-addressed attachment store (plan §5.14: "text in
// SQLite; attachments in the artifact store by digest"). Content is immutable
// per digest: a re-PUT of a stored digest converges on the existing bytes
// (sync test 10), and a mismatch between path digest and body is rejected.
// Bytes are opaque to the daemon (§5.15 rule 3); rendering happens in
// clients.
//
// Durability contract (§5.14 agent completion: "finalize and fsync blobs"
// before the SQLite transaction): Put streams to a temp file, fsyncs it,
// renames it into place, and fsyncs the directory, so a blob whose Put
// returned is durable before any row referencing it commits. A crash
// mid-upload leaves only a temp file; a completed upload whose referencing
// transaction never commits leaves a harmless orphan blob.
type BlobStore struct {
	dir string
}

// ErrDigestMismatch is returned when an uploaded body does not hash to the
// digest naming it.
var ErrDigestMismatch = errors.New("attachment body does not hash to the path digest")

// ErrInvalidDigest is returned for a digest outside the strict form the store
// accepts as a filename.
var ErrInvalidDigest = errors.New("attachment digest is not sha256:<64 lowercase hex>")

// ErrBlobNotFound is returned when no stored content carries the digest.
var ErrBlobNotFound = errors.New("no attachment stored under the digest")

// NewBlobStore opens (creating if needed) the attachment directory.
func NewBlobStore(dir string) (*BlobStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("blob store %q: %w", dir, err)
	}
	return &BlobStore{dir: dir}, nil
}

// blobPath validates the digest and derives the content path. The digest
// becomes a filename, so the accepted form is deliberately stricter than
// domain.Digest's non-empty rule: exactly "sha256:" plus 64 lowercase hex
// characters, no case folding, nothing else. Anything outside that form is
// rejected before touching the filesystem, so path construction never sees
// attacker-shaped input (separators, traversal, alternate encodings).
func (b *BlobStore) blobPath(digest domain.Digest) (string, error) {
	raw, ok := strings.CutPrefix(string(digest), "sha256:")
	if !ok || len(raw) != 64 {
		return "", fmt.Errorf("digest %q: %w", digest, ErrInvalidDigest)
	}
	for _, c := range raw {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("digest %q: %w", digest, ErrInvalidDigest)
		}
	}
	return filepath.Join(b.dir, "sha256-"+raw), nil
}

// Put stores the reader's bytes under digest, verifying the content hashes to
// it. It reports whether new content was stored: false means the digest was
// already present and the upload converged on the existing immutable bytes
// (the retried-upload half of sync test 10).
func (b *BlobStore) Put(digest domain.Digest, r io.Reader) (created bool, err error) {
	path, err := b.blobPath(digest)
	if err != nil {
		return false, err
	}

	tmp, err := os.CreateTemp(b.dir, "tmp-*")
	if err != nil {
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), r); err != nil {
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}
	if got := "sha256:" + hex.EncodeToString(hasher.Sum(nil)); got != string(digest) {
		return false, fmt.Errorf("attachment %q: body hashes to %q: %w", digest, got, ErrDigestMismatch)
	}

	// Convergence check after hashing: the body must prove it names the
	// stored content before the request is called converged, or a mismatched
	// re-PUT of an existing digest would return success.
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}

	// fsync file, rename into place, fsync directory: the §5.14 finalize
	// order, so a visible blob is a durable blob.
	if err := tmp.Sync(); err != nil {
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}
	if err := b.syncDir(); err != nil {
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}
	return true, nil
}

// Open returns a reader over the stored bytes; the caller closes it.
func (b *BlobStore) Open(digest domain.Digest) (io.ReadCloser, error) {
	path, err := b.blobPath(digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // path derives from the strict digest form blobPath enforces
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("attachment %q: %w", digest, ErrBlobNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("attachment %q: %w", digest, err)
	}
	return f, nil
}

// Has reports whether content is stored under the digest.
func (b *BlobStore) Has(digest domain.Digest) (bool, error) {
	path, err := b.blobPath(digest)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}
	return true, nil
}

func (b *BlobStore) syncDir() error {
	d, err := os.Open(b.dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// hasAttachment is the service-side gate: with no blob store composed,
// attachment references fail closed rather than passing unverified.
func (s *Service) hasAttachment(digest domain.Digest) (bool, error) {
	if s.blobs == nil {
		return false, ErrAttachmentsUnavailable
	}
	return s.blobs.Has(digest)
}
