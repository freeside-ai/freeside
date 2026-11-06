package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestTaskLineSelectionMigrationKeepsV1Rows proves migration 0130 rewrites no
// task line: every row of every source, a cleared one among them, keeps its
// columns and its id, reads back as v1 with its chain intact, a v1 change
// record still backs the line that cites it, and an admission citing a line
// written before the migration still resolves its derivation after it.
func TestTaskLineSelectionMigrationKeepsV1Rows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	migrateThrough(t, ctx, db, "0130_")
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: "run-1", ProjectID: "proj-1", SpecDigest: agentSpecDigest, PolicyDigest: agentPolicyDigest,
		Stages: []domain.Stage{{
			ID: "stage-1", RunID: "run-1", Name: "implementation",
			Attempts: []domain.Attempt{{ID: "attempt-1", StageID: "stage-1", Number: 1, InvocationID: "inv-1"}},
		}},
	}
	body, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertRawRunFixture(ctx, db, string(run.ID), string(run.ProjectID), agentPolicyDigest, 1, 1, string(body)); err != nil {
		t.Fatal(err)
	}
	var taskID domain.TaskID
	if err := db.QueryRowContext(ctx, `SELECT task_id FROM runs WHERE id = 'run-1'`).Scan(&taskID); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	provenance := &domain.TaskLineProvenance{
		PolicyRevision: 1, Tier: domain.TaskLineSelectionDrainFirstAccount, Pool: "claude-max-a",
		RankedBy: []domain.TaskLineRankingReading{{
			Pool: "claude-max-a", Window: "seven_day", ResetsAt: at.Add(30 * time.Hour), ObservedAt: at.Add(-time.Hour),
		}},
	}
	// heads holds each role's newest line, so the next version chains to it.
	heads := map[domain.RoleName]domain.TaskLine{}
	var seeded []domain.TaskLine
	insertLine := func(in domain.TaskLineInput) domain.TaskLine {
		t.Helper()
		in.TaskID = taskID
		version, predecessor := 1, (*domain.Digest)(nil)
		if head, ok := heads[in.Role]; ok {
			version, predecessor = head.Version+1, &head.ID
		}
		// Built by hand: no constructor writes a v1 clear any more.
		line := domain.TaskLine{
			Encoding: domain.TaskLineEncodingV1, TaskID: in.TaskID, Role: in.Role, Agent: in.Agent, Version: version,
			PredecessorID: predecessor, Source: in.Source, SetBy: in.SetBy, SetAt: at, Provenance: in.Provenance,
			Cleared: in.Cleared,
		}
		var err error
		if line.ID, err = line.ComputeID(); err != nil {
			t.Fatal(err)
		}
		if err := line.Validate(); err != nil {
			t.Fatal(err)
		}
		var agent, predecessorID, provenanceText any
		if line.Agent != "" {
			agent = line.Agent
		}
		if line.PredecessorID != nil {
			predecessorID = string(*line.PredecessorID)
		}
		if line.Provenance != nil {
			text, err := json.Marshal(line.Provenance)
			if err != nil {
				t.Fatal(err)
			}
			provenanceText = string(text)
		}
		// 0125's columns: no encoding, no selection.
		if _, err := db.ExecContext(ctx, `INSERT INTO task_lines
			(task_id, role, version, id, agent, predecessor_id, source, set_by, set_at, provenance, cleared)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			line.TaskID, line.Role, line.Version, line.ID, agent, predecessorID, line.Source, line.SetBy,
			formatTime(line.SetAt), provenanceText, line.Cleared); err != nil {
			t.Fatal(err)
		}
		heads[in.Role] = line
		seeded = append(seeded, line)
		return line
	}
	// insertChange records a v1 change as it was stored before v2, and the
	// line that cites it.
	var changes []domain.TaskLineChange
	insertChange := func(role domain.RoleName, agent string, source domain.TaskLineSource) domain.TaskLine {
		t.Helper()
		head := heads[role]
		change := domain.TaskLineChange{
			Encoding: domain.TaskLineChangeEncodingV1, TaskID: taskID, Role: role,
			Agent: agent, Cleared: agent == "", ReplacesID: &head.ID, RecordedAt: at,
		}
		if change.ID, err = change.ComputeID(); err != nil {
			t.Fatal(err)
		}
		if err := change.Validate(); err != nil {
			t.Fatal(err)
		}
		text, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO task_line_changes (id, task_id, role, recorded_at, body)
			VALUES (?, ?, ?, ?, ?)`, change.ID, change.TaskID, change.Role, formatTime(change.RecordedAt), string(text)); err != nil {
			t.Fatal(err)
		}
		changes = append(changes, change)
		return insertLine(domain.TaskLineInput{
			Role: role, Agent: agent, Cleared: agent == "", Source: source, SetBy: string(change.ID),
		})
	}
	insertLine(domain.TaskLineInput{Role: domain.RoleSpecifier, Agent: "claude", Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1"})
	insertLine(domain.TaskLineInput{Role: domain.RoleSpecifier, Agent: "claude-escalated", Source: domain.TaskLineSourceTriage, SetBy: "triage-1"})
	cited := insertLine(domain.TaskLineInput{Role: domain.RoleImplementer, Agent: "codex", Source: domain.TaskLineSourceCLISubmit, SetBy: "cmd-2"})
	insertChange(domain.RoleImplementer, "claude-b", domain.TaskLineSourceCLILineChange)
	insertChange(domain.RoleImplementer, "", domain.TaskLineSourceCLILineChange)
	insertLine(domain.TaskLineInput{Role: domain.RoleRemediator, Agent: "codex", Source: domain.TaskLineSourceProposalStart, SetBy: "cmd-3"})
	insertLine(domain.TaskLineInput{
		Role: domain.RoleRemediator, Agent: "claude-a", Source: domain.TaskLineSourceAlternateAgentMove,
		SetBy: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Provenance: provenance,
	})
	insertLine(domain.TaskLineInput{
		Role: domain.RoleReviewer, Agent: "claude-a", Source: domain.TaskLineSourceRouter,
		SetBy: "router:" + string(taskID), Provenance: provenance,
	})
	insertChange(domain.RoleReviewer, "codex", domain.TaskLineSourceClientLineChange)

	rowsText := func(query string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				t.Fatal(err)
			}
			out = append(out, text)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	const lineColumns = `SELECT json_array(task_id, role, version, id, agent, predecessor_id, source, set_by,
		set_at, provenance, cleared) FROM task_lines ORDER BY id`
	const changeColumns = `SELECT json_array(id, task_id, role, recorded_at, body) FROM task_line_changes ORDER BY id`
	beforeLines, beforeChanges := rowsText(lineColumns), rowsText(changeColumns)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening the store runs 0130 and every migration after it.
	s := openTemplateStoreAt(t, path, Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: tamperFloor(),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	db = s.db
	if got := rowsText(lineColumns); !reflect.DeepEqual(got, beforeLines) {
		t.Fatalf("task_lines columns after = %v\nbefore = %v", got, beforeLines)
	}
	if got := rowsText(changeColumns); !reflect.DeepEqual(got, beforeChanges) {
		t.Fatalf("task_line_changes after = %v\nbefore = %v", got, beforeChanges)
	}
	if got := rowsText(`SELECT DISTINCT encoding || ':' || coalesce(enrollment_id, '') || coalesce(model_id, '') ||
		coalesce(effort, '') FROM task_lines`); !reflect.DeepEqual(got, []string{string(domain.TaskLineEncodingV1) + ":"}) {
		t.Fatalf("encodings and selections after = %v, want v1 rows that select nothing", got)
	}

	if err := s.Read(ctx, func(tx *ReadTx) error {
		// TaskLines re-runs each row's id and the chain check.
		lines, err := tx.TaskLines(ctx, taskID)
		if err != nil {
			return err
		}
		byID := map[domain.Digest]domain.TaskLine{}
		for _, line := range lines {
			byID[line.ID] = line
		}
		if len(byID) != len(seeded) {
			t.Fatalf("lines after = %d, want %d", len(byID), len(seeded))
		}
		for _, want := range seeded {
			if got, ok := byID[want.ID]; !ok || !reflect.DeepEqual(got, want) {
				t.Fatalf("line %s after = %+v, want %+v", want.ID, got, want)
			}
		}
		for _, want := range changes {
			change, err := tx.GetTaskLineChange(ctx, want.ID)
			if err != nil {
				return err
			}
			if change.Encoding != domain.TaskLineChangeEncodingV1 {
				t.Fatalf("change %s encoding = %s, want v1", change.ID, change.Encoding)
			}
			var backed bool
			for _, line := range lines {
				if line.SetBy == string(change.ID) {
					backed = change.Backs(line)
				}
			}
			if !backed {
				t.Fatalf("change %s backs no line after the migration", change.ID)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The admission's run already exists with its task; seedAgentClosure
	// rewrites it with the same body and keeps that task.
	generation := seedAgentClosure(t, s)
	admission := agentAdmissionCiting(t, generation, cited.ID)
	if err := s.Write(ctx, func(tx *WriteTx) error { return tx.RecordExecutionAdmission(ctx, admission) }); err != nil {
		t.Fatalf("record an admission citing a pre-migration line: %v", err)
	}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetExecutionAdmission(ctx, admission.InvocationID)
		if err != nil {
			return err
		}
		if got.ID != admission.ID || got.AgentBinding.SelectionRecordID != cited.ID {
			t.Fatalf("admission read back = %+v", got.AgentBinding)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// rewindTaskLineSelection returns a store at head to the shape a daemon at
// 0129 holds: task_lines with 0125's columns and CHECKs. A v2 row has no
// 0129 form, so a store holding one fails the copy's CHECK.
func rewindTaskLineSelection(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE task_lines RENAME TO task_lines_v2`,
		`CREATE TABLE task_lines (
			task_id        TEXT    NOT NULL REFERENCES tasks (id),
			role           TEXT    NOT NULL CHECK (role IN ('specifier', 'implementer', 'remediator', 'reviewer')),
			version        INTEGER NOT NULL CHECK (version > 0),
			id             TEXT    NOT NULL UNIQUE CHECK (id <> ''),
			agent          TEXT    CHECK (agent <> ''),
			predecessor_id TEXT,
			source         TEXT    NOT NULL CHECK (source <> ''),
			set_by         TEXT    NOT NULL CHECK (set_by <> ''),
			set_at         TEXT    NOT NULL,
			provenance     TEXT    CHECK ((source IN ('router', 'alternate_agent_move')) = (provenance IS NOT NULL)),
			cleared        INTEGER NOT NULL DEFAULT 0 CHECK (cleared IN (0, 1)),
			CHECK ((version = 1) = (predecessor_id IS NULL)),
			CHECK ((cleared = 1) = (agent IS NULL)),
			PRIMARY KEY (task_id, role, version)
		) STRICT`,
		`INSERT INTO task_lines
			(task_id, role, version, id, agent, predecessor_id, source, set_by, set_at, provenance, cleared)
		SELECT task_id, role, version, id, agent, predecessor_id, source, set_by, set_at, provenance, cleared
		FROM task_lines_v2`,
		`DROP TABLE task_lines_v2`,
		`DELETE FROM schema_migrations WHERE name = '0130_task_line_selection.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
