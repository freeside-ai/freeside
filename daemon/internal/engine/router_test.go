package engine

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The router (plan §5.4, routing policy; issue #2393) pins a task to one
// usage pool at its first routable admission, and later roles hold on that
// pool's marked windows instead of moving.

const (
	routerMainPool   domain.UsagePoolID = "identity:claude-main"
	routerSecondPool domain.UsagePoolID = "identity:claude-second"
)

// setRoutingPolicy records the next routing policy revision at at, with no
// drain-first account.
func (f *agentAdmissionFixture) setRoutingPolicy(
	t *testing.T, at time.Time, roles ...domain.RoutingPolicyRole,
) domain.RoutingPolicy {
	t.Helper()
	return f.setDrainingPolicy(t, at, nil, roles...)
}

// setDrainingPolicy is setRoutingPolicy with a drain-first account.
func (f *agentAdmissionFixture) setDrainingPolicy(
	t *testing.T, at time.Time, drainFirst *domain.UsagePoolID, roles ...domain.RoutingPolicyRole,
) domain.RoutingPolicy {
	t.Helper()
	ctx := context.Background()
	var policy domain.RoutingPolicy
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		policy, err = tx.AppendRoutingPolicy(ctx, domain.RoutingPolicyInput{
			Roles: roles, Source: domain.RoutingPolicySourceCLISet, SetBy: "operator",
			LineupRevision: f.selection.LineupRevision, DrainFirstAccount: drainFirst,
		}, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return policy
}

func order(role domain.RoleName, agents ...string) domain.RoutingPolicyRole {
	return domain.RoutingPolicyRole{Role: role, Agents: agents}
}

// routerLine appends an implementer router line naming the second agent.
func routerLine(t *testing.T, f *agentAdmissionFixture, setBy string, provenance *domain.TaskLineProvenance, setAt time.Time) {
	t.Helper()
	f.appendLine(t, domain.TaskLineInput{
		TaskID: f.task(t, f.run.ID).ID, Role: domain.RoleImplementer, Agent: agentTestSecondAgent,
		Source: domain.TaskLineSourceRouter, SetBy: setBy, Provenance: provenance,
	}, setAt)
}

// policyOrder is the provenance of a routing to pool that the policy's order
// decided, with no reset known and nothing passed over.
func policyOrder(revision int, pool domain.UsagePoolID) *domain.TaskLineProvenance {
	return &domain.TaskLineProvenance{
		PolicyRevision: revision, Tier: domain.TaskLineSelectionPolicyOrder, Pool: pool,
		RankedBy: []domain.TaskLineRankingReading{},
	}
}

// passedOver is the provenance's record of agent on pool passed over for mark.
func passedOver(agent string, pool domain.UsagePoolID, mark domain.ExhaustedMark) *domain.TaskLinePassedOverMark {
	return &domain.TaskLinePassedOverMark{
		Agent: agent, Pool: pool, Window: mark.Window, Source: mark.Source, MarkedAt: mark.SetAt, ResetsAt: mark.ResetsAt,
	}
}

// routeModel returns the route model id the agent's offer passes.
func (f *agentAdmissionFixture) routeModel(t *testing.T, agent string) string {
	t.Helper()
	ctx := context.Background()
	var candidate routeCandidate
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var ok bool
		var err error
		if candidate, ok, err = f.selection.routeCandidate(ctx, tx, agent); err == nil && !ok {
			t.Fatalf("agent %s does not resolve", agent)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return candidate.model
}

// updatePoolUsage rewrites the pool's usage record at at, starting one under
// the pool's default collector when it has none.
func (f *agentAdmissionFixture) updatePoolUsage(
	t *testing.T, pool domain.UsagePoolID, at time.Time,
	update func(tx *store.ReadTx, record domain.PoolUsageObservation) domain.PoolUsageObservation,
) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		record, err := tx.GetPoolUsageObservation(ctx, pool)
		if errors.Is(err, store.ErrNotFound) {
			var collector domain.ClientEnrollmentID
			if collector, err = tx.DefaultPoolCollector(ctx, pool); err == nil {
				record = domain.PoolUsageObservation{Pool: pool, Collector: collector}
			}
		}
		if err != nil {
			return err
		}
		return tx.RecordPoolUsageObservation(ctx, update(&tx.ReadTx, record), at)
	}); err != nil {
		t.Fatal(err)
	}
}

// routerWindow names the fixture's window for model. A provider window name
// is lowercase letters, digits, and underscores.
func routerWindow(model string) string {
	return "window_" + strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, strings.ToLower(model))
}

// markPool marks the pool's window for model exhausted by a provider
// refusal at at, resetting at resetsAt (nil for no reset). The window is
// named for the model and mapped to it alone.
func (f *agentAdmissionFixture) markPool(
	t *testing.T, pool domain.UsagePoolID, model string, resetsAt *time.Time, at time.Time,
) domain.ExhaustedMark {
	t.Helper()
	return f.markWindow(t, pool, model, routerWindow(model), resetsAt, at)
}

