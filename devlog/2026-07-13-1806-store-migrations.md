---
run: manual
stage: store-migrations
date: 2026-07-13
branch: feat/store-migrations
---

# Store, migrations, and outbox (Wave 0 unit 3)

Spine-role session: Wave 0 unit 3 (#8, `kind:contract`), the daemon's
durable storage layer: SQLite with the §5.2 pragmas, embedded
transactional migrations, the persisted form of the unit-2 domain
types, inbox/outbox with idempotency keys (§5.9), and ServerState with
once-per-client-visible-transaction revision semantics (§5.14).
Storage only; no sync endpoints, no engine. Selection was mechanical
per tracking issue #4: #8 is the first unchecked box, its dependency
#7 merged as PR #19, no open claim. Declared paths held, with two
called-out adjacencies (below). PR #20.

## Decisions

- **`modernc.org/sqlite` (pure Go, database/sql), the module's first
  dependency.** cgo (mattn/go-sqlite3) would cost the
  single-static-binary goal (§5.2) and complicate dual-platform CI;
  zombiezen's bespoke non-database/sql API is a niche every later lane
  would have to learn. Decisive detail: modernc's `_pragma=` DSN
  parameters run on **every** new connection, which is the only sound
  fix for pragmas being per-connection state under a database/sql pool
  (a `db.Exec("PRAGMA ...")` configures one pooled connection and
  leaves the rest at defaults). User approved the dependency at plan
  review.
- **`daemon/migrations/`, not repo-root `migrations/`.** The contract
  text and glossary said `migrations/`, but go:embed cannot reach
  outside the module rooted at `daemon/`, and reading SQL from disk at
  runtime would abandon both "embedded" and the static-binary goal.
  The files live in a tiny `daemon/migrations` package exposing an
  `embed.FS`; the glossary row now says `daemon/migrations/`. User
  approved the deviation at plan review; called out in the PR body.
- **Aggregate-root rows with a canonical-JSON body, not
  fully-relational columns.** One table per aggregate root: identity
  and join keys as real columns (so foreign keys actually enforce:
  delivery→item, finding→run, classification→finding, policy→run,
  item→conversation), `entity_version` + `as_of_revision` for §5.14
  sync, and the domain type's canonical JSON as the body. Children
  stay embedded in their root's body (Run⊃Stage⊃Attempt,
  Conversation⊃Message), matching Phase 1 whole-snapshot-per-
  conversation sync. Rejected: a column per field and child tables now
  (large row↔struct mapping surface for 12 types, and every domain
  field tweak becomes its own kind:contract schema migration).
  Explicitly a two-way door, confirmed with the user: extracting a
  field later is an ordinary migration (`json_extract` backfill or a
  generated column), normalizing a child is `json_each` backfill, and
  the store's Put/Get API is unchanged by either.
- **Write vs WriteInternal.** `Write` is the single client-visible
  transaction path: it stamps the committing revision into the Tx for
  entity rows and bumps `server_state.revision` exactly once at
  commit. `WriteInternal` shares the transaction shape without the
  bump (inbox intake, dispatch bookkeeping); `Read` is a consistent
  snapshot. The split makes "is this client-visible?" an explicit
  call-site decision instead of a property inferred later.
- **Epoch semantics.** `NewEpoch` (restore path) issues a random
  128-bit epoch and deliberately does not bump the revision: the epoch
  change itself invalidates every cursor, and a monotonic never-reset
  revision can never be ambiguous between epochs. `Open` seeds the
  first epoch idempotently into the migration's empty-epoch row.
- **One-connection pool.** SQLite has a single writer regardless;
  serializing in Go avoids SQLITE_BUSY under self-contention, and
  `busy_timeout` still guards cross-process access. `_txlock=immediate`
  converts upgrade deadlocks into timeout waits. Widening later (a
  read pool) is internal to the store package. An internal test widens
  the pool to two to prove the DSN configures every new connection.

## Conventions introduced (flagged for spine review)

