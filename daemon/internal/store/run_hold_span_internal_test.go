package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// rewindRunHoldSpans returns a store at head to the shape a daemon at 0144
// holds: no hold history.
func rewindRunHoldSpans(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE run_hold_spans`,
		`DELETE FROM schema_migrations WHERE name = '0145_run_hold_spans.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func countRunHoldSpans(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM run_hold_spans`).Scan(&n); err != nil {
		t.Fatalf("count run hold spans: %v", err)
	}
	return n
}

// TestRunHoldSpanSkipsUnusableCurrentRow: the stored current hold is display
// state, so a row that can't make a valid span ends as it would without the
// history, with no span and no error, whichever way the hold ends.
func TestRunHoldSpanSkipsUnusableCurrentRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := formatTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	drain := string(domain.HoldUpdateDrain)
	type currentRow struct{ reason, first, last string }
	rows := map[string]currentRow{
		"unparsable first instant": {drain, "not-a-time", at},
		"unparsable last instant":  {drain, at, "not-a-time"},
		"retired reason":           {"retired_reason", at, at},
	}
	// Each end names the reason the current row holds afterwards; "" is no
	// row. The two cause-scoped clears can't name a retired reason, so they
	// leave that row as it stands.
	leftUnlessDrain := func(row currentRow) string {
		if row.reason == drain {
			return ""
		}
		return row.reason
	}
	ends := map[string]struct {
		end  func(tx *WriteTx) error
		left func(row currentRow) string
	}{
		"clear": {
			func(tx *WriteTx) error { return tx.ClearRunHold(ctx, "run-1") },
			func(currentRow) string { return "" },
		},
		"clear cause": {
			func(tx *WriteTx) error { return tx.ClearRunHoldCause(ctx, "run-1", domain.HoldUpdateDrain) },
			leftUnlessDrain,
		},
		"drain lift": {
			func(tx *WriteTx) error { return tx.ClearUpdateDrainHolds(ctx) },
			leftUnlessDrain,
		},
		"replace": {
			func(tx *WriteTx) error {
				later := time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC)
				return tx.RecordRunHold(ctx, domain.RunHoldObservation{
					RunID: "run-1", Reason: domain.HoldTrustBlocked,
					FirstObservedAt: later, LastObservedAt: later,
				})
			},
			func(currentRow) string { return string(domain.HoldTrustBlocked) },
		},
	}
	for rowName, row := range rows {
		for endName, end := range ends {
			t.Run(rowName+"/"+endName, func(t *testing.T) {
				t.Parallel()
				s := seedObservationStore(t)
				if _, err := s.db.ExecContext(ctx,
					`INSERT INTO run_hold_observations
					   (run_id, reason, first_observed_at, last_observed_at)
					 VALUES ('run-1', ?, ?, ?)`, row.reason, row.first, row.last); err != nil {
					t.Fatalf("seed current hold: %v", err)
				}
				if err := s.Write(ctx, end.end); err != nil {
					t.Fatalf("end hold: %v", err)
				}
				if n := countRunHoldSpans(t, s); n != 0 {
					t.Errorf("wrote %d spans from an unusable current row, want 0", n)
				}
				var left string
				err := s.db.QueryRowContext(ctx,
					`SELECT reason FROM run_hold_observations WHERE run_id = 'run-1'`).Scan(&left)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("read current hold: %v", err)
				}
				if want := end.left(row); left != want {
					t.Errorf("current hold reason = %q, want %q", left, want)
				}
			})
		}
	}
}

// TestRunHoldSpanReadsFailOnInvalidRow: a stored span that doesn't validate
// fails both list reads.
func TestRunHoldSpanReadsFailOnInvalidRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	first := formatTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	cases := []struct {
		name, reason, cleared string
		want                  error
	}{
		{"unknown reason", "made_up_reason", first, domain.ErrInvalidRunHoldReason},
		{
			"cleared before last observation",
			string(domain.HoldOperationStopped),
			formatTime(time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)),
			domain.ErrTimestampOutOfOrder,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := seedObservationStore(t)
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO run_hold_spans
				   (run_id, reason, first_observed_at, last_observed_at, cleared_at)
				 VALUES ('run-1', ?, ?, ?, ?)`, tc.reason, first, first, tc.cleared); err != nil {
				t.Fatalf("seed span: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.ListRunHoldSpans(ctx, "run-1")
				return err
			})
			if !errors.Is(err, tc.want) {
				t.Errorf("ListRunHoldSpans() = %v, want %v", err, tc.want)
			}
			err = s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.ListRunHoldSpansBetween(ctx, time.Time{}, time.Now())
				return err
			})
			if !errors.Is(err, tc.want) {
				t.Errorf("ListRunHoldSpansBetween() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRunHoldSpanRoundTripsEveryReason: the reason column has no CHECK list,
// so every member of the vocabulary must round-trip through the table.
func TestRunHoldSpanRoundTripsEveryReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := seedObservationStore(t)
	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cleared := first.Add(time.Minute)
	var want []domain.RunHoldSpan
	for _, reason := range domain.AllRunHoldReasons {
		ended := endedRunHold{
			runID: "run-1", reason: string(reason), first: formatTime(first), last: formatTime(first),
		}
		if err := s.Write(ctx, func(tx *WriteTx) error {
			return tx.recordRunHoldSpan(ctx, ended, cleared)
		}); err != nil {
			t.Fatalf("record span %s: %v", reason, err)
		}
		want = append(want, domain.RunHoldSpan{
			RunID: "run-1", Reason: reason,
			FirstObservedAt: first, LastObservedAt: first, ClearedAt: &cleared,
		})
	}
	var got []domain.RunHoldSpan
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.ListRunHoldSpans(ctx, "run-1")
		return err
	}); err != nil {
		t.Fatalf("list run hold spans: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d spans, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Reason != want[i].Reason || !got[i].ClearedAt.Equal(cleared) {
			t.Errorf("span %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