// markWindow is markPool for a named window, which it maps to model beside
// any window the model already has.
func (f *agentAdmissionFixture) markWindow(
	t *testing.T, pool domain.UsagePoolID, model, window string, resetsAt *time.Time, at time.Time,
) domain.ExhaustedMark {
	t.Helper()
	f.updatePoolUsage(t, pool, at, func(_ *store.ReadTx, record domain.PoolUsageObservation) domain.PoolUsageObservation {
		entry := domain.ModelWindow{RouteModelID: model, Window: window}
		if !slices.Contains(record.ModelWindows, entry) {
			record.ModelWindows = append(slices.Clone(record.ModelWindows), entry)
			slices.SortFunc(record.ModelWindows, func(a, b domain.ModelWindow) int {
				return cmp.Or(cmp.Compare(a.RouteModelID, b.RouteModelID), cmp.Compare(a.Window, b.Window))
			})
		}
		return record.WithRefusalMark(domain.RefusalMark{
			Window: window, InvocationID: "inv-refused", ResetsAt: resetsAt, SetAt: at,
		})
	})
	return domain.ExhaustedMark{
		Window: window, Source: domain.ExhaustedMarkProviderRefusal, InvocationID: new(domain.InvocationID("inv-refused")),
		ResetsAt: resetsAt, SetAt: at,
	}
}

// allowWindow records an allowed reading of the model's window at at, which
// clears a refusal mark set before it.
func (f *agentAdmissionFixture) allowWindow(t *testing.T, pool domain.UsagePoolID, model string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	f.updatePoolUsage(t, pool, at, func(tx *store.ReadTx, record domain.PoolUsageObservation) domain.PoolUsageObservation {
		generation, err := tx.CurrentEnrollmentGeneration(ctx, record.Collector)
		if err != nil {
			t.Fatal(err)
		}
		record.LastAttempt = &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: at}
		record.Windows = []domain.WindowReading{{
			Window: routerWindow(model), EnrollmentID: record.Collector, Generation: generation.Ordinal,
			Status: domain.WindowStatusAllowed, ObservedAt: at,
		}}
		return record
	})
}

// readWindow records an allowed reading at at of the pool's general window,
// one no model is mapped to, so it applies to every model: length long,
// resetting at resetsAt. It returns the reading as a routing ranks by it.
func (f *agentAdmissionFixture) readWindow(
	t *testing.T, pool domain.UsagePoolID, window string, length time.Duration, resetsAt, at time.Time,
) domain.TaskLineRankingReading {
	t.Helper()
	ctx := context.Background()
	f.updatePoolUsage(t, pool, at, func(tx *store.ReadTx, record domain.PoolUsageObservation) domain.PoolUsageObservation {
		generation, err := tx.CurrentEnrollmentGeneration(ctx, record.Collector)
		if err != nil {
			t.Fatal(err)
		}
		record.LastAttempt = &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: at}
		record.Windows = append(slices.DeleteFunc(slices.Clone(record.Windows), func(r domain.WindowReading) bool {
			return r.Window == window
		}), domain.WindowReading{
			Window: window, EnrollmentID: record.Collector, Generation: generation.Ordinal,
			Status: domain.WindowStatusAllowed, ResetsAt: &resetsAt, Length: &length, ObservedAt: at,
		})
		slices.SortFunc(record.Windows, func(a, b domain.WindowReading) int { return cmp.Compare(a.Window, b.Window) })
		return record
	})
	return domain.TaskLineRankingReading{Pool: pool, Window: window, ResetsAt: resetsAt, ObservedAt: at}
}

// newTaskRun seeds a run bound to a task of its own.
func (f *agentAdmissionFixture) newTaskRun(t *testing.T, id domain.RunID) domain.Run {
	t.Helper()
	run := seedRunPolicy(t, f.store, domain.Run{ID: id, ProjectID: f.run.ProjectID, SpecDigest: agentTestDigest("9")}, nil)
	if f.task(t, id).ID == f.task(t, f.run.ID).ID {
		t.Fatal("the new run shares the fixture run's task")
	}
	return run
}

