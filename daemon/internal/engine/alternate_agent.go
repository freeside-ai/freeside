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

// failedAlternateFacts is what the failed invocation ran on: the agent's
// digest, its tree name (empty when the tree no longer carries that digest),
// the enrollment generation its credential came from, and the route and pool
// QualifyAlternate compares an alternate's to.
type failedAlternateFacts struct {
	agent      domain.Digest
	name       string
	enrollment domain.ClientEnrollmentID
	generation int
	facts      domain.FailureFacts
}

// freshCredential reports whether agent's credential is another than the one
// that failed: another enrollment, or a later generation of the same one. A
// second agent on the failed generation would run the expired or revoked
// credential again, so it does not escape a credential failure.
func (f failedAlternateFacts) freshCredential(agent RoleAgent) bool {
	return agent.Enrollment.ID != f.enrollment || agent.Generation.Ordinal != f.generation
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
	failed := failedAlternateFacts{
		agent: binding.AgentDigest, enrollment: binding.EnrollmentID, generation: binding.EnrollmentGeneration,
	}
	pool, found, err := quotaRefusalPool(ctx, tx, invocationID)
	if err != nil {
		return failedAlternateFacts{}, false, err
	}
	if found {
		failed.facts.UsagePool = string(pool)
	}
	for _, source := range tree.Agents {
		digest, err := tree.AgentDigest(source.Name)
		if err != nil || digest != failed.agent {
			continue
		}
		failed.name = source.Name
		agent, err := resolveAgent(ctx, tx, tree, source.Name)
		if err == nil {
			failed.facts.Route = string(agent.Resolved.Route.Digest)
		}
		break
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
		if line, err := lineupLine(s.Tree, failure.role); err == nil {
			names = append(names, line.AgentName)
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
	agent, err := resolveAgent(ctx, tx, s.Tree, name)
	if err != nil || agent.Resolved.Definition.Digest == failed.agent {
		// An agent that no longer resolves is not one admission would run.
		return domain.AlternateAgentOffer{}, false, nil
	}
	if err := agent.readGeneration(ctx, tx); err != nil {
		return domain.AlternateAgentOffer{}, false, nil
	}
	if err := agent.requireUnmarked(ctx, tx); errors.Is(err, domain.ErrGenerationIntegrityMarked) {
		return domain.AlternateAgentOffer{}, false, nil
	} else if err != nil {
		return domain.AlternateAgentOffer{}, false, err
	}
	deadline := now.Add(s.AttemptBudget)
	if domain.ValidateGenerationExpiryMargin(agent.Enrollment, agent.Generation, deadline, s.ExpiryMargin) != nil ||
		domain.ValidateOfferCoversDeadline(agent.Resolved.Offer, deadline) != nil {
		return domain.AlternateAgentOffer{}, false, nil
	}
	for _, authority := range s.EffectiveEgress {
		if !slices.Contains(agent.Resolved.Route.InferenceAuthorities, authority) {
			return domain.AlternateAgentOffer{}, false, nil
		}
	}
	passes, err := domain.DeriveAgentLaunchSelection(agent.Resolved.Definition, agent.Resolved.Adapter, agent.Resolved.Offer)
	if err != nil {
		return domain.AlternateAgentOffer{}, false, nil
	}
	pool := agent.Identity.EffectiveUsagePool()
	marks, err := poolMarks(ctx, tx, pool, passes.RouteModelID, now)
	if err != nil {
		return domain.AlternateAgentOffer{}, false, err
	}
	if _, held := heldUntil(marks); held {
		return domain.AlternateAgentOffer{}, false, nil
	}
	qualification, ok := domain.QualifyAlternate(cause, failed.facts, domain.FailureAlternate{
		Agent: agent.Resolved.Definition.Digest, Route: string(agent.Resolved.Route.Digest),
		UsagePool: string(pool), GenerationValid: failed.freshCredential(agent),
	})
	if !ok {
		return domain.AlternateAgentOffer{}, false, nil
	}
	offer := domain.AlternateAgentOffer{
		Agent: name, Pool: pool, Harness: agent.Resolved.Adapter.ClientKind,
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

// regateCardAgent re-derives a card's rule for the agent admission resolved
// (plan §5.4 admission step 5): it is not the agent that failed, it draws on
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
	if agent.Resolved.Definition.Digest == failed.agent {
		return errors.New("it chose the agent that failed")
	}
	pool := agent.Identity.EffectiveUsagePool()
	if pool != record.Pool {
		return errors.New("the agent draws on another pool than the one chosen")
	}
	qualification, ok := domain.QualifyAlternate(outcome.Cause, failed.facts, domain.FailureAlternate{
		Agent: agent.Resolved.Definition.Digest, Route: string(agent.Resolved.Route.Digest),
		UsagePool: string(pool), GenerationValid: failed.freshCredential(agent),
	})
	if !ok || qualification != record.Qualification {
		return fmt.Errorf("the agent no longer escapes the failure under %s", record.Qualification)
	}
	return nil
}
