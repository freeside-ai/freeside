package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// BackupArtifactStore is the part of the content-addressed blob store local
// backup health needs to prove closure.
type BackupArtifactStore interface {
	Verify(domain.Digest) (bool, error)
}

// BackupPayloadDigestExtractor validates one reconstructed durable task and
// returns every blob digest needed to replay it after restore. An extractor
// that rejects its row does not fail the scan around it; see
// ErrBackupClosureIncomplete.
type BackupPayloadDigestExtractor func(QueueEntry) ([]domain.Digest, error)

// ErrBackupClosureIncomplete reports a durable outbox row whose blob
// references this binary cannot compute: an unregistered kind, an extractor
// that rejects the payload, or a row this store cannot reconstruct at all.
// The dominant cause is a downgrade past a row a newer daemon wrote, which is
// reversible, so the condition is reported rather than fatal: a daemon that
// refuses to start cannot be upgraded in place, and the same intolerance
// reached checkpoint production, backup health, and daemon startup alike.
//
// Reported means fail-closed but alive. Backup health reports the artifact
// closure unhealthy, which is domain.ErrArtifactClosureIncomplete at the
// admission boundary and holds unattended work until an operator acts, while
// attended work continues. No checkpoint is sealed from a scan carrying the
// gap and no checkpoint is verified against one, so a manifest never claims a
// closure that was not computed.
var ErrBackupClosureIncomplete = fmt.Errorf(
	"%w: a durable task's references are not reconstructable by this binary",
	domain.ErrArtifactClosureIncomplete)

// artifactClosure is one artifact-closure scan: the blob digests the scanned
// database needs after restore, plus the first row the scan could not read.
// A non-nil gap means digests describes less than the database holds.
type artifactClosure struct {
	digests []domain.Digest
	gap     error
}

func rejectBackupDuplicateJSONKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := checkBackupJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("backup JSON contains more than one top-level value")
		}
		return err
	}
	return nil
}

func checkBackupJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("backup JSON object key is not a string")
			}
			folded := strings.ToLower(key)
			if _, duplicate := seen[folded]; duplicate {
				return errors.New("backup JSON object contains a duplicate key")
			}
			seen[folded] = struct{}{}
			if err := checkBackupJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("backup JSON object is not terminated")
		}
	case '[':
		for decoder.More() {
			if err := checkBackupJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("backup JSON array is not terminated")
		}
	default:
		return errors.New("backup JSON contains an unexpected delimiter")
	}
	return nil
}

// recordGap keeps the first unreadable row in outbox order. Later rows are
// dropped rather than joined: one named row is what an operator acts on, and
// a downgrade past a version boundary makes every row of that kind fail.
func (c *artifactClosure) recordGap(gap error) {
	if c.gap == nil {
		c.gap = gap
	}
}

const (
	localBackupMarkerSchemaVersion = 15
	// DefaultLocalCheckpointMaxAge is the Phase 1A.2 currency window for the
	// local checkpoint. A daily checkpoint remains current across the writes
	// it protects while a missed daily cycle closes unattended admission.
	DefaultLocalCheckpointMaxAge = 24 * time.Hour
	// DefaultLocalRestoreTestMaxAge requires a successful restored copy at
	// least monthly. #238 can expose these policy values operationally without
	// changing the non-encryption health dimensions.
	DefaultLocalRestoreTestMaxAge = 30 * 24 * time.Hour
)

// LocalCheckpointHealthOptions names the owner-only local checkpoint, the
// restored copy that proves the checkpoint was tested, and the artifact store
// whose closure the checkpoint requires.
type LocalCheckpointHealthOptions struct {
	CheckpointPath    string
	RestoreTestPath   string
	Artifacts         BackupArtifactStore
	ApprovedRecipes   map[domain.Digest]bool
	PayloadExtractors map[string]BackupPayloadDigestExtractor
	CheckpointMaxAge  time.Duration
	RestoreTestMaxAge time.Duration
	Now               func() time.Time
}

