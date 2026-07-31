package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func healthyBackupHealth() domain.BackupHealth {
	return domain.BackupHealth{
		Encryption:         domain.BackupHealthHealthy,
		CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure:    domain.BackupHealthHealthy,
		RestoreTestAge:     domain.BackupHealthHealthy,
	}
}

func TestBackupCheckpointValidation(t *testing.T) {
	completedAt := time.Date(2026, 7, 30, 21, 0, 0, 0, time.UTC)
	restoreTestedAt := completedAt.Add(time.Hour)
	valid := domain.BackupCheckpoint{
		CheckpointID:           "0123456789abcdef0123456789abcdef",
		SyncEpoch:              "abcdef0123456789abcdef0123456789",
		ServerRevision:         42,
		SQLiteSnapshotDigest:   "sha256:snapshot",
		ArtifactManifestDigest: "sha256:manifest",
		CreatedAt:              completedAt,
		CompletedAt:            completedAt,
		RestoreTestedAt:        &restoreTestedAt,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid checkpoint: %v", err)
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*domain.BackupCheckpoint)
		wantErr error
	}{
		{"missing id", func(c *domain.BackupCheckpoint) { c.CheckpointID = "" }, domain.ErrEmptyID},
		{"missing epoch", func(c *domain.BackupCheckpoint) { c.SyncEpoch = "" }, domain.ErrEmptyField},
		{
			"missing snapshot digest",
			func(c *domain.BackupCheckpoint) { c.SQLiteSnapshotDigest = "" },
			domain.ErrEmptyField,
		},
		{
			"completion before creation",
			func(c *domain.BackupCheckpoint) { c.CompletedAt = c.CreatedAt.Add(-time.Second) },
			domain.ErrTimestampOutOfOrder,
		},
		{
			"restore before completion",
			func(c *domain.BackupCheckpoint) {
				value := c.CompletedAt.Add(-time.Second)
				c.RestoreTestedAt = &value
			},
			domain.ErrTimestampOutOfOrder,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkpoint := valid
			tc.mutate(&checkpoint)
			if err := checkpoint.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestBackupHealthRequiresEveryDimension(t *testing.T) {
	cases := []struct {
		name    string
		health  domain.BackupHealth
		wantErr error
	}{
		{"healthy", healthyBackupHealth(), nil},
		{
			"checkpoint not encrypted",
			func() domain.BackupHealth {
				health := healthyBackupHealth()
				health.Encryption = domain.BackupHealthUnhealthy
				return health
			}(),
			domain.ErrCheckpointNotEncrypted,
		},
		{
			"checkpoint not current",
			func() domain.BackupHealth {
				health := healthyBackupHealth()
				health.CheckpointCurrency = domain.BackupHealthUnhealthy
				return health
			}(),
			domain.ErrCheckpointNotCurrent,
		},
		{
			"artifact closure incomplete",
			func() domain.BackupHealth {
				health := healthyBackupHealth()
				health.ArtifactClosure = domain.BackupHealthUnhealthy
				return health
			}(),
			domain.ErrArtifactClosureIncomplete,
		},
		{
			"restore test stale",
			func() domain.BackupHealth {
				health := healthyBackupHealth()
				health.RestoreTestAge = domain.BackupHealthUnhealthy
				return health
			}(),
			domain.ErrRestoreTestStale,
		},
		{
			"missing checkpoint evaluation",
			func() domain.BackupHealth {
				health := healthyBackupHealth()
				health.CheckpointCurrency = ""
				return health
			}(),
			domain.ErrInvalidBackupHealthStatus,
		},
		{
			"unknown artifact evaluation",
			func() domain.BackupHealth {
				health := healthyBackupHealth()
				health.ArtifactClosure = "degraded"
				return health
			}(),
			domain.ErrInvalidBackupHealthStatus,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.health.RequireHealthy(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("RequireHealthy = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
