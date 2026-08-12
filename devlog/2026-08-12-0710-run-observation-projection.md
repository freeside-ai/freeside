# Project Run Observation Instead of Persisting a Second Status

Work unit: #657. This note records the API contract and the returned-object
trust-boundary verification for the first runs list and run timeline.

## Decision

Chose a **store-derived observation projection** over persisting a second run
status. `Run` remains the durable execution graph; milestones, holds,
invocation observations, and execution outcomes remain daemon-owned records.
The sync server joins them in one read transaction and stamps the run summary
and timeline with that transaction's revision. The client can therefore join a
run to schedules and render its latest milestone, outcome, hold, stage, and
round without interpreting agent text or inventing a competing lifecycle.

Chose a separate `GET /runs/{run_id}/timeline` snapshot over embedding history
in every list response. The list carries only the latest summary fields needed
for scanning and filtering; the selected run fetches its full history. The app
caches that snapshot under the same epoch/revision rules as the existing sync
resources, so a partial refresh cannot make a stale full cache look current.

Chose to leave #692 open rather than folding degraded candidate-readiness
states into this unit. Production currently records only `ready_clean`; adding
the degraded vocabulary would change durable AttentionItem and readiness trust
contracts, while #657 needs only to render the ready milestone already proven
by the daemon. That safety-policy change deserves its own refute-first unit.

## Rejected Alternatives

- **Client-derived run status:** rejected because it would duplicate domain
  policy across Swift and Go and could turn stale or partial client data into a
  false terminal state.
- **One large run resource:** rejected because project lists would repeatedly
  transfer complete milestone and invocation histories for rows the operator
  never opens.
- **Agent-authored timeline text:** rejected because the timeline is an
  operator evidence surface. Every rendered event is a typed daemon record.

## Refute-First Findings

Two independent refute-first lenses reviewed persistence, sync, returned-object
authentication, cache races, and operator presentation. Their dispositions:

- **Confirmed and fixed, invisible observation writes:** milestone, hold,
  invocation, and execution writes originally used the internal transaction
  path, so the run surface could change without advancing `/sync/revision`.
  Client-visible observation writes now use `WriteTx` and advance the revision
  in the transaction that commits them.
- **Confirmed and fixed, unauthenticated conclusions:** structurally valid
  forged milestones could make a failed run appear ready or blocked. The sync
  boundary now validates every backing snapshot against the transaction's
  server revision, binds invocation observations and execution admissions to
  the returned run's exact stage and attempt, proves started invocations have
  a dispatched durable intent, and proves ready or blocked milestones against
  durable publication evidence. Ready requires the workflow-owned item ID and
  its authenticated publication binding. A definitive block requires its own
  workflow-owned ID, exact terminal item shape, and prose-to-enum reason
  binding, so generic or transient items cannot authenticate either outcome.
- **Confirmed and fixed, partial-response races:** run and timeline responses
  issued before cache replacement could overwrite a restored epoch, while an
  ordinary same-epoch heartbeat could incorrectly discard a valid lazy read.
  A cache-replacement generation now invalidates only responses older than a
  canonical replacement; per-resource generations preserve request recency.
  Error and cancellation paths carry the same guards. A later review also
  found that a delayed bootstrap could replace a newer same-epoch partial
  read, clear its observations, and incorrectly mark the older full snapshot
  fresh. Adoption now rejects that response and refetches once; a gated
  bootstrap/partial-read test proves the cursor converges at the newer
  revision. Heartbeats make the same live-cursor check after their response,
  so an earlier revision cannot reset a newer partial read to fresh.
- **Confirmed and fixed, pre-runs cache upgrade:** a format-2 cache decodes
  missing run and schedule fields as empty arrays. Retaining its cursors could
  let an upgraded client call that incomplete state fresh until some unrelated
  server revision changed. Cache format 3 invalidates those files and forces
  one bootstrap while migrating the independent pending-command ledger; the
  disk-cache suite reconstructs an otherwise-valid format-2 file missing every
  new run surface and proves its retry identity remains intact.
- **Confirmed and fixed, publication-invocation substitution:** terminal
  publication milestones now require the dedicated
  `publish-production-<run>` invocation. Ready milestones also match that
  invocation to the authenticated ready binding, so an implementation attempt
  cannot be relabeled as a publication decision.
- **Confirmed and fixed, retry supersession and started-intent retargeting:** a
  failed attempt no longer remains the run's final outcome after another
  attempt is admitted or started. Started milestones now authenticate the
  dispatched intent's recognized lane and payload binding, so an unrelated
  dispatched row cannot be
  projected as a run attempt. The shared, store-independent intent contract
  keeps that projection boundary from importing an execution lane.
- **Confirmed and fixed, divergent publication authority:** a returned run
  cannot now simultaneously claim a ready and a blocked publication milestone.
  Those outcomes are mutually exclusive workflow verdicts, so the projection
  rejects the corrupted snapshot rather than selecting the later row. The
  dispatch-intent kinds are also a typed, exhaustive domain vocabulary; adding
  a lane now requires registration instead of silently falling through a
  string switch.
- **Confirmed and fixed, pre-admission submissions:** `run_submitted` names a
  reserved invocation before the workflow creates its first attempt. The
  projection authenticates that durable outbox intent against a declared run
  stage, rather than treating an empty attempt list as corruption. The client
  likewise withholds a round label until an attempt exists.
