package engine

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The alternate-agent retry (issue #869) offers only an agent that escapes
// what failed, and admission re-derives that rule for the agent a card or a
// move chose before it runs.

// failImplementer admits and records the fixture run's implementer under the
// lineup agent on the main pool, records its failure with cause, and returns
// the failure an offer answers.
func (f *agentAdmissionFixture) failImplementer(t *testing.T, cause domain.FailureCause) alternateAgentFailure {
	t.Helper()
	ctx := context.Background()
	admission, admitted, err := f.admit(t)
	if err != nil || !admitted {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	f.record(t, admission)
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
			InvocationID: admission.InvocationID, AdmissionID: admission.ID,
			Status: domain.ExecutionOutcomeFailed, Summary: "provider refused",
			Cause: cause, RecordedAt: agentTestAt.Add(time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}
	return alternateAgentFailure{
		taskID: f.task(t, f.run.ID).ID, role: domain.RoleImplementer,
		cause: cause, invocationID: admission.InvocationID,
	}
}

func (f *agentAdmissionFixture) offers(t *testing.T, failure alternateAgentFailure) []domain.AlternateAgentOffer {
	t.Helper()
	ctx := context.Background()
	var offers []domain.AlternateAgentOffer
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		offers, err = f.selection.alternateAgentOffers(ctx, tx, failure, agentTestAt.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return offers
}

// TestAlternateAgentOffersEscapeTheCause: under a policy that orders a second
// agent on another pool for the implementer, a quota failure offers it under
// different_pool and a credential failure under valid_generation. A capacity
// failure offers nothing, since both agents share one route, and neither does
// a cause no rule covers. A parked task's quota refusal offers the same
// agent. Without a policy, the only candidate is the lineup agent, which is
// the one that failed; a held pool is not offered.
func TestAlternateAgentOffersEscapeTheCause(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	failure := f.failImplementer(t, domain.FailureCauseQuota)
	if offers := f.offers(t, failure); offers != nil {
		t.Fatalf("offers without a policy = %+v, want none", offers)
	}

	f.setRoutingPolicy(t, agentTestAt, order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
	model := f.routeModel(t, agentTestSecondAgent)
	offer := func(rule domain.AlternateAgentQualification) []domain.AlternateAgentOffer {
		return []domain.AlternateAgentOffer{{
			Agent: agentTestSecondAgent, Pool: routerSecondPool, Harness: domain.HarnessClientClaudeCode,
			Model: model, Qualification: rule,
		}}
	}
	for cause, want := range map[domain.FailureCause][]domain.AlternateAgentOffer{
		domain.FailureCauseQuota:             offer(domain.AlternateAgentDifferentPool),
		domain.FailureCauseCredentialExpired: offer(domain.AlternateAgentValidGeneration),
		domain.FailureCauseServiceCapacity:   nil,
		domain.FailureCauseUnknown:           nil,
	} {
		failure.cause = cause
		if got := f.offers(t, failure); !reflect.DeepEqual(got, want) {
			t.Errorf("%s offers = %+v, want %+v", cause, got, want)
		}
	}

	// A task parked on the same refusal offers the same agent on its hold.
	parked := parkedTaskFailure(failure.taskID, failure.invocationID)
	if got, want := f.offers(t, parked), offer(domain.AlternateAgentDifferentPool); !reflect.DeepEqual(got, want) {
		t.Errorf("parked offers = %+v, want %+v", got, want)
	}

	resetsAt := agentTestAt.Add(time.Hour)
	f.markPool(t, routerSecondPool, model, &resetsAt, agentTestAt)
	failure.cause = domain.FailureCauseQuota
	if offers := f.offers(t, failure); offers != nil {
		t.Fatalf("offers with the second pool held = %+v, want none", offers)
	}
}

// TestCardAdmissionRegatesTheChosenAgent: admission holds a card's agent to
// the record against the failed invocation's own admission and outcome. The
// failed agent, another pool than the one chosen, and a rule the cause does
// not name are each refused.
func TestCardAdmissionRegatesTheChosenAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	failure := f.failImplementer(t, domain.FailureCauseQuota)
	record := func(pool domain.UsagePoolID, rule domain.AlternateAgentQualification) domain.AlternateAgentRetry {
		record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
			TaskID: failure.taskID, Role: domain.RoleImplementer, FailedInvocationID: failure.invocationID,
			ItemID: "item-card", CommandID: "cmd-card", Agent: agentTestSecondAgent, Pool: pool,
			Qualification: rule, RecordedAt: agentTestAt.Add(2 * time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	regate := func(record domain.AlternateAgentRetry, agent string) error {
		return f.store.Read(ctx, func(tx *store.ReadTx) error {
			resolved, err := resolveAgent(ctx, tx, f.selection.Tree, agent)
			if err != nil {
				t.Fatal(err)
			}
			if err := resolved.readGeneration(ctx, tx); err != nil {
				t.Fatal(err)
			}
			return regateCardAgent(ctx, tx, f.selection.Tree, record, resolved)
		})
	}
	chosen := record(routerSecondPool, domain.AlternateAgentDifferentPool)
	if err := regate(chosen, agentTestSecondAgent); err != nil {
		t.Fatalf("the chosen agent was refused: %v", err)
	}
	for name, tc := range map[string]struct {
		record domain.AlternateAgentRetry
		agent  string
		want   string
	}{
		"the failed agent": {chosen, agentTestWriterAgent, "chose the agent that failed"},
		"another pool": {
			record(routerMainPool, domain.AlternateAgentDifferentPool), agentTestSecondAgent, "another pool",
		},
		"a rule the cause lacks": {
			record(routerSecondPool, domain.AlternateAgentValidGeneration), agentTestSecondAgent, "no longer escapes",
		},
	} {
		if err := regate(tc.record, tc.agent); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: regate = %v, want a refusal naming %q", name, err, tc.want)
		}
	}
}

// moveTask records a parked move of the fixture run's task to the second
// agent on its pool under policy, naming passedOver as the agent it left,
// with the lines it sets, in one transaction, and the command's receipt
// unless withoutReceipt. It returns the moved implementer line.
func (f *agentAdmissionFixture) moveTask(
	t *testing.T, policy domain.RoutingPolicy, passedOver *domain.TaskLinePassedOverMark, withoutReceipt bool,
) domain.TaskLine {
	t.Helper()
	ctx := context.Background()
	taskID := f.task(t, f.run.ID).ID
	var moved domain.TaskLine
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
			TaskID: taskID, Role: domain.RoleImplementer, FailedInvocationID: "inv-refused",
			CommandID: "cmd-move", Agent: agentTestSecondAgent, Pool: routerSecondPool,
			Qualification: domain.AlternateAgentDifferentPool, RecordedAt: agentTestAt,
		})
		if err != nil {
			return err
		}
		if err := tx.RecordAlternateAgentRetry(ctx, record); err != nil {
			return err
		}
		inputs, err := f.selection.moveLines(ctx, &tx.ReadTx, policy, record, passedOver)
		if err != nil {
			return err
		}
		movedTo, err := f.selection.Tree.SelectionOfAgent(record.Agent)
		if err != nil {
			return err
		}
		lines, err := tx.RecordAlternateAgentMoveLines(ctx, record, movedTo, inputs)
		if err != nil {
			return err
		}
		for _, line := range lines {
			if line.Role == domain.RoleImplementer {
				moved = line
			}
		}
		if withoutReceipt {
			return nil
		}
		_, err = tx.RecordCommandReceipt(ctx, domain.CommandReceipt{ParkedTaskMove: &domain.ParkedTaskMoveReceipt{
			ParkedTaskMoveRequest: domain.ParkedTaskMoveRequest{
				CommandIdentity: domain.CommandIdentity{CommandID: "cmd-move", DeviceID: "device-1"},
				TaskID:          taskID, Agent: agentTestSecondAgent,
			},
			Retry: record, Lines: lines,
		}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return moved
}

func (f *agentAdmissionFixture) movedOffPool(t *testing.T, refused domain.UsagePoolID) bool {
	t.Helper()
	ctx := context.Background()
	var moved bool
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		moved, err = implementerMovedOffPool(ctx, tx, &f.selection, f.run.ID, refused)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return moved
}

// TestMovedLineAnchorsToItsCommand: a move's implementer line, with the
// command receipt that records the move, admits the chosen agent under its
// own enrollment and releases the park on the refused pool; its provenance
// names the agent it left. The same line with no receipt is one no operator
// moved: admission refuses it, and the park holds.
func TestMovedLineAnchorsToItsCommand(t *testing.T) {
	t.Parallel()
	resetsAt := agentTestAt.Add(time.Hour)
	left := &domain.TaskLinePassedOverMark{
		Agent: agentTestWriterAgent, Pool: routerMainPool, Window: "five_hour",
		Source: domain.ExhaustedMarkProviderRefusal, MarkedAt: agentTestAt.Add(-2 * time.Minute),
		ResetsAt: &resetsAt,
	}
	for _, withoutReceipt := range []bool{false, true} {
		f := newAgentAdmissionFixture(t)
		_, enrollment := f.addSecondAgent(t)
		policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Minute),
			order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
		line := f.moveTask(t, policy, left, withoutReceipt)
		if f.lineAgent(line) != agentTestSecondAgent || line.Source != domain.TaskLineSourceAlternateAgentMove ||
			line.Provenance == nil || line.Provenance.Pool != routerSecondPool ||
			line.Provenance.PolicyRevision != policy.Revision || !reflect.DeepEqual(line.Provenance.PassedOver, left) {
			t.Fatalf("moved line = %+v (provenance %+v)", line, line.Provenance)
		}
		admission, err := f.admitRun(t, f.run, agentTestAt.Add(time.Minute))
		if withoutReceipt {
			if err == nil || !strings.Contains(err.Error(), "its record's command") {
				t.Fatalf("a move with no receipt = %+v, %v; want its anchor refused", admission.AgentBinding, err)
			}
			if f.movedOffPool(t, routerMainPool) {
				t.Fatal("a move with no receipt released the park")
			}
			continue
		}
		if err != nil {
			t.Fatalf("admit the moved task: %v", err)
		}
		wantCites(t, admission, line, enrollment.ID)
		if !f.movedOffPool(t, routerMainPool) || f.movedOffPool(t, routerSecondPool) {
			t.Fatal("the park did not read the move off the refused pool")
		}
	}
}

// TestMoveRefusesATaskThatIsNotParked: a move names a parked task, and a task
// with no parked attempt is refused before anything is written.
func TestMoveRefusesATaskThatIsNotParked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	taskID := f.task(t, f.run.ID).ID
	err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		_, err := f.selection.MoveParkedTaskTx(ctx, tx, domain.ParkedTaskMoveRequest{
			CommandIdentity: domain.CommandIdentity{CommandID: "cmd-move", DeviceID: "device-1"},
			TaskID:          taskID, Agent: agentTestSecondAgent,
		}, agentTestAt)
		return err
	})
	if !errors.Is(err, signet.ErrTaskNotParked) {
		t.Fatalf("move of an unparked task = %v, want ErrTaskNotParked", err)
	}
	if lines := f.taskLines(t, f.run.ID); len(lines) != 0 {
		t.Fatalf("a refused move wrote lines %+v", lines)
	}
}

