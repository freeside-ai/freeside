package signet

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The operator commands (refresh_usage, reassign_collector, change_task_line,
// set_routing_policy, move_parked_task, set_workflow_policy) bind to no entity version: each records a
// domain.CommandReceipt in the transaction that applies it, and a replay of
// its command_id returns that receipt. Their effects live in the engine and,
// for usage, in the collection lifecycle, which import signet, so each reaches
// signet through an injected hook.

// RefreshUsagePayload mirrors the API's RefreshUsagePayload.
type RefreshUsagePayload struct {
	Pool domain.UsagePoolID `json:"pool"`
}

// ReassignCollectorPayload mirrors the API's ReassignCollectorPayload.
type ReassignCollectorPayload struct {
	Pool         domain.UsagePoolID        `json:"pool"`
	EnrollmentID domain.ClientEnrollmentID `json:"enrollment_id"`
}

// ChangeTaskLinePayload mirrors the API's ChangeTaskLinePayload; a nil
// Selection clears the line.
type ChangeTaskLinePayload struct {
	TaskID    domain.TaskID             `json:"task_id"`
	Role      domain.RoleName           `json:"role"`
	Selection *domain.TaskLineSelection `json:"selection"`
}

// SetRoutingPolicyPayload mirrors the API's SetRoutingPolicyPayload; a nil
// ExpectedRevision says the client saw no policy, and a nil
// DrainFirstAccount names none, clearing any the current policy names.
type SetRoutingPolicyPayload struct {
	Roles             []RoutingPolicyRoleInput `json:"roles"`
	DrainFirstAccount *domain.UsagePoolID      `json:"drain_first_account"`
	ExpectedRevision  *int                     `json:"expected_revision"`
}

// RoutingPolicyRoleInput mirrors the API's RoutingPolicyRoleInput: one
// role's order of enrollment ids. It has no agents key, so the strict
// decoder refuses a request that orders agent names.
type RoutingPolicyRoleInput struct {
	Role        domain.RoleName             `json:"role"`
	Enrollments []domain.ClientEnrollmentID `json:"enrollments"`
}

// MoveParkedTaskPayload mirrors the API's MoveParkedTaskPayload: the task
// parked on an exhausted quota window and the choice of an offered
// enrollment to move it to.
type MoveParkedTaskPayload struct {
	TaskID domain.TaskID         `json:"task_id"`
	Choice *AlternateAgentChoice `json:"choice"`
}

// ErrInvalidOperatorCommandPayload rejects a malformed operator command, or
// one carrying the decision envelope or another kind's payload.
var ErrInvalidOperatorCommandPayload = errors.New("operator command payload is invalid")

// ErrUsageOperationUnavailable answers refresh_usage and reassign_collector
// on a service composed without a UsageOperator, as ErrTaskSubmissionUnavailable
// answers submit_task: the command fails closed and records nothing.
var ErrUsageOperationUnavailable = errors.New("usage operation is not available")

// ErrOperatorSettingUnavailable answers change_task_line,
// set_routing_policy, move_parked_task, set_workflow_policy, and a retry_with_alternate_agent
// decision on a service composed without their hook.
var ErrOperatorSettingUnavailable = errors.New("operator setting is not available")

// UsageOperator runs a pool's refresh or collector reassignment
// (WithUsageOperator). The collection lifecycle owns what each does; the
// command records only that it was accepted. Each method runs in the
// accepting transaction and writes there whatever must commit with the
// receipt. It returns after, nil when there is nothing more, which signet
// runs once after that transaction commits and never when it rolls back or
// the command replays: an effect outside the store, such as starting a
// collection, must not run for a command that recorded nothing.
type UsageOperator interface {
	RefreshUsage(ctx context.Context, tx *store.WriteTx, pool domain.UsagePoolID, now time.Time) (after func(), err error)
	ReassignCollector(
		ctx context.Context, tx *store.WriteTx, pool domain.UsagePoolID, collector domain.ClientEnrollmentID, now time.Time,
	) (after func(), err error)
}

// TaskLineChange is one line change for the TaskLineChanger: Selection is
// the client's, checked against the store (validateLineSelection), nil
// exactly when Clear.
type TaskLineChange struct {
	TaskID    domain.TaskID
	Role      domain.RoleName
	Selection *domain.TaskLineSelection
	Clear     bool
	Source    domain.TaskLineSource
}

// TaskLineChangeOutcome is the role's newest line after the change, nil when
// it never had one. Recorded is false when the role already had the line.
type TaskLineChangeOutcome struct {
	Recorded bool
	Line     *domain.TaskLine
}

// TaskLineChanger applies a line change in the accepting transaction
// (engine.ChangeTaskLineTx, bound by WithTaskLineChanger). It refuses a role
// whose attempt runs with domain.ErrTaskLineRoleRunning and an unknown task
// with store.ErrNotFound.
type TaskLineChanger func(ctx context.Context, tx *store.WriteTx, change TaskLineChange, now time.Time) (TaskLineChangeOutcome, error)

// RoutingPolicyChange is one policy change for the RoutingPolicySetter.
type RoutingPolicyChange struct {
	Roles             []domain.RoutingPolicyRole
	DrainFirstAccount *domain.UsagePoolID
	Source            domain.RoutingPolicySource
	SetBy             string
}

// RoutingPolicyOutcome is the policy current after the change. Recorded is
// false when it already held the orders and account.
type RoutingPolicyOutcome struct {
	Recorded bool
	Policy   domain.RoutingPolicy
}

// RoutingPolicySetter applies a policy change in the accepting transaction
// (engine.SetRoutingPolicyTx against the daemon's agent tree, bound by
// WithRoutingPolicySetter). The change's orders are the request's enrollment
// ids in canonical form; the setter refuses one the store does not hold, a
// disabled one, and one that cannot run its role's lineup model and effort.
type RoutingPolicySetter func(ctx context.Context, tx *store.WriteTx, change RoutingPolicyChange, now time.Time) (RoutingPolicyOutcome, error)

// ParkedTaskMover moves a parked task in the accepting transaction
// (engine.AgentSelection.MoveParkedTaskTx, bound by WithParkedTaskMover). It
// refuses a task with no parked attempt with ErrTaskNotParked, an enrollment
// the park does not offer now with ErrAlternateAgentNotQualified, and a model
// or effort the enrollment does not serve with
// ErrAlternateAgentSelectionUnserved.
type ParkedTaskMover func(
	ctx context.Context, tx *store.WriteTx, request domain.ParkedTaskMoveRequest, now time.Time,
) (domain.ParkedTaskMoveReceipt, error)

// AlternateAgentRetrier records an accepted retry_with_alternate_agent
// choice in the decision's transaction
// (engine.AgentSelection.AcceptAlternateAgentRetryTx, bound by
// WithAlternateAgentRetrier). It refuses an agent that no longer qualifies
// with ErrAlternateAgentNotQualified.
type AlternateAgentRetrier func(
	ctx context.Context, tx *store.WriteTx, command domain.Command, item domain.AttentionItem, now time.Time,
) (domain.AlternateAgentRetry, error)

func WithParkedTaskMover(mover ParkedTaskMover) Option {
	return func(s *Service) { s.parkedTaskMover = mover }
}

func WithAlternateAgentRetrier(retrier AlternateAgentRetrier) Option {
	return func(s *Service) { s.alternateAgentRetrier = retrier }
}

func WithUsageOperator(operator UsageOperator) Option {
	return func(s *Service) { s.usageOperator = operator }
}

func WithTaskLineChanger(changer TaskLineChanger) Option {
	return func(s *Service) { s.taskLineChanger = changer }
}

func WithRoutingPolicySetter(setter RoutingPolicySetter) Option {
	return func(s *Service) { s.routingPolicySetter = setter }
}

// TaskLineChangeRejectionError refuses a line change for a role whose attempt
// runs, with the current task projection and epoch.
type TaskLineChangeRejectionError struct {
	Reason          string       `json:"reason"`
	ReplacementTask TaskSnapshot `json:"replacement_task"`
	SyncEpoch       string       `json:"sync_epoch"`
}

func (*TaskLineChangeRejectionError) Error() string { return "an attempt of the role is running" }

// RoutingPolicyRejectionError refuses a policy change prepared against a
// revision that is no longer current, with the current policy (nil when none
// was ever set).
type RoutingPolicyRejectionError struct {
	CurrentPolicy *RoutingPolicyWire `json:"current_policy"`
}

func (*RoutingPolicyRejectionError) Error() string { return "the routing policy changed" }

// operatorArmOnly reports whether the command carries no decision envelope and
// at most one payload arm; each operator command then validates its own arm.
func (in ClientCommand) operatorArmOnly() bool {
	set := 0
	for _, arm := range []any{
		in.Payload, in.StopTask, in.SubmitTask,
		in.RefreshUsage, in.ReassignCollector, in.ChangeTaskLine, in.SetRoutingPolicy, in.MoveParkedTask,
		in.SetWorkflowPolicy,
	} {
		if !reflect.ValueOf(arm).IsZero() {
			set++
		}
	}
	return in.ExpectedEntityVersion == 0 && set <= 1
}

// commitReceipt runs one operator command: the device gate, then replay of a
// recorded receipt, then the refusal of an id another kind holds, then apply, which performs the effect and returns the
// receipt to record in the same transaction. same reports whether a recorded
// receipt answers this request; any other receipt under the command_id is
// ErrImmutableConflict.
func (s *Service) commitReceipt(
	ctx context.Context, in ClientCommand,
	same func(domain.CommandReceipt) bool,
	apply func(*store.WriteTx, time.Time) (domain.CommandReceipt, error),
) (CommandResult, error) {
	var result CommandResult
	err := s.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := gateActiveDevice(ctx, tx, in.DeviceID); err != nil {
			return err
		}
		if old, snap, err := tx.GetCommandReceipt(ctx, in.CommandID); err == nil {
			if !same(old) {
				return fmt.Errorf("command %q: %w", in.CommandID, store.ErrImmutableConflict)
			}
			result = CommandResult{Receipt: &old, Revision: snap.AsOfRevision}
			return errReplay
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := tx.RequireCommandIDFree(ctx, in.CommandID); err != nil {
			return err
		}
		receipt, err := apply(tx, s.now().UTC())
		if err != nil {
			return err
		}
		snap, err := tx.RecordCommandReceipt(ctx, receipt)
		if err != nil {
			return err
		}
		result = CommandResult{Receipt: &receipt, Revision: snap.AsOfRevision}
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, err)
	}
	return result, nil
}

