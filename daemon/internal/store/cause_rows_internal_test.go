package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// seedCauseRows writes rows for every source ReadCauseRows reads: run-1 in
// proj-1 and run-2 in proj-2, with one unknown value and one undecodable row
// per table. The concluded attention item goes through the store's own writer,
// so the read's body projection is checked against the shape the store
// actually stores.
func seedCauseRows(t *testing.T, ctx context.Context, st *Store, concludedAt time.Time) {
	t.Helper()
	concluded := attentionItemForRun(t, "item-concluded", "run-1")
	putItem(t, ctx, st, &concluded)
	open := attentionItemForRun(t, "item-open", "run-2")
	open.ProjectID = "proj-2"
	putItem(t, ctx, st, &open)

	closed := concluded
	closed.Status, closed.ItemVersion = domain.StatusExpired, 2
	conclusion, err := domain.NewDaemonConclusion(domain.ConcludedByExpiry, concludedAt, "Expired by the test.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if closed, err = closed.WithConclusion(conclusion); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, closed) }); err != nil {
		t.Fatalf("close item: %v", err)
	}

	for _, statement := range []string{
		`INSERT INTO execution_admissions
		   (invocation_id, id, run_id, stage_id, attempt_id, operating_mode, admitted_at, body)
		 VALUES ('inv-1', 'adm-1', 'run-1', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}'),
		        ('inv-2', 'adm-2', 'run-1', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}'),
		        ('inv-3', 'adm-3', 'run-2', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}'),
		        ('inv-4', 'adm-4', 'run-1', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}'),
		        ('inv-5', 'adm-5', 'run-1', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}'),
		        ('inv-6', 'adm-6', 'run-1', 's', 'a', 'attended_dev', '2026-11-10T00:00:00Z', '{}')`,
		`INSERT INTO execution_outcomes
		   (invocation_id, admission_id, status, summary, cause, recorded_at, body)
		 VALUES ('inv-1', 'adm-1', 'failed', 's', 'quota', '2026-11-10T01:00:00Z', '{}'),
		        ('inv-2', 'adm-2', 'failed', 's', 'solar_flare', '2026-11-10T02:00:00Z', '{}'),
		        ('inv-3', 'adm-3', 'failed', 's', 'quota', '2026-11-10T03:00:00Z', '{}'),
		        ('inv-4', 'adm-4', 'failed', 's', 'quota', 'yesterday', '{}'),
		        ('inv-5', 'adm-5', 'failed', 's', '', '2026-11-10T05:00:00Z', '{}'),
		        ('inv-6', 'adm-6', 'succeeded', 's', '', '2026-11-10T06:00:00Z', '{}')`,
		`INSERT INTO run_milestones (id, run_id, kind, invocation_id, terminal, outcome, reason, recorded_at)
		 VALUES (1, 'run-1', 'terminal_recorded', 'inv-1', 'failed', NULL, NULL, '2026-11-11T01:00:00Z'),
		        (2, 'run-2', 'terminal_recorded', 'inv-3', 'failed', NULL, NULL, '2026-11-11T02:00:00Z'),
		        (3, 'run-missing', 'terminal_recorded', 'inv-8', 'canceled', NULL, NULL, '2026-11-11T03:00:00Z'),
		        (4, 'run-1', 'terminal_recorded', 'inv-9', NULL, NULL, NULL, '2026-11-11T04:00:00Z'),
		        (5, 'run-1', 'publication_blocked', 'inv-1', NULL, NULL, 'trust_blocked', '2026-11-11T05:00:00Z'),
		        (7, 'run-1', 'terminal_recorded', 'inv-2', 'completed', NULL, NULL, '2026-11-11T07:00:00Z'),
		        (6, 'run-1', 'execution_outcome_recorded', 'inv-1', NULL, 'failed', NULL, '2026-11-11T06:00:00Z')`,
		`INSERT INTO review_failures
		   (invocation_id, run_id, round, failure_class, observed_at, body_digest, body)
		 VALUES ('rev-1', 'run-1', 1, 'quota', '2026-11-12T01:00:00Z', 'sha256:x', '{}'),
		        ('rev-2', 'run-2', 1, 'transient', 'not-a-time', 'sha256:x', '{}')`,
		`INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body)
		 VALUES ('item-bad', 'proj-1', NULL, 1, 1, '{'),
		        ('item-no-time', 'proj-1', NULL, 1, 1, '{"conclusion":{"cause":"expired"}}')`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed: %v\n%s", err, statement)
		}
	}
}

func TestReadCauseRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	concludedAt := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	seedCauseRows(t, ctx, st, concludedAt)

	ts := func(s string) time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	all, err := st.ReadCauseRows(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	want := CauseRows{
		FailureCauses: []CauseRow{
			{Key: "inv-1", Value: "quota", RunID: "run-1", At: ts("2026-11-10T01:00:00Z")},
			{Key: "inv-2", Value: "solar_flare", RunID: "run-1", At: ts("2026-11-10T02:00:00Z")},
			{Key: "inv-3", Value: "quota", RunID: "run-2", At: ts("2026-11-10T03:00:00Z")},
		},
		Terminals: []CauseRow{
			{Key: "1", Value: "failed", RunID: "run-1", At: ts("2026-11-11T01:00:00Z")},
			{Key: "2", Value: "failed", RunID: "run-2", At: ts("2026-11-11T02:00:00Z")},
			{Key: "3", Value: "canceled", RunID: "run-missing", At: ts("2026-11-11T03:00:00Z")},
			{Key: "7", Value: "completed", RunID: "run-1", At: ts("2026-11-11T07:00:00Z")},
		},
		PublicationBlocks: []CauseRow{
			{Key: "5", Value: "trust_blocked", RunID: "run-1", At: ts("2026-11-11T05:00:00Z")},
		},
		ReviewFailures: []CauseRow{
			{Key: "rev-1", Value: "quota", RunID: "run-1", At: ts("2026-11-12T01:00:00Z")},
		},
		AttentionConclusions: []CauseRow{
			{Key: "item-concluded", Value: "expired", RunID: "run-1", At: concludedAt},
		},
	}
	wantSkipped := []struct{ table, key, err string }{
		{"execution_outcomes", "inv-4", "recorded_at: parse RFC3339Nano timestamp"},
		{"execution_outcomes", "inv-5", "empty cause"},
		{"run_milestones", "4", "empty cause"},
		{"review_failures", "rev-2", "observed_at: parse RFC3339Nano timestamp"},
		{"attention_items", "item-bad", "body: unexpected end of JSON input"},
		{"attention_items", "item-no-time", "conclusion has no concluded_at"},
	}
	skipped := all.Skipped
	all.Skipped = nil
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("ReadCauseRows =\n%+v\nwant\n%+v", all, want)
	}
	if len(skipped) != len(wantSkipped) {
		t.Fatalf("skipped = %+v, want %d rows", skipped, len(wantSkipped))
	}
	for i, w := range wantSkipped {
		got := skipped[i]
		if got.Table != w.table || got.Key != w.key || !strings.HasPrefix(got.Error, w.err) {
			t.Errorf("skipped[%d] = %+v, want %s %s %q...", i, got, w.table, w.key, w.err)
		}
	}

	// The project filter keeps proj-1's rows: run-2's rows, the milestone
	// whose run row is missing, and run-2's bad review failure drop out.
	scoped, err := st.ReadCauseRows(ctx, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	keys := func(rows []CauseRow) []string {
		out := []string{}
		for _, row := range rows {
			out = append(out, row.Key)
		}
		return out
	}
	for name, check := range map[string]struct{ got, want []string }{
		"failure causes":        {keys(scoped.FailureCauses), []string{"inv-1", "inv-2"}},
		"terminals":             {keys(scoped.Terminals), []string{"1", "7"}},
		"publication blocks":    {keys(scoped.PublicationBlocks), []string{"5"}},
		"review failures":       {keys(scoped.ReviewFailures), []string{"rev-1"}},
		"attention conclusions": {keys(scoped.AttentionConclusions), []string{"item-concluded"}},
	} {
		if !reflect.DeepEqual(check.got, check.want) {
			t.Errorf("proj-1 %s = %v, want %v", name, check.got, check.want)
		}
	}
	scopedSkips := []string{}
	for _, s := range scoped.Skipped {
		scopedSkips = append(scopedSkips, s.Key)
	}
	if want := []string{"inv-4", "inv-5", "4", "item-bad", "item-no-time"}; !reflect.DeepEqual(scopedSkips, want) {
		t.Errorf("proj-1 skipped = %v, want %v", scopedSkips, want)
	}
}
