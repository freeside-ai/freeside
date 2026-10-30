package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// errTaskLineUnchanged rolls the write back when the role already has the
// requested line, so a repeated request records nothing and moves no
// revision.
var errTaskLineUnchanged = errors.New("the role already has the requested line")

// ChangeTaskLineRequest is one operator change to a task's line for a role:
// an agent to move the role to, or a clear that returns it to the lineup.
type ChangeTaskLineRequest struct {
	TaskID domain.TaskID
	Role   domain.RoleName
	Agent  string
	Clear  bool
}

func (r ChangeTaskLineRequest) validate() error {
	if !r.Clear {
		return domain.ValidateTaskLineChoices([]domain.TaskLineChoice{{Role: r.Role, Agent: r.Agent}})
	}
	if !r.Role.TaskLineEligible() {
		return fmt.Errorf("task line role: %w", domain.ErrTaskLineRoleIneligible)
	}
	if r.Agent != "" {
		return fmt.Errorf("task line %s: a clear names no agent: %w", r.Role, domain.ErrTaskLineInconsistent)
	}
	return nil
}

// sameLine reports whether the role's newest version already is the request.
// head is nil when the role has no line.
func (r ChangeTaskLineRequest) sameLine(head *domain.TaskLine) bool {
	if r.Clear {
		return head == nil || head.Cleared
	}
	return head != nil && !head.Cleared && head.Agent == r.Agent
}

// TaskLineChangeResult reports a change. Recorded is false when the role
// already had the requested line. Line is the role's newest version after the
// request, nil when it has never had one.
type TaskLineChangeResult struct {
	Recorded bool
	Line     *domain.TaskLine
}

// ChangeTaskLine records a change to a task's line for a role, unless the
// role already has that line. An attempt of the role running refuses it with
// domain.ErrTaskLineRoleRunning and records nothing: that attempt holds the
// line that authorized it, and a change must not read as applying to it.
//
// The agent isn't resolved here. Admission resolves it at the admitting
// revision, as it does a line set at submission, and refuses without falling
// back to the lineup.
//
// The change applies to attempts admitted after it commits. This write
// serializes with other writes but not with an admission's earlier read, so
// an admission that read the old line before the commit may still run on it.
func ChangeTaskLine(
	ctx context.Context, st *store.Store, req ChangeTaskLineRequest, now time.Time,
) (TaskLineChangeResult, error) {
	if err := req.validate(); err != nil {
		return TaskLineChangeResult{}, err
	}
	var result TaskLineChangeResult
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.GetTask(ctx, req.TaskID); err != nil {
			return err
		}
		heads, err := tx.TaskLineHeads(ctx, req.TaskID)
		if err != nil {
			return err
		}
		var replaces *domain.Digest
		if head, ok := heads[req.Role]; ok {
			result.Line, replaces = &head, &head.ID
		}
		if req.sameLine(result.Line) {
			return errTaskLineUnchanged
		}
		running, err := taskRoleAttemptRunning(ctx, &tx.ReadTx, req.TaskID, req.Role)
		if err != nil {
			return err
		}
		if running {
			return fmt.Errorf("task %s role %s: %w", req.TaskID, req.Role, domain.ErrTaskLineRoleRunning)
		}
		change, err := domain.NewTaskLineChange(req.TaskID, req.Role, req.Agent, req.Clear, replaces, now)
		if err != nil {
			return err
		}
		line, err := tx.RecordTaskLineChange(ctx, change)
		if err != nil {
			return err
		}
		result = TaskLineChangeResult{Recorded: true, Line: &line}
		return nil
	})
	if errors.Is(err, errTaskLineUnchanged) {
		return result, nil
	}
	if err != nil {
		return TaskLineChangeResult{}, err
	}
	return result, nil
}

// taskRoleAttemptRunning reports whether an attempt of the role runs on any
// of the task's runs. A writer role's attempt runs from its execution
// admission until its export or outcome is recorded, a reviewer's from its
// review request until its review record or failure is. A review refused at
// admission never requests, so the operator can change that line.
//
// A closed workflow episode holds no line. Nothing advances it, so an attempt
// or review left open there may never record its end, and a line held on it
// would stay held through the reattempt that recovers the task. An attempt
// still finishing after the close keeps the line version it cited.
func taskRoleAttemptRunning(
	ctx context.Context, tx *store.ReadTx, taskID domain.TaskID, role domain.RoleName,
) (bool, error) {
	runIDs, err := tx.TaskRunIDs(ctx, taskID)
	if err != nil {
		return false, err
	}
	for _, runID := range runIDs {
		if err := tx.RequireRunWorkflowOpen(ctx, runID); errors.Is(err, store.ErrTaskWorkflowClosed) {
			continue
		} else if err != nil {
			return false, err
		}
		running, err := runRoleAttemptRunning(ctx, tx, runID, role)
		if err != nil || running {
			return running, err
		}
	}
	return false, nil
}

func runRoleAttemptRunning(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID, role domain.RoleName,
) (bool, error) {
	if role == domain.RoleReviewer {
		return runReviewOpen(ctx, tx, runID)
	}
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return false, err
	}
	attempts, err := tx.OpenRunExecutionAttempts(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, attempt := range attempts {
		index := slices.IndexFunc(run.Stages, func(stage domain.Stage) bool { return stage.ID == attempt.StageID })
		if index < 0 {
			return false, fmt.Errorf("attempt %s names stage %s, which run %s lacks: %w",
				attempt.InvocationID, attempt.StageID, runID, domain.ErrParentKeyMismatch)
		}
		if attemptRole, ok := wardRole(run, run.Stages[index], attempt.InvocationID); ok && attemptRole == role {
			return true, nil
		}
	}
	return false, nil
}

// runReviewOpen reports whether a primary review of the run was requested and
// has neither a record nor a failure. Only the primary reviewer writes
// review_requests; a shadow review derives its invocation from the round's,
// and the shadow reviewer takes no task line.
func runReviewOpen(ctx context.Context, tx *store.ReadTx, runID domain.RunID) (bool, error) {
	requests, err := tx.ListReviewRequests(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, request := range requests {
		_, err := tx.GetReviewRecord(ctx, request.InvocationID)
		if err == nil {
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		if _, err := tx.GetReviewFailure(ctx, request.InvocationID); errors.Is(err, store.ErrNotFound) {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	return false, nil
}
