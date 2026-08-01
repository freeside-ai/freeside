# Operator Run-Follow CLI

Work unit: #409. Scope: `daemon/` (a new `internal/observe` package, the
`freesided follow` verb, docs) and this note. Consumes #394's contract
unchanged.

## Position: A Pull Diagnostic Beside The Push Channels

Raised by the owner mid-unit: Freeside's premise is that nobody is watching,
so a command an operator sits and watches reads against the thesis. Recorded
here because the answer bounds what this command is allowed to become.

**Follow is not, and must not become, the way a stall reaches an operator.**
That path is push: an attention item for work needing judgment, and the ward-
or daemon-observed stall heartbeat and its notice (plan §5.12, phase 1B.1).
Follow answers the question a push channel structurally cannot, which is what
justifies it: the state of one named run right now, including the case where
the daemon itself stopped and therefore raised nothing. `observation_gap`
exists for exactly that case, and no process inside a stopped daemon can
report it.

So the intended reach-for moments are after a notice, when an expected pull
request has not appeared, and during 1A.2 unattended bring-up. `-once` is
that question in its plainest form and is the thesis-aligned mode; the
streaming loop is the affordance that assumes someone is present, and is the
part to be suspicious of later. A stall an operator only learns about by
watching a terminal is a defect in the notice path, not a use for this
command; if that starts happening, the fix is 1B.1's heartbeat, not a better
display here.

## Decision

**Chose a line-oriented text display, not the JSON the other verbs emit.**
`setup`, `onboard`, `doctor`, and `submit` each produce one result record, and
JSON is right for a record. `follow` produces a live display for a human
watching a run, and its only other consumer would be the API unit, which
#409 explicitly defers and which will carry #394's shapes over `api/` rather
than reuse a CLI's stdout. Emitting a second machine format now would invent
a wire contract that nothing consumes and that the API unit would not adopt,
so the command emits exactly one rendering and the vocabulary in it is the
contract's own (`run_submitted`, `observation_gap`, `verification_findings`)
rather than friendlier prose that would drift from the codes an operator has
to search for.

**Chose to put the whole verb in one package whose import allowlist is the
containment proof, and to keep `package main` out of it.** #409's acceptance
asks for a test that the command never reads a live writer's filesystem,
stdout, stderr, or transcript. There is no syscall-tracing assertion to write
here, and a "sentinel bytes did not appear in the output" test proves only
that one path was not taken. So the verb lives in `internal/observe`, and a
test parses its non-test files and holds every import to a closed allowlist:
eight standard packages plus `domain` and `store`. That list names no way to
open a file, start a process, or open a socket, and no stage driver, runtime,
workspace, or artifact store, so containment is a property of the package
rather than a claim about the code, and widening it is a visible edit to the
allowlist instead of a test that still passes.

Three weaker forms were tried and rejected, each caught by a review round
rather than by the author, which is the honest record of how this landed. An
assertion over `cmd/freesided/follow.go` proves nothing, because a file in
`package main` reaches every unexported sibling helper without importing
anything. An allowlist that waves the standard library through proves nothing
either, because `os` alone is a file read; hence a total allowlist rather
than a denylist, since a denylist has to anticipate every way to name a file
(`os`, `io/fs`, `path/filepath`, `syscall`, a second `database/sql` driver)
and silently admits the one it forgot. And permitting `internal/store`
admitted its whole method surface, `WriteInternal`, `Checkpoint`, `Restore`,
and the backup files included, none of which a run observer should hold.

**The class, stated once so it stops recurring: an import allowlist bounds
which packages a caller can name, never which methods of a permitted package
it calls.** So the proof reaches only as far as the smallest permitted
surface, and the regress must stop at something a human can check by eye.
It stops at `internal/observe/observedb`: one short file whose exported
surface is open, observe-one-run, close, pinned by a test that fails when a
fourth export appears. Claiming the allowlist alone is total is what earned
the second round; the test and the note now say where the mechanical proof
ends and reading begins.

The narrow `Observer` interface (one `ObserveRun` method, adapted to the
store beside it) keeps the display functions themselves pure of persistence,
so they take a `domain.RunObservation` and nothing else.

