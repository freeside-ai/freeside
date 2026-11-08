package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// An escalated task's specifier routes among the counterparts of its order
// (plan §5.13, Specifier Triage), and every widened anchor check refuses a line
// outside what that routing writes.

const agentTestSecondEscalatedAgent = "claude-second-escalated"

var agentTestSpecifierAgent = agentbaseline.WriterAgentName(domain.RoleSpecifier)

// addSecondEscalated adds the second agent's escalated counterpart: an agent
// like the fixture's escalated one on the second identity's enrollment, and
// the lineup's escalated line pairing the second agent with it. Call it after
// addSecondAgent and addEscalatedSpecifier.
func (f *agentAdmissionFixture) addSecondEscalated(t *testing.T) {
	t.Helper()
	tree := f.selection.Tree
	second, ok := tree.Agent(agentTestSecondAgent)
	if !ok {
		t.Fatal("the tree has no second agent")
	}
	agent, ok := tree.Agent(agentTestEscalatedAgent)
	if !ok {
		t.Fatal("the tree has no escalated agent")
	}
	agent.Name, agent.Enrollment, agent.Route = agentTestSecondEscalatedAgent, second.Enrollment, second.Route
	tree.Agents = append(slices.Clone(tree.Agents), agent)
	tree.Sort()
	line, err := lineupLine(tree, domain.RoleSpecifier)
	if err != nil {
		t.Fatal(err)
	}
	// The second enrollment's counterpart: the escalated model on the second
	// agent's enrollment.
	line.Names.Enrollment, line.Names.Model = second.Enrollment, agentTestEscalatedModel
	key, err := domain.LineupEscalatedKey(domain.RoleSpecifier, second.Enrollment)
	if err != nil {
		t.Fatal(err)
	}
	tree.Lineup = append(slices.Clone(tree.Lineup), agenttree.LineupLine{
		Key: strings.TrimPrefix(key, domain.LineupRoleKeyPrefix), Selection: line,
	})
	f.useTree(t, tree)
}

// escalatedRoutingFixture has a second agent on its own pool, a counterpart
// for each pool's specifier agent, and a policy ordering the specifier's
// lineup agent and then the second agent for the specifier, implementer, and
// remediator.
func escalatedRoutingFixture(t *testing.T) (*agentAdmissionFixture, domain.RoutingPolicy, domain.ClientEnrollment) {
	t.Helper()
	f := newAgentAdmissionFixture(t)
	_, second := f.addSecondAgent(t)
	f.addEscalatedSpecifier(t, true)
	f.addSecondEscalated(t)
	policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
		order(domain.RoleSpecifier, agentTestEnrollment, agentTestSecondEnrollment),
		order(domain.RoleImplementer, agentTestEnrollment, agentTestSecondEnrollment),
		order(domain.RoleRemediator, agentTestEnrollment, agentTestSecondEnrollment))
	return f, policy, second
}

// escalate records the fixture task's escalated triage and its triage line at
// agentTestAt, as triage writes them.
func (f *agentAdmissionFixture) escalate(t *testing.T) domain.TaskLine {
	t.Helper()
	task := f.task(t, f.run.ID)
	f.recordTriage(t, domain.TaskTriage{
		TaskID: task.ID, Outcome: domain.TriageOutcomeCallFailed, Escalated: true, RecordedAt: agentTestAt,
	})
	return f.appendLine(t, domain.TaskLineInput{
		TaskID: task.ID, Role: domain.RoleSpecifier, Agent: agentTestEscalatedAgent,
		Source: domain.TaskLineSourceTriage, SetBy: string(task.ID),
	}, agentTestAt)
}

// heads returns each role's newest line on the fixture task.
func (f *agentAdmissionFixture) heads(t *testing.T) map[domain.RoleName]domain.TaskLine {
	t.Helper()
	heads := map[domain.RoleName]domain.TaskLine{}
	for _, line := range f.taskLines(t, f.run.ID) {
		heads[line.Role] = line
	}
	return heads
}

