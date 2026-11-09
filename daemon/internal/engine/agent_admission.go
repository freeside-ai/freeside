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

// RoleAgent is a role's selection resolved against the store and the tree:
// the names and the prompt the role runs, the enrollment and enabled
// identity the names select, the route the enrollment is enrolled through
// as the tree holds it, the enrollment's catalog, and the store generation
// it would mount now. Which adapter launches the selection depends on the
// launch, so Selection and Adapter are set only once Select has run. No
// agent document is in the path.
type RoleAgent struct {
	// Line is the selection and the prompt the role runs. It is the role's
	// lineup line, except that under a task line, a card, or an escalated
	// counterpart the names are the ones that chose.
	Line domain.LineupSelection
	// TaskLine is the task line that chose the selection; nil when the
	// lineup did.
	TaskLine *domain.TaskLine
	// Card is the alternate-agent record that chose the agent for this one
	// attempt; nil when a line or the lineup did. It takes precedence over
	// TaskLine.
	Card       *domain.AlternateAgentRetry
	Route      domain.RouteFragment
	Identity   domain.AuthIdentity
	Enrollment domain.ClientEnrollment
	Generation domain.EnrollmentGeneration
	// Catalog is the enrollment's discovered model catalog, nil when none was
	// ever recorded, which resolves under the bridge
	// (domain.ResolveAgentSelection).
	Catalog *domain.DiscoveredModelCatalog
	// Selection and Adapter are what Select resolved: zero until it ran.
	Selection domain.AgentSelection
	Adapter   domain.AdapterFragment

	// label is what a refusal may print for the choice: the lineup's text, or
	// the tree's name for an agent a v1 line or a card names. It is nil while
	// the choice is a v2 task line's names, which an operator typed and a
	// credential can be pasted into, until a recorded catalog matched them.
	label *string
}

// Select resolves the selection the agent's names stand for under adapter
// (domain.ResolveAgentSelection): the model and effort must be ones the
// enrollment's catalog reports, or under the bridge ones the client's native
// table produces. A refusal over operator text keeps its sentinel but not
// its words, which quote the names.
func (a RoleAgent) Select(adapter domain.AdapterFragment) (RoleAgent, error) {
	selection, err := domain.ResolveAgentSelection(domain.AgentSelectionInput{
		Names: a.Line.Names, Identity: a.Identity, Enrollment: a.Enrollment, Adapter: adapter, Catalog: a.Catalog,
	})
	if err != nil {
		return RoleAgent{}, hideCause(a.label, "its selection does not resolve against its enrollment", err)
	}
	a.Selection, a.Adapter = selection, adapter
	if a.label == nil && a.Catalog != nil {
		text := a.Line.TaskLineSelection().Text()
		a.label = &text
	}
	return a, nil
}

// describe is what a refusal prints for the choice (label).
func (a RoleAgent) describe() string {
	if a.label != nil {
		return *a.label
	}
	return "the line's selection"
}

// Describe is what a refusal outside the engine may print for the choice:
// the lineup's text, or an operator's selection once a recorded catalog
// matched it (Select), and otherwise only that a line chose. Never print the
// names themselves: a credential can be pasted into them.
func (a RoleAgent) Describe() string {
	return a.describe()
}

// refusalAgent is the agent text a stored refusal records, nil when the
// choice may not be printed.
func (a RoleAgent) refusalAgent() *string {
	if a.label == nil {
		return nil
	}
	text := *a.label
	return &text
}

// hiddenCause is an error whose text is msg alone while it still matches
// what it wraps, for a refusal over operator text: the wrapped error quotes
// the names the text held.
type hiddenCause struct {
	msg string
	err error
}

func (e hiddenCause) Error() string { return e.msg }

func (e hiddenCause) Unwrap() error { return e.err }

// hideCause wraps err with msg, printing err's own text only when label says
// the choice may be printed.
func hideCause(label *string, msg string, err error) error {
	if label != nil {
		return fmt.Errorf("%s: %s: %w", *label, msg, err)
	}
	return hiddenCause{msg: msg, err: err}
}