Documented at point of use (`store/doc.go`, `migrations/migrations.go`);
recorded here for the Wave 0 exit review:

- **Migration mechanism**: `NNNN_description.sql`, contiguous from
  0001; one transaction per file with its `schema_migrations` row
  inserted inside it (a failing migration rolls back completely, the
  prior version stands); files are immutable once merged (a diverged
  applied history is a hard error); no BEGIN/COMMIT inside files.
  `migrate` takes an `fs.FS`, so failure tests substitute an
  `fstest.MapFS` rather than checking in broken SQL.
- **Persistence pattern**: aggregate-root table + extracted key
  columns + canonical-JSON body, STRICT tables throughout. Gotcha
  worth knowing: STRICT TEXT columns reject `[]byte` binds (BLOB
  affinity), so JSON bodies bind as `string`.
- **Validation at both edges**: every Put validates before writing,
  every Get validates after reading (`Validate` is the domain
  contract's deserialization backstop), so a corrupt row fails loudly
  at the boundary.
- **Constant SQL only**: statements are spelled out per entity as
  constants; no SQL is assembled at runtime (also keeps gosec quiet
  without suppressions).
- **Timestamps are Go-written RFC3339Nano UTC TEXT**; the store never
  relies on SQLite clock functions or driver time conversion.

## Deferred

Nothing escalated to issues; all are the next units' contracted scope,
not gaps: outbox dispatch/consumption and any `Tx` write-guard for the
Read path (engine, Wave 2); device/pairing tables and sync endpoints
(§5.14, signet/1A.0); backup checkpoint tables (§5.10, later 1A);
CI dependency caching flipped on in this PR per the bootstrap entry's
pre-recorded instruction, closing that thread.

Pre-existing queue swept: the licensing ADR-candidate is escalated
(`-> Refs #18`) and the bootstrap entry's done-block wording item is
routed to the agent-setup skill (external); nothing drainable in this
scope.

## Review rounds

**Round 1 — three Codex P2 (five threads, two duplicated) plus two
maintainer findings; all five accepted, folded by class into their
owning commits.**

- **Path not URI-escaped (maintainer, P1).** The DSN rides a `file:`
  URI whose parser cuts the query at the first `?` and decodes percent
  escapes, so a legal path containing `?` opened a *different* database
  file (state split). The path is now escaped via `url.URL.EscapedPath`
  (encodes `%`/`?`/`#`, keeps `/`); the regression test opens a store
  under a directory named `we?ird #dir 100%` and proves reopen sees the
  same data at exactly that path.
- **Body/key-column consistency (Codex).** The store's foreign keys and
  lookups act on the extracted columns, but Gets trusted the JSON body,
  so a row whose body disagreed with its columns returned as trusted
  domain data: the returned-object-trust-boundary class. Every Get now
  selects the extracted columns alongside the body and fails loudly
  (`errRowInconsistent`) on any disagreement, covering both the queried
  key and join columns the lookup did not filter by.
- **Migration digests (Codex).** Divergence detection was name-only, so
  a same-name rewrite silently diverged existing and fresh databases.
  `schema_migrations` now records a sha256 content digest per applied
  file and `migrate` hard-errors on mismatch: file immutability is
  enforced, not conventional.
- **Queue identities (Codex).** Empty idempotency keys collapsed
  unrelated actions onto one row; empty kinds were unroutable. Rejected
  in Go (named errors) and by `CHECK` constraints in the still-unmerged
  0003 migration.
- **Busy-timeout truncation (maintainer, P2).** A negative or
  sub-millisecond `Options.BusyTimeout` truncated to `busy_timeout(0)`,
  silently disabling waiting. `Open` now refuses both; zero still means
  the default.

Class sweep notes: the consistency check was applied to all nine Gets,
not the cited ones; the empty-identity check has no sibling surface
(entity Puts already validate identity via domain `Validate`, migration
names via the regex, epochs are generated); `BusyTimeout` is the only
duration option. The refute-first pass for the trust-boundary finding
is the new `TestGetRejectsInconsistentRow` writing corrupt rows past
the Put boundary via raw SQL.

**Round 2 — one Codex P2, accepted and swept as a class.** The
classification upsert overwrote a historical version in place: the
domain corrects a Classification by minting version+1 (`Annotate`),
so a same-(finding_id, version) write with different fields erased
history while sync saw an ordinary update. The class is "a store
upsert subverting a domain immutability contract", drawn from the
domain package's own docs: write-once records (Artifact's one-digest
identity, AgentInvocation's input binding, Finding's immutable
observation, Classification versions, ResolvedPolicy's digest-bound
resolution) now insert with ON CONFLICT DO NOTHING and tolerate only a
byte-identical replay (canonical `json.Marshal` is deterministic, so
retries converge with no entity_version churn); a rewrite fails with
the exported `ErrImmutableConflict`. Current-state aggregates (Run,
Conversation, AttentionItem, AttentionDelivery) legitimately keep the
upsert; a Conversation re-Put can still replace message history, which
is append discipline for the engine/signet transaction shape (§5.14),
not enforceable row-by-row in a storage-only unit.