// admitRun admits the implementation attempt of run at at.
func (f *agentAdmissionFixture) admitRun(t *testing.T, run domain.Run, at time.Time) (domain.ExecutionAdmission, error) {
	t.Helper()
	invocation, err := domain.NewAgentInvocation(
		productionInvocationID(run.ID), []domain.ArtifactID{agentTestInputArtifact}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.engine.admission.now = func() time.Time { return at }
	admission, admitted, err := f.engine.admitAttempt(context.Background(),
		invocationBinding{run: run, invocation: invocation},
		domain.Stage{ID: productionStageID(run.ID), RunID: run.ID, Name: productionStageName}, invocation.ID)
	if err == nil && !admitted {
		t.Fatal("admitAttempt admitted nothing")
	}
	return admission, err
}

// taskLines returns every line version of run's task.
func (f *agentAdmissionFixture) taskLines(t *testing.T, runID domain.RunID) []domain.TaskLine {
	t.Helper()
	ctx := context.Background()
	taskID := f.task(t, runID).ID
	var lines []domain.TaskLine
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		lines, err = tx.TaskLines(ctx, taskID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return lines
}

// wantRouterLines checks that the task's lines are exactly the router lines
// named, by role and agent, all citing provenance and set at setAt.
func wantRouterLines(
	t *testing.T, f *agentAdmissionFixture, runID domain.RunID, provenance *domain.TaskLineProvenance,
	setAt time.Time, want map[domain.RoleName]string,
) map[domain.RoleName]domain.TaskLine {
	t.Helper()
	taskID := f.task(t, runID).ID
	got := map[domain.RoleName]domain.TaskLine{}
	for _, line := range f.taskLines(t, runID) {
		if line.Source != domain.TaskLineSourceRouter {
			continue
		}
		got[line.Role] = line
		if line.Agent != want[line.Role] || line.Version != 1 || line.SetBy != "router:"+string(taskID) ||
			!line.SetAt.Equal(setAt) || !reflect.DeepEqual(line.Provenance, provenance) {
			t.Errorf("%s router line = %+v (provenance %+v), want %s citing %+v at %s",
				line.Role, line, line.Provenance, want[line.Role], provenance, setAt)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("router lines = %+v, want one per role in %v", got, want)
	}
	return got
}

func wantCites(t *testing.T, admission domain.ExecutionAdmission, line domain.TaskLine, enrollment domain.ClientEnrollmentID) {
	t.Helper()
	binding := admission.AgentBinding
	if binding == nil || binding.SelectionSource != domain.AgentSelectionSourceTaskLine ||
		binding.SelectionRecordID != line.ID || binding.EnrollmentID != enrollment {
		t.Fatalf("agent binding = %+v, want line %s on %s", binding, line.ID, enrollment)
	}
}

// TestRouterPinsEveryRoleToOnePool is the acceptance fixture for the first
// admission: under a policy and no marks, the role's first agent takes the
// task, every ordered role without a line gets a router line on that agent's
// pool, a role whose order holds no agent there keeps the lineup, and the
// task's version moves. A later admission reads its line and routes nothing,
// even under a newer policy.
func TestRouterPinsEveryRoleToOnePool(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
		order(domain.RoleSpecifier, agentTestSecondAgent),
		order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent),
		order(domain.RoleRemediator, agentTestSecondAgent, agentTestWriterAgent))
	before := f.taskRevision(t)

	admission, err := f.admitRun(t, f.run, agentTestAt)
	if err != nil {
		t.Fatal(err)
	}
	lines := wantRouterLines(t, f, f.run.ID, policyOrder(1, routerMainPool), agentTestAt, map[domain.RoleName]string{
		domain.RoleImplementer: agentTestWriterAgent, domain.RoleRemediator: agentTestWriterAgent,
	})
	wantCites(t, admission, lines[domain.RoleImplementer], f.enrollment.ID)
	if after := f.taskRevision(t); after.EntityVersion <= before.EntityVersion || after.AsOfRevision <= before.AsOfRevision {
		t.Fatalf("task snapshot = %+v after %+v, want the routing to advance it", after, before)
	}

	routed := f.taskLines(t, f.run.ID)
	f.setRoutingPolicy(t, agentTestAt.Add(time.Minute),
		order(domain.RoleImplementer, agentTestSecondAgent, agentTestWriterAgent))
	again, err := f.admitRun(t, f.run, agentTestAt.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	wantCites(t, again, lines[domain.RoleImplementer], f.enrollment.ID)
	if got := f.taskLines(t, f.run.ID); !reflect.DeepEqual(got, routed) {
		t.Fatalf("lines after a later admission = %+v, want the routing's %+v", got, routed)
	}
}

// TestRouterPassesOverAMarkedPool is the exit proof's shape: a mark on the
// first agent's pool for its model routes the next task to the next agent,
// citing the mark and the agent passed over, and the timeline shows it. The
// routed task stays on its pool after the mark clears, and new tasks return
// to the first agent once the reset passes or a contrary reading clears it.
func TestRouterPassesOverAMarkedPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	_, second := f.addSecondAgent(t)
	f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
		order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent),
		order(domain.RoleRemediator, agentTestWriterAgent, agentTestSecondAgent))
	model := f.routeModel(t, agentTestWriterAgent)
	reset := agentTestAt.Add(time.Hour)
	mark := f.markPool(t, routerMainPool, model, &reset, agentTestAt.Add(-time.Minute))

	admission, err := f.admitRun(t, f.run, agentTestAt)
	if err != nil {
		t.Fatal(err)
	}
	provenance := policyOrder(1, routerSecondPool)
	provenance.PassedOver = passedOver(agentTestWriterAgent, routerMainPool, mark)
	lines := wantRouterLines(t, f, f.run.ID, provenance, agentTestAt, map[domain.RoleName]string{
		domain.RoleImplementer: agentTestSecondAgent, domain.RoleRemediator: agentTestSecondAgent,
	})
	wantCites(t, admission, lines[domain.RoleImplementer], second.ID)
	timeline, err := f.engine.signet.GetTaskTimeline(ctx, f.task(t, f.run.ID).ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(timeline.Events, func(event domain.TaskEvent) bool {
		return event.Kind == domain.TaskEventTaskLineSet && event.Line != nil &&
			event.Line.LineID == lines[domain.RoleImplementer].ID && reflect.DeepEqual(event.Line.Provenance, provenance)
	}) {
		t.Fatalf("timeline events = %+v, want the switch", timeline.Events)
	}

	// The reset passes: the routed task stays where it was, and a new task
	// goes to the first agent.
	afterReset := reset.Add(time.Second)
	if again, err := f.admitRun(t, f.run, afterReset); err != nil {
		t.Fatal(err)
	} else {
		wantCites(t, again, lines[domain.RoleImplementer], second.ID)
	}
	next := f.newTaskRun(t, "run-next")
	if _, err := f.admitRun(t, next, afterReset); err != nil {
		t.Fatal(err)
	}
	wantRouterLines(t, f, next.ID, policyOrder(1, routerMainPool), afterReset, map[domain.RoleName]string{
		domain.RoleImplementer: agentTestWriterAgent, domain.RoleRemediator: agentTestWriterAgent,
	})

	// A contrary reading clears a mark before its reset.
	marked := afterReset.Add(time.Minute)
	later := marked.Add(time.Hour)
	f.markPool(t, routerMainPool, model, &later, marked)
	f.allowWindow(t, routerMainPool, model, marked.Add(time.Minute))
	cleared := f.newTaskRun(t, "run-cleared")
	if _, err := f.admitRun(t, cleared, marked.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	wantRouterLines(t, f, cleared.ID, policyOrder(1, routerMainPool), marked.Add(2*time.Minute), map[domain.RoleName]string{
		domain.RoleImplementer: agentTestWriterAgent, domain.RoleRemediator: agentTestWriterAgent,
	})
}

// TestRouterLeavesOperatorLinesAlone: an operator line and a cleared line
// keep their role off the router, a role whose order holds no agent on the
// picked pool keeps the lineup, and a line change supersedes a router line.
func TestRouterLeavesOperatorLinesAlone(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	_, second := f.addSecondAgent(t)
	operator := f.submitLine(t, domain.RoleReviewer, agentTestSecondAgent)
	f.submitLine(t, domain.RoleRemediator, agentTestSecondAgent)
	f.changeLine(t, domain.RoleRemediator, "", true)
	f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
		order(domain.RoleSpecifier, agentbaseline.WriterAgentName(domain.RoleSpecifier)),
		order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent),
		order(domain.RoleRemediator, agentTestSecondAgent),
		order(domain.RoleReviewer, agentTestWriterAgent))
	reset := agentTestAt.Add(time.Hour)
	mark := f.markPool(t, routerMainPool, f.routeModel(t, agentTestWriterAgent), &reset, agentTestAt.Add(-time.Minute))

	admission, err := f.admitRun(t, f.run, agentTestAt)
	if err != nil {
		t.Fatal(err)
	}
	provenance := policyOrder(1, routerSecondPool)
	provenance.PassedOver = passedOver(agentTestWriterAgent, routerMainPool, mark)
	lines := wantRouterLines(t, f, f.run.ID, provenance, agentTestAt,
		map[domain.RoleName]string{domain.RoleImplementer: agentTestSecondAgent})
	wantCites(t, admission, lines[domain.RoleImplementer], second.ID)
	if got := f.lineVersions(t, domain.RoleReviewer); len(got) != 1 || got[0].ID != operator.ID {
		t.Fatalf("reviewer lines = %+v, want the operator's alone", got)
	}
	if got := f.lineVersions(t, domain.RoleRemediator); len(got) != 2 || !got[1].Cleared {
		t.Fatalf("remediator lines = %+v, want the operator's line and its clear", got)
	}

	change := f.changeLine(t, domain.RoleImplementer, agentTestWriterAgent, false)
	again, err := f.admitRun(t, f.run, agentTestAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	wantCites(t, again, *change.Line, f.enrollment.ID)
}

