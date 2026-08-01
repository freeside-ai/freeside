package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"modernc.org/sqlite"
)

const (
	backupCheckpointEnvelopeVersion = 1
	backupEncryptionKeySize         = 32
	backupEncryptionKeySuffix       = ".backup-encryption.key"
	encryptedCheckpointFilename     = "latest.backup"
	legacyCheckpointFilename        = "latest.db"
	legacyRestoreTestFilename       = "restore-test.db"
)

var (
	errBackupKeyPermissions       = errors.New("backup encryption key is not a private (0600) regular file")
	errBackupKeyMalformed         = errors.New("backup encryption key is corrupt")
	errBackupKeyMissing           = errors.New("backup encryption key is absent for an encrypted checkpoint")
	errCheckpointManifestMismatch = errors.New("checkpoint artifact manifest mismatch")
)

type encryptedCheckpointEnvelope struct {
	Version    int           `json:"version"`
	KeyID      domain.Digest `json:"key_id"`
	Nonce      []byte        `json:"nonce"`
	Ciphertext []byte        `json:"ciphertext"`
}

type encryptedCheckpointAAD struct {
	Version int           `json:"version"`
	KeyID   domain.Digest `json:"key_id"`
}

type encryptedCheckpointHealthSource struct {
	files             *LocalBackupFiles
	artifacts         BackupArtifactStore
	approvedRecipes   map[domain.Digest]bool
	payloadExtractors map[string]BackupPayloadDigestExtractor
	checkpointMaxAge  time.Duration
	restoreTestMaxAge time.Duration
	now               func() time.Time
}

