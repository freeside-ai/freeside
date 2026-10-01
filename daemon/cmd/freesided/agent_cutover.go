package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// Agent selection replaced the identity flags (plan §5.4, issue #867). The
// daemon reads every identity it runs under from the lineup, and before it
// admits anything it checks that each role its configuration asks work from
// resolves and that no retired identity still owns open work. The daemon
// never retires an identity or stops a task on what it finds: it holds
// admission and says why. Retiring is the operator's act, through auth adopt.

const (
	agentSelectionItemPrefix = "system-health-agent-selection-"
	roleCausePrefix          = "role-"
	retiredCausePrefix       = "retired-"
)

// errRetiredIdentityOwnsWork holds lineup admission while a retired
// identity still owns an open task.
var errRetiredIdentityOwnsWork = errors.New("a retired auth identity still owns open work")

// roleAdmissionError names the role whose lineup line the daemon cannot run.
type roleAdmissionError struct {
	Role domain.RoleName
	Err  error
}

func (e *roleAdmissionError) Error() string {
	return fmt.Sprintf("agent selection: role %s: %v", e.Role, e.Err)
}

func (e *roleAdmissionError) Unwrap() error { return e.Err }

// reviewRoleAgent resolves a review role's line and checks what the daemon
// can check without a review launch record (#898 adds launch coverage): the
// line runs the code-owned review prompt, the agent's harness is the one this
// role's review source drives, the offer is still on sale, the enrollment
// holds a current credential, and the agent carries the attended mark for the
// review launch. Review sources run only in an unattended daemon, so the mark
// is always required. A review has no attempt budget until #898, so the offer
// and the credential are held to the instant of the check.
func reviewRoleAgent(
	ctx context.Context, st *store.Store, tree agenttree.Tree, role domain.RoleName, now time.Time,
) (engine.RoleAgent, error) {
	client := domain.HarnessClientCodexCLI
	if role == domain.RoleShadowReviewer {
		client = domain.HarnessClientClaudeCode
	}
	agent, err := engine.ResolveRole(ctx, st, tree, role)
	if err != nil {
		return engine.RoleAgent{}, &roleAdmissionError{Role: role, Err: err}
	}
	refuse := func(format string, args ...any) (engine.RoleAgent, error) {
		return engine.RoleAgent{}, &roleAdmissionError{
			Role: role, Err: fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), engine.ErrAgentNotAdmissible),
		}
	}
	name, digest := ward.ProductionReviewPromptIdentity()
	if agent.Line.PromptName != name || agent.Line.PromptDigest != digest {
		return refuse("the lineup selects prompt %s %s, this daemon reviews with %s %s",
			agent.Line.PromptName, agent.Line.PromptDigest, name, digest)
	}
	if agent.Enrollment.HarnessClient != client {
		return refuse("agent %s runs harness client %s, the %s source drives %s",
			agent.Line.AgentName, agent.Enrollment.HarnessClient, role, client)
	}
	if err := domain.ValidateOfferCoversDeadline(agent.Resolved.Offer, now); err != nil {
		return refuse("%v", err)
	}
	// A store the review source refreshes under its lease has no expiry to
	// hold it to here; any other observable expiry must not have passed.
	if agent.Enrollment.RefreshStrategy != domain.RefreshOnDemand {
		if err := domain.ValidateGenerationExpiryMargin(agent.Enrollment, agent.Generation, now, 0); err != nil {
			return refuse("%v", err)
		}
	}
	launch, err := agentbaseline.RoleLaunch(role)
	if err != nil {
		return refuse("launch: %v", err)
	}
	if tree.Attended(agent.Line.AgentName, agent.Resolved.Definition.Digest, launch.Digest) {
		return refuse("agent %s has no attended mark for launch %s, so it cannot review unattended",
			agent.Line.AgentName, launch.Digest)
	}
	return agent, nil
}

// resolveReviewSelection fills the review identities and cost owners the
// removed flags carried from the reviewer and shadow reviewer lines, so the
// review sources and the approved configuration digests read the values the
// lineup selects. A role the configuration does not use is not resolved.
func resolveReviewSelection(
	ctx context.Context, st *store.Store, tree agenttree.Tree, cfg claudeDriverConfig, now time.Time,
) (claudeDriverConfig, error) {
	if cfg.OperatingMode != domain.ModeUnattended {
		return cfg, nil
	}
	reviewer, err := reviewRoleAgent(ctx, st, tree, domain.RoleReviewer, now)
	if err != nil {
		return cfg, err
	}
	cfg.ReviewAuthIdentityID, cfg.ReviewCostOwner = reviewer.Identity.ID, reviewer.Identity.CostOwner
	if cfg.ShadowReviewImage == "" {
		return cfg, nil
	}
	shadow, err := reviewRoleAgent(ctx, st, tree, domain.RoleShadowReviewer, now)
	if err != nil {
		return cfg, err
	}
	cfg.ShadowReviewAuthIdentityID, cfg.ShadowReviewCostOwner = shadow.Identity.ID, shadow.Identity.CostOwner
	return cfg, nil
}