**Chose to exit on a decided outcome only after a read that adds nothing, and
to bound what that claims.** Publication readiness and terminal recording are
separate transactions whose order varies by lane (#394's own integration
fixture records `publication_ready` before `terminal_recorded`). Exiting the
moment an outcome is final would truncate the timeline whenever the poll
landed between the two writes. Quiescence — final *and* the last read printed
nothing new — costs one interval and is order-independent, so it needs no
assumption about which milestone lands last.

The second review round correctly held the first statement of this to a
stronger promise than the mechanism keeps: a daemon that crashes between
those two transactions appends the sibling on a recovery pass minutes or
hours later, long after the command printed the decided outcome and
returned. The mechanism change it proposed (block until the lane's terminal
milestone) was declined: it would make an operator wait out an arbitrary
outage for a run whose result they already have, and it buys nothing, because
the timeline is durable and a later `-once` shows whatever landed after. The
claim was what needed fixing, so it now states what the settling read is for,
interleaving within one reconcile pass, and what it is not, a durability
guarantee.

A completed execution is deliberately not final either: import and
publication still decide the run, and an attended daemon holds it by design,
which the status block states while the follow keeps waiting.

Rebasing onto #437, which moved production publication onto its own reconcile
loop, strengthens rather than threatens this: two independent loops make the
interleaving between an outcome and its sibling milestone more likely, not
less, and the settling read is order-independent precisely so it needs no
assumption about which loop commits first.

**Chose to report observation success in the exit status, never the run's
verdict.** `doctor` exits 1 when its own checks are unhealthy, which is its
verdict about the system. `follow` has no verdict: a correctly blocked run is
a successful observation. Making a blocked or failed run exit non-zero would
make an operator's terminal report a Freeside failure for a run that behaved
exactly as designed, and would collapse the distinction between "could not
observe" and "observed a block". The printed `outcome` line carries the
verdict precisely, including the block's closed reason code.

**Chose to refuse a run with nothing observed rather than follow it.** An
empty aggregate means a mistyped run id or a run submitted before migration
0024; following either would wait silently forever. The refusal names both
causes. The cost is that an operator cannot start a follow before the
submission it wants to watch, which is the right trade: the typo is the
common case and the silent wait is the expensive failure.

**Chose to run the elapsed clock to now until the outcome is final, rather
than take the model's frozen span.** `RunObservation.Elapsed` freezes at the
last concluding milestone, and `terminal_recorded` is one of those, so a run
whose execution completed while import and publication have decided nothing
would show a stopped clock for as long as an operator watched it. Since this
display deliberately does not treat a completed terminal as the run's outcome
(above), taking the model's span would have the header contradict the outcome
line. The display therefore counts to the read instant while pending and
takes the model's frozen span once final, preserving the model's refusal to
report a backwards span in both branches. Changing `Elapsed` itself was
rejected outright: it is #394's contract, and a contract change stops this
unit by its own non-goals.

**Chose to key the status block on observed state, not on the whole
rendering.** Elapsed time advances every read, so comparing the full block
would reprint it every second for the life of a run. Keying on hold and
per-invocation status/liveness/observation-instant means the block appears
when something was actually observed, which the engine's 10-second refresh
turns into a natural heartbeat while the daemon lives and into silence when
it does not — until the freshness window opens an `observation_gap`, which is
itself a state change and prints. The exiting follow repeats the block once
more when it would read differently, so the last elapsed time and last
observation on screen are current at exit.

## Containment and Leak Review

Refute-first pass over the diff, run in-session by the author rather than by
an independent context: this session cannot delegate, so the load-bearing
independent pass is the automated reviewer on the PR, and that limit is
disclosed here rather than implied away.

**Rejected by verification** (attacked, held): every value the command can
print is an ID, a closed code, a bool, or a timestamp, because
`domain.RunObservation` has no field that carries anything else (#394 made
free text unrepresentable); the command constructs no driver, runtime,
workspace path, or artifact store, and cannot reach one without adding an
import the containment tests refuse; the display holds no authority, since it
only reads and the store's observation rows are projection the workflow never
reads back; a read failure propagates loudly instead of rendering as an idle
run, so a row the current vocabulary cannot express fails the follow closed
exactly as the store intends.

**Confirmed by the automated reviewer, round one** (three findings, all
accepted and fixed in the same push; the author's own pass had missed all
three, which is the concrete case for the independent eyes above). The
containment assertion was two weak forms of a real boundary, fixed by moving
the verb out of `package main` and making the allowlist total (above). The
elapsed clock froze at a completed terminal while the display called the run
pending (above). And the documented `0 / 1 / 2` exit contract had no path to
2: every refusal exited 1, so a caller could not tell a mistyped flag from a
run it could not read; usage errors now carry an `ErrUsage` sentinel that the
shim maps to 2.