// CheckTaskLineSelection reports whether a task line's selection for role
// resolves now, the way admission resolves it: the enrollment it names and
// that enrollment's identity as the store holds them, the adapter the role's
// launch shape picks for the enrollment's client and the line's effort, and
// the enrollment's catalog. It reads no tree agent and runs none of
// admission's later checks (the task's anchor, prompt, conformance,
// generation), so a pass says the selection can run, not that an attempt
// will be admitted. A refusal over the line's names keeps its sentinel but
// not its words, as admission's does.
func CheckTaskLineSelection(
	ctx context.Context, st *store.Store, tree agenttree.Tree,
	launch func(domain.RoleName) (domain.LaunchSpec, error), role domain.RoleName,
	selection domain.TaskLineSelection,
) error {
	spec, err := launch(role)
	if err != nil {
		return fmt.Errorf("role %s launch: %w", role, err)
	}
	// The review launch shape waives auxiliary-inference control, as
	// CheckReviewRole picks its adapter.
	var waived []domain.LaunchCapability
	if role == domain.RoleReviewer || role == domain.RoleShadowReviewer {
		waived = append(waived, domain.LaunchCapAuxiliaryInferenceControl)
	}
	return st.Read(ctx, func(tx *store.ReadTx) error {
		agent, err := resolveNames(ctx, tx, tree, selection.Names(), nil)
		if err != nil {
			return err
		}
		agent.Line.Names = selection.Names()
		adapter, err := tree.AdapterForLaunch(agent.Enrollment.HarnessClient, spec, selection.Effort, waived...)
		if err != nil {
			return hideCause(nil, "no one adapter launches its selection", err)
		}
		_, err = agent.Select(adapter)
		return err
	})
}

// ResolveRole runs the admission steps that need no launch (plan §5.4): the
// role's line names an enrollment of an enabled identity, enrolled through a
// route the tree holds, and the enrollment holds an unmarked store
// generation. The selection itself resolves once a launch picks the adapter
// (Select). Stage admission adds the adapter, selection, launch,
// conformance, and deadline checks; the review roles add the selection and
// the credential's expiry in CheckReviewRole.
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

// lineupText is a lineup line's names as a refusal prints them. The lineup
// is the tree's own text.
func lineupText(line domain.LineupSelection) *string {
	text := line.TaskLineSelection().Text()
	return &text
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
	agent, err := resolveNames(ctx, tx, tree, line.Names, lineupText(line))
	if err != nil {
		return RoleAgent{}, err
	}
	agent.Line = line
	if err := agent.readGeneration(ctx, tx); err != nil {
		return RoleAgent{}, err
	}
	return agent, nil
}

// resolveAttemptRole is the one point that selects what a role runs in an
// attempt (plan §5.4). The precedence is the alternate-agent card for this
// one attempt, then the task's line for the role, then the lineup. A card's
// selection is re-gated against the failure it answered (regateCardAgent).
//
// A task line picks the selection only. The prompt stays the one the role's
// lineup line names, so a role with no lineup line is still refused. A line
// that fails any step refuses the attempt: it never falls back to the
// lineup's selection, because the operator chose which credential runs.
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
	taskLine, found, err := RunTaskLine(ctx, tx, tree, runID, role)
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

// taskLineNames is the names a task line selects and what a refusal may
// print for them: a v1 line names a tree agent, which reduces through the
// tree (agenttree.Tree.SelectionOfAgent) and whose name is the tree's own
// once the tree holds it; a v2 line holds its names, which are operator
// text. Refusals name the line by its id, never by what it holds.
func taskLineNames(tree agenttree.Tree, line domain.TaskLine) (domain.AgentSelectionNames, *string, error) {
	if line.Encoding == domain.TaskLineEncodingV1 {
		if _, ok := tree.Agent(line.Agent); !ok {
			return domain.AgentSelectionNames{}, nil, fmt.Errorf("task line %s names an agent the tree lacks", line.ID)
		}
		selection, err := tree.SelectionOfAgent(line.Agent)
		if err != nil {
			return domain.AgentSelectionNames{}, nil, fmt.Errorf("task line %s: %w", line.ID, err)
		}
		name := line.Agent
		return selection.Names(), &name, nil
	}
	if line.Selection == nil {
		return domain.AgentSelectionNames{}, nil, fmt.Errorf("task line %s selects nothing: %w",
			line.ID, domain.ErrInvalidTaskLineSelection)
	}
	return line.Selection.Names(), nil, nil
}

