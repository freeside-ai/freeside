package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func attentionItemForRun(t *testing.T, id domain.ItemID, runID domain.RunID) domain.AttentionItem {
	t.Helper()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:              domain.AttentionBlocked,
		Priority:          domain.PriorityNormal,
		Reason:            "waiting on an external dependency",
		RequestedDecision: []domain.Action{},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	return item
}

func attentionItemWithEvidence(
	t *testing.T, id domain.ItemID, runID domain.RunID, recipe domain.Digest,
) domain.AttentionItem {
	t.Helper()
	approved := map[domain.Digest]bool{recipe: true}
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: domain.ArtifactID("artifact-" + string(id)), Type: domain.ArtifactKindVerifyLog,
		Digest: domain.Digest("sha256:" + string(id)),
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "invocation-1",
			HeadBinding:              domain.HeadBound,
			SourceHeadSHA:            "cafebabe",
			VerificationRecipeDigest: &recipe,
			SensitivityClass:         domain.SensitivityNormal,
		},
	}, approved)
	if err != nil {
		t.Fatalf("NewArtifact: %v", err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:              domain.AttentionBlocked,
		Priority:          domain.PriorityNormal,
		Reason:            "waiting with evidence",
		RequestedDecision: []domain.Action{},
		EvidenceSnapshot:  []domain.Artifact{artifact},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	return item
}

func TestAttentionSubjectRunBindingMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0050_")
	if got := rawVersion(t, db); got != 49 {
		t.Fatalf("prior schema version = %d, want 49", got)
	}

	runItem := attentionItemForRun(t, "run-item", "run-1")
	runBody, err := encode(runItem)
	if err != nil {
		t.Fatalf("encode run item: %v", err)
	}
	nonRunItem := runItem
	nonRunItem.ID = "system-item"
	nonRunItem.Subject = domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	nonRunBody, err := encode(nonRunItem)
	if err != nil {
		t.Fatalf("encode non-run item: %v", err)
	}
	for _, row := range []struct {
		id, body string
	}{
		{string(runItem.ID), runBody},
		{string(nonRunItem.ID), nonRunBody},
		{"malformed-item", "{"},
	} {
		if _, err := db.ExecContext(ctx, `
INSERT INTO attention_items
    (id, project_id, conversation_id, item_type, status, health_posture,
     entity_version, as_of_revision, body)
VALUES (?, 'proj-1', NULL, 'blocked', 'open', NULL, 1, 1, ?)`, row.id, row.body); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := rawVersion(t, db); got != 57 {
		t.Fatalf("schema version = %d, want 57", got)
	}
	bindings := map[string]sql.NullString{}
	for _, id := range []string{"run-item", "system-item", "malformed-item"} {
		var binding sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT subject_run_id FROM attention_items WHERE id = ?`, id).Scan(&binding); err != nil {
			t.Fatalf("read binding for %s: %v", id, err)
		}
		bindings[id] = binding
	}
	if got := bindings["run-item"]; !got.Valid || got.String != "run-1" {
		t.Fatalf("run binding = %+v, want run-1", got)
	}
	for _, id := range []string{"system-item", "malformed-item"} {
		if got := bindings[id]; got.Valid {
			t.Fatalf("%s binding = %+v, want NULL", id, got)
		}
	}
	var indexSQL string
	if err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_schema WHERE type = 'index' AND name = 'attention_items_open_by_run'`).
		Scan(&indexSQL); err != nil {
		t.Fatalf("read run-binding index: %v", err)
	}
	if !strings.Contains(indexSQL, "WHERE status = 'open'") {
		t.Fatalf("run-binding index = %q, want open-item partial index", indexSQL)
	}
}

func TestAttentionSubjectRunBindingWriteAndRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	item := attentionItemForRun(t, "selected-item", "run-selected")
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatalf("PutAttentionItem: %v", err)
	}

	var binding sql.NullString
	if err := st.db.QueryRowContext(ctx,
		`SELECT subject_run_id FROM attention_items WHERE id = ?`, item.ID).Scan(&binding); err != nil {
		t.Fatalf("read persisted binding: %v", err)
	}
	if !binding.Valid || binding.String != "run-selected" {
		t.Fatalf("persisted binding = %+v, want run-selected", binding)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
		if err != nil {
			return err
		}
		if got.ID != item.ID {
			t.Fatalf("GetAttentionItemSnapshot id = %q, want %q", got.ID, item.ID)
		}
		all, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if len(all) != 1 || all[0].Value.ID != item.ID {
			t.Fatalf("ListAttentionItems = %+v, want %q", all, item.ID)
		}
		scoped, err := tx.ListOpenAttentionItemsForRun(ctx, "run-selected")
		if err != nil {
			return err
		}
		if len(scoped) != 1 || scoped[0].ID != item.ID {
			t.Fatalf("ListOpenAttentionItemsForRun = %+v, want %q", scoped, item.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("read attention item: %v", err)
	}
}

func TestListOpenAttentionItemsForRunIsolatesUnrelatedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	recipe := domain.Digest("sha256:recipe-now-stale")
	approving, err := Open(ctx, path, Options{ApprovedRecipes: map[domain.Digest]bool{recipe: true}})
	if err != nil {
		t.Fatalf("Open approving store: %v", err)
	}
	selected := attentionItemForRun(t, "selected-item", "run-selected")
	stale := attentionItemWithEvidence(t, "stale-item", "run-stale", recipe)
	malformed := attentionItemForRun(t, "malformed-item", "run-malformed")
	if err := approving.Write(ctx, func(tx *WriteTx) error {
		for _, item := range []domain.AttentionItem{selected, stale, malformed} {
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := approving.Close(); err != nil {
		t.Fatalf("close approving store: %v", err)
	}

	closed, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("Open closed-policy store: %v", err)
	}
	t.Cleanup(func() { _ = closed.Close() })
	if _, err := closed.db.ExecContext(ctx,
		`UPDATE attention_items SET body = '{' WHERE id = ?`, malformed.ID); err != nil {
		t.Fatalf("malform unrelated body: %v", err)
	}
	if err := closed.Read(ctx, func(tx *ReadTx) error {
		items, err := tx.ListOpenAttentionItemsForRun(ctx, "run-selected")
		if err != nil {
			return err
		}
		if len(items) != 1 || items[0].ID != selected.ID {
			t.Fatalf("scoped items = %+v, want only %q", items, selected.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("ListOpenAttentionItemsForRun: %v", err)
	}
}

func TestListOpenAttentionItemsForRunRejectsSelectedCorruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, context.Context, *Store, domain.AttentionItem)
	}{
		{
			name: "malformed body",
			mutate: func(t *testing.T, ctx context.Context, st *Store, item domain.AttentionItem) {
				t.Helper()
				if _, err := st.db.ExecContext(ctx,
					`UPDATE attention_items SET body = '{' WHERE id = ?`, item.ID); err != nil {
					t.Fatalf("malform body: %v", err)
				}
			},
		},
		{
			name: "body retargeted",
			mutate: func(t *testing.T, ctx context.Context, st *Store, item domain.AttentionItem) {
				t.Helper()
				other := domain.RunID("run-other")
				item.Subject.ID = domain.SubjectID(other)
				item.Subject.RunID = &other
				body, err := encode(item)
				if err != nil {
					t.Fatalf("encode retargeted body: %v", err)
				}
				if _, err := st.db.ExecContext(ctx,
					`UPDATE attention_items SET body = ? WHERE id = ?`, body, item.ID); err != nil {
					t.Fatalf("retarget body: %v", err)
				}
			},
		},
		{
			name: "column retargeted",
			mutate: func(t *testing.T, ctx context.Context, st *Store, item domain.AttentionItem) {
				t.Helper()
				if _, err := st.db.ExecContext(ctx,
					`UPDATE attention_items SET subject_run_id = 'run-other' WHERE id = ?`, item.ID); err != nil {
					t.Fatalf("retarget column: %v", err)
				}
			},
		},
		{
			name: "column cleared",
			mutate: func(t *testing.T, ctx context.Context, st *Store, item domain.AttentionItem) {
				t.Helper()
				if _, err := st.db.ExecContext(ctx,
					`UPDATE attention_items SET subject_run_id = NULL WHERE id = ?`, item.ID); err != nil {
					t.Fatalf("clear column: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			item := attentionItemForRun(t, "selected-item", "run-selected")
			if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
				t.Fatalf("PutAttentionItem: %v", err)
			}
			tc.mutate(t, ctx, st, item)
			err = st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.ListOpenAttentionItemsForRun(ctx, "run-selected")
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("ListOpenAttentionItemsForRun error = %v, want %v", err, errRowInconsistent)
			}
		})
	}
}

func TestListOpenAttentionItemsForRunRejectsAmbiguousSelectedBody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(string) string
		column string
		status string
	}{
		{
			name: "duplicate status uses Go last value",
			mutate: func(body string) string {
				return strings.Replace(body, `"status":"open"`,
					`"status":"resolved","status":"open"`, 1)
			},
			column: "run-selected",
			status: "resolved",
		},
		{
			name: "duplicate subject uses Go last value",
			mutate: func(body string) string {
				return strings.Replace(body, `"subject":`,
					`"subject":{"subject_type":"run","subject_id":"run-other","run_id":"run-other"},"subject":`, 1)
			},
			column: "run-other",
			status: "open",
		},
		{
			name: "duplicate subject merges omitted run ID",
			mutate: func(body string) string {
				return strings.Replace(body, `,"type":`,
					`,"subject":{},"type":`, 1)
			},
			column: "run-other",
			status: "open",
		},
		{
			name: "case variant subject key",
			mutate: func(body string) string {
				return strings.Replace(body, `"subject":`, `"Subject":`, 1)
			},
			column: "run-other",
			status: "open",
		},
		{
			name: "case variant run ID key",
			mutate: func(body string) string {
				return strings.Replace(body, `"run_id":`, `"Run_ID":`, 1)
			},
			column: "run-other",
			status: "open",
		},
		{
			name: "Unicode-fold subject key",
			mutate: func(body string) string {
				return strings.Replace(body, `"subject":`, `"ſubject":`, 1)
			},
			column: "run-other",
			status: "open",
		},
		{
			name: "Unicode-fold status uses Go last value",
			mutate: func(body string) string {
				return strings.Replace(body, `"status":"open"`,
					`"status":"resolved","ſtatus":"open"`, 1)
			},
			column: "run-selected",
			status: "resolved",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			item := attentionItemForRun(t, "selected-item", "run-selected")
			if err := st.Write(ctx, func(tx *WriteTx) error {
				return tx.PutAttentionItem(ctx, item)
			}); err != nil {
				t.Fatalf("PutAttentionItem: %v", err)
			}
			body, err := encode(item)
			if err != nil {
				t.Fatalf("encode item: %v", err)
			}
			body = tc.mutate(body)
			if _, err := st.db.ExecContext(ctx, `
UPDATE attention_items
SET body = ?, subject_run_id = ?, status = ?
WHERE id = ?`, body, tc.column, tc.status, item.ID); err != nil {
				t.Fatalf("mutate lookup views: %v", err)
			}
			err = st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.ListOpenAttentionItemsForRun(ctx, "run-selected")
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("ListOpenAttentionItemsForRun error = %v, want %v",
					err, errRowInconsistent)
			}
		})
	}
}

