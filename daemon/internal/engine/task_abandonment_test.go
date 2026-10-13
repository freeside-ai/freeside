package engine

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestRetireAbandonedTaskItemsLifecycle(t *testing.T) {
	for _, lifecycle := range []string{"unstarted", "active", "abandoned", "readmitted", "finished"} {
		t.Run(lifecycle, func(t *testing.T) {
			ctx := t.Context()
			dbPath := filepath.Join(t.TempDir(), "store.db")
			st := storetest.Open(t, dbPath, store.Options{})
			run := domain.Run{ID: "abandon-run", ProjectID: "abandon-project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutRun(ctx, run); err != nil {
					return err
				}
				var err error
				run, err = tx.GetRun(ctx, run.ID)
				if err != nil {
					return err
				}
				if lifecycle == "unstarted" {
					return nil
				}
				if err := tx.RecordTaskStart(ctx, run.ID, time.Now().UTC()); err != nil {
					return err
				}
				if lifecycle == "active" {
					return nil
				}
				if _, err := tx.AbandonTask(ctx, run.TaskID, time.Now().UTC()); err != nil {
					return err
				}
				if lifecycle == "readmitted" {
					next := run
					next.ID = "abandon-next-run"
					if err := tx.PutRun(ctx, next); err != nil {
						return err
					}
					return tx.AdmitTaskStart(ctx, next.ID, time.Now().UTC(), 2)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if lifecycle == "finished" {
				// Seed only the completion binding consulted by lifecycle reads;
				// publication evidence is covered by the work-unit store tests.
				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				at := time.Now().UTC().Format(time.RFC3339Nano)
				if _, err := db.ExecContext(ctx, `INSERT INTO work_unit_declarations VALUES ('workunit-abandon-run', 'abandon-run', 'abandon-project', '{}', ?)`, at); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO work_unit_completions VALUES ('workunit-abandon-run', '{}', ?)`, at); err != nil {
					t.Fatal(err)
				}
				if err := st.Write(ctx, func(tx *store.WriteTx) error {
					return tx.RecordTaskCompletion(ctx, run.ID, "workunit-abandon-run", time.Now().UTC())
				}); err != nil {
					t.Fatal(err)
				}
			}
			item, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID: "execution-failure-abandon", ProjectID: run.ProjectID,
				Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID},
				Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh, Reason: "Recovery needed.",
				RequestedDecision: []domain.Action{domain.ActionStop}, ItemVersion: 1, Status: domain.StatusOpen,
				InterruptionClass: domain.InterruptionExceptional,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
				t.Fatal(err)
			}
			var before domain.AttentionItem
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				before, err = tx.GetAttentionItem(ctx, item.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				ids, err := RetireAbandonedTaskItems(ctx, tx, run.TaskID)
				if err != nil {
					return err
				}
				wantCount := 0
				if lifecycle == "abandoned" {
					wantCount = 1
				}
				if len(ids) != wantCount {
					t.Fatalf("retired = %v for %s", ids, lifecycle)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if lifecycle != "abandoned" {
				if err := st.Read(ctx, func(tx *store.ReadTx) error {
					after, err := tx.GetAttentionItem(ctx, item.ID)
					if err == nil && !reflect.DeepEqual(before, after) {
						t.Fatal("non-abandoned task item changed")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
