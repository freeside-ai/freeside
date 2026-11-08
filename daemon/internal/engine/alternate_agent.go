package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The alternate-agent retry (plan §4, §5.4 admission step 5; issues #869
// and #2735) lets an operator run a role under another enrollment, model, and
// effort after a failure the agent's quota, credential, or capacity caused.
// The daemon offers only enrollments that escape what failed
// (domain.QualifyAlternate) and would pass admission now under the role's
// lineup model and effort; the operator picks one, a record stores the
// selection, and admission re-derives the rule against the failed admission
// before it runs. Nothing here picks an enrollment: an offer is a choice for
// the operator, never a fallback.

// alternateAgentFailure is the failure an offer answers: the task and role
// whose agent failed, the classified cause, and the invocation whose
// admission names the failed enrollment.
type alternateAgentFailure struct {
	taskID       domain.TaskID
	role         domain.RoleName
	cause        domain.FailureCause
	invocationID domain.InvocationID
	// policyOnly limits the candidates to the enrollments the routing
	// policy in effect orders for the role (roleOrder). A parked
	// task's move writes router-shaped lines, whose provenance cites a
	// policy revision, so only an enrollment a policy orders can be moved to.
	policyOnly bool
}

// failedAlternateFacts is what the failed invocation ran on: the enrollment
// and generation its credential came from, and the route and pool
// QualifyAlternate compares an alternate's to.
type failedAlternateFacts struct {
	enrollment domain.ClientEnrollmentID
	generation int
	facts      domain.FailureFacts
}

// freshCredential reports whether a credential is another than the one
// that failed: another enrollment, or a later generation of the same one. A
// second run on the failed generation would run the expired or revoked
// credential again, so it does not escape a credential failure.
func (f failedAlternateFacts) freshCredential(
	enrollment domain.ClientEnrollmentID, generation domain.EnrollmentGeneration,
) bool {
	return enrollment != f.enrollment || generation.Ordinal != f.generation
}

// readFailedAlternateFacts reads the failed invocation's admission. found is
// false when it has no agent binding to compare against (an admission made
// before agent selection), which offers nothing. The pool is the one its
// admitted identity meters; the route is its enrollment's route id, empty
// when the enrollment is gone, which qualifies no capacity alternate.
func readFailedAlternateFacts(
	ctx context.Context, tx *store.ReadTx, invocationID domain.InvocationID,
) (failedAlternateFacts, bool, error) {
	binding, found, err := failedBinding(ctx, tx, invocationID)
	if err != nil || !found {
		return failedAlternateFacts{}, false, err
	}
	failed := failedAlternateFacts{enrollment: binding.EnrollmentID, generation: binding.EnrollmentGeneration}
	pool, found, err := quotaRefusalPool(ctx, tx, invocationID)
	if err != nil {
		return failedAlternateFacts{}, false, err
	}
	if found {
		failed.facts.UsagePool = string(pool)
	}
	enrollment, err := tx.GetClientEnrollment(ctx, binding.EnrollmentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return failedAlternateFacts{}, false, err
	default:
		failed.facts.Route = enrollment.Route
	}
	return failed, true, nil
}

// failedBinding is the failed invocation's agent binding, or false when its
// admission is missing or binds no agent.
func failedBinding(
	ctx context.Context, tx *store.ReadTx, invocationID domain.InvocationID,
) (*domain.AdmissionAgentBinding, bool, error) {
	admission, err := tx.GetExecutionAdmissionRecord(ctx, invocationID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return admission.AgentBinding, admission.AgentBinding != nil, nil
}

// lineupServes reports whether an enrollment serves line's model and effort
// (domain.SelectionServed) given its catalog. With no recorded catalog the
// bridge would take any model as named, so such an enrollment serves the
// line only when it binds the client the line's own enrollment does,
// lineupClient: the bridge never puts one harness's model on another.
func lineupServes(
	enrollment domain.ClientEnrollment, catalog *domain.DiscoveredModelCatalog, line domain.LineupSelection,
	lineupClient domain.HarnessClientKind,
) bool {
	if catalog == nil && enrollment.HarnessClient != lineupClient {
		return false
	}
	return domain.SelectionServed(enrollment.HarnessClient, catalog, line.Names.Model, line.Names.Effort) == nil
}

// lineupClient is the client the line's own enrollment binds, empty when the
// store no longer holds that enrollment.
func lineupClient(ctx context.Context, tx *store.ReadTx, line domain.LineupSelection) (domain.HarnessClientKind, error) {
	enrollment, err := tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(line.Names.Enrollment))
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return enrollment.HarnessClient, nil
}

// alternateAgentOffers returns the enrollments the operator may retry the
// role on, sorted by enrollment id, or nil when none qualifies or the role
// has no lineup line. A candidate is every enrollment of an enabled identity
// the store holds, or under policyOnly every one the policy in effect orders
// for the role. It is offered only when it serves the role's lineup model
// and effort (lineupServes) and passes the admission steps that need no
// launch at now (its generation is unmarked and valid through the attempt's
// deadline and margin, and its route admits the daemon's egress), its pool
// marks no window for that model with a reset ahead, and it escapes the
// failure's cause. The failed enrollment is offered only for a credential
// failure, and then only with a later generation.
//
// The offers are a snapshot for the operator. Acceptance and admission
// re-derive the rule rather than trust them.
func (s AgentSelection) alternateAgentOffers(
	ctx context.Context, tx *store.ReadTx, failure alternateAgentFailure, now time.Time,
) ([]domain.AlternateAgentOffer, error) {
	failed, found, err := readFailedAlternateFacts(ctx, tx, failure.invocationID)
	if err != nil || !found {
		return nil, err
	}
	line, err := lineupLine(s.Tree, failure.role)
	if err != nil {
		return nil, nil
	}
	client, err := lineupClient(ctx, tx, line)
	if err != nil {
		return nil, err
	}
	candidates, err := s.alternateCandidates(ctx, tx, failure, now)
	if err != nil {
		return nil, err
	}
	var offers []domain.AlternateAgentOffer
	for _, candidate := range candidates {
		offer, ok, err := s.alternateOffer(ctx, tx, candidate, line, client, failure.cause, failed, now)
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

// alternateCandidate is an enrollment with the enabled identity it is bound
// to.
type alternateCandidate struct {
	enrollment domain.ClientEnrollment
	identity   domain.AuthIdentity
}

// alternateCandidates lists the enrollments a failure's offers draw from:
// every enrollment of an enabled identity, limited under policyOnly to those
// the policy in effect orders for the role, and none when no policy is in
// effect then.
func (s AgentSelection) alternateCandidates(
	ctx context.Context, tx *store.ReadTx, failure alternateAgentFailure, now time.Time,
) ([]alternateCandidate, error) {
	var ordered []domain.ClientEnrollmentID
	if failure.policyOnly {
		policy, found, err := policyInEffect(ctx, tx, now)
		if err != nil || !found {
			return nil, err
		}
		ordered = roleOrder(s.Tree, policy, failure.role)
	}
	identities, err := tx.ListAuthIdentities(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []alternateCandidate
	for _, identity := range identities {
		if !identity.Enabled {
			continue
		}
		enrollments, err := tx.ListClientEnrollments(ctx, identity.ID)
		if err != nil {
			return nil, err
		}
		for _, enrollment := range enrollments {
			if !failure.policyOnly || slices.Contains(ordered, enrollment.ID) {
				candidates = append(candidates, alternateCandidate{enrollment: enrollment, identity: identity})
			}
		}
	}
	return candidates, nil
}

// alternateOffer reports the candidate's offer, or false when it does not
// serve the lineup line, would not pass admission now, is held on its pool,
// or escapes nothing the cause names.
func (s AgentSelection) alternateOffer(
	ctx context.Context, tx *store.ReadTx, candidate alternateCandidate, line domain.LineupSelection,
	client domain.HarnessClientKind, cause domain.FailureCause, failed failedAlternateFacts, now time.Time,
) (domain.AlternateAgentOffer, bool, error) {
	enrollment, identity := candidate.enrollment, candidate.identity
	if domain.ValidateEnrollmentIdentityBinding(identity, enrollment) != nil {
		return domain.AlternateAgentOffer{}, false, nil
	}
	catalog, err := readCatalog(ctx, tx, enrollment.ID)
	if err != nil {
		return domain.AlternateAgentOffer{}, false, err
	}
	if !lineupServes(enrollment, catalog, line, client) {
		return domain.AlternateAgentOffer{}, false, nil
	}
	generation, err := tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
	if err != nil {
		return domain.AlternateAgentOffer{}, false, nil
	}
	if err := tx.RequireGenerationUnmarked(
		ctx, enrollment.ID, generation.Ordinal,
	); errors.Is(err, domain.ErrGenerationIntegrityMarked) {
		return domain.AlternateAgentOffer{}, false, nil
	} else if err != nil {
		return domain.AlternateAgentOffer{}, false, err
	}
	if domain.ValidateGenerationExpiryMargin(enrollment, generation, now.Add(s.AttemptBudget), s.ExpiryMargin) != nil {
		return domain.AlternateAgentOffer{}, false, nil
	}
	route, ok := s.Tree.Route(enrollment.Route)
	if !ok {
		return domain.AlternateAgentOffer{}, false, nil
	}
	for _, authority := range s.EffectiveEgress {
		if !slices.Contains(route.InferenceAuthorities, authority) {
			return domain.AlternateAgentOffer{}, false, nil
		}
	}
	pool := identity.EffectiveUsagePool()
	marks, err := poolMarks(ctx, tx, pool, line.Names.Model, now)
	if err != nil {
		return domain.AlternateAgentOffer{}, false, err
	}
	if _, held := heldUntil(marks); held {
		return domain.AlternateAgentOffer{}, false, nil
	}
	qualification, ok := domain.QualifyAlternate(cause, failed.facts, domain.FailureAlternate{
		Route: enrollment.Route, UsagePool: string(pool),
		GenerationValid: failed.freshCredential(enrollment.ID, generation),
	})
	if !ok {
		return domain.AlternateAgentOffer{}, false, nil
	}
	offer := domain.AlternateAgentOffer{
		Enrollment: enrollment.ID, Pool: pool, Harness: enrollment.HarnessClient,
		Model: line.Names.Model, Qualification: qualification,
	}
	if err := offer.Validate(); err != nil {
		return domain.AlternateAgentOffer{}, false, fmt.Errorf("alternate enrollment %s: %w", enrollment.ID, err)
	}
	return offer, true, nil
}

// requalifyAlternate re-derives, against current state, the rule a choice
// names: the enrollment must still be offered for the failure it answers,
// under the same pool and rule. Acceptance runs it before writing a record,
// so it never trusts an offer made earlier; admission re-derives the rule
// again (regateCardAgent).
func (s AgentSelection) requalifyAlternate(
	ctx context.Context, tx *store.ReadTx, failure alternateAgentFailure, chosen domain.AlternateAgentOffer, now time.Time,
) error {
	offers, err := s.alternateAgentOffers(ctx, tx, failure, now)
	if err != nil {
		return err
	}
	offer, ok := domain.OfferedEnrollment(offers, chosen.Enrollment)
	if !ok || offer.Pool != chosen.Pool || offer.Qualification != chosen.Qualification {
		return signet.ErrAlternateAgentNotQualified
	}
	return nil
}

// chosenSelection is the selection a choice from an offer runs for role: the
// offered enrollment with the choice's model and effort, or the role's
// lineup line's where it names none. The enrollment's catalog must serve the
// pair (domain.SelectionServed); an enrollment with no recorded catalog,
// which the bridge would take any model on, accepts only the lineup's. A
// refusal names the field, never the operator's text.
func (s AgentSelection) chosenSelection(
	ctx context.Context, tx *store.ReadTx, role domain.RoleName, choice domain.AlternateAgentChoice,
) (domain.TaskLineSelection, error) {
	line, err := lineupLine(s.Tree, role)
	if err != nil {
		return domain.TaskLineSelection{}, signet.ErrAlternateAgentNotQualified
	}
	selection := domain.TaskLineSelection{
		EnrollmentID: choice.EnrollmentID,
		ModelID:      cmp.Or(choice.ModelID, line.Names.Model),
		Effort:       cmp.Or(choice.Effort, line.Names.Effort),
	}
	enrollment, err := tx.GetClientEnrollment(ctx, selection.EnrollmentID)
	if err != nil {
		return domain.TaskLineSelection{}, err
	}
	catalog, err := readCatalog(ctx, tx, enrollment.ID)
	if err != nil {
		return domain.TaskLineSelection{}, err
	}
	modelServed, effortServed := selection.ModelID == line.Names.Model, selection.Effort == line.Names.Effort
	if catalog != nil {
		err := domain.SelectionServed(enrollment.HarnessClient, catalog, selection.ModelID, selection.Effort)
		modelServed = !errors.Is(err, domain.ErrDiscoveredModelUnserved)
		effortServed = err == nil
	}
	switch {
	case !modelServed:
		return domain.TaskLineSelection{}, fmt.Errorf("model_id: %w", signet.ErrAlternateAgentSelectionUnserved)
	case !effortServed:
		return domain.TaskLineSelection{}, fmt.Errorf("effort: %w", signet.ErrAlternateAgentSelectionUnserved)
	}
	return selection, nil
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

// regateCardAgent re-derives a card's rule for the selection the record
// names, as admission resolved it (plan §5.4 admission step 5): it runs
// another credential than the failed one (freshCredential), it draws on the
// pool the operator chose, and against the failed invocation's recorded
// cause and facts it still escapes what failed under the rule the record
// names. agent's generation must already be read: whether it is unexpired
// and unmarked is admission step 4's to prove. The refusals name no
// selection and none of the failed invocation's facts.
func regateCardAgent(
	ctx context.Context, tx *store.ReadTx, record domain.AlternateAgentRetry, agent RoleAgent,
) error {
	failed, found, err := readFailedAlternateFacts(ctx, tx, record.FailedInvocationID)
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
	if !failed.freshCredential(agent.Enrollment.ID, agent.Generation) {
		return errors.New("it chose the credential that failed")
	}
	pool := agent.Identity.EffectiveUsagePool()
	if pool != record.Pool {
		return errors.New("the selection draws on another pool than the one chosen")
	}
	qualification, ok := domain.QualifyAlternate(outcome.Cause, failed.facts, domain.FailureAlternate{
		Route: agent.Enrollment.Route, UsagePool: string(pool), GenerationValid: true,
	})
	if !ok || qualification != record.Qualification {
		return fmt.Errorf("the selection no longer escapes the failure under %s", record.Qualification)
	}
	return nil
}
