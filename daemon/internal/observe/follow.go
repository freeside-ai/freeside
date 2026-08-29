// Package observe renders the operator-facing follow view of one run's
// observation projection (issue #409; contract in issue #394, plan §8). It is
// a pure consumer of the run-observation aggregate: it classifies, formats,
// and paces, and it decides nothing the workflow reads back.
//
// Containment is structural, not a runtime check. The run state this package
// can reach is the observation aggregate plus observedb's bounded lineage,
// admission, and AttentionItem identity projections. None can express a
// writer's stdout, stderr, filesystem, transcript, attention reason, evidence,
// or claims (#394). Nothing here can name a file, start a process, or open a
// socket. containment_test.go pins that import boundary and states where a
// mechanical proof stops and reading the observedb surface takes over.
package observe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/observedb"
)

// DefaultInterval paces the follow loop's reads. The engine refreshes an
// unchanged observation every 10 seconds, so a one-second read is fast enough
// that no observation state is skipped and slow enough to stay negligible
// beside the daemon's own reconcile cadence.
const DefaultInterval = time.Second

// ErrNoObservedTimeline reports that the selected run has nothing observed at
// all. It is a refusal, not an empty display: a mistyped run id would
// otherwise follow silently forever.
var ErrNoObservedTimeline = errors.New("run has no observed timeline")

// Observer is the read surface Follow consumes: one run's re-validated
// observation aggregate. Narrowing the dependency to this single method is
// deliberate; it is what keeps the rest of the daemon's persistence, and
// every driver, out of this package's reach.
type Observer interface {
	ObserveRun(ctx context.Context, runID domain.RunID) (domain.RunObservation, error)
}

type authenticatedObserver interface {
	ObserveConclusion(
		ctx context.Context, runID domain.RunID,
	) (domain.RunObservation, domain.RunConclusion, error)
}

// Config selects the run and paces the follow loop.
type Config struct {
	RunID domain.RunID
	// Interval is the read cadence; zero takes DefaultInterval.
	Interval time.Duration
	// Window is the liveness freshness bound passed to
	// domain.DeriveInvocationLiveness; zero takes the contract default.
	Window time.Duration
	// Once emits one snapshot and returns instead of following.
	Once bool
	// Now is the reader's clock, injectable for tests; zero takes UTC wall
	// time. Liveness and elapsed time compare the daemon's recorded instants
	// against it, so a reader clock far from the daemon's reads as an
	// observation gap rather than a confident verdict.
	Now func() time.Time
}

