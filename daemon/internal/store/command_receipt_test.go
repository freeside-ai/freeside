package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func refreshReceipt(commandID string) domain.CommandReceipt {
	return domain.CommandReceipt{UsageRefresh: &domain.UsageRefreshReceipt{
		UsageRefreshRequest: domain.UsageRefreshRequest{
			CommandIdentity: domain.CommandIdentity{CommandID: commandID, DeviceID: "device-1"}, Pool: "pool-a",
		},
		RequestedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}}
}

func recordReceipt(t *testing.T, s *store.Store, receipt domain.CommandReceipt) (store.Snapshot, error) {
	t.Helper()
	var snap store.Snapshot
	err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		snap, err = tx.RecordCommandReceipt(t.Context(), receipt)
		return err
	})
	return snap, err
}

func readReceipt(t *testing.T, s *store.Store, commandID string) (domain.CommandReceipt, store.Snapshot, error) {
	t.Helper()
	var (
		receipt domain.CommandReceipt
		snap    store.Snapshot
	)
	err := s.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		receipt, snap, err = tx.GetCommandReceipt(t.Context(), commandID)
		return err
	})
	return receipt, snap, err
}

// A receipt is a client-visible write: it commits at a new revision, reads
// back as recorded with that revision, and is never rewritten.
func TestCommandReceiptRoundTripsAtItsRevision(t *testing.T) {
	t.Parallel()
	s := openStore(t, store.Options{})
	if _, _, err := readReceipt(t, s, "cmd-refresh"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unrecorded receipt = %v, want ErrNotFound", err)
	}
	before := currentRevision(t, s)
	written, err := recordReceipt(t, s, refreshReceipt("cmd-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	if written != (store.Snapshot{EntityVersion: 1, AsOfRevision: before + 1}) {
		t.Fatalf("written snapshot = %+v, want revision %d", written, before+1)
	}
	got, snap, err := readReceipt(t, s, "cmd-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if snap != written || got.Kind() != domain.CommandKindRefreshUsage || *got.UsageRefresh != *refreshReceipt("cmd-refresh").UsageRefresh {
		t.Fatalf("read receipt = %+v at %+v", got, snap)
	}
	if _, err := recordReceipt(t, s, refreshReceipt("cmd-refresh")); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("second receipt under one command_id = %v, want ErrImmutableConflict", err)
	}
}

func TestRecordCommandReceiptRefusesAnInvalidReceipt(t *testing.T) {
	t.Parallel()
	s := openStore(t, store.Options{})
	before := currentRevision(t, s)
	if _, err := recordReceipt(t, s, domain.CommandReceipt{}); !errors.Is(err, domain.ErrCommandReceiptArms) {
		t.Fatalf("empty receipt = %v, want ErrCommandReceiptArms", err)
	}
	if after := currentRevision(t, s); after != before {
		t.Fatalf("refused receipt moved revision %d -> %d", before, after)
	}
}

// A command_id names one command of any kind: each writer refuses an id
// another kind recorded.
func TestCommandReceiptCommandIDIsUniqueAcrossKinds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedComprehensionDeps(t, ctx, s, f)
	taskID, runID := seedTaskSubmissionTarget(t, ctx, s)
	submission := domain.TaskSubmission{
		CommandID: "cmd-submitted", Origin: domain.TaskSubmissionOriginDevice,
		DeviceID: new(domain.DeviceID("device-1")), ProjectID: "project",
		SourceDigest: taskSubmissionDigest, TaskID: taskID, SpecificationRunID: runID,
		Name: domain.DisplayName{Text: "Name", Source: domain.DisplayNameSourceOperator},
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutTaskSubmission(ctx, submission) }); err != nil {
		t.Fatal(err)
	}
	stopped := currentState(t, s)
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		_, _, err := tx.StopTask(ctx, domain.StopTaskRequest{
			CommandID: "cmd-stopped", DeviceID: "device-1", TaskID: taskID, ProjectID: "project",
			ExpectedSyncEpoch: stopped.SyncEpoch, ExpectedEntityVersion: stopped.Revision,
		}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.command.CommandID, submission.CommandID, "cmd-stopped"} {
		if _, err := recordReceipt(t, s, refreshReceipt(id)); !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("receipt under recorded command %q = %v, want ErrImmutableConflict", id, err)
		}
	}

	if _, err := recordReceipt(t, s, refreshReceipt("cmd-receipt")); err != nil {
		t.Fatal(err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "cmd-receipt", DeviceID: "device-1", ItemID: "item-1", ItemVersion: 1, Action: domain.ActionOpenPR,
	})
	if err != nil {
		t.Fatal(err)
	}
	reused := submission
	reused.CommandID = "cmd-receipt"
	state := currentState(t, s)
	for name, write := range map[string]func(*store.WriteTx) error{
		"decision":   func(tx *store.WriteTx) error { return tx.PutCommand(ctx, command) },
		"submission": func(tx *store.WriteTx) error { return tx.PutTaskSubmission(ctx, reused) },
		"stop": func(tx *store.WriteTx) error {
			_, _, err := tx.StopTask(ctx, domain.StopTaskRequest{
				CommandID: "cmd-receipt", DeviceID: "device-1", TaskID: taskID, ProjectID: "project",
				ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
			}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
			return err
		},
	} {
		if err := s.Write(ctx, write); !errors.Is(err, store.ErrImmutableConflict) {
			t.Errorf("%s under a receipt's command_id = %v, want ErrImmutableConflict", name, err)
		}
	}
}

// A receipt row is re-validated on read: a body that fails its arm's rule, a
// body under another command_id, and a revision the store never reached all
// fail closed.
func TestGetCommandReceiptRefusesAnInconsistentRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name, update string
		args         []any
	}{
		{
			"invalid body", `UPDATE client_command_receipts SET body = ?`,
			[]any{`{"command_id":"cmd-refresh","device_id":"","pool":"pool-a","requested_at":"2026-01-02T03:04:05Z"}`},
		},
		{
			"foreign command_id", `UPDATE client_command_receipts SET body = ?`,
			[]any{`{"command_id":"cmd-other","device_id":"device-1","pool":"pool-a","requested_at":"2026-01-02T03:04:05Z"}`},
		},
		{"unknown kind", `UPDATE client_command_receipts SET kind = 'stop_task'`, nil},
		{"revision ahead", `UPDATE client_command_receipts SET as_of_revision = as_of_revision + 100`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := tempDBPath(t)
			s := openStoreAt(t, path, store.Options{})
			if _, err := recordReceipt(t, s, refreshReceipt("cmd-refresh")); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.ExecContext(ctx, tc.update, tc.args...); err != nil {
				t.Fatalf("corrupt receipt: %v", err)
			}
			if _, _, err := readReceipt(t, s, "cmd-refresh"); err == nil || errors.Is(err, store.ErrNotFound) {
				t.Fatalf("read corrupted receipt = %v, want an inconsistent-row error", err)
			}
		})
	}
}

func currentState(t *testing.T, s *store.Store) store.ServerState {
	t.Helper()
	var state store.ServerState
	if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		state, err = tx.ServerState(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

func currentRevision(t *testing.T, s *store.Store) int64 {
	t.Helper()
	return currentState(t, s).Revision
}
