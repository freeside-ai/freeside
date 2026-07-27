package domain_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func healthyBackupHealth() domain.BackupHealth {
	return domain.BackupHealth{
		CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure:    domain.BackupHealthHealthy,
		RestoreTestAge:     domain.BackupHealthHealthy,
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
