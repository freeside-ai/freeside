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
		Roles:  []domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Agents: []string{"codex-primary", "claude-primary"}}},
		Source: domain.RoutingPolicySourceClientSet, SetBy: "device:device-1",
		LineupRevision: domain.Digest("sha256:" + strings.Repeat("c", 64)),
	}, revision, receiptAt)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func validReceipts(t *testing.T) map[string]domain.CommandReceipt {
	t.Helper()
	line := receiptTaskLine(t)
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
		"task_line_clear_receipt": {TaskLineChange: &domain.TaskLineChangeReceipt{
			TaskLineChangeRequest: domain.TaskLineChangeRequest{
				CommandIdentity: receiptIdentity("cmd-clear"), TaskID: "task-1", Role: domain.RoleImplementer,
			},
			Recorded: true,
		}},
		"routing_policy_change_receipt": {RoutingPolicyChange: &domain.RoutingPolicyChangeReceipt{
			RoutingPolicyChangeRequest: domain.RoutingPolicyChangeRequest{
				CommandIdentity: receiptIdentity("cmd-policy"), ExpectedRevision: new(1),
			},
			Recorded: true, Policy: receiptPolicy(t, 2),
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
	policyChange := func(edit func(*domain.RoutingPolicyChangeReceipt)) domain.CommandReceipt {
		receipt := *validReceipts(t)["routing_policy_change_receipt"].RoutingPolicyChange
		edit(&receipt)
		return domain.CommandReceipt{RoutingPolicyChange: &receipt}
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
