package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrAgentNotAdmissible refuses an attempt whose role's lineup line does not
// pass the five §5.4 admission steps. Nothing is appended and nothing starts.
var ErrAgentNotAdmissible = errors.New("the role's agent does not pass admission")

// AgentSelection makes the engine admit ward stages through the lineup (plan
// §5.4) instead of one configured identity: each attempt resolves its role's
// line in Tree, and the identity, credential mode, and agent binding the
// admission records come from that resolution.
type AgentSelection struct {
	// Tree is the admitted-agent tree at the commit the daemon was started
	// with, and LineupRevision the content address of its files.
	Tree           agenttree.Tree
	LineupRevision domain.Digest
	// Launch is the launch spec the daemon's code runs for a ward role.
	Launch func(domain.RoleName) (domain.LaunchSpec, error)
	// EffectiveEgress is the provider allowlist the writer's proxy admits.
	// Every authority must be one the agent's route names.
	EffectiveEgress []string
	// AttemptBudget is the wall-clock budget of one attempt; the admission
	// instant plus it is the deadline the offer and credential must cover.
	AttemptBudget time.Duration
	// ExpiryMargin is how far past the deadline an expiring credential must
	// last.
	ExpiryMargin time.Duration
	// Gate, when set, is consulted before every lineup admission; an error
	// refuses the attempt. The daemon holds admission through it until
	// selection has activated (no retired identity still owns open work).
	Gate func(context.Context) error
}

// RoleAgent is a role's lineup line resolved against the tree and the store:
// the agent the line selects, the enrollment and identity it runs under, and
// the store generation it would mount now.
type RoleAgent struct {
	Line       domain.LineupSelection
	Resolved   agenttree.ResolvedAgent
	Identity   domain.AuthIdentity
	Enrollment domain.ClientEnrollment
	Generation domain.EnrollmentGeneration
}

// ResolveRole runs the admission steps that need no launch (plan §5.4): the
// role's line resolves to an agent whose fragments, enrollment, and enabled
// identity join; the line selects that agent's current digest; and the
// enrollment holds a store generation. Stage admission adds the launch,
// conformance, and deadline checks; the review roles, whose launch coverage
// arrives with the review record (#898), stop here.
func ResolveRole(
	ctx context.Context, st *store.Store, tree agenttree.Tree, role domain.RoleName,
) (RoleAgent, error) {
	var agent RoleAgent
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		agent, err = resolveRole(ctx, tx, tree, role)
		return err
	})
	if err != nil {
		return RoleAgent{}, fmt.Errorf("role %s: %w", role, errors.Join(ErrAgentNotAdmissible, err))
	}
	return agent, nil
}

func resolveRole(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, role domain.RoleName,
) (RoleAgent, error) {
	lineup, err := tree.ResolveLineup()
	if err != nil {
		return RoleAgent{}, err
	}
	line, ok := lineup.Line(role)
	if !ok {
		return RoleAgent{}, errors.New("the lineup has no line for it")
	}
	source, ok := tree.Agent(line.AgentName)
	if !ok {
		return RoleAgent{}, fmt.Errorf("the lineup names agent %q, which the tree lacks", line.AgentName)
	}
	agent := RoleAgent{Line: line}
	if agent.Enrollment, err = tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(source.Enrollment)); err != nil {
		return RoleAgent{}, fmt.Errorf("enrollment %s: %w", source.Enrollment, err)
	}
	if agent.Identity, err = tx.GetAuthIdentity(ctx, agent.Enrollment.AuthIdentityID); err != nil {
		return RoleAgent{}, fmt.Errorf("auth identity %s: %w", agent.Enrollment.AuthIdentityID, err)
	}
	// Resolve: the agent and its fragments; this refuses a disabled identity.
	if agent.Resolved, err = tree.ResolveAgent(line.AgentName, agent.Enrollment, agent.Identity); err != nil {
		return RoleAgent{}, err
	}
	// Selected: the line names this agent's digest.
	if line.AgentDigest != agent.Resolved.Definition.Digest {
		return RoleAgent{}, fmt.Errorf("the lineup selects agent digest %s, the tree resolves %s",
			line.AgentDigest, agent.Resolved.Definition.Digest)
	}
	if agent.Generation, err = tx.CurrentEnrollmentGeneration(ctx, agent.Enrollment.ID); err != nil {
		return RoleAgent{}, fmt.Errorf("enrollment %s holds no store generation: %w", agent.Enrollment.ID, err)
	}
	return agent, nil
}

