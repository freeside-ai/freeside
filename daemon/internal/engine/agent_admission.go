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
func (e *Engine) resolveAgentAdmission(
	ctx context.Context, selection AgentSelection, role domain.RoleName,
	promptDigest domain.Digest, mode domain.OperatingMode, admittedAt time.Time,
) (agentAdmission, error) {
	refuse := func(format string, args ...any) (agentAdmission, error) {
		return agentAdmission{}, fmt.Errorf("role %s: %s: %w", role, fmt.Sprintf(format, args...), ErrAgentNotAdmissible)
	}
	stage, ok := role.Stage()
	if !ok {
		return refuse("it is not a ward role")
	}
	lineup, err := selection.Tree.ResolveLineup()
	if err != nil {
		return refuse("%v", err)
	}
	line, ok := lineup.Line(role)
	if !ok {
		return refuse("the lineup has no line for it")
	}
	source, ok := selection.Tree.Agent(line.AgentName)
	if !ok {
		return refuse("the lineup names agent %q, which the tree lacks", line.AgentName)
	}
	launch, err := selection.Launch(role)
	if err != nil {
		return refuse("launch: %v", err)
	}

	var (
		enrollment  domain.ClientEnrollment
		identity    domain.AuthIdentity
		generation  domain.EnrollmentGeneration
		conformance domain.AdapterConformance
		resolved    agenttree.ResolvedAgent
	)
	// One read transaction, so the enrollment, its identity, its current
	// generation, and the conformance record are one consistent view.
	err = e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if enrollment, err = tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(source.Enrollment)); err != nil {
			return fmt.Errorf("enrollment %s: %w", source.Enrollment, err)
		}
		if identity, err = tx.GetAuthIdentity(ctx, enrollment.AuthIdentityID); err != nil {
			return fmt.Errorf("auth identity %s: %w", enrollment.AuthIdentityID, err)
		}
		// Step 1, resolve: the agent and its fragments.
		if resolved, err = selection.Tree.ResolveAgent(line.AgentName, enrollment, identity); err != nil {
			return err
		}
		var found bool
		if conformance, found, err = tx.LatestAdapterConformance(ctx, resolved.Adapter.Digest); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("adapter %s has no conformance record", resolved.Adapter.Digest)
		}
		if generation, err = tx.CurrentEnrollmentGeneration(ctx, enrollment.ID); err != nil {
			return fmt.Errorf("enrollment %s holds no store generation: %w", enrollment.ID, err)
		}
		return nil
	})
	if err != nil {
		return refuse("%v", err)
	}
	agent := resolved.Definition
	// Step 2, selected: the line names this agent's digest and this prompt.
	if line.AgentDigest != agent.Digest {
		return refuse("the lineup selects agent digest %s, the tree resolves %s", line.AgentDigest, agent.Digest)
	}
	if line.PromptDigest != promptDigest {
		return refuse("the lineup selects prompt %s, the attempt runs %s", line.PromptDigest, promptDigest)
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
	if mode == domain.ModeUnattended && selection.Tree.Attended(line.AgentName, agent.Digest, launch.Digest) {
		return refuse("agent %s has no attended mark for launch %s, so it cannot run unattended",
			line.AgentName, launch.Digest)
	}
	for _, authority := range selection.EffectiveEgress {
		if !slices.Contains(resolved.Route.InferenceAuthorities, authority) {
			return refuse("provider endpoint %q is outside the route's inference authorities", authority)
		}
	}
	// Step 5, snapshot.
	passes, err := domain.DeriveAgentLaunchSelection(agent, resolved.Adapter, resolved.Offer)
	if err != nil {
		return refuse("%v", err)
	}
	effort, err := domain.TranslateEffort(resolved.Adapter.ClientKind, agent.Effort)
	if err != nil {
		return refuse("%v", err)
	}
	treatment, err := domain.ComputeTreatmentDigest(
		resolved.Route, resolved.Adapter.Digest, launch.Digest, resolved.Offer, agent.Effort, effort.Effective(),
	)
	if err != nil {
		return refuse("%v", err)
	}
	return agentAdmission{
		binding: domain.AdmissionAgentBinding{
			AgentDigest: agent.Digest, LaunchDigest: launch.Digest, TreatmentDigest: treatment,
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