// TestMoveNamesTheAgentItLeft: a move's provenance names the refused
// invocation's agent with the parked run's quota-window mark, but only while
// the run holds that window and the policy orders the agent, which a line's
// anchor requires of the agent it passed over.
func TestMoveNamesTheAgentItLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	failure := f.failImplementer(t, domain.FailureCauseQuota)
	ordered := f.setRoutingPolicy(t, agentTestAt, order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
	unordered := f.setRoutingPolicy(t, agentTestAt.Add(time.Second), order(domain.RoleImplementer, agentTestSecondAgent))
	left := func(policy domain.RoutingPolicy) *domain.TaskLinePassedOverMark {
		var mark *domain.TaskLinePassedOverMark
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			mark, err = f.selection.movedPastMark(ctx, tx, policy, f.run.ID, failure.invocationID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return mark
	}
	if mark := left(ordered); mark != nil {
		t.Fatalf("a run with no hold named %+v", mark)
	}

	resetsAt := agentTestAt.Add(time.Hour)
	markedAt := agentTestAt.Add(time.Minute)
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordRunHold(ctx, domain.RunHoldObservation{
			RunID: f.run.ID, Reason: domain.HoldQuotaWindowExhausted,
			QuotaWindow: &domain.QuotaWindowHold{Pool: routerMainPool, ExhaustedMark: domain.ExhaustedMark{
				Window: "five_hour", Source: domain.ExhaustedMarkProviderRefusal,
				InvocationID: &failure.invocationID, ResetsAt: &resetsAt, SetAt: markedAt,
			}},
			FirstObservedAt: markedAt, LastObservedAt: markedAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	want := &domain.TaskLinePassedOverMark{
		Agent: agentTestWriterAgent, Pool: routerMainPool, Window: "five_hour",
		Source: domain.ExhaustedMarkProviderRefusal, MarkedAt: markedAt, ResetsAt: &resetsAt,
	}
	if got := left(ordered); !reflect.DeepEqual(got, want) {
		t.Fatalf("passed over = %+v, want %+v", got, want)
	}
	if mark := left(unordered); mark != nil {
		t.Fatalf("a policy that orders no failed agent named %+v", mark)
	}
}

const agentTestSiblingAgent = "claude-sibling"

// addSiblingAgent adds an agent on the fixture's own enrollment, and so on
// the failed agent's credential, under the specifier's model offer, so it is
// a different agent from the implementer's.
func (f *agentAdmissionFixture) addSiblingAgent(t *testing.T) {
	t.Helper()
	tree := f.selection.Tree
	tree.Agents = append(slices.Clone(tree.Agents), domain.AgentSource{
		Name: agentTestSiblingAgent, Enrollment: string(f.enrollment.ID), Route: f.enrollment.Route,
		Adapter: agentbaseline.ClaudeAdapterName, Offer: agentbaseline.WriterAgentName(domain.RoleSpecifier),
		Effort: domain.EffortHigh,
	})
	tree.Sort()
	files, err := agenttree.Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if f.selection.LineupRevision, err = agenttree.Revision(files); err != nil {
		t.Fatal(err)
	}
	if f.selection.Tree, err = agenttree.Parse(files); err != nil {
		t.Fatal(err)
	}
	selection := f.selection
	f.engine.admission.environment.Agents = &selection
}

// TestCredentialAlternateNeedsAnotherCredential: a credential failure offers
// only an agent on another credential. A sibling on the failed agent's own
// enrollment and generation would run the failed credential again, so it is
// neither offered nor admitted from a card that names it.
func TestCredentialAlternateNeedsAnotherCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	f.addSiblingAgent(t)
	failure := f.failImplementer(t, domain.FailureCauseCredentialRevoked)
	f.setRoutingPolicy(t, agentTestAt,
		order(domain.RoleImplementer, agentTestWriterAgent, agentTestSiblingAgent, agentTestSecondAgent))
	offers := f.offers(t, failure)
	if names := func() []string {
		var names []string
		for _, offer := range offers {
			names = append(names, offer.Agent)
		}
		return names
	}(); !slices.Equal(names, []string{agentTestSecondAgent}) {
		t.Fatalf("credential offers = %v, want only %s", names, agentTestSecondAgent)
	}

	record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
		TaskID: failure.taskID, Role: domain.RoleImplementer, FailedInvocationID: failure.invocationID,
		ItemID: "item-card", CommandID: "cmd-card", Agent: agentTestSiblingAgent, Pool: routerMainPool,
		Qualification: domain.AlternateAgentValidGeneration, RecordedAt: agentTestAt.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.Read(ctx, func(tx *store.ReadTx) error {
		sibling, err := resolveAgent(ctx, tx, f.selection.Tree, agentTestSiblingAgent)
		if err != nil {
			t.Fatal(err)
		}
		if err := sibling.readGeneration(ctx, tx); err != nil {
			t.Fatal(err)
		}
		return regateCardAgent(ctx, tx, f.selection.Tree, record, sibling)
	})
	if err == nil || !strings.Contains(err.Error(), "no longer escapes") {
		t.Fatalf("regate of the sibling = %v, want it refused", err)
	}
}
