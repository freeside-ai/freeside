package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

var receiptAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func receiptIdentity(id string) domain.CommandIdentity {
	return domain.CommandIdentity{CommandID: id, DeviceID: "device-1"}
}

func receiptTaskLine(t *testing.T) domain.TaskLine {
	t.Helper()
	return receiptTaskLineOf(t, "task-1", domain.TaskLineSourceClientLineChange)
}

func receiptTaskLineOf(t *testing.T, task domain.TaskID, source domain.TaskLineSource) domain.TaskLine {
	t.Helper()
	line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: task, Role: domain.RoleImplementer, Agent: "codex-primary",
		Source: source,
		SetBy:  "sha256:" + strings.Repeat("1", 64),
	}, 2, new(domain.Digest("sha256:"+strings.Repeat("2", 64))), receiptAt)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func receiptPolicy(t *testing.T, revision int) domain.RoutingPolicy {
	t.Helper()
	policy, err := domain.NewRoutingPolicy(domain.RoutingPolicyInput{
		Roles: []domain.RoutingPolicyRole{{
			Role: domain.RoleImplementer, Enrollments: []domain.ClientEnrollmentID{"codex-primary/codex", "claude-primary/claude_code"},
		}},
		Source: domain.RoutingPolicySourceClientSet, SetBy: "device:device-1",
		LineupRevision: domain.Digest("sha256:" + strings.Repeat("c", 64)),
	}, revision, receiptAt)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// receiptParkedTaskMove is a move of task-1's implementer to claude-b's
// enrollment on pool-b at the lineup's model and effort, with the record and
// line it wrote.
func receiptParkedTaskMove(t *testing.T) domain.CommandReceipt {
	t.Helper()
	selection := &domain.TaskLineSelection{EnrollmentID: "claude-b/claude_code", ModelID: "claude-opus-4-5", Effort: "high"}
	record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
		TaskID: "task-1", Role: domain.RoleImplementer, FailedInvocationID: "inv-refused",
		CommandID: "cmd-move", Selection: selection, Pool: "pool-b",
		Qualification: domain.AlternateAgentDifferentPool, RecordedAt: receiptAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleImplementer, Selection: selection,
		Source: domain.TaskLineSourceAlternateAgentMove, SetBy: string(record.ID),
		Provenance: &domain.TaskLineProvenance{
			PolicyRevision: 1, Tier: domain.TaskLineSelectionPolicyOrder, Pool: "pool-b",
			RankedBy: []domain.TaskLineRankingReading{},
		},
	}, 1, nil, receiptAt)
	if err != nil {
		t.Fatal(err)
	}
	return domain.CommandReceipt{ParkedTaskMove: &domain.ParkedTaskMoveReceipt{
		ParkedTaskMoveRequest: domain.ParkedTaskMoveRequest{
			CommandIdentity: receiptIdentity("cmd-move"), TaskID: "task-1",
			Choice: &domain.AlternateAgentChoice{EnrollmentID: "claude-b/claude_code"},
		},
		Retry: record, Lines: []domain.TaskLine{line},
	}}
}

// receiptParkedTaskMoveAgent is the move as stored under #2735: its request
// names the tree agent the wire sent and the selection it reduced to.
func receiptParkedTaskMoveAgent(t *testing.T) domain.CommandReceipt {
	t.Helper()
	receipt := *receiptParkedTaskMove(t).ParkedTaskMove
	receipt.Choice, receipt.Agent, receipt.Selection = nil, "claude-b", receipt.Retry.Selection
	return domain.CommandReceipt{ParkedTaskMove: &receipt}
}

