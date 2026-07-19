# Decision-instant contract: placement, stamp rule, and the no-migration deviation

Work unit #171 (spine `kind:contract`, last of the three finding-remediation
prerequisites; source note
`2026-07-18-1125-finding-contract-prereqs.md`). Adds the durable
decision/acceptance timestamp the open-to-decision metric needs after a
restart; consumer #164 binds to this shape. Status lives on the issue and PR,
never here.

## Decisions and rejected alternatives

- **The field lives on AttentionItem (`decided_at`, optional pointer), not on
  the Command record.** The issue left placement open ("AttentionItem or
  command-result aggregate"). Command placement is not merely worse, it is
  broken: Command is write-once and byte-canonical, and PutCommand converges a
  retry by byte-comparing the rebuilt body against the stored row. A
  server-stamped instant in that body would make every legitimate retry
  byte-differ and fail as a false immutable conflict. A column-only stamp
  outside the body was rejected too: it would be store metadata with no domain
  shape, satisfying neither the golden nor the validation acceptance criteria.
  The item field follows the `Timing` precedent (derived, never caller-set;
  excluded from `AttentionItemInput`) and the `WaiverRecord.DecidedAt`
  naming/UTC discipline.

- **Only the first concluding command stamps (owner decision).** The issue's
  "the transaction that records the first command result" is read as
  first-vs-replay, not first-of-any-kind. Records-only actions are documented
  non-decisions ("mark_seen decides nothing", signet `actionOutcome`), discuss
  is not a decision, and #164's own evidence derives the metric from "the
  concluding command". Literal any-first-command stamping was rejected: it
  would give records-only actions an item write they are documented not to
  have, and version churn would invalidate prepared commands. Because a
  concluded item accepts no further commands, at most one concluding command
  ever commits, so the stamp is set exactly once by construction; the replay
  branch rolls its transaction back before the stamping point.

- **Body-only persistence; no migration (owner decision, deviation from
  acceptance criterion 3).** Items persist as JSON body plus key columns, and
  the schema's own doc calls column extraction "an ordinary migration
  (json_extract backfill)" done when a consumer needs it. No consumer needs a
  `decided_at` column: the metric's other endpoint (`opened_at`) lives in
  delivery bodies, so a column would not enable SQL-only derivation. The
  migration + round-trip criterion is superseded by the reopen test
  (`TestDecisionInstantsSurviveReopen`), which proves durability against a
  fresh handle. Recorded on #171.

- **Immutability is a transition rule, not a validation rule.**
  `ValidateAttentionItemTransition` refuses to move or erase a recorded
  `decided_at` (nil → set stays legal), which protects the stamp from every
  writer holding a constructor-built copy (constructors always produce nil),
  including the dev harness and future engine/expiry writers. Coupling
  presence to terminal status inside `Validate` was rejected in both
  directions: terminal⇒stamped would brick pre-#171 rows on store decode
  (Validate re-runs at reconstruction), and expired/superseded items
  legitimately carry no decision.

## Verification (refute-first pass)

Two independent refute lenses ran against the replay-stability and
erasure/compat claims before handoff, with probe tests executed against the
branch. Dispositions:

- **Rejected by verification (claims hold; do not re-raise):** an idempotent
  replay can never move, re-stamp, or erase the instant, and never fails a
  byte-identical retry (replay is judged command-id-first and its transaction
  rolls back before the stamping point; probes included a 48h clock advance
  and a changed `ExpectedEntityVersion`). No command interleaving
  (discuss/supersede, records-only, cross-device conflict, closed-item
  rejection) produces a second stamp or erases one; completion and timing
  re-puts rebuild from the durable row; altering or erasing a stored stamp
  fails `ErrImmutableTransition`; no production delete path exists for items.
  Pre-#171 bodies decode to nil and validate; the generated Swift client's
  synthesized Codable tolerates the key's absence and presence.
- **Confirmed and fixed:** `Service.PutItem` accepted a caller-supplied
  `decided_at` (Validate deliberately doesn't couple it to status, and the
  transition guard's nil→set arm is writer-agnostic), which would forge the
  metric endpoint and permanently wedge an open item — the concluding stamp
  refuses an already-recorded instant. Fixed at the intake boundary:
  `ErrCallerSetDecidedAt`, failing closed before any Write, with a regression
  test.
- **Accepted by decision:** (a) direct `store.WriteTx` writers below signet
  can still insert or nil→set a stamp; they are internal trusted code, the
  same trust class as `Timing` (also convention-guarded), and the wire
  exposes no item-write route. (b) A same-version idempotent re-put of an
  unchanged **pre-#171** row no longer byte-converges (the new encoding adds
  `"decided_at":null`), surfacing as `ErrStaleTransition`; this
  upgrade-convergence window is a class every body-field addition shares
  (#173's required AgentClaim provenance was strictly harsher: old rows
  failed decode outright) and pre-1A databases are disposable by that same
  precedent. (c) A corrupted open+stamped row wedges: Submit fails closed on
  it loudly rather than silently accepting corrupt state, per the
  reconstruction-gate house style.

## Revisit when

- An expiry sweep or a Wave 2 engine writer starts concluding items: those
  paths must not stamp (expiry is not a decision); the transition guard
  already blocks erasure, but stamping semantics for engine-driven
  conclusions will need an owner call.
- A consumer needs SQL-level `decided_at` queries: extract the column with an
  ordinary json_extract-backfill migration and add the scanner cross-check.
- `snooze` lands (#22 widening): a snoozed-then-decided item keeps
  first-conclusion semantics unless the owner says otherwise.
