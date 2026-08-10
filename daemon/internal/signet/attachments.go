package signet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
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
	if err := makeBlobStoreDirectory(dir, 0o750); err != nil {
		return nil, fmt.Errorf("blob store %q: %w", dir, err)
	}
	return &BlobStore{dir: dir}, nil
}

// makeBlobStoreDirectory re-syncs the deepest existing boundary before
// creating and parent-syncing each missing component.
func makeBlobStoreDirectory(path string, mode fs.FileMode) error {
	return makeBlobStoreDirectoryWithSync(path, mode, atomicfile.SyncDir)
}

func makeBlobStoreDirectoryWithSync(
	path string,
	mode fs.FileMode,
	syncDir func(string) error,
) error {
	var missing []string
	var existing string
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", current)
			}
			existing = current
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		if parent := filepath.Dir(current); parent == current {
			return fmt.Errorf("no existing ancestor for %s", path)
		}
	}
	if err := syncDir(filepath.Dir(existing)); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], mode); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, statErr := os.Stat(missing[i])
			if statErr != nil {
				return statErr
			}
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", missing[i])
			}
		}
		if err := syncDir(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	return nil
}

// blobPath validates the digest and derives the content path. The digest
// becomes a filename, so the accepted form is deliberately stricter than
// domain.Digest's non-empty rule: exactly "sha256:" plus 64 lowercase hex
// characters, no case folding, nothing else. Anything outside that form is
// rejected before touching the filesystem, so path construction never sees
// attacker-shaped input (separators, traversal, alternate encodings).
func (b *BlobStore) blobPath(digest domain.Digest) (string, error) {
	raw, ok := contentaddr.Parse(string(digest))
	if !ok {
		return "", fmt.Errorf("digest %q: %w", digest, ErrInvalidDigest)
	}
	return filepath.Join(b.dir, "sha256-"+raw), nil
}

// Put stores the reader's bytes under digest, verifying the content hashes to
// it. It reports whether new content was stored: false means the digest was
// already present and the upload converged on the existing immutable bytes
// (the retried-upload half of sync test 10).
func (b *BlobStore) Put(digest domain.Digest, r io.Reader) (created bool, err error) {
	return b.put(digest, r, nil)
}

// PutStageInput stores daemon-created canonical stage-input bytes through the
// same durable content-addressed boundary as attachments. A canceled caller
// never receives success; a blob completed concurrently with cancellation is
// a harmless immutable orphan until an admission references it.
func (s *Service) PutStageInput(
	ctx context.Context, digest domain.Digest, body []byte,
) error {
	if s.blobs == nil {
		return ErrAttachmentsUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.blobs.Put(digest, bytes.NewReader(body)); err != nil {
		return err
	}
	return ctx.Err()
}

func (b *BlobStore) put(
	digest domain.Digest,
	r io.Reader,
	syncDir func(string) error,
) (created bool, err error) {
	path, err := b.blobPath(digest)
	if err != nil {
		return false, err
	}

	tmp, err := atomicfile.Create(b.dir, "tmp-*")
	if err != nil {
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}
	defer func() { _ = tmp.Abort() }()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), r); err != nil {
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}
	if got := contentaddr.Format(hasher.Sum(nil)); got != string(digest) {
		return false, fmt.Errorf("attachment %q: body hashes to %q: %w", digest, got, ErrDigestMismatch)
	}

	// Convergence check after hashing: the body must prove it names the
	// stored content before the request is called converged, or a mismatched
	// re-PUT of an existing digest would return success.
	if _, err := os.Stat(path); err == nil {
		if err := b.syncDirectory(syncDir); err != nil {
			return false, fmt.Errorf("attachment %q: %w", digest, err)
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("attachment %q: %w", digest, err)
	}

	// The shared commit preserves the §5.14 finalize order, so a visible blob
	// is a durable blob.
	var commitErr error
	if syncDir == nil {
		commitErr = tmp.Commit(path)
	} else {
		commitErr = tmp.CommitWithSync(path, syncDir)
	}
	if commitErr != nil {
		return false, fmt.Errorf("attachment %q: %w", digest, commitErr)
	}
	return true, nil
}

func (b *BlobStore) syncDirectory(syncDir func(string) error) error {
	if syncDir != nil {
		return syncDir(b.dir)
	}
	return atomicfile.SyncDir(b.dir)
}

// Open returns a reader over the stored bytes; the caller closes it.
func (b *BlobStore) Open(digest domain.Digest) (io.ReadCloser, error) {
	return b.OpenContext(context.Background(), digest)
}

// OpenContext returns a reader over the stored bytes unless lookup was
// canceled. The post-open check closes a file obtained concurrently with
// cancellation rather than handing it to a caller whose operation has ended.
func (b *BlobStore) OpenContext(
	ctx context.Context, digest domain.Digest,
) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, f.Close())
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

// Verify reports whether the stored bytes exist and hash to digest. Backup
// closure uses this stronger check: a digest-named path is not evidence that
// the bytes survived intact.
func (b *BlobStore) Verify(digest domain.Digest) (bool, error) {
	body, err := b.Open(digest)
	if errors.Is(err, ErrBlobNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, body)
	closeErr := body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return false, fmt.Errorf("attachment %q: verify: %w", digest, err)
	}
	got := contentaddr.Format(hasher.Sum(nil))
	return got == string(digest), nil
}

// hasAttachment is the service-side gate: with no blob store composed,
// attachment references fail closed rather than passing unverified.
func (s *Service) hasAttachment(digest domain.Digest) (bool, error) {
	if s.blobs == nil {
		return false, ErrAttachmentsUnavailable
	}
	return s.blobs.Has(digest)
}
