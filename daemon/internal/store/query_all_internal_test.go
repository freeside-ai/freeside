package store

import (
	"context"
	"errors"
	"slices"
	"testing"
)

const queryAllCountSQL = `
WITH RECURSIVE n(v) AS (SELECT 1 UNION ALL SELECT v + 1 FROM n WHERE v < ?)
SELECT v FROM n ORDER BY v DESC`

func scanQueryAllInt(s scanner) (int, error) {
	var v int
	err := s.Scan(&v)
	return v, err
}

// TestQueryAll covers the shared row helper's contract: rows in query order,
// nil for zero rows, a scan error located by its 1-based row position and
// still matching errors.Is, and a query error returned unchanged.
func TestQueryAll(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	errCorrupt := errors.New("corrupt row")

	err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := queryAll(ctx, tx.tx, scanQueryAllInt, queryAllCountSQL, 3)
		if err != nil {
			t.Fatalf("queryAll: %v", err)
		}
		if want := []int{3, 2, 1}; !slices.Equal(got, want) {
			t.Errorf("queryAll = %v, want %v (query order)", got, want)
		}

		empty, err := queryAll(ctx, tx.tx, scanQueryAllInt, `SELECT 1 WHERE 0`)
		if err != nil {
			t.Fatalf("queryAll zero rows: %v", err)
		}
		if empty != nil {
			t.Errorf("queryAll zero rows = %#v, want nil", empty)
		}

		calls := 0
		_, err = queryAll(ctx, tx.tx, func(s scanner) (int, error) {
			calls++
			v, err := scanQueryAllInt(s)
			if err == nil && v == 2 {
				err = errCorrupt
			}
			return v, err
		}, queryAllCountSQL, 3)
		if !errors.Is(err, errCorrupt) {
			t.Errorf("scan error = %v, want errors.Is %v", err, errCorrupt)
		}
		if err == nil || err.Error() != "row 2: corrupt row" {
			t.Errorf("scan error = %v, want %q", err, "row 2: corrupt row")
		}
		if calls != 2 {
			t.Errorf("scan calls = %d, want 2 (stop at the failing row)", calls)
		}

		wantQueryErr := func() error {
			_, err := tx.tx.QueryContext(ctx, `SELECT * FROM no_such_table`)
			return err
		}()
		if wantQueryErr == nil {
			t.Fatal("invalid query unexpectedly succeeded")
		}
		_, err = queryAll(ctx, tx.tx, scanQueryAllInt, `SELECT * FROM no_such_table`)
		if err == nil || err.Error() != wantQueryErr.Error() {
			t.Errorf("query error = %v, want unchanged %v", err, wantQueryErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
}
