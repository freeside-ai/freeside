package integration_test

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestRealRunDismissedCheckpointPreservesHistory(t *testing.T) {
	p := newProductionPublicationHarnessWithBoundIssue(t, "", 79)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("publish original: %#v, %v", result, err)
	}
	assertDismissedRunHistory(t, p)
	recordOriginalCompletion(t, p)
	assertRealRunCheckpoint(t, p, true, "completed")
	assertRealRunCheckpoint(t, p, false, "")
}

// Use the same assertion for the original publication and a reviewed successor.
func assertDismissedRunHistory(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	assertRealRunCheckpoint(t, p, false, "ready")
	id := decideCurrentReady(t, p, domain.ActionDismiss)
	before, err := p.attention.GetAttentionItem(p.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if before.Item.Status != domain.StatusDismissed || before.Item.DecidedAt == nil {
		t.Fatalf("dismissal was not recorded: %#v", before.Item)
	}
	// Reopening read-only models the verifier's access while preserving the
	// live writer's command and decision history.
	ro, err := store.OpenReadOnly(p.ctx, p.dbPath, store.Options{
		ApprovedRecipes: map[domain.Digest]bool{p.image.RecipeDigest: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ro.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := ro.Read(p.ctx, func(tx *store.ReadTx) error {
		checkpoint, err := readRealRunCheckpoint(p.ctx, tx, p.runID, true)
		if err != nil {
			return err
		}
		if checkpoint.State != "retained" || checkpoint.Binding.ItemID != id ||
			checkpoint.Binding.HeadSHA != before.Item.PRHeadSHA || checkpoint.Review != nil {
			t.Fatalf("dismissed history became current acceptance: %#v", checkpoint)
		}
		if _, err := readRealRunCheckpoint(p.ctx, tx, p.runID, false); err == nil {
			t.Fatal("dismissed card passed final publication verification")
		}
		after, err := tx.GetAttentionItem(p.ctx, id)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before.Item, after) {
			t.Fatal("retained verification changed the dismissed item")
		}
		commands, err := tx.ListCommandsForItem(p.ctx, id)
		if err != nil {
			return err
		}
		if len(commands) != 1 || commands[0].Action != domain.ActionDismiss {
			t.Fatalf("dismissal evidence changed: %#v", commands)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: "return-after-dismiss", DeviceID: "retained-history-device",
		ExpectedEntityVersion: before.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: id, ItemVersion: before.Item.ItemVersion,
			PRHeadSHA: before.Item.PRHeadSHA, ArtifactDigests: before.Item.ArtifactDigests,
			Action: domain.ActionReturnToAgent, Message: "Must remain closed.",
		},
	})
	if !errors.Is(err, signet.ErrClosedItem) {
		t.Fatalf("feedback on dismissed history did not reach the closed-item gate: %v", err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetOutbox(p.ctx, "inv-operator-feedback-return-after-dismiss")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("dismissed history created feedback work: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRealRunDismissedCheckpointRejectsDamagedHistory(t *testing.T) {
	for _, damage := range []struct {
		name, query string
	}{
		{"missing-binding", "DELETE FROM ready_item_pr_bindings WHERE item_id = ?"},
		{"wrong-binding-head", "UPDATE ready_item_pr_bindings SET body = json_set(body, '$.head_sha', 'wrong-head') WHERE item_id = ?"},
		{"missing-outcome", "DELETE FROM inbox WHERE idempotency_key = ?"},
		{"missing-export", "DELETE FROM execution_exports WHERE invocation_id = ?"},
	} {
		t.Run(damage.name, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.startAndRecordExport(t)
			if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
				t.Fatalf("publish original: %#v, %v", result, err)
			}
			id := decideCurrentReady(t, p, domain.ActionDismiss)
			assertRealRunCheckpoint(t, p, true, "retained")
			var binding domain.ReadyItemPRBinding
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				var err error
				binding, err = tx.GetReadyItemPRBinding(p.ctx, id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			argument := string(id)
			switch damage.name {
			case "missing-outcome":
				argument = publicationrecord.OutcomeKey(binding.PublicationIdentity)
			case "missing-export":
				argument = string(binding.ProducingInvocationID)
			}
			raw, err := sql.Open("sqlite", p.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := raw.Close(); err != nil {
					t.Error(err)
				}
			}()
			if _, err := raw.ExecContext(p.ctx, damage.query, argument); err != nil {
				t.Fatal(err)
			}
			assertRealRunCheckpoint(t, p, true, "")
		})
	}
}

func TestRealRunStoppedCheckpointIsNotDismissedHistory(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("publish original: %#v, %v", result, err)
	}
	decideCurrentReady(t, p, domain.ActionStop)
	assertRealRunCheckpoint(t, p, true, "")
}

func decideCurrentReady(t *testing.T, p *productionPublicationHarness, action domain.Action) domain.ItemID {
	t.Helper()
	var id domain.ItemID
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		id, err = tx.CurrentProductionReadyItemID(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ready, err := p.attention.GetAttentionItem(p.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	const deviceID domain.DeviceID = "retained-history-device"
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{
			ID: deviceID, DisplayName: "Retained history device",
			Status: domain.DeviceActive, PairedAt: p.now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: "decide-retained-history", DeviceID: deviceID,
		ExpectedEntityVersion: ready.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: id, ItemVersion: ready.Item.ItemVersion,
			PRHeadSHA: ready.Item.PRHeadSHA, ArtifactDigests: ready.Item.ArtifactDigests,
			Action: action,
		},
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