// TestRouterLeavesALineupTaskOnTheLineup: with no policy, with a policy that
// doesn't order the role, or for a task the lineup admitted before a policy
// existed, admission behaves as it did before routing and writes no line.
func TestRouterLeavesALineupTaskOnTheLineup(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(t *testing.T, f *agentAdmissionFixture){
		"no policy": func(*testing.T, *agentAdmissionFixture) {},
		"the policy doesn't order the role": func(t *testing.T, f *agentAdmissionFixture) {
			f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour), order(domain.RoleReviewer, agentTestSecondAgent))
		},
		"the lineup admitted the task before a policy existed": func(t *testing.T, f *agentAdmissionFixture) {
			admission, err := f.admitRun(t, f.run, agentTestAt.Add(-time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			f.record(t, admission)
			f.setRoutingPolicy(t, agentTestAt.Add(-time.Second),
				order(domain.RoleImplementer, agentTestSecondAgent))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			f.addSecondAgent(t)
			setup(t, f)
			admission, err := f.admitRun(t, f.run, agentTestAt)
			if err != nil {
				t.Fatal(err)
			}
			if binding := admission.AgentBinding; binding == nil ||
				binding.SelectionSource != domain.AgentSelectionSourceLineup || binding.EnrollmentID != f.enrollment.ID {
				t.Fatalf("agent binding = %+v, want the lineup's", binding)
			}
			if lines := f.taskLines(t, f.run.ID); len(lines) != 0 {
				t.Fatalf("task lines = %+v, want none", lines)
			}
		})
	}
}

