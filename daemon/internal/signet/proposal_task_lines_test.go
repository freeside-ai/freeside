package signet_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestDecisionRefusesTaskLines holds the refusal that stands until proposal
// starts apply task lines (#1641): a decision that carries the field is
// rejected whatever its action, records no command, and leaves the card
// open, so a start can never run on the lineup while the operator believes
// an agent was chosen.
func TestDecisionRefusesTaskLines(t *testing.T) {
	ctx := context.Background()
	revision := signet.TaskProposalRevisionInput{
		Intent:            domain.TaskProposalIntentImplement,
		ExpectedCostUnits: 5, Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}
	chosen := []domain.TaskLineChoice{{Role: domain.RoleImplementer, Agent: "codex"}}
	for _, tc := range []struct {
		name   string
		action domain.Action
		lines  []domain.TaskLineChoice
	}{
		{"start", domain.ActionStart, chosen},
		{"start with changes", domain.ActionStartWithChanges, chosen},
		{"decline", domain.ActionDecline, chosen},
		{"empty list", domain.ActionStart, []domain.TaskLineChoice{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProposalDecisionFixture(t)
			command := f.proposalCommand("command-lines", tc.action)
			if tc.action == domain.ActionStartWithChanges {
				command.Payload.TaskProposalRevision = &revision
			}
			withLines := command
			withLines.Payload.TaskLines = tc.lines
			before := f.revision(t)
			if _, err := f.service.Submit(ctx, withLines); !errors.Is(err, signet.ErrInvalidProposalDecisionPayload) {
				t.Fatalf("error = %v, want ErrInvalidProposalDecisionPayload", err)
			}
			if after := f.revision(t); after != before {
				t.Fatalf("revision moved %d to %d on a refused decision", before, after)
			}
			if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
				if _, err := tx.GetCommand(ctx, command.CommandID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("command lookup = %v, want ErrNotFound", err)
				}
				item, err := tx.GetAttentionItem(ctx, f.item.ID)
				if err != nil {
					return err
				}
				if item.Status != f.item.Status || item.DecidedAt != nil {
					t.Fatalf("item = status %q decided %v, want the open card", item.Status, item.DecidedAt)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			// The same decision without lines is accepted as before, and a
			// retry that adds lines to the recorded command is still refused.
			if _, err := f.service.Submit(ctx, command); err != nil {
				t.Fatalf("decision without lines: %v", err)
			}
			if _, err := f.service.Submit(ctx, withLines); !errors.Is(err, signet.ErrInvalidProposalDecisionPayload) {
				t.Fatalf("retry with lines: error = %v, want ErrInvalidProposalDecisionPayload", err)
			}
		})
	}
}

// TestDecisionHTTPRefusesTaskLines covers the wire path: the decision arm
// decodes task_lines and the daemon answers 400, where an undeclared field
// would also answer 400 but for the wrong reason once #1641 accepts it.
func TestDecisionHTTPRefusesTaskLines(t *testing.T) {
	f := newProposalDecisionFixture(t)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	payload := map[string]any{
		"kind": "decision", "item_id": f.item.ID, "action": "start", "item_version": f.item.ItemVersion,
		"pr_head_sha": "", "artifact_digests": f.item.ArtifactDigests,
	}
	post := func(commandID string) int {
		t.Helper()
		body := mustJSON(map[string]any{
			"command_id": commandID, "device_id": f.device.ID, "expected_entity_version": 1,
			"expected_bindings": map[string]string{}, "payload": payload,
		})
		return authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader(body)).Code
	}
	for name, lines := range map[string]any{
		"one line":   []map[string]string{{"role": "implementer", "agent": "codex"}},
		"empty list": []map[string]string{},
	} {
		payload["task_lines"] = lines
		if status := post("command-http-lines"); status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, status)
		}
	}
	delete(payload, "task_lines")
	if status := post("command-http-lines"); status != http.StatusOK {
		t.Fatalf("start without lines: status = %d, want 200", status)
	}
}