// Follow emits the run's observed timeline to out and keeps following it
// until the run reaches a final outcome, the context is canceled, or (with
// Once) the first snapshot is emitted.
//
// The loop exits on a final outcome only once a read adds nothing new. That
// settling read absorbs the interleaving within one reconcile pass, where the
// outcome and a sibling milestone commit in separate transactions
// milliseconds apart (the publication lane records publication_ready before
// terminal_recorded). It is explicitly not a durability guarantee: a daemon
// that crashes between those two transactions can append the sibling on a
// recovery pass minutes or hours later, long after this command has printed
// the decided outcome and returned. Blocking on that would make an operator
// wait out an arbitrary outage for a run whose result they already have,
// which is the worse trade, and it costs nothing: the timeline is durable, so
// a later `-once` shows whatever landed after.
//
// A completed execution is not a final outcome: import and publication still
// decide the run, and an attended daemon holds it deliberately, which the
// status block states.
func Follow(ctx context.Context, obs Observer, out io.Writer, cfg Config) error {
	if cfg.RunID == "" {
		return fmt.Errorf("follow: run id: %w", domain.ErrEmptyID)
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	window := cfg.Window
	if window <= 0 {
		window = domain.DefaultObservationFreshnessWindow
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	f := newFollower(out, window)
	started := false
	var (
		last            Conclusion
		lastObservation domain.RunObservation
	)
	for {
		observation, conclusion, err := observeConclusion(ctx, obs, cfg.RunID)
		if err != nil {
			// An interrupt that lands inside a read is the same ordinary end
			// as one that lands between reads, and ends the same way: a
			// closing status block dated to the interrupt, then the outcome
			// so far. Only a real read failure is a failure. The read that
			// just failed produced nothing, so the block reads the last
			// observation this follow did see against the current clock.
			if started && ctx.Err() != nil {
				if err := f.printStatusIfStale(lastObservation, now()); err != nil {
					return err
				}
				return f.emitOutcome(last)
			}
			return fmt.Errorf("follow %q: %w", cfg.RunID, err)
		}
		lastObservation = observation
		if !started {
			if !observed(observation) {
				return fmt.Errorf("follow %q: %w", cfg.RunID, ErrNoObservedTimeline)
			}
			if err := f.line("run " + identifier(string(cfg.RunID))); err != nil {
				return err
			}
			started = true
		}
		at := now()
		changed, err := f.emit(observation, at)
		if err != nil {
			return err
		}
		last = conclusion
		done := cfg.Once || (conclusion.Final && !changed)
		if !done {
			select {
			case <-ctx.Done():
				// An interrupted follow is an ordinary end for an operator
				// watching a run: the outcome so far is stated and the
				// command succeeds. Resample the clock, because the wait it
				// just left can be an entire interval long: the instant
				// captured before it would date the closing status block to
				// before the operator pressed anything, understate elapsed
				// time, and make the block compare equal to the one already
				// on screen so nothing final printed at all. The observation
				// stays as last read, which is honest: an old observation
				// against a fresh clock is what derives an observation gap.
				done, at = true, now()
			case <-time.After(interval):
			}
		}
		if !done {
			continue
		}
		// The last thing an exiting follow shows is a current status block,
		// so elapsed time and the last observation are never left reading
		// from an earlier state change.
		if err := f.printStatusIfStale(observation, at); err != nil {
			return err
		}
		return f.emitOutcome(conclusion)
	}
}

func observeConclusion(
	ctx context.Context, obs Observer, runID domain.RunID,
) (domain.RunObservation, Conclusion, error) {
	if authenticated, ok := obs.(authenticatedObserver); ok {
		return authenticated.ObserveConclusion(ctx, runID)
	}
	observation, err := obs.ObserveRun(ctx, runID)
	if err != nil {
		return domain.RunObservation{}, Conclusion{}, err
	}
	return observation, Conclude(observation), nil
}

// observed reports whether the daemon has recorded anything at all about the
// run.
func observed(o domain.RunObservation) bool {
	return len(o.Milestones) > 0 || len(o.Invocations) > 0 || o.Hold != nil
}

// follower holds what has already been emitted, so a read that repeats the
// durable timeline (every read does) prints only what is new.
type follower struct {
	out    io.Writer
	window time.Duration
	// seen keys milestones by the store's own uniqueness key (kind plus
	// invocation), so an emitted milestone is never reprinted even if the
	// timeline's order were ever to change under us.
	seen map[string]bool
	// statusKey is the observed state last printed, so the status block
	// reprints on a real change rather than on every advancing clock;
	// lastBlock is the exact block on screen, so an exiting follow repeats it
	// only when it would read differently.
	statusKey  string
	lastBlock  string
	haveStatus bool
}

func newFollower(out io.Writer, window time.Duration) *follower {
	return &follower{out: out, window: window, seen: map[string]bool{}}
}

func (f *follower) line(text string) error {
	if _, err := fmt.Fprintln(f.out, text); err != nil {
		return fmt.Errorf("follow: write output: %w", err)
	}
	return nil
}

// emit prints milestones not yet seen and the status block when its observed
// state changed, reporting whether anything was printed.
func (f *follower) emit(o domain.RunObservation, asOf time.Time) (bool, error) {
	changed := false
	for _, m := range o.Milestones {
		key := milestoneKey(m)
		if f.seen[key] {
			continue
		}
		f.seen[key] = true
		if err := f.line(formatMilestone(m)); err != nil {
			return changed, err
		}
		changed = true
	}
	_, state := statusBlock(o, asOf, f.window)
	if f.haveStatus && strings.Join(state, "\n") == f.statusKey {
		return changed, nil
	}
	if err := f.printStatus(o, asOf); err != nil {
		return changed, err
	}
	return true, nil
}

// printStatusIfStale is the exiting follow's last word: it repeats the status
// block only when the block on screen no longer reads the same, so an
// advanced elapsed clock is shown while an unchanged one is not duplicated.
func (f *follower) printStatusIfStale(o domain.RunObservation, asOf time.Time) error {
	if f.haveStatus && f.block(o, asOf) == f.lastBlock {
		return nil
	}
	return f.printStatus(o, asOf)
}

// printStatus prints the status block unconditionally and remembers both its
// observed state (which drives the follow loop's change detection) and the
// exact block printed (which drives the exiting repeat).
func (f *follower) printStatus(o domain.RunObservation, asOf time.Time) error {
	header, state := statusBlock(o, asOf, f.window)
	lines := append([]string{header}, state...)
	f.haveStatus = true
	f.statusKey, f.lastBlock = strings.Join(state, "\n"), strings.Join(lines, "\n")
	for _, text := range lines {
		if err := f.line(text); err != nil {
			return err
		}
	}
	return nil
}

// block is the exact text printStatus would print, used to decide whether the
// exiting repeat would show the operator anything new.
func (f *follower) block(o domain.RunObservation, asOf time.Time) string {
	header, state := statusBlock(o, asOf, f.window)
	return strings.Join(append([]string{header}, state...), "\n")
}

func (f *follower) emitOutcome(c Conclusion) error {
	text := "outcome  " + string(c.Outcome)
	if c.Reason != nil {
		text += "  reason=" + string(*c.Reason)
	}
	if c.Terminal != nil {
		text += "  terminal=" + string(*c.Terminal)
	}
	return f.line(text)
}

// milestoneKey mirrors the store's per-(run, kind, invocation) milestone
// identity; the run is the aggregate's own, so it is not repeated here.
func milestoneKey(m domain.RunMilestone) string {
	invocation := ""
	if m.InvocationID != nil {
		invocation = string(*m.InvocationID)
	}
	return string(m.Kind) + "\x00" + invocation
}

// formatMilestone renders one timeline entry. Detail fields are rendered by
// presence rather than by kind, so a milestone can only ever show the
// vocabulary its own kind declared (domain.RunMilestone.Validate) and a new
// kind needs no change here.
func formatMilestone(m domain.RunMilestone) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s", stamp(m.RecordedAt), m.Kind)
	if m.InvocationID != nil {
		fmt.Fprintf(&b, "  invocation=%s", identifier(string(*m.InvocationID)))
	}
	if m.Terminal != nil {
		fmt.Fprintf(&b, "  terminal=%s", *m.Terminal)
	}
	if m.Outcome != nil {
		fmt.Fprintf(&b, "  outcome=%s", *m.Outcome)
	}
	if m.Reason != nil {
		fmt.Fprintf(&b, "  reason=%s", *m.Reason)
	}
	return b.String()
}

// statusBlock renders the current state: a header carrying elapsed time and
// the last observation, and the state lines (hold and per-invocation
// liveness). Only the state lines identify the block, so the header's
// ever-advancing elapsed clock does not by itself reprint the block every
// read; an invocation's refreshed observation instant does, which is what
// makes a live run report its own heartbeat and a stopped daemon fall silent
// until the freshness window opens an observation gap.
func statusBlock(
	o domain.RunObservation, asOf time.Time, window time.Duration,
) (string, []string) {
	header := fmt.Sprintf("status  elapsed=%s  last-observed=%s",
		elapsedField(o, asOf, Conclude(o).Final), lastObservedField(o))
	state := []string{holdLine(o)}
	observations := make(map[domain.InvocationID]domain.InvocationObservation, len(o.Invocations))
	for _, obs := range o.Invocations {
		observations[obs.InvocationID] = obs
	}
	for _, id := range invocationIDs(o) {
		obs, ok := observations[id]
		var last *domain.InvocationObservation
		if ok {
			last = &obs
		}
		liveness := domain.DeriveInvocationLiveness(last, asOf, window)
		if last == nil {
			state = append(state, fmt.Sprintf("  invocation  %s  liveness=%s",
				identifier(string(id)), liveness))
			continue
		}
		state = append(state,
			fmt.Sprintf("  invocation  %s  status=%s  liveness=%s  observed-at=%s",
				identifier(string(id)), last.Status, liveness, stamp(last.ObservedAt)))
	}
	return header, state
}

func holdLine(o domain.RunObservation) string {
	if o.Hold == nil {
		return "  hold  none"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  hold  %s", o.Hold.Reason)
	if o.Hold.InvocationID != nil {
		fmt.Fprintf(&b, "  invocation=%s", identifier(string(*o.Hold.InvocationID)))
	}
	// Both instants ride the line, and deliberately: FirstObservedAt dates
	// the cause, while LastObservedAt is what the engine advances on every
	// paced refresh. Since the status block is keyed on these state lines,
	// carrying the refreshed instant is what gives a held run the same
	// heartbeat a running one gets from its invocation observation. Without
	// it, a run held before any invocation was observed goes silent, and an
	// operator cannot tell a standing hold from a stopped daemon.
	fmt.Fprintf(&b, "  first-observed=%s  last-observed=%s",
		stamp(o.Hold.FirstObservedAt), stamp(o.Hold.LastObservedAt))
	return b.String()
}

// invocationIDs is every invocation the run names, from milestones as well as
// observations, so an invocation the daemon admitted but never looked at is
// displayed as never_observed instead of omitted.
func invocationIDs(o domain.RunObservation) []domain.InvocationID {
	seen := map[domain.InvocationID]bool{}
	ids := []domain.InvocationID{}
	add := func(id domain.InvocationID) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, obs := range o.Invocations {
		add(obs.InvocationID)
	}
	for _, m := range o.Milestones {
		if m.InvocationID != nil {
			add(*m.InvocationID)
		}
	}
	slices.Sort(ids)
	return ids
}

// elapsedField renders the run's elapsed clock, truncated to whole seconds.
// "unknown" is the honest reading when submission was never observed or the
// endpoints read backwards; the model refuses to derive a span from either
// (domain.RunObservation.Elapsed), and no completion fraction exists to show
// in its place.
//
// The model freezes elapsed at the last concluding milestone, and
// terminal_recorded is one of those, so a run whose execution completed while
// import and publication have decided nothing yet would show a stopped clock
// for as long as the operator keeps following it. Conclude deliberately keeps
// such a run pending, so the clock runs to asOf until the outcome is final;
// once it is, the model's frozen span is the answer, and the two derivations
// agree on when the run ended.
func elapsedField(o domain.RunObservation, asOf time.Time, final bool) string {
	if final {
		elapsed, ok := o.Elapsed(asOf)
		if !ok {
			return "unknown"
		}
		return elapsed.Truncate(time.Second).String()
	}
	submitted, ok := o.SubmittedAt()
	if !ok || asOf.Before(submitted) {
		return "unknown"
	}
	return asOf.Sub(submitted).Truncate(time.Second).String()
}

func lastObservedField(o domain.RunObservation) string {
	at, ok := o.LastObservedAt()
	if !ok {
		return "unknown"
	}
	return stamp(at)
}

func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// identifier renders a run or invocation id safely. Ids are the one rendered
// value a caller chooses: domain.Run.Validate requires only that they be
// non-empty, so an operator (and, once submission is exposed remotely,
// whoever reaches that surface) can make one durable that holds a newline or
// an ANSI escape. Written verbatim, such an id could forge a milestone, a
// status block, or an outcome line in this display, or drive the operator's
// terminal.
//
// Nothing else rendered here needs this. Every other field is a closed code
// or a timestamp, and the store re-validates each row against those
// vocabularies and fails the read closed, so a row cannot carry a kind,
// status, liveness, or reason the daemon does not define.
func identifier(id string) string {
	if !safeIdentifier(id) {
		return strconv.Quote(id)
	}
	return id
}

// safeIdentifier reports whether an id can be written as-is: printable, and
// free of the space and quote characters that would otherwise blur the
// field-separated layout.
func safeIdentifier(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r == ' ' || r == '"' || !strconv.IsPrint(r) {
			return false
		}
	}
	return true
}

type (
	Outcome    = domain.RunOutcome
	Conclusion = domain.RunConclusion
)

// SupervisionState is the closed lifecycle vocabulary consumed by the
// production real-run supervisor.
type SupervisionState string

const (
	SupervisionUnobserved             SupervisionState = "unobserved"
	SupervisionPending                SupervisionState = "pending"
	SupervisionWaitingForSpecApproval SupervisionState = "waiting_for_specification_approval"
	SupervisionImplementationBound    SupervisionState = "implementation_bound"
	SupervisionAttentionRequired      SupervisionState = "attention_required"
	SupervisionPublicationReady       SupervisionState = "publication_ready"
	SupervisionPublished              SupervisionState = "published"
	SupervisionBlocked                SupervisionState = "blocked"
	SupervisionFailed                 SupervisionState = "failed"
	SupervisionLost                   SupervisionState = "lost"
)

// AllSupervisionStates is the single registration point for the snapshot
// lifecycle contract shared with scripts/run-real-work-supervision.sh.
var AllSupervisionStates = []SupervisionState{
	SupervisionUnobserved,
	SupervisionPending,
	SupervisionWaitingForSpecApproval,
	SupervisionImplementationBound,
	SupervisionAttentionRequired,
	SupervisionPublicationReady,
	SupervisionPublished,
	SupervisionBlocked,
	SupervisionFailed,
	SupervisionLost,
}

func (s SupervisionState) valid() bool {
	switch s {
	case SupervisionUnobserved,
		SupervisionPending,
		SupervisionWaitingForSpecApproval,
		SupervisionImplementationBound,
		SupervisionAttentionRequired,
		SupervisionPublicationReady,
		SupervisionPublished,
		SupervisionBlocked,
		SupervisionFailed,
		SupervisionLost:
		return true
	default:
		return false
	}
}

const (
	OutcomePending   = domain.RunOutcomePending
	OutcomePublished = domain.RunOutcomePublished
	OutcomeBlocked   = domain.RunOutcomeBlocked
	OutcomeFailed    = domain.RunOutcomeFailed
	OutcomeLost      = domain.RunOutcomeLost
)

var AllOutcomes = domain.AllRunOutcomes

var (
	ErrInvalidOutcome        = domain.ErrInvalidRunOutcome
	ErrOutcomeDetailMismatch = domain.ErrRunOutcomeDetailMismatch
)

func Conclude(observation domain.RunObservation) Conclusion {
	return domain.ConcludeRun(observation)
}

func deriveSupervisionState(snapshot observedb.Snapshot, conclusion Conclusion) SupervisionState {
	if conclusion.Final {
		if conclusion.Outcome == OutcomePublished && !publicationAccepted(snapshot) {
			return SupervisionPublicationReady
		}
		return SupervisionState(conclusion.Outcome)
	}
	for _, item := range snapshot.AttentionItems {
		if item.Type == domain.AttentionSpecApproval && len(item.RequestedDecision) > 0 {
			return SupervisionWaitingForSpecApproval
		}
	}
	for _, item := range snapshot.AttentionItems {
		if len(item.RequestedDecision) > 0 {
			return SupervisionAttentionRequired
		}
	}
	if snapshot.Attempt != nil && snapshot.Attempt.ApprovedSpecDigest != "" &&
		snapshot.Observation.RunID == snapshot.Attempt.ElaborationRunID {
		return SupervisionImplementationBound
	}
	return SupervisionState(conclusion.Outcome)
}

// publicationAccepted requires both sides of the publication worker's final
// durable boundary under their authenticated owners: a completed terminal for
// the producing invocation and publication_ready for the dedicated publication
// invocation. The selected run's publication task is already authenticated and
// marked dispatched. publication_ready alone deliberately remains an outcome
// for operators, but is not permission for the real-run supervisor to stop the
// daemon.
func publicationAccepted(snapshot observedb.Snapshot) bool {
	if !snapshot.PublicationReadyAuthenticated ||
		snapshot.ProducingInvocationID == "" || snapshot.PublicationInvocationID == "" {
		return false
	}
	completed := false
	ready := false
	for _, milestone := range snapshot.Observation.Milestones {
		if milestone.InvocationID == nil {
			continue
		}
		if milestone.Kind == domain.MilestoneTerminalRecorded &&
			*milestone.InvocationID == snapshot.ProducingInvocationID &&
			milestone.Terminal != nil && *milestone.Terminal == domain.ObservedStatusCompleted {
			completed = true
		}
		if milestone.Kind == domain.MilestonePublicationReady &&
			*milestone.InvocationID == snapshot.PublicationInvocationID {
			ready = true
		}
	}
	return completed && ready
}
