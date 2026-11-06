package store

import (
	"context"
	"testing"
)

// TestItemWithRetiredBaseFreshnessReads: a ready item the retired
// base-advance watch (#2645) stamped keeps a base_freshness object in its
// stored body. The row still reads, lists, and converges on an unchanged
// replay without a version advance, so the retirement needs no rewrite of
// item rows.
func TestItemWithRetiredBaseFreshnessReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	item := degradedReadyDetailItem(t)
	st := openTestStore(t)
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE attention_items SET body = json_set(body, '$.base_freshness', json(?)) WHERE id = ?`,
		`{"base_ref":"main","admitted_base_sha":"cafebabe","observed_base_sha":"deadbeef",`+
			`"advanced":true,"observed_at":"2026-02-03T05:00:00Z"}`, item.ID,
	); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if got.ID != item.ID || got.ItemVersion != item.ItemVersion {
			t.Fatalf("read item %s version %d, want %s version %d",
				got.ID, got.ItemVersion, item.ID, item.ItemVersion)
		}
		listed, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if len(listed) != 1 || listed[0].Value.ID != item.ID {
			t.Fatalf("listed %d items, want the stamped one", len(listed))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("unchanged replay against the stamped row: %v", err)
	}
	var version int
	if err := st.db.QueryRowContext(ctx,
		`SELECT entity_version FROM attention_items WHERE id = ?`, item.ID,
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("entity_version = %d after an unchanged replay, want 1", version)
	}
}
