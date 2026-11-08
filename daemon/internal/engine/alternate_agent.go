package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The alternate-agent retry (plan §4, §5.4 admission step 5; issue #869) lets
// an operator run a role under another agent after a failure the agent's
// quota, credential, or capacity caused. The daemon offers only agents that
// escape what failed (domain.QualifyAlternate) and would pass admission now;
// the operator picks one, a record stores the pick, and admission re-derives
// the rule against the failed admission before the agent runs. Nothing here
// picks an agent: an offer is a choice for the operator, never a fallback.

// alternateAgentFailure is the failure an offer answers: the task and role
// whose agent failed, the classified cause, and the invocation whose
// admission names the failed agent.
type alternateAgentFailure struct {
	taskID       domain.TaskID
	role         domain.RoleName
	cause        domain.FailureCause
	invocationID domain.InvocationID
	// policyOnly limits the candidates to the routing policy's order for the
	// role. A parked task's move writes router-shaped lines, whose
	// provenance cites a policy revision, so only an agent a policy orders
	// can be moved to.
	policyOnly bool
}

// failedAlternateFacts is what the failed invocation ran on: the tree agents
// that ran it (ranFailedBinding; none when the tree no longer carries one),
// the enrollment generation its credential came from, and the route and pool
// QualifyAlternate compares an alternate's to.
type failedAlternateFacts struct {
	names      []string
	enrollment domain.ClientEnrollmentID
	generation int
	facts      domain.FailureFacts
}

// ran reports whether the tree agent name is one that ran the failed
// invocation.
func (f failedAlternateFacts) ran(name string) bool {
	return slices.Contains(f.names, name)
}

// freshCredential reports whether a credential is another than the one
// that failed: another enrollment, or a later generation of the same one. A
// second agent on the failed generation would run the expired or revoked
// credential again, so it does not escape a credential failure.
func (f failedAlternateFacts) freshCredential(
	enrollment domain.ClientEnrollmentID, generation domain.EnrollmentGeneration,
) bool {
	return enrollment != f.enrollment || generation.Ordinal != f.generation
}

// ranFailedBinding reports whether the tree agent source ran what binding
// records. A binding admitted under an agent names its definition digest, so
// the agents at that digest ran it; one admitted under a selection names no
// agent, so every agent that reduces to the selection's enrollment, model,
// and effort ran it, since each would run the same credential and model.
// Twins (one definition under several names) all count either way.
func ranFailedBinding(tree agenttree.Tree, source domain.AgentSource, binding *domain.AdmissionAgentBinding) bool {
	if !binding.SelectionAdmitted() {
		digest, err := tree.AgentDigest(source.Name)
		return err == nil && digest == binding.AgentDigest
	}
	selection, err := tree.SelectionOfAgent(source.Name)
	return err == nil && selection == (domain.TaskLineSelection{
		EnrollmentID: binding.EnrollmentID, ModelID: binding.SelectionModelID, Effort: binding.SelectionEffort,
	})
}

// treeAgent is a tree agent's closure under its enrollment's records, which
// the offers read until the alternate-agent record names a selection
// (#2735). Admission does not resolve agents; only the offers do.
type treeAgent struct {
	resolved   agenttree.ResolvedAgent
	enrollment domain.ClientEnrollment
	identity   domain.AuthIdentity
}

// resolveTreeAgent resolves the tree agent name and its fragments against
// its enrollment and identity; it refuses a disabled identity.
func resolveTreeAgent(ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, name string) (treeAgent, error) {
	source, ok := tree.Agent(name)
	if !ok {
		return treeAgent{}, fmt.Errorf("agent %q: %w", name, agenttree.ErrUnknownAgent)
	}
	var (
		agent treeAgent
		err   error
	)
	if agent.enrollment, err = tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(source.Enrollment)); err != nil {
		return treeAgent{}, fmt.Errorf("enrollment %s: %w", source.Enrollment, err)
	}
	if agent.identity, err = tx.GetAuthIdentity(ctx, agent.enrollment.AuthIdentityID); err != nil {
		return treeAgent{}, fmt.Errorf("auth identity %s: %w", agent.enrollment.AuthIdentityID, err)
	}
	if agent.resolved, err = tree.ResolveAgent(name, agent.enrollment, agent.identity); err != nil {
		return treeAgent{}, err
	}
	return agent, nil
}

