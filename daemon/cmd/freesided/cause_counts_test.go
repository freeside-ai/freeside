package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/causecount"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// causeCountsEnd is the fixture's -end. With the default 168h period, the
// period is [Nov 9, Nov 16) and the previous period [Nov 2, Nov 9).
var causeCountsEnd = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

// causeCountsFixture writes rows from all five sources and hold rows: run-1
// and run-2 in proj-1 and run-3 in proj-2. The hold history starts on Nov 5,
// inside the previous period. Attention items go through the store's writer,
// so the conclusion read sees the body shape the store stores; the other rows
// are inserted directly, including one unknown value and one undecodable row.
func causeCountsFixture(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RegisterProject(ctx, domain.Project{ID: "proj-1", Repo: "owner/repo", RepositoryID: 7})
	}); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct {
		item    domain.ItemID
		run     domain.RunID
		project domain.ProjectID
		// concluded is when the item expired; zero leaves it open.
		concluded time.Time
	}{
		{"item-1", "run-1", "proj-1", time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)},
		{"item-2", "run-2", "proj-1", time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)},
		{"item-3", "run-3", "proj-2", time.Time{}},
	} {
		putCauseCountsItem(t, ctx, st, seed.item, seed.run, seed.project, seed.concluded)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close() //nolint:errcheck // test cleanup
	for _, statement := range []string{
		`INSERT INTO execution_admissions
		   (invocation_id, id, run_id, stage_id, attempt_id, operating_mode, admitted_at, body)
		 VALUES ('inv-1', 'adm-1', 'run-1', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}'),
		        ('inv-2', 'adm-2', 'run-1', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}'),
		        ('inv-3', 'adm-3', 'run-2', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}'),
		        ('inv-4', 'adm-4', 'run-2', 's', 'a', 'attended_dev', '2026-11-03T00:00:00Z', '{}'),
		        ('inv-5', 'adm-5', 'run-3', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}'),
		        ('inv-6', 'adm-6', 'run-1', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}')`,
		`INSERT INTO execution_outcomes
		   (invocation_id, admission_id, status, summary, cause, recorded_at, body)
		 VALUES ('inv-1', 'adm-1', 'failed', 's', 'quota', '2026-11-10T01:00:00Z', '{}'),
		        ('inv-2', 'adm-2', 'failed', 's', 'quota', '2026-11-14T01:00:00Z', '{}'),
		        ('inv-3', 'adm-3', 'failed', 's', 'quota', '2026-11-11T01:00:00Z', '{}'),
		        ('inv-4', 'adm-4', 'failed', 's', 'quota', '2026-11-03T01:00:00Z', '{}'),
		        ('inv-5', 'adm-5', 'failed', 's', 'solar_flare', '2026-11-12T01:00:00Z', '{}'),
		        ('inv-6', 'adm-6', 'failed', 's', 'quota', 'yesterday', '{}')`,
		`INSERT INTO run_milestones (run_id, kind, invocation_id, terminal, outcome, reason, recorded_at)
		 VALUES ('run-1', 'terminal_recorded', 'inv-1', 'completed', NULL, NULL, '2026-11-14T00:00:00Z'),
		        ('run-1', 'terminal_recorded', 'inv-2', 'completed', NULL, NULL, '2026-11-15T00:00:00Z'),
		        ('run-2', 'terminal_recorded', 'inv-3', 'failed', NULL, NULL, '2026-11-16T00:00:00Z'),
		        ('run-3', 'terminal_recorded', 'inv-5', 'completed', NULL, NULL, '2026-11-09T00:00:00Z'),
		        ('run-1', 'publication_blocked', 'inv-2', NULL, NULL, 'trust_blocked', '2026-11-13T00:00:00Z')`,
		`INSERT INTO review_failures
		   (invocation_id, run_id, round, failure_class, observed_at, body_digest, body)
		 VALUES ('rev-1', 'run-1', 1, 'transient', '2026-11-10T00:00:00Z', 'sha256:x', '{}'),
		        ('rev-2', 'run-2', 1, 'transient', '2026-11-08T00:00:00Z', 'sha256:x', '{}')`,
		// run-2's quota hold spans the period boundary; run-1's solar_flare
		// hold is a reason this build doesn't know; span 6's end doesn't parse.
		`INSERT INTO run_hold_spans
		   (id, run_id, invocation_id, reason, first_observed_at, last_observed_at, cleared_at)
		 VALUES (1, 'run-1', NULL, 'machine_capacity', '2026-11-10T00:00:00Z', '2026-11-10T00:00:00Z', '2026-11-10T12:00:00Z'),
		        (2, 'run-2', NULL, 'machine_capacity', '2026-11-11T00:00:00Z', '2026-11-11T00:00:00Z', '2026-11-11T06:00:00Z'),
		        (3, 'run-3', NULL, 'machine_capacity', '2026-11-12T00:00:00Z', '2026-11-12T00:00:00Z', '2026-11-12T01:00:00Z'),
		        (4, 'run-2', NULL, 'quota_window_exhausted', '2026-11-08T00:00:00Z', '2026-11-08T00:00:00Z', '2026-11-10T00:00:00Z'),
		        (5, 'run-1', NULL, 'solar_flare', '2026-11-06T00:00:00Z', '2026-11-06T00:00:00Z', '2026-11-07T00:00:00Z'),
		        (6, 'run-1', NULL, 'scope_conflict', '2026-11-13T00:00:00Z', '2026-11-13T00:00:00Z', 'yesterday')`,
		// Still open: it runs to the real now, past -end, so it clips to -end.
		`INSERT INTO run_hold_observations (run_id, invocation_id, reason, first_observed_at, last_observed_at)
		 VALUES ('run-1', NULL, 'scope_conflict', '2026-11-15T12:00:00Z', '2026-11-15T12:00:00Z')`,
		`UPDATE schema_migrations SET applied_at = '2026-11-05T00:00:00Z' WHERE version = 145`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed: %v\n%s", err, statement)
		}
	}
	return dbPath
}

