package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The router applies the routing policy once per task, at the first admission
// of its first task-line role that has no operator line, and pins every role
// of the task to the one usage pool it picks (plan §5.4, routing policy;
// revisions 99, 103, and 107). It writes router-sourced task lines and nothing
// else: admission still reads only the line, never a reading or a mark. Later
// roles read those lines, and a line's pool whose window is marked holds the
// role until the mark clears; nothing reroutes a task in flight.

// routerSetByPrefix starts the SetBy of every router line, followed by the
// line's own task id.
const routerSetByPrefix = "router:"

func routerSetBy(taskID domain.TaskID) string { return routerSetByPrefix + string(taskID) }

// routeCandidate is a policy agent that resolves in the tree against the
// store: the pool its identity meters and the route model id its offer passes.
type routeCandidate struct {
	agent string
	pool  domain.UsagePoolID
	model string
}

// routeBeforeAdmission runs the router for the role's admission and then the
// routed pool's hold. Stage and review admission call it before they resolve
// the role's agent, each in its own transaction ahead of admission's read.
//
// An attempt whose card chose the role's agent neither routes nor holds: the
// card overrides the task's lines for that attempt, so their pool is not the
// one it runs on. A card that cannot be read is admission's to refuse.
func (s AgentSelection) routeBeforeAdmission(
	ctx context.Context, st *store.Store, runID domain.RunID, role domain.RoleName, now time.Time,
) error {
	if runID != "" && role.TaskLineEligible() {
		var carded bool
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			_, carded, err = attemptCard(ctx, tx, runID, role)
			return err
		}); err == nil && carded {
			return nil
		}
	}
	if err := s.routeTask(ctx, st, runID, role, now); err != nil {
		return err
	}
	return s.requireRoutedPoolOpen(ctx, st, runID, role, now)
}

// routeTask writes the task's router lines when this admission is its first
// routable one: the role is a task-line role with no line version; no role of
// the task has a router version; the policy in effect at now orders the role
// (policyInEffect); and no admission of the task's runs ran a task-line role
// on the lineup, which would mean the task was admitted before routing
// applied to it. The checks run cheapest first, so a deployment with no
// policy pays one line read.
//
// It decides in a read and opens a write only when it has lines to record,
// deciding again inside it: every Store.Write moves the sync revision, and an
// admission that routes nothing, or a held one that retries, must not.
//
// The role's order ranks the pools (pickRoute): the drain-first account,
// then the soonest known weekly reset, then the policy's order. The first
// ranked agent whose pool's windows for its model carry no exhausted mark
// takes the task. When every one is marked, it returns a
// QuotaWindowExhaustedError for the earliest reset to hold the attempt on, or,
// when no mark gives a reset, picks the first ranked agent and lets the
// provider decide. With no resolvable agent it writes nothing, and the task
// stays on the lineup.
//
// An escalated task's specifier (its head line is the triage line and its
// record escalated) routes over that line among the counterparts of its
// order (escalatedOrder), so the counterparts' windows pick the pool, and its
// router line is version 2. With no resolvable counterpart it writes
// nothing, and the triage line keeps the specifier escalated.
func (s AgentSelection) routeTask(
	ctx context.Context, st *store.Store, runID domain.RunID, role domain.RoleName, now time.Time,
) error {
	if runID == "" || !role.TaskLineEligible() {
		return nil
	}
	var inputs []domain.TaskLineInput
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		inputs, err = s.planRoute(ctx, tx, runID, role, now)
		return err
	}); err != nil || len(inputs) == 0 {
		return err
	}
	return st.Write(ctx, func(tx *store.WriteTx) error {
		inputs, err := s.planRoute(ctx, &tx.ReadTx, runID, role, now)
		if err != nil || len(inputs) == 0 {
			return err
		}
		_, err = tx.RecordRouterTaskLines(ctx, inputs[0].TaskID, inputs, now)
		return err
	})
}

