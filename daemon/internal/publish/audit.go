package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// MintRecord is the per-mint audit row (issue #80 acceptance 3; plan
// §8's typed-observability discipline). It deliberately has no token
// field: like the store's device_credentials shape, the secret is
// unrepresentable in the audited value, so no audit read path can leak
// it. Requested and Granted both persist — an audit trail that shows
// only what was asked for would go silently stale if GitHub ever
// narrows a grant.
type MintRecord struct {
	MintedAt       time.Time   `json:"minted_at"`
	InstallationID int64       `json:"installation_id"`
	Repo           string      `json:"repo"`
	Requested      Permissions `json:"requested"`
	Granted        Permissions `json:"granted"`
	ExpiresAt      time.Time   `json:"expires_at"`
}

// Recorder receives one record per successful mint. Minting fails when
// recording fails: an unauditable token must not circulate.
type Recorder interface {
	RecordMint(MintRecord) error
}

// StoreRecorder lands each mint on the store-owned SQLite audit
// surface (plan §5.9; issue #107). The enclosing transaction's commit
// is the durability barrier the JSONL substrate provided with fsync: a
// record that fails to commit fails the mint, so an unauditable token
// never circulates.
type StoreRecorder struct {
	store *store.Store
}

// NewStoreRecorder wires the recorder to an open store; a nil store
// fails closed at construction rather than at the first mint.
func NewStoreRecorder(s *store.Store) (*StoreRecorder, error) {
	if s == nil {
		return nil, errors.New("audit: nil store")
	}
	return &StoreRecorder{store: s}, nil
}

// RecordMint commits the record in its own internal transaction (audit
// is daemon bookkeeping, invisible to client sync). It deliberately
// runs under context.Background() rather than a caller context: the
// Recorder interface carries no context by design, and a request-scoped
// cancellation mid-commit would fail mints on a deadline that has
// nothing to do with audit durability; the local SQLite write either
// commits or the mint fails.
func (r *StoreRecorder) RecordMint(rec MintRecord) error {
	ctx := context.Background()
	err := r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordMintAudit(ctx, store.MintAudit{
			MintedAt:              rec.MintedAt,
			InstallationID:        rec.InstallationID,
			Repo:                  rec.Repo,
			RequestedContents:     rec.Requested.Contents,
			RequestedPullRequests: rec.Requested.PullRequests,
			RequestedMetadata:     rec.Requested.Metadata,
			GrantedContents:       rec.Granted.Contents,
			GrantedPullRequests:   rec.Granted.PullRequests,
			GrantedMetadata:       rec.Granted.Metadata,
			ExpiresAt:             rec.ExpiresAt,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("audit: record mint: %w", err)
	}
	return nil
}

// JSONLRecorder is the 1A audit substrate: one JSON line per mint,
// appended durably under the state directory (audit rows are not
// secret and belong in checkpoints). Plan §5.9 puts audit in SQLite
// long-term; the migration to a store-owned table is a filed
// kind:contract unit, referenced from this work unit's decision note.
type JSONLRecorder struct {
	stateDir string
	dir      string
	path     string
	stateID  fs.FileInfo
	dirID    fs.FileInfo
}

// NewJSONLRecorder creates the publish audit log at
// <stateDir>/publish/mints.jsonl.
func NewJSONLRecorder(stateDir string) (*JSONLRecorder, error) {
	if stateDir == "" {
		return nil, errors.New("audit: empty state dir")
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("audit: absolute state dir: %w", err)
	}
	// The state root is the caller's surface and must already exist:
	// if MkdirAll created it here, its own entry in the parent would
	// stay unsynced and a crash could lose the whole log despite every
	// sync below it. The recorder owns only publish/.
	stateID, err := os.Lstat(stateDir)
	if err != nil {
		return nil, fmt.Errorf("audit: state dir: %w", err)
	} else if stateID.Mode()&os.ModeSymlink != 0 || !stateID.IsDir() {
		return nil, fmt.Errorf("audit: state dir %s is not a real directory", stateDir)
	}
	// The log must live where the recorder owns it: a pre-existing
	// symlinked publish/ would relocate audit rows off the state
	// surface while mints report success, so it fails closed (the same
	// discipline as the keystore's directories).
	dir := filepath.Join(stateDir, "publish")
	if err := rejectNonDir(dir); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // 0700 is a directory mode
		return nil, fmt.Errorf("audit: narrow %s: %w", dir, err)
	}
	// Persist the publish/ entry itself: a crash could otherwise lose
	// the directory the durable log lives in.
	if err := syncDir(stateDir); err != nil {
		return nil, fmt.Errorf("audit: sync %s: %w", stateDir, err)
	}
	dirID, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("audit: lstat %s: %w", dir, err)
	}
	return &JSONLRecorder{
		stateDir: stateDir,
		dir:      dir,
		path:     filepath.Join(dir, "mints.jsonl"),
		stateID:  stateID,
		dirID:    dirID,
	}, nil
}

// RecordMint appends the record and syncs before returning, so a mint
// only proceeds once its audit row is durable.
func (r *JSONLRecorder) RecordMint(rec MintRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit: encode record: %w", err)
	}
	// Re-check both owned path components at the write boundary: either
	// could have been replaced after construction, and Lstat on the log
	// alone follows symlinked parents before it reaches the final name.
	// The residual check/open race requires a same-user attacker inside
	// the daemon's protected state tree and is the work unit's recorded
	// post-construction TOCTOU class.
	for _, dir := range []struct {
		path string
		id   fs.FileInfo
	}{{r.stateDir, r.stateID}, {r.dir, r.dirID}} {
		if err := assertSameDir(dir.path, dir.id); err != nil {
			return fmt.Errorf("audit: %w", err)
		}
	}
	// The log file itself gets the same non-symlink discipline as its
	// directory: an existing link would carry appends off the surface.
	if info, err := os.Lstat(r.path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("audit: %s is not a regular file", r.path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("audit: lstat %s: %w", r.path, err)
	}
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", r.path, err)
	}
	defer f.Close() //nolint:errcheck // Sync below is the durability barrier; the deferred close only releases the descriptor

	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("audit: append: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("audit: sync: %w", err)
	}
	// Syncing the file does not persist its directory entry, so the
	// first mint's newly created log could vanish on a crash even
	// though the token circulated; sync the parent too (cheap at mint
	// cadence, so unconditionally rather than only on creation).
	if err := syncDir(filepath.Dir(r.path)); err != nil {
		return fmt.Errorf("audit: sync dir: %w", err)
	}
	return nil
}

func assertSameDir(path string, expected fs.FileInfo) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a real directory", path)
	}
	if !os.SameFile(info, expected) {
		return fmt.Errorf("%s was replaced after recorder construction", path)
	}
	return nil
}
