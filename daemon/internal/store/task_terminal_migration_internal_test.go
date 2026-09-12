package store

import (
	"encoding/json"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestTaskMigrationRebindsOnlyAuthenticatedFakeTerminals(t *testing.T) {
	for _, scenario := range []string{"ready", "blocked", "v1", "reason", "label", "task", "surface"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			db := openRaw(t)
			migrateThrough(t, ctx, db, "0070_")
			f := newLegacyFakePRFixture(t)
			item := f.item
			item.DisplayNames = &domain.DisplayNames{
				Project: domain.DisplayName{Text: "Legacy project", Source: domain.DisplayNameSourceName},
				Task:    domain.DisplayName{Text: "Original work item", Source: domain.DisplayNameSourceName},
			}
			if scenario == "blocked" {
				item.ID = fakepublication.BlockedItemID(f.task.RunID)
				item.Type = domain.AttentionPublishBlocked
				item.PRReference = nil
				item.RequestedDecision = []domain.Action{domain.ActionInspectTrustFailure}
			}
			if scenario == "v1" {
				item.DisplayNames = nil
			}
			plainReason := item.Reason
			digest, err := fakepublication.TerminalDigestBeforeTasks(f.task, item)
			if scenario == "v1" {
				digest, err = fakepublication.TerminalDigestBeforePRReference(f.task, item)
			}
			if err != nil {
				t.Fatal(err)
			}
			item.Reason += "\n\n<!-- freeside:fake-publication-terminal=" + string(digest) + " -->"
			if scenario == "reason" {
				item.Reason = "Forged publication facts" + item.Reason
			}
			if scenario == "label" {
				item.DisplayNames.Task.Text = "Forged original label"
			}
			surface, err := domain.NewDecisionSurface(item)
			if err != nil {
				t.Fatal(err)
			}
			item.DecisionSurface = domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}
			itemBody, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(itemBody, &fields); err != nil {
				t.Fatal(err)
			}
			if item.DisplayNames != nil {
				fields["display_names"], err = json.Marshal(struct {
					Project  domain.DisplayName `json:"project"`
					WorkUnit domain.DisplayName `json:"work_unit"`
				}{item.DisplayNames.Project, item.DisplayNames.Task})
				if err != nil {
					t.Fatal(err)
				}
			}
			itemBody, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			surfaceBody, err := json.Marshal(surface)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "surface" {
				surfaceBody = []byte("{}")
			}
			if scenario == "task" {
				f.task.ProjectID = "foreign-project"
			}
			payload, err := fakepublication.EncodeTask(f.task)
			if err != nil {
				t.Fatal(err)
			}
			run := domain.Run{ID: *item.Subject.RunID, ProjectID: item.ProjectID, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{}}
			runBody, err := encode(run)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range []struct {
				query string
				args  []any
			}{
				{`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES (?, ?, ?, 1, 1, ?)`, []any{run.ID, run.ProjectID, run.PolicyDigest, runBody}},
				{`INSERT INTO outbox (idempotency_key, kind, payload, payload_version, payload_digest, status, created_at) VALUES (?, ?, ?, 1, ?, 'pending', '2026-08-09T12:00:00Z')`, []any{fakepublication.TaskKey(run.ID), fakepublication.TaskKind, payload, contentaddr.Sum(payload)}},
				{`INSERT INTO attention_items (id, project_id, item_type, status, subject_run_id, entity_version, as_of_revision, body) VALUES (?, ?, ?, ?, ?, 1, 1, ?)`, []any{item.ID, item.ProjectID, item.Type, item.Status, run.ID, string(itemBody)}},
				{`INSERT INTO attention_decision_surfaces (item_id, epoch, digest, body) VALUES (?, ?, ?, ?)`, []any{item.ID, surface.Epoch, surface.Digest, string(surfaceBody)}},
			} {
				if _, err := db.ExecContext(ctx, row.query, row.args...); err != nil {
					t.Fatal(err)
				}
			}
			if err := migrate(ctx, db, migrations.FS); err != nil {
				t.Fatal(err)
			}
			st := &Store{db: db}
			if err := st.Read(ctx, func(tx *ReadTx) error {
				got, err := tx.GetAttentionItemRecord(ctx, item.ID)
				if scenario != "ready" && scenario != "blocked" && scenario != "v1" {
					if err == nil {
						t.Fatalf("migration repaired a forged %s", scenario)
					}
					return nil
				}
				if err != nil {
					return err
				}
				if got.Subject.TaskID == nil || got.DecisionSurface != item.DecisionSurface {
					t.Fatal("task migration changed the decision binding or omitted its task")
				}
				validated, err := fakepublication.ValidateTerminalBinding(f.task, got)
				if err != nil {
					return err
				}
				if validated.Reason != plainReason {
					t.Fatalf("changed human reason: %q", validated.Reason)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