// The counterparts' windows for the escalated model pick the pool: a mark on
// the main pool's counterpart routes the specifier to the second pool's, at
// version 2 over the triage line, and every other role lands on that pool. A
// mark on the standard model does not move it.
func TestRouterRoutesAnEscalatedSpecifierAmongCounterparts(t *testing.T) {
	t.Parallel()
	at := agentTestAt.Add(time.Minute)
	reset := agentTestAt.Add(time.Hour)
	t.Run("counterpart marked", func(t *testing.T) {
		f, _, second := escalatedRoutingFixture(t)
		triage := f.escalate(t)
		mark := f.markPool(t, routerMainPool, f.routeModel(t, agentTestEscalatedAgent), &reset, agentTestAt.Add(-time.Minute))
		f.engine.admission.now = func() time.Time { return at }
		admission, err := f.admitSpecifier(t, "inv-specification-1")
		if err != nil {
			t.Fatal(err)
		}
		heads := f.heads(t)
		specifier := heads[domain.RoleSpecifier]
		if specifier.Source != domain.TaskLineSourceRouter || f.lineAgent(specifier) != agentTestSecondEscalatedAgent ||
			specifier.Version != 2 || specifier.PredecessorID == nil || *specifier.PredecessorID != triage.ID {
			t.Fatalf("specifier head = %+v, want the second counterpart over %s", specifier, triage.ID)
		}
		provenance := specifier.Provenance
		if provenance == nil || provenance.Pool != routerSecondPool || provenance.PassedOver == nil ||
			provenance.PassedOver.Enrollment != agentTestEnrollment || provenance.PassedOver.Window != mark.Window {
			t.Fatalf("provenance = %+v, want the main counterpart passed over", provenance)
		}
		for _, role := range []domain.RoleName{domain.RoleImplementer, domain.RoleRemediator} {
			if line := heads[role]; line.Source != domain.TaskLineSourceRouter || f.lineAgent(line) != agentTestSecondAgent {
				t.Errorf("%s head = %+v, want the second agent", role, line)
			}
		}
		wantCites(t, admission, specifier, second.ID)
		// The implementer's line names the passed-over counterpart's
		// enrollment, which the policy orders, so its anchor accepts it.
		implementer, err := f.admitRole(t, domain.RoleImplementer)
		if err != nil || implementer.binding.SelectionRecordID != heads[domain.RoleImplementer].ID {
			t.Fatalf("implementer admission = %+v, %v; want its router line", implementer.binding, err)
		}
	})
	t.Run("standard model marked", func(t *testing.T) {
		f, _, _ := escalatedRoutingFixture(t)
		f.escalate(t)
		f.markPool(t, routerMainPool, f.routeModel(t, agentTestSpecifierAgent), &reset, agentTestAt.Add(-time.Minute))
		f.engine.admission.now = func() time.Time { return at }
		admission, err := f.admitSpecifier(t, "inv-specification-1")
		if err != nil {
			t.Fatal(err)
		}
		specifier := f.heads(t)[domain.RoleSpecifier]
		if f.lineAgent(specifier) != agentTestEscalatedAgent || specifier.Provenance.Pool != routerMainPool ||
			specifier.Provenance.PassedOver != nil {
			t.Fatalf("specifier head = %+v (provenance %+v), want the main counterpart", specifier, specifier.Provenance)
		}
		wantCites(t, admission, specifier, f.enrollment.ID)
	})
}

// Every counterpart marked parks the specifier and writes nothing.
func TestRouterParksAnEscalatedSpecifierWhenEveryCounterpartIsMarked(t *testing.T) {
	t.Parallel()
	f, _, _ := escalatedRoutingFixture(t)
	triage := f.escalate(t)
	model := f.routeModel(t, agentTestEscalatedAgent)
	reset := agentTestAt.Add(time.Hour)
	f.markPool(t, routerMainPool, model, &reset, agentTestAt.Add(-time.Minute))
	f.markPool(t, routerSecondPool, model, &reset, agentTestAt.Add(-time.Minute))
	f.engine.admission.now = func() time.Time { return agentTestAt.Add(time.Minute) }
	_, err := f.admitSpecifier(t, "inv-specification-1")
	var exhausted *QuotaWindowExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("admission = %v, want a quota hold", err)
	}
	if lines := f.taskLines(t, f.run.ID); len(lines) != 1 || lines[0].ID != triage.ID {
		t.Fatalf("lines = %+v, want only the triage line", lines)
	}
}

