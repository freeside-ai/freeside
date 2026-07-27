package domain

import "fmt"

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
// dimension is explicit because the Phase 1A.2 waiver covers encryption only:
// it cannot collapse these three checks into one waivable verdict.
//
// The signal describes health, not a checkpoint representation. A source can
// evaluate the local owner-only checkpoint now, and the encrypted
// BackupCheckpoint can replace that source without changing admission policy.
type BackupHealth struct {
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

// RequireHealthy enforces the three non-encryption dimensions that gate
// unattended admission. Validation runs first so an absent dimension is not
// reported as an ordinary unhealthy observation.
func (h BackupHealth) RequireHealthy() error {
	if err := h.Validate(); err != nil {
		return err
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