func (s AgentSelection) validate() error {
	switch {
	case !contentaddr.Valid(string(s.LineupRevision)):
		return fmt.Errorf("agent selection lineup revision %q is not canonical", s.LineupRevision)
	case s.Launch == nil:
		return errors.New("agent selection has no launch source")
	case len(s.EffectiveEgress) == 0:
		return errors.New("agent selection has no effective egress")
	case s.AttemptBudget <= 0:
		return errors.New("agent selection attempt budget is not positive")
	case s.ExpiryMargin < 0:
		return errors.New("agent selection expiry margin is negative")
	}
	return nil
}

// CheckRole runs the five admission steps for a ward role outside an attempt
// and returns the binding an admission at that instant would snapshot. The
// daemon runs it at startup, so a role that cannot be admitted is named
// before any work is offered. Gate is not consulted: it holds attempts, not
// the check that decides whether to open it.
func (s AgentSelection) CheckRole(
	ctx context.Context, st *store.Store, role domain.RoleName,
	promptDigest domain.Digest, mode domain.OperatingMode, at time.Time,
) (domain.AdmissionAgentBinding, error) {
	if err := s.validate(); err != nil {
		return domain.AdmissionAgentBinding{}, err
	}
	resolved, err := resolveAgentAdmission(ctx, st, s, role, promptDigest, mode, at)
	return resolved.binding, err
}

// agentAdmission is one role's resolution: what the admission records, and
// the closure the derivation recheck reads.
type agentAdmission struct {
	binding    domain.AdmissionAgentBinding
	resolved   agenttree.ResolvedAgent
	launch     domain.LaunchSpec
	stage      domain.StageName
	enrollment domain.ClientEnrollment
	generation domain.EnrollmentGeneration
}

// wardRole maps an attempt to the ward role that runs it. The implementation
// stage holds two roles: a remediation round or an operator-feedback pass is
// the remediator's, and every other implementation attempt the implementer's.
// A stage no ward role runs (clean verification) reports false.
func wardRole(
	run domain.Run, stage domain.Stage, invocationID domain.InvocationID,
) (domain.RoleName, bool) {
	switch {
	case stage.ID == specificationStageID(run.ID) && stage.Name == specificationStageName:
		return domain.RoleSpecifier, true
	case stage.Name != productionStageName:
		return "", false
	}
	if _, remediation := remediationRoundForInvocation(run.ID, invocationID); remediation ||
		stage.ID == operatorFeedbackStageID(invocationID) {
		return domain.RoleRemediator, true
	}
	return domain.RoleImplementer, true
}

