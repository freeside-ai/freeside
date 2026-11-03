package main

import (
	"context"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
)

// usageOperator routes a client's usage commands to the collection
// lifecycle. Signet has already checked the pool and the enrollment. What
// must commit with the receipt is written in the command's transaction; the
// collection starts only after it commits, so a rolled-back or replayed
// command starts none.
type usageOperator struct {
	coordinator *usagecollect.Coordinator
}

var _ signet.UsageOperator = usageOperator{}

// RefreshUsage writes nothing: the collection writes the record, as
// `freesided auth refresh-usage` does, but the command does not wait for it.
func (o usageOperator) RefreshUsage(
	_ context.Context, _ *store.WriteTx, pool domain.UsagePoolID, _ time.Time,
) (func(), error) {
	return func() { o.coordinator.RequestRefresh(pool) }, nil
}

// ReassignCollector records the new collector with the receipt, then resets
// the pool's collection under it, as `freesided auth set-collector` does.
// Naming the current collector writes and starts nothing.
func (o usageOperator) ReassignCollector(
	ctx context.Context, tx *store.WriteTx, pool domain.UsagePoolID, collector domain.ClientEnrollmentID, now time.Time,
) (func(), error) {
	result, err := usagecollect.ReassignTx(ctx, tx, pool, collector, now)
	if err != nil || !result.Recorded {
		return nil, err
	}
	return func() { o.coordinator.CollectorReassigned(pool) }, nil
}
