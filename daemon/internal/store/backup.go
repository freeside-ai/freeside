package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// BackupHealthContext is the live state a source compares with its checkpoint.
// It deliberately excludes representation details so the encrypted checkpoint
// can replace the provisional local evaluator behind the same interface.
type BackupHealthContext struct {
	ServerState
	SchemaVersion int
}

// BackupHealthSource supplies the live, representation-independent backup
// health signal consumed by unattended admission. A Phase 1A.2 implementation
// evaluates the local owner-only checkpoint; the encrypted checkpoint can
// replace it behind this seam without changing the gate.
//
// Implementations must not call back into the Store whose transaction is
// querying them. Admission checks run inside the write or reconstruction
// transaction so the verdict and the record cannot be separated by another
// store operation.
type BackupHealthSource interface {
	BackupHealth(context.Context, BackupHealthContext) (domain.BackupHealth, error)
}

// BackupHealthSourceFunc adapts a function into a BackupHealthSource.
type BackupHealthSourceFunc func(context.Context, BackupHealthContext) (domain.BackupHealth, error)

// BackupHealth implements BackupHealthSource.
func (f BackupHealthSourceFunc) BackupHealth(
	ctx context.Context, state BackupHealthContext,
) (domain.BackupHealth, error) {
	return f(ctx, state)
}

// BackupHealth queries the configured source without attempting admission.
// Unhealthy dimensions are returned as data; only a missing source, source
// failure, or malformed signal is an error.
func (s *Store) BackupHealth(ctx context.Context) (domain.BackupHealth, error) {
	if s.backupHealthSource == nil {
		return domain.BackupHealth{}, domain.ErrBackupHealthUnavailable
	}
	var health domain.BackupHealth
	err := s.Read(ctx, func(tx *ReadTx) error {
		state, err := tx.backupHealthContext(ctx)
		if err != nil {
			return err
		}
		health, err = s.backupHealthSource.BackupHealth(ctx, state)
		return err
	})
	if err != nil {
		return domain.BackupHealth{}, fmt.Errorf("query backup health: %w", err)
	}
	if err := health.Validate(); err != nil {
		return domain.BackupHealth{}, fmt.Errorf("query backup health: %w", err)
	}
	return health, nil
}

func (tx *ReadTx) backupHealthContext(ctx context.Context) (BackupHealthContext, error) {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return BackupHealthContext{}, err
	}
	var schemaVersion int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).
		Scan(&schemaVersion); err != nil {
		return BackupHealthContext{}, fmt.Errorf("read schema version: %w", err)
	}
	return BackupHealthContext{ServerState: state, SchemaVersion: schemaVersion}, nil
}
