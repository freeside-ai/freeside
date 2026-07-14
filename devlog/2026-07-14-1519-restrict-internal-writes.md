# Restrict internal writes to non-synchronized state (Wave 0 exit, #38)

Track A exit fix. Issue #38, PR #53. Declared paths: `daemon/internal/store`.
`kind:fix`, spine-owned. Chain predecessors #33/#32/#37 all merged; #38 is the
non-contract Track A tail and had no path-colliding open PR, so it was
takeable.

## Decisions

- **Structural type boundary over a runtime guard (user choice).** `Write` and
  `WriteInternal` handed the callback the same `*WriteTx`; only a
  `clientVisible bool` decided the `server_state.revision` bump, so an internal
  transaction could `Put` a synchronized entity and commit undetectably. Chose
  a three-tier handle lattice — `ReadTx` (Get*) ⊂ `InternalTx` (+ queue
  methods) ⊂ `WriteTx` (+ the 10 `Put*`) — so `WriteInternal` yields
  `*InternalTx` and a synchronized write through the non-bumping path *fails to
  compile*. Rejected the runtime alternative (single `WriteTx` + `internal`
  flag + per-`Put` sentinel error) because it re-creates the per-call-site
  convention #38's Contract explicitly rejects ("a store invariant, not a
  convention for future callers"): a future `Put` whose author forgets the
  guard ships the exact silent bug. Mirrors the existing `ReadTx`/`WriteTx`
  "does not compile" ethos (doc.go) and the #32/#37 make-invalid-states-
  unrepresentable precedent.
- **`asOfRevision` stays on `WriteTx`, not `InternalTx`.** Only `Put*` stamp
  it; the internal path then needs no `server_state` read at all, reinforcing
  that an internal tx has no revision concept in its type. `transact` (the
  shared `clientVisible` switch) is deleted and the two paths inlined.
- **Acceptance #3 met structurally, not literally.** Under the type split
  `internalTx.PutRun(...)` cannot compile, so the "attempt every write through
  the internal path" test is a compile guarantee, not a runtime call. The
  runnable proof is a reflection test (`internal_tx_test.go`): every `Put*`
  (discovered by prefix-scan, so a future `Put` is guarded without a test edit)
  must be present on `*WriteTx` and absent on `*InternalTx`, and the queue
  methods must be on both. `revision_test` now runs a real `EnqueueOutbox`
  through `WriteInternal` and asserts no bump (Acceptance #2).

## Refute-first pass (store trust / data-integrity boundary)

One fresh-context reviewer, tasked to disprove the boundary across six angles
(internal→synchronized reachability, bump exactly-once, capability loss,
reflection-test soundness, rollback integrity, consumer/API regression).

- **Confirmed:** none. No scenario produced a synchronized write on the
  non-bumping path, a double/missed bump, or a partial commit.
- **Rejected-by-verification (so not re-raised):** `InternalTx` exposes no
  `Put`/raw exec (`tx *sql.Tx` unexported); inbox/outbox tables carry no
  `as_of_revision` (`0003_inbox_outbox.sql`), so they are genuinely
  non-synchronized. `Write` bumps once, `WriteInternal` never reads/writes
  `server_state`. Queue methods promote onto `WriteTx` (still callable inside
  `Write`). Reflection test is non-vacuous: a `Put` added to `ReadTx`/
  `InternalTx` promotes onto `WriteTx`, enters the prefix scan, and is then
  caught on `InternalTx`; `len(puts)==0` fatals if reflection breaks;
  `NumMethod` ignores unexported helpers. Both paths `defer Rollback` before
  any commit; post-commit `Rollback` returns `ErrTxDone`, discarded (standard
  idiom, no double-close).
- **Accepted-by-decision:** the guarantee rests on "synchronized write == a
  method named `Put*`". A future *store-package author* could add a synchronized
  raw-SQL write to `InternalTx` under a non-`Put` name; it would compile and the
  reflection test would miss it. In-bounds — #38 defines the synchronized
  surface as the `Put*` methods, and callers cannot do this (`tx` unexported).
  Recorded as the residual assumption; added a caution to the `InternalTx` doc
  directing authors to put any `as_of_revision`-carrying write on `WriteTx`.

## Verification

- Reflection test confirmed as a real guard, not vacuous (refute angle 4): it
  fails if any `Put` reaches the internal surface, including via embedding
  promotion. No golden changes (no schema/SQL touched).

## To promote

- **Candidate (new):** "A transaction that does not bump `ServerState.revision`
  cannot mutate synchronized state; the write methods for `as_of_revision`
  entities live only on the revision-bumping handle." This is a *distinct*
  store trust-boundary invariant from the still-open `approved-recipe-boundary`
  one (that is returned-object re-gating; this is write-path capability).
  Enforced structurally in code and documented in `doc.go`/`InternalTx`;
  promoting to an AGENTS.md trust-boundary line would be its own docs-gated
  change (Document gating), outside this `kind:fix` unit's scope, and gated on
  recurrence beyond this store like its sibling. Left open.
- **Queue swept.** The two pre-existing open items — `approved-recipe-boundary`
  store trust-boundary invariant (tracked by #52) and the `domain-package`
  conventions Wave-0-exit spine review (tracked by #27) — remain out of this
  unit's scope (both are docs promotions, not store code); neither drained, no
  spurious re-defer. No `needs-human` item is agent-actionable here.
