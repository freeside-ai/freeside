package domain

import (
	"fmt"
	"time"
)

// BackupCheckpoint is the authenticated metadata bound to one encrypted
// SQLite snapshot (plan §5.10). The ciphertext is a store concern; this shape
// is the durable contract a restore verifies before trusting the plaintext.
type BackupCheckpoint struct {
	CheckpointID           string     `json:"checkpoint_id"`
	SyncEpoch              string     `json:"sync_epoch"`
	ServerRevision         int64      `json:"server_revision"`
	SQLiteSnapshotDigest   Digest     `json:"sqlite_snapshot_digest"`
	ArtifactManifestDigest Digest     `json:"artifact_manifest_digest"`
	CreatedAt              time.Time  `json:"created_at"`
	CompletedAt            time.Time  `json:"completed_at"`
	RestoreTestedAt        *time.Time `json:"restore_tested_at"`
}

// Validate rejects incomplete or non-canonical checkpoint metadata before it
// is accepted as authenticated restore input.
func (c BackupCheckpoint) Validate() error {
	switch {
	case c.CheckpointID == "":
		return fmt.Errorf("backup checkpoint checkpoint_id: %w", ErrEmptyID)
	case c.SyncEpoch == "":
		return fmt.Errorf("backup checkpoint sync_epoch: %w", ErrEmptyField)
	case c.SQLiteSnapshotDigest == "":
		return fmt.Errorf("backup checkpoint sqlite_snapshot_digest: %w", ErrEmptyField)
	case c.ArtifactManifestDigest == "":
		return fmt.Errorf("backup checkpoint artifact_manifest_digest: %w", ErrEmptyField)
	case c.CreatedAt.IsZero():
		return fmt.Errorf("backup checkpoint created_at: %w", ErrMissingTimestamp)
	case c.CompletedAt.IsZero():
		return fmt.Errorf("backup checkpoint completed_at: %w", ErrMissingTimestamp)
	case c.CreatedAt.Location() != time.UTC:
		return fmt.Errorf("backup checkpoint created_at: %w", ErrTimestampNotUTC)
	case c.CompletedAt.Location() != time.UTC:
		return fmt.Errorf("backup checkpoint completed_at: %w", ErrTimestampNotUTC)
	case c.CompletedAt.Before(c.CreatedAt):
		return fmt.Errorf("backup checkpoint completed_at: %w", ErrTimestampOutOfOrder)
	}
	if c.RestoreTestedAt != nil {
		switch {
		case c.RestoreTestedAt.Location() != time.UTC:
			return fmt.Errorf("backup checkpoint restore_tested_at: %w", ErrTimestampNotUTC)
		case c.RestoreTestedAt.Before(c.CompletedAt):
			return fmt.Errorf("backup checkpoint restore_tested_at: %w", ErrTimestampOutOfOrder)
		}
	}
	return nil
}

// BackupHealthStatus is one independently evaluated dimension of backup
// health. The zero value is invalid so an unpopulated signal cannot be
// mistaken for a failing-but-complete evaluation.
type BackupHealthStatus string

const (
	BackupHealthHealthy   BackupHealthStatus = "healthy"
	BackupHealthUnhealthy BackupHealthStatus = "unhealthy"
)

// AllBackupHealthStatuses is the single registration point for backup-health
// statuses.
var AllBackupHealthStatuses = []BackupHealthStatus{
	BackupHealthHealthy,
	BackupHealthUnhealthy,
}

func (s BackupHealthStatus) valid() bool {
	switch s {
	case BackupHealthHealthy, BackupHealthUnhealthy:
		return true
	default:
		return false
	}
}

// BackupHealth is the queryable §5.7 signal admission consumes. Each
// dimension is explicit so encryption, currency, closure, and restore proof
// remain independently queryable and cannot silently substitute for one
// another.
//
// The signal describes health, not a checkpoint representation. The encrypted
// BackupCheckpoint source and any later portable source satisfy the same
// admission contract.
type BackupHealth struct {
	Encryption         BackupHealthStatus `json:"encryption"`
	CheckpointCurrency BackupHealthStatus `json:"checkpoint_currency"`
	ArtifactClosure    BackupHealthStatus `json:"artifact_closure"`
	RestoreTestAge     BackupHealthStatus `json:"restore_test_age"`
}

// Validate reports whether every dimension was evaluated.
func (h BackupHealth) Validate() error {
	for _, dimension := range []struct {
		name   string
		status BackupHealthStatus
	}{
		{"encryption", h.Encryption},
		{"checkpoint_currency", h.CheckpointCurrency},
		{"artifact_closure", h.ArtifactClosure},
		{"restore_test_age", h.RestoreTestAge},
	} {
		if !dimension.status.valid() {
			return fmt.Errorf("backup health %s %q: %w",
				dimension.name, dimension.status, ErrInvalidBackupHealthStatus)
		}
	}
	return nil
}

// RequireHealthy enforces every dimension that gates unattended admission.
// Validation runs first so an absent dimension is not reported as an ordinary
// unhealthy observation.
func (h BackupHealth) RequireHealthy() error {
	if err := h.Validate(); err != nil {
		return err
	}
	switch h.Encryption {
	case BackupHealthHealthy:
	case BackupHealthUnhealthy:
		return ErrCheckpointNotEncrypted
	}
	switch h.CheckpointCurrency {
	case BackupHealthHealthy:
	case BackupHealthUnhealthy:
		return ErrCheckpointNotCurrent
	}
	switch h.ArtifactClosure {
	case BackupHealthHealthy:
	case BackupHealthUnhealthy:
		return ErrArtifactClosureIncomplete
	}
	switch h.RestoreTestAge {
	case BackupHealthHealthy:
		return nil
	case BackupHealthUnhealthy:
		return ErrRestoreTestStale
	}
	return ErrInvalidBackupHealthStatus
}
