package store

import (
	"strings"
	"testing"
	"time"
)

// TestParseTime pins the store's one RFC3339Nano reader: it normalizes any
// input offset to the same instant in UTC (so callers never re-normalize),
// round-trips formatTime, and reports an unparsable column with a uniform,
// value-free prefix over the standard library's error (issue #553).
func TestParseTime(t *testing.T) {
	t.Parallel()

	t.Run("normalizes an offset input to UTC", func(t *testing.T) {
		t.Parallel()
		want := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
		offset := want.In(time.FixedZone("+0200", 2*60*60)).Format(time.RFC3339Nano)
		got, err := parseTime(offset)
		if err != nil {
			t.Fatalf("parseTime(%q): %v", offset, err)
		}
		if got.Location() != time.UTC {
			t.Errorf("location = %v, want UTC", got.Location())
		}
		if !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, want the same instant as %v", offset, got, want)
		}
	})

	t.Run("round-trips formatTime with nanoseconds", func(t *testing.T) {
		t.Parallel()
		want := time.Date(2026, 1, 2, 15, 4, 5, 123456789, time.UTC)
		got, err := parseTime(formatTime(want))
		if err != nil {
			t.Fatalf("parseTime(formatTime): %v", err)
		}
		if got.Location() != time.UTC || !got.Equal(want) {
			t.Errorf("round-trip = %v (%v), want %v (UTC)", got, got.Location(), want)
		}
	})

	t.Run("rejects an unparsable column with the uniform prefix", func(t *testing.T) {
		t.Parallel()
		_, err := parseTime("not-a-timestamp")
		if err == nil {
			t.Fatal("parseTime of garbage returned a nil error")
		}
		if !strings.Contains(err.Error(), "parse RFC3339Nano timestamp") {
			t.Errorf("error = %q, want the uniform parse prefix", err)
		}
	})
}