func putCauseCountsItem(
	t *testing.T, ctx context.Context, st *store.Store,
	id domain.ItemID, runID domain.RunID, project domain.ProjectID, concluded time.Time,
) {
	t.Helper()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: project,
		Subject:           domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:              domain.AttentionExecutionFailure,
		Priority:          domain.PriorityNormal,
		Reason:            "waiting on an external dependency",
		RequestedDecision: []domain.Action{},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := storetest.BindSubject(ctx, tx, &item); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
	if concluded.IsZero() {
		return
	}
	item.Status, item.ItemVersion = domain.StatusExpired, 2
	conclusion, err := domain.NewDaemonConclusion(domain.ConcludedByExpiry, concluded, "Expired by the test.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if item, err = item.WithConclusion(conclusion); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatalf("close %s: %v", id, err)
	}
}

func runCauseCounts(t *testing.T, args ...string) causecount.Report {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runCauseCountsCommand(context.Background(), args, &stdout, &stderr); err != nil {
		t.Fatalf("cause-counts %v: %v; stderr=%s", args, err, stderr.String())
	}
	var report causecount.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("printed report is invalid: %v", err)
	}
	return report
}

func day(d int) *time.Time {
	t := time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC)
	return &t
}

func dayAt(d, hour int) *time.Time {
	t := time.Date(2026, 11, d, hour, 0, 0, 0, time.UTC)
	return &t
}