**Round five** (three findings, all confirmed and fixed; two were this unit's
own incomplete sweeps rather than new ground). The interrupt fix from round
four covered the branch where cancellation ends the wait but not the one where
it aborts a blocking read, which returned the outcome with no closing status
at all; both branches now finalize the same way, against a resampled clock and
the last observation actually read. The hold line carried only
`FirstObservedAt`, deliberately, to keep a refreshed hold from churning the
display; that was the wrong axis. A run held before any invocation is observed
has nothing else that advances, so excluding the refreshed instant from the
keyed state made exactly that case go silent, which is indistinguishable from
a stopped daemon. `LastObservedAt` now rides the line and drives the
heartbeat.

Third, and the containment class a third time: the `observedb` surface pin
recorded only the type's name, so an exported field (`Raw *store.Store`) or an
embedded `*store.Store` would have handed the follow path the store's write,
checkpoint, and restore methods with no import change and a green test. The
collector now enumerates exported fields, embeddings, and interface methods,
and, since the pin is load-bearing, a test proves the collector catches each
of those routes rather than asserting that it does. That is the correction the
class needed: the first two rounds were a boundary that did not bound, and
this one was a check that was never itself checked.

**Round four** (two findings, both confirmed and fixed). The `Outcome` type is
a new daemon enum and shipped without the conventions binding on one (AGENTS.md
"Daemon coding conventions"): no `valid()` predicate, no `AllOutcomes`
registration slice, and nothing stopping an assembled `Conclusion` from
claiming a block with no reason. Both pieces are added, and `Conclusion` gained
a `Validate` with the outcome-scoped detail contract, checked against every
classification `Conclude` produces. Membership for the domain's own
vocabularies goes through `AllRunHoldReasons` and
`AllObservedInvocationStatuses`, since those registration slices are exactly
what a package outside the domain needs when the predicates are unexported.

Also fixed: the interrupt path passed the instant captured *before* the wait to
the closing status block. With a long `-interval` that dated the block to
before the operator pressed anything, understated elapsed time, and, because
the block then compared equal to the one already on screen, printed no closing
status at all. The clock is resampled when the interrupt ends the wait; the
observation stays as last read, which is honest, since an old observation
against a fresh clock is precisely what derives an observation gap.

**Round three** (one finding). Confirmed and fixed: run and invocation ids are
the only rendered values a caller chooses (`domain.Run.Validate` requires only
non-empty, and `freesided submit` takes an explicit `-run-id`), so an id
holding a newline or an ANSI escape reached the operator's terminal verbatim
and could forge a milestone, status, or outcome line. Every rendered
identifier now goes through one escaper that quotes anything non-printable or
containing a space or quote, and leaves ordinary ids untouched; the error path
quotes the id too. Nothing else needed it, and that is worth stating as an
invariant rather than defending everywhere: every other rendered field is a
closed code or a timestamp, and the store re-validates each row against those
vocabularies and fails the read closed, so no row can carry a kind, status,
liveness, or reason the daemon does not define.

**Round two** (two findings). Confirmed and fixed: permitting `internal/store`
in the allowlist admitted its whole method surface, closed by routing the
follow path through `observe/observedb` and pinning that package's exports
(above). Declined on the mechanism, fixed on the claim: the settling read
cannot survive a daemon crash between the outcome and its sibling milestone,
and should not try to (above).

Two rounds of the same class, containment asserted at a boundary that did not
bound behaviour, is the signal that the author's own refute pass could not
substitute for independent eyes on this axis. Recorded rather than smoothed
over, because the next unit that writes a structural-containment test should
expect to get it wrong the same way.

**Accepted by decision:** `store.Open` migrates the database to head, so
following a run with a newer binary than the daemon's would migrate the
daemon's store. That is the existing `submit` and `doctor` behaviour on the
same direct-store transport, not a new exposure, and the alternative (a
read-only open that refuses an unmigrated schema) would be a store-level
change outside this unit's scope.

Revisit when the API unit exposes run observation remotely: the display's
vocabulary and its outcome classification are presentation that a remote
client would re-derive, and the direct-store transport this command assumes
stops bounding the reader.
