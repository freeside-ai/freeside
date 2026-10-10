package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrAgentNotAdmissible refuses an attempt whose role's selection (its task
// line, or else its lineup line) does not pass the five §5.4 admission steps.
// Nothing is appended and nothing starts.
var ErrAgentNotAdmissible = errors.New("the role's agent does not pass admission")

// cliSubmissionIdentityPrefix starts the manual-submission identity of a
// `freesided submit`, which is also the SetBy of the task lines it records.
const cliSubmissionIdentityPrefix = "cli:"

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

// RoleAgent is a role's selection resolved against the tree and the store:
// the agent the role runs, the enrollment and identity it runs under, and the
// store generation it would mount now.
type RoleAgent struct {
	// Line is the agent and the prompt the role runs. It is the role's lineup
	// line, except that under a task line the agent is the one that line
	// names, at the digest the tree resolves it to.
	Line domain.LineupSelection
	// TaskLine is the task line that chose the agent; nil when the lineup did.
	TaskLine   *domain.TaskLine
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
// arrives with the review record (#898), add only the offer's not_after and
// the attended mark, in the daemon's review check.
//
// It reads the lineup only. No task is in view here, so no task line is: the
// startup and preflight checks, the shadow reviewer, and the review source's
// own agent all resolve through it.
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

func lineupLine(tree agenttree.Tree, role domain.RoleName) (domain.LineupSelection, error) {
	lineup, err := tree.ResolveLineup()
	if err != nil {
		return domain.LineupSelection{}, err
	}
	line, ok := lineup.Line(role)
	if !ok {
		return domain.LineupSelection{}, errors.New("the lineup has no line for it")
	}
	return line, nil
}

func resolveRole(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, role domain.RoleName,
) (RoleAgent, error) {
	line, err := lineupLine(tree, role)
	if err != nil {
		return RoleAgent{}, err
	}
	agent, err := resolveAgent(ctx, tx, tree, line.AgentName, "lineup")
	if err != nil {
		return RoleAgent{}, err
	}
	agent.Line = line
	// Selected: the line names this agent's digest.
	if line.AgentDigest != agent.Resolved.Definition.Digest {
		return RoleAgent{}, fmt.Errorf("the lineup selects agent digest %s, the tree resolves %s",
			line.AgentDigest, agent.Resolved.Definition.Digest)
	}
	if err := agent.readGeneration(ctx, tx); err != nil {
		return RoleAgent{}, err
	}
	return agent, nil
}

// resolveAttemptRole is the one point that selects which agent a role runs
// in an attempt (plan §5.4). The precedence is the alternate-agent card for
// this one attempt, then the task's line for the role, then the lineup. The
// card has no writer yet (#869); it overrides here when it does.
//
// A task line picks the agent only. The prompt stays the one the role's
// lineup line names, so a role with no lineup line is still refused. A line
// that fails any step refuses the attempt: it never falls back to the
// lineup's agent, because the operator chose which credential runs.
func resolveAttemptRole(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, runID domain.RunID, role domain.RoleName,
) (RoleAgent, error) {
	taskLine, found, err := RunTaskLine(ctx, tx, runID, role)
	if err != nil {
		return RoleAgent{}, err
	}
	if !found {
		return resolveRole(ctx, tx, tree, role)
	}
	line, err := lineupLine(tree, role)
	if err != nil {
		return RoleAgent{}, err
	}
	agent, err := resolveAgent(ctx, tx, tree, taskLine.Agent, "task line")
	if err != nil {
		return RoleAgent{}, err
	}
	// A task line names no digest to match (step 2, selected): it names the
	// agent, and the binding records the digest the tree resolves now.
	line.AgentName, line.AgentDigest = taskLine.Agent, agent.Resolved.Definition.Digest
	agent.Line, agent.TaskLine = line, &taskLine
	if err := agent.readGeneration(ctx, tx); err != nil {
		return RoleAgent{}, err
	}
	return agent, nil
}

// resolveAgent joins the named agent to its fragments and to its enrollment
// and identity as the store holds them now. chosenBy names the selection
// that picked the agent, for the refusal.
func resolveAgent(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, name, chosenBy string,
) (RoleAgent, error) {
	source, ok := tree.Agent(name)
	if !ok {
		return RoleAgent{}, fmt.Errorf("the %s names agent %q, which the tree lacks", chosenBy, name)
	}
	var (
		agent RoleAgent
		err   error
	)
	if agent.Enrollment, err = tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(source.Enrollment)); err != nil {
		return RoleAgent{}, fmt.Errorf("enrollment %s: %w", source.Enrollment, err)
	}
	if agent.Identity, err = tx.GetAuthIdentity(ctx, agent.Enrollment.AuthIdentityID); err != nil {
		return RoleAgent{}, fmt.Errorf("auth identity %s: %w", agent.Enrollment.AuthIdentityID, err)
	}
	// Resolve: the agent and its fragments; this refuses a disabled identity.
	if agent.Resolved, err = tree.ResolveAgent(name, agent.Enrollment, agent.Identity); err != nil {
		return RoleAgent{}, err
	}
	return agent, nil
}