// resolveLineAgent is resolveRoleLine for the selection a task line runs:
// the names a v2 line holds, or those the agent a v1 line names reduces to.
// A triage line, and an escalated specifier's router or move line, must run
// the escalated counterpart the lineup names (escalatedCounterpart). It runs
// without the generation's integrity mark, which the caller reads in the
// same transaction.
func resolveLineAgent(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, role domain.RoleName, taskLine domain.TaskLine,
) (RoleAgent, error) {
	line, err := lineupLine(tree, role)
	if err != nil {
		return RoleAgent{}, err
	}
	names, label, err := taskLineNames(tree, taskLine)
	if err != nil {
		return RoleAgent{}, err
	}
	bases, basis, err := counterpartBases(ctx, tx, tree, line, taskLine)
	if err != nil {
		return RoleAgent{}, fmt.Errorf("task line %s: %w", taskLine.ID, err)
	}
	if bases != nil {
		counterpart, err := escalatedCounterpart(tree, taskLine.Role, names, bases, basis)
		if err != nil {
			return RoleAgent{}, fmt.Errorf("task line %s: %w", taskLine.ID, err)
		}
		// The line runs the counterpart's names, which are the lineup's own
		// text and so printable.
		label = lineupText(counterpart)
	}
	agent, err := resolveNames(ctx, tx, tree, names, label)
	if err != nil {
		return RoleAgent{}, fmt.Errorf("task line %s: %w", taskLine.ID, err)
	}
	line.Names = names
	agent.Line, agent.TaskLine = line, &taskLine
	if err := agent.readGeneration(ctx, tx); err != nil {
		return RoleAgent{}, err
	}
	return agent, nil
}

// lineRuns is agenttree.Tree.LineRuns for one line, for a check that asks
// whether the line runs an agent a policy or record names. A line the tree
// cannot resolve runs no agent.
func lineRuns(tree agenttree.Tree, line domain.TaskLine) func(agent string) bool {
	return func(agent string) bool {
		runs, err := tree.LineRuns(line, agent)
		return err == nil && runs
	}
}

// resolveCardAgent is resolveLineAgent for what a card record chose: the
// prompt stays the lineup line's, the names are the record's selection (or,
// on a v1 record, the ones its tree agent reduces to through
// agenttree.Tree.SelectionOfAgent until #2599), and the rule the record names
// is re-derived for them. It runs without the generation's integrity mark,
// which the caller reads in the same transaction. A record that fails any
// step refuses the attempt, never falling back to the task's line or the
// lineup.
func resolveCardAgent(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, role domain.RoleName, card domain.AlternateAgentRetry,
) (RoleAgent, error) {
	line, err := lineupLine(tree, role)
	if err != nil {
		return RoleAgent{}, err
	}
	names, label, err := cardNames(tree, card)
	if err != nil {
		return RoleAgent{}, err
	}
	agent, err := resolveNames(ctx, tx, tree, names, label)
	if err != nil {
		return RoleAgent{}, fmt.Errorf("card record %s: %w", card.ID, err)
	}
	if err := agent.readGeneration(ctx, tx); err != nil {
		return RoleAgent{}, err
	}
	if err := regateCardAgent(ctx, tx, card, agent); err != nil {
		return RoleAgent{}, fmt.Errorf("card record %s: %w", card.ID, err)
	}
	line.Names = names
	agent.Line, agent.Card = line, &card
	return agent, nil
}

// cardNames is the names a card record chose and what a refusal may print
// for them, as taskLineNames is for a line: a v2 record's selection, which
// the operator chose and so is not printed until a recorded catalog matches
// it, or the names a v1 record's tree agent reduces to, under its name.
func cardNames(tree agenttree.Tree, card domain.AlternateAgentRetry) (domain.AgentSelectionNames, *string, error) {
	if card.Selection != nil {
		return card.Selection.Names(), nil, nil
	}
	if _, ok := tree.Agent(card.Agent); !ok {
		return domain.AgentSelectionNames{}, nil, fmt.Errorf("card record %s names an agent the tree lacks", card.ID)
	}
	selection, err := tree.SelectionOfAgent(card.Agent)
	if err != nil {
		return domain.AgentSelectionNames{}, nil, fmt.Errorf("card record %s: %w", card.ID, err)
	}
	name := card.Agent
	return selection.Names(), &name, nil
}