func newEncryptedCheckpointHealthSource(
	opts LocalCheckpointHealthOptions, files *LocalBackupFiles,
) (BackupHealthSource, error) {
	if opts.CheckpointMaxAge == 0 {
		opts.CheckpointMaxAge = DefaultLocalCheckpointMaxAge
	}
	if opts.RestoreTestMaxAge == 0 {
		opts.RestoreTestMaxAge = DefaultLocalRestoreTestMaxAge
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	switch {
	case files == nil:
		return nil, errors.New("encrypted checkpoint health: nil backup files")
	case files.checkpointPath == "":
		return nil, errors.New("encrypted checkpoint health: empty checkpoint path")
	case len(files.encryptionKey) != backupEncryptionKeySize:
		return nil, errors.New("encrypted checkpoint health: invalid encryption key")
	case opts.Artifacts == nil:
		return nil, errors.New("encrypted checkpoint health: nil artifact store")
	case opts.CheckpointMaxAge < 0:
		return nil, errors.New("encrypted checkpoint health: negative checkpoint max age")
	case opts.RestoreTestMaxAge < 0:
		return nil, errors.New("encrypted checkpoint health: negative restore-test max age")
	}
	return &encryptedCheckpointHealthSource{
		files:             files,
		artifacts:         opts.Artifacts,
		approvedRecipes:   maps.Clone(opts.ApprovedRecipes),
		payloadExtractors: maps.Clone(opts.PayloadExtractors),
		checkpointMaxAge:  opts.CheckpointMaxAge,
		restoreTestMaxAge: opts.RestoreTestMaxAge,
		now:               opts.Now,
	}, nil
}

func (s *encryptedCheckpointHealthSource) BackupHealth(
	ctx context.Context, current BackupHealthContext,
) (domain.BackupHealth, error) {
	s.files.mu.RLock()
	defer s.files.mu.RUnlock()

	health := unhealthyBackupHealth()
	snapshot, checkpoint, found, err := inspectEncryptedCheckpoint(
		ctx,
		s.files,
		true,
		s.approvedRecipes,
		s.payloadExtractors,
	)
	if err != nil {
		if errors.Is(err, domain.ErrCheckpointAuthentication) ||
			errors.Is(err, domain.ErrCheckpointDigestMismatch) {
			return health, nil
		}
		return domain.BackupHealth{}, fmt.Errorf("encrypted checkpoint health: %w", err)
	}
	if !found {
		return health, nil
	}
	health.Encryption = domain.BackupHealthHealthy

	now := s.now()
	checkpointAge := now.Sub(checkpoint.CompletedAt)
	if snapshot.schemaVersion == current.SchemaVersion &&
		checkpoint.SyncEpoch == current.SyncEpoch &&
		checkpointAge >= 0 && checkpointAge <= s.checkpointMaxAge {
		health.CheckpointCurrency = domain.BackupHealthHealthy
	}

	// The inspection above already refused a checkpoint whose own closure this
	// binary cannot compute, so only the live half remains: a durable row the
	// last maintenance pass could not read means no checkpoint can be proven
	// from here on, whatever the one on disk still proves.
	closed := !s.files.liveClosureGap.Load()
	for _, digest := range snapshot.digests {
		verified, verifyErr := s.artifacts.Verify(digest)
		if verifyErr != nil {
			return domain.BackupHealth{}, fmt.Errorf(
				"encrypted checkpoint health: artifact %s: %w", digest, verifyErr)
		}
		if !verified {
			closed = false
		}
	}
	if closed {
		health.ArtifactClosure = domain.BackupHealthHealthy
	}

	if checkpoint.RestoreTestedAt != nil {
		restoreAge := now.Sub(*checkpoint.RestoreTestedAt)
		if restoreAge >= 0 && restoreAge <= s.restoreTestMaxAge {
			health.RestoreTestAge = domain.BackupHealthHealthy
		}
	}
	return health, nil
}

// RestoreCheckpoint verifies and decrypts the completed checkpoint, proves its
// artifact closure, then restores its rows through Store.Restore. The returned
// checkpoint preserves the captured epoch and revision; the returned server
// state has the same revision and a fresh epoch, as rollback safety requires.
func (f *LocalBackupFiles) RestoreCheckpoint(
	ctx context.Context, target *Store,
) (domain.BackupCheckpoint, ServerState, error) {
	switch {
	case f == nil:
		return domain.BackupCheckpoint{}, ServerState{},
			errors.New("restore encrypted checkpoint: nil backup files")
	case target == nil:
		return domain.BackupCheckpoint{}, ServerState{},
			errors.New("restore encrypted checkpoint: nil target store")
	case f.artifacts == nil:
		return domain.BackupCheckpoint{}, ServerState{},
			errors.New("restore encrypted checkpoint: artifact store is not configured")
	}

	f.mu.RLock()
	defer f.mu.RUnlock()

	plaintext, checkpoint, err := openEncryptedCheckpoint(f.checkpointPath, f.encryptionKey)
	if err != nil {
		return domain.BackupCheckpoint{}, ServerState{},
			fmt.Errorf("restore encrypted checkpoint: %w", err)
	}
	sourceDB, source, err := openDeserializedBackupDatabase(ctx, plaintext)
	if err != nil {
		return domain.BackupCheckpoint{}, ServerState{}, err
	}
	defer closeDeserializedBackupDatabase(sourceDB, source)

	snapshot, err := inspectBackupDB(
		ctx,
		source,
		digestBytes(plaintext),
		true,
		f.approvedRecipes,
		f.payloadExtractors,
	)
	if err != nil {
		return domain.BackupCheckpoint{}, ServerState{},
			fmt.Errorf("restore encrypted checkpoint: inspect: %w", err)
	}
	// Restore proves the closure before it overwrites a store, so a checkpoint
	// this binary cannot fully scan fails closed here even though the daemon
	// tolerates the same gap while running: an older binary restoring a newer
	// daemon's checkpoint would admit rows whose blobs it never checked.
	if snapshot.closureGap != nil {
		return domain.BackupCheckpoint{}, ServerState{},
			fmt.Errorf("restore encrypted checkpoint: %w: %w",
				ErrBackupClosureIncomplete, snapshot.closureGap)
	}
	if snapshot.fileDigest != checkpoint.SQLiteSnapshotDigest ||
		snapshot.state.SyncEpoch != checkpoint.SyncEpoch ||
		snapshot.state.Revision != checkpoint.ServerRevision ||
		artifactManifestDigest(snapshot.digests) != checkpoint.ArtifactManifestDigest {
		return domain.BackupCheckpoint{}, ServerState{},
			fmt.Errorf("restore encrypted checkpoint: %w", domain.ErrCheckpointDigestMismatch)
	}
	for _, digest := range snapshot.digests {
		verified, verifyErr := f.artifacts.Verify(digest)
		if verifyErr != nil {
			return domain.BackupCheckpoint{}, ServerState{},
				fmt.Errorf("restore encrypted checkpoint: artifact %s: %w", digest, verifyErr)
		}
		if !verified {
			return domain.BackupCheckpoint{}, ServerState{},
				fmt.Errorf("restore encrypted checkpoint: artifact %s: %w",
					digest, domain.ErrArtifactClosureIncomplete)
		}
	}
	state, err := target.restoreFromDatabase(ctx, source)
	if err != nil {
		return domain.BackupCheckpoint{}, ServerState{},
			fmt.Errorf("restore encrypted checkpoint: %w", err)
	}
	return checkpoint, state, nil
}

func inspectEncryptedCheckpoint(
	ctx context.Context,
	files *LocalBackupFiles,
	collectDigests bool,
	approvedRecipes map[domain.Digest]bool,
	payloadExtractors map[string]BackupPayloadDigestExtractor,
) (backupDatabaseSnapshot, domain.BackupCheckpoint, bool, error) {
	info, err := os.Lstat(files.checkpointPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return backupDatabaseSnapshot{}, domain.BackupCheckpoint{}, false, nil
	case err != nil:
		return backupDatabaseSnapshot{}, domain.BackupCheckpoint{}, false,
			fmt.Errorf("stat %s: %w", files.checkpointPath, err)
	case !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0:
		return backupDatabaseSnapshot{}, domain.BackupCheckpoint{}, false, nil
	}

	plaintext, checkpoint, err := openEncryptedCheckpoint(files.checkpointPath, files.encryptionKey)
	if err != nil {
		return backupDatabaseSnapshot{}, domain.BackupCheckpoint{}, false, err
	}
	db, conn, err := openDeserializedBackupDatabase(ctx, plaintext)
	if err != nil {
		return backupDatabaseSnapshot{}, domain.BackupCheckpoint{}, false, err
	}
	defer closeDeserializedBackupDatabase(db, conn)

	snapshot, err := inspectBackupDB(
		ctx,
		conn,
		digestBytes(plaintext),
		collectDigests,
		approvedRecipes,
		payloadExtractors,
	)
	if err != nil {
		return backupDatabaseSnapshot{}, domain.BackupCheckpoint{}, false, err
	}
	if snapshot.fileDigest != checkpoint.SQLiteSnapshotDigest ||
		snapshot.state.SyncEpoch != checkpoint.SyncEpoch ||
		snapshot.state.Revision != checkpoint.ServerRevision {
		return backupDatabaseSnapshot{}, domain.BackupCheckpoint{}, false,
			domain.ErrCheckpointDigestMismatch
	}
	if collectDigests && snapshot.closureGap != nil {
		// The manifest binds a closure this binary cannot recompute, so it can
		// neither be confirmed nor refuted: the omitted rows may contribute no
		// digest the rest of the scan lacks, making equality prove nothing.
		// Unverifiable is unusable, one step before the comparison.
		return backupDatabaseSnapshot{}, domain.BackupCheckpoint{}, false,
			fmt.Errorf("%w: %w: %w",
				domain.ErrCheckpointDigestMismatch, ErrBackupClosureIncomplete, snapshot.closureGap)
	}
	if collectDigests && artifactManifestDigest(snapshot.digests) != checkpoint.ArtifactManifestDigest {
		return backupDatabaseSnapshot{}, domain.BackupCheckpoint{}, false,
			fmt.Errorf("%w: %w",
				domain.ErrCheckpointDigestMismatch, errCheckpointManifestMismatch)
	}
	snapshot.generatedAt = checkpoint.CompletedAt
	if checkpoint.RestoreTestedAt != nil {
		snapshot.restoredAt = *checkpoint.RestoreTestedAt
	}
	return snapshot, checkpoint, true, nil
}

func sealEncryptedCheckpoint(
	plaintext []byte, checkpoint domain.BackupCheckpoint, key []byte,
) ([]byte, error) {
	if err := checkpoint.Validate(); err != nil {
		return nil, fmt.Errorf("seal checkpoint: %w", err)
	}
	aead, err := backupAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal checkpoint: generate nonce: %w", err)
	}
	keyID := backupKeyID(key)
	aad, err := json.Marshal(encryptedCheckpointAAD{
		Version: backupCheckpointEnvelopeVersion, KeyID: keyID,
	})
	if err != nil {
		return nil, fmt.Errorf("seal checkpoint: encode authenticated metadata: %w", err)
	}
	payload, err := encodeEncryptedCheckpointPayload(checkpoint, plaintext)
	if err != nil {
		return nil, err
	}
	envelope := encryptedCheckpointEnvelope{
		Version: backupCheckpointEnvelopeVersion,
		KeyID:   keyID,
		Nonce:   nonce,
		Ciphertext: aead.Seal(
			nil, nonce, payload, aad),
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("seal checkpoint: encode envelope: %w", err)
	}
	return append(body, '\n'), nil
}

