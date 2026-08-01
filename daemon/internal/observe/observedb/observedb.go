// Package observedb is the whole of the follow path's access to the daemon's
// database: open it, read one run's observation aggregate, close it. Nothing
// else is exported, so the follow view cannot reach the store's write,
// checkpoint, restore, or backup-file surfaces even though this package
// imports the store to build on them.
//
// Its own proof is not another assertion: it is that this file is short
// enough to read in full and exports exactly three things. An import
// allowlist bounds which packages a caller can name, never which methods of
// a permitted package it calls, so the regress has to stop at a surface
// small enough to check by eye. This is that surface, and
// internal/observe/containment_test.go says so rather than claiming its
// allowlist is total.
package observedb

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Store is a read-only view of one daemon database's run observations.
type Store struct {
	store *store.Store
}

// Open opens the daemon's database at path. Options are empty by design: the
// observation read surface re-validates every row it returns and gates
// nothing on operator policy, so following a run needs no approved-recipe or
// admission-floor configuration. Opening migrates the schema to head, the
// same behaviour freesided submit and doctor already have on this transport.
func Open(ctx context.Context, path string) (*Store, error) {
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return &Store{store: st}, nil
}

// ObserveRun reads one run's observation aggregate, re-validated by the store.
func (s *Store) ObserveRun(
	ctx context.Context, runID domain.RunID,
) (domain.RunObservation, error) {
	var observation domain.RunObservation
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		observation, err = tx.ObserveRun(ctx, runID)
		return err
	}); err != nil {
		return domain.RunObservation{}, fmt.Errorf("observe run: %w", err)
	}
	return observation, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if err := s.store.Close(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}
	return nil
}