// readFailedAlternateFacts reads the failed invocation's admission. found is
// false when it has no agent binding to compare against (an admission made
// before agent selection), which offers nothing. The pool is the one its
// admitted identity meters; the route is the one the tree resolves the
// admitted agent to now, empty when it no longer resolves, which qualifies
// no capacity alternate.
func readFailedAlternateFacts(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, invocationID domain.InvocationID,
) (failedAlternateFacts, bool, error) {
	admission, err := tx.GetExecutionAdmissionRecord(ctx, invocationID)
	if errors.Is(err, store.ErrNotFound) {
		return failedAlternateFacts{}, false, nil
	}
	if err != nil {
		return failedAlternateFacts{}, false, err
	}
	if admission.AgentBinding == nil {
		return failedAlternateFacts{}, false, nil
	}
	binding := admission.AgentBinding
	failed := failedAlternateFacts{enrollment: binding.EnrollmentID, generation: binding.EnrollmentGeneration}
	pool, found, err := quotaRefusalPool(ctx, tx, invocationID)
	if err != nil {
		return failedAlternateFacts{}, false, err
	}
	if found {
		failed.facts.UsagePool = string(pool)
	}
	for _, source := range tree.Agents {
		if !ranFailedBinding(tree, source, binding) {
			continue
		}
		failed.names = append(failed.names, source.Name)
		if failed.facts.Route != "" {
			continue
		}
		// Every agent that ran the binding runs its enrollment, and an
		// agent's route is its enrollment's, so the first that resolves
		// names the route.
		if agent, err := resolveTreeAgent(ctx, tx, tree, source.Name); err == nil {
			failed.facts.Route = string(agent.resolved.Route.Digest)
		}
	}
	return failed, true, nil
}

// alternateAgentOffers returns the agents the operator may retry the role
// under, sorted by name, or nil when none qualifies. A candidate is an agent
// the routing policy in effect orders for the role, or the role's lineup
// agent unless policyOnly, other than the failed agent. It is offered only
// when it passes the admission steps that need no launch at now (it
// resolves, its generation is unmarked and valid through the attempt's
// deadline and margin, its offer covers that deadline, and its route admits
// the daemon's egress), its pool marks no window for its model with a reset
// ahead, and it escapes the failure's cause.
//
// The offers are a snapshot for the operator. Acceptance and admission
// re-derive the rule rather than trust them.
func (s AgentSelection) alternateAgentOffers(
	ctx context.Context, tx *store.ReadTx, failure alternateAgentFailure, now time.Time,
) ([]domain.AlternateAgentOffer, error) {
	failed, found, err := readFailedAlternateFacts(ctx, tx, s.Tree, failure.invocationID)
	if err != nil || !found {
		return nil, err
	}
	candidates, err := s.alternateCandidates(ctx, tx, failure, now)
	if err != nil {
		return nil, err
	}
	var offers []domain.AlternateAgentOffer
	for _, name := range candidates {
		offer, ok, err := s.alternateOffer(ctx, tx, name, failure.cause, failed, now)
		if err != nil {
			return nil, err
		}
		if ok {
			offers = append(offers, offer)
		}
	}
	domain.SortAlternateAgentOffers(offers)
	return offers, nil
}

