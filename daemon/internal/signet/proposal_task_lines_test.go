package signet_test

import (
	"bytes"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func proposalLineChoices() []domain.TaskLineChoice {
	return []domain.TaskLineChoice{{Role: domain.RoleSpecifier, Agent: "claude"}, {Role: domain.RoleImplementer, Agent: "codex"}}
}

func (f proposalDecisionFixture) taskLines(t *testing.T) []domain.TaskLine {
	t.Helper()
	var lines []domain.TaskLine
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		taskID, err := tx.ProposalTask(t.Context(), f.item.ID)
		if err != nil {
			return err
		}
		lines, err = tx.TaskLines(t.Context(), taskID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestProposalStartTaskLines(t *testing.T) {
	for _, action := range []domain.Action{domain.ActionStart, domain.ActionStartWithChanges} {
		t.Run(string(action), func(t *testing.T) {
			f := newProposalDecisionFixture(t)
			*f.now = f.now.Add(time.Hour)
			command := f.proposalCommand("start-lines", action)
			command.Payload.TaskLines = proposalLineChoices()
			if action == domain.ActionStartWithChanges {
				command.Payload.TaskProposalRevision = &signet.TaskProposalRevisionInput{
					Intent: domain.TaskProposalIntentImplement, ExpectedCostUnits: 5,
					Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
				}
			}
			first, err := f.service.Submit(t.Context(), command)
			if err != nil {
				t.Fatal(err)
			}
			lines := f.taskLines(t)
			if len(lines) != 2 {
				t.Fatalf("lines = %d, want 2", len(lines))
			}
			if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
				decision, err := tx.ProposalDecisionForInstance(t.Context(), f.instance.ID)
				if err != nil {
					return err
				}
				if decision == nil {
					t.Fatal("no recorded proposal decision")
				}
				taskID, err := tx.ProposalTask(t.Context(), command.Payload.ItemID)
				if err != nil {
					return err
				}
				for _, line := range lines {
					if line.TaskID != taskID || line.Source != domain.TaskLineSourceProposalStart ||
						line.SetBy != command.CommandID || !line.SetAt.Equal(decision.DecidedAt) {
						t.Fatalf("line = %+v, decision = %+v", line, decision)
					}
					got, found, err := engine.RunTaskLine(t.Context(), tx, f.policy.RunID, line.Role)
					if err != nil || !found || got.ID != line.ID {
						t.Fatalf("RunTaskLine = %s, %t, %v; want %s", got.ID, found, err, line.ID)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			*f.now = f.now.Add(time.Hour)
			command.Payload.TaskLines = slices.Clone(command.Payload.TaskLines)
			slices.Reverse(command.Payload.TaskLines)
			before := f.revision(t)
			replay, err := f.service.Submit(t.Context(), command)
			if err != nil || replay.Revision != first.Revision || f.revision(t) != before || len(f.taskLines(t)) != 2 {
				t.Fatalf("replay = %+v, %v; want unchanged result and lines", replay, err)
			}
			for _, changed := range [][]domain.TaskLineChoice{nil, {{Role: domain.RoleSpecifier, Agent: "other"}}} {
				command.Payload.TaskLines = changed
				if _, err := f.service.Submit(t.Context(), command); !errors.Is(err, store.ErrImmutableConflict) {
					t.Fatalf("changed replay = %v, want immutable conflict", err)
				}
			}
			if f.revision(t) != before || len(f.taskLines(t)) != 2 {
				t.Fatal("a conflicting replay wrote state")
			}
		})
	}
}

func TestProposalStartWithoutTaskLines(t *testing.T) {
	f := newProposalDecisionFixture(t)
	command := f.proposalCommand("start-no-lines", domain.ActionStart)
	first, err := f.service.Submit(t.Context(), command)
	if err != nil || len(f.taskLines(t)) != 0 {
		t.Fatalf("start without lines: %v", err)
	}
	if replay, err := f.service.Submit(t.Context(), command); err != nil || replay.Revision != first.Revision {
		t.Fatalf("lineless replay = %+v, %v", replay, err)
	}
	command.Payload.TaskLines = proposalLineChoices()
	if _, err := f.service.Submit(t.Context(), command); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("retry adding lines = %v, want immutable conflict", err)
	}
}

func (f proposalDecisionFixture) assertUnwrittenDecision(t *testing.T, command signet.ClientCommand) {
	t.Helper()
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetCommand(t.Context(), command.CommandID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("command lookup = %v, want not found", err)
		}
		decision, err := tx.ProposalDecisionForInstance(t.Context(), f.instance.ID)
		if err != nil {
			return err
		}
		if decision != nil {
			t.Fatalf("refused decision recorded %+v", decision)
		}
		item, err := tx.GetAttentionItem(t.Context(), f.item.ID)
		if err == nil && (item.Status != domain.StatusOpen || item.DecidedAt != nil) {
			t.Fatalf("refused decision concluded the card: %+v", item)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.taskLines(t)) != 0 {
		t.Fatal("refused decision recorded task lines")
	}
}

func TestProposalTaskLinesStaleDecisionWritesNothing(t *testing.T) {
	for _, staleBinding := range []bool{false, true} {
		t.Run(map[bool]string{false: "entity version", true: "item version"}[staleBinding], func(t *testing.T) {
			f := newProposalDecisionFixture(t)
			next := f.item
			next.ItemVersion++
			if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error { return tx.PutAttentionItem(t.Context(), next) }); err != nil {
				t.Fatal(err)
			}
			command := f.proposalCommand("stale-lines", domain.ActionStart)
			command.Payload.TaskLines = proposalLineChoices()
			if staleBinding {
				command.ExpectedEntityVersion = 2
			} else {
				command.Payload.ItemVersion = 2
			}
			before := f.revision(t)
			if _, err := f.service.Submit(t.Context(), command); !errors.Is(err, signet.ErrStaleVersion) {
				t.Fatalf("stale decision = %v", err)
			}
			if f.revision(t) != before {
				t.Fatal("stale decision advanced the revision")
			}
			f.assertUnwrittenDecision(t, command)
		})
	}
}

func TestDecisionTaskLinesRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action domain.Action
		lines  []domain.TaskLineChoice
	}{
		{"decline", domain.ActionDecline, proposalLineChoices()},
		{"snooze", domain.ActionSnooze, proposalLineChoices()},
		{"approve", domain.ActionApprove, proposalLineChoices()},
		{"empty", domain.ActionStart, []domain.TaskLineChoice{}},
		{"duplicate", domain.ActionStart, []domain.TaskLineChoice{{Role: domain.RoleSpecifier, Agent: "a"}, {Role: domain.RoleSpecifier, Agent: "b"}}},
		{"ineligible role", domain.ActionStart, []domain.TaskLineChoice{{Role: domain.RoleShadowReviewer, Agent: "a"}}},
		{"malformed agent", domain.ActionStart, []domain.TaskLineChoice{{Role: domain.RoleSpecifier, Agent: "bad name"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProposalDecisionFixture(t)
			command := f.proposalCommand("bad-lines", tc.action)
			command.Payload.TaskLines = tc.lines
			before := f.revision(t)
			if _, err := f.service.Submit(t.Context(), command); !errors.Is(err, signet.ErrInvalidProposalDecisionPayload) {
				t.Fatalf("invalid lines = %v", err)
			}
			if f.revision(t) != before {
				t.Fatal("invalid lines advanced the revision")
			}
			f.assertUnwrittenDecision(t, command)
		})
	}
}

func TestDecisionHTTPTaskLines(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		lines        []map[string]string
		status       int
	}{
		{"start", "start", []map[string]string{{"role": "specifier", "agent": "codex"}}, http.StatusOK},
		{"empty", "start", []map[string]string{}, http.StatusBadRequest},
		{"decline", "decline", []map[string]string{{"role": "specifier", "agent": "codex"}}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProposalDecisionFixture(t)
			handler := signet.NewHTTPHandler(f.service, testAuthorizer)
			body := mustJSON(map[string]any{
				"command_id": "http-lines", "device_id": f.device.ID, "expected_entity_version": 1,
				"expected_bindings": map[string]string{},
				"payload": map[string]any{
					"kind": "decision", "item_id": f.item.ID, "action": tc.action, "item_version": f.item.ItemVersion,
					"pr_head_sha": "", "artifact_digests": f.item.ArtifactDigests, "task_lines": tc.lines,
				},
			})
			before := f.revision(t)
			response := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader(body))
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
			if tc.status == http.StatusBadRequest {
				if f.revision(t) != before {
					t.Fatal("HTTP refusal wrote state")
				}
				f.assertUnwrittenDecision(t, f.proposalCommand("http-lines", domain.Action(tc.action)))
			}
		})
	}
}
