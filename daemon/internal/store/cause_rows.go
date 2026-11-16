package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Cause rows for `freesided cause-counts` (issue #3238): the stored typed
// causes, read as raw strings so a value this build doesn't know is still
// counted. The counts are advisory (Tier C) and grant nothing, so a row that
// can't be decoded is skipped and reported, never a reason to fail the read.
// Every query filters by project when one is given: through the row's run, or
// on attention_items.project_id.

const (
	listFailureCauseRowsSQL = `
SELECT o.invocation_id, o.cause, a.run_id, o.recorded_at
FROM execution_outcomes o
JOIN execution_admissions a ON a.invocation_id = o.invocation_id
JOIN runs r ON r.id = a.run_id
WHERE o.status = 'failed' AND (? = '' OR r.project_id = ?)
ORDER BY o.invocation_id`
	// run_milestones.run_id has no foreign key, so the join is outer: without
	// a project filter a milestone whose run row is missing still counts.
	listTerminalRowsSQL = `
SELECT CAST(m.id AS TEXT), COALESCE(m.terminal, ''), m.run_id, m.recorded_at
FROM run_milestones m
LEFT JOIN runs r ON r.id = m.run_id
WHERE m.kind = 'terminal_recorded' AND (? = '' OR r.project_id = ?)
ORDER BY m.id`
	listPublicationBlockedRowsSQL = `
SELECT CAST(m.id AS TEXT), COALESCE(m.reason, ''), m.run_id, m.recorded_at
FROM run_milestones m
LEFT JOIN runs r ON r.id = m.run_id
WHERE m.kind = 'publication_blocked' AND (? = '' OR r.project_id = ?)
ORDER BY m.id`
	listReviewFailureRowsSQL = `
SELECT f.invocation_id, f.failure_class, f.run_id, f.observed_at
FROM review_failures f
JOIN runs r ON r.id = f.run_id
WHERE (? = '' OR r.project_id = ?)
ORDER BY f.invocation_id`
	listAttentionConclusionRowsSQL = `
SELECT id, subject_run_id, body
FROM attention_items
WHERE (? = '' OR project_id = ?)
ORDER BY id`
)

// CauseRow is one stored cause. Key identifies the row in its table, and an
// empty RunID means the row names no run.
type CauseRow struct {
	Key   string
	Value string
	RunID domain.RunID
	At    time.Time
}

// SkippedCauseRow is a row ReadCauseRows couldn't decode: an unparseable
// time, an empty value, or an attention body that isn't valid JSON.
type SkippedCauseRow struct {
	Table string
	Key   string
	Error string
}

// CauseRows holds every stored cause, one slice per source, and the rows that
// couldn't be decoded. Skipped rows are listed whatever their time, since an
// unparseable time can't be placed in a period.
type CauseRows struct {
	// FailureCauses are failed execution outcomes' causes.
	FailureCauses []CauseRow
	// Terminals are terminal_recorded milestones' terminals.
	Terminals []CauseRow
	// PublicationBlocks are publication_blocked milestones' reasons.
	PublicationBlocks []CauseRow
	// ReviewFailures are review failures' classes.
	ReviewFailures []CauseRow
	// AttentionConclusions are attention items' conclusion causes. An item
	// with no conclusion is open, or closed before conclusions existed, and
	// isn't listed.
	AttentionConclusions []CauseRow
	Skipped              []SkippedCauseRow
}

// ReadCauseRows reads every stored cause in one read-only transaction,
// limited to project's rows when project is non-empty. A query or scan error
// fails the read; a row that can't be decoded is skipped and listed.
func (s *Store) ReadCauseRows(ctx context.Context, project domain.ProjectID) (CauseRows, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CauseRows{}, fmt.Errorf("begin cause read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var rows CauseRows
	for _, source := range []struct {
		table, column, query string
		into                 *[]CauseRow
	}{
		{"execution_outcomes", "recorded_at", listFailureCauseRowsSQL, &rows.FailureCauses},
		{"run_milestones", "recorded_at", listTerminalRowsSQL, &rows.Terminals},
		{"run_milestones", "recorded_at", listPublicationBlockedRowsSQL, &rows.PublicationBlocks},
		{"review_failures", "observed_at", listReviewFailureRowsSQL, &rows.ReviewFailures},
	} {
		raw, err := queryAll(ctx, tx, scanRawCauseRow, source.query, project, project)
		if err != nil {
			return CauseRows{}, fmt.Errorf("read %s causes: %w", source.table, err)
		}
		for _, r := range raw {
			row, err := r.decode(source.column)
			if err != nil {
				rows.Skipped = append(rows.Skipped, SkippedCauseRow{Table: source.table, Key: r.key, Error: err.Error()})
				continue
			}
			*source.into = append(*source.into, row)
		}
	}

	items, err := queryAll(ctx, tx, scanRawAttentionConclusion, listAttentionConclusionRowsSQL, project, project)
	if err != nil {
		return CauseRows{}, fmt.Errorf("read attention_items causes: %w", err)
	}
	for _, item := range items {
		row, concluded, err := item.decode()
		switch {
		case err != nil:
			rows.Skipped = append(rows.Skipped, SkippedCauseRow{Table: "attention_items", Key: item.id, Error: err.Error()})
		case concluded:
			rows.AttentionConclusions = append(rows.AttentionConclusions, row)
		}
	}

	if err := tx.Commit(); err != nil {
		return CauseRows{}, fmt.Errorf("commit cause read: %w", err)
	}
	return rows, nil
}

type rawCauseRow struct {
	key, value, runID, at string
}

func scanRawCauseRow(row scanner) (rawCauseRow, error) {
	var r rawCauseRow
	err := row.Scan(&r.key, &r.value, &r.runID, &r.at)
	return r, err
}

func (r rawCauseRow) decode(timeColumn string) (CauseRow, error) {
	if r.value == "" {
		return CauseRow{}, errors.New("empty cause")
	}
	at, err := parseTime(r.at)
	if err != nil {
		return CauseRow{}, fmt.Errorf("%s: %w", timeColumn, err)
	}
	return CauseRow{Key: r.key, Value: r.value, RunID: domain.RunID(r.runID), At: at}, nil
}

type rawAttentionConclusion struct {
	id    string
	runID sql.NullString
	body  []byte
}

func scanRawAttentionConclusion(row scanner) (rawAttentionConclusion, error) {
	var r rawAttentionConclusion
	err := row.Scan(&r.id, &r.runID, &r.body)
	return r, err
}

// decode projects the conclusion out of a stored item body. It decodes only
// the fields it counts, not a domain.AttentionItem, so an item this build
// can't validate, or a cause it doesn't know, is still counted. concluded is
// false for an item with no conclusion.
func (r rawAttentionConclusion) decode() (row CauseRow, concluded bool, err error) {
	var body struct {
		Conclusion *struct {
			Cause       string    `json:"cause"`
			ConcludedAt time.Time `json:"concluded_at"`
		} `json:"conclusion"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		return CauseRow{}, false, fmt.Errorf("body: %w", err)
	}
	conclusion := body.Conclusion
	switch {
	case conclusion == nil:
		return CauseRow{}, false, nil
	case conclusion.Cause == "":
		return CauseRow{}, false, errors.New("empty cause")
	case conclusion.ConcludedAt.IsZero():
		return CauseRow{}, false, errors.New("conclusion has no concluded_at")
	}
	return CauseRow{
		Key: r.id, Value: conclusion.Cause, RunID: domain.RunID(r.runID.String), At: conclusion.ConcludedAt.UTC(),
	}, true, nil
}