// readGeneration reads the store generation the agent's enrollment would
// mount now.
func (a *RoleAgent) readGeneration(ctx context.Context, tx *store.ReadTx) error {
	var err error
	if a.Generation, err = tx.CurrentEnrollmentGeneration(ctx, a.Enrollment.ID); err != nil {
		return fmt.Errorf("enrollment %s holds no store generation: %w", a.Enrollment.ID, err)
	}
	// Credentialed, in part (§5.4 admission rule 4): a generation the
	// credential-integrity probe marked is not a valid one. The mark is read
	// here, in the transaction that read the generation, and not carried on
	// it; re-enrollment appends an unmarked successor, which clears this.
	return tx.RequireGenerationUnmarked(ctx, a.Enrollment.ID, a.Generation.Ordinal)
}

// RunTaskLine returns the line the run's task holds for the role, once that
// line is held against the command that set it. found is false when the task
// has no line for the role, which leaves the role on the lineup.
//
// An error means a line exists that the caller must neither honor nor pass
// over: it refuses, and never runs the lineup's agent instead (plan §5.4).
func RunTaskLine(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID, role domain.RoleName,
) (line domain.TaskLine, found bool, err error) {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return domain.TaskLine{}, false, err
	}
	// Every persisted run is bound to a task (store.AssignTask). A run read
	// back without one cannot show that it has no line.
	if run.TaskID == "" {
		return domain.TaskLine{}, false, fmt.Errorf("run %s names no task, so its task lines cannot be read", runID)
	}
	lines, err := tx.CurrentTaskLines(ctx, run.TaskID)
	if err != nil {
		return domain.TaskLine{}, false, err
	}
	if line, found = lines[role]; !found {
		return domain.TaskLine{}, false, nil
	}
	if err := anchorTaskLine(ctx, tx, line); err != nil {
		return domain.TaskLine{}, false, fmt.Errorf("task line %s, set by %s %q: %w", line.ID, line.Source, line.SetBy, err)
	}
	return line, true, nil
}

