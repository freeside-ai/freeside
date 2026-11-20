package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestReadReadyShareRowsSkipsUndecodableRows covers the rows the lenient
// decode skips. The good rows' projection is checked end to end, through
// the store's item writer, by the ready-share command's tests.
func TestReadReadyShareRowsSkipsUndecodableRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	if _, err := st.db.ExecContext(ctx, `
INSERT INTO attention_items (id, project_id, conversation_id, item_type, entity_version, as_of_revision, body)
VALUES ('item-bad-json', 'proj-1', NULL, 'execution_failure', 1, 1, '{'),
       ('item-no-time', 'proj-1', NULL, 'execution_failure', 1, 1, '{"interruption_class":"exceptional"}'),
       ('item-no-class', 'proj-1', NULL, 'execution_failure', 1, 1, '{"created_at":"2026-11-10T00:00:00Z"}'),
       ('item-no-type', 'proj-1', NULL, '', 1, 1, '{"interruption_class":"exceptional","created_at":"2026-11-10T00:00:00Z"}'),
       ('item-other-project', 'proj-2', NULL, '', 1, 1, '{')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rows, err := st.ReadReadyShareRows(ctx, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Items) != 0 {
		t.Fatalf("items = %+v, want none", rows.Items)
	}
	type skip struct{ key, err string }
	var got []skip
	for _, row := range rows.Skipped {
		if row.Table != "attention_items" {
			t.Errorf("skipped %s table = %q", row.Key, row.Table)
		}
		got = append(got, skip{row.Key, row.Error})
	}
	want := []skip{
		{"item-bad-json", "body: unexpected end of JSON input"},
		{"item-no-class", "empty interruption_class"},
		{"item-no-time", "no created_at"},
		{"item-no-type", "empty item_type"},
	}
	if len(got) != len(want) {
		t.Fatalf("skipped = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].key != want[i].key || !strings.HasPrefix(got[i].err, want[i].err) {
			t.Fatalf("skipped = %+v, want %+v", got, want)
		}
	}

	all, err := st.ReadReadyShareRows(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, row := range all.Skipped {
		keys = append(keys, row.Key)
	}
	if wantKeys := []string{"item-bad-json", "item-no-class", "item-no-time", "item-no-type", "item-other-project"}; !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("unscoped skipped keys = %v, want %v", keys, wantKeys)
	}
}