// With no counterpart to route to, the router writes nothing at the
// specifier's admission and the triage line keeps it escalated; the later
// roles route at the implementer's first admission, and the specifier keeps
// its triage line.
func TestRouterLeavesAnEscalatedSpecifierWithNoCounterpartOnTriage(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	f.addEscalatedSpecifier(t, true)
	f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
		order(domain.RoleSpecifier, agentTestSecondEnrollment),
		order(domain.RoleImplementer, agentTestSecondEnrollment))
	triage := f.escalate(t)
	f.engine.admission.now = func() time.Time { return agentTestAt.Add(time.Minute) }
	admission, err := f.admitSpecifier(t, "inv-specification-1")
	if err != nil {
		t.Fatal(err)
	}
	wantCites(t, admission, triage, f.enrollment.ID)
	if lines := f.taskLines(t, f.run.ID); len(lines) != 1 {
		t.Fatalf("lines = %+v, want only the triage line", lines)
	}
	if _, err := f.admitRun(t, f.run, agentTestAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	heads := f.heads(t)
	if heads[domain.RoleSpecifier].ID != triage.ID || f.lineAgent(heads[domain.RoleImplementer]) != agentTestSecondAgent {
		t.Fatalf("heads = %+v, want the triage specifier and a routed implementer", heads)
	}
}

// Each widened anchor check refuses a line outside what an escalated
// routing writes.
func TestEscalatedRouterLineAnchorRefusals(t *testing.T) {
	t.Parallel()
	provenance := func(t *testing.T, f *agentAdmissionFixture, pool domain.UsagePoolID) *domain.TaskLineProvenance {
		p := policyOrder(1, pool)
		p.PassedOver = &domain.TaskLinePassedOverMark{
			Enrollment: agentTestEnrollment, Pool: routerMainPool, Window: "five_hour",
			Source: domain.ExhaustedMarkProviderRefusal, MarkedAt: agentTestAt.Add(-time.Minute),
		}
		return p
	}
	routed := func(t *testing.T, f *agentAdmissionFixture, role domain.RoleName, agent string, p *domain.TaskLineProvenance) {
		f.appendLine(t, domain.TaskLineInput{
			TaskID: f.task(t, f.run.ID).ID, Role: role, Agent: agent, Source: domain.TaskLineSourceRouter,
			SetBy: routerSetBy(f.task(t, f.run.ID).ID), Provenance: p,
		}, agentTestAt.Add(time.Minute))
	}
	for _, tc := range []struct {
		name  string
		role  domain.RoleName
		setup func(*testing.T, *agentAdmissionFixture)
		want  string // empty: admitted
	}{
		{
			name: "the escalated routing", role: domain.RoleSpecifier,
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.escalate(t)
				routed(t, f, domain.RoleSpecifier, agentTestSecondEscalatedAgent, provenance(t, f, routerSecondPool))
			},
		},
		{
			name: "a counterpart for another role", role: domain.RoleImplementer,
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.escalate(t)
				routed(t, f, domain.RoleImplementer, agentTestSecondEscalatedAgent, policyOrder(1, routerSecondPool))
			},
			want: "does not order the line's selection",
		},
		{
			name: "a task that did not escalate", role: domain.RoleSpecifier,
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.recordTriage(t, domain.TaskTriage{
					TaskID: f.task(t, f.run.ID).ID, Outcome: domain.TriageOutcomeAnswered,
					Tier: new(domain.TriageTierStandard), RecordedAt: agentTestAt,
				})
				routed(t, f, domain.RoleSpecifier, agentTestSecondEscalatedAgent, policyOrder(1, routerSecondPool))
			},
			want: "does not order the line's selection",
		},
		{
			name: "a passed-over enrollment the policy does not order", role: domain.RoleImplementer,
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				p := provenance(t, f, routerSecondPool)
				p.PassedOver.Enrollment = "claude-unordered/claude_code"
				routed(t, f, domain.RoleImplementer, agentTestSecondAgent, p)
			},
			want: "orders nothing the line passed over",
		},
		{
			name: "a counterpart the lineup no longer names", role: domain.RoleSpecifier,
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.escalate(t)
				routed(t, f, domain.RoleSpecifier, agentTestSecondEscalatedAgent, provenance(t, f, routerSecondPool))
				second, _ := f.selection.Tree.Agent(agentTestSecondAgent)
				key, err := domain.LineupEscalatedKey(domain.RoleSpecifier, second.Enrollment)
				if err != nil {
					t.Fatal(err)
				}
				f.useTree(t, withoutLineupLine(t, f.selection.Tree, strings.TrimPrefix(key, domain.LineupRoleKeyPrefix)))
			},
			want: "does not order the line's selection",
		},
		{
			name: "a counterpart the lineup moved", role: domain.RoleSpecifier,
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.escalate(t)
				routed(t, f, domain.RoleSpecifier, agentTestSecondEscalatedAgent, provenance(t, f, routerSecondPool))
				tree := f.selection.Tree
				second, _ := tree.Agent(agentTestSecondAgent)
				key, err := domain.LineupEscalatedKey(domain.RoleSpecifier, second.Enrollment)
				if err != nil {
					t.Fatal(err)
				}
				tree.Lineup = slices.Clone(tree.Lineup)
				i := slices.IndexFunc(tree.Lineup, func(l agenttree.LineupLine) bool {
					return l.Key == strings.TrimPrefix(key, domain.LineupRoleKeyPrefix)
				})
				tree.Lineup[i].Selection.Names.Model = agentTestOtherEscalatedModel
				f.useTree(t, tree)
			},
			want: "does not order the line's selection",
		},
		{
			name: "a second version over an operator's line", role: domain.RoleSpecifier,
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.escalate(t)
				f.changeLine(t, domain.RoleSpecifier, agentTestSpecifierAgent, false)
				// The change is version 2; the router's line over it is 3.
				routed(t, f, domain.RoleSpecifier, agentTestSecondEscalatedAgent, provenance(t, f, routerSecondPool))
			},
			want: "or an escalated specifier's second",
		},
		{
			name: "a second version over a submitted line", role: domain.RoleSpecifier,
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.recordTriage(t, domain.TaskTriage{
					TaskID: f.task(t, f.run.ID).ID, Outcome: domain.TriageOutcomeCallFailed, Escalated: true,
					RecordedAt: agentTestAt,
				})
				f.submitLine(t, domain.RoleSpecifier, agentTestEscalatedAgent)
				routed(t, f, domain.RoleSpecifier, agentTestSecondEscalatedAgent, provenance(t, f, routerSecondPool))
			},
			want: "only over its triage line",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, second := escalatedRoutingFixture(t)
			tc.setup(t, f)
			admission, err := f.admitRole(t, tc.role)
			if tc.want == "" {
				if err != nil || admission.binding.EnrollmentID != second.ID {
					t.Fatalf("admission = %+v, %v; want the second counterpart", admission.binding, err)
				}
				return
			}
			if !errors.Is(err, ErrAgentNotAdmissible) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("admission = %+v, %v; want refusal naming %q", admission.binding, err, tc.want)
			}
		})
	}
}