func (s *Service) refreshUsage(ctx context.Context, in ClientCommand) (CommandResult, error) {
	request := domain.UsageRefreshRequest{
		CommandIdentity: domain.CommandIdentity{CommandID: in.CommandID, DeviceID: in.DeviceID},
		Pool:            in.RefreshUsage.Pool,
	}
	if !in.operatorArmOnly() || request.Pool == "" {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, ErrInvalidOperatorCommandPayload)
	}
	var after func()
	result, err := s.commitReceipt(ctx, in,
		func(old domain.CommandReceipt) bool {
			return old.UsageRefresh != nil && old.UsageRefresh.UsageRefreshRequest == request
		},
		func(tx *store.WriteTx, now time.Time) (domain.CommandReceipt, error) {
			if _, err := tx.GetUsagePool(ctx, request.Pool); err != nil {
				return domain.CommandReceipt{}, err
			}
			if s.usageOperator == nil {
				return domain.CommandReceipt{}, ErrUsageOperationUnavailable
			}
			var err error
			if after, err = s.usageOperator.RefreshUsage(ctx, tx, request.Pool, now); err != nil {
				return domain.CommandReceipt{}, err
			}
			return domain.CommandReceipt{UsageRefresh: &domain.UsageRefreshReceipt{
				UsageRefreshRequest: request, RequestedAt: now,
			}}, nil
		})
	runAfterCommit(err, after)
	return result, err
}

