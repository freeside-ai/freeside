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

// ReviewRole is what the daemon's composition fixes about one review role's
// source, which the lineup's agent is held to: the code-owned prompt the
// source reviews with, the harness client it drives and the vendor whose
// native instructions it consumes, the allowlist its proxy admits, and the
// model shape its launch can pass as one argument.
type ReviewRole struct {
	Role          domain.RoleName
	PromptName    string
	PromptDigest  domain.Digest
	HarnessClient domain.HarnessClientKind
	Vendor        domain.AgentVendor
	// EffectiveEgress is recorded on the snapshot. It is not held to the
	// route's inference authorities (domain.ValidateReviewAdmissionDerivations).
	EffectiveEgress []string
	ValidModel      func(string) bool
}

// ReviewRoleAgent is a review role's resolved agent with what its launch
// passes: the offer's model and the effort as the harness names it.
type ReviewRoleAgent struct {
	RoleAgent
	Model, NativeEffort string

	launch domain.LaunchSpec
	passes domain.AgentLaunchSelection
}

// ReviewAdmitter resolves the snapshot one review invocation is admitted
// under. The publication workflow calls it once per invocation, before the
// review source is asked, and stores what it returns; a refusal fails the
// invocation and nothing is requested.
type ReviewAdmitter func(
	ctx context.Context, role domain.RoleName, runID domain.RunID,
	invocationID domain.InvocationID, admittedAt time.Time,
) (domain.ReviewAdmission, error)

// CheckReviewRole runs the review admission steps for one role (plan §5.4
// steps 1, 2, and 4): the role's line, the task's or else the lineup's,
// resolves to an agent whose fragments,
// enrollment, and enabled identity join; the line selects that agent's
// current digest and the code-owned review prompt; the agent's harness is the
// one the role's source drives and its adapter is that source's vendor; the
// offer is still on sale and the enrollment
// holds a current credential at now; and the agent names a model and an
// effort the launch can pass.
//
// Step 3, launch coverage, is not enforced for the review roles: the review
// launch forbids auxiliary inference and no review adapter declares that
// control yet (#1980). A review has no attempt budget, so the offer and the
// credential are held to the instant of the check.
//
// runID names the run a review is about to be requested for; it is empty at
// startup and preflight, where no task is in view. A task line for the role
// selects the agent the way it does for a writer (resolveAttemptRole): the
// prompt stays the lineup line's, and a line that fails any step refuses the
// review and never falls back to the lineup's agent. So is a line that does
// not hold against the command that set it. The line selects within the
// source the daemon composed: the harness and vendor checks refuse an agent
// the role's source does not drive (#397). The shadow reviewer takes no task
// line and is not checked for one.
//
// The generation's integrity mark is read in the same transaction as the
// rest but reported after every other step. Any non-nil error refuses
// admission. Only a *domain.GenerationIntegrityMarkedError returns a checked
// agent alongside the error, so startup can compose the source while each
// launch still refuses. Every other error returns a zero agent.
//
// The selection gate is not consulted. It holds new writer work until no
// retired identity owns open work, and that work finishes through its
// reviews.
func CheckReviewRole(
	ctx context.Context, st *store.Store, tree agenttree.Tree,
	launch func(domain.RoleName) (domain.LaunchSpec, error),
	spec ReviewRole, runID domain.RunID, now time.Time,
) (ReviewRoleAgent, error) {
	role := spec.Role
	refuse := func(format string, args ...any) (ReviewRoleAgent, error) {
		return ReviewRoleAgent{}, fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrAgentNotAdmissible)
	}
	if role != domain.RoleReviewer && role != domain.RoleShadowReviewer {
		return refuse("role %s is not a review role", role)
	}
	var (
		agent  ReviewRoleAgent
		line   domain.TaskLine
		found  bool
		marked error
	)
	// One read transaction, so the task's line, the enrollment, its
	// identity, its current generation, and its mark are one consistent view.
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if runID != "" && role.TaskLineEligible() {
			if line, found, err = RunTaskLine(ctx, tx, runID, role); err != nil {
				return err
			}
		}
		if found {
			agent.RoleAgent, err = resolveReviewLine(ctx, tx, tree, role, line)
		} else {
			agent.RoleAgent, err = resolveRoleLine(ctx, tx, tree, role)
		}
		if err != nil {
			return fmt.Errorf("role %s: %w", role, err)
		}
		if err := agent.requireUnmarked(ctx, tx); err != nil {
			// Only the mark is held for last; a failed read refuses now.
			if !errors.Is(err, domain.ErrGenerationIntegrityMarked) {
				return fmt.Errorf("role %s: %w", role, err)
			}
			marked = fmt.Errorf("role %s: %w", role, err)
		}
		return nil
	})
	if err != nil {
		return ReviewRoleAgent{}, errors.Join(ErrAgentNotAdmissible, err)
	}
	if found {
		// Every later refusal names the line that selected the agent. The
		// agent's name is the tree's own by now, so it may be printed.
		refuseRole := refuse
		refuse = func(format string, args ...any) (ReviewRoleAgent, error) {
			return refuseRole("task line %s: "+format, append([]any{line.ID}, args...)...)
		}
	}
	if agent.Line.PromptName != spec.PromptName || agent.Line.PromptDigest != spec.PromptDigest {
		return refuse("the lineup selects prompt %s %s, this daemon reviews with %s %s",
			agent.Line.PromptName, agent.Line.PromptDigest, spec.PromptName, spec.PromptDigest)
	}
	if agent.Enrollment.HarnessClient != spec.HarnessClient {
		return refuse("agent %s runs harness client %s, the %s source drives %s",
			agent.Line.AgentName, agent.Enrollment.HarnessClient, role, spec.HarnessClient)
	}
	if agent.Resolved.Adapter.Vendor != spec.Vendor {
		return refuse("agent %s is on a %s adapter, the %s source consumes %s instructions",
			agent.Line.AgentName, agent.Resolved.Adapter.Vendor, role, spec.Vendor)
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
	if agent.launch, err = launch(role); err != nil {
		return refuse("launch: %v", err)
	}
	// Every review launch passes a model and an effort, and the approved
	// review configuration digest covers both. An agent that names neither
	// would be a launch nobody approved, so it is refused, not run on the
	// harness's own defaults.
	agent.passes, err = domain.DeriveAgentLaunchSelection(
		agent.Resolved.Definition, agent.Resolved.Adapter, agent.Resolved.Offer)
	if err != nil {
		return refuse("%v", err)
	}
	if agent.passes.RouteModelID == "" {
		return refuse("agent %s is on an offer that names no model, and a review launch always passes one",
			agent.Line.AgentName)
	}
	if agent.passes.NativeEffort == "" {
		return refuse("agent %s asks effort %s, which sends none, and a review launch always passes one",
			agent.Line.AgentName, agent.Resolved.Definition.Effort)
	}
	if spec.ValidModel != nil && !spec.ValidModel(agent.passes.RouteModelID) {
		return refuse("agent %s is on an offer whose model the review launch cannot pass as one argument",
			agent.Line.AgentName)
	}
	agent.Model, agent.NativeEffort = agent.passes.RouteModelID, agent.passes.NativeEffort
	if marked != nil {
		return agent, errors.Join(ErrAgentNotAdmissible, marked)
	}
	return agent, nil
}

