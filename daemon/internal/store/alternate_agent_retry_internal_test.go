package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindAlternateAgentRetries returns a store at head to the shape a daemon
// at 0124 holds: task_lines rebuilt with 0113's router-only provenance check.
func rewindAlternateAgentRetries(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindHandoffChecks(t, ctx, db)
	for _, statement := range []string{
		`ALTER TABLE run_hold_observations DROP COLUMN quota_alternate_agents`,
		`DROP TABLE alternate_agent_retries`,
		`ALTER TABLE task_lines RENAME TO task_lines_v3`,
		`CREATE TABLE task_lines (
			task_id TEXT NOT NULL REFERENCES tasks (id),
			role TEXT NOT NULL CHECK (role IN ('specifier', 'implementer', 'remediator', 'reviewer')),
			version INTEGER NOT NULL CHECK (version > 0),
			id TEXT NOT NULL UNIQUE CHECK (id <> ''),
			agent TEXT CHECK (agent <> ''),
			predecessor_id TEXT,
			source TEXT NOT NULL CHECK (source <> ''),
			set_by TEXT NOT NULL CHECK (set_by <> ''),
			set_at TEXT NOT NULL,
			provenance TEXT CHECK ((source = 'router') = (provenance IS NOT NULL)),
			cleared INTEGER NOT NULL DEFAULT 0 CHECK (cleared IN (0, 1)),
			CHECK ((version = 1) = (predecessor_id IS NULL)),
			CHECK ((cleared = 1) = (agent IS NULL)),
			PRIMARY KEY (task_id, role, version)
		) STRICT`,
		`INSERT INTO task_lines SELECT task_id, role, version, id, agent, predecessor_id, source, set_by, set_at,
			provenance, cleared FROM task_lines_v3`,
		`DROP TABLE task_lines_v3`,
		`DELETE FROM schema_migrations WHERE name = '0125_alternate_agent_retries.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

func routerProvenance(pool domain.UsagePoolID) *domain.TaskLineProvenance {
	return &domain.TaskLineProvenance{
		PolicyRevision: 1, Tier: domain.TaskLineSelectionPolicyOrder, Pool: pool,
		RankedBy: []domain.TaskLineRankingReading{},
	}
}

// TestAlternateAgentRetryMigrationKeepsTaskLines: 0125 rebuilds task_lines,
// so every line recorded before it reads back unchanged, and a moved line
// with its provenance is storable only after it.
func TestAlternateAgentRetryMigrationKeepsTaskLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/alternate-agent-retry.db", Options{})
	taskID, _ := seedOriginTestTarget(t, ctx, st)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var want []domain.TaskLine
	if err := st.Write(ctx, func(tx *WriteTx) error {
		router, err := tx.RecordRouterTaskLines(ctx, taskID, []domain.TaskLineInput{{
			TaskID: taskID, Role: domain.RoleImplementer, Agent: "claude-a",
			Source: domain.TaskLineSourceRouter, SetBy: "router:" + string(taskID),
			Provenance: routerProvenance("pool-a"),
		}}, at)
		if err != nil {
			return err
		}
		operator, err := tx.AppendTaskLine(ctx, domain.TaskLineInput{
			TaskID: taskID, Role: domain.RoleReviewer, Agent: "codex",
			Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
		}, at)
		want = append(router, operator)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rewindAlternateAgentRetries(t, ctx, st.db)
	if _, err := st.db.ExecContext(ctx, `INSERT INTO task_lines
		(task_id, role, version, id, agent, predecessor_id, source, set_by, set_at, provenance, cleared)
		VALUES (?, 'specifier', 1, 'line-x', 'claude-a', NULL, 'alternate_agent_move', 'x', ?, '{}', 0)`,
		taskID, formatTime(at)); err == nil {
		t.Fatal("0123's task_lines accepted a provenance on a non-router line")
	}
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)
	var got []domain.TaskLine
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.TaskLines(ctx, taskID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("lines after 0125 = %+v, want %+v", got, want)
	}
	for _, line := range want {
		var found bool
		for _, g := range got {
			found = found || reflect.DeepEqual(g, line)
		}
		if !found {
			t.Fatalf("line %+v changed across 0125: %+v", line, got)
		}
	}
}

// moveSelection stands in for a chosen enrollment and its served tuple.
func moveSelection(agent string) *domain.TaskLineSelection {
	return &domain.TaskLineSelection{
		EnrollmentID: domain.ClientEnrollmentID("enrollment-" + agent), ModelID: "model-" + agent, Effort: "high",
	}
}

func seedParkedMove(t *testing.T, taskID domain.TaskID, at time.Time) domain.AlternateAgentRetry {
	t.Helper()
	record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
		TaskID: taskID, Role: domain.RoleImplementer, FailedInvocationID: "inv-1",
		CommandID: "cmd-move", Selection: moveSelection("claude-b"), Pool: "pool-b",
		Qualification: domain.AlternateAgentDifferentPool, RecordedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// TestAlternateAgentRetryRecord: a record round-trips by id and by command,
// replays idempotently, refuses a second choice under one command, and fails
// closed on a body edited in place.
func TestAlternateAgentRetryRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/alternate-agent-record.db", Options{})
	taskID, _ := seedOriginTestTarget(t, ctx, st)
	record := seedParkedMove(t, taskID, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	for range 2 {
		if err := st.Write(ctx, func(tx *WriteTx) error { return tx.RecordAlternateAgentRetry(ctx, record) }); err != nil {
			t.Fatal(err)
		}
	}
	other := record
	other.Selection = moveSelection("claude-c")
	other, err := domain.NewAlternateAgentRetry(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.RecordAlternateAgentRetry(ctx, other) }); err == nil {
		t.Fatal("a second choice under one command was stored")
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		byID, err := tx.GetAlternateAgentRetry(ctx, record.ID)
		if err != nil {
			return err
		}
		byCommand, err := tx.GetAlternateAgentRetryByCommand(ctx, record.CommandID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(byID, record) || !reflect.DeepEqual(byCommand, record) {
			t.Fatalf("read back %+v and %+v, want %+v", byID, byCommand, record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE alternate_agent_retries
		SET body = json_set(body, '$.selection.enrollment_id', 'enrollment-claude-c') WHERE id = ?`, record.ID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetAlternateAgentRetry(ctx, record.ID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("an edited body: err = %v, want a corrupt row", err)
	}
}

// TestAlternateAgentMoveLines: a move supersedes the role's router line with
// a cited version, refuses lines that do not cite a stored parked move, and
// requires the record's own role to move to its selection.
func TestAlternateAgentMoveLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/alternate-agent-move.db", Options{})
	taskID, _ := seedOriginTestTarget(t, ctx, st)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	record := seedParkedMove(t, taskID, at.Add(time.Hour))
	move := func(role domain.RoleName, agent, setBy string) domain.TaskLineInput {
		return domain.TaskLineInput{
			TaskID: taskID, Role: role, Selection: moveSelection(agent), Source: domain.TaskLineSourceAlternateAgentMove,
			SetBy: setBy, Provenance: routerProvenance("pool-b"),
		}
	}
	write := func(inputs ...domain.TaskLineInput) ([]domain.TaskLine, error) {
		var lines []domain.TaskLine
		err := st.Write(ctx, func(tx *WriteTx) error {
			var err error
			lines, err = tx.RecordAlternateAgentMoveLines(ctx, record, inputs)
			return err
		})
		return lines, err
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		_, err := tx.RecordRouterTaskLines(ctx, taskID, []domain.TaskLineInput{{
			TaskID: taskID, Role: domain.RoleImplementer, Agent: "claude-a",
			Source: domain.TaskLineSourceRouter, SetBy: "router:" + string(taskID),
			Provenance: routerProvenance("pool-a"),
		}}, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := write(move(domain.RoleImplementer, "claude-b", string(record.ID))); err == nil {
		t.Fatal("lines citing an unstored record were written")
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.RecordAlternateAgentRetry(ctx, record) }); err != nil {
		t.Fatal(err)
	}
	for name, inputs := range map[string][]domain.TaskLineInput{
		"another record": {move(domain.RoleImplementer, "claude-b", "sha256:other")},
		"role unmoved":   {move(domain.RoleRemediator, "claude-b", string(record.ID))},
		"another agent":  {move(domain.RoleImplementer, "claude-c", string(record.ID))},
	} {
		if _, err := write(inputs...); !errors.Is(err, domain.ErrTaskLineInconsistent) {
			t.Fatalf("%s: err = %v, want ErrTaskLineInconsistent", name, err)
		}
	}
	lines, err := write(
		move(domain.RoleImplementer, "claude-b", string(record.ID)),
		move(domain.RoleRemediator, "claude-b", string(record.ID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if lines[0].Version != 2 || lines[0].PredecessorID == nil || lines[1].Version != 1 ||
		!lines[0].SetAt.Equal(record.RecordedAt) {
		t.Fatalf("moved lines = %+v, want the implementer's version 2 and the remediator's first", lines)
	}
}

// TestRunHoldQuotaWindowOffersRoundTrip: a parked hold's offers round-trip,
// and a stored list the contract cannot express fails the read.
func TestRunHoldQuotaWindowOffersRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/run-hold-offers.db", Options{})
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	offers := []domain.AlternateAgentOffer{{
		Enrollment: "enrollment-b", Pool: "pool-b", Harness: domain.HarnessClientClaudeCode,
		Model: "claude-opus-4-1", Qualification: domain.AlternateAgentDifferentPool,
	}}
	want := domain.RunHoldObservation{
		RunID: "run-1", InvocationID: new(domain.InvocationID("inv-1")), Reason: domain.HoldQuotaWindowExhausted,
		QuotaWindow: &domain.QuotaWindowHold{Pool: "pool-a", ExhaustedMark: domain.ExhaustedMark{
			Window: "five_hour", Source: domain.ExhaustedMarkProviderRefusal,
			InvocationID: new(domain.InvocationID("inv-1")), ResetsAt: new(at.Add(5 * time.Hour)), SetAt: at,
		}, AlternateAgents: offers},
		FirstObservedAt: at, LastObservedAt: at,
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.RecordRunHold(ctx, want) }); err != nil {
		t.Fatal(err)
	}
	read := func() (domain.RunHoldObservation, error) {
		var hold domain.RunHoldObservation
		err := st.Read(ctx, func(tx *ReadTx) error {
			var err error
			hold, _, err = tx.GetRunHold(ctx, "run-1")
			return err
		})
		return hold, err
	}
	got, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.QuotaWindow.AlternateAgents, offers) {
		t.Fatalf("offers = %+v, want %+v", got.QuotaWindow.AlternateAgents, offers)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE run_hold_observations
		SET quota_alternate_agents = '[]' WHERE run_id = 'run-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := read(); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("an empty stored list: err = %v, want ErrEmptyField", err)
	}
}