// reviewAdmission is the check a review source runs before each review: the
// role's line still resolves, still holds a current credential, and still
// selects the identity the source was composed with. The tree is fixed for
// the daemon's run, so what can change is the store (a disabled identity, a
// credential that expired) and the clock (an offer past its not_after).
func reviewAdmission(
	st *store.Store, tree agenttree.Tree, role domain.RoleName, identity domain.AuthIdentityID,
	now func() time.Time,
) func(context.Context) error {
	return func(ctx context.Context) error {
		agent, err := reviewRoleAgent(ctx, st, tree, role, now())
		if err != nil {
			return err
		}
		if agent.Identity.ID != identity {
			return &roleAdmissionError{Role: role, Err: fmt.Errorf(
				"the line now resolves identity %s, the review source runs under %s: %w",
				agent.Identity.ID, identity, engine.ErrAgentNotAdmissible)}
		}
		return nil
	}
}

// checkWriterRoles runs the five admission steps for each writer role with
// the prompt the daemon runs it on, and returns the first role that fails.
func checkWriterRoles(
	ctx context.Context, st *store.Store, agents engine.AgentSelection,
	prompts map[domain.RoleName]domain.Digest, mode domain.OperatingMode, now time.Time,
) *roleAdmissionError {
	for _, role := range []domain.RoleName{domain.RoleSpecifier, domain.RoleImplementer, domain.RoleRemediator} {
		if _, err := agents.CheckRole(ctx, st, role, prompts[role], mode, now); err != nil {
			return &roleAdmissionError{Role: role, Err: err}
		}
	}
	return nil
}

// retiredWork is one retired identity and the open tasks it still owns.
type retiredWork struct {
	Identity domain.AuthIdentityID
	Tasks    []domain.Task
}

// retiredOpenWork finds the open tasks of retired identities. An identity is
// retired when it is disabled and holds no enrollment: no agent can resolve
// to it, so work admitted under it cannot continue through the lineup. That
// state is also what a flag-era identity nobody adopted yet looks like when
// it was stored disabled, which is why the daemon only reports it.
func retiredOpenWork(ctx context.Context, tx *store.ReadTx) ([]retiredWork, error) {
	identities, err := tx.ListAuthIdentities(ctx)
	if err != nil {
		return nil, err
	}
	var retired []domain.AuthIdentityID
	for _, identity := range identities {
		if identity.Enabled {
			continue
		}
		enrollments, err := tx.ListClientEnrollments(ctx, identity.ID)
		if err != nil {
			return nil, err
		}
		if len(enrollments) == 0 {
			retired = append(retired, identity.ID)
		}
	}
	if len(retired) == 0 {
		return nil, nil
	}
	owned, err := legacyOpenTasks(ctx, tx)
	if err != nil {
		return nil, err
	}
	var work []retiredWork
	for _, identity := range retired {
		if tasks := owned[identity]; len(tasks) != 0 {
			work = append(work, retiredWork{Identity: identity, Tasks: tasks})
		}
	}
	return work, nil
}

// agentSelectionGate holds every lineup admission while selection is not
// active: a writer role failed its startup check, or a retired identity still
// owns open work. The role check is fixed for the daemon's run. The retired
// work is read again on each admission, so the gate opens once that work is
// closed; the item activation raised stays open until the next start.
func agentSelectionGate(st *store.Store, failure *roleAdmissionError) func(context.Context) error {
	return func(ctx context.Context) error {
		if failure != nil {
			return failure
		}
		var work []retiredWork
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			work, err = retiredOpenWork(ctx, tx)
			return err
		}); err != nil {
			return fmt.Errorf("read retired identities: %w", err)
		}
		if len(work) != 0 {
			return fmt.Errorf("auth identity %s, task %s: %w",
				work[0].Identity, work[0].Tasks[0].ID, errRetiredIdentityOwnsWork)
		}
		return nil
	}
}