// planRoute decides routeTask's lines, the deciding role's first. It returns
// none when the admission routes nothing.
func (s AgentSelection) planRoute(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID, role domain.RoleName, now time.Time,
) ([]domain.TaskLineInput, error) {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	// RunTaskLine refuses a run with no task at admission.
	if run.TaskID == "" {
		return nil, nil
	}
	lines, err := tx.TaskLines(ctx, run.TaskID)
	if err != nil {
		return nil, err
	}
	lined := make(map[domain.RoleName]bool, len(lines))
	var specifierHead domain.TaskLine
	for _, line := range lines {
		if line.Source == domain.TaskLineSourceRouter {
			return nil, nil
		}
		lined[line.Role] = true
		// Lines come ordered by role, then version: the last is the head.
		if line.Role == domain.RoleSpecifier {
			specifierHead = line
		}
	}
	escalated := false
	if role == domain.RoleSpecifier && specifierHead.Source == domain.TaskLineSourceTriage {
		if escalated, err = taskEscalated(ctx, tx, run.TaskID); err != nil {
			return nil, err
		}
		// The triage line names the tier, not a pool: the router may still
		// route the specifier, among the counterparts (escalatedOrder).
		lined[role] = !escalated
	}
	if lined[role] {
		return nil, nil
	}
	policy, found, err := policyInEffect(ctx, tx, now)
	if err != nil || !found {
		return nil, err
	}
	order := routingOrder(policy, role)
	if order == nil {
		return nil, nil
	}
	if lineup, err := admittedThroughLineup(ctx, tx, run.TaskID); err != nil || lineup {
		return nil, err
	}
	candidates, err := s.routeCandidates(ctx, tx, policy)
	if err != nil {
		return nil, err
	}
	if escalated {
		if order, err = s.escalatedOrder(ctx, tx, order, candidates); err != nil || len(order) == 0 {
			return nil, err
		}
	}
	choice, err := pickRoute(ctx, tx, policy, order, candidates, now)
	if err != nil || choice == nil {
		return nil, err
	}
	provenance := &choice.provenance
	lined[role] = true
	pools := make(map[string]domain.UsagePoolID, len(candidates))
	for name, candidate := range candidates {
		pools[name] = candidate.pool
	}
	choices := append([]domain.TaskLineChoice{{Role: role, Agent: choice.agent.agent}},
		routerLinesForPool(policy, choice.agent.pool, pools, lined)...)
	inputs := make([]domain.TaskLineInput, 0, len(choices))
	for _, line := range choices {
		selection, err := s.Tree.LineSelection(line.Role, line.Agent)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, domain.TaskLineInput{
			TaskID: run.TaskID, Role: line.Role, Selection: &selection,
			Source: domain.TaskLineSourceRouter, SetBy: routerSetBy(run.TaskID), Provenance: provenance,
		})
	}
	return inputs, nil
}

// taskEscalated reports whether the task's triage record escalated its
// specifier.
func taskEscalated(ctx context.Context, tx *store.ReadTx, taskID domain.TaskID) (bool, error) {
	record, found, err := tx.GetTaskTriage(ctx, taskID)
	return found && record.Escalated, err
}

// escalatedOrder is an escalated specifier's order (plan §5.13, Specifier
// Triage): for each agent of the policy's specifier order, in that order,
// the agent that runs the escalated line the lineup names for its
// enrollment (escalatedAgent). An agent with no counterpart, a counterpart
// that does not resolve, and a repeat are skipped; each
// resolvable counterpart joins candidates, so its pool's windows for the
// escalated model rank it. A lineup that does not resolve has none, and
// admission refuses it.
func (s AgentSelection) escalatedOrder(
	ctx context.Context, tx *store.ReadTx, order []string, candidates map[string]routeCandidate,
) ([]string, error) {
	lineup, err := s.Tree.ResolveLineup()
	if err != nil {
		return nil, nil
	}
	var counterparts []string
	for _, name := range order {
		counterpart, ok := escalatedAgent(s.Tree, lineup, name)
		if !ok || slices.Contains(counterparts, counterpart) {
			continue
		}
		if _, ok := candidates[counterpart]; !ok {
			candidate, ok, err := s.routeCandidate(ctx, tx, counterpart)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			candidates[counterpart] = candidate
		}
		counterparts = append(counterparts, counterpart)
	}
	return counterparts, nil
}

// escalatedAgent is the tree agent that runs the escalated line the lineup
// names for the specifier agent name's enrollment, or false when the agent
// does not resolve to a selection, its enrollment has no escalated line, or
// no one agent runs that line's selection. The routing policy still names
// agents (#2736), so the line maps back to one through the bridge.
func escalatedAgent(tree agenttree.Tree, lineup domain.Lineup, name string) (string, bool) {
	selection, err := tree.SelectionOfAgent(name)
	if err != nil {
		return "", false
	}
	counterpart, ok := lineup.EscalatedCounterpart(domain.RoleSpecifier, string(selection.EnrollmentID))
	if !ok {
		return "", false
	}
	escalated, err := tree.AgentForSelection(domain.RoleSpecifier, counterpart.TaskLineSelection())
	return escalated, err == nil
}