// TestRouterHoldsWhenEveryPoolIsMarked: with every agent's pool marked, the
// task holds until the earliest reset and is then routed. With no reset on
// any mark, nothing could resume a hold, so the first agent takes it.
func TestRouterHoldsWhenEveryPoolIsMarked(t *testing.T) {
	t.Parallel()
	t.Run("until the earliest reset", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		_, second := f.addSecondAgent(t)
		f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
			order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
		model := f.routeModel(t, agentTestWriterAgent)
		mainReset, secondReset := agentTestAt.Add(3*time.Hour), agentTestAt.Add(time.Hour)
		mainMark := f.markPool(t, routerMainPool, model, &mainReset, agentTestAt.Add(-time.Minute))
		secondMark := f.markPool(t, routerSecondPool, model, &secondReset, agentTestAt.Add(-time.Minute))

		_, err := f.admitRun(t, f.run, agentTestAt)
		if !errors.Is(err, ErrQuotaWindowExhausted) || !invocationDispatchHold(err) {
			t.Fatalf("admitAttempt = %v, want a quota window hold", err)
		}
		if reason, ok := dispatchHoldReason(err); !ok || reason != domain.HoldQuotaWindowExhausted {
			t.Fatalf("hold reason = %q, %t", reason, ok)
		}
		hold := holdDetailFor(err).quotaWindow
		if hold == nil || !reflect.DeepEqual(*hold, domain.QuotaWindowHold{Pool: routerSecondPool, ExhaustedMark: secondMark}) {
			t.Fatalf("hold = %+v, want the earliest reset, %s", hold, secondMark.Window)
		}
		if lines := f.taskLines(t, f.run.ID); len(lines) != 0 {
			t.Fatalf("task lines while held = %+v, want none", lines)
		}

		freed := secondReset.Add(time.Second)
		admission, err := f.admitRun(t, f.run, freed)
		if err != nil {
			t.Fatal(err)
		}
		provenance := policyOrder(1, routerSecondPool)
		provenance.PassedOver = passedOver(agentTestWriterAgent, routerMainPool, mainMark)
		lines := wantRouterLines(t, f, f.run.ID, provenance, freed,
			map[domain.RoleName]string{domain.RoleImplementer: agentTestSecondAgent})
		wantCites(t, admission, lines[domain.RoleImplementer], second.ID)
	})
	// A mark with no reset doesn't hold, here as for a later role, so a pool
	// that has one beside a mark with a reset frees at that reset.
	t.Run("past a mark with no reset", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		f.addSecondAgent(t)
		f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
			order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
		model := f.routeModel(t, agentTestWriterAgent)
		reset := agentTestAt.Add(3 * time.Hour)
		f.markWindow(t, routerMainPool, model, "weekly", nil, agentTestAt.Add(-2*time.Minute))
		mainMark := f.markPool(t, routerMainPool, model, &reset, agentTestAt.Add(-time.Minute))
		f.markPool(t, routerSecondPool, model, nil, agentTestAt.Add(-time.Minute))

		_, err := f.admitRun(t, f.run, agentTestAt)
		hold := holdDetailFor(err).quotaWindow
		if !errors.Is(err, ErrQuotaWindowExhausted) || hold == nil ||
			!reflect.DeepEqual(*hold, domain.QuotaWindowHold{Pool: routerMainPool, ExhaustedMark: mainMark}) {
			t.Fatalf("admitAttempt = %v, hold %+v; want the main pool's reset", err, hold)
		}
		if lines := f.taskLines(t, f.run.ID); len(lines) != 0 {
			t.Fatalf("task lines while held = %+v, want none", lines)
		}
	})
	t.Run("with no reset, on the first agent", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		f.addSecondAgent(t)
		f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
			order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
		model := f.routeModel(t, agentTestWriterAgent)
		f.markPool(t, routerMainPool, model, nil, agentTestAt.Add(-time.Minute))
		f.markPool(t, routerSecondPool, model, nil, agentTestAt.Add(-time.Minute))

		admission, err := f.admitRun(t, f.run, agentTestAt)
		if err != nil {
			t.Fatal(err)
		}
		lines := wantRouterLines(t, f, f.run.ID, policyOrder(1, routerMainPool), agentTestAt,
			map[domain.RoleName]string{domain.RoleImplementer: agentTestWriterAgent})
		wantCites(t, admission, lines[domain.RoleImplementer], f.enrollment.ID)
	})
}

