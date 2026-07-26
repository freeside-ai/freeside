# Publication Invocation Reservation

Work unit: #308. Scope: `daemon/internal/store`, `daemon/internal/publish`,
`daemon/internal/engine`, `daemon/internal/integration`, `devlog/`.

## Decisions

**Occupy the contested key rather than reserving beside it.** The reservation
is written at `publish/<invocation-id>/publish.publication` itself, under kind
`publish.invocation_reservation`, not at a sibling key. The outbox is unique by
idempotency key alone, and both store-backed writers already re-checked the
kind of the row `EnqueueOutbox` converged on, so occupying the key makes an
unaware writer fail closed with no cooperation from it. The rejected
alternative was a side namespace every intent writer consults inside its own
transaction: it keeps the outbox append-only, but it converts a structural
invariant into a convention Go cannot enforce, and "a writer forgot to consult
the reservation" is the exact failure #308 exists to close. The strongest
available mitigation for that alternative (unexporting the intent kind so no
package outside `publish` can name it) still leaves an in-package writer
needing nothing but the string literal.

**Settle the reservation in place instead of deleting and re-inserting.**
`store.PromoteOutbox` refines the row from the reservation kind to the
publication kind under a guard on the exact current kind, payload, and pending
status. Delete-then-insert was rejected: it releases the key inside the window
it was taken to protect, and it destroys the row identity an append-only intent
ledger keeps. Preserving `id` means a settled intent holds its admission-time
slot in `ListPendingOutbox` order rather than its settlement-time slot; no
correctness dependency on that order was found, since each drained row is
independently idempotent. In-place `kind` mutation is new for the `outbox`
table, and is acceptable only because `PromoteOutbox` is its single path and is
fully guarded. Affecting no row is deliberately not an error: a retried
promotion after a crash is ordinary, so the current row is returned and the
caller decides whether what it found is its own already-settled intent.

**Keep the engine's publication-role owner row.** Settling consumes the
reservation payload, and `publish.Intent` carries no run ID, so subsuming the
owner row would leave no durable run-to-invocation binding for
`validateFakePublicationReconciliation` at the moment reconciliation most needs
one. The two rows assert different things: the owner row binds the invocation
to a run for the engine's own checks, the reservation denies the key to other
writers.

**The run ID is the whole ownership proof, and it is not adversarial.** The
engine's publication-role binding digest was already `digest({run_id})`, so a
separate opaque digest would carry no information the run ID does not. What
this enforces is that two *different* owners cannot collide on one invocation;
any code holding the run ID can present a matching claim. Publish only compares
equality, so a future owner can strengthen its binding without a publish
change. Stating the limit is the point: this is a defense against a confused or
buggy writer, not against a hostile in-process caller.

**No migration; reconciliation backfills instead.** The schema is unchanged and
no row is rewritten. A task admitted before this change carries no reservation,
and recovery reaches reconciliation without passing through admission, so a
restart alone would never install one and that task's key would stay
unprotected for the rest of its life. Reconciliation therefore validates the
task and claims the invocation in one write transaction, so no writer can take
the key in the gap between them. Claiming is idempotent for a task that already
holds its reservation and accepts the settled intent of one that already
published. This was chosen over an upgrade-time backfill migration: the claim
path is already convergent, so recovery gets the repair for free, where a
migration would have to hand-construct the reservation payload in SQL and
duplicate an encoding contract the Go side owns and a golden test pins.

**A legacy task cannot prove ownership of an intent, and that is not fixable
here.** For a task that never held a reservation, a publication intent already
at its key is accepted by the claim, because `publish.Intent` carries no run ID
and there is no durable evidence to distinguish that task's own pre-upgrade
intent from a foreign one. Reconciliation then fails on the intent-versus-
candidate cross-check, before any external effect. That is the correct outcome
either way: a foreign intent means the task genuinely cannot publish under an
invocation ID it cannot renegotiate, so detecting it at claim time rather than
a few steps later would change the error, not the fate. The irreducible
residue is that a database straddling this change, with a second publisher
writer running, has one unprotected window per legacy task; that writer does
not exist until #288 or #301, and fake publication admits only
`attended_dev`, so a straddling task is a dev-loop artifact.

**Two existing assertions moved with the mechanism.** A second run reusing a
publication invocation is now refused by the reservation gate rather than the
owner-row mismatch, which is earlier (before any of the second task's state is
committed) and more specific, so the test asserts `ErrInvocationReserved`. The
terminal-substitution fixture now seeds its intent through the task's own
claim, which is the path a real publication takes; recording it without a claim
is what a foreign writer attempts, and is refused. Neither change loosens what
is checked. `TestFakeCandidatePublicationRejectsPreexistingPublisherIntentAtAdmission`,
the "existing publisher intent still rejects admission" criterion, is unchanged.

