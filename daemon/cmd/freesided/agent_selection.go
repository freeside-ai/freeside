package main

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// recordBaselineAdapterConformance appends one adapter conformance record for
// each baseline adapter build (plan §5.4, admission step 3). The proved set
// is the adapter's declared set: no stage contract suite drives the real
// harness yet, so the record attests what the launch code for the pinned
// build does, and a launch needing more fails admission closed. An adapter
// the operator edits in the tree has another digest and so no record.
func recordBaselineAdapterConformance(ctx context.Context, st *store.Store, provedAt time.Time) error {
	records, err := agentbaseline.ConformanceRecords(provedAt)
	if err != nil {
		return fmt.Errorf("baseline adapter conformance: %w", err)
	}
	return st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, record := range records {
			if _, err := tx.RecordAdapterConformance(ctx, record); err != nil {
				return err
			}
		}
		return nil
	})
}