// receiptParkedTaskMoveV1 is the move as stored before #2735: its record
// names the tree agent, and its request carries no selection. No writer
// builds one now, so the fixture computes the record's id itself.
func receiptParkedTaskMoveV1(t *testing.T) domain.CommandReceipt {
	t.Helper()
	record := domain.AlternateAgentRetry{
		TaskID: "task-1", Role: domain.RoleImplementer, FailedInvocationID: "inv-refused",
		CommandID: "cmd-move", Agent: "claude-b", Pool: "pool-b",
		Qualification: domain.AlternateAgentDifferentPool, RecordedAt: receiptAt,
	}
	var err error
	if record.ID, err = record.ComputeID(); err != nil {
		t.Fatal(err)
	}
	line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleImplementer, Agent: "claude-b",
		Source: domain.TaskLineSourceAlternateAgentMove, SetBy: string(record.ID),
		Provenance: &domain.TaskLineProvenance{
			PolicyRevision: 1, Tier: domain.TaskLineSelectionPolicyOrder, Pool: "pool-b",
			RankedBy: []domain.TaskLineRankingReading{},
		},
	}, 1, nil, receiptAt)
	if err != nil {
		t.Fatal(err)
	}
	return domain.CommandReceipt{ParkedTaskMove: &domain.ParkedTaskMoveReceipt{
		ParkedTaskMoveRequest: domain.ParkedTaskMoveRequest{
			CommandIdentity: receiptIdentity("cmd-move"), TaskID: "task-1", Agent: "claude-b",
		},
		Retry: record, Lines: []domain.TaskLine{line},
	}}
}

// receiptSelection is the selection a change since #2598 sends and its v2
// line records.
var receiptSelection = domain.TaskLineSelection{
	EnrollmentID: "openai-a/codex_cli", ModelID: "gpt-5.5-codex", Effort: "high",
}

func receiptSelectionLine(t *testing.T) domain.TaskLine {
	t.Helper()
	selection := receiptSelection
	line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleImplementer, Selection: &selection,
		Source: domain.TaskLineSourceClientLineChange, SetBy: "cmd-selection",
	}, 1, nil, receiptAt)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func validReceipts(t *testing.T) map[string]domain.CommandReceipt {
	t.Helper()
	line := receiptTaskLine(t)
	selectionLine := receiptSelectionLine(t)
	return map[string]domain.CommandReceipt{
		"usage_refresh_receipt": {UsageRefresh: &domain.UsageRefreshReceipt{
			UsageRefreshRequest: domain.UsageRefreshRequest{CommandIdentity: receiptIdentity("cmd-refresh"), Pool: "pool-a"},
			RequestedAt:         receiptAt,
		}},
		"collector_reassignment_receipt": {CollectorReassignment: &domain.CollectorReassignmentReceipt{
			CollectorReassignmentRequest: domain.CollectorReassignmentRequest{
				CommandIdentity: receiptIdentity("cmd-reassign"), Pool: "pool-a", Collector: "enrollment-2",
			},
			RecordedAt: receiptAt,
		}},
		"task_line_change_receipt": {TaskLineChange: &domain.TaskLineChangeReceipt{
			TaskLineChangeRequest: domain.TaskLineChangeRequest{
				CommandIdentity: receiptIdentity("cmd-line"), TaskID: "task-1",
				Role: domain.RoleImplementer, Agent: new("codex-primary"),
			},
			Recorded: true, Line: &line,
		}},
		"task_line_selection_receipt": {TaskLineChange: &domain.TaskLineChangeReceipt{
			TaskLineChangeRequest: domain.TaskLineChangeRequest{
				CommandIdentity: receiptIdentity("cmd-selection"), TaskID: "task-1",
				Role: domain.RoleImplementer, Selection: &receiptSelection,
			},
			Recorded: true, Line: &selectionLine,
		}},
		"task_line_clear_receipt": {TaskLineChange: &domain.TaskLineChangeReceipt{
			TaskLineChangeRequest: domain.TaskLineChangeRequest{
				CommandIdentity: receiptIdentity("cmd-clear"), TaskID: "task-1", Role: domain.RoleImplementer,
			},
			Recorded: true,
		}},
		"parked_task_move_receipt":       receiptParkedTaskMove(t),
		"parked_task_move_receipt_agent": receiptParkedTaskMoveAgent(t),
		"parked_task_move_receipt_v1":    receiptParkedTaskMoveV1(t),
		"routing_policy_change_receipt": {RoutingPolicyChange: &domain.RoutingPolicyChangeReceipt{
			RoutingPolicyChangeRequest: domain.RoutingPolicyChangeRequest{
				CommandIdentity: receiptIdentity("cmd-policy"), ExpectedRevision: new(1),
			},
			Recorded: true, Policy: receiptPolicy(t, 2),
		}},
		"workflow_policy_change_receipt": {WorkflowPolicyChange: &domain.WorkflowPolicyChangeReceipt{
			WorkflowPolicyChangeRequest: domain.WorkflowPolicyChangeRequest{
				CommandIdentity: receiptIdentity("cmd-workflow"), Scope: domain.WorkflowPolicyScopeProject,
				ProjectID: new(domain.ProjectID("proj-1")), Key: domain.WorkflowGateSourceIssueClosure,
				Value: new("true"), ExpectedRevision: new(3),
			},
			Recorded: true, Revision: 4,
		}},
	}
}