// anchorTaskLine holds a decoded line against the operator action that set
// it. The store's read proves only that the row is self-consistent: a line's
// id is an unkeyed hash of its own columns, so a row nobody submitted still
// decodes. Choosing an agent chooses which credential runs (§5.8), so the
// record SetBy names must exist, be of the kind Source claims, and have
// created this task at the instant the line was set.
//
// This proves the line rests on a real operator action on this task. It does
// not prove the agent is the one that action sent: neither record stores the
// lines, and the request digest cannot be recomputed from stored state.
func anchorTaskLine(ctx context.Context, tx *store.ReadTx, line domain.TaskLine) error {
	task, err := tx.GetTask(ctx, line.TaskID)
	if err != nil {
		return err
	}
	// Every writer sets a line in the transaction that creates its task.
	if !line.SetAt.Equal(task.CreatedAt) {
		return fmt.Errorf("it was set at %s, its task was created at %s",
			line.SetAt.Format(time.RFC3339Nano), task.CreatedAt.Format(time.RFC3339Nano))
	}
	// No default: a new source's author decides what record backs it.
	switch line.Source {
	case domain.TaskLineSourceSubmitTask:
		submission, err := tx.GetTaskSubmission(ctx, line.SetBy)
		if err != nil {
			return err
		}
		if submission.TaskID != line.TaskID {
			return fmt.Errorf("that command created task %s", submission.TaskID)
		}
		return nil
	case domain.TaskLineSourceCLISubmit:
		// submit_task records a manual submission too, under a client:
		// identity; only a cli: one is a `freesided submit`.
		if !strings.HasPrefix(line.SetBy, cliSubmissionIdentityPrefix) {
			return errors.New("that is not a CLI submission identity")
		}
		submission, err := tx.GetManualSubmission(ctx, line.SetBy)
		if err != nil {
			return fmt.Errorf("manual submission: %w", err)
		}
		// The submission names its implementation run, which is not stored
		// until the specification is approved. The run it stores, in the
		// transaction that records it, is the specification run derived
		// from that identity; a line is only ever set by a submission of
		// the current identity family.
		run, err := tx.GetRun(ctx, domain.SpecificationRunIDForImplementation(submission.ImplementationRunID))
		if err != nil {
			return err
		}
		if run.TaskID != line.TaskID {
			return fmt.Errorf("that submission created task %s", run.TaskID)
		}
		return nil
	case domain.TaskLineSourceProposalStart:
		// Nothing writes this source yet. Its arm, against the decision
		// command, arrives with its writer (#1641).
		return errors.New("no proposal start records a task line yet")
	}
	return fmt.Errorf("source %q is not a task line source", line.Source)
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
// the check that decides whether to open it. No task is in view, so the check
// proves the lineup's agent only: a task line that cannot be admitted is
// first seen at its attempt.
func (s AgentSelection) CheckRole(
	ctx context.Context, st *store.Store, role domain.RoleName,
	promptDigest domain.Digest, mode domain.OperatingMode, at time.Time,
) (domain.AdmissionAgentBinding, error) {
	if err := s.validate(); err != nil {
		return domain.AdmissionAgentBinding{}, err
	}
	resolved, err := resolveAgentAdmission(ctx, st, s, "", role, promptDigest, mode, at)
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
// attempt is about to run; the lineup line must name it. runID is the
// attempt's run, whose task's line for the role picks the agent when it has
// one; it is empty outside an attempt (CheckRole), where the lineup selects.
func resolveAgentAdmission(
	ctx context.Context, st *store.Store, selection AgentSelection, runID domain.RunID, role domain.RoleName,
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
	// One read transaction, so the task's line, the enrollment, its identity,
	// its current generation, and the conformance record are one consistent
	// view.
	err = st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		// Steps 1 and 2, resolve and selected, and the credential's store.
		if runID == "" {
			agent, err = resolveRole(ctx, tx, selection.Tree, role)
		} else {
			agent, err = resolveAttemptRole(ctx, tx, selection.Tree, runID, role)
		}
		if err != nil {
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
		// Wrapped, not flattened like the other refusals: a marked
		// generation's refusal is typed, and a caller reads the mark off it.
		// The text is what refuse would have printed.
		return agentAdmission{}, fmt.Errorf("role %s: %w: %w", role, err, ErrAgentNotAdmissible)
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
	// Step 5, snapshot. The lineup has no selection record beyond its
	// revision; a task line is cited by its id.
	source, record := domain.AgentSelectionSourceLineup, domain.Digest("")
	if agent.TaskLine != nil {
		source, record = domain.AgentSelectionSourceTaskLine, agent.TaskLine.ID
	}
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
			NativeEffort:    passes.NativeEffort,
			SelectionSource: source, SelectionRecordID: record,
		},
		resolved: resolved, launch: launch, stage: stage,
		enrollment: enrollment, generation: generation,
	}, nil
}