func startupNow(now func() time.Time) time.Time {
	if now != nil {
		return now().UTC()
	}
	return time.Now().UTC()
}

// activateAgentSelection runs once at startup, before the engine loops, and
// converges the system_health items: one per failing role, one per retired
// identity that still owns open work. An item whose cause is gone is
// resolved. It cancels nothing.
func activateAgentSelection(
	ctx context.Context, st *store.Store, failure *roleAdmissionError, now time.Time,
) error {
	var work []retiredWork
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		work, err = retiredOpenWork(ctx, tx)
		return err
	}); err != nil {
		return fmt.Errorf("read retired identities: %w", err)
	}
	causes := map[string]string{}
	if failure != nil {
		causes[roleCausePrefix+string(failure.Role)] = fmt.Sprintf(
			"Agent selection is not active: role %s cannot be admitted (%v). The daemon admits no work "+
				"until the admitted-agent tree and the store agree; fix the line or the enrollment and restart.",
			failure.Role, failure.Err)
	}
	for _, retired := range work {
		ids := make([]string, 0, len(retired.Tasks))
		for _, task := range retired.Tasks {
			ids = append(ids, string(task.ID))
		}
		causes[retiredCausePrefix+contentaddr.Sum([]byte(retired.Identity))] = fmt.Sprintf(
			"Auth identity %s is disabled, holds no enrollment, and still owns open work: %s. "+
				"The daemon admits no work through the lineup while it does. Stop the daemon and either "+
				"adopt the identity with auth adopt, or retire it with auth adopt -retire-unadoptable %s, "+
				"which stops these tasks. The daemon confirms the cancellations after its next start; "+
				"restart it once more after that to clear this item.",
			retired.Identity, strings.Join(ids, ", "), retired.Identity)
	}
	return convergeAgentSelectionItems(ctx, st, causes, "", now)
}

// reportRoleFailure records the item for a role failure that stopped startup
// before the engine existed. The retirement items are left as they are: the
// next start that composes converges them.
func reportRoleFailure(ctx context.Context, st *store.Store, failure *roleAdmissionError, now time.Time) error {
	return convergeAgentSelectionItems(ctx, st, map[string]string{
		roleCausePrefix + string(failure.Role): fmt.Sprintf(
			"Agent selection is not active: role %s cannot be admitted (%v). The daemon did not start; "+
				"fix the line or the enrollment and restart.", failure.Role, failure.Err),
	}, retiredCausePrefix, now)
}

// convergeAgentSelectionItems keeps one open system_health item per cause,
// keyed by the cause in the item id, and resolves the items whose cause is
// gone. Items whose cause starts with a non-empty keep prefix are left alone.
func convergeAgentSelectionItems(
	ctx context.Context, st *store.Store, causes map[string]string, keep string, now time.Time,
) error {
	return st.Write(ctx, func(tx *store.WriteTx) error {
		items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		open := map[string]bool{}
		for _, item := range items {
			rest, ok := strings.CutPrefix(string(item.ID), agentSelectionItemPrefix)
			if !ok {
				continue
			}
			// The id ends with the revision that created it.
			cause := rest[:max(strings.LastIndexByte(rest, '-'), 0)]
			if _, current := causes[cause]; current {
				open[cause] = true
				continue
			}
			if keep != "" && strings.HasPrefix(cause, keep) {
				continue
			}
			item.Status = domain.StatusResolved
			item.ItemVersion++
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
		names, err := tx.DisplayNamesFor(ctx, "project-system", subject)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(causes))
		for cause := range causes {
			keys = append(keys, cause)
		}
		slices.Sort(keys)
		createdAt := now.UTC()
		for _, cause := range keys {
			if open[cause] {
				continue
			}
			posture := domain.HealthPostureBlocking
			item, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID:        domain.ItemID(fmt.Sprintf("%s%s-%d", agentSelectionItemPrefix, cause, state.Revision+1)),
				ProjectID: "project-system", Subject: subject,
				Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
				Reason:            causes[cause],
				RequestedDecision: []domain.Action{domain.ActionAcknowledge},
				HealthDiagnostic: &domain.HealthDiagnostic{
					Code: "agent_selection_inactive", Impairs: domain.ImpairedCapabilityUnattendedAdmission,
				},
				DisplayNames: names, ItemVersion: 1,
				InterruptionClass: domain.InterruptionExceptional,
				CreatedAt:         &createdAt, Posture: &posture, Status: domain.StatusOpen,
			}, nil)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	})
}
