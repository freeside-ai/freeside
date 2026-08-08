# Freeze Codex Review After Reconstruction

Chose to keep the launch intent `preparing` through final reconstruction and
freeze it as `prepared` immediately before the durable `starting` transition.
This preserves journal-before-create ownership evidence for every resource
mutation while retaining the adapter's fail-closed rule that resources are
immutable after preparation. Replacing the second reconstruction pass with a
non-mutating approximation was rejected because it would weaken the final
runtime observation that closes the pre-start attachment window.

The later freeze made `preparing` with a persisted binding a normal durable
state. An independent refutation found that binding-presence checks could not
make request rejection safe: rejection could observe no binding immediately
before launch persisted one and continued to start. Chose a durable rejection
outcome as a transactionally checked preparation fence. Whichever write wins
determines the safe path: preparation wins and rejection authenticates and
reaps the resulting topology, or rejection wins and the adapter refuses the
`preparing` to `prepared` transition. A binding re-read alone was rejected as
time-of-check/time-of-use unsafe.

A second refutation found that the outcome fence alone did not serialize
cleanup with an in-process launch that was still creating resources. Chose a
per-run backend lifecycle gate over changing durable lease-adoption semantics.
The source holds the gate across workspace preparation, backend launch, and
proxy publication; direct backend callers hold it for the launch itself. A
rejection persists its outcome fence before waiting for the gate, then cleanup
reconstructs from the settled journal and runtime. This ordering also covers
the windows before `BeginIntent` and after `CodexReview` returns but before the
source publishes the proxy. The gate is intentionally process-local, so a
restarted daemon with no surviving launch proceeds directly to durable
recovery.

The source constructor now rejects a non-nil but uninitialized backend with
`ErrInvalidConfig`. This preserves the backend's existing startup contract and
prevents the source's per-run lifecycle gate from dereferencing its
constructor-owned map before configuration is valid.

The same fence can be committed before a launch writes either an intent or a
workspace. Recovery therefore enumerates durable outcomes as well as intents
and workspace bindings, validates a fence-only outcome, and marks it ready
only once there is no topology left to reconcile. Leaving outcome discovery to
later `Inspect` was rejected because generic cleanup rightly requires a
durable intent and would turn this safe no-launch crash window into a false
teardown contradiction.

Recovery was verified across a closed and reopened SQLite store with a fresh
production volume lifecycle coordinator after a simulated lost journal
response following binding persistence and a committed reconstruction resource
update. The surviving `preparing` intent authenticated cleanup, removed runtime
and snapshot-stage residue, and closed without being treated as prepared.

The opt-in Apple-container lifecycle proof crossed final reconstruction,
`prepared`, `starting`, and `started`, then left no review containers, volumes,
or networks. It also exposed an existing teardown identity defect: Apple
container preserves environment entries but may reorder them after start, while
the digest is order-sensitive. That separate defect is tracked in #606 and was
not folded into this resource-journal work.

Revisit when outcome persistence and intent transitions no longer share one
transactional store, or when the launch protocol gains another path from
`preparing` to an externally observable running state.

Follow-up: #606
