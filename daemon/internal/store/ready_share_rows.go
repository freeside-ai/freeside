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

// Attention item rows for `freesided ready-share` (issue #3406): each item's
// type, task, class, and creation time, so the report can tell which items a
// task raised before its first ready card. The report is advisory (Tier C)
// and grants nothing, so the read projects only those fields rather than
// decoding a domain.AttentionItem, whose integrity checks would fail the whole
// read on one bad row; a row it can't decode is skipped and listed.

const listReadyShareRowsSQL = `
SELECT id, item_type, COALESCE(subject_task_id, ''), body
FROM attention_items
WHERE (? = '' OR project_id = ?)
ORDER BY id`

// ReadyShareRow is one attention item. An empty TaskID means the item names
// no task. Type and Class are the stored strings, so a value this build
// doesn't know is still counted.
type ReadyShareRow struct {
	ItemID    domain.ItemID
	TaskID    domain.TaskID
	Type      string
	Class     string
	CreatedAt time.Time
}

// ReadyShareRows holds every decodable attention item and the rows that
// couldn't be decoded.
type ReadyShareRows struct {
	Items   []ReadyShareRow
	Skipped []SkippedCauseRow
}

// ReadReadyShareRows reads every attention item in one read-only transaction,
// limited to project's items when project is non-empty. A query or scan error
// fails the read; a row with an empty type, a body that isn't valid JSON, no
// created_at, or an empty interruption_class is skipped and listed.
func (s *Store) ReadReadyShareRows(ctx context.Context, project domain.ProjectID) (ReadyShareRows, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ReadyShareRows{}, fmt.Errorf("begin ready share read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	raw, err := queryAll(ctx, tx, scanRawReadyShareRow, listReadyShareRowsSQL, project, project)
	if err != nil {
		return ReadyShareRows{}, fmt.Errorf("read attention_items: %w", err)
	}
	var rows ReadyShareRows
	for _, r := range raw {
		row, err := r.decode()
		if err != nil {
			rows.Skipped = append(rows.Skipped, SkippedCauseRow{Table: "attention_items", Key: r.id, Error: err.Error()})
			continue
		}
		rows.Items = append(rows.Items, row)
	}
	if err := tx.Commit(); err != nil {
		return ReadyShareRows{}, fmt.Errorf("commit ready share read: %w", err)
	}
	return rows, nil
}

type rawReadyShareRow struct {
	id, itemType, taskID string
	body                 []byte
}

func scanRawReadyShareRow(row scanner) (rawReadyShareRow, error) {
	var r rawReadyShareRow
	err := row.Scan(&r.id, &r.itemType, &r.taskID, &r.body)
	return r, err
}

func (r rawReadyShareRow) decode() (ReadyShareRow, error) {
	if r.itemType == "" {
		return ReadyShareRow{}, errors.New("empty item_type")
	}
	var body struct {
		InterruptionClass string     `json:"interruption_class"`
		CreatedAt         *time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		return ReadyShareRow{}, fmt.Errorf("body: %w", err)
	}
	switch {
	case body.CreatedAt == nil || body.CreatedAt.IsZero():
		return ReadyShareRow{}, errors.New("no created_at")
	case body.InterruptionClass == "":
		return ReadyShareRow{}, errors.New("empty interruption_class")
	}
	return ReadyShareRow{
		ItemID: domain.ItemID(r.id), TaskID: domain.TaskID(r.taskID), Type: r.itemType,
		Class: body.InterruptionClass, CreatedAt: body.CreatedAt.UTC(),
	}, nil
}