func openEncryptedCheckpoint(
	path string, key []byte,
) ([]byte, domain.BackupCheckpoint, error) {
	file, err := os.OpenFile( //nolint:gosec // caller-owned path, opened without following links
		path,
		os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK,
		0,
	)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, domain.BackupCheckpoint{}, domain.ErrCheckpointAuthentication
		}
		return nil, domain.BackupCheckpoint{}, fmt.Errorf("open encrypted checkpoint: %w", err)
	}
	defer file.Close() //nolint:errcheck // read/authentication is the useful signal
	info, err := file.Stat()
	if err != nil {
		return nil, domain.BackupCheckpoint{}, fmt.Errorf("stat encrypted checkpoint: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, domain.BackupCheckpoint{}, domain.ErrCheckpointAuthentication
	}
	body, err := io.ReadAll(file)
	if err != nil {
		return nil, domain.BackupCheckpoint{}, fmt.Errorf("read encrypted checkpoint: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope encryptedCheckpointEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return nil, domain.BackupCheckpoint{},
			fmt.Errorf("%w: decode envelope", domain.ErrCheckpointAuthentication)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, domain.BackupCheckpoint{}, err
	}
	if envelope.Version != backupCheckpointEnvelopeVersion ||
		envelope.KeyID != backupKeyID(key) {
		return nil, domain.BackupCheckpoint{}, domain.ErrCheckpointAuthentication
	}
	aead, err := backupAEAD(key)
	if err != nil {
		return nil, domain.BackupCheckpoint{}, err
	}
	if len(envelope.Nonce) != aead.NonceSize() {
		return nil, domain.BackupCheckpoint{}, domain.ErrCheckpointAuthentication
	}
	aad, err := json.Marshal(encryptedCheckpointAAD{
		Version: envelope.Version, KeyID: envelope.KeyID,
	})
	if err != nil {
		return nil, domain.BackupCheckpoint{}, err
	}
	payload, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, aad)
	if err != nil {
		return nil, domain.BackupCheckpoint{}, domain.ErrCheckpointAuthentication
	}
	checkpoint, plaintext, err := decodeEncryptedCheckpointPayload(payload)
	if err != nil {
		return nil, domain.BackupCheckpoint{}, err
	}
	if digestBytes(plaintext) != checkpoint.SQLiteSnapshotDigest {
		return nil, domain.BackupCheckpoint{}, domain.ErrCheckpointDigestMismatch
	}
	return plaintext, checkpoint, nil
}