// policyInEffect returns the revision in effect at now: the current one, or,
// when that was set after now, the latest set by then. now is fixed before
// this read, so a revision recorded in between, or a clock step back, must
// not be cited by a line whose anchor requires its policy to precede it.
func policyInEffect(ctx context.Context, tx *store.ReadTx, now time.Time) (domain.RoutingPolicy, bool, error) {
	current, err := tx.CurrentRoutingPolicy(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return domain.RoutingPolicy{}, false, nil
	}
	if err != nil {
		return domain.RoutingPolicy{}, false, err
	}
	if !current.SetAt.After(now) {
		return current, true, nil
	}
	revisions, err := tx.RoutingPolicyRevisions(ctx)
	if err != nil {
		return domain.RoutingPolicy{}, false, err
	}
	for _, policy := range slices.Backward(revisions) {
		if !policy.SetAt.After(now) {
			return policy, true, nil
		}
	}
	return domain.RoutingPolicy{}, false, nil
}

// routingOrder returns the policy's agent order for the role, nil when the
// policy does not order it.
func routingOrder(policy domain.RoutingPolicy, role domain.RoleName) []string {
	i := slices.IndexFunc(policy.Roles, func(r domain.RoutingPolicyRole) bool { return r.Role == role })
	if i < 0 {
		return nil
	}
	return policy.Roles[i].Agents
}

// admittedThroughLineup reports whether any run of the task holds an execution
// or review admission of a task-line role that the lineup selected. It reads
// the stored records without re-gating them against current policy: routing
// asks what history holds, and a later trust-profile change must not hide it.
// The shadow reviewer always runs on the lineup, so its admissions don't
// count.
func admittedThroughLineup(ctx context.Context, tx *store.ReadTx, taskID domain.TaskID) (bool, error) {
	runIDs, err := tx.TaskRunIDs(ctx, taskID)
	if err != nil {
		return false, err
	}
	for _, runID := range runIDs {
		executions, err := tx.ListRunExecutionAdmissionRecords(ctx, runID)
		if err != nil {
			return false, err
		}
		for _, admission := range executions {
			if admission.AgentBinding != nil &&
				admission.AgentBinding.SelectionSource == domain.AgentSelectionSourceLineup {
				return true, nil
			}
		}
		reviews, err := tx.ListRunReviewAdmissions(ctx, runID)
		if err != nil {
			return false, err
		}
		for _, admission := range reviews {
			if admission.Role.TaskLineEligible() &&
				admission.AgentBinding.SelectionSource == domain.AgentSelectionSourceLineup {
				return true, nil
			}
		}
	}
	return false, nil
}

// routeCandidates resolves every agent the policy orders. An agent that no
// longer resolves (dropped from the tree, its enrollment or identity gone, a
// disabled identity, or a join the tree no longer satisfies) is absent: the
// router skips it and never cites it. A failed store read fails the routing.
func (s AgentSelection) routeCandidates(
	ctx context.Context, tx *store.ReadTx, policy domain.RoutingPolicy,
) (map[string]routeCandidate, error) {
	candidates := map[string]routeCandidate{}
	for _, role := range policy.Roles {
		for _, name := range role.Agents {
			if _, seen := candidates[name]; seen {
				continue
			}
			candidate, ok, err := s.routeCandidate(ctx, tx, name)
			if err != nil {
				return nil, err
			}
			if ok {
				candidates[name] = candidate
			}
		}
	}
	return candidates, nil
}

func (s AgentSelection) routeCandidate(
	ctx context.Context, tx *store.ReadTx, name string,
) (routeCandidate, bool, error) {
	source, ok := s.Tree.Agent(name)
	if !ok {
		return routeCandidate{}, false, nil
	}
	enrollment, err := tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(source.Enrollment))
	if errors.Is(err, store.ErrNotFound) {
		return routeCandidate{}, false, nil
	}
	if err != nil {
		return routeCandidate{}, false, err
	}
	identity, err := tx.GetAuthIdentity(ctx, enrollment.AuthIdentityID)
	if errors.Is(err, store.ErrNotFound) {
		return routeCandidate{}, false, nil
	}
	if err != nil {
		return routeCandidate{}, false, err
	}
	// The join is pure: an error here is the agent not resolving.
	resolved, err := s.Tree.ResolveAgent(name, enrollment, identity)
	if err != nil {
		return routeCandidate{}, false, nil
	}
	passes, err := domain.DeriveAgentLaunchSelection(resolved.Definition, resolved.Adapter, resolved.Offer)
	if err != nil {
		return routeCandidate{}, false, nil
	}
	return routeCandidate{agent: name, pool: identity.EffectiveUsagePool(), model: passes.RouteModelID}, true, nil
}