// resolveReviewLine resolves the agent a reviewer line names, refusing by the
// line's id and never by the name it holds. That name is text an operator
// typed, and a credential pasted there can pass the name grammar; a refusal
// becomes the review failure's recorded reason. Once the tree holds the
// agent, the name is the tree's own, so later refusals may print it.
func resolveReviewLine(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, role domain.RoleName, line domain.TaskLine,
) (RoleAgent, error) {
	if _, ok := tree.Agent(line.Agent); !ok {
		return RoleAgent{}, fmt.Errorf("task line %s names an agent the tree lacks", line.ID)
	}
	agent, err := resolveLineAgent(ctx, tx, tree, role, line)
	if err != nil {
		return RoleAgent{}, fmt.Errorf("task line %s: %w", line.ID, err)
	}
	return agent, nil
}

// AdmitReview resolves the snapshot one review invocation is admitted under
// (plan §5.4 step 5): CheckReviewRole's checks for the invocation's run at
// admittedAt, then the binding an execution admission records, built the same
// way and citing the task line that chose the agent, with the offer's lineage
// group and the identity the enrollment belongs to. The caller stores it once and never resolves the invocation again.
func (s AgentSelection) AdmitReview(
	ctx context.Context, st *store.Store, spec ReviewRole, runID domain.RunID,
	invocationID domain.InvocationID, admittedAt time.Time,
) (domain.ReviewAdmission, error) {
	refuse := func(err error) (domain.ReviewAdmission, error) {
		return domain.ReviewAdmission{}, fmt.Errorf("admission snapshot: %w: %w", err, ErrAgentNotAdmissible)
	}
	if runID == "" {
		return refuse(errors.New("a review is admitted for a run"))
	}
	agent, err := CheckReviewRole(ctx, st, s.Tree, s.Launch, spec, runID, admittedAt)
	if err != nil {
		return domain.ReviewAdmission{}, err
	}
	resolved, definition := agent.Resolved, agent.Resolved.Definition
	source, record := agent.Selection()
	effort, err := domain.TranslateEffort(resolved.Adapter.ClientKind, definition.Effort)
	if err != nil {
		return refuse(err)
	}
	treatment, err := domain.ComputeTreatmentDigest(
		resolved.Route, resolved.Adapter.Digest, agent.launch.Digest, resolved.Offer,
		definition.Effort, effort.Effective(),
	)
	if err != nil {
		return refuse(err)
	}
	admission, err := domain.NewReviewAdmission(domain.ReviewAdmission{
		InvocationID: invocationID, RunID: runID, Role: spec.Role,
		AuthIdentityID: agent.Identity.ID, LineageGroup: resolved.Offer.LineageGroup,
		AgentBinding: domain.AdmissionAgentBinding{
			AgentDigest: definition.Digest, LaunchDigest: agent.launch.Digest, TreatmentDigest: treatment,
			PricingRevision: resolved.Offer.PricingRevision, LineupRevision: s.LineupRevision,
			EnrollmentID: agent.Enrollment.ID, EnrollmentGeneration: agent.Generation.Ordinal,
			StoreManifestDigest: agent.Generation.StoreManifestDigest,
			EffectiveEgress:     slices.Clone(spec.EffectiveEgress),
			RouteModelID:        agent.passes.RouteModelID, RequestedEffort: agent.passes.RequestedEffort,
			NativeEffort:    agent.passes.NativeEffort,
			SelectionSource: source, SelectionRecordID: record,
		},
		AdmittedAt: admittedAt,
	})
	if err != nil {
		return refuse(err)
	}
	// The closure recheck, so a resolver bug cannot store a snapshot the
	// domain would refuse.
	if err := domain.ValidateReviewAdmissionDerivations(
		admission, definition, resolved.Adapter, resolved.Route, resolved.Offer, agent.launch,
		agent.Enrollment, agent.Generation,
	); err != nil {
		return refuse(err)
	}
	return admission, nil
}