func TestListOpenAttentionItemsForRunRejectsSQLiteInvalidBodyRetargetedFromRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	item := attentionItemForRun(t, "selected-item", "run-selected")
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatalf("PutAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode item: %v", err)
	}
	// SQLite JSON1 rejects more than 1,000 nested values, but encoding/json
	// accepts this unknown field and therefore still reconstructs the item.
	body = strings.TrimSuffix(body, "}") + `,"padding":` +
		strings.Repeat("[", 1_001) + "null" + strings.Repeat("]", 1_001) + "}"
	if _, err := st.db.ExecContext(ctx, `
UPDATE attention_items SET body = ?, subject_run_id = 'run-other' WHERE id = ?`, body, item.ID); err != nil {
		t.Fatalf("retarget SQLite-invalid body: %v", err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListOpenAttentionItemsForRun(ctx, "run-selected")
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("ListOpenAttentionItemsForRun error = %v, want %v", err, errRowInconsistent)
	}
}

func TestAttentionSubjectRunBindingReconstructionRejectsRetargetedBody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	item := attentionItemForRun(t, "selected-item", "run-selected")
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatalf("PutAttentionItem: %v", err)
	}
	other := domain.RunID("run-other")
	item.Subject.ID = domain.SubjectID(other)
	item.Subject.RunID = &other
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode retargeted body: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE attention_items SET body = ? WHERE id = ?`, body, item.ID); err != nil {
		t.Fatalf("retarget body: %v", err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("GetAttentionItemSnapshot error = %v, want %v", err, errRowInconsistent)
	}
}

func TestListOpenAttentionItemsChecksSubjectRunBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	item := attentionItemForRun(t, "selected-item", "run-selected")
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatalf("PutAttentionItem: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE attention_items SET subject_run_id = 'run-other' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("retarget column: %v", err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListOpenAttentionItems(ctx, domain.AttentionBlocked)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("ListOpenAttentionItems error = %v, want %v", err, errRowInconsistent)
	}
}