// runAfterCommit runs a usage operator's after-commit effect once the command
// committed. A replay never ran the operator, so after is nil; any error
// means nothing committed. store.Write runs its function once, so after is
// the one the committed transaction returned.
func runAfterCommit(err error, after func()) {
	if err == nil && after != nil {
		after()
	}
}

func (s *Service) reassignCollector(ctx context.Context, in ClientCommand) (CommandResult, error) {
	request := domain.CollectorReassignmentRequest{
		CommandIdentity: domain.CommandIdentity{CommandID: in.CommandID, DeviceID: in.DeviceID},
		Pool:            in.ReassignCollector.Pool, Collector: in.ReassignCollector.EnrollmentID,
	}
	if !in.operatorArmOnly() || request.Pool == "" || request.Collector == "" {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, ErrInvalidOperatorCommandPayload)
	}
	var after func()
	result, err := s.commitReceipt(ctx, in,
		func(old domain.CommandReceipt) bool {
			return old.CollectorReassignment != nil && old.CollectorReassignment.CollectorReassignmentRequest == request
		},
		func(tx *store.WriteTx, now time.Time) (domain.CommandReceipt, error) {
			if _, err := tx.GetUsagePool(ctx, request.Pool); err != nil {
				return domain.CommandReceipt{}, err
			}
			// Checked before the operator, so a foreign enrollment is refused
			// with or without one.
			if err := tx.RequireEnrollmentOnPool(ctx, request.Pool, request.Collector); err != nil {
				return domain.CommandReceipt{}, err
			}
			if s.usageOperator == nil {
				return domain.CommandReceipt{}, ErrUsageOperationUnavailable
			}
			var err error
			if after, err = s.usageOperator.ReassignCollector(ctx, tx, request.Pool, request.Collector, now); err != nil {
				return domain.CommandReceipt{}, err
			}
			return domain.CommandReceipt{CollectorReassignment: &domain.CollectorReassignmentReceipt{
				CollectorReassignmentRequest: request, RecordedAt: now,
			}}, nil
		})
	runAfterCommit(err, after)
	return result, err
}

