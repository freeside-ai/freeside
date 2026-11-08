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

// TestAlternateAgentOffersEscapeTheCause: a card's offers draw on every
// enrollment the store holds (#2735), so with no policy a quota failure
// offers the second enrollment under different_pool and a credential failure
// under valid_generation, at the role's lineup model. A capacity failure
// offers nothing, since both enrollments share one route, and neither does a
// cause no rule covers. A parked task's offers draw only on the enrollments
// the policy in effect orders for the role. A held pool is not offered.
func TestAlternateAgentOffersEscapeTheCause(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	_, second := f.addSecondAgent(t)
	failure := f.failImplementer(t, domain.FailureCauseQuota)
	offer := func(rule domain.AlternateAgentQualification) []domain.AlternateAgentOffer {
		return []domain.AlternateAgentOffer{{
			Enrollment: second.ID, Pool: routerSecondPool, Harness: domain.HarnessClientClaudeCode,
			Model: "claude-implementer-test", Qualification: rule,
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

	// A parked task offers only what the policy in effect orders.
	parked := parkedTaskFailure(failure.taskID, failure.invocationID)
	if got := f.offers(t, parked); got != nil {
		t.Errorf("parked offers with no policy = %+v, want none", got)
	}
	f.setRoutingPolicy(t, agentTestAt, order(domain.RoleImplementer, agentTestEnrollment))
	if got := f.offers(t, parked); got != nil {
		t.Errorf("parked offers off the policy = %+v, want none", got)
	}
	f.setRoutingPolicy(t, agentTestAt.Add(time.Second),
		order(domain.RoleImplementer, agentTestEnrollment, agentTestSecondEnrollment))
	if got, want := f.offers(t, parked), offer(domain.AlternateAgentDifferentPool); !reflect.DeepEqual(got, want) {
		t.Errorf("parked offers = %+v, want %+v", got, want)
	}

	resetsAt := agentTestAt.Add(time.Hour)
	f.markPool(t, routerSecondPool, "claude-implementer-test", &resetsAt, agentTestAt)
	failure.cause = domain.FailureCauseQuota
	if offers := f.offers(t, failure); offers != nil {
		t.Fatalf("offers with the second pool held = %+v, want none", offers)
	}
}

// TestAlternateOffersNeedTheLineupServed: an enrollment is offered only when
// it serves the role's lineup model and effort. A recorded catalog decides
// that; with none, the enrollment must bind the lineup enrollment's client,
// since the bridge would take any model as named.
func TestAlternateOffersNeedTheLineupServed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		catalog func(f *agentAdmissionFixture, enrollment domain.ClientEnrollmentID)
		offered bool
	}{
		"no catalog, the lineup's client": {offered: true},
		"a catalog serving the lineup": {
			catalog: func(f *agentAdmissionFixture, enrollment domain.ClientEnrollmentID) {
				f.recordCatalog(t, enrollment, "claude-implementer-test", "low", "high")
			},
			offered: true,
		},
		"a catalog without the model": {
			catalog: func(f *agentAdmissionFixture, enrollment domain.ClientEnrollmentID) {
				f.recordCatalog(t, enrollment, "claude-other-test", "high")
			},
		},
		"a catalog without the effort": {
			catalog: func(f *agentAdmissionFixture, enrollment domain.ClientEnrollmentID) {
				f.recordCatalog(t, enrollment, "claude-implementer-test", "low")
			},
		},
	} {
		f := newAgentAdmissionFixture(t)
		_, second := f.addSecondAgent(t)
		failure := f.failImplementer(t, domain.FailureCauseQuota)
		if tc.catalog != nil {
			tc.catalog(f, second.ID)
		}
		if got := f.offers(t, failure); (len(got) == 1) != tc.offered {
			t.Errorf("%s: offers = %+v, want offered %t", name, got, tc.offered)
		}
	}
}

// TestParkedOffersReadAPreUnitPolicy: a revision stored before #2736 orders
// agent names, and a parked task's offers are the enrollments those agents
// reduce to through the tree (roleOrder), at the role's lineup model, as
// under a revision that orders enrollments.
func TestParkedOffersReadAPreUnitPolicy(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	_, second := f.addSecondAgent(t)
	failure := f.failImplementer(t, domain.FailureCauseQuota)
	f.setPreUnitPolicy(t, domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent)
	want := []domain.AlternateAgentOffer{{
		Enrollment: second.ID, Pool: routerSecondPool, Harness: domain.HarnessClientClaudeCode,
		Model: "claude-implementer-test", Qualification: domain.AlternateAgentDifferentPool,
	}}
	if got := f.offers(t, parkedTaskFailure(failure.taskID, failure.invocationID)); !reflect.DeepEqual(got, want) {
		t.Fatalf("parked offers = %+v, want %+v", got, want)
	}
}

// TestCardAdmissionRegatesTheChosenAgent: admission holds a card's selection
// to the record against the failed invocation's own admission and outcome.
// The failed credential, another pool than the one chosen, and a rule the
// cause does not name are each refused. A v1 record, which names a tree
// agent, re-gates the same way.
func TestCardAdmissionRegatesTheChosenAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	failure := f.failImplementer(t, domain.FailureCauseQuota)
	record := func(agent string, pool domain.UsagePoolID, rule domain.AlternateAgentQualification) domain.AlternateAgentRetry {
		record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
			TaskID: failure.taskID, Role: domain.RoleImplementer, FailedInvocationID: failure.invocationID,
			ItemID: "item-card", CommandID: "cmd-card", Selection: f.lineSelection(t, agent), Pool: pool,
			Qualification: rule, RecordedAt: agentTestAt.Add(2 * time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	regate := func(record domain.AlternateAgentRetry, agent string) error {
		return f.store.Read(ctx, func(tx *store.ReadTx) error {
			return regateCardAgent(ctx, tx, record, cardRoleAgent(t, ctx, tx, f.selection.Tree, agent))
		})
	}
	chosen := record(agentTestSecondAgent, routerSecondPool, domain.AlternateAgentDifferentPool)
	if err := regate(chosen, agentTestSecondAgent); err != nil {
		t.Fatalf("the chosen selection was refused: %v", err)
	}
	v1 := chosen
	v1.Selection, v1.Agent = nil, agentTestSecondAgent
	var err error
	if v1.ID, err = v1.ComputeID(); err != nil {
		t.Fatal(err)
	}
	if err := v1.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := regate(v1, agentTestSecondAgent); err != nil {
		t.Fatalf("a v1 record's agent was refused: %v", err)
	}
	for name, tc := range map[string]struct {
		record domain.AlternateAgentRetry
		agent  string
		want   string
	}{
		"the failed credential": {
			record(agentTestWriterAgent, routerSecondPool, domain.AlternateAgentDifferentPool),
			agentTestWriterAgent, "chose the credential that failed",
		},
		"another pool": {
			record(agentTestSecondAgent, routerMainPool, domain.AlternateAgentDifferentPool),
			agentTestSecondAgent, "another pool",
		},
		"a rule the cause lacks": {
			record(agentTestSecondAgent, routerSecondPool, domain.AlternateAgentValidGeneration),
			agentTestSecondAgent, "no longer escapes",
		},
	} {
		if err := regate(tc.record, tc.agent); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: regate = %v, want a refusal naming %q", name, err, tc.want)
		}
	}
}

// moveTask records a parked move of the fixture run's task to the second
// agent's selection on its pool under policy, naming passedOver as the agent
// it left, with the lines moveLines sets plus extra, in one transaction, and
// the command's receipt unless withoutReceipt. It returns the lines by role.
func (f *agentAdmissionFixture) moveTask(
	t *testing.T, policy domain.RoutingPolicy, passedOver *domain.TaskLinePassedOverMark, withoutReceipt bool,
	extra ...domain.TaskLineInput,
) map[domain.RoleName]domain.TaskLine {
	t.Helper()
	ctx := context.Background()
	taskID := f.task(t, f.run.ID).ID
	moved := map[domain.RoleName]domain.TaskLine{}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
			TaskID: taskID, Role: domain.RoleImplementer, FailedInvocationID: "inv-refused",
			CommandID: "cmd-move", Selection: f.lineSelection(t, agentTestSecondAgent), Pool: routerSecondPool,
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
		for _, in := range extra {
			in.TaskID, in.SetBy, in.Source = taskID, string(record.ID), domain.TaskLineSourceAlternateAgentMove
			inputs = append(inputs, in)
		}
		lines, err := tx.RecordAlternateAgentMoveLines(ctx, record, inputs)
		if err != nil {
			return err
		}
		for _, line := range lines {
			moved[line.Role] = line
		}
		if withoutReceipt {
			return nil
		}
		_, err = tx.RecordCommandReceipt(ctx, domain.CommandReceipt{ParkedTaskMove: &domain.ParkedTaskMoveReceipt{
			ParkedTaskMoveRequest: domain.ParkedTaskMoveRequest{
				CommandIdentity: domain.CommandIdentity{CommandID: "cmd-move", DeviceID: "device-1"},
				TaskID:          taskID, Choice: &domain.AlternateAgentChoice{EnrollmentID: record.Selection.EnrollmentID},
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
// command receipt that records the move, admits the chosen selection under
// its own enrollment and releases the park on the refused pool; its provenance
// names the enrollment it left. The same line with no receipt is one no operator
// moved: admission refuses it, and the park holds.
func TestMovedLineAnchorsToItsCommand(t *testing.T) {
	t.Parallel()
	resetsAt := agentTestAt.Add(time.Hour)
	left := &domain.TaskLinePassedOverMark{
		Enrollment: agentTestEnrollment, Pool: routerMainPool, Window: "five_hour",
		Source: domain.ExhaustedMarkProviderRefusal, MarkedAt: agentTestAt.Add(-2 * time.Minute),
		ResetsAt: &resetsAt,
	}
	for _, withoutReceipt := range []bool{false, true} {
		f := newAgentAdmissionFixture(t)
		_, enrollment := f.addSecondAgent(t)
		policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Minute),
			order(domain.RoleImplementer, agentTestEnrollment, agentTestSecondEnrollment))
		line := f.moveTask(t, policy, left, withoutReceipt)[domain.RoleImplementer]
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

// TestMovedLineKeepsAPreUnitAgentMark: a move made before #2736 cites a
// revision in the agents form, and its passed-over mark names an agent.
// The anchor admits the mark while that revision orders the agent, and
// refuses one naming an agent it does not order, on a task that never
// escalated.
func TestMovedLineKeepsAPreUnitAgentMark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	resetsAt := agentTestAt.Add(time.Hour)
	for agent, admits := range map[string]bool{agentTestWriterAgent: true, "claude-unordered": false} {
		f := newAgentAdmissionFixture(t)
		f.addSecondAgent(t)
		f.setPreUnitPolicy(t, domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent)
		var policy domain.RoutingPolicy
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			policy, err = routingPolicyRevision(ctx, tx, 1)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		left := &domain.TaskLinePassedOverMark{
			Agent: agent, Pool: routerMainPool, Window: "five_hour",
			Source: domain.ExhaustedMarkProviderRefusal, MarkedAt: agentTestAt.Add(-2 * time.Minute),
			ResetsAt: &resetsAt,
		}
		f.moveTask(t, policy, left, false)
		admission, err := f.admitRun(t, f.run, agentTestAt.Add(time.Minute))
		switch {
		case admits && err != nil:
			t.Errorf("%s: a mark the revision orders was refused: %v", agent, err)
		case !admits && (err == nil || !strings.Contains(err.Error(), "orders nothing the line passed over")):
			t.Errorf("%s: a mark the revision does not order = %+v, %v; want refused", agent, admission.AgentBinding, err)
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
			TaskID:          taskID,
			Choice:          &domain.AlternateAgentChoice{EnrollmentID: f.lineSelection(t, agentTestSecondAgent).EnrollmentID},
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

// TestMoveNamesTheEnrollmentItLeft: a move's provenance names the refused
// invocation's enrollment with the parked run's quota-window mark, but only
// while the run holds that window and the policy orders the enrollment, which
// a line's anchor requires of what it passed over.
func TestMoveNamesTheEnrollmentItLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	failure := f.failImplementer(t, domain.FailureCauseQuota)
	ordered := f.setRoutingPolicy(t, agentTestAt, order(domain.RoleImplementer, agentTestEnrollment, agentTestSecondEnrollment))
	unordered := f.setRoutingPolicy(t, agentTestAt.Add(time.Second), order(domain.RoleImplementer, agentTestSecondEnrollment))
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
		Enrollment: agentTestEnrollment, Pool: routerMainPool, Window: "five_hour",
		Source: domain.ExhaustedMarkProviderRefusal, MarkedAt: markedAt, ResetsAt: &resetsAt,
	}
	if got := left(ordered); !reflect.DeepEqual(got, want) {
		t.Fatalf("passed over = %+v, want %+v", got, want)
	}
	if mark := left(unordered); mark != nil {
		t.Fatalf("a policy that orders no failed enrollment named %+v", mark)
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
// only another credential. The failed enrollment at its failed generation
// would run that credential again, so it is not offered, and a card whose
// selection a sibling agent on it reduces to is not admitted.
func TestCredentialAlternateNeedsAnotherCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	_, second := f.addSecondAgent(t)
	f.addSiblingAgent(t)
	failure := f.failImplementer(t, domain.FailureCauseCredentialRevoked)
	f.setRoutingPolicy(t, agentTestAt,
		order(domain.RoleImplementer, agentTestEnrollment, agentTestSecondEnrollment))
	offers := f.offers(t, failure)
	if len(offers) != 1 || offers[0].Enrollment != second.ID {
		t.Fatalf("credential offers = %+v, want only %s", offers, second.ID)
	}

	record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
		TaskID: failure.taskID, Role: domain.RoleImplementer, FailedInvocationID: failure.invocationID,
		ItemID: "item-card", CommandID: "cmd-card", Selection: f.lineSelection(t, agentTestSiblingAgent), Pool: routerMainPool,
		Qualification: domain.AlternateAgentValidGeneration, RecordedAt: agentTestAt.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.Read(ctx, func(tx *store.ReadTx) error {
		sibling := cardRoleAgent(t, ctx, tx, f.selection.Tree, agentTestSiblingAgent)
		return regateCardAgent(ctx, tx, record, sibling)
	})
	if err == nil || !strings.Contains(err.Error(), "chose the credential that failed") {
		t.Fatalf("regate of the sibling = %v, want it refused", err)
	}
}

// cardRoleAgent resolves the selection a card record's tree agent reduces
// to, with its generation, as resolveCardAgent does before it re-gates the
// record.
func cardRoleAgent(t *testing.T, ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, name string) RoleAgent {
	t.Helper()
	selection, err := tree.SelectionOfAgent(name)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := resolveNames(ctx, tx, tree, selection.Names(), &name)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.readGeneration(ctx, tx); err != nil {
		t.Fatal(err)
	}
	return agent
}

// TestMoveRepinsTheRolesThePolicyOrders: a move pins every other role the
// policy orders the chosen enrollment for to that role's lineup model and
// effort on it, skips a role the policy orders elsewhere, and skips a role
// whose lineup tuple the enrollment's catalog does not serve.
func TestMoveRepinsTheRolesThePolicyOrders(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	_, second := f.addSecondAgent(t)
	// The catalog serves the implementer's and remediator's model, not the
	// specifier's.
	f.recordCatalog(t, second.ID, "claude-implementer-test", "high")
	policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Minute),
		order(domain.RoleSpecifier, agentTestEnrollment, agentTestSecondEnrollment),
		order(domain.RoleImplementer, agentTestEnrollment, agentTestSecondEnrollment),
		order(domain.RoleRemediator, agentTestSecondEnrollment),
		order(domain.RoleReviewer, agentTestEnrollment))
	lines := f.moveTask(t, policy, nil, false)
	want := domain.TaskLineSelection{EnrollmentID: second.ID, ModelID: "claude-implementer-test", Effort: "high"}
	for _, role := range []domain.RoleName{domain.RoleImplementer, domain.RoleRemediator} {
		if line, ok := lines[role]; !ok || line.Selection == nil || *line.Selection != want {
			t.Fatalf("%s line = %+v, want %+v", role, line, want)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("moved lines = %+v, want only the implementer and remediator", lines)
	}
	if _, err := f.admitRole(t, domain.RoleRemediator); err != nil {
		t.Fatalf("admit the re-pinned remediator: %v", err)
	}
}

// TestMoveLineAnchorRefusesAnotherTuple: a moved line for another role than
// the record's must hold that role's lineup model and effort on the record's
// enrollment, so admission refuses one the store accepted with another model.
func TestMoveLineAnchorRefusesAnotherTuple(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	_, second := f.addSecondAgent(t)
	policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Minute),
		order(domain.RoleImplementer, agentTestEnrollment, agentTestSecondEnrollment))
	f.moveTask(t, policy, nil, false, domain.TaskLineInput{
		Role:      domain.RoleRemediator,
		Selection: &domain.TaskLineSelection{EnrollmentID: second.ID, ModelID: "claude-specifier-test", Effort: "high"},
		Provenance: &domain.TaskLineProvenance{
			PolicyRevision: policy.Revision, Tier: domain.TaskLineSelectionPolicyOrder, Pool: routerSecondPool,
			RankedBy: []domain.TaskLineRankingReading{},
		},
	})
	if _, err := f.admitRole(t, domain.RoleRemediator); err == nil ||
		!strings.Contains(err.Error(), "lineup model and effort") {
		t.Fatalf("admit a remediator moved to another model = %v, want its anchor refused", err)
	}
}

// TestChosenSelectionFillsAndChecksTheChoice: a choice that omits its model
// or effort takes the lineup's. With no catalog only the lineup's tuple is
// accepted; with one, the catalog decides. A refusal names the field, never
// the operator's text.
func TestChosenSelectionFillsAndChecksTheChoice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	_, second := f.addSecondAgent(t)
	chosen := func(choice domain.AlternateAgentChoice) (domain.TaskLineSelection, error) {
		var selection domain.TaskLineSelection
		err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			selection, err = f.selection.chosenSelection(ctx, tx, domain.RoleImplementer, choice)
			return err
		})
		return selection, err
	}
	lineup := domain.TaskLineSelection{EnrollmentID: second.ID, ModelID: "claude-implementer-test", Effort: "high"}
	if got, err := chosen(domain.AlternateAgentChoice{EnrollmentID: second.ID}); err != nil || got != lineup {
		t.Fatalf("an omitted model and effort = %+v, %v; want %+v", got, err, lineup)
	}
	refused := func(name string, choice domain.AlternateAgentChoice, field string) {
		t.Helper()
		_, err := chosen(choice)
		if !errors.Is(err, signet.ErrAlternateAgentSelectionUnserved) || !strings.HasPrefix(err.Error(), field+":") ||
			strings.Contains(err.Error(), "claude-other") || strings.Contains(err.Error(), "max") {
			t.Fatalf("%s = %v, want a refusal naming %s alone", name, err, field)
		}
	}
	refused("another model with no catalog",
		domain.AlternateAgentChoice{EnrollmentID: second.ID, ModelID: "claude-other-test"}, "model_id")
	refused("another effort with no catalog", domain.AlternateAgentChoice{EnrollmentID: second.ID, Effort: "max"}, "effort")

	f.recordCatalog(t, second.ID, "claude-other-test", "low", "high")
	other := domain.TaskLineSelection{EnrollmentID: second.ID, ModelID: "claude-other-test", Effort: "low"}
	if got, err := chosen(domain.AlternateAgentChoice(other)); err != nil || got != other {
		t.Fatalf("a served choice = %+v, %v; want %+v", got, err, other)
	}
	refused("the lineup model the catalog lacks", domain.AlternateAgentChoice{EnrollmentID: second.ID}, "model_id")
	refused("an effort the catalog lacks",
		domain.AlternateAgentChoice{EnrollmentID: second.ID, ModelID: "claude-other-test", Effort: "max"}, "effort")
}