// TestCommandReceiptGoldens pins each receipt arm's stored body; every
// fixture is also a validation-positive case.
func TestCommandReceiptGoldens(t *testing.T) {
	for name, receipt := range validReceipts(t) {
		t.Run(name, func(t *testing.T) {
			if err := receipt.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			var arm any
			switch receipt.Kind() {
			case domain.CommandKindRefreshUsage:
				arm = receipt.UsageRefresh
			case domain.CommandKindReassignCollector:
				arm = receipt.CollectorReassignment
			case domain.CommandKindChangeTaskLine:
				arm = receipt.TaskLineChange
			case domain.CommandKindSetRoutingPolicy:
				arm = receipt.RoutingPolicyChange
			case domain.CommandKindMoveParkedTask:
				arm = receipt.ParkedTaskMove
			case domain.CommandKindSetWorkflowPolicy:
				arm = receipt.WorkflowPolicyChange
			case domain.CommandKindDecision, domain.CommandKindSubmitTask, domain.CommandKindStopTask:
				t.Fatalf("kind %q", receipt.Kind())
			}
			body, err := json.MarshalIndent(arm, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, name, append(body, '\n'))
		})
	}
}

func TestCommandReceiptRefusals(t *testing.T) {
	otherTask := receiptTaskLineOf(t, "task-2", domain.TaskLineSourceClientLineChange)
	cli := receiptTaskLineOf(t, "task-1", domain.TaskLineSourceCLILineChange)
	lineChange := func(edit func(*domain.TaskLineChangeReceipt)) domain.CommandReceipt {
		receipt := *validReceipts(t)["task_line_change_receipt"].TaskLineChange
		edit(&receipt)
		return domain.CommandReceipt{TaskLineChange: &receipt}
	}
	selectionChange := func(edit func(*domain.TaskLineChangeReceipt)) domain.CommandReceipt {
		receipt := *validReceipts(t)["task_line_selection_receipt"].TaskLineChange
		edit(&receipt)
		return domain.CommandReceipt{TaskLineChange: &receipt}
	}
	policyChange := func(edit func(*domain.RoutingPolicyChangeReceipt)) domain.CommandReceipt {
		receipt := *validReceipts(t)["routing_policy_change_receipt"].RoutingPolicyChange
		edit(&receipt)
		return domain.CommandReceipt{RoutingPolicyChange: &receipt}
	}
	workflowChange := func(edit func(*domain.WorkflowPolicyChangeReceipt)) domain.CommandReceipt {
		receipt := *validReceipts(t)["workflow_policy_change_receipt"].WorkflowPolicyChange
		edit(&receipt)
		return domain.CommandReceipt{WorkflowPolicyChange: &receipt}
	}
	refresh := validReceipts(t)["usage_refresh_receipt"]
	for _, tc := range []struct {
		name    string
		receipt domain.CommandReceipt
		want    error
	}{
		{"no arm", domain.CommandReceipt{}, domain.ErrCommandReceiptArms},
		{"two arms", domain.CommandReceipt{
			UsageRefresh: refresh.UsageRefresh, TaskLineChange: lineChange(func(*domain.TaskLineChangeReceipt) {}).TaskLineChange,
		}, domain.ErrCommandReceiptArms},
		{"no device", lineChange(func(r *domain.TaskLineChangeReceipt) { r.DeviceID = "" }), domain.ErrEmptyID},
		{
			"ineligible role", lineChange(func(r *domain.TaskLineChangeReceipt) { r.Role = "verifier" }),
			domain.ErrTaskLineRoleIneligible,
		},
		{
			"line for another task", lineChange(func(r *domain.TaskLineChangeReceipt) { r.Line = &otherTask }),
			domain.ErrParentKeyMismatch,
		},
		{
			"no line for an agent", lineChange(func(r *domain.TaskLineChangeReceipt) { r.Line = nil }),
			domain.ErrParentKeyMismatch,
		},
		{
			"line for a clear", lineChange(func(r *domain.TaskLineChangeReceipt) { r.Agent = nil }),
			domain.ErrParentKeyMismatch,
		},
		{
			"no line for a selection", selectionChange(func(r *domain.TaskLineChangeReceipt) { r.Line = nil }),
			domain.ErrParentKeyMismatch,
		},
		{
			"line holding another selection", selectionChange(func(r *domain.TaskLineChangeReceipt) {
				other := receiptSelection
				other.Effort = "max"
				r.Selection = &other
			}),
			domain.ErrParentKeyMismatch,
		},
		{
			"v1 line for a selection", selectionChange(func(r *domain.TaskLineChangeReceipt) {
				v1 := receiptTaskLineOf(t, "task-1", domain.TaskLineSourceClientLineChange)
				r.Line = &v1
			}),
			domain.ErrParentKeyMismatch,
		},
		{
			"agent and selection", selectionChange(func(r *domain.TaskLineChangeReceipt) { r.Agent = new("codex-primary") }),
			domain.ErrInvalidTaskLineSelection,
		},
		{
			"recorded under another source", lineChange(func(r *domain.TaskLineChangeReceipt) { r.Line = &cli }),
			domain.ErrInvalidTaskLineSource,
		},
		{"recorded revision skips one", policyChange(func(r *domain.RoutingPolicyChangeReceipt) {
			r.ExpectedRevision = nil
		}), domain.ErrParentKeyMismatch},
		{"unrecorded revision moved", policyChange(func(r *domain.RoutingPolicyChangeReceipt) {
			r.Recorded = false
		}), domain.ErrParentKeyMismatch},
		{"non-positive expected revision", policyChange(func(r *domain.RoutingPolicyChangeReceipt) {
			r.ExpectedRevision = new(0)
		}), domain.ErrNonPositive},
		{"workflow recorded revision skips one", workflowChange(func(r *domain.WorkflowPolicyChangeReceipt) {
			r.Revision = 5
		}), domain.ErrParentKeyMismatch},
		{"workflow unrecorded revision moved", workflowChange(func(r *domain.WorkflowPolicyChangeReceipt) {
			r.Recorded = false
		}), domain.ErrParentKeyMismatch},
		{"workflow unknown key", workflowChange(func(r *domain.WorkflowPolicyChangeReceipt) {
			r.Key = "gates.spec_approval"
		}), domain.ErrInvalidWorkflowPolicyKey},
		{"workflow general scope with project", workflowChange(func(r *domain.WorkflowPolicyChangeReceipt) {
			r.Scope = domain.WorkflowPolicyScopeGeneral
		}), domain.ErrWorkflowPolicyProjectMismatch},
		{"workflow bad gate value", workflowChange(func(r *domain.WorkflowPolicyChangeReceipt) {
			r.Value = new("yes")
		}), domain.ErrInvalidWorkflowPolicyValue},
		{"workflow non-positive expected revision", workflowChange(func(r *domain.WorkflowPolicyChangeReceipt) {
			r.ExpectedRevision = new(0)
		}), domain.ErrNonPositive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.receipt.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTaskLineChangeRequestSame(t *testing.T) {
	base := domain.TaskLineChangeRequest{
		CommandIdentity: receiptIdentity("cmd-line"), TaskID: "task-1", Role: domain.RoleImplementer,
		Agent: new("codex-primary"),
	}
	if !base.Same(domain.TaskLineChangeRequest{
		CommandIdentity: base.CommandIdentity, TaskID: "task-1", Role: domain.RoleImplementer,
		Agent: new("codex-primary"),
	}) {
		t.Fatal("equal requests with distinct agent pointers differ")
	}
	clear := base
	clear.Agent = nil
	if base.Same(clear) || clear.Same(base) {
		t.Fatal("a clear matches a change to an agent")
	}
}