// LocalBackupFiles owns the paired checkpoint and restore-test paths plus the
// in-process lease that keeps health evaluation on one installed generation.
type LocalBackupFiles struct {
	dir               string
	checkpointPath    string
	restoreTestPath   string
	encryptionKey     []byte
	artifacts         BackupArtifactStore
	approvedRecipes   map[domain.Digest]bool
	payloadExtractors map[string]BackupPayloadDigestExtractor
	mu                sync.RWMutex
	// liveClosureGap carries the producer's last verdict on the live database
	// to the health evaluator that shares this file set: true once a
	// maintenance pass finds a durable row whose references this binary cannot
	// compute (see ErrBackupClosureIncomplete). It is deliberately outside mu,
	// which leases one installed checkpoint generation; this describes the
	// live store, not the file, and a maintenance pass records it while
	// holding no lease. NewProducer sets it until that first pass, so a file
	// set with a live database behind it never answers from the checkpoint
	// alone; a file set with no producer keeps the checkpoint-only verdict.
	liveClosureGap atomic.Bool
}

type localCheckpointHealthSource struct {
	files             *LocalBackupFiles
	artifacts         BackupArtifactStore
	approvedRecipes   map[domain.Digest]bool
	payloadExtractors map[string]BackupPayloadDigestExtractor
	checkpointMaxAge  time.Duration
	restoreTestMaxAge time.Duration
	now               func() time.Time
}

// NewDefaultLocalBackupFiles uses the daemon's established owner-only
// checkpoint directory beside the database.
func NewDefaultLocalBackupFiles(dbPath string) (*LocalBackupFiles, error) {
	dir, checkpointPath, restoreTestPath, err := defaultLocalBackupPaths(dbPath)
	if err != nil {
		return nil, fmt.Errorf("local backup files: %w", err)
	}
	key, err := loadOrCreateBackupEncryptionKey(dbPath, checkpointPath)
	if err != nil {
		return nil, fmt.Errorf("local backup files: %w", err)
	}
	return &LocalBackupFiles{
		dir: dir, checkpointPath: checkpointPath, restoreTestPath: restoreTestPath,
		encryptionKey: key,
	}, nil
}

// NewEncryptedLocalBackupFiles builds an encrypted checkpoint file set around
// an externally supplied data key. The key is copied and never persisted by
// this constructor; production uses NewDefaultLocalBackupFiles, whose
// host-local key is stored outside the checkpoint directory.
func NewEncryptedLocalBackupFiles(
	checkpointPath string, encryptionKey []byte,
) (*LocalBackupFiles, error) {
	if checkpointPath == "" {
		return nil, errors.New("encrypted local backup files: empty checkpoint path")
	}
	if len(encryptionKey) != backupEncryptionKeySize {
		return nil, fmt.Errorf("encrypted local backup files: key is %d bytes, want %d",
			len(encryptionKey), backupEncryptionKeySize)
	}
	dir := filepath.Dir(checkpointPath)
	return &LocalBackupFiles{
		dir:             dir,
		checkpointPath:  checkpointPath,
		restoreTestPath: filepath.Join(dir, legacyRestoreTestFilename),
		encryptionKey:   slices.Clone(encryptionKey),
	}, nil
}

// NewCheckpointHealthSource builds the health evaluator paired with this
// encrypted checkpoint file set. The producer and evaluator share a lease so
// each health query sees one complete authenticated generation.
func (f *LocalBackupFiles) NewCheckpointHealthSource(
	artifacts BackupArtifactStore,
	approvedRecipes map[domain.Digest]bool,
	payloadExtractors map[string]BackupPayloadDigestExtractor,
) (BackupHealthSource, error) {
	opts := LocalCheckpointHealthOptions{
		Artifacts:         artifacts,
		ApprovedRecipes:   approvedRecipes,
		PayloadExtractors: payloadExtractors,
	}
	if f == nil || len(f.encryptionKey) == 0 {
		return nil, errors.New("encrypted checkpoint health: missing encryption key")
	}
	f.artifacts = artifacts
	f.approvedRecipes = maps.Clone(approvedRecipes)
	f.payloadExtractors = maps.Clone(payloadExtractors)
	return newEncryptedCheckpointHealthSource(opts, f)
}

// NewLocalCheckpointHealthSource retains the pre-encryption evaluator for
// compatibility tests. It always reports encryption unhealthy, so it cannot
// admit unattended work in this build.
func NewLocalCheckpointHealthSource(opts LocalCheckpointHealthOptions) (BackupHealthSource, error) {
	return newLocalCheckpointHealthSource(opts, &LocalBackupFiles{
		checkpointPath:  opts.CheckpointPath,
		restoreTestPath: opts.RestoreTestPath,
	})
}