func TestCauseCountsCommandCountsEverySource(t *testing.T) {
	t.Parallel()
	dbPath := causeCountsFixture(t)
	end := causeCountsEnd.Format(time.RFC3339)

	report := runCauseCounts(t, "-db", dbPath, "-end", end)
	wantPeriod := causecount.Window{Start: *day(9), End: causeCountsEnd}
	if report.ProjectID != nil || report.Period != wantPeriod || report.PreviousPeriod != wantPeriod.Previous() {
		t.Fatalf("report header = %v %+v %+v", report.ProjectID, report.Period, report.PreviousPeriod)
	}
	want := []causecount.Count{
		{
			Source: causecount.SourceFailureCause, Cause: "quota", Known: true, Occurrences: 3, Runs: 2,
			FirstSeen: dayAt(10, 1), LastSeen: dayAt(14, 1), PreviousOccurrences: 1,
		},
		{
			Source: causecount.SourceFailureCause, Cause: "solar_flare", Known: false, Occurrences: 1, Runs: 1,
			FirstSeen: dayAt(12, 1), LastSeen: dayAt(12, 1),
		},
		// run-1 recorded two terminals, so occurrences exceed runs. run-2's
		// failed terminal falls on -end, which belongs to the next period.
		{
			Source: causecount.SourceTerminal, Cause: "completed", Known: true, Occurrences: 3, Runs: 2,
			FirstSeen: day(9), LastSeen: day(15),
		},
		{
			Source: causecount.SourcePublicationBlocked, Cause: "trust_blocked", Known: true, Occurrences: 1, Runs: 1,
			FirstSeen: day(13), LastSeen: day(13),
		},
		{
			Source: causecount.SourceReviewFailure, Cause: "transient", Known: true, Occurrences: 1, Runs: 1,
			FirstSeen: day(10), LastSeen: day(10), PreviousOccurrences: 1,
		},
		{
			Source: causecount.SourceAttentionConclusion, Cause: "expired", Known: true, Occurrences: 1, Runs: 1,
			FirstSeen: day(12), LastSeen: day(12), PreviousOccurrences: 1,
		},
	}
	if !reflect.DeepEqual(report.Causes, want) {
		got, _ := json.MarshalIndent(report.Causes, "", "  ")
		t.Fatalf("causes =\n%s", got)
	}
	if len(report.Skipped) != 2 || report.Skipped[0].Table != "execution_outcomes" ||
		report.Skipped[0].Row != "inv-6" || !strings.HasPrefix(report.Skipped[0].Error, "recorded_at:") ||
		report.Skipped[1].Table != "run_hold_spans" || report.Skipped[1].Row != "6" ||
		!strings.HasPrefix(report.Skipped[1].Error, "cleared_at:") {
		t.Fatalf("skipped = %+v", report.Skipped)
	}
	const hour = 3600
	wantHolds := causecount.Holds{
		RecordedSince:  *day(5),
		PeriodCoverage: causecount.CoverageFull, PreviousPeriodCoverage: causecount.CoveragePartial,
		Reasons: []causecount.HoldCount{
			holdCount("machine_capacity", true, 3, 3, 19*hour, 0, 0, 0),
			holdCount("quota_window_exhausted", true, 1, 1, 24*hour, 1, 1, 24*hour),
			holdCount("scope_conflict", true, 1, 1, 12*hour, 0, 0, 0),
			holdCount("solar_flare", false, 0, 0, 0, 1, 1, 24*hour),
		},
	}
	assertHolds(t, "holds", report.Holds, wantHolds)

	// -project keeps proj-1's rows: run-3's unknown failure cause and its
	// completed terminal drop out.
	scoped := runCauseCounts(t, "-db", dbPath, "-end", end, "-project", "proj-1")
	if scoped.ProjectID == nil || *scoped.ProjectID != "proj-1" {
		t.Fatalf("scoped project = %v", scoped.ProjectID)
	}
	var scopedWant []causecount.Count
	for _, c := range want {
		switch {
		case c.Cause == "solar_flare":
			continue
		case c.Source == causecount.SourceTerminal:
			c.Occurrences, c.Runs, c.FirstSeen = 2, 1, day(14)
		}
		scopedWant = append(scopedWant, c)
	}
	if !reflect.DeepEqual(scoped.Causes, scopedWant) {
		got, _ := json.MarshalIndent(scoped.Causes, "", "  ")
		t.Fatalf("scoped causes =\n%s", got)
	}
	// run-3's machine capacity hold drops out too.
	scopedHolds := wantHolds
	scopedHolds.Reasons = slices.Clone(wantHolds.Reasons)
	scopedHolds.Reasons[0] = holdCount("machine_capacity", true, 2, 2, 18*hour, 0, 0, 0)
	assertHolds(t, "scoped holds", scoped.Holds, scopedHolds)

	// -period moves both windows: the last 24h holds only run-1's second
	// terminal, and the day before it its first and inv-2's quota failure.
	lastDay := runCauseCounts(t, "-db", dbPath, "-end", end, "-period", "24h")
	lastDayWant := []causecount.Count{
		{Source: causecount.SourceFailureCause, Cause: "quota", Known: true, PreviousOccurrences: 1},
		{
			Source: causecount.SourceTerminal, Cause: "completed", Known: true, Occurrences: 1, Runs: 1,
			FirstSeen: day(15), LastSeen: day(15), PreviousOccurrences: 1,
		},
	}
	if !reflect.DeepEqual(lastDay.Causes, lastDayWant) {
		got, _ := json.MarshalIndent(lastDay.Causes, "", "  ")
		t.Fatalf("24h causes =\n%s", got)
	}
	// Both 24h windows are recorded, and only the open hold falls in them.
	assertHolds(t, "24h holds", lastDay.Holds, causecount.Holds{
		RecordedSince:  *day(5),
		PeriodCoverage: causecount.CoverageFull, PreviousPeriodCoverage: causecount.CoverageFull,
		Reasons: []causecount.HoldCount{holdCount("scope_conflict", true, 1, 1, 12*hour, 0, 0, 0)},
	})
}

func holdCount(reason string, known bool, holds, runs int, held int64, prevHolds, prevRuns int, prevHeld int64) causecount.HoldCount {
	return causecount.HoldCount{
		Reason: reason, Known: known, Holds: &holds, Runs: &runs, HeldSeconds: &held,
		PreviousHolds: &prevHolds, PreviousRuns: &prevRuns, PreviousHeldSeconds: &prevHeld,
	}
}

func assertHolds(t *testing.T, name string, got, want causecount.Holds) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("%s =\n%s\nwant\n%s", name, gotJSON, wantJSON)
	}
}

func TestCauseCountsCommandRefusesBadArguments(t *testing.T) {
	t.Parallel()
	dbPath := causeCountsFixture(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"unknown project": {[]string{"-db", dbPath, "-project", "proj-2"}, `unknown project "proj-2"`},
		"missing db":      {[]string{}, "-db is required"},
		"zero period":     {[]string{"-db", dbPath, "-period", "0s"}, "-period must be positive"},
		"bad end":         {[]string{"-db", dbPath, "-end", "tomorrow"}, "-end:"},
		"positional":      {[]string{"-db", dbPath, "extra"}, "unexpected positional arguments"},
	} {
		var stdout bytes.Buffer
		err := runCauseCountsCommand(context.Background(), tc.args, &stdout, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tc.want)
		}
		if name == "unknown project" && !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown project error %v does not wrap store.ErrNotFound", err)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: printed %q", name, stdout.String())
		}
	}
}
