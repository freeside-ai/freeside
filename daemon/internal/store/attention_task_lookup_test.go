package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/domain/domaintest"
)

func TestListOpenAttentionItemRecordsForTask(t *testing.T) {
	ctx := t.Context()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	selected := attentionItemForRun(t, "selected-item", "run-selected")
	closed := attentionItemForRun(t, "closed-item", "run-selected")
	other := attentionItemForRun(t, "other-item", "run-other")
	if err := st.Write(ctx, func(tx *WriteTx) error {
		for _, item := range []*domain.AttentionItem{&selected, &closed, &other} {
			if err := putTestAttentionItem(ctx, tx, item); err != nil {
				return err
			}
		}
		closed.Status = domain.StatusExpired
		closed.ItemVersion++
		closed = domaintest.MustConclude(closed)
		return tx.PutAttentionItem(ctx, closed)
	}); err != nil {
		t.Fatal(err)
	}
	// A malformed unrelated row must not prevent retirement of this task.
	if _, err := st.db.ExecContext(ctx, `UPDATE attention_items SET body = '{' WHERE id = ?`, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		items, err := tx.ListOpenAttentionItemRecordsForTask(ctx, *selected.Subject.TaskID)
		if err == nil && (len(items) != 1 || items[0].ID != selected.ID) {
			t.Fatalf("items = %+v", items)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestListOpenAttentionItemRecordsForTaskRejectsHiddenRows(t *testing.T) {
	for _, mutation := range []string{
		"column retargeted", "column cleared", "body retargeted", "malformed body",
		"closed column", "wrong type", "duplicate subject", "case-fold task", "Unicode-fold task",
		"deep body",
	} {
		t.Run(mutation, func(t *testing.T) {
			ctx := t.Context()
			st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
			item := attentionItemForRun(t, "selected-item", "run-selected")
			other := attentionItemForRun(t, "other-item", "run-other")
			if err := st.Write(ctx, func(tx *WriteTx) error { return putTestAttentionItem(ctx, tx, &item) }); err != nil {
				t.Fatal(err)
			}
			if err := st.Write(ctx, func(tx *WriteTx) error { return putTestAttentionItem(ctx, tx, &other) }); err != nil {
				t.Fatal(err)
			}
			otherTaskID := string(*other.Subject.TaskID)
			body, err := encode(item)
			if err != nil {
				t.Fatal(err)
			}
			var column any = string(*item.Subject.TaskID)
			status, kind := "open", string(item.Type)
			switch mutation {
			case "column retargeted":
				column = otherTaskID
			case "column cleared":
				column = nil
			case "body retargeted":
				body = strings.ReplaceAll(body, string(*item.Subject.TaskID), otherTaskID)
			case "malformed body":
				body = "{"
			case "closed column":
				status = "expired"
			case "wrong type":
				kind = string(domain.AttentionSpecApproval)
			case "duplicate subject":
				body = strings.Replace(body, `,"type":`, `,"subject":{},"type":`, 1)
				column = otherTaskID
			case "case-fold task":
				body = strings.Replace(body, `"task_id":`, `"Task_ID":`, 1)
				column = otherTaskID
			case "Unicode-fold task":
				body = strings.Replace(body, `"task_id":`, `"taſk_id":`, 1)
				column = otherTaskID
			case "deep body":
				body = strings.TrimSuffix(body, "}") + `,"padding":` + strings.Repeat("[", 1001) + "null" + strings.Repeat("]", 1001) + "}"
				column = otherTaskID
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE attention_items SET body = ?, subject_task_id = ?, status = ?, item_type = ? WHERE id = ?`, body, column, status, kind, item.ID); err != nil {
				t.Fatal(err)
			}
			err = st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.ListOpenAttentionItemRecordsForTask(ctx, *item.Subject.TaskID)
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("lookup error = %v, want inconsistent row", err)
			}
		})
	}
}
