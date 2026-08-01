# Run-Monitoring Observability Contract

Work unit: #394. Scope: `daemon/` (domain, store, migrations, exec,
engine, docs) and this note.

## Decision

**Chose an observational projection beside the workflow, never a state
column on it.** `domain.Run` deliberately carries no status field, and
the ward journal's design position (daemon/internal/ward/journal.go)
argues that persisted per-stage progress bits become decoded trust bits
recovery is tempted to believe instead of re-observing. The contract
therefore adds append-only `run_milestones` plus last-write
`invocation_observations` and `run_hold_observations` rows that are
written inside the same transactions that commit the underlying
workflow facts, but that no recovery, publication, or teardown decision
ever reads. Recovery re-observes runtime ownership and writer absence
exactly as before; a forged or deleted observation row changes nothing
but what an operator sees. A mutable run-status column was rejected
because it either becomes decoded authority or diverges from the facts;
deriving status live from the join the daemon does internally was
rejected because reconnect and restart would lose the already-observed
timeline (#409's acceptance) and every client would re-implement the
join.

**Chose closed, code-only hold and refusal reasons; free text is
unrepresentable.** The observation surface sits on a credential-leak
surface: hold and block causes originate in errors and operator strings
that can embed workspace paths, provider output, or configuration
content. Following the `publish.MintRecord` precedent (leak-unsafe
values are unrepresentable in the audited type, not filtered out of
it), `RunHoldReason` is a closed enum and no observation type carries a
free-text reason, summary, or transcript field. The engine maps the
sentinel-error catalogue onto the codes; an error that classifies as no
code records no hold (it stays a loud pass failure). Attaching the
existing attention-item prose was rejected because that prose is
composed for a different, already-gated surface and would make every
future reason a leak review.

**Chose to widen `StageDriver.Inspect` to a typed `exec.Inspection`
rather than add a parallel method.** Inspection needs liveness (does
the driver currently observe the execution?) beside status; two
methods answering overlapping questions from one driver invite drift
and racing calls. The Claude driver already computes the `live` bit
internally (`inspectIntent`); the contract change exposes it. The
production call surface is one engine site, so the churn is in tests.
`ReviewSource` is left unchanged: review observability belongs to the
review-stage unit (#427).

**Chose liveness as derivation, not stored state.** A stored "live"
verdict goes stale the moment the daemon stops; the model stores only
the last observation (status, live bit, daemon-clock instant) and
derives liveness (`observed_live`, `observed_idle`, `observation_gap`,
`terminal`, `never_observed`) from that instant against a caller-held
freshness window. A daemon restart therefore yields an observation gap
structurally, with no cleanup step, and elapsed time and last
observation fall out of the milestone and observation timestamps. No
percentage-complete field exists anywhere in the model.

**No-backfill binds every milestone writer, not just the migration.**
Review pressure (three rounds) established the full rule: a milestone
is minted only by the transaction that first inserts its underlying
fact — the creating submission, the inserting authority write
(admission, export, outcome), the first terminal insert — never by a
byte-identical replay, because a replay against a database whose
timeline predates migration 0024 would present reconstruction as
observation. Publication milestones on the authenticated recovery path
are the deliberate exception: the recovery pass genuinely re-observes
convergence at a real instant, so its first-wins append records an
observation, not a backfill.

**Milestone writes are first-observation-wins and idempotent.**
Milestone appends use insert-or-ignore against a per-(run, kind,
invocation) uniqueness key, so replayed transactions and crash-retry
convergence (the store's existing idempotency discipline) never
duplicate or reorder the timeline. Store-level embedding in
`RecordExecutionAdmission`, `RecordExecutionExport`, and
`RecordExecutionOutcome` was chosen over engine-side calls for those
facts so every lane (fake and Claude, live and recovery) records them
atomically with the fact itself.

**Hold causes are stated by the classifying site, never derived from
prose.** The dispatch path classifies its sentinel errors through one
mapping (`dispatchHoldReason`), tested total over the hold-classified
sentinel set; the publication lane's durable holds take an explicit
typed cause parameter at each `holdBlockedTask` call site, because the
operator prose passed beside it is composed per situation and matching
on it would silently drop holds when wording changes. The definitive
block strings are a closed set recovery already fails closed on, so
their mapping is total by the same contract. One deliberate behavior
change rides this: a dispatch pass held by the operating-state gate now
still lists pending production intents (a read) so each held run's
typed cause is observable; dispatch remains skipped exactly as before.

**A hold ends with the cause that raised it.** Three writers end a
hold: a milestone that actually inserts (forward progress), the next
classified cause replacing the row, and the attempt that accepts a
queued publication task — which ends only the hold-only composition's
`attended_mode_active` pause, so the read surface stops reporting an
attended daemon's hold through a whole fetch, verification, and
publication attempt. That clear is cause-scoped rather than a blanket
delete because the accepting attempt is about to decide holds of its
own: a definitive block and an environmental back-off are re-recorded
on the retry cadence, so a blanket clear would blink a live cause out
of the read surface once per retry cycle and restart its
`first_observed_at` span. The cause stays a SQL delete predicate, so
the decision still reads no projection row back. Symmetrically, every
environmental pause of a publication task states the same typed cause
(`publication_environment`), including the lock-setup branches that
previously deferred with no observation; a cancelled context records
nothing, because daemon shutdown is not a cause an operator can act
on.

## Containment

Monitoring never reads a live writer's stdout, stderr, filesystem, or
transcript. The observation path consumes only `Inspect` (status +
liveness), the daemon's own durable records, and the daemon clock;
`Stream` remains empty for the Claude driver by prior decision, and the
scenario fixtures assert the observation flow never calls it.

## Refute-First Verification

An independent refute pass over the full diff (lenses: leak axes,
trust boundary in both directions, hot-path regressions, replay
convergence, migration semantics, pacing starvation) produced one
blocking finding, confirmed by demonstration, and three related
non-blocking ones; all four are fixed in the same commits.

**Confirmed and fixed: the write-path read-backs gave projection rows
workflow authority.** `RecordInvocationObservation` refused a write
whose stored row named a different run, and `RecordRunHold` re-read
the stored hold through the fail-closed read surface; both errors
propagated through the reconcile pass into `Engine.Run`, so a forged
or corrupt projection row could durably wedge the daemon — exactly the
authority the contract denies these rows. The write paths now repair
by overwrite: the incoming value derives from the durable records the
engine just joined, so a divergent stored binding, an inexpressible
stored reason, an unparsable instant, or a stepped-back clock all fall
through to plain replacement, and only real SQL faults still fail the
pass. The read surface keeps failing closed. Also confirmed and fixed
in the same class: a converged (insert-ignored) milestone replay used
to clear a standing hold (it now clears only on a real insert), and a
pace stamp recorded before a failed write suppressed the retry (the
stamp is now dropped on write failure).

**Rejected by verification** (attacked, held): every persisted
observation field traces to IDs, closed codes, a bit, or timestamps
(no leak path); no reader of the observation tables exists outside the
store read surface and tests; the Inspect widening is
decision-identical to the base for every driver; milestone appends
converge under every replay path including the write-once terminal
row; the migration's CHECKs match the Go vocabularies; a changed
observation state is always written immediately despite pacing.

**Accepted by decision:** a pass held by the operating-state gate now
lists pending intents and writes paced hold observations, so it gained
the store's read/write failure surface where it previously returned
untouched; that is the fail-loud convention applied to a new write,
not a masked error, and it is disclosed in the PR body.

Revisit when the API unit first exposes run observation to remote
clients (the store-transport assumption — local, daemon-equivalent
access — no longer bounds the reader), or when a second StageDriver
implements richer inspection than the status+liveness pair.