- **Confirmed and fixed, convergence fixture drift:** the dev-only real-daemon
  seed route had skipped the intent that production records atomically with a
  submission. It now writes that same durable production intent, so the
  convergence suite exercises the authenticated contract instead of an
  impossible projection.
- **Confirmed and fixed, clock-relative liveness and terminal disagreement:**
  timeline snapshots now carry their daemon-clock projection instant, which
  the client uses to classify observation freshness. A device clock therefore
  cannot manufacture a gap or retain stale liveness. The projection also
  rejects an observation whose status contradicts a re-authenticated export,
  outcome, or terminal authority.
- **Confirmed and fixed, projected item retargeting:** every attention item
  considered as run authority now re-binds its project and complete run
  subject before its type-specific evidence can authenticate an outcome.
- **Confirmed and fixed, misleading or stranded UI state:** terminal schedules
  no longer render as live; project changes and restored project sets repair
  selection; timeline failures stop the loading state; epoch plus revision
  retriggers a selected timeline; milestone-specific terminal, outcome, and
  blocked-reason fields render explicitly.
- **Confirmed and fixed, stale lost observations:** an invocation reported as
  `gone` is nonterminal. Once its observation ages out, the timeline presents
  an observation gap rather than a stable terminal state, preserving the
  distinction between missing telemetry and a completed execution.
- **Accepted boundary, typed current holds:** a current hold is an observation,
  not a workflow verdict. It is validated and bound to an invocation owned by
  the run, but it is not promoted into independent execution or publication
  authority. The contract and UI call these “daemon observations”; only the
  separately authenticated terminal milestones drive `RunOutcome`.
- **Rejected as out of scope, degraded readiness:** the lenses did not find a
  safe reason to absorb #692. Its durable readiness vocabulary remains a
  separate safety-policy unit.

The implementation also shares the fail-closed run scanner between `GetRun`
and the revision-bearing `GetRunSnapshot`; the corruption suite invokes both
accessors against divergent lookup columns.

## Returned-Object Boundary Contract

Review hardened `authenticateRunObservation` one finding per round, and each
fix enlarged the surface the next round examined. This section pins the
boundary's contract so the class closes at once and later findings get a
standing disposition instead of a round.

The sync read boundary proves three things for every projected fact, inside
the read transaction that returned it: the backing snapshot carries the
transaction's server authority; the fact's identity binds to the returned
run's exact project, run, stage, and attempt; a first-order durable
authority record exists and agrees field-for-field (reserved or dispatched
outbox intent, admission, execution export or outcome record, terminal
record, ready-item PR binding, workflow-owned ready or blocked item). The
mechanical criterion for the line: a single-record fetch plus field-equality
binding is inside this boundary; a multi-record reconstruction or derivation
is not.

The boundary does not re-execute engine recovery. Reconstructing a blocked
task's checkpoint and import account (`recoverDefinitiveBlockedTask`,
`compatibleTerminalItem`) is publication-watcher work; repeating it in the
read path would duplicate the engine decision-for-decision, and the regress
has no floor, because every durable authority record could in turn demand
re-derivation of the records behind it. The writers of those records and
startup recovery own their internal integrity; the read boundary owns
binding the projection to them and failing closed.

Standing disposition: a finding that names a missing first-order binding is
a gap and gets fixed; a finding that asks the read boundary to re-run a
recovery-owned reconstruction is declined with a pointer to this section.
The within-line space is pinned by a table-driven forge corpus in the signet
package (`run_observation_corpus_test.go`) that enumerates every milestone
and item kind against every forgeable binding field; that fixture list is
the spec.

The conversation-start binding is the sharp case of this line. A conversation
(discuss) invocation is dispatched unbound: the engine records it as a run
attempt and appends its `invocation_started` milestone without an execution
admission, and admission records are optional by construction (an engine with
no admitter configured records none). So the read boundary binds a
conversation start not to an admission but to the engine's deterministic
attempt identity (`attempt-<invocation>`, recomputed at the boundary the same
way the publication invocation id is), which the engine enforces when it
records the attempt. That catches a run graph that retargets the invocation to
a different attempt. Retargeting it to a different stage has no first-order
authority for an unbound invocation, so it stays on the run-record-integrity
side of the line, owned by the attempt writer, not re-derived here.

Refute-first dispositions, this round:

- Confirmed and fixed, conversation-start attempt binding (Codex sync.go:815):
  a corrupted run graph could attach a conversation invocation to another
  attempt. Now bound to the deterministic attempt identity above. An earlier
  fix that bound it to a durable admission was rejected by verification: it
  fails legitimate unbound conversation starts closed (seven engine and cmd
  suites), because those invocations carry no admission.
- Declined, beyond the line (Codex sync.go:762): re-running
  `recoverDefinitiveBlockedTask` / `compatibleTerminalItem` in the read path
  is the recovery reconstruction this section excludes.
- Base integration (#719): admission refusal now records a hold on the
  reserved invocation before it becomes an attempt (an identity-parallelism
  limit or a backend below the floor). The hold binding accepts a reserved
  invocation under the same reserved-intent authority the `run_submitted`
  milestone uses, so a pre-attempt hold projects while a hold on an
  unreserved invocation still fails closed.

## Revisit When

The daemon introduces pagination or compaction for observation history. The
timeline resource can then become a cursor-bearing stream without changing the
run summary contract.

A demonstrated corruption class forges a consistent pair of workflow-owned
records across the boundary line above. That is the trigger to move the
projection to derive-and-compare (recompute the observation from durable
authority and require wholesale equality) as its own unit, rather than
deepening per-fact authentication.