// resolveAgentAdmission runs the five §5.4 admission steps for one role and
// returns the binding the admission snapshots. promptDigest is the prompt the
// attempt is about to run; the lineup line must name it.
func resolveAgentAdmission(
	ctx context.Context, st *store.Store, selection AgentSelection, role domain.RoleName,
	promptDigest domain.Digest, mode domain.OperatingMode, admittedAt time.Time,
) (agentAdmission, error) {
	refuse := func(format string, args ...any) (agentAdmission, error) {
		return agentAdmission{}, fmt.Errorf("role %s: %s: %w", role, fmt.Sprintf(format, args...), ErrAgentNotAdmissible)
	}
	stage, ok := role.Stage()
	if !ok {
		return refuse("it is not a ward role")
	}
	launch, err := selection.Launch(role)
	if err != nil {
		return refuse("launch: %v", err)
	}

	var (
		agent       RoleAgent
		conformance domain.AdapterConformance
	)
	// One read transaction, so the enrollment, its identity, its current
	// generation, and the conformance record are one consistent view.
	err = st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		// Steps 1 and 2, resolve and selected, and the credential's store.
		if agent, err = resolveRole(ctx, tx, selection.Tree, role); err != nil {
			return err
		}
		var found bool
		if conformance, found, err = tx.LatestAdapterConformance(ctx, agent.Resolved.Adapter.Digest); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("adapter %s has no conformance record", agent.Resolved.Adapter.Digest)
		}
		return nil
	})
	if err != nil {
		return refuse("%v", err)
	}
	resolved, enrollment, generation := agent.Resolved, agent.Enrollment, agent.Generation
	definition := resolved.Definition
	// The line also names the prompt the attempt is about to run.
	if agent.Line.PromptDigest != promptDigest {
		return refuse("the lineup selects prompt %s, the attempt runs %s", agent.Line.PromptDigest, promptDigest)
	}
	// Step 3, proved: the adapter's latest conformance covers the launch.
	if err := domain.ValidateAdapterLaunchCoverage(conformance, resolved.Adapter.Digest, launch); err != nil {
		return refuse("%v", err)
	}
	// Step 4, credentialed: a credential that outlives the attempt and an
	// offer still on sale at its deadline. Resolution already refused a
	// disabled identity.
	deadline := admittedAt.Add(selection.AttemptBudget)
	if err := domain.ValidateGenerationExpiryMargin(enrollment, generation, deadline, selection.ExpiryMargin); err != nil {
		return refuse("%v", err)
	}
	if err := domain.ValidateOfferCoversDeadline(resolved.Offer, deadline); err != nil {
		return refuse("%v", err)
	}
	// An agent and launch pair with no attended mark runs attended only.
	if mode == domain.ModeUnattended && selection.Tree.Attended(agent.Line.AgentName, definition.Digest, launch.Digest) {
		return refuse("agent %s has no attended mark for launch %s, so it cannot run unattended",
			agent.Line.AgentName, launch.Digest)
	}
	for _, authority := range selection.EffectiveEgress {
		if !slices.Contains(resolved.Route.InferenceAuthorities, authority) {
			return refuse("provider endpoint %q is outside the route's inference authorities", authority)
		}
	}
	// Step 5, snapshot.
	passes, err := domain.DeriveAgentLaunchSelection(definition, resolved.Adapter, resolved.Offer)
	if err != nil {
		return refuse("%v", err)
	}
	effort, err := domain.TranslateEffort(resolved.Adapter.ClientKind, definition.Effort)
	if err != nil {
		return refuse("%v", err)
	}
	treatment, err := domain.ComputeTreatmentDigest(
		resolved.Route, resolved.Adapter.Digest, launch.Digest, resolved.Offer, definition.Effort, effort.Effective(),
	)
	if err != nil {
		return refuse("%v", err)
	}
	return agentAdmission{
		binding: domain.AdmissionAgentBinding{
			AgentDigest: definition.Digest, LaunchDigest: launch.Digest, TreatmentDigest: treatment,
			PricingRevision: resolved.Offer.PricingRevision, LineupRevision: selection.LineupRevision,
			EnrollmentID: enrollment.ID, EnrollmentGeneration: generation.Ordinal,
			StoreManifestDigest: generation.StoreManifestDigest,
			EffectiveEgress:     slices.Clone(selection.EffectiveEgress),
			Attended:            mode != domain.ModeUnattended,
			RouteModelID:        passes.RouteModelID, RequestedEffort: passes.RequestedEffort,
			NativeEffort: passes.NativeEffort,
		},
		resolved: resolved, launch: launch, stage: stage,
		enrollment: enrollment, generation: generation,
	}, nil
}