func newLocalCheckpointHealthSource(
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
		return nil, errors.New("local checkpoint health: nil backup files")
	case files.checkpointPath == "":
		return nil, errors.New("local checkpoint health: empty checkpoint path")
	case files.restoreTestPath == "":
		return nil, errors.New("local checkpoint health: empty restore-test path")
	case opts.Artifacts == nil:
		return nil, errors.New("local checkpoint health: nil artifact store")
	case opts.CheckpointMaxAge < 0:
		return nil, errors.New("local checkpoint health: negative checkpoint max age")
	case opts.RestoreTestMaxAge < 0:
		return nil, errors.New("local checkpoint health: negative restore-test max age")
	}
	return &localCheckpointHealthSource{
		files:             files,
		artifacts:         opts.Artifacts,
		approvedRecipes:   maps.Clone(opts.ApprovedRecipes),
		payloadExtractors: maps.Clone(opts.PayloadExtractors),
		checkpointMaxAge:  opts.CheckpointMaxAge,
		restoreTestMaxAge: opts.RestoreTestMaxAge,
		now:               opts.Now,
	}, nil
}

type backupDatabaseSnapshot struct {
	state         ServerState
	schemaVersion int
	digests       []domain.Digest
	// closureGap names the first durable row whose blob references this scan
	// could not compute, and is nil when the closure is complete. It is
	// meaningful only for a scan that collected digests.
	closureGap              error
	fileDigest              domain.Digest
	restoreCheckpointDigest domain.Digest
	generatedAt             time.Time
	restoredAt              time.Time
}

type backupDatabaseReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func unhealthyBackupHealth() domain.BackupHealth {
	return domain.BackupHealth{
		Encryption:         domain.BackupHealthUnhealthy,
		CheckpointCurrency: domain.BackupHealthUnhealthy,
		ArtifactClosure:    domain.BackupHealthUnhealthy,
		RestoreTestAge:     domain.BackupHealthUnhealthy,
	}
}

func (s *localCheckpointHealthSource) BackupHealth(
	ctx context.Context, current BackupHealthContext,
) (domain.BackupHealth, error) {
	s.files.mu.RLock()
	defer s.files.mu.RUnlock()

	health := unhealthyBackupHealth()
	checkpoint, found, err := inspectBackupDatabase(
		ctx, s.files.checkpointPath, true, s.approvedRecipes, s.payloadExtractors)
	if err != nil {
		return domain.BackupHealth{}, fmt.Errorf("local checkpoint health: %w", err)
	}
	if !found {
		return health, nil
	}
	now := s.now()
	checkpointAge := now.Sub(checkpoint.generatedAt)
	if checkpoint.schemaVersion == current.SchemaVersion &&
		checkpoint.state.SyncEpoch == current.SyncEpoch &&
		!checkpoint.generatedAt.IsZero() &&
		checkpointAge >= 0 && checkpointAge <= s.checkpointMaxAge {
		health.CheckpointCurrency = domain.BackupHealthHealthy
	}

	// No manifest binds a plaintext checkpoint, so the scan's own gap is the
	// only evidence here that it described everything the file holds; the live
	// gap is the same signal for the database the checkpoint protects.
	closed := checkpoint.closureGap == nil && !s.files.liveClosureGap.Load()
	for _, digest := range checkpoint.digests {
		verified, err := s.artifacts.Verify(digest)
		if err != nil {
			return domain.BackupHealth{}, fmt.Errorf("local checkpoint health: artifact %s: %w", digest, err)
		}
		if !verified {
			closed = false
		}
	}
	if closed {
		health.ArtifactClosure = domain.BackupHealthHealthy
	}

	restored, found, err := inspectBackupDatabase(
		ctx, s.files.restoreTestPath, false, nil, nil)
	if err != nil {
		return domain.BackupHealth{}, fmt.Errorf("local checkpoint health: restore test: %w", err)
	}
	if found {
		restoreTestAge := now.Sub(restored.restoredAt)
		if restoreTestAge >= 0 && restoreTestAge <= s.restoreTestMaxAge &&
			restored.schemaVersion == checkpoint.schemaVersion &&
			restored.restoreCheckpointDigest == checkpoint.fileDigest {
			health.RestoreTestAge = domain.BackupHealthHealthy
		}
	}
	return health, nil
}

