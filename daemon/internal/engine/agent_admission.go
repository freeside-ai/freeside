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
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrAgentNotAdmissible refuses an attempt whose role's selection (its task
// line, or else its lineup line) does not pass the five §5.4 admission steps.
// Nothing is appended and nothing starts.
var ErrAgentNotAdmissible = errors.New("the role's agent does not pass admission")

// AgentAdmissionRefusal is the typed form of a stage admission's
// ErrAgentNotAdmissible refusal. It reads and matches as the error it wraps,
// so errors.Is still finds ErrAgentNotAdmissible and anything the refusal
// wraps, while errors.As hands a hold the refused check, agent, and launch
// without reading the refusal's text (issue #2211).
type AgentAdmissionRefusal struct {
	// Refusal names the check, and the agent and launch once they resolved.
	Refusal domain.AdmissionRefusal
	err     error
}

func (e *AgentAdmissionRefusal) Error() string { return e.err.Error() }

func (e *AgentAdmissionRefusal) Unwrap() error { return e.err }

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
	TaskLine *domain.TaskLine
	// Card is the alternate-agent record that chose the agent for this one
	// attempt; nil when a line or the lineup did. It takes precedence over
	// TaskLine.
	Card       *domain.AlternateAgentRetry
	Resolved   agenttree.ResolvedAgent
	Identity   domain.AuthIdentity
	Enrollment domain.ClientEnrollment
	Generation domain.EnrollmentGeneration
}

// ResolveRole runs the admission steps that need no launch (plan §5.4): the
// role's line resolves to an agent whose fragments, enrollment, and enabled
// identity join; the line selects that agent's current digest; and the
// enrollment holds an unmarked store generation. Stage admission adds the launch,
// conformance, and deadline checks; the review roles, whose launch coverage
// is not enforced yet (#1980), add only the offer's not_after, in the
// daemon's review check.
//
// It reads the lineup only. No task is in view here, so no task line is: the
// startup and preflight checks resolve through it. A review check for a run
// with no reviewer line resolves the same line, but reports a mark after its
// other admission steps.
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

// RoleHarnessClient is the harness client the role's lineup line runs. It
// runs ResolveRole's steps except the generation's integrity mark, and
// returns no agent, so nothing launches on it: it picks the review source the
// daemon composes for the reviewer, and CheckReviewRole, which every review
// launch runs, refuses a marked generation.
func RoleHarnessClient(
	ctx context.Context, st *store.Store, tree agenttree.Tree, role domain.RoleName,
) (domain.HarnessClientKind, error) {
	var agent RoleAgent
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		agent, err = resolveRoleLine(ctx, tx, tree, role)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("role %s: %w", role, errors.Join(ErrAgentNotAdmissible, err))
	}
	return agent.Enrollment.HarnessClient, nil
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
	agent, err := resolveRoleLine(ctx, tx, tree, role)
	if err != nil {
		return RoleAgent{}, err
	}
	if err := agent.requireUnmarked(ctx, tx); err != nil {
		return RoleAgent{}, err
	}
	return agent, nil
}