// counterpartBases returns the enrollments whose escalated counterpart a
// task line must run, and a description of them that is safe to print, or
// nil when the line need not run a counterpart. A triage line runs the
// counterpart of the specifier's lineup enrollment. A specifier router or
// move line that its cited revision does not order (policyOrdersLine) runs
// the counterpart of an enrollment that revision orders for the specifier:
// the router routes an escalated specifier among those (escalatedOrder).
// anchorLinePolicy admits such a line only for a task whose triage record
// escalated. A v2 record's move line needs no counterpart here when the
// revision orders its enrollment for the specifier (roleOrder): a move
// re-pins the specifier on the record's enrollment, and its anchor holds the
// line to that enrollment's standard or escalated lineup tuple
// (anchorAlternateAgentMoveLine). A v1 record's line keeps the counterpart
// check.
func counterpartBases(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, line domain.LineupSelection, taskLine domain.TaskLine,
) ([]string, string, error) {
	if taskLine.Source == domain.TaskLineSourceTriage {
		return []string{line.Names.Enrollment}, "lineup enrollment " + line.Names.Enrollment, nil
	}
	if taskLine.Role != domain.RoleSpecifier || !taskLine.Source.CarriesProvenance() {
		return nil, "", nil
	}
	policy, err := routingPolicyRevision(ctx, tx, taskLine.Provenance.PolicyRevision)
	if err != nil {
		return nil, "", err
	}
	if taskLine.Source == domain.TaskLineSourceAlternateAgentMove {
		record, err := tx.GetAlternateAgentRetry(ctx, domain.Digest(taskLine.SetBy))
		if err != nil {
			return nil, "", err
		}
		if record.Selection != nil {
			enrollment, err := lineEnrollment(tree, taskLine)
			if err != nil {
				return nil, "", err
			}
			if slices.Contains(roleOrder(tree, policy, taskLine.Role), enrollment) {
				return nil, "", nil
			}
		}
	}
	if policyOrdersLine(tree, policy, taskLine, false) {
		return nil, "", nil
	}
	// Not nil even when the order resolves to none: the line needs a
	// counterpart, and escalatedCounterpart refuses an empty set.
	enrollments := []string{}
	for _, enrollment := range roleOrder(tree, policy, taskLine.Role) {
		enrollments = append(enrollments, string(enrollment))
	}
	return enrollments, fmt.Sprintf("an enrollment routing policy revision %d orders", policy.Revision), nil
}

// lineEnrollment is the enrollment a line runs: a v2 line's selection's, or
// the one a v1 line's tree agent reduces to.
func lineEnrollment(tree agenttree.Tree, line domain.TaskLine) (domain.ClientEnrollmentID, error) {
	if line.Selection != nil {
		return line.Selection.EnrollmentID, nil
	}
	selection, err := tree.SelectionOfAgent(line.Agent)
	if err != nil {
		return "", fmt.Errorf("task line %s: %w", line.ID, err)
	}
	return selection.EnrollmentID, nil
}

// escalatedCounterpart is the Tier A half of an escalated line's anchor that
// needs the tree: the line's names must be the escalated counterpart the
// current lineup names for one of bases (counterpartBases), described by
// basis. It is re-read from the lineup at every admission, never trusted
// from the row, so a lineup edit that drops or changes the counterpart
// refuses a line written before it. The other half, the line's own task and
// its triage record, is anchorTaskLine's.
func escalatedCounterpart(
	tree agenttree.Tree, role domain.RoleName, names domain.AgentSelectionNames, bases []string, basis string,
) (domain.LineupSelection, error) {
	lineup, err := tree.ResolveLineup()
	if err != nil {
		return domain.LineupSelection{}, err
	}
	var named []string
	for _, base := range bases {
		counterpart, ok := lineup.EscalatedCounterpart(role, base)
		if !ok {
			continue
		}
		if counterpart.Names == names {
			return counterpart, nil
		}
		named = append(named, *lineupText(counterpart))
	}
	if len(named) == 0 {
		return domain.LineupSelection{}, fmt.Errorf("the lineup names no escalated counterpart for %s's %s",
			role, basis)
	}
	// The refusal names only the lineup's text: the row's is operator text,
	// and a refusal is stored.
	return domain.LineupSelection{}, fmt.Errorf("it selects other than %s, the escalated counterpart of %s",
		strings.Join(named, " or "), basis)
}

// SelectionSource is the selection source that chose the agent and the
// record an admission cites for it. The lineup has no record beyond its revision; a
// card and a task line are cited by their ids.
func (a RoleAgent) SelectionSource() (domain.AgentSelectionSource, domain.Digest) {
	if a.Card != nil {
		return domain.AgentSelectionSourceCard, a.Card.ID
	}
	if a.TaskLine != nil {
		return domain.AgentSelectionSourceTaskLine, a.TaskLine.ID
	}
	return domain.AgentSelectionSourceLineup, ""
}