// weeklyWindowLength is the length of the windows whose reset ranks a pool
// (plan §5.4, routing policy; revision 107). The router reads the length a
// reading reports, not a provider's window name, so every provider ranks the
// same way; a reading that reports no length never ranks.
const weeklyWindowLength = 7 * 24 * time.Hour

// poolRecords reads each pool's usage record once per routing. A pool with no
// record maps to nil.
type poolRecords map[domain.UsagePoolID]*domain.PoolUsageObservation

func (r poolRecords) get(ctx context.Context, tx *store.ReadTx, pool domain.UsagePoolID) (*domain.PoolUsageObservation, error) {
	if record, ok := r[pool]; ok {
		return record, nil
	}
	record, err := tx.GetPoolUsageObservation(ctx, pool)
	if errors.Is(err, store.ErrNotFound) {
		r[pool] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r[pool] = &record
	return &record, nil
}

// poolMarks returns the exhausted marks the pool's record derives at now for
// the windows that apply to model. A pool with no record has none.
func poolMarks(
	ctx context.Context, tx *store.ReadTx, pool domain.UsagePoolID, model string, now time.Time,
) ([]domain.ExhaustedMark, error) {
	record, err := poolRecords{}.get(ctx, tx, pool)
	if err != nil || record == nil {
		return nil, err
	}
	return record.ExhaustedMarksForModel(model, now), nil
}

// weeklyReading returns the reading that ranks a pool for model: of the
// weekly windows that apply to the model, the one whose reset comes soonest,
// when that reset is known and ahead of now. A marked window still ranks: its
// reset is when the pool has allowance again.
func weeklyReading(
	record *domain.PoolUsageObservation, model string, now time.Time,
) (domain.TaskLineRankingReading, bool) {
	if record == nil {
		return domain.TaskLineRankingReading{}, false
	}
	applies := record.WindowsForModel(model)
	var best *domain.WindowReading
	for i, reading := range record.Windows {
		if reading.Length == nil || *reading.Length != weeklyWindowLength ||
			reading.ResetsAt == nil || !reading.ResetsAt.After(now) || !slices.Contains(applies, reading.Window) {
			continue
		}
		if best == nil || reading.ResetsAt.Before(*best.ResetsAt) {
			best = &record.Windows[i]
		}
	}
	if best == nil {
		return domain.TaskLineRankingReading{}, false
	}
	return domain.TaskLineRankingReading{
		Pool: record.Pool, Window: best.Window, ResetsAt: best.ResetsAt.UTC(), ObservedAt: best.ObservedAt.UTC(),
	}, true
}

// rankedAgent is a candidate of the deciding role in rank order: the tier
// that ranked it and the marks its pool holds for its model at now.
type rankedAgent struct {
	routeCandidate
	tier  domain.TaskLineSelectionTier
	marks []domain.ExhaustedMark
}

// rankRoute ranks the deciding role's resolvable agents in three tiers (plan
// §5.4, routing policy; revision 107):
//
//  1. Agents on the policy's drain-first account, when the order holds one.
//  2. Agents on pools with a known weekly reset ahead (weeklyReading), soonest
//     first; equal resets keep the policy's order.
//  3. The rest, in the policy's order.
//
// Agents of one pool keep the policy's order within it, and the pool's first
// agent in that order names the model whose windows rank it, so a pool holds
// one rank. It returns the ranking and the readings that ranked tier 2, in
// rank order; the drain-first account's own reading ranks nothing.
func rankRoute(
	ctx context.Context, tx *store.ReadTx, policy domain.RoutingPolicy, order []string,
	candidates map[string]routeCandidate, now time.Time,
) ([]rankedAgent, []domain.TaskLineRankingReading, error) {
	records := poolRecords{}
	var (
		agents []rankedAgent
		pools  []domain.UsagePoolID
	)
	for _, name := range order {
		candidate, ok := candidates[name]
		if !ok {
			continue
		}
		record, err := records.get(ctx, tx, candidate.pool)
		if err != nil {
			return nil, nil, err
		}
		agent := rankedAgent{routeCandidate: candidate, tier: domain.TaskLineSelectionPolicyOrder}
		if record != nil {
			agent.marks = record.ExhaustedMarksForModel(candidate.model, now)
		}
		agents = append(agents, agent)
		if !slices.Contains(pools, candidate.pool) {
			pools = append(pools, candidate.pool)
		}
	}
	readings := []domain.TaskLineRankingReading{}
	for _, pool := range pools {
		if policy.DrainFirstAccount != nil && pool == *policy.DrainFirstAccount {
			continue
		}
		first := agents[slices.IndexFunc(agents, func(a rankedAgent) bool { return a.pool == pool })]
		if reading, ok := weeklyReading(records[pool], first.model, now); ok {
			readings = append(readings, reading)
		}
	}
	slices.SortStableFunc(readings, func(a, b domain.TaskLineRankingReading) int {
		return a.ResetsAt.Compare(b.ResetsAt)
	})
	rank := func(agent rankedAgent) int {
		if policy.DrainFirstAccount != nil && agent.pool == *policy.DrainFirstAccount {
			return -1
		}
		if i := slices.IndexFunc(readings, func(r domain.TaskLineRankingReading) bool { return r.Pool == agent.pool }); i >= 0 {
			return i
		}
		return len(readings)
	}
	for i := range agents {
		switch rank(agents[i]) {
		case -1:
			agents[i].tier = domain.TaskLineSelectionDrainFirstAccount
		case len(readings):
		default:
			agents[i].tier = domain.TaskLineSelectionSoonestReset
		}
	}
	slices.SortStableFunc(agents, func(a, b rankedAgent) int { return rank(a) - rank(b) })
	return agents, readings, nil
}

// routeChoice is the agent the ranking picked and the provenance it records.
type routeChoice struct {
	agent      routeCandidate
	provenance domain.TaskLineProvenance
}

// pickRoute takes the first ranked agent whose pool holds no mark for its
// model (rankRoute). It returns nil with no resolvable agent, and a
// QuotaWindowExhaustedError when every agent is marked and some mark gives a
// reset. When none gives one, nothing could resume a hold, so the first ranked
// agent takes the task and the provider decides.
func pickRoute(
	ctx context.Context, tx *store.ReadTx, policy domain.RoutingPolicy, order []string,
	candidates map[string]routeCandidate, now time.Time,
) (*routeChoice, error) {
	agents, readings, err := rankRoute(ctx, tx, policy, order, candidates, now)
	if err != nil || len(agents) == 0 {
		return nil, err
	}
	choice := func(agent rankedAgent, ahead []rankedAgent) *routeChoice {
		c := &routeChoice{agent: agent.routeCandidate, provenance: domain.TaskLineProvenance{
			PolicyRevision: policy.Revision, Tier: agent.tier, Pool: agent.pool, RankedBy: readings,
		}}
		// The drain-first account leads every pool, so its provenance passes
		// over nothing, even past a marked agent on the account itself.
		if len(ahead) > 0 && agent.tier != domain.TaskLineSelectionDrainFirstAccount {
			first, mark := ahead[0], ahead[0].marks[0]
			c.provenance.PassedOver = &domain.TaskLinePassedOverMark{
				Agent: first.agent, Pool: first.pool, Window: mark.Window, Source: mark.Source,
				MarkedAt: mark.SetAt.UTC(), ResetsAt: utcPtr(mark.ResetsAt),
			}
		}
		return c
	}
	// hold is the earliest time a marked agent's windows all reset.
	var hold *domain.QuotaWindowHold
	for i, agent := range agents {
		if len(agent.marks) == 0 {
			return choice(agent, agents[:i]), nil
		}
		if free, ok := heldUntil(agent.marks); ok && (hold == nil || free.ResetsAt.Before(*hold.ResetsAt)) {
			hold = &domain.QuotaWindowHold{Pool: agent.pool, ExhaustedMark: free}
		}
	}
	if hold != nil {
		return nil, &QuotaWindowExhaustedError{Hold: *hold}
	}
	return choice(agents[0], nil), nil
}

func utcPtr(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	return new(at.UTC())
}

// heldUntil returns the mark whose reset frees the last of the windows that
// hold, which is when the agent's pool has allowance again. A mark with no
// reset doesn't hold, matching the park's resume gate: the provider decides
// then. It reports false when no mark holds. The router and the later-role
// hold share it, so a role the router pins is never held on a basis the
// router ignored.
func heldUntil(marks []domain.ExhaustedMark) (domain.ExhaustedMark, bool) {
	marks = slices.DeleteFunc(slices.Clone(marks), func(mark domain.ExhaustedMark) bool { return mark.ResetsAt == nil })
	if len(marks) == 0 {
		return domain.ExhaustedMark{}, false
	}
	return slices.MaxFunc(marks, compareResets), true
}

// compareResets orders marks that all carry a reset by that reset.
func compareResets(a, b domain.ExhaustedMark) int { return a.ResetsAt.Compare(*b.ResetsAt) }

// routerLinesForPool derives the lines that pin a task's roles to a pool:
// for each role the policy orders that skip doesn't hold, the first agent in
// its order whose pool (by pools, which holds only resolvable agents) is
// pool. A role whose order holds no agent on the pool gets none and keeps the
// lineup. An agent the router passed over on this pool for its model's marked
// window still takes its role's line, which holds on that window like any
// later role: the task stays on one pool. A parked task's move reuses this
// to re-pin the task to the pool it moves to.
func routerLinesForPool(
	policy domain.RoutingPolicy, pool domain.UsagePoolID, pools map[string]domain.UsagePoolID,
	skip map[domain.RoleName]bool,
) []domain.TaskLineChoice {
	var lines []domain.TaskLineChoice
	for _, role := range policy.Roles {
		if skip[role.Role] {
			continue
		}
		i := slices.IndexFunc(role.Agents, func(name string) bool {
			agentPool, ok := pools[name]
			return ok && agentPool == pool
		})
		if i >= 0 {
			lines = append(lines, domain.TaskLineChoice{Role: role.Role, Agent: role.Agents[i]})
		}
	}
	return lines
}

// requireRoutedPoolOpen holds a role whose current line the router or a
// parked task's move set while that line's pool still marks a window for the
// line's model with a reset ahead (linePool). The hold names the mark that resets last (heldUntil). The router
// never reroutes the role.
//
// A line that fails its anchor, or an agent that no longer resolves, passes
// here: admission refuses it with its typed refusal.
func (s AgentSelection) requireRoutedPoolOpen(
	ctx context.Context, st *store.Store, runID domain.RunID, role domain.RoleName, now time.Time,
) error {
	if runID == "" || !role.TaskLineEligible() {
		return nil
	}
	return st.Read(ctx, func(tx *store.ReadTx) error {
		line, found, err := RunTaskLine(ctx, tx, s.Tree, runID, role)
		if err != nil || !found || !line.Source.CarriesProvenance() {
			// An unreadable or unanchored line is admission's to refuse.
			return nil
		}
		pool, model, ok, err := s.linePool(ctx, tx, line)
		if err != nil || !ok {
			return err
		}
		marks, err := poolMarks(ctx, tx, pool, model, now)
		if err != nil {
			return err
		}
		mark, held := heldUntil(marks)
		if !held {
			return nil
		}
		return fmt.Errorf("task line %s: %w", line.ID,
			&QuotaWindowExhaustedError{Hold: domain.QuotaWindowHold{Pool: pool, ExhaustedMark: mark}})
	})
}

// linePool is the pool a line's credential draws on and the model it runs,
// or false when what it selects no longer resolves (its enrollment or
// identity gone, or the identity disabled), which admission refuses. A v2
// line's enrollment names both, whether or not a tree agent maps to it; a v1
// line's agent resolves through the tree (routeCandidate).
func (s AgentSelection) linePool(
	ctx context.Context, tx *store.ReadTx, line domain.TaskLine,
) (domain.UsagePoolID, string, bool, error) {
	if line.Selection == nil {
		agent, err := s.Tree.LineAgent(line)
		if err != nil {
			return "", "", false, nil
		}
		candidate, ok, err := s.routeCandidate(ctx, tx, agent)
		return candidate.pool, candidate.model, ok, err
	}
	enrollment, err := tx.GetClientEnrollment(ctx, line.Selection.EnrollmentID)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	identity, err := tx.GetAuthIdentity(ctx, enrollment.AuthIdentityID)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", false, nil
	}
	if err != nil || !identity.Enabled {
		return "", "", false, err
	}
	return identity.EffectiveUsagePool(), line.Selection.ModelID, true, nil
}
