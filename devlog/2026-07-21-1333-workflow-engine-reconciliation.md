# Workflow engine: durable reconciliation over the existing ledger (#235)

Wave 2's first convergence unit needed a restart-safe engine without changing
the frozen signet, store, StageDriver, domain, migration, or API contracts. The
load-bearing choices are how the engine finds dispatched work after restart and
where it re-establishes authority before accepting a driver result.

## Decisions and rejected alternatives

- **Record the invocation as a Run attempt before `StageDriver.Start`, then
  mark the outbox row dispatched immediately after Start succeeds or reports a
  duplicate.** The attempt is the engine's durable restart index: dispatched
  rows correctly leave the store's pending scan, while every started
  invocation remains discoverable through the run aggregate. Reconciliation
  scans those attempts, inspects/collects by invocation ID, and delegates the
  completion transaction to signet's inbox-backed acceptor. Rejected: leaving
  the outbox row pending until local acceptance, which would misuse
  `dispatched` as `completed` and contradict the store contract; adding a
  dispatched-row query or new workflow table, which would widen the shared
  store/schema contract that #235 explicitly consumes unchanged; and keeping a
  process-local started set, which loses the kill boundary this unit exists to
  prove.

- **Use one fixed action at each walking-skeleton item: approve, then discuss.**
  The existing command store is keyed by command ID and intentionally exposes
  no item-indexed command query. Both approve and stop conclude an item as
  `resolved`, so an engine observing only the item could not distinguish them
  without a contract change. The 1A.0 fixture therefore offers only the action
  its specified path drives. Rejected: treating every resolved item as
  approval, which would silently convert stop into progress; and widening the
  command query contract inside this feature unit. This is concrete 1A.0
  behavior, not the general workflow action model.

- **Use the deterministic initial approval item as the fake workflow's
  ownership marker.** Run has no workflow-kind field, so a global scan that
  attached the 1A.0 state machine to every stored Run would corrupt later
  workflows. `StartFakeRun` creates the marker explicitly; ordinary reconcile
  ignores unmarked runs, the attempt scan skips them before inspecting their
  contents, and invocation dispatch and acceptance re-validate that same
  marker before touching a lookalike feedback stage. Rejected:
  inferring ownership from an otherwise generic Run shape, feedback-stage
  shape, or policy digest; none names a workflow contract.

- **Re-derive invocation authority from durable relationships and distrust
  both serialized queue payloads and returned driver objects.** Before Start,
  the engine decodes a closed payload shape and cross-checks its invocation,
  conversation, item, and run bindings against the stored AgentInvocation and
  run-scoped feedback item. Before acceptance, it validates the collected
  terminal result, requires its invocation ID to equal the recorded attempt,
  and requires a terminal status to agree with Inspect (except `gone`, whose
  contract is resolved by Collect). A foreign but otherwise valid result fails
  closed without entering the conversation or advancing its item. Rejected:
  trusting the outbox payload because it originated locally, or trusting the
  StageDriver's returned invocation ID because the call was keyed by the
  expected ID; both are reconstruction/returned-object trust boundaries under
  the repository's high-assurance rules.

- **Treat a gone invocation with no committed result as a loud engine error in
  1A.0.** The intent and attempt remain durable and no workflow result is
  invented. Retry and the execution-failure AttentionItem belong to the later
  real-work workflow; silently polling forever or synthesizing a successful
  reply would hide the loss.

- **Make fresh-daemon pairing a one-time startup capability while persisting
  only the ntfy topic-derivation key.** `freesided` generates an in-memory
  pairing HMAC key, mints one short-lived code, and prints that code in its
  startup readiness record; only the code digest enters SQLite. Hosted ntfy is
  the Phase 1 default. Its derive-all-topics key lives in a fixed 0600 sibling
  file, is rejected when symlinked, hard-linked, widened, or malformed, and is
  never regenerated beside an existing store. Rejected: an unauthenticated
  signet listener with no usable pairing path; persisting the pairing code or
  its plaintext; and silently re-keying existing device subscriptions.

- **Seed exactly one deterministic fake run in the 1A.0 daemon composition.**
  Startup calls the same idempotent `StartFakeRun` boundary tests and future
  initiators use, so a paired client immediately sees the initial approval and
  restart preserves existing progress. Rejected: requiring an in-process test
  hook or a hand-seeded database to begin the advertised standalone flow; a
  general run-submission API belongs to later workflow units.

## Refute-first verification findings

- Confirmed: restarting after the discuss commit but before dispatch finds the
  pending outbox intent, starts once, accepts once, and leaves a repeated
  reconcile revision-neutral.
- Confirmed: restarting after the permanent fake commits a result but before
  the dispatch marker observes a duplicate Start and accepts once; restarting
  after the dispatch marker has removed the pending row discovers that same
  result solely through the recorded Run attempt and accepts/advances once.
- Rejected by verification: a driver can return a well-formed result under a
  foreign invocation ID and reach signet. The adversarial driver fixture is
  stopped at the engine boundary; the legitimate attempt remains recorded, the
  conversation remains awaiting the expected agent, and the item version does
  not advance.
- Rejected by verification: malformed queue payloads can gain authority
  through unknown fields, trailing JSON, missing identities, or a non-positive
  item version. The closed decoder rejects each class before any driver call.
- Rejected by verification: a failed terminal result can be rendered as a
  successful agent reply. The failure leaves its durable attempt in place and
  the conversation awaiting; 1A.0 fails loudly until later workflow policy
  creates the execution-failure path.
- Rejected by verification: another workflow can reproduce the deterministic
  feedback item and stage shape and have this engine dispatch its invocation.
  Run and outbox selection skip absent ownership markers without consuming
  foreign work; owned binding requires a resolved initial marker and the full
  feedback-item shape. Reconstructed acceptance also re-derives the
  deterministic attempt ID. Lookalike and retargeted state remains untouched.
- Rejected by verification: a credential file can be substituted through a
  symlink or hard link, exposed through widened permissions, truncated and
  regenerated, or lost beside an existing store without stopping startup.
  Each case fails closed; a fresh private key reloads byte-identically.

Follow-ups: #41 extends these in-process reconstruction boundaries to the real
daemon SIGKILL matrix; #241 binds completion atomically to its expected
attention item when later workflow concurrency makes the current preflight
check insufficient.

Revisit when a workflow must offer multiple concluding actions whose item
statuses coincide, or when workflows need invocation states richer than the
append-only Run attempt plus signet conversation status. That is a shared
contract unit, not an engine-local inference.
