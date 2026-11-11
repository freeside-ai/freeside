package main

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func storeRevision(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var revision int64
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		state, err := tx.ServerState(context.Background())
		if err != nil {
			return err
		}
		revision = state.Revision
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return revision
}
