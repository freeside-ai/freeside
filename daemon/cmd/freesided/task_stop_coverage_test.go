package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestUnprovableTaskStopFollowsTheCoverageRecord(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := ward.OpenCancellationCoverage(
		filepath.Join(t.TempDir(), "task-runtime-coverage.json"), state.SyncEpoch, []domain.TaskID{"task-before-record"})
	if err != nil {
		t.Fatal(err)
	}
	if reason, err := unprovableTaskStop(ctx, st, coverage, "task-after-record"); err != nil || reason != "" {
		t.Fatalf("covered task reported unprovable: %q, %v", reason, err)
	}
	if reason, err := unprovableTaskStop(ctx, st, coverage, "task-before-record"); err != nil || !strings.HasPrefix(reason, "runtime coverage: ") {
		t.Fatalf("task that predates the record: %q, %v", reason, err)
	}
	if _, err := st.NewEpoch(ctx); err != nil {
		t.Fatal(err)
	}
	if reason, err := unprovableTaskStop(ctx, st, coverage, "task-after-record"); err != nil || !strings.HasPrefix(reason, "runtime coverage: ") {
		t.Fatalf("epoch that differs from the record's: %q, %v", reason, err)
	}
	// A reason ends retries for good, so a failed read must never produce one.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if reason, err := unprovableTaskStop(ctx, st, coverage, "task-before-record"); err == nil || reason != "" {
		t.Fatalf("failed epoch read reported a reason: %q, %v", reason, err)
	}
}