**A reservation is never released, and that is the intended lifetime.** A task
that ends blocked without publishing leaves its reservation pending forever.
Releasing it was rejected: the invocation ID is burned either way, so a release
would only let a different owner take an ID whose first owner already made
durable decisions under it. The reservation therefore behaves exactly like the
never-deleted invocation owner rows beside it. The residue is accumulation, one
dead row per blocked publication, which is the pre-existing outbox retention
question rather than a new one. Note that today's bound on the blast radius is
an accident of the caller: `daemon/cmd/freesided` derives the publication
invocation as `"publish-" + RunID`, so no later run reuses a burned key. The
reservation does not enforce that derivation.

## Verification Findings

Every commit builds and passes the daemon suite on its own. Regressions pin the
race deliberately, since it is unreachable in-tree today (one process, one
non-test `Publisher` construction site) and becomes reachable with #288 and
#301: a task is admitted, a second store-backed writer reaches the invocation's
key with no claim and with another run's claim and is refused without moving
the row, and the owning task then publishes onto that same row. A concurrent
variant runs many writers at once against one reserved invocation and asserts
the only committed intent is the owner's; a foreigner there legitimately meets
either the reservation refusal or ordinary convergence on the owner's intent,
depending on where it lands, and asserting only the former was a real flake the
first run caught.

An undecodable row at the intent key fails closed under the same mismatch class
as a row naming the wrong parent, because the alternative reading is "the
invocation is free", which is the state the reservation exists to deny.

Reading the publication outcome had to learn about reservations: it rejected
any non-publication kind at the intent key and runs on every reconcile, so
without that change admission would have broken every task it admitted.

A refute-first pass tried to break the enforcement and reported what it could
not. **Rejected by verification, so they need not be re-raised:** no path
reaches the contested key under the publication kind outside the single commit
helper (every `EnqueueOutbox` call site was enumerated); every crash point
between admission and dispatch converges, including a task admitted by a
pre-#308 build, whose replay finds a free key and later settles by ordinary
insert; the promotion guard genuinely matches (`payload = ?` binds `[]byte`
against the `BLOB` column) and cannot land on a moved row under the store's
single connection; preserving `id` and `created_at` breaks nothing, since
`ListPendingOutbox` orders by `id` alone and `finalizePublicationEntry`
re-reads by key; migration 0012's quarantine predicate keys on the publication
kind and an authorization field a reservation has neither of; `classifyInvocation`
returns "free" on exactly one condition, `store.ErrNotFound`, so no decode
failure reads as an absent reservation; and neither moved test assertion
weakened coverage (a coverage profile confirmed the owner-row refusal the first
one no longer reaches is still exercised elsewhere, and the second changed only
setup). No production consumer branches on the one changed error class.

**Confirmed and fixed:** the settlement path re-validated the reservation but
never checked that the intent replacing it named the invocation whose key it
took, so a mismatched payload would have committed a row the drain rejects on
every pass and can never finish — the exact wedge this unit exists to prevent.
The read side already refused such a row; the write side now does too.
Separately, the terminal-recovery path built its candidate without the run ID,
which would have made a task's own reservation read as foreign; unreachable
today and fail-closed, but silent, so it is set.

The automated reviewer then found the upgrade gap the refute pass missed: a
task admitted before this change reaches reconciliation without ever passing
through admission, so nothing installed its reservation and its key stayed
open. Reconciliation now claims the invocation first (see the migration
decision above), and the regression proves it by dropping the reservation to
recreate an older build's durable state, failing the reconcile after the claim,
and observing the backfilled row mid-flight. A follow-up round pointed out that
the claim still trailed a separate read transaction, leaving a gap a writer
could use; validation and the claim now share one write transaction. The same
round's ownership-proof half was declined for the reason recorded above: a
legacy task has no durable evidence to prove an intent is its own.

The reviewer also read the daemon enum convention as binding on the classifier's
package-private `invocationState`, which had an `AllX` registration point but no
`valid()`; it was omitted because nothing decodes that type, so the predicate
would have been dead code the linter rejects. Rather than argue the exemption,
the gates' trailing fallbacks now use the predicate to separate the two ways
they can be reached: a registered state a gate forgot to handle, which is a bug
in that gate, and the invalid zero value, which was never classified at all.

## Revisit When

A writer outside `publish` needs to name the publication intent kind. The
enforcement here is structural for anything reaching the key through
`EnqueueOutbox`; unexporting `IntentKindPublication` behind
`PublicationIntentKey`/`PendingPublicationIntents` would additionally make the
key unnameable outside the package, at the cost of touching the five engine
call sites.

The reservation payload version changes. `DecodeReservation` fails closed on an
unknown version, and a reservation is the row a task must settle, so a bump
would leave every in-flight task unable to publish *and* unable to be
re-admitted. Migration 0012's shape does not apply: its predicate cannot see
reservation rows. A version change needs its own migration decided with it.