func (s *Service) changeTaskLine(ctx context.Context, in ClientCommand) (CommandResult, error) {
	p := in.ChangeTaskLine
	request := domain.TaskLineChangeRequest{
		CommandIdentity: domain.CommandIdentity{CommandID: in.CommandID, DeviceID: in.DeviceID},
		TaskID:          p.TaskID, Role: p.Role, Selection: p.Selection,
	}
	if !in.operatorArmOnly() || p.TaskID == "" {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, ErrInvalidOperatorCommandPayload)
	}
	change := TaskLineChange{
		TaskID: p.TaskID, Role: p.Role, Selection: p.Selection, Clear: p.Selection == nil,
		Source: domain.TaskLineSourceClientLineChange,
	}
	if p.Selection != nil {
		if err := domain.ValidateTaskLineChoices([]domain.TaskLineChoice{{Role: p.Role, Selection: p.Selection}}); err != nil {
			return CommandResult{}, fmt.Errorf("submit command %q: %w: %w", in.CommandID, ErrInvalidOperatorCommandPayload, err)
		}
	} else if !p.Role.TaskLineEligible() {
		return CommandResult{}, fmt.Errorf("submit command %q: %w: %w",
			in.CommandID, ErrInvalidOperatorCommandPayload, domain.ErrTaskLineRoleIneligible)
	}
	return s.commitReceipt(ctx, in,
		func(old domain.CommandReceipt) bool {
			return old.TaskLineChange != nil && old.TaskLineChange.Same(request)
		},
		func(tx *store.WriteTx, now time.Time) (domain.CommandReceipt, error) {
			if s.taskLineChanger == nil {
				return domain.CommandReceipt{}, ErrOperatorSettingUnavailable
			}
			// Checked only for a new command. A replay compares the request
			// with the stored one above.
			if p.Selection != nil {
				if err := validateLineSelection(ctx, &tx.ReadTx, *p.Selection, "selection"); err != nil {
					return domain.CommandReceipt{}, err
				}
			}
			outcome, err := s.taskLineChanger(ctx, tx, change, now)
			if errors.Is(err, domain.ErrTaskLineRoleRunning) {
				return domain.CommandReceipt{}, s.taskLineRoleRunning(ctx, tx, p.TaskID)
			}
			if err != nil {
				return domain.CommandReceipt{}, err
			}
			line := outcome.Line
			// A cleared head is no line: the wire TaskLine names a choice.
			if line != nil && line.Cleared {
				line = nil
			}
			return domain.CommandReceipt{TaskLineChange: &domain.TaskLineChangeReceipt{
				TaskLineChangeRequest: request, Recorded: outcome.Recorded, Line: line,
			}}, nil
		})
}

func (s *Service) taskLineRoleRunning(ctx context.Context, tx *store.WriteTx, id domain.TaskID) error {
	task, err := tx.GetTask(ctx, id)
	if err != nil {
		return err
	}
	state, err := tx.ServerState(ctx)
	if err != nil {
		return err
	}
	snapshot, err := currentTaskSnapshot(ctx, &tx.ReadTx, state, task)
	if err != nil {
		return err
	}
	return &TaskLineChangeRejectionError{Reason: "role_running", ReplacementTask: snapshot, SyncEpoch: state.SyncEpoch}
}