**Round 3 — three maintainer findings; one stale, two accepted as the
class one level up.** The first ("immutable records still use
destructive upserts") was evaluated against the pre-round-2 head; the
current head already rejects those rewrites (`putImmutable`,
`TestPutImmutableConflict`), and Codex's own re-review of that head
came back clean (clean-pass reaction). The other two are the same
contract family widened from write-once *rows* to the *fixed parts of
mutable aggregates*, accepted and swept together:

- **Run retargeting**: `PutRun` on an existing run now rejects any
  change to `ProjectID`, `SpecDigest`, or `PolicyDigest` (fixed at
  creation, §5.3 binds a run to its digests) and requires stages and
  attempts to extend append-only (`stagesExtend`: identities, names,
  and recorded attempts unchanged). `TestPutAgainUpdates` previously
  blessed a `SpecDigest` change as the upsert example (my miss); it
  now demonstrates legitimate evolution (append an attempt and a
  stage) and `TestRunFixedBindingsAndHistory` covers the six rewrite
  shapes. The run's `policy_digest` is extracted as a column, and
  `PutResolvedPolicy` cross-checks its digest against it (a resolved
  policy that disagrees with its run's binding is rejected).
- **Conversation truncation**: `PutConversation` requires the stored
  messages to be carried byte-for-byte (canonical-JSON prefix) and
  only appended to (§5.14 messages immutable, corrections are new
  messages). `TestConversationAppendOnly` covers drop, rewrite, and
  the appending happy path.
- **Class sweep to the remaining aggregates**: `PutAttentionItem` now
  fixes what an item is about (`ProjectID`, `Subject`, `Type`);
  transitions (status, evidence, item_version) evolve on the same
  identity, a different subject is a new superseding item (§4).
  `AttentionDelivery` deliberately keeps a free upsert: its identity
  is the full tuple key and body evolution *is* the delivery
  lifecycle, with ordering enforced by domain `Validate`.

**Round 4 — two Codex P2, both accepted: the monotonic-progression
dimension, completing the guard family.** Round 3 fixed what may never
change; round 4 fixes what must only move forward, on exactly the two
mutable aggregates round 3 left with a free upsert:

- **Stale item bodies**: a re-put with a changed body must advance
  `item_version` (a resolved v2 could otherwise be rolled back to an
  open v1); a byte-identical replay converges silently with no write
  and no entity_version churn.
- **Delivery lifecycle regressions**: `PutAttentionDelivery` now
  requires the status rank to strictly advance
  (submitted → channel_accepted → opened) and already-recorded
  receipts to be preserved; a stale retry can no longer roll an opened
  delivery back and drop the receipts timing aggregates depend on.
  This supersedes round 3's "deliveries keep a free upsert" boundary:
  the reviewer was right that lifecycle evolution still needs a
  direction.

New exported sentinel **`ErrStaleWrite`**, distinct from
`ErrImmutableConflict`: version/lifecycle conflicts are the §5.14
optimistic-concurrency signal a caller may handle (refetch, rebase,
retry), while an immutability violation is a contract bug. Guard order
on both: identical-replay skip, fixed-binding check, then staleness.
Sweep check: Run and Conversation already enforce forward-only via
append-only history (round 3); ServerState.revision is
internally-bumped only; write-once records reject any change. The
family (fixed bindings, append-only history, forward-only lifecycle,
write-once) now covers all nine persisted shapes.

**Round 5 (maintainer design review) — three points, two acted on, one
answered.**

- **ReadTx/WriteTx split, done now.** Read was read-only by convention
  only (its callback received the same `*Tx` carrying Puts), so an
  accidental mutation without a revision bump was one call away.
  Callbacks now receive `*ReadTx` (Gets, ServerState) or `*WriteTx`
  (embeds ReadTx; Puts and queue methods, carries asOfRevision):
  mutating outside a write path does not compile. Done pre-engine, per
  the maintainer's timing call, while the only call sites were tests.
- **Reader/writer connection split, deferred with its instruction.**
  `_txlock=immediate` makes every transaction, Read included, take the
  write lock at BEGIN: acceptable and deliberate on today's
  single-connection pool, wrong for a future read pool. doc.go now
  carries the caveat (a read pool must use a separate deferred-txlock
  reader DSN, never reuse this one); drains with whichever unit widens
  the pool.
- **Domain transition validators, escalated.** The transition rules
  (fixed bindings, append-only histories, forward-only lifecycle)
  belong in the domain vocabulary with the store enforcing them, but
  `daemon/internal/domain` is outside this unit's declared paths, so
  the move is a contract change -> Refs #21 (kind:contract, deferral).
- **Migration naming under parallel agents, answered as designed.**
  Contiguous NNNN is deliberately hostile to parallel authorship: the
  protocol serializes migration work (kind:contract, exclusive; plan
  §11 marks migration PRs exclusive), and a collision is a loud
  contiguity/digest error at merge, not a silent ordering ambiguity.
  Rejected timestamp naming, which buys parallel authorship the
  project forbids at the cost of deterministic ordering; the rationale
  now lives in migrations.go's doc.

**Watch-protocol gotcha (recorded for the review-watch habit).** Two
reviewer passes were missed by the watcher for different reasons, both
operator error, neither the mechanism: (1) a review racing a
force-push is stamped with the superseded head, so a head-filtered
watch armed on the new head silently discards it (the script documents
this; after any force-push, check for a stale-head pass before
waiting); (2) the second watch was armed with a hand-expanded full SHA
whose tail was wrong, so the `startswith` head filter matched nothing.
Full SHAs must be captured from `git rev-parse` in the same step that
uses them, never expanded from a short form.

## Verification

- Passed: `go build ./...`, `go test ./...`, `go vet ./...`,
  `golangci-lint run` clean at every commit (each commit verified
  green in sequence during development).
- Passed: the seven acceptance fixtures map 1:1 to tests:
  `TestOpenPragmas` (1), `TestMigrateFreshAndIdempotent` (2),
  `TestFailingMigrationRollsBack` (3), `TestQueueIdempotency` (4),
  `TestWriteIncrementsRevisionOnce`/`TestNewEpoch` (5),
  `TestForeignKeysEnforced` (6), `TestGoldenRoundTrip` (7).
- Checked: the store's nine golden files are byte-identical to the
  domain package's for the shared shapes (`diff` on run.golden;
  the round-trip test asserts serialized equality for all nine).
- Checked: docs coherent for the touched scope: glossary spine row
  updated (`daemon/internal/store`, `daemon/migrations/`);
  daemon/README's placeholder-package status line remains true (store
  is a new package, not a lane placeholder).