// TestRoutedLaterRoleHoldsOnItsPool: a role whose router line's pool marks
// its model's window holds with quota_window_exhausted until the mark
// clears, then admits on the same line; it is never rerouted. This holds for
// a writer role and for the reviewer.
func TestRoutedLaterRoleHoldsOnItsPool(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*agentAdmissionFixture, ReviewRole) {
		t.Helper()
		f := newAgentAdmissionFixture(t)
		routed, _ := f.reviewTree(t)
		selection := f.selection
		f.engine.admission.environment.Agents = &selection
		f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
			order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent),
			order(domain.RoleReviewer, agentbaseline.ClaudeReviewAgentName, agentTestSecondAgent))
		return f, routed
	}
	reset := agentTestAt.Add(time.Hour)
	wantHeld := func(t *testing.T, err error, mark domain.ExhaustedMark) {
		t.Helper()
		if !errors.Is(err, ErrQuotaWindowExhausted) {
			t.Fatalf("admission = %v, want a quota window hold", err)
		}
		if hold := holdDetailFor(err).quotaWindow; hold == nil ||
			!reflect.DeepEqual(*hold, domain.QuotaWindowHold{Pool: routerMainPool, ExhaustedMark: mark}) {
			t.Fatalf("hold = %+v, want %+v on %s", hold, mark, routerMainPool)
		}
	}

	t.Run("writer", func(t *testing.T) {
		f, routed := setup(t)
		if _, err := f.selection.AdmitReview(context.Background(), f.store, routed, f.run.ID, "review-1", agentTestAt); err != nil {
			t.Fatal(err)
		}
		lines := wantRouterLines(t, f, f.run.ID, policyOrder(1, routerMainPool), agentTestAt, map[domain.RoleName]string{
			domain.RoleImplementer: agentTestWriterAgent, domain.RoleReviewer: agentbaseline.ClaudeReviewAgentName,
		})
		routedLines := f.taskLines(t, f.run.ID)
		mark := f.markPool(t, routerMainPool, f.routeModel(t, agentTestWriterAgent), &reset, agentTestAt.Add(time.Minute))
		_, err := f.admitRun(t, f.run, agentTestAt.Add(2*time.Minute))
		wantHeld(t, err, mark)
		if !invocationDispatchHold(err) {
			t.Fatal("the hold was not classified as an invocation hold")
		}

		admission, err := f.admitRun(t, f.run, reset.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		wantCites(t, admission, lines[domain.RoleImplementer], f.enrollment.ID)
		if got := f.taskLines(t, f.run.ID); !reflect.DeepEqual(got, routedLines) {
			t.Fatalf("lines = %+v, want the routing's %+v", got, routedLines)
		}
	})
	t.Run("reviewer", func(t *testing.T) {
		f, routed := setup(t)
		if _, err := f.admitRun(t, f.run, agentTestAt); err != nil {
			t.Fatal(err)
		}
		lines := wantRouterLines(t, f, f.run.ID, policyOrder(1, routerMainPool), agentTestAt, map[domain.RoleName]string{
			domain.RoleImplementer: agentTestWriterAgent, domain.RoleReviewer: agentbaseline.ClaudeReviewAgentName,
		})
		mark := f.markPool(t, routerMainPool, f.routeModel(t, agentbaseline.ClaudeReviewAgentName), &reset,
			agentTestAt.Add(time.Minute))
		_, err := f.selection.AdmitReview(context.Background(), f.store, routed, f.run.ID, "review-1",
			agentTestAt.Add(2*time.Minute))
		wantHeld(t, err, mark)
		if errors.Is(err, ErrAgentNotAdmissible) {
			t.Fatalf("a hold reads as a refusal: %v", err)
		}

		admission, err := f.selection.AdmitReview(context.Background(), f.store, routed, f.run.ID, "review-1",
			reset.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if admission.AgentBinding.SelectionSource != domain.AgentSelectionSourceTaskLine ||
			admission.AgentBinding.SelectionRecordID != lines[domain.RoleReviewer].ID {
			t.Fatalf("review binding = %+v, want the reviewer's router line", admission.AgentBinding)
		}
	})
}

// TestRouterWritesOnlyWhenItRoutes: an admission the router doesn't route,
// or holds, moves no sync revision, so a held role's retries stay quiet; one
// it routes records its lines in one write.
func TestRouterWritesOnlyWhenItRoutes(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	ctx := context.Background()
	route := func(t *testing.T, at time.Time) (error, bool) {
		t.Helper()
		before := serverRevision(t, f.store)
		err := f.selection.routeBeforeAdmission(ctx, f.store, f.run.ID, domain.RoleImplementer, at)
		return err, serverRevision(t, f.store) != before
	}
	if err, moved := route(t, agentTestAt); err != nil || moved {
		t.Fatalf("with no policy = %v, moved %t; want nil, still", err, moved)
	}

	f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
		order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
	model := f.routeModel(t, agentTestWriterAgent)
	reset := agentTestAt.Add(time.Hour)
	f.markPool(t, routerMainPool, model, &reset, agentTestAt.Add(-time.Minute))
	f.markPool(t, routerSecondPool, model, &reset, agentTestAt.Add(-time.Minute))
	if err, moved := route(t, agentTestAt); !errors.Is(err, ErrQuotaWindowExhausted) || moved {
		t.Fatalf("every pool held = %v, moved %t; want a hold, still", err, moved)
	}

	freed := reset.Add(time.Second)
	if err, moved := route(t, freed); err != nil || !moved {
		t.Fatalf("routed = %v, moved %t; want nil, moved", err, moved)
	}
	if err, moved := route(t, freed); err != nil || moved {
		t.Fatalf("already routed = %v, moved %t; want nil, still", err, moved)
	}
}

// TestRouterCitesThePolicyInEffect: a revision set after the admission's
// instant, as when routing set commits between the instant and the router's
// read, is not cited; the line cites the revision in effect then, which its
// anchor accepts. With none in effect, the task stays on the lineup.
func TestRouterCitesThePolicyInEffect(t *testing.T) {
	t.Parallel()
	t.Run("an earlier revision", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		f.addSecondAgent(t)
		f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour), order(domain.RoleImplementer, agentTestWriterAgent))
		f.setRoutingPolicy(t, agentTestAt.Add(time.Minute), order(domain.RoleImplementer, agentTestSecondAgent))

		admission, err := f.admitRun(t, f.run, agentTestAt)
		if err != nil {
			t.Fatal(err)
		}
		lines := wantRouterLines(t, f, f.run.ID, policyOrder(1, routerMainPool), agentTestAt,
			map[domain.RoleName]string{domain.RoleImplementer: agentTestWriterAgent})
		wantCites(t, admission, lines[domain.RoleImplementer], f.enrollment.ID)
	})
	t.Run("none yet", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		f.addSecondAgent(t)
		f.setRoutingPolicy(t, agentTestAt.Add(time.Minute), order(domain.RoleImplementer, agentTestSecondAgent))

		if _, err := f.admitRun(t, f.run, agentTestAt); err != nil {
			t.Fatal(err)
		}
		if lines := f.taskLines(t, f.run.ID); len(lines) != 0 {
			t.Fatalf("task lines = %+v, want the lineup", lines)
		}
	})
}