// TestAPreUnitEscalatedSpecifierLineNeedsItsOrder: under a revision stored
// before #2736, an escalated specifier's router line is ordered only while an
// agent the revision orders for the specifier still resolves in the tree, and
// its counterpart check then takes that agent's enrollment as its base. With
// no such agent, or with an empty tree, the anchor refuses the line, and the
// counterpart check has no base to accept it under.
func TestAPreUnitEscalatedSpecifierLineNeedsItsOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		agent     string
		emptyTree bool
		ordered   bool
	}{
		"an ordered agent resolves": {agent: agentTestSpecifierAgent, ordered: true},
		"no ordered agent resolves": {agent: "claude-gone"},
		"the tree is empty":         {agent: agentTestSpecifierAgent, emptyTree: true},
	} {
		f := newAgentAdmissionFixture(t)
		f.addEscalatedSpecifier(t, true)
		triage := f.escalate(t)
		f.setPreUnitPolicy(t, domain.RoleSpecifier, tc.agent)
		line := domain.TaskLine{
			TaskID: triage.TaskID, Role: domain.RoleSpecifier, Version: 2, Encoding: domain.TaskLineEncodingV2,
			Selection: f.lineSelection(t, agentTestEscalatedAgent), Source: domain.TaskLineSourceRouter,
			SetAt: agentTestAt.Add(time.Minute), Provenance: &domain.TaskLineProvenance{
				PolicyRevision: 1, Tier: domain.TaskLineSelectionPolicyOrder, Pool: routerMainPool,
				RankedBy: []domain.TaskLineRankingReading{},
			},
		}
		tree := f.selection.Tree
		if tc.emptyTree {
			tree = agenttree.Tree{}
		}
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			if err := anchorLinePolicy(ctx, tx, tree, line); (err == nil) != tc.ordered {
				t.Errorf("%s: anchor = %v, want ordered %t", name, err, tc.ordered)
			}
			if tc.emptyTree {
				return nil
			}
			bases, _, err := counterpartBases(ctx, tx, tree, domain.LineupSelection{}, line)
			if err != nil {
				return err
			}
			if bases == nil || (len(bases) > 0) != tc.ordered {
				t.Errorf("%s: counterpart bases = %#v, want a required set, non-empty %t", name, bases, tc.ordered)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