func inspectBackupDatabase(
	ctx context.Context, path string, collectDigests bool,
	approvedRecipes map[domain.Digest]bool,
	payloadExtractors map[string]BackupPayloadDigestExtractor,
) (backupDatabaseSnapshot, bool, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return backupDatabaseSnapshot{}, false, nil
	case err != nil:
		return backupDatabaseSnapshot{}, false, fmt.Errorf("stat %s: %w", path, err)
	case !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0:
		return backupDatabaseSnapshot{}, false, nil
	}

	q := url.Values{"mode": []string{"ro"}}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return backupDatabaseSnapshot{}, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	fileDigest, err := localBackupFileDigest(path)
	if err != nil {
		return backupDatabaseSnapshot{}, false, fmt.Errorf("hash %s: %w", path, err)
	}
	snapshot, err := inspectBackupDB(
		ctx, db, fileDigest, collectDigests, approvedRecipes, payloadExtractors)
	if err != nil {
		return backupDatabaseSnapshot{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	return snapshot, true, nil
}

func inspectBackupDB(
	ctx context.Context,
	db backupDatabaseReader,
	fileDigest domain.Digest,
	collectDigests bool,
	approvedRecipes map[domain.Digest]bool,
	payloadExtractors map[string]BackupPayloadDigestExtractor,
) (backupDatabaseSnapshot, error) {
	snapshot := backupDatabaseSnapshot{fileDigest: fileDigest}
	if err := db.QueryRowContext(ctx,
		`SELECT sync_epoch, revision FROM server_state WHERE id = 1`).
		Scan(&snapshot.state.SyncEpoch, &snapshot.state.Revision); err != nil {
		return backupDatabaseSnapshot{}, fmt.Errorf("server state: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).
		Scan(&snapshot.schemaVersion); err != nil {
		return backupDatabaseSnapshot{}, fmt.Errorf("schema version: %w", err)
	}
	if snapshot.schemaVersion >= localBackupMarkerSchemaVersion {
		var err error
		var generatedAt string
		err = db.QueryRowContext(ctx,
			`SELECT generated_at
			   FROM local_backup_checkpoint_marker WHERE id = 1`).
			Scan(&generatedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return backupDatabaseSnapshot{}, fmt.Errorf("checkpoint marker: %w", err)
		default:
			snapshot.generatedAt, err = parseTime(generatedAt)
			if err != nil {
				return backupDatabaseSnapshot{}, fmt.Errorf("checkpoint marker time: %w", err)
			}
		}

		var restoredAt string
		err = db.QueryRowContext(ctx,
			`SELECT checkpoint_digest, restored_at
			   FROM local_backup_restore_marker WHERE id = 1`).
			Scan(&snapshot.restoreCheckpointDigest, &restoredAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return backupDatabaseSnapshot{}, fmt.Errorf("restore marker: %w", err)
		default:
			snapshot.restoredAt, err = parseTime(restoredAt)
			if err != nil {
				return backupDatabaseSnapshot{}, fmt.Errorf("restore marker time: %w", err)
			}
		}
	}
	if collectDigests {
		closure, err := checkpointArtifactDigests(
			ctx, db, approvedRecipes, payloadExtractors)
		if err != nil {
			return backupDatabaseSnapshot{}, fmt.Errorf("artifact closure: %w", err)
		}
		snapshot.digests, snapshot.closureGap = closure.digests, closure.gap
	}
	return snapshot, nil
}

func checkpointArtifactDigests(
	ctx context.Context,
	db backupDatabaseReader,
	approvedRecipes map[domain.Digest]bool,
	payloadExtractors map[string]BackupPayloadDigestExtractor,
) (artifactClosure, error) {
	digests := make(map[domain.Digest]struct{})
	sqlTx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return artifactClosure{}, err
	}
	defer func() { _ = sqlTx.Rollback() }()
	readTx := &ReadTx{tx: sqlTx, approvedRecipes: approvedRecipes}

	artifactIDs, err := checkpointIDs[domain.ArtifactID](
		ctx, sqlTx, `SELECT id FROM artifacts ORDER BY id`)
	if err != nil {
		return artifactClosure{}, err
	}
	for _, id := range artifactIDs {
		artifact, err := readTx.GetArtifact(ctx, id)
		if err != nil {
			return artifactClosure{}, err
		}
		digests[artifact.Digest] = struct{}{}
	}

	conversations, err := readTx.ListConversations(ctx)
	if err != nil {
		return artifactClosure{}, err
	}
	for _, snapshotted := range conversations {
		conversation := snapshotted.Value
		for _, message := range conversation.Messages {
			for _, digest := range message.Attachments {
				digests[digest] = struct{}{}
			}
		}
	}

	admissionRows, err := sqlTx.QueryContext(ctx, listExecutionAdmissionsSQL)
	if err != nil {
		return artifactClosure{}, err
	}
	for admissionRows.Next() {
		admission, err := scanExecutionAdmissionRecord(admissionRows)
		if err != nil {
			_ = admissionRows.Close()
			return artifactClosure{}, err
		}
		if admission.StageInputs == nil {
			continue
		}
		stageInputs := admission.StageInputs
		digests[stageInputs.SpecificationDigest] = struct{}{}
		digests[stageInputs.PromptPackageDigest] = struct{}{}
		digests[stageInputs.PolicyDigest] = struct{}{}
		if stageInputs.VendorInstructions != nil &&
			stageInputs.VendorInstructions.Digest != nil {
			digests[*stageInputs.VendorInstructions.Digest] = struct{}{}
		}
		if stageInputs.ConversationDigest != nil {
			digests[*stageInputs.ConversationDigest] = struct{}{}
		}
		for _, digest := range stageInputs.PriorArtifactDigests {
			digests[digest] = struct{}{}
		}
		for _, digest := range stageInputs.ImageInputDigests {
			digests[digest] = struct{}{}
		}
	}
	if err := errors.Join(admissionRows.Err(), admissionRows.Close()); err != nil {
		return artifactClosure{}, err
	}

	reviewRows, err := sqlTx.QueryContext(ctx,
		`SELECT body_digest, body FROM codex_review_requests ORDER BY invocation_id`)
	if err != nil {
		return artifactClosure{}, err
	}
	for reviewRows.Next() {
		var bodyDigest string
		var body []byte
		if err := reviewRows.Scan(&bodyDigest, &body); err != nil {
			_ = reviewRows.Close()
			return artifactClosure{}, err
		}
		if bodyDigest != codexReviewBodyDigest(body) {
			_ = reviewRows.Close()
			return artifactClosure{}, errors.New("codex review request body digest is invalid")
		}
		if err := rejectBackupDuplicateJSONKeys(body); err != nil {
			_ = reviewRows.Close()
			return artifactClosure{}, errors.New("codex review request instruction closure is ambiguous")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			_ = reviewRows.Close()
			return artifactClosure{}, errors.New("codex review request instruction closure is invalid")
		}
		var instructionRaw json.RawMessage
		for key, raw := range fields {
			if strings.EqualFold(key, "instructions") {
				instructionRaw = raw
			}
		}
		if instructionRaw == nil {
			// Review requests written before instruction authority carry no
			// instruction artifacts. Their review records cannot satisfy the
			// current gate, but they do not make historical backups incomplete.
			continue
		}
		if bytes.Equal(bytes.TrimSpace(instructionRaw), []byte("null")) {
			_ = reviewRows.Close()
			return artifactClosure{}, errors.New("codex review request instruction closure is null")
		}
		var request exec.ReviewRequest
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF || request.Validate() != nil {
			_ = reviewRows.Close()
			return artifactClosure{}, errors.New("codex review request instruction closure is invalid")
		}
		instructions := request.Instructions
		if instructions.CompositionVersion != "codex_explicit_bundle_v1" ||
			!contentaddr.Valid(string(instructions.ResultDigest)) {
			_ = reviewRows.Close()
			return artifactClosure{}, errors.New("codex review request instruction closure is invalid")
		}
		if instructions.HostDigest != nil {
			if !contentaddr.Valid(string(*instructions.HostDigest)) {
				_ = reviewRows.Close()
				return artifactClosure{}, errors.New("codex review host instruction closure is invalid")
			}
			digests[*instructions.HostDigest] = struct{}{}
		}
		priorPath := ""
		for _, source := range instructions.RepositorySources {
			if !fs.ValidPath(source.Path) || source.Path == "." ||
				priorPath >= source.Path || !contentaddr.Valid(string(source.Digest)) {
				_ = reviewRows.Close()
				return artifactClosure{}, errors.New("codex review repository instruction closure is invalid")
			}
			priorPath = source.Path
			digests[source.Digest] = struct{}{}
		}
		digests[instructions.ResultDigest] = struct{}{}
	}
	if err := errors.Join(reviewRows.Err(), reviewRows.Close()); err != nil {
		return artifactClosure{}, err
	}

	items, err := readTx.ListAttentionItems(ctx)
	if err != nil {
		return artifactClosure{}, err
	}
	for _, snapshotted := range items {
		item := snapshotted.Value
		for _, artifact := range item.EvidenceSnapshot {
			digests[artifact.Digest] = struct{}{}
		}
		for _, claim := range item.AgentClaims {
			if claim.Text == nil {
				digests[claim.Digest] = struct{}{}
			}
		}
	}

	commandIDs, err := checkpointIDs[string](
		ctx, sqlTx, `SELECT command_id FROM commands ORDER BY command_id`)
	if err != nil {
		return artifactClosure{}, err
	}
	for _, commandID := range commandIDs {
		command, inline, _, err := readTx.getStoredCommandSnapshot(ctx, commandID)
		if err != nil {
			return artifactClosure{}, err
		}
		for _, digest := range command.ArtifactDigests {
			if _, carriedInline := inline[digest]; !carriedInline {
				digests[digest] = struct{}{}
			}
		}
		for _, digest := range command.Attachments {
			digests[digest] = struct{}{}
		}
	}

	outbox, err := outboxArtifactDigests(ctx, readTx, payloadExtractors)
	if err != nil {
		return artifactClosure{}, err
	}
	for _, digest := range outbox.digests {
		digests[digest] = struct{}{}
	}

	out := make([]domain.Digest, 0, len(digests))
	for digest := range digests {
		out = append(out, digest)
	}
	return artifactClosure{digests: out, gap: outbox.gap}, nil
}

// outboxArtifactDigests reconstructs every durable task and returns the blob
// digests they need after restore. Only this scan tolerates a row it cannot
// read, because only here is the row a payload written by some binary's
// version of the intent rather than state this store owns: a row this binary
// cannot reconstruct is a closure gap (see ErrBackupClosureIncomplete), while
// a queue read that fails is not. Broken owned state stays loud everywhere
// else in the closure.
func outboxArtifactDigests(
	ctx context.Context,
	tx *ReadTx,
	payloadExtractors map[string]BackupPayloadDigestExtractor,
) (artifactClosure, error) {
	keys, err := checkpointIDs[string](
		ctx, tx.tx, `SELECT idempotency_key FROM outbox ORDER BY id`)
	if err != nil {
		return artifactClosure{}, err
	}
	closure := artifactClosure{}
	for _, key := range keys {
		// A row this store cannot rebuild at all is the same gap one column
		// over: a status or timestamp a newer daemon wrote wedges the scan
		// exactly as an unreadable payload does, and has the same remedy.
		entry, err := tx.GetOutbox(ctx, key)
		if err != nil {
			closure.recordGap(fmt.Errorf("outbox backup references: %w", err))
			continue
		}
		extract := payloadExtractors[entry.Kind]
		if extract == nil {
			closure.recordGap(fmt.Errorf(
				"outbox %q backup references: unregistered kind %q", key, entry.Kind))
			continue
		}
		references, err := extract(entry)
		if err != nil {
			closure.recordGap(fmt.Errorf("outbox %q backup references: %w", key, err))
			continue
		}
		if slices.Contains(references, "") {
			closure.recordGap(fmt.Errorf("outbox %q backup references: empty digest", key))
			continue
		}
		closure.digests = append(closure.digests, references...)
	}
	return closure, nil
}

func checkpointIDs[T ~string](
	ctx context.Context, tx *sql.Tx, query string,
) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []T{}
	for rows.Next() {
		var id T
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func localBackupFileDigest(path string) (domain.Digest, error) {
	file, err := os.Open(path) //nolint:gosec // validated owner-only backup path
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return "", err
	}
	return domain.Digest(contentaddr.Format(hash.Sum(nil))), nil
}