func encodeEncryptedCheckpointPayload(
	checkpoint domain.BackupCheckpoint, plaintext []byte,
) ([]byte, error) {
	metadata, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, fmt.Errorf("seal checkpoint: encode metadata: %w", err)
	}
	payload := make([]byte, 8+len(metadata)+len(plaintext))
	binary.BigEndian.PutUint64(payload[:8], uint64(len(metadata)))
	copy(payload[8:], metadata)
	copy(payload[8+len(metadata):], plaintext)
	return payload, nil
}

func decodeEncryptedCheckpointPayload(
	payload []byte,
) (domain.BackupCheckpoint, []byte, error) {
	if len(payload) < 8 {
		return domain.BackupCheckpoint{}, nil, domain.ErrCheckpointAuthentication
	}
	metadataSize := binary.BigEndian.Uint64(payload[:8])
	if metadataSize > uint64(len(payload)-8) { //nolint:gosec // len is non-negative, so the widening conversion is safe.
		return domain.BackupCheckpoint{}, nil, domain.ErrCheckpointAuthentication
	}
	metadataEnd := 8 + int(metadataSize) //nolint:gosec // the bound above proves metadataSize fits in int.
	decoder := json.NewDecoder(bytes.NewReader(payload[8:metadataEnd]))
	decoder.DisallowUnknownFields()
	var checkpoint domain.BackupCheckpoint
	if err := decoder.Decode(&checkpoint); err != nil {
		return domain.BackupCheckpoint{}, nil,
			fmt.Errorf("%w: decode metadata", domain.ErrCheckpointAuthentication)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return domain.BackupCheckpoint{}, nil, err
	}
	if err := checkpoint.Validate(); err != nil {
		return domain.BackupCheckpoint{}, nil,
			fmt.Errorf("%w: invalid metadata", domain.ErrCheckpointAuthentication)
	}
	return checkpoint, payload[metadataEnd:], nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing envelope content", domain.ErrCheckpointAuthentication)
	}
	return nil
}

func backupAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != backupEncryptionKeySize {
		return nil, fmt.Errorf("backup encryption key is %d bytes, want %d",
			len(key), backupEncryptionKeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create backup cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create backup AEAD: %w", err)
	}
	return aead, nil
}

func backupKeyID(key []byte) domain.Digest {
	sum := sha256.Sum256(key)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func digestBytes(body []byte) domain.Digest {
	sum := sha256.Sum256(body)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func artifactManifestDigest(digests []domain.Digest) domain.Digest {
	canonical := slices.Clone(digests)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i] < canonical[j] })
	canonical = slices.Compact(canonical)
	hash := sha256.New()
	_, _ = io.WriteString(hash, "freeside-backup-artifact-manifest-v1\n")
	for _, digest := range canonical {
		_, _ = io.WriteString(hash, string(digest))
		_, _ = io.WriteString(hash, "\n")
	}
	return domain.Digest(fmt.Sprintf("sha256:%x", hash.Sum(nil)))
}

type sqliteDeserializer interface {
	Deserialize([]byte) error
}

type sqliteSerializer interface {
	Serialize() ([]byte, error)
}

type sqliteBackupper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

func serializeStoreCheckpoint(ctx context.Context, store *Store) ([]byte, error) {
	conn, err := store.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: connect: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var body []byte
	if err := conn.Raw(func(driverConn any) error {
		backupper, ok := driverConn.(sqliteBackupper)
		if !ok {
			return errors.New("sqlite driver does not support online backup")
		}
		backup, err := backupper.NewBackup(":memory:")
		if err != nil {
			return fmt.Errorf("start in-memory backup: %w", err)
		}
		more, err := backup.Step(-1)
		if err != nil {
			return errors.Join(
				fmt.Errorf("copy in-memory backup: %w", err),
				backup.Finish(),
			)
		}
		if more {
			return errors.Join(
				errors.New("copy in-memory backup: incomplete after full step"),
				backup.Finish(),
			)
		}
		destination, err := backup.Commit()
		if err != nil {
			return fmt.Errorf("commit in-memory backup: %w", err)
		}
		body, err = serializeBackupDriverConnection(destination)
		return errors.Join(err, destination.Close())
	}); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	return body, nil
}

