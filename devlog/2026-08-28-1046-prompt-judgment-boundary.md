# Prompt Judgment Boundary for the Phase 1A Elaborator and Implementer

## Decision

Move the elaborator/implementer judgment boundary into the Phase 1A prompt
text now, as prose, ahead of the typed outcomes that will later carry it.

The elaborator may state a bounded assumption only for an implementation
detail that has one default following existing repository practice and would
not invalidate an acceptance criterion if it changed. Product, policy,
compatibility, security, data-migration, and scope questions are never
settled by assumption: they are listed in both the summary and the body as
open owner decisions with options and a recommendation, so the existing
`spec_approval` gate sees them while the specification is still a proposal.
Acceptance criteria must name an observable behavior or a test class rather
than "add tests", and the body ends with replan triggers: the discoveries at
which the implementer stops instead of adapting.

The implementer gets the matching half. It adapts internal implementation
details while behavior, scope, invariants, and compatibility hold, and
records each adaptation in its result. It never makes verification pass by
deleting or skipping a relevant test, weakening an assertion, broadening an
exclusion, suppressing an error, or editing generated output without
regenerating it. It stops when proceeding would require inventing observable
behavior, settling an open product or policy question, widening scope, or
crossing an invariant, non-goal, or replan trigger. Stopping is an interim
protocol until a typed outcome exists (#990): leave no changes in the
workspace, write no commit plan, and report the blocker with its repository
evidence, viable options, and a recommendation. Results name each
verification command actually run.

The boundary is an owner-level safety policy, not prompt wordsmithing: it
decides which questions an unattended run is allowed to answer for itself,
and which ones must reach a human at the approval gate that already exists.

The interim protocol has a known cost, accepted here. A stop has nowhere
typed to land: `StageResult` carries no blocked outcome, and with an empty
change set `importer.Import` still builds a commit
(`daemon/internal/importer/importer.go`) with no stage-level guard rejecting
it. So a stop today surfaces as the run's blocker report against an empty
candidate, not as an `agent_question` interruption. That is accepted because
the alternative it replaces is worse: an implementer that proceeds on an
invented product decision publishes a candidate that looks legitimate. An
empty candidate plus an evidence-backed blocker report is visible and inert.

The elaborator half has the mirror cost. A listed open owner decision does
not make the specification unapprovable: `authorizeElaborationImplementation`
accepts an empty-message Approve, and a project with `gates.spec_approval`
off skips approval entirely, so the question reaches no human until the
implementer stops on it. Under `spec_approval` the recommendation is visible
at the gate and the answer returns through request-changes and the revision
path; without the gate, the escalation is only as good as the stop that
follows it. #990 closes both halves by typing the outcomes: `needs_decision`
on the specification side, a blocked `StageResult` on the implementation
side, each rendering as an `agent_question` regardless of gate settings.

## Rejected Alternatives

- **Wait for the typed outcome in #990 before touching the prompts.**
  Rejected. Today's prompts let the elaborator settle an owner-level question
  as a "bounded assumption" and give the implementer no stop rule at all, so
  an unattended run can build an invented product decision and call it done.
  That is a live gap, and #990 changes the transport (a typed
  `agent_question`/blocked outcome instead of a prose blocker report), not
  the boundary itself. The prose rules stay correct after the typed outcome
  lands; only the stop protocol's mechanics are replaced.
- **Let the elaborator assume freely and merely flag the assumption.**
  Rejected. Once an assumption reaches the approved specification it becomes
  the implementer's contract, and a flagged product decision still gets
  built. Escalating before approval puts the decision at the `spec_approval`
  gate, where a human is already looking.
- **Put the boundary only in the implementer prompt.** Rejected. A
  specification that already buried a product decision inside an assumption
  gives the implementer nothing to stop on, and no set of implementer rules
  recovers the question the elaborator dissolved. The two prompts carry
  complementary halves: the elaborator names the triggers, the implementer
  honors them.
- **Encode the rules as a daemon check instead of prompt text.** Rejected for
  this unit. Mechanical enforcement belongs in the typed outcome (#990) and
  the commit-plan validator (#988), both filed; a check cannot express "this
  is a product question" in the first place. The honest cost of the prose
  route is that its effect is unmeasured until the eval baseline (#989).

## Constraint Recorded

The elaborator prompt is budget-bound: `TestPhase1AElaboratorPromptLeavesEnvelopeHeadroom`
holds the rendered package under 4096 bytes so the daemon-authenticated
prior-artifact envelope keeps headroom. This change took it from 3,379 to 3,963
bytes. A later elaborator rule must trade existing text out, or move to
a check; raising the cap defeats the headroom the test protects.

## Revisit When

- **#990 lands a typed question/blocked outcome.** Replace the interim prose
  stop protocol with the typed outcome and keep the boundary as written.
- **#989 produces an eval baseline.** It is the first measurement of whether
  the boundary changes behavior. Retune the wording if elaborations escalate
  routine implementation details, or still assume through owner-level ones.
- **The elaborator budget binds.** If a needed rule does not fit the
  remaining headroom, trade text out rather than relaxing the size test.
