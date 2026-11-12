package signet_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func recordDrain(t *testing.T, f fixture, at time.Time) {
	t.Helper()
	if err := f.store.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordUpdateDrainTransition(context.Background(), domain.UpdateDrainTransition{
			State:      domain.UpdateDrainDraining,
			Build:      "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			OccurredAt: at,
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// TestUnattendedOperationReportsAnUpdateDrain: a drain in force stops
// unattended admission and projects as its own stop, after any other cause,
// dated by the drain and naming no item or command.
func TestUnattendedOperationReportsAnUpdateDrain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	since := f.now.Add(-time.Minute)
	recordDrain(t, f, since)
	requireDrainStop := func(when string, got signet.UnattendedStop) {
		t.Helper()
		if got.Kind != domain.UnattendedStopUpdateDrain || got.ItemID != nil || got.CommandID != nil ||
			got.Since == nil || !got.Since.Equal(since) {
			t.Fatalf("%s: stop = %+v, want update_drain since %v", when, got, since)
		}
	}
	drained := bootstrapUnattended(t, f.service)
	if drained.Admission != domain.UnattendedAdmissionStopped || len(drained.Stops) != 1 {
		t.Fatalf("drained: unattended_operation = %+v, want one stop", drained)
	}
	requireDrainStop("drain alone", drained.Stops[0])

	health := seedHealthItem(t, f, "health-1")
	both := bootstrapUnattended(t, f.service)
	if len(both.Stops) != 2 || both.Stops[0].Kind != domain.UnattendedStopBlockingSystemHealth ||
		both.Stops[0].ItemID == nil || *both.Stops[0].ItemID != health.ID {
		t.Fatalf("drain and finding: stops = %+v, want the finding first", both.Stops)
	}
	requireDrainStop("drain last", both.Stops[1])
}

// TestRestartReadinessCountsRunningWork: in-process work the daemon reports
// running is a blocker, so a drain with no stored work held is not ready
// until that work ends.
func TestRestartReadinessCountsRunningWork(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	running := map[domain.RestartBlockerKind]int{domain.RestartBlockerPublication: 1}
	service := signet.NewService(f.store,
		signet.WithClock(func() time.Time { return *f.now }),
		signet.WithRunningWork(func() map[domain.RestartBlockerKind]int { return running }))
	recordDrain(t, f, f.now.Add(-time.Minute))

	busy, err := service.RestartReadiness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if busy.Ready || busy.Drain == nil || len(busy.Blockers) != 1 ||
		busy.Blockers[0] != (domain.RestartBlocker{Kind: domain.RestartBlockerPublication, Count: 1}) {
		t.Fatalf("readiness with a running pass = %+v, want not ready on one publication", busy)
	}
	running = nil
	idle, err := service.RestartReadiness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !idle.Ready || len(idle.Blockers) != 0 {
		t.Fatalf("readiness after the pass = %+v, want ready", idle)
	}
}