func serializeBackupDriverConnection(conn driver.Conn) ([]byte, error) {
	serializer, ok := conn.(sqliteSerializer)
	if !ok {
		return nil, errors.New("sqlite backup destination does not support Serialize")
	}
	body, err := serializer.Serialize()
	if err != nil {
		return nil, fmt.Errorf("serialize in-memory backup: %w", err)
	}
	if len(body) < 100 || !bytes.Equal(body[:16], []byte("SQLite format 3\x00")) {
		return nil, errors.New("serialize in-memory backup: invalid SQLite header")
	}
	// The online backup contains all committed WAL pages, but SQLite copies the
	// source header's WAL read/write markers into the destination. The
	// serialized standalone image has no companion WAL, so normalize those two
	// documented header bytes to rollback-journal mode before deserializing it.
	body[18], body[19] = 1, 1
	return body, nil
}

func openDeserializedBackupDatabase(
	ctx context.Context, plaintext []byte,
) (*sql.DB, *sql.Conn, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, fmt.Errorf("open in-memory checkpoint: %w", err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("connect in-memory checkpoint: %w", err)
	}
	deserializeErr := conn.Raw(func(driverConn any) error {
		deserializer, ok := driverConn.(sqliteDeserializer)
		if !ok {
			return errors.New("sqlite driver does not support Deserialize")
		}
		return deserializer.Deserialize(plaintext)
	})
	if deserializeErr != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, nil, fmt.Errorf("deserialize in-memory checkpoint: %w", deserializeErr)
	}
	// sqlite3_deserialize disconnects existing handles. Close the invalidated
	// handle and ask database/sql for the replacement connection that owns the
	// deserialized in-memory database.
	_ = conn.Close()
	conn, err = db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("reconnect in-memory checkpoint: %w", err)
	}
	return db, conn, nil
}

func closeDeserializedBackupDatabase(db *sql.DB, conn *sql.Conn) {
	_ = conn.Close()
	_ = db.Close()
}

func serializeBackupConnection(conn *sql.Conn) ([]byte, error) {
	var body []byte
	if err := conn.Raw(func(driverConn any) error {
		serializer, ok := driverConn.(sqliteSerializer)
		if !ok {
			return errors.New("sqlite driver does not support Serialize")
		}
		var err error
		body, err = serializer.Serialize()
		return err
	}); err != nil {
		return nil, err
	}
	return body, nil
}

func installEncryptedCheckpoint(dir, target string, body []byte) error {
	file, err := os.CreateTemp(dir, ".latest-*.backup")
	if err != nil {
		return fmt.Errorf("reserve encrypted checkpoint: %w", err)
	}
	tempPath := file.Name()
	success := false
	defer func() {
		if !success {
			_ = file.Close()
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict encrypted checkpoint: %w", err)
	}
	if _, err := file.Write(body); err != nil {
		return fmt.Errorf("write encrypted checkpoint: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync encrypted checkpoint: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close encrypted checkpoint: %w", err)
	}
	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("install encrypted checkpoint: %w", err)
	}
	success = true
	return syncLocalBackupDirectory(dir)
}