// resolveRoleLine is resolveRole without the generation's integrity mark.
// Admission callers must also read the mark in the same transaction and
// refuse on it. RoleHarnessClient only selects a source; it admits nothing.
func resolveRoleLine(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, role domain.RoleName,
) (RoleAgent, error) {
	line, err := lineupLine(tree, role)
	if err != nil {
		return RoleAgent{}, err
	}
	agent, err := resolveAgent(ctx, tx, tree, line.AgentName)
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
// this one attempt, then the task's line for the role, then the lineup. A
// card's agent is re-gated against the failure it answered (regateCardAgent).
//
// A task line picks the agent only. The prompt stays the one the role's
// lineup line names, so a role with no lineup line is still refused. A line
// that fails any step refuses the attempt: it never falls back to the
// lineup's agent, because the operator chose which credential runs.
func resolveAttemptRole(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, runID domain.RunID, role domain.RoleName,
) (RoleAgent, error) {
	card, carded, err := attemptCard(ctx, tx, runID, role)
	if err != nil {
		return RoleAgent{}, err
	}
	if carded {
		agent, err := resolveCardAgent(ctx, tx, tree, role, card)
		if err != nil {
			return RoleAgent{}, err
		}
		if err := agent.requireUnmarked(ctx, tx); err != nil {
			return RoleAgent{}, err
		}
		return agent, nil
	}
	taskLine, found, err := RunTaskLine(ctx, tx, runID, role)
	if err != nil {
		return RoleAgent{}, err
	}
	if !found {
		return resolveRole(ctx, tx, tree, role)
	}
	agent, err := resolveLineAgent(ctx, tx, tree, role, taskLine)
	if err != nil {
		return RoleAgent{}, err
	}
	if err := agent.requireUnmarked(ctx, tx); err != nil {
		return RoleAgent{}, err
	}
	return agent, nil
}

// resolveLineAgent is resolveRoleLine for an agent a task line names. It runs
// without the generation's integrity mark, which the caller reads in the same
// transaction.
func resolveLineAgent(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, role domain.RoleName, taskLine domain.TaskLine,
) (RoleAgent, error) {
	line, err := lineupLine(tree, role)
	if err != nil {
		return RoleAgent{}, err
	}
	bases, basis, err := counterpartBases(ctx, tx, line, taskLine)
	if err != nil {
		return RoleAgent{}, fmt.Errorf("task line %s: %w", taskLine.ID, err)
	}
	var counterpart *domain.LineupSelection
	if bases != nil {
		escalated, err := triageCounterpart(tree, taskLine, bases, basis)
		if err != nil {
			return RoleAgent{}, fmt.Errorf("task line %s: %w", taskLine.ID, err)
		}
		counterpart = &escalated
	}
	// Refuse by the line's id, never the name it holds. That name is text an
	// operator typed, and a credential pasted there can pass the name grammar.
	// Once the tree holds the agent, the name is the tree's own, so later
	// refusals may print it.
	if _, ok := tree.Agent(taskLine.Agent); !ok {
		return RoleAgent{}, fmt.Errorf("task line %s names an agent the tree lacks", taskLine.ID)
	}
	agent, err := resolveAgent(ctx, tx, tree, taskLine.Agent)
	if err != nil {
		return RoleAgent{}, fmt.Errorf("task line %s: %w", taskLine.ID, err)
	}
	// The escalated line pins the counterpart's digest as the primary line
	// pins its agent's, and the triage line's authority is that line's.
	if counterpart != nil && counterpart.AgentDigest != agent.Resolved.Definition.Digest {
		return RoleAgent{}, fmt.Errorf("task line %s: the escalated line selects agent digest %s, the tree resolves %s",
			taskLine.ID, counterpart.AgentDigest, agent.Resolved.Definition.Digest)
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

// resolveCardAgent is resolveLineAgent for the agent a card record chose: the
// prompt stays the lineup line's, the agent is the record's at the digest the
// tree resolves now, and the rule the record names is re-derived for it. It
// runs without the generation's integrity mark, which the caller reads in the
// same transaction. A record that fails any step refuses the attempt, never
// falling back to the task's line or the lineup.
func resolveCardAgent(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, role domain.RoleName, card domain.AlternateAgentRetry,
) (RoleAgent, error) {
	line, err := lineupLine(tree, role)
	if err != nil {
		return RoleAgent{}, err
	}
	if _, ok := tree.Agent(card.Agent); !ok {
		return RoleAgent{}, fmt.Errorf("card record %s names an agent the tree lacks", card.ID)
	}
	agent, err := resolveAgent(ctx, tx, tree, card.Agent)
	if err != nil {
		return RoleAgent{}, fmt.Errorf("card record %s: %w", card.ID, err)
	}
	if err := agent.readGeneration(ctx, tx); err != nil {
		return RoleAgent{}, err
	}
	if err := regateCardAgent(ctx, tx, tree, card, agent); err != nil {
		return RoleAgent{}, fmt.Errorf("card record %s: %w", card.ID, err)
	}
	line.AgentName, line.AgentDigest = card.Agent, agent.Resolved.Definition.Digest
	agent.Line, agent.Card = line, &card
	return agent, nil
}

// counterpartBases returns the agents whose escalated counterpart a task
// line must name, and a description of them that is safe to print, or nil
// when the line need not name a counterpart. A triage line names the
// counterpart of the specifier's lineup agent. A specifier router or move
// line whose agent its cited revision does not order names the counterpart
// of an agent that revision orders for the specifier: the router routes an
// escalated specifier among those (escalatedOrder), and a move re-pins it
// the same way. anchorLinePolicy admits such an agent only for a task whose
// triage record escalated.
func counterpartBases(
	ctx context.Context, tx *store.ReadTx, line domain.LineupSelection, taskLine domain.TaskLine,
) ([]string, string, error) {
	if taskLine.Source == domain.TaskLineSourceTriage {
		return []string{line.AgentName}, "lineup agent " + line.AgentName, nil
	}
	if taskLine.Role != domain.RoleSpecifier || !taskLine.Source.CarriesProvenance() {
		return nil, "", nil
	}
	policy, err := routingPolicyRevision(ctx, tx, taskLine.Provenance.PolicyRevision)
	if err != nil {
		return nil, "", err
	}
	order := routingOrder(policy, taskLine.Role)
	if slices.Contains(order, taskLine.Agent) {
		return nil, "", nil
	}
	return order, fmt.Sprintf("an agent routing policy revision %d orders", policy.Revision), nil
}

// triageCounterpart is the Tier A half of an escalated line's anchor that
// needs the tree: the line's agent must be the escalated counterpart the
// current lineup names for one of bases (counterpartBases), described by
// basis. It is re-read from the lineup at every admission, never trusted from
// the row, so a lineup edit that drops or changes the counterpart refuses a
// line written before it. The other half, the line's own task and its
// triage record, is anchorTaskLine's.
func triageCounterpart(
	tree agenttree.Tree, taskLine domain.TaskLine, bases []string, basis string,
) (domain.LineupSelection, error) {
	lineup, err := tree.ResolveLineup()
	if err != nil {
		return domain.LineupSelection{}, err
	}
	var named []string
	for _, base := range bases {
		counterpart, ok := lineup.EscalatedCounterpart(taskLine.Role, base)
		if !ok {
			continue
		}
		if counterpart.AgentName == taskLine.Agent {
			return counterpart, nil
		}
		named = append(named, counterpart.AgentName)
	}
	if len(named) == 0 {
		return domain.LineupSelection{}, fmt.Errorf("the lineup names no escalated counterpart for %s's %s",
			taskLine.Role, basis)
	}
	// The refusal names neither agent the row holds: a refusal is stored,
	// and only the tree's names are safe to print.
	return domain.LineupSelection{}, fmt.Errorf("it names an agent other than %s, the escalated counterpart of %s",
		strings.Join(named, " or "), basis)
}

// Selection is the selection source that chose the agent and the record an
// admission cites for it. The lineup has no record beyond its revision; a
// card and a task line are cited by their ids.
func (a RoleAgent) Selection() (domain.AgentSelectionSource, domain.Digest) {
	if a.Card != nil {
		return domain.AgentSelectionSourceCard, a.Card.ID
	}
	if a.TaskLine != nil {
		return domain.AgentSelectionSourceTaskLine, a.TaskLine.ID
	}
	return domain.AgentSelectionSourceLineup, ""
}

// resolveAgent joins the named agent to its fragments and to its enrollment
// and identity as the store holds them now.
func resolveAgent(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, name string,
) (RoleAgent, error) {
	source, ok := tree.Agent(name)
	if !ok {
		return RoleAgent{}, fmt.Errorf("the lineup names agent %q, which the tree lacks", name)
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
	return nil
}

// requireUnmarked refuses the generation readGeneration read when the
// credential-integrity probe marked it: credentialed, in part (§5.4 admission
// rule 4), since a marked generation is not a valid one. The caller reads the
// mark in the transaction that read the generation, so the two are one view;
// the mark is not carried on the generation, and re-enrollment appends an
// unmarked successor, which clears this.
func (a *RoleAgent) requireUnmarked(ctx context.Context, tx *store.ReadTx) error {
	return tx.RequireGenerationUnmarked(ctx, a.Enrollment.ID, a.Generation.Ordinal)
}

// RunTaskLine returns the line the run's task holds for the role, once that
// line is held against the command that set it. found is false when the task
// has no line for the role, which leaves the role on the lineup. A cleared
// version is anchored like any other first: it moves the role to the lineup,
// so a row nobody cleared must not do that.
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
	heads, err := tx.TaskLineHeads(ctx, run.TaskID)
	if err != nil {
		return domain.TaskLine{}, false, err
	}
	if line, found = heads[role]; !found {
		return domain.TaskLine{}, false, nil
	}
	if err := anchorTaskLine(ctx, tx, line); err != nil {
		return domain.TaskLine{}, false, fmt.Errorf("task line %s, set by %s %q: %w", line.ID, line.Source, line.SetBy, err)
	}
	if line.Cleared {
		return domain.TaskLine{}, false, nil
	}
	return line, true, nil
}

// anchorTaskLine holds a decoded line against the operator action that set
// it. The store's read proves only that the row is self-consistent: a line's
// id is an unkeyed hash of its own columns, so a row nobody submitted still
// decodes. Choosing an agent chooses which credential runs (§5.8), so the
// record SetBy names must exist, be of the kind Source claims, and
// name this task. Submission lines must be set at task creation and name the
// agent their command recorded for the role; proposal lines must be set at
// the recorded decision's instant; a changed line at its change's. A router
// line must hold to the policy revision it cites (anchorRouterLine). A triage
// line sets only the specifier's first version, cites its own task, and was
// set at the instant of that task's triage record, which must exist and have
// escalated; resolveLineAgent holds its agent to the lineup's escalated
// counterpart (triageCounterpart).
//
// A submission or change record stores what the operator chose, so those arms
// bind the agent. A proposal line rests on a real operator action on this
// task, but its record does not store the lines, so that arm cannot (#1641).
func anchorTaskLine(ctx context.Context, tx *store.ReadTx, line domain.TaskLine) error {
	task, err := tx.GetTask(ctx, line.TaskID)
	if err != nil {
		return err
	}
	// No default: a new source's author decides what record backs it.
	switch line.Source {
	case domain.TaskLineSourceSubmitTask:
		if !line.SetAt.Equal(task.CreatedAt) {
			return fmt.Errorf("it was set at %s, its task was created at %s",
				line.SetAt.Format(time.RFC3339Nano), task.CreatedAt.Format(time.RFC3339Nano))
		}
		submission, err := tx.GetTaskSubmission(ctx, line.SetBy)
		if err != nil {
			return err
		}
		if submission.TaskID != line.TaskID {
			return fmt.Errorf("that command created task %s", submission.TaskID)
		}
		return submittedLineBacked(submission.TaskLines, line)
	case domain.TaskLineSourceCLISubmit:
		if !line.SetAt.Equal(task.CreatedAt) {
			return fmt.Errorf("it was set at %s, its task was created at %s",
				line.SetAt.Format(time.RFC3339Nano), task.CreatedAt.Format(time.RFC3339Nano))
		}
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
		return submittedLineBacked(submission.TaskLines, line)
	case domain.TaskLineSourceProposalStart:
		command, err := tx.GetCommand(ctx, line.SetBy)
		if err != nil {
			return err
		}
		if command.Action != domain.ActionStart && command.Action != domain.ActionStartWithChanges {
			return errors.New("that command is not a proposal start")
		}
		if command.DeviceID == signet.UnattendedInitiatorDeviceID {
			return errors.New("that command is an unattended start, not an operator choice")
		}
		instance, _, err := tx.ProposalForItem(ctx, command.ItemID)
		if err != nil {
			return err
		}
		decision, err := tx.ProposalDecisionForInstance(ctx, instance.ID)
		if err != nil {
			return err
		}
		if decision == nil || decision.CommandID != line.SetBy {
			return errors.New("that command is not the proposal's recorded decision")
		}
		if !decision.DecidedAt.Equal(line.SetAt) {
			return errors.New("the line was not set at the proposal's decision instant")
		}
		taskID, err := tx.ProposalTask(ctx, command.ItemID)
		if err != nil {
			return err
		}
		if taskID != line.TaskID {
			return fmt.Errorf("that proposal starts task %s", taskID)
		}
		return nil
	case domain.TaskLineSourceRouter:
		return anchorRouterLine(ctx, tx, line)
	case domain.TaskLineSourceAlternateAgentMove:
		return anchorAlternateAgentMoveLine(ctx, tx, line)
	case domain.TaskLineSourceTriage:
		if line.Role != domain.RoleSpecifier {
			return fmt.Errorf("a triage line sets only the specifier, not the %s", line.Role)
		}
		if line.SetBy != string(line.TaskID) {
			return errors.New("a triage line cites its own task's triage record, and this one cites another")
		}
		record, found, err := tx.GetTaskTriage(ctx, line.TaskID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("its task has no triage record")
		}
		if !record.Escalated {
			return errors.New("its task's triage record did not escalate")
		}
		// Triage writes the specifier's first version at its record's
		// instant, in the transaction that records it.
		if line.Version != 1 {
			return errors.New("triage sets only the specifier's first version")
		}
		if !line.SetAt.Equal(record.RecordedAt) {
			return errors.New("the line was not set at its task's triage instant")
		}
		return nil
	case domain.TaskLineSourceCLILineChange, domain.TaskLineSourceClientLineChange:
		change, err := tx.GetTaskLineChange(ctx, domain.Digest(line.SetBy))
		if err != nil {
			return err
		}
		if !change.Backs(line) {
			return errors.New("that change names another task, role, agent, version, or instant")
		}
		return nil
	}
	return fmt.Errorf("source %q is not a task line source", line.Source)
}

// anchorRouterLine holds a router line to the routing that wrote it. No
// operator action backs one, so the anchor is the policy: the line names its
// own task's routing, it is its role's first version (a later version that
// re-pins a moved task is an alternate_agent_move line, which anchors to its
// record), and it holds to its policy revision (anchorLinePolicy). The one
// later version is an escalated specifier's second, over its task's triage
// line (plan §5.13, Specifier Triage).
//
// The refusals name no agent, as the other arms don't.
func anchorRouterLine(ctx context.Context, tx *store.ReadTx, line domain.TaskLine) error {
	if line.SetBy != routerSetBy(line.TaskID) {
		return errors.New("it does not name its own task's routing")
	}
	if line.Version != 1 {
		if line.Role != domain.RoleSpecifier || line.Version != 2 || line.PredecessorID == nil {
			return errors.New("the router sets only a role's first version, or an escalated specifier's second")
		}
		// A predecessor is its own task's line (the store's chain check).
		predecessor, err := tx.GetTaskLine(ctx, *line.PredecessorID)
		if err != nil {
			return err
		}
		if predecessor.Source != domain.TaskLineSourceTriage {
			return errors.New("the router sets a specifier's second version only over its triage line")
		}
	}
	return anchorLinePolicy(ctx, tx, line)
}

// anchorAlternateAgentMoveLine holds a moved line to the operator's move that
// set it: the record it cites is a parked move of its own task, made by a
// command whose receipt records that move, and the line was set at the
// record's instant on the record's pool. The record's role moves to the
// record's agent; every line holds to its policy revision (anchorLinePolicy),
// since a move re-pins the task as the router would. Any version may be a
// move: it supersedes whatever the role held.
func anchorAlternateAgentMoveLine(ctx context.Context, tx *store.ReadTx, line domain.TaskLine) error {
	record, err := tx.GetAlternateAgentRetry(ctx, domain.Digest(line.SetBy))
	if err != nil {
		return err
	}
	if record.TaskID != line.TaskID || !record.ParkedMove() {
		return errors.New("its record is not a move of its own task")
	}
	receipt, _, err := tx.GetCommandReceipt(ctx, record.CommandID)
	if err != nil {
		return fmt.Errorf("its record's command: %w", err)
	}
	if move := receipt.ParkedTaskMove; move == nil || move.Retry.ID != record.ID {
		return errors.New("its record's command did not move the task")
	}
	if !line.SetAt.Equal(record.RecordedAt) || line.Provenance.Pool != record.Pool {
		return errors.New("it was not set at its record's instant on its record's pool")
	}
	if line.Role == record.Role && line.Agent != record.Agent {
		return errors.New("its record moved the role to another agent")
	}
	return anchorLinePolicy(ctx, tx, line)
}

// anchorLinePolicy holds a line that carries a provenance to the policy
// revision it cites: the revision exists and was set no later than the line,
// it orders the line's agent for its role, any agent the line passed over is
// in its orders, and a line the drain-first tier chose names the pool it
// drains first. The marks and readings the routing acted on are not
// rechecked, because both change over time: a line routed past a mark that
// has since cleared, or ranked by a reset since passed, is still the routing
// that chose it.
//
// A task whose triage record escalated routes its specifier among
// counterparts the revision need not order (escalatedOrder). For that task
// only, the specifier's line may name an agent the revision does not order,
// which resolveLineAgent then holds to the lineup's counterpart of an agent
// it does (counterpartBases), and any of its lines may cite a passed-over
// agent outside the orders: that one is history and selects nothing.
func anchorLinePolicy(ctx context.Context, tx *store.ReadTx, line domain.TaskLine) error {
	revision := line.Provenance.PolicyRevision
	policy, err := routingPolicyRevision(ctx, tx, revision)
	if err != nil {
		return err
	}
	if policy.SetAt.After(line.SetAt) {
		return fmt.Errorf("routing policy revision %d was set after the line", revision)
	}
	ordered := slices.Contains(routingOrder(policy, line.Role), line.Agent)
	passedOver := line.Provenance.PassedOver
	passedOverOrdered := passedOver == nil || slices.ContainsFunc(policy.Roles, func(role domain.RoutingPolicyRole) bool {
		return slices.Contains(role.Agents, passedOver.Agent)
	})
	escalated := false
	if !ordered || !passedOverOrdered {
		if escalated, err = taskEscalated(ctx, tx, line.TaskID); err != nil {
			return err
		}
	}
	if !ordered && (line.Role != domain.RoleSpecifier || !escalated) {
		return fmt.Errorf("routing policy revision %d does not order the line's agent for its role", revision)
	}
	if !passedOverOrdered && !escalated {
		return fmt.Errorf("routing policy revision %d orders no agent the line passed over", revision)
	}
	if line.Provenance.Tier == domain.TaskLineSelectionDrainFirstAccount &&
		(policy.DrainFirstAccount == nil || *policy.DrainFirstAccount != line.Provenance.Pool) {
		return fmt.Errorf("routing policy revision %d drains no such account first", revision)
	}
	return nil
}

// routingPolicyRevision returns the routing policy revision a line cites.
func routingPolicyRevision(ctx context.Context, tx *store.ReadTx, revision int) (domain.RoutingPolicy, error) {
	policies, err := tx.RoutingPolicyRevisions(ctx)
	if err != nil {
		return domain.RoutingPolicy{}, err
	}
	i := slices.IndexFunc(policies, func(policy domain.RoutingPolicy) bool { return policy.Revision == revision })
	if i < 0 {
		return domain.RoutingPolicy{}, fmt.Errorf("routing policy revision %d was never set", revision)
	}
	return policies[i], nil
}

// submittedLineBacked holds a line to the choices its submission recorded. A
// submission sets only a role's first version, so a later one citing it would
// undo the operator's change. A record from before the field holds no choices,
// so it refuses every line that cites it; `freesided task-line` sets a version
// its own change record backs. The refusal repeats neither agent: the line's
// is operator text a credential can be pasted into, and the refusal is stored.
func submittedLineBacked(recorded []domain.TaskLineChoice, line domain.TaskLine) error {
	if line.Version != 1 {
		return errors.New("a submission sets only a role's first version")
	}
	i := slices.IndexFunc(recorded, func(choice domain.TaskLineChoice) bool { return choice.Role == line.Role })
	if i < 0 {
		return errors.New("that command's record names no agent for this role")
	}
	if recorded[i].Agent != line.Agent {
		return errors.New("that command's record names another agent for this role")
	}
	return nil
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
	promptDigest domain.Digest, at time.Time,
) (domain.AdmissionAgentBinding, error) {
	if err := s.validate(); err != nil {
		return domain.AdmissionAgentBinding{}, err
	}
	resolved, err := resolveAgentAdmission(ctx, st, s, "", role, promptDigest, at)
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
	identity   domain.AuthIdentity
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
	promptDigest domain.Digest, admittedAt time.Time,
) (agentAdmission, error) {
	// refused gains the launch and the agent as each resolves, so a refusal
	// names whichever of them it came after.
	var refused domain.AdmissionRefusal
	refuse := func(check domain.AdmissionCheck, format string, args ...any) (agentAdmission, error) {
		refused.Check = check
		return agentAdmission{}, &AgentAdmissionRefusal{Refusal: refused, err: fmt.Errorf(
			"role %s: %s: %w", role, fmt.Sprintf(format, args...), ErrAgentNotAdmissible)}
	}
	stage, ok := role.Stage()
	if !ok {
		return refuse(domain.AdmissionCheckAgentRole, "it is not a ward role")
	}
	launch, err := selection.Launch(role)
	if err != nil {
		return refuse(domain.AdmissionCheckAgentLaunch, "launch: %v", err)
	}
	refused.LaunchStage, refused.LaunchDigest = &launch.Stage, &launch.Digest

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
		refused.Check = domain.AdmissionCheckAgentResolution
		if errors.Is(err, domain.ErrGenerationIntegrityMarked) {
			refused.Check = domain.AdmissionCheckGenerationIntegrityMarked
		}
		return agentAdmission{}, &AgentAdmissionRefusal{
			Refusal: refused, err: fmt.Errorf("role %s: %w: %w", role, err, ErrAgentNotAdmissible),
		}
	}
	refused.Agent = &agent.Line.AgentName
	resolved, enrollment, generation := agent.Resolved, agent.Enrollment, agent.Generation
	definition := resolved.Definition
	// The line also names the prompt the attempt is about to run.
	if agent.Line.PromptDigest != promptDigest {
		return refuse(domain.AdmissionCheckAgentPrompt, "the lineup selects prompt %s, the attempt runs %s", agent.Line.PromptDigest, promptDigest)
	}
	// Step 3, proved: the adapter's latest conformance covers the launch.
	if err := domain.ValidateAdapterLaunchCoverage(conformance, resolved.Adapter.Digest, launch); err != nil {
		return refuse(domain.AdmissionCheckAgentLaunchCoverage, "%v", err)
	}
	// Step 4, credentialed: a credential that outlives the attempt and an
	// offer still on sale at its deadline. Resolution already refused a
	// disabled identity.
	deadline := admittedAt.Add(selection.AttemptBudget)
	if err := domain.ValidateGenerationExpiryMargin(enrollment, generation, deadline, selection.ExpiryMargin); err != nil {
		return refuse(domain.AdmissionCheckAgentCredentialExpiry, "%v", err)
	}
	if err := domain.ValidateOfferCoversDeadline(resolved.Offer, deadline); err != nil {
		return refuse(domain.AdmissionCheckAgentOfferDeadline, "%v", err)
	}
	for _, authority := range selection.EffectiveEgress {
		if !slices.Contains(resolved.Route.InferenceAuthorities, authority) {
			return refuse(domain.AdmissionCheckAgentEgressAuthority, "provider endpoint %q is outside the route's inference authorities", authority)
		}
	}
	// Step 5, snapshot.
	source, record := agent.Selection()
	passes, err := domain.DeriveAgentLaunchSelection(definition, resolved.Adapter, resolved.Offer)
	if err != nil {
		return refuse(domain.AdmissionCheckAgentSnapshot, "%v", err)
	}
	effort, err := domain.TranslateEffort(resolved.Adapter.ClientKind, definition.Effort)
	if err != nil {
		return refuse(domain.AdmissionCheckAgentSnapshot, "%v", err)
	}
	treatment, err := domain.ComputeTreatmentDigest(
		resolved.Route, resolved.Adapter.Digest, launch.Digest, resolved.Offer, definition.Effort, effort.Effective(),
	)
	if err != nil {
		return refuse(domain.AdmissionCheckAgentSnapshot, "%v", err)
	}
	return agentAdmission{
		binding: domain.AdmissionAgentBinding{
			AgentDigest: definition.Digest, LaunchDigest: launch.Digest, TreatmentDigest: treatment,
			PricingRevision: resolved.Offer.PricingRevision, LineupRevision: selection.LineupRevision,
			EnrollmentID: enrollment.ID, EnrollmentGeneration: generation.Ordinal,
			StoreManifestDigest: generation.StoreManifestDigest,
			EffectiveEgress:     slices.Clone(selection.EffectiveEgress),
			RouteModelID:        passes.RouteModelID, RequestedEffort: passes.RequestedEffort,
			NativeEffort:    passes.NativeEffort,
			SelectionSource: source, SelectionRecordID: record,
			HarnessClient: resolved.Adapter.ClientKind, LineageGroup: resolved.Offer.LineageGroup,
			CostOwner: agent.Identity.CostOwner,
		},
		resolved: resolved, launch: launch, stage: stage,
		enrollment: enrollment, generation: generation, identity: agent.Identity,
	}, nil
}
