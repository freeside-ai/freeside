package integration_test

import (
	"database/sql"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A session interrupted after admission and before publication resumes from a
// pre-publication checkpoint, and the same run reports ready once it publishes.
func TestRealRunPrePublicationCheckpoint(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		checkpoint, err := readRealRunCheckpoint(p.ctx, tx, p.runID, true)
		if err != nil {
			return err
		}
		if checkpoint.State != "pre-publication" || checkpoint.Binding != (domain.ReadyItemPRBinding{}) ||
			checkpoint.Review != nil || checkpoint.Completion != nil {
			t.Fatalf("unpublished run checkpoint = %#v, want an empty pre-publication checkpoint", checkpoint)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertRealRunCheckpoint(t, p, false, "")

	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("publish original: %#v, %v", result, err)
	}
	assertRealRunCheckpoint(t, p, true, "ready")
}

// Once a run has published, neither a missing binding nor a missing ready item
// makes it pre-publication. The second case models a publish_blocked hold after
// publication, which records no ready item, so only the committed publication
// intent refuses it.
func TestRealRunPrePublicationCheckpointRejectsPublishedRun(t *testing.T) {
	for _, damage := range []struct {
		name    string
		queries []string
	}{
		{"missing-binding", []string{"DELETE FROM ready_item_pr_bindings WHERE item_id = ?"}},
		{"no-ready-item", []string{
			"DELETE FROM ready_item_pr_bindings WHERE item_id = ?",
			"DELETE FROM attention_items WHERE id = ?",
		}},
	} {
		t.Run(damage.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.startAndRecordExport(t)
			if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
				t.Fatalf("publish original: %#v, %v", result, err)
			}
			raw, err := sql.Open("sqlite", p.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := raw.Close(); err != nil {
					t.Error(err)
				}
			}()
			for _, query := range damage.queries {
				if _, err := raw.ExecContext(p.ctx, query, string(domain.ProductionReadyItemID(p.runID))); err != nil {
					t.Fatal(err)
				}
			}
			assertRealRunCheckpoint(t, p, true, "")
		})
	}
}

// An unpublished run whose publication slot lost its reservation is damaged
// history: a free slot cannot prove that publication never began, because the
// same damage could have taken the committed intent.
func TestRealRunPrePublicationCheckpointRejectsFreeSlot(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	reservation, err := publish.NewReservation(domain.ProductionPublicationInvocationID(p.runID), p.runID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := reservation.Key()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	}()
	if result, err := raw.ExecContext(p.ctx, "DELETE FROM outbox WHERE idempotency_key = ? AND kind = ?", key, publish.IntentKindReservation); err != nil {
		t.Fatal(err)
	} else if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("deleted %d reservation rows, %v; want 1", n, err)
	}
	assertRealRunCheckpoint(t, p, true, "")
}