func loadOrCreateBackupEncryptionKey(dbPath, checkpointPath string) ([]byte, error) {
	path := dbPath + backupEncryptionKeySuffix
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // fixed sibling credential path
	switch {
	case err == nil:
		key, readErr := readBackupEncryptionKey(path, file)
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, err
		}
		// Retry the publication durability barrier on every open. If the
		// creating daemon failed its directory sync after rename, a later
		// startup must not silently accept the key without completing it.
		if err := syncBackupKeyDirectory(filepath.Dir(path)); err != nil {
			return nil, err
		}
		return key, nil
	case errors.Is(err, fs.ErrNotExist):
		if _, statErr := os.Lstat(checkpointPath); statErr == nil {
			return nil, fmt.Errorf("%w: restore %s", errBackupKeyMissing, path)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("stat encrypted checkpoint: %w", statErr)
		}
		return createBackupEncryptionKey(path)
	case errors.Is(err, syscall.ELOOP):
		return nil, fmt.Errorf("backup encryption key %s is a symlink: %w",
			path, errBackupKeyPermissions)
	default:
		return nil, fmt.Errorf("open backup encryption key %s: %w", path, err)
	}
}

func readBackupEncryptionKey(path string, file *os.File) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat backup encryption key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("backup encryption key %s has mode %04o: %w",
			path, info.Mode().Perm(), errBackupKeyPermissions)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return nil, fmt.Errorf("backup encryption key %s has %d hard links, want 1: %w",
			path, stat.Nlink, errBackupKeyPermissions)
	}
	if info.Size() != backupEncryptionKeySize {
		return nil, fmt.Errorf("backup encryption key %s is %d bytes, want %d: %w",
			path, info.Size(), backupEncryptionKeySize, errBackupKeyMalformed)
	}
	key := make([]byte, backupEncryptionKeySize)
	if _, err := io.ReadFull(file, key); err != nil {
		return nil, fmt.Errorf("read backup encryption key %s: %w", path, err)
	}
	return key, nil
}

func createBackupEncryptionKey(path string) ([]byte, error) {
	key := make([]byte, backupEncryptionKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate backup encryption key: %w", err)
	}
	dirPath := filepath.Dir(path)
	file, err := os.CreateTemp(
		dirPath,
		"."+filepath.Base(path)+"-*.tmp",
	)
	if err != nil {
		return nil, fmt.Errorf("reserve backup encryption key %s: %w", path, err)
	}
	tempPath := file.Name()
	settled := false
	defer func() {
		if !settled {
			_ = file.Close()
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("restrict backup encryption key %s: %w", tempPath, err)
	}
	if _, err := file.Write(key); err != nil {
		return nil, fmt.Errorf("write backup encryption key %s: %w", tempPath, err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync backup encryption key %s: %w", tempPath, err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close backup encryption key %s: %w", tempPath, err)
	}
	if err := renameNoReplace(tempPath, path); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("publish backup encryption key %s: %w", path, err)
		}
		// Another daemon won the publication race. Its complete, synced key
		// is authoritative; never overwrite it or retain our losing key.
		winner, openErr := os.OpenFile( //nolint:gosec // fixed sibling credential path
			path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0,
		)
		if openErr != nil {
			return nil, fmt.Errorf("open winning backup encryption key %s: %w", path, openErr)
		}
		winningKey, readErr := readBackupEncryptionKey(path, winner)
		closeErr := winner.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, err
		}
		if err := os.Remove(tempPath); err != nil {
			return nil, fmt.Errorf("remove losing backup encryption key %s: %w", tempPath, err)
		}
		settled = true
		if err := syncBackupKeyDirectory(dirPath); err != nil {
			return nil, err
		}
		return winningKey, nil
	}
	settled = true
	if err := syncBackupKeyDirectory(dirPath); err != nil {
		return nil, err
	}
	return key, nil
}

func syncBackupKeyDirectory(dirPath string) error {
	dir, err := os.Open(dirPath) //nolint:gosec // fixed sibling credential directory
	if err != nil {
		return fmt.Errorf("open backup encryption key directory: %w", err)
	}
	defer dir.Close() //nolint:errcheck // Sync below is the durability signal
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync backup encryption key directory: %w", err)
	}
	return nil
}