// alternateCandidates lists the agent names a failure's offers draw from, in
// first-seen order with no name twice.
func (s AgentSelection) alternateCandidates(
	ctx context.Context, tx *store.ReadTx, failure alternateAgentFailure, now time.Time,
) ([]string, error) {
	var names []string
	policy, found, err := policyInEffect(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	if found {
		names = append(names, routingOrder(policy, failure.role)...)
	}
	if !failure.policyOnly {
		// A lineup line names a selection; the agents that reduce to it
		// for the role are candidates under their tree names.
		if line, err := lineupLine(s.Tree, failure.role); err == nil {
			if name, err := s.Tree.AgentForSelection(failure.role, line.TaskLineSelection()); err == nil {
				names = append(names, name)
			}
		}
	}
	seen := map[string]bool{}
	return slices.DeleteFunc(names, func(name string) bool {
		defer func() { seen[name] = true }()
		return seen[name]
	}), nil
}

// alternateOffer resolves one candidate and reports its offer, or false when
// it is the failed agent, would not pass admission now, is held on its pool,
// or escapes nothing the cause names.
func (s AgentSelection) alternateOffer(
	ctx context.Context, tx *store.ReadTx, name string, cause domain.FailureCause,
	failed failedAlternateFacts, now time.Time,
) (domain.AlternateAgentOffer, bool, error) {
	if failed.ran(name) {
		return domain.AlternateAgentOffer{}, false, nil
	}
	agent, err := resolveTreeAgent(ctx, tx, s.Tree, name)
	if err != nil {
		// An agent that no longer resolves is not one admission would run.
		return domain.AlternateAgentOffer{}, false, nil
	}
	generation, err := tx.CurrentEnrollmentGeneration(ctx, agent.enrollment.ID)
	if err != nil {
		return domain.AlternateAgentOffer{}, false, nil
	}
	if err := tx.RequireGenerationUnmarked(
		ctx, agent.enrollment.ID, generation.Ordinal,
	); errors.Is(err, domain.ErrGenerationIntegrityMarked) {
		return domain.AlternateAgentOffer{}, false, nil
	} else if err != nil {
		return domain.AlternateAgentOffer{}, false, err
	}
	deadline := now.Add(s.AttemptBudget)
	if domain.ValidateGenerationExpiryMargin(agent.enrollment, generation, deadline, s.ExpiryMargin) != nil ||
		domain.ValidateOfferCoversDeadline(agent.resolved.Offer, deadline) != nil {
		return domain.AlternateAgentOffer{}, false, nil
	}
	for _, authority := range s.EffectiveEgress {
		if !slices.Contains(agent.resolved.Route.InferenceAuthorities, authority) {
			return domain.AlternateAgentOffer{}, false, nil
		}
	}
	passes, err := domain.DeriveAgentLaunchSelection(agent.resolved.Definition, agent.resolved.Adapter, agent.resolved.Offer)
	if err != nil {
		return domain.AlternateAgentOffer{}, false, nil
	}
	pool := agent.identity.EffectiveUsagePool()
	marks, err := poolMarks(ctx, tx, pool, passes.RouteModelID, now)
	if err != nil {
		return domain.AlternateAgentOffer{}, false, err
	}
	if _, held := heldUntil(marks); held {
		return domain.AlternateAgentOffer{}, false, nil
	}
	qualification, ok := domain.QualifyAlternate(cause, failed.facts, domain.FailureAlternate{
		Agent: agent.resolved.Definition.Digest, Route: string(agent.resolved.Route.Digest),
		UsagePool: string(pool), GenerationValid: failed.freshCredential(agent.enrollment.ID, generation),
	})
	if !ok {
		return domain.AlternateAgentOffer{}, false, nil
	}
	offer := domain.AlternateAgentOffer{
		Agent: name, Pool: pool, Harness: agent.resolved.Adapter.ClientKind,
		Model: passes.RouteModelID, Qualification: qualification,
	}
	if err := offer.Validate(); err != nil {
		return domain.AlternateAgentOffer{}, false, fmt.Errorf("alternate agent %s: %w", name, err)
	}
	return offer, true, nil
}

// requalifyAlternate re-derives, against current state, the rule a record
// names: the record's agent must still be offered for the failure it
// answers, under the same pool and rule. Acceptance runs it before writing a
// record, and admission before running the agent, so neither trusts an offer
// made earlier.
func (s AgentSelection) requalifyAlternate(
	ctx context.Context, tx *store.ReadTx, failure alternateAgentFailure, agent string,
	pool domain.UsagePoolID, qualification domain.AlternateAgentQualification, now time.Time,
) error {
	offers, err := s.alternateAgentOffers(ctx, tx, failure, now)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(offers, func(offer domain.AlternateAgentOffer) bool {
		return offer.Agent == agent && offer.Pool == pool && offer.Qualification == qualification
	}) {
		return signet.ErrAlternateAgentNotQualified
	}
	return nil
}

// attemptCard returns the alternate-agent record the run's production attempt
// cites for role, or false when the run has no attempt, its attempt cites no
// record, or the record chose another role's agent. The store authenticates
// the attempt's binding to the record, its command, and its card on read
// (GetProductionAttemptByRun), so the record returned is the one the operator
// accepted for this attempt.
func attemptCard(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID, role domain.RoleName,
) (domain.AlternateAgentRetry, bool, error) {
	attempt, err := tx.GetProductionAttemptByRun(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.AlternateAgentRetry{}, false, nil
	}
	if err != nil {
		return domain.AlternateAgentRetry{}, false, err
	}
	if attempt.AlternateAgentRetryID == nil {
		return domain.AlternateAgentRetry{}, false, nil
	}
	record, err := tx.GetAlternateAgentRetry(ctx, *attempt.AlternateAgentRetryID)
	if err != nil {
		return domain.AlternateAgentRetry{}, false, err
	}
	if record.Role != role {
		return domain.AlternateAgentRetry{}, false, nil
	}
	return record, true, nil
}

// regateCardAgent re-derives a card's rule for the agent the record names,
// as admission resolved it (plan §5.4 admission step 5): it is not an agent
// that ran the failed invocation (failedAlternateFacts.ran), it draws on
// the pool the operator chose, and against the failed invocation's recorded
// cause and facts it still escapes what failed under the rule the record
// names. agent's generation must already be read: it counts as valid here
// when it is another credential than the failed one (freshCredential), and
// whether it is unexpired and unmarked is admission step 4's to prove. The
// refusals name no agent: the record's is the tree's, but
// the failed agent's facts are not printed.
func regateCardAgent(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, record domain.AlternateAgentRetry, agent RoleAgent,
) error {
	failed, found, err := readFailedAlternateFacts(ctx, tx, tree, record.FailedInvocationID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("its failed invocation was admitted with no agent to compare")
	}
	outcome, err := tx.GetExecutionOutcomeRecord(ctx, record.FailedInvocationID)
	if err != nil {
		return fmt.Errorf("its failed invocation's outcome: %w", err)
	}
	if failed.ran(record.Agent) {
		return errors.New("it chose the agent that failed")
	}
	digest, err := tree.AgentDigest(record.Agent)
	if err != nil {
		return err
	}
	pool := agent.Identity.EffectiveUsagePool()
	if pool != record.Pool {
		return errors.New("the agent draws on another pool than the one chosen")
	}
	qualification, ok := domain.QualifyAlternate(outcome.Cause, failed.facts, domain.FailureAlternate{
		Agent: digest, Route: string(agent.Route.Digest),
		UsagePool: string(pool), GenerationValid: failed.freshCredential(agent.Enrollment.ID, agent.Generation),
	})
	if !ok || qualification != record.Qualification {
		return fmt.Errorf("the agent no longer escapes the failure under %s", record.Qualification)
	}
	return nil
}