func (s *Service) setRoutingPolicy(ctx context.Context, in ClientCommand) (CommandResult, error) {
	p := in.SetRoutingPolicy
	request := domain.RoutingPolicyChangeRequest{
		CommandIdentity:  domain.CommandIdentity{CommandID: in.CommandID, DeviceID: in.DeviceID},
		ExpectedRevision: p.ExpectedRevision,
	}
	if !in.operatorArmOnly() || p.ExpectedRevision != nil && *p.ExpectedRevision < 1 ||
		p.DrainFirstAccount != nil && *p.DrainFirstAccount == "" {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, ErrInvalidOperatorCommandPayload)
	}
	requested := make([]domain.RoutingPolicyRole, 0, len(p.Roles))
	for _, role := range p.Roles {
		requested = append(requested, domain.RoutingPolicyRole{Role: role.Role, Enrollments: role.Enrollments})
	}
	roles, err := domain.CanonicalRoutingPolicyRoles(requested)
	if err != nil {
		return CommandResult{}, fmt.Errorf("submit command %q: %w: %w", in.CommandID, ErrInvalidOperatorCommandPayload, err)
	}
	return s.commitReceipt(ctx, in,
		func(old domain.CommandReceipt) bool {
			return old.RoutingPolicyChange != nil && old.RoutingPolicyChange.SameRequest(request, roles, p.DrainFirstAccount)
		},
		func(tx *store.WriteTx, now time.Time) (domain.CommandReceipt, error) {
			if s.routingPolicySetter == nil {
				return domain.CommandReceipt{}, ErrOperatorSettingUnavailable
			}
			var current *domain.RoutingPolicy
			switch policy, err := tx.CurrentRoutingPolicy(ctx); {
			case err == nil:
				current = &policy
			case !errors.Is(err, store.ErrNotFound):
				return domain.CommandReceipt{}, err
			}
			if !sameRevision(current, p.ExpectedRevision) {
				rejection := &RoutingPolicyRejectionError{}
				if current != nil {
					rendered := renderRoutingPolicy(*current)
					rejection.CurrentPolicy = &rendered
				}
				return domain.CommandReceipt{}, rejection
			}
			outcome, err := s.routingPolicySetter(ctx, tx, RoutingPolicyChange{
				Roles: roles, DrainFirstAccount: p.DrainFirstAccount,
				Source: domain.RoutingPolicySourceClientSet, SetBy: "device:" + string(in.DeviceID),
			}, now)
			if err != nil {
				return domain.CommandReceipt{}, err
			}
			return domain.CommandReceipt{RoutingPolicyChange: &domain.RoutingPolicyChangeReceipt{
				RoutingPolicyChangeRequest: request, Recorded: outcome.Recorded, Policy: outcome.Policy,
			}}, nil
		})
}

// sameRevision reports whether the client's expected revision names the
// current policy: nil for none.
func sameRevision(current *domain.RoutingPolicy, expected *int) bool {
	if current == nil || expected == nil {
		return current == nil && expected == nil
	}
	return current.Revision == *expected
}

func (s *Service) moveParkedTask(ctx context.Context, in ClientCommand) (CommandResult, error) {
	p := in.MoveParkedTask
	if !in.operatorArmOnly() || p.TaskID == "" || p.Choice == nil {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, ErrInvalidOperatorCommandPayload)
	}
	// Whether the park offers the enrollment and its catalog serves the
	// model and effort is the mover's to derive, in the command's
	// transaction.
	choice, err := p.Choice.choice()
	if err != nil {
		return CommandResult{}, fmt.Errorf("submit command %q: %w: %w", in.CommandID, ErrInvalidOperatorCommandPayload, err)
	}
	request := domain.ParkedTaskMoveRequest{
		CommandIdentity: domain.CommandIdentity{CommandID: in.CommandID, DeviceID: in.DeviceID},
		TaskID:          p.TaskID, Choice: &choice,
	}
	return s.commitReceipt(ctx, in,
		func(old domain.CommandReceipt) bool {
			return old.ParkedTaskMove != nil && old.ParkedTaskMove.Same(request)
		},
		func(tx *store.WriteTx, now time.Time) (domain.CommandReceipt, error) {
			if s.parkedTaskMover == nil {
				return domain.CommandReceipt{}, ErrOperatorSettingUnavailable
			}
			if _, err := tx.GetTask(ctx, request.TaskID); err != nil {
				return domain.CommandReceipt{}, err
			}
			receipt, err := s.parkedTaskMover(ctx, tx, request, now)
			if err != nil {
				return domain.CommandReceipt{}, err
			}
			return domain.CommandReceipt{ParkedTaskMove: &receipt}, nil
		})
}