// TestRouterRanksPoolsByTier is revision 107's ranking: the drain-first
// account when the role's order holds an agent on it, then the soonest known
// weekly reset, then the policy's order, each passing over a marked pool.
func TestRouterRanksPoolsByTier(t *testing.T) {
	t.Parallel()
	const weekly, fiveHour = "seven_day", "five_hour"
	week := 7 * 24 * time.Hour
	observed := agentTestAt.Add(-time.Minute)
	soonest := func(pool domain.UsagePoolID, ranked ...domain.TaskLineRankingReading) *domain.TaskLineProvenance {
		return &domain.TaskLineProvenance{
			PolicyRevision: 1, Tier: domain.TaskLineSelectionSoonestReset, Pool: pool, RankedBy: ranked,
		}
	}
	cases := []struct {
		name string
		// setup seeds the pools and returns the agent the routing picks and
		// its provenance.
		setup func(t *testing.T, f *agentAdmissionFixture, model string) (string, *domain.TaskLineProvenance)
		drain *domain.UsagePoolID
	}{
		{
			name: "the soonest weekly reset leads the policy's order",
			setup: func(t *testing.T, f *agentAdmissionFixture, _ string) (string, *domain.TaskLineProvenance) {
				main := f.readWindow(t, routerMainPool, weekly, week, agentTestAt.Add(5*24*time.Hour), observed)
				second := f.readWindow(t, routerSecondPool, weekly, week, agentTestAt.Add(2*24*time.Hour), observed)
				return agentTestSecondAgent, soonest(routerSecondPool, second, main)
			},
		},
		{
			name: "the soonest of several weekly windows ranks a pool",
			setup: func(t *testing.T, f *agentAdmissionFixture, _ string) (string, *domain.TaskLineProvenance) {
				main := f.readWindow(t, routerMainPool, weekly, week, agentTestAt.Add(3*24*time.Hour), observed)
				f.readWindow(t, routerSecondPool, weekly, week, agentTestAt.Add(4*24*time.Hour), observed)
				second := f.readWindow(t, routerSecondPool, "seven_day_extra", week, agentTestAt.Add(2*24*time.Hour),
					observed.Add(time.Second))
				return agentTestSecondAgent, soonest(routerSecondPool, second, main)
			},
		},
		{
			name: "equal resets keep the policy's order",
			setup: func(t *testing.T, f *agentAdmissionFixture, _ string) (string, *domain.TaskLineProvenance) {
				reset := agentTestAt.Add(2 * 24 * time.Hour)
				main := f.readWindow(t, routerMainPool, weekly, week, reset, observed)
				second := f.readWindow(t, routerSecondPool, weekly, week, reset, observed)
				return agentTestWriterAgent, soonest(routerMainPool, main, second)
			},
		},
		{
			name: "a known reset leads a pool with none",
			setup: func(t *testing.T, f *agentAdmissionFixture, _ string) (string, *domain.TaskLineProvenance) {
				second := f.readWindow(t, routerSecondPool, weekly, week, agentTestAt.Add(6*24*time.Hour), observed)
				return agentTestSecondAgent, soonest(routerSecondPool, second)
			},
		},
		{
			name: "a reset already passed ranks nothing",
			setup: func(t *testing.T, f *agentAdmissionFixture, _ string) (string, *domain.TaskLineProvenance) {
				f.readWindow(t, routerSecondPool, weekly, week, agentTestAt, observed)
				return agentTestWriterAgent, policyOrder(1, routerMainPool)
			},
		},
		{
			name: "a window that isn't weekly ranks nothing",
			setup: func(t *testing.T, f *agentAdmissionFixture, _ string) (string, *domain.TaskLineProvenance) {
				f.readWindow(t, routerSecondPool, fiveHour, 5*time.Hour, agentTestAt.Add(time.Hour), observed)
				return agentTestWriterAgent, policyOrder(1, routerMainPool)
			},
		},
		{
			name: "a marked pool keeps its rank and is passed over",
			setup: func(t *testing.T, f *agentAdmissionFixture, model string) (string, *domain.TaskLineProvenance) {
				main := f.readWindow(t, routerMainPool, weekly, week, agentTestAt.Add(5*24*time.Hour), observed)
				second := f.readWindow(t, routerSecondPool, weekly, week, agentTestAt.Add(2*24*time.Hour), observed)
				reset := agentTestAt.Add(time.Hour)
				mark := f.markPool(t, routerSecondPool, model, &reset, observed.Add(time.Second))
				provenance := soonest(routerMainPool, second, main)
				provenance.PassedOver = passedOver(agentTestSecondAgent, routerSecondPool, mark)
				return agentTestWriterAgent, provenance
			},
		},
		{
			name:  "the drain-first account leads every reset",
			drain: new(routerSecondPool),
			setup: func(t *testing.T, f *agentAdmissionFixture, _ string) (string, *domain.TaskLineProvenance) {
				main := f.readWindow(t, routerMainPool, weekly, week, agentTestAt.Add(24*time.Hour), observed)
				// The account's own reset ranks nothing.
				f.readWindow(t, routerSecondPool, weekly, week, agentTestAt.Add(2*24*time.Hour), observed)
				return agentTestSecondAgent, &domain.TaskLineProvenance{
					PolicyRevision: 1, Tier: domain.TaskLineSelectionDrainFirstAccount, Pool: routerSecondPool,
					RankedBy: []domain.TaskLineRankingReading{main},
				}
			},
		},
		{
			name:  "a marked drain-first account is passed over",
			drain: new(routerSecondPool),
			setup: func(t *testing.T, f *agentAdmissionFixture, model string) (string, *domain.TaskLineProvenance) {
				reset := agentTestAt.Add(time.Hour)
				mark := f.markPool(t, routerSecondPool, model, &reset, observed)
				provenance := policyOrder(1, routerMainPool)
				provenance.PassedOver = passedOver(agentTestSecondAgent, routerSecondPool, mark)
				return agentTestWriterAgent, provenance
			},
		},
		{
			name:  "a drain-first account the role has no agent on ranks nothing",
			drain: new(domain.UsagePoolID("identity:claude-elsewhere")),
			setup: func(t *testing.T, f *agentAdmissionFixture, _ string) (string, *domain.TaskLineProvenance) {
				second := f.readWindow(t, routerSecondPool, weekly, week, agentTestAt.Add(2*24*time.Hour), observed)
				return agentTestSecondAgent, soonest(routerSecondPool, second)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			f.addSecondAgent(t)
			f.setDrainingPolicy(t, agentTestAt.Add(-time.Hour), tc.drain,
				order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
			agent, provenance := tc.setup(t, f, f.routeModel(t, agentTestWriterAgent))

			if _, err := f.admitRun(t, f.run, agentTestAt); err != nil {
				t.Fatal(err)
			}
			wantRouterLines(t, f, f.run.ID, provenance, agentTestAt, map[domain.RoleName]string{domain.RoleImplementer: agent})
		})
	}
}
