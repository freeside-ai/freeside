package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// seedExhaustedArgs seeds pool-a's five_hour window for gpt-test.
func seedExhaustedArgs(dbPath string, resetsAt time.Time) []string {
	return []string{
		"seed-exhausted", "-db", dbPath, "-pool", "pool-a", "-window", "five_hour",
		"-model", "gpt-test", "-resets-at", resetsAt.Format(time.RFC3339),
	}
}

// wantSeededMark checks that pool-a's record marks five_hour exhausted for
// gpt-test until resetsAt, from a reading the record says was seeded, and
// that the window limits no other model.
func wantSeededMark(t *testing.T, st *store.Store, resetsAt time.Time) {
	t.Helper()
	ctx := t.Context()
	var record domain.PoolUsageObservation
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetPoolUsageObservation(ctx, "pool-a")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if record.LastAttempt == nil || record.LastAttempt.Status != domain.CollectionAttemptSucceeded ||
		record.LastAttempt.Detail != seededUsageDetail {
		t.Fatalf("last attempt = %+v, want a seeded success", record.LastAttempt)
	}
	now := time.Now().UTC()
	marks := record.ExhaustedMarksForModel("gpt-test", now)
	if len(marks) != 1 || marks[0].Window != "five_hour" || marks[0].Source != domain.ExhaustedMarkUsageReading ||
		marks[0].ResetsAt == nil || !marks[0].ResetsAt.Equal(resetsAt) {
		t.Fatalf("gpt-test marks = %+v, want five_hour until %s", marks, resetsAt)
	}
	if other := record.ExhaustedMarksForModel("gpt-other", now); len(other) != 0 {
		t.Fatalf("gpt-other marks = %+v, want the window scoped to gpt-test", other)
	}
}

func TestAuthSeedExhaustedMarksAModelWindow(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	seedUsagePools(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	resetsAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	output, err := runAuthUsage(t, t.Context(), seedExhaustedArgs(dbPath, resetsAt)...)
	if err != nil {
		t.Fatal(err)
	}
	var printed domain.PoolUsageObservation
	if err := json.Unmarshal([]byte(output), &printed); err != nil || printed.Pool != "pool-a" ||
		len(printed.Windows) != 1 || printed.Windows[0].EnrollmentID != "enroll-first" {
		t.Fatalf("printed record = %s (%v), want pool-a's seeded reading under its collector", output, err)
	}
	wantSeededMark(t, storetest.Open(t, dbPath, store.Options{}), resetsAt)

	if _, err := runAuthUsage(t, t.Context(), seedExhaustedArgs(dbPath, time.Now().Add(-time.Minute))...); err == nil ||
		!strings.Contains(err.Error(), "is not in the future") {
		t.Fatalf("a past reset = %v, want it refused", err)
	}
	args := seedExhaustedArgs(dbPath, resetsAt)
	if _, err := runAuthUsage(t, t.Context(), args[:len(args)-2]...); err == nil ||
		!strings.Contains(err.Error(), "-resets-at is required") {
		t.Fatalf("no reset = %v, want it required", err)
	}
}

func TestAuthSeedExhaustedUsesTheRunningDaemon(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	seedUsagePools(t, h.store)
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("unexpected direct store open")
		}))
	resetsAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	if output, err := runAuthUsage(t, ctx, seedExhaustedArgs(dbPath, resetsAt)...); err != nil {
		t.Fatalf("seed-exhausted = %v (%s)", err, output)
	}
	wantSeededMark(t, h.store, resetsAt)
	if _, err := runAuthUsage(t, ctx, seedExhaustedArgs(dbPath, time.Now().Add(-time.Minute))...); err == nil ||
		!strings.Contains(err.Error(), "is not in the future") {
		t.Fatalf("forwarded past reset = %v, want it refused", err)
	}
}