// resolveNames joins the names to the enrollment they select and its
// identity as the store holds them now, the route that enrollment is
// enrolled through as the tree holds it, and the enrollment's catalog. The
// identity must be enabled and the enrollment bound to it. label is what a
// refusal may print for the names (RoleAgent.label).
func resolveNames(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, names domain.AgentSelectionNames, label *string,
) (RoleAgent, error) {
	agent := RoleAgent{label: label}
	var err error
	if agent.Enrollment, err = tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(names.Enrollment)); err != nil {
		return RoleAgent{}, hideCause(label, "its enrollment cannot be read", err)
	}
	if agent.Identity, err = tx.GetAuthIdentity(ctx, agent.Enrollment.AuthIdentityID); err != nil {
		return RoleAgent{}, fmt.Errorf("auth identity %s: %w", agent.Enrollment.AuthIdentityID, err)
	}
	if err := domain.ValidateEnrollmentIdentityBinding(agent.Identity, agent.Enrollment); err != nil {
		return RoleAgent{}, fmt.Errorf("enrollment %s: %w", agent.Enrollment.ID, err)
	}
	if !agent.Identity.Enabled {
		return RoleAgent{}, fmt.Errorf("enrollment %s: identity %s is disabled: %w",
			agent.Enrollment.ID, agent.Identity.ID, domain.ErrAgentJoinInvalid)
	}
	route, ok := tree.Route(agent.Enrollment.Route)
	if !ok {
		return RoleAgent{}, fmt.Errorf("enrollment %s is enrolled through route %q, which the tree lacks: %w",
			agent.Enrollment.ID, agent.Enrollment.Route, domain.ErrAgentJoinInvalid)
	}
	agent.Route = route
	if agent.Catalog, err = readCatalog(ctx, tx, agent.Enrollment.ID); err != nil {
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
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, runID domain.RunID, role domain.RoleName,
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
	if err := anchorTaskLine(ctx, tx, tree, line); err != nil {
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
// bind the agent or selection. The router and move arms compare the line's
// selection with the policy's orders and the tree's lineup (policyOrdersLine),
// and the move arm the line's agent with a record that names one
// (agenttree.Tree.LineRuns); with an empty tree a line of either source
// refuses, except a v2 move record's line for its own role under a revision
// that orders enrollments, whose anchor then reads nothing from the tree, and
// every other line anchors without one. A proposal line
// rests on a real operator action on this task, but its record does not store
// the lines, so that arm cannot (#1641).
func anchorTaskLine(ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, line domain.TaskLine) error {
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
		return anchorRouterLine(ctx, tx, tree, line)
	case domain.TaskLineSourceAlternateAgentMove:
		return anchorAlternateAgentMoveLine(ctx, tx, tree, line)
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
			return errors.New("that change names another task, role, selection, version, or instant")
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
func anchorRouterLine(ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, line domain.TaskLine) error {
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
	return anchorLinePolicy(ctx, tx, tree, line)
}

// anchorAlternateAgentMoveLine holds a moved line to the operator's move that
// set it: the record it cites is a parked move of its own task, made by a
// command whose receipt records that move, and the line was set at the
// record's instant on the record's pool. Under a v2 record the line holds a
// selection on the record's enrollment: the record's selection for the
// record's role, and for every other role its lineup model and effort, or
// for an escalated task's specifier the escalated line's (moveLineTuple);
// it holds to its policy revision as a move (anchorMovePolicy). Under a v1
// record the record's role runs the record's agent and every line holds to
// its policy as the router's would (anchorLinePolicy). Any version may be a
// move: it supersedes whatever the role held.
func anchorAlternateAgentMoveLine(ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, line domain.TaskLine) error {
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
	if record.Selection == nil {
		if line.Role == record.Role {
			runs, err := tree.LineRuns(line, record.Agent)
			if err != nil {
				return err
			}
			if !runs {
				return errors.New("its record moved the role to another agent")
			}
		}
		return anchorLinePolicy(ctx, tx, tree, line)
	}
	if line.Selection == nil || line.Selection.EnrollmentID != record.Selection.EnrollmentID {
		return errors.New("it selects another enrollment than its record moved the task to")
	}
	if line.Role == record.Role {
		if *line.Selection != *record.Selection {
			return errors.New("its record moved the role to another selection")
		}
	} else if err := moveLineTuple(ctx, tx, tree, line); err != nil {
		return err
	}
	return anchorMovePolicy(ctx, tx, tree, line)
}

// moveLineTuple holds a moved line of a role other than its record's to the
// model and effort a move re-pins that role to on the line's enrollment: the
// role's lineup line's, or for the specifier of a task whose triage record
// escalated, the escalated line the lineup names for that enrollment. It is
// re-read from the lineup at every admission, so a lineup edit refuses a
// line written before it.
func moveLineTuple(ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, line domain.TaskLine) error {
	lineup, err := tree.ResolveLineup()
	if err != nil {
		return err
	}
	enrollment := string(line.Selection.EnrollmentID)
	runs := func(lineupLine domain.LineupSelection) bool {
		return lineupLine.Names.Model == line.Selection.ModelID && lineupLine.Names.Effort == line.Selection.Effort
	}
	if standard, ok := lineup.Line(line.Role); ok && runs(standard) {
		return nil
	}
	if counterpart, ok := lineup.EscalatedCounterpart(line.Role, enrollment); ok && runs(counterpart) {
		escalated, err := taskEscalated(ctx, tx, line.TaskID)
		if err != nil {
			return err
		}
		if escalated && line.Role == domain.RoleSpecifier {
			return nil
		}
	}
	return errors.New("it selects other than its role's lineup model and effort on its record's enrollment")
}

// anchorMovePolicy holds a move line under a v2 record to the policy
// revision it cites, as anchorLinePolicy holds a router line: the revision
// exists and was set no later than the line, it orders the line's
// enrollment for its role (roleOrder), the line was chosen at the
// policy-order tier a move writes, and what the line passed over is in its
// orders (policyOrdersPassedOver) unless the task escalated (that one is
// history and selects nothing). An escalated specifier needs no exception: a
// move re-pins it on the record's enrollment, which the revision orders for
// it.
func anchorMovePolicy(ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, line domain.TaskLine) error {
	revision := line.Provenance.PolicyRevision
	policy, err := routingPolicyRevision(ctx, tx, revision)
	if err != nil {
		return err
	}
	if policy.SetAt.After(line.SetAt) {
		return fmt.Errorf("routing policy revision %d was set after the line", revision)
	}
	if !slices.Contains(roleOrder(tree, policy, line.Role), line.Selection.EnrollmentID) {
		return fmt.Errorf("routing policy revision %d does not order the line's enrollment for its role", revision)
	}
	if line.Provenance.Tier != domain.TaskLineSelectionPolicyOrder {
		return fmt.Errorf("a move routes at the %s tier", domain.TaskLineSelectionPolicyOrder)
	}
	if policyOrdersPassedOver(tree, policy, line.Provenance.PassedOver) {
		return nil
	}
	escalated, err := taskEscalated(ctx, tx, line.TaskID)
	if err != nil {
		return err
	}
	if !escalated {
		return fmt.Errorf("routing policy revision %d orders nothing the line passed over", revision)
	}
	return nil
}

// policyOrdersPassedOver reports whether the revision orders what a line's
// passed-over mark names for some role, or the line passed over nothing. A
// mark names an enrollment (roleOrder), or, when written before routing
// policy ordered enrollments (#2736), an agent the revision names in the
// agents form.
func policyOrdersPassedOver(
	tree agenttree.Tree, policy domain.RoutingPolicy, passedOver *domain.TaskLinePassedOverMark,
) bool {
	return passedOver == nil || slices.ContainsFunc(policy.Roles, func(role domain.RoutingPolicyRole) bool {
		if passedOver.Enrollment != "" {
			return slices.Contains(roleOrder(tree, policy, role.Role), passedOver.Enrollment)
		}
		return slices.Contains(role.Agents, passedOver.Agent)
	})
}

// anchorLinePolicy holds a line that carries a provenance to the policy
// revision it cites: the revision exists and was set no later than the line,
// it orders what the line runs for its role (policyOrdersLine), what the line
// passed over is in its orders (policyOrdersPassedOver), and a line the
// drain-first tier chose names
// the pool it drains first. The marks and readings the routing acted on are
// not rechecked, because both change over time: a line routed past a mark
// that has since cleared, or ranked by a reset since passed, is still the
// routing that chose it. The lineup's model and effort are the current
// tree's, read at every admission: a lineup edit that changes a role's model
// refuses that role's routed lines, as it would a lineup that no longer
// resolves.
//
// A task whose triage record escalated routes its specifier among the
// escalated counterparts of its order (escalatedOrder). For that task only,
// the specifier's line may run an ordered enrollment's counterpart, which
// resolveLineAgent holds to the lineup's counterpart (counterpartBases), and
// any of its lines may cite a passed-over candidate outside the orders: that
// one is history and selects nothing.
func anchorLinePolicy(ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, line domain.TaskLine) error {
	revision := line.Provenance.PolicyRevision
	policy, err := routingPolicyRevision(ctx, tx, revision)
	if err != nil {
		return err
	}
	if policy.SetAt.After(line.SetAt) {
		return fmt.Errorf("routing policy revision %d was set after the line", revision)
	}
	ordered := policyOrdersLine(tree, policy, line, false)
	passedOverOrdered := policyOrdersPassedOver(tree, policy, line.Provenance.PassedOver)
	escalated := false
	if !ordered || !passedOverOrdered {
		if escalated, err = taskEscalated(ctx, tx, line.TaskID); err != nil {
			return err
		}
	}
	if !ordered && line.Role == domain.RoleSpecifier && escalated {
		ordered = policyOrdersLine(tree, policy, line, true)
	}
	if !ordered {
		return fmt.Errorf("routing policy revision %d does not order the line's selection for its role", revision)
	}
	if !passedOverOrdered && !escalated {
		return fmt.Errorf("routing policy revision %d orders nothing the line passed over", revision)
	}
	if line.Provenance.Tier == domain.TaskLineSelectionDrainFirstAccount &&
		(policy.DrainFirstAccount == nil || *policy.DrainFirstAccount != line.Provenance.Pool) {
		return fmt.Errorf("routing policy revision %d drains no such account first", revision)
	}
	return nil
}

// policyOrdersLine reports whether the revision orders what the line runs
// for its role: the line's enrollment is in the role's order (roleOrder), and
// its model and effort are the role's lineup line's or, when escalated, that
// enrollment's escalated counterpart's. The line's selection is the one it
// holds, or for a v1 line its agent's in tree (lineSelection).
//
// A role in the agents form, from a revision stored before routing policy
// ordered enrollments (#2736), also keeps the rule a line routed under it
// was written to: the line runs one of its agents (agenttree.Tree.LineRuns),
// or, when escalated, it is any specifier line while some ordered agent
// still resolves in tree (roleOrder), and resolveLineAgent holds it to such
// an agent's counterpart (counterpartBases). Otherwise the line is held as
// above, through the enrollments its agents reduce to, which is what the
// router writes under such a revision. With an empty tree no agents-form
// line is ordered.
func policyOrdersLine(tree agenttree.Tree, policy domain.RoutingPolicy, line domain.TaskLine, escalated bool) bool {
	ordered, ok := policyRole(policy, line.Role)
	if !ok || line.Cleared {
		return false
	}
	if ordered.AgentsForm() && (escalated && len(roleOrder(tree, policy, line.Role)) > 0 ||
		slices.ContainsFunc(ordered.Agents, lineRuns(tree, line))) {
		return true
	}
	selection, err := lineSelection(tree, line)
	if err != nil || !slices.Contains(roleOrder(tree, policy, line.Role), selection.EnrollmentID) {
		return false
	}
	lineup, err := tree.ResolveLineup()
	if err != nil {
		return false
	}
	want, bound := lineup.Line(line.Role)
	if escalated {
		want, bound = lineup.EscalatedCounterpart(line.Role, string(selection.EnrollmentID))
	}
	if !bound {
		return false
	}
	return selection.ModelID == want.Names.Model && selection.Effort == want.Names.Effort
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
// its own change record backs. A v1 line matches a choice that names its
// agent and a v2 line one that holds its selection; a record of the other
// encoding never backs it. The refusal repeats neither choice: the line's is
// operator text a credential can be pasted into, and the refusal is stored.
func submittedLineBacked(recorded []domain.TaskLineChoice, line domain.TaskLine) error {
	if line.Version != 1 {
		return errors.New("a submission sets only a role's first version")
	}
	i := slices.IndexFunc(recorded, func(choice domain.TaskLineChoice) bool { return choice.Role == line.Role })
	if i < 0 {
		return errors.New("that command's record names no agent for this role")
	}
	if !recorded[i].Same(line.Choice()) {
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
	selection  domain.AgentSelection
	adapter    domain.AdapterFragment
	route      domain.RouteFragment
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
		adapter     domain.AdapterFragment
		conformance domain.AdapterConformance
	)
	// One read transaction, so the task's line, the enrollment, its identity,
	// its current generation, its catalog, and the conformance record are one
	// consistent view.
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
		// The adapter is the one the stage launch shape picks for the
		// enrollment's client and the selection's effort.
		if adapter, err = selection.Tree.AdapterForLaunch(
			agent.Enrollment.HarnessClient, launch, agent.Line.Names.Effort,
		); err != nil {
			return hideCause(agent.label, "no one adapter launches its selection", err)
		}
		var found bool
		if conformance, found, err = tx.LatestAdapterConformance(ctx, adapter.Digest); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("adapter %s has no conformance record", adapter.Digest)
		}
		return nil
	})
	if err != nil {
		// Wrapped, not flattened like the other refusals: a marked
		// generation's refusal is typed, and a caller reads the mark off it.
		// The text is what refuse would have printed.
		refused.Check = domain.AdmissionCheckAgentResolution
		// A refusal after the line resolved, such as no one adapter for its
		// launch, names what it may print; one before names nothing.
		refused.Agent = agent.refusalAgent()
		if errors.Is(err, domain.ErrGenerationIntegrityMarked) {
			refused.Check = domain.AdmissionCheckGenerationIntegrityMarked
		}
		return agentAdmission{}, &AgentAdmissionRefusal{
			Refusal: refused, err: fmt.Errorf("role %s: %w: %w", role, err, ErrAgentNotAdmissible),
		}
	}
	refused.Agent = agent.refusalAgent()
	enrollment, generation := agent.Enrollment, agent.Generation
	// The line also names the prompt the attempt is about to run.
	if agent.Line.PromptDigest != promptDigest {
		return refuse(domain.AdmissionCheckAgentPrompt, "the lineup selects prompt %s, the attempt runs %s", agent.Line.PromptDigest, promptDigest)
	}
	// Step 3, proved: the adapter's latest conformance covers the launch.
	if err := domain.ValidateAdapterLaunchCoverage(conformance, adapter.Digest, launch); err != nil {
		return refuse(domain.AdmissionCheckAgentLaunchCoverage, "%v", err)
	}
	// Step 1 again, by selection: the names resolve against the enrollment's
	// catalog under the adapter. It runs after the prompt and coverage checks
	// so their refusals keep their own check.
	if agent, err = agent.Select(adapter); err != nil {
		return refuse(domain.AdmissionCheckAgentResolution, "%v", err)
	}
	refused.Agent = agent.refusalAgent()
	// Step 4, credentialed: a credential that outlives the attempt. Resolution
	// already refused a disabled identity. A selection names no offer, so no
	// offer deadline is checked.
	deadline := admittedAt.Add(selection.AttemptBudget)
	if err := domain.ValidateGenerationExpiryMargin(enrollment, generation, deadline, selection.ExpiryMargin); err != nil {
		return refuse(domain.AdmissionCheckAgentCredentialExpiry, "%v", err)
	}
	for _, authority := range selection.EffectiveEgress {
		if !slices.Contains(agent.Route.InferenceAuthorities, authority) {
			return refuse(domain.AdmissionCheckAgentEgressAuthority, "provider endpoint %q is outside the route's inference authorities", authority)
		}
	}
	// Step 5, snapshot.
	binding, err := selectionBinding(agent, launch, selection.LineupRevision, selection.EffectiveEgress)
	if err != nil {
		return refuse(domain.AdmissionCheckAgentSnapshot, "%v", err)
	}
	return agentAdmission{
		binding: binding, selection: agent.Selection, adapter: agent.Adapter, route: agent.Route,
		launch: launch, stage: stage,
		enrollment: enrollment, generation: generation, identity: agent.Identity,
	}, nil
}

// selectionBinding is the binding an admission under the agent's resolved
// selection records (domain.AdmissionAgentBinding.SelectionAdmitted): the
// selection digest as the agent digest, the treatment over the selection,
// the model and native effort the launch passes, and the lineage group the
// client and model derive. It records no offer pricing revision and no
// Freeside effort level, because a selection names neither.
func selectionBinding(
	agent RoleAgent, launch domain.LaunchSpec, lineupRevision domain.Digest, egress []string,
) (domain.AdmissionAgentBinding, error) {
	selection, adapter := agent.Selection, agent.Adapter
	treatment, err := domain.SelectionTreatmentDigest(agent.Route, adapter, launch.Digest, selection)
	if err != nil {
		return domain.AdmissionAgentBinding{}, err
	}
	model, nativeEffort := selection.LaunchValues(adapter.ClientKind)
	source, record := agent.SelectionSource()
	return domain.AdmissionAgentBinding{
		AgentDigest: selection.Digest, LaunchDigest: launch.Digest, TreatmentDigest: treatment,
		LineupRevision: lineupRevision,
		EnrollmentID:   agent.Enrollment.ID, EnrollmentGeneration: agent.Generation.Ordinal,
		StoreManifestDigest: agent.Generation.StoreManifestDigest,
		EffectiveEgress:     slices.Clone(egress),
		RouteModelID:        model, NativeEffort: nativeEffort,
		SelectionSource: source, SelectionRecordID: record,
		SelectionDigest: selection.Digest, SelectionModelID: selection.ModelID, SelectionEffort: selection.Effort,
		HarnessClient: adapter.ClientKind, LineageGroup: domain.LineageGroupOf(adapter.ClientKind, selection.ModelID),
		CostOwner: agent.Identity.CostOwner,
	}, nil
}

// readCatalog reads the enrollment's discovered model catalog, nil when none
// was ever recorded.
func readCatalog(
	ctx context.Context, tx *store.ReadTx, id domain.ClientEnrollmentID,
) (*domain.DiscoveredModelCatalog, error) {
	catalog, err := tx.GetDiscoveredModelCatalog(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &catalog, nil
}
