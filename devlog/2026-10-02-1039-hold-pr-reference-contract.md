# Publication Hold Pull Request Contract: Reference, Action, and Binding

Work unit #1643, the contract half split out of #531 at planning. It lets a
`publish_blocked` item carry its run's pull request, offer `open_pr`, and have
a durable pull request binding. #531 fills these shapes in from the engine and
reads the binding from the reconciler. No producer changes here.

## Sibling Binding Table Over Widening `ready_item_pr_bindings`

Chose a new `held_item_pr_bindings` table and `domain.HeldItemPRBinding` over
adding held rows to `ready_item_pr_bindings`. The owner decided this at
planning (2026-10-02), for two reasons found in the code:

- **A ready row means "this ready item published".**
  `publishedReadyAtOrBefore` tests for the row, and the callers of
  `GetReadyItemPRBinding` assume a ready item. A held row there would change
  what each of those reads proves.
- **`publication_invocation_id` is unique in the ready table.** A hold and the
  ready item that later replaces it belong to the same publication, so they
  could not both have a row.

`HeldItemPRBinding` has the ready binding's fields and validation. The store
runs the same publication proof for both (admission, export, intent, outcome,
successor target, or the re-entry authority), through one shared function that
takes the expected item type.

Final names: `domain.HeldItemPRBinding`, table `held_item_pr_bindings`
(migration 0086), `RecordHeldItemPRBinding`, `GetHeldItemPRBinding`.

## The Held Proof Does Not Compare the Successor's Item ID

Chose to tie a hold to its publication by run, head, and anchored pull request,
and not by item ID. The ready proof requires the publication successor's
`ReadyItemID()` to equal the item. The held equivalent,
`successor.BlockedItemID()`, is wrong for a rerun: a hold raised after a trust
re-evaluation has the ID `ReevaluatedBlockedItemID(run, command)` while it
shares the publication invocation of the hold it replaces. For the same reason
`publication_invocation_id` is not unique in the held table.

What this lets through: any `publish_blocked` item of the run, at the bound
head, anchored to the bound pull request, may hold a binding. The refute-first
pass judged the harm not real for the binding's use. Repository, repository
ID, pull request number, base, head, and run are still fully proven, and the
reconciler reads only those.

Revisit when a reader starts attributing a publication cycle from a held
binding's invocation IDs. That reader needs an item-to-cycle proof first.

## A Set Reference Cannot Be Dropped, and the Store Does Not Restore It

The transition rule allows one change to a hold's reference: none to set, on an
open item that stays open. A set reference is immutable, including removal.
The store anchors the reference on the first version that carries it and does
not copy it forward onto a later version that omits it.

Rejected: restoring the reference in `PutAttentionItem` when a producer
restates the hold without it. Silent restoration would hide a producer bug and
make the store a second source for a field the producer owns. The cost is on
#531: its hold producer must carry an existing reference forward, or its write
is refused with `ErrImmutableTransition`.

`PRHeadSHA` is not immutable across versions. A bound hold restated at another
head fails its snapshot read (the binding's head no longer matches). That
fails closed, and #531 must not restate a bound hold at a new head.

## The `open_pr` Rule Lives in Signet, Not Domain

Chose to enforce "`open_pr` on a hold only with a reference" in signet's
`validateRequestedActions`, beside the per-type action table, over a domain
`Validate` rule. Domain validates the item's shape, and which actions a type
may offer is signet policy everywhere else. The app's convergence matrix
cannot seed a reference, so it skips that one cell and names the daemon test
that covers it (`TestOpenPRNeedsAPRReference`).

## A Corrupt Binding Never Reads as Absent

`GetHeldItemPRBinding` returns `ErrNotFound` only when the item has no binding
row. A row whose proof records are missing reads as an inconsistent row. The
first draft converted this only in the item gate. The refute-first pass
pointed out that the next caller, #531's reconciler, would test `ErrNotFound`
and stop observing a pull request whose binding is corrupt, so the conversion
moved into the accessor.

The ready path still has the older behavior: `gateItemPRReference` treats any
`ErrNotFound` from `GetReadyItemPRBinding` as "no binding". Left unchanged
here because it predates this unit and changing it alters the ready gate.
Follow-up: #1695.

## Refute-First Findings

A fresh-context reviewer attacked the domain, signet, and store diff.

- **Confirmed and fixed:** the `ErrNotFound` conflation above, and held-binding
  test assertions that accepted any error.
- **Confirmed and left to #531 by plan:** signet and the engine recognize a
  hold by its exact action list (`publicationHoldItemAuthenticated`, the
  rerun submit check, and the sync matches). A hold offering `open_pr` fails
  them. Nothing emits such a hold until #531, whose plan already owns updating
  those matches; the sites are listed on that issue.
- **Disproved:** a change to the ready path (the diff only adds guards that are
  true for ready items), an ungated read path, a put path that attaches or
  swaps a reference outside the one allowed transition, and recursion between
  the held gate and the snapshot read.
- **Not covered by a store test:** a held binding on a publication with a
  successor that is not an in-place re-entry. The ready binding's store tests
  don't build that fixture either; the integration suite exercises it for
  ready items.

## Plan Revision Not Bumped

The `publish_blocked` row of plan §4 changes with this unit. The revision
number stays at 76: a bump also needs a §13 entry and
`docs/history/decisions.md`, which are outside this unit's declared scope.
This is the agent's call, flagged in the PR for the owner to overrule.
