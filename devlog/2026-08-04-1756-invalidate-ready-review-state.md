# Invalidate ready review state on base/head change (#496)

Scope decisions and the returned-object-trust-boundary call for the
readiness-invalidation work. Work unit #496; re-entry split to #502.

## Chose: invalidate only in #496, split automatic re-entry to #502

Chose to deliver **invalidation** (detect a base/head change after a
`ready_for_final_review` item exists, supersede the item, stale prepared
commands) in #496, and split **automatic re-entry** (re-verify → review round
N+1 → fresh ready item) to #502, over building both here.

Why: a code-lifecycle read showed re-entry does not match the task machinery.
The production outbox is one row per `RunID` with `ON CONFLICT DO NOTHING`,
`MarkOutboxDispatched` is one-way with no `dispatched→pending` store method,
the review gate hard-errors on a changed candidate rather than advancing a
round (`production_publication.go` ~1695), and for a base advance the
**admitted base coordinate** (`binding.admission.Base.BaseSHA`) is fixed at
admission, so "re-verify for the new base" is re-admission on the contract
surface, likely leaning on the unbuilt Wave 5 verification algebra the issue's
audit anticipated. That is a task-lifecycle + contract unit, not part of a
bug-fix. The P1 harm (Freeside presenting a stale pass as ready and a human
consuming it) is closed by invalidation alone, matching the issue's own
framing that #427/#482 made **invalidation** the Wave 4 acceptance condition.

## Chose: record the invalidation as an item-visible fact (contract unit)

Chose to record a typed `ReadinessInvalidation` fact on the item, mirrored on
the api wire and in the app client, over superseding silently to stay a
`kind:fix`. #496 was reclassified `kind:contract`, scope `daemon/` + `api/` +
`app/`.

Why: the issue's harm statement names the missing signal ("a head change is
not even projected onto the item"), so recording why the item was superseded
is the legibility half of the invalidation, not gold-plating. And it is
unavoidably a wire change: the sync surface embeds `domain.AttentionItem`
whole (`signet/sync.go`), so any new field is client-visible and the api
schema must mirror it. This matches the `base_freshness` precedent (#442,
`kind:contract`), not #490 (engine-internal vocab that never touched the api
schema). Dropping the fact to preserve a `kind:fix` label was rejected on the
merits: it would make the fix hollow and batch nothing, whereas the correct
shape is a contract unit carrying its generated consumers.

Rejected alternative: overloading the existing `reason` prose string instead
of a typed fact. It would destroy the ready-reason, is not machine-renderable,
and breaks the parallel with `base_freshness` (staleness gets a typed fact).

## Chose: supersede + new item, never reopen

Chose to supersede the ready item (`Status→Superseded`, `ItemVersion++`) and
record the fact in the **same transaction** as the staleness observation, over
mutating or reopening it.

Why: attention-item `Type` is fixed and terminal statuses never reopen
(`transitions.go`), so "invalidate ready" means supersede and earn a new item
(the #502 half). Superseding in the observation's transaction is what stales
prepared commands: signet re-gates on status **and** version at both Submit
and PutCommand (`signet/service.go`, `store/entities.go`), so no command-side
mechanism was added. Command-staleness is thus established by composition:
invalidation produces `superseded` + bumped version (this unit's tests), and
that state rejects commands (existing `TestSubmitSupersededVersion`,
`TestSubmitClosedItemCurrentVersion`). Precedent: `supersedeBlockedHold`.

## Chose: a foreign repository identity does NOT invalidate (trust boundary)

Chose to invalidate only on a change to a pull that is **provably this PR**
(observed repository ID and number match the binding) but whose head or base
ref moved. A foreign observed repository ID does **not** invalidate; it remains
the requires-exact defense (no terminal action, item stays open). The
`identity_changed` reason was consequently dropped from the vocabulary.

Why (refute-first, returned-object-trust boundary): the observer looks up a
path (`owner/name`) and could return a resource whose numeric repository ID
differs (the path was reassigned/transferred). That cannot prove anything
about this item's actual PR, which may still exist elsewhere, so superseding
on it would destroy valid ready state on a path-resolution artifact and hand a
griefing lever to anything that can influence path→ID resolution. The existing
`TestActiveResourceReconcileRequiresExactReturnedResource/repository_identity`
already encodes that a foreign repository must not drive a terminal action;
extending "invalidate" to it would have regressed that invariant.

Refute-first findings:

- **Confirmed:** a blanket `!exact → supersede` regressed the requires-exact
  trust boundary (the existing test failed). Fixed by gating on matching
  repository ID + number.
- **Rejected by verification:** "identity_changed is needed because the issue
  lists PR identity." A PR's (repo, number) is immutable, so there is no
  observation that both trusts the resource and proves this PR's identity
  changed; the only trustworthy changes are head and base ref. The reason is
  reserved for a future consumer with a trustworthy signal (cf. issue #22).
- **Accepted by decision:** a base advance always supersedes (no "advanced but
  benign" case); retarget (base ref change on our PR) supersedes; head change
  on our PR supersedes.

## Watch supersedes and concludes, does not re-arm

The base-advance watch, on `Advanced` (observed base tip past the admitted
base), supersedes the item and concludes the publication schedules via the
shared `concludePublicationSchedules` helper, rather than re-arming. A material
change back to the admitted tip keeps the existing record-fact-and-re-arm
path. The scheduler's own subject-liveness check and the active-resource
reconciler's `settleSchedules` are the convergence backstops.

## Revisit when

- #502 builds automatic re-entry: it re-touches the admitted base coordinate
  and the production task lifecycle, and may reintroduce a trustworthy
  identity-change signal that warrants restoring the `identity_changed` reason.
- The §7 PR-anchored vs pre-publication review anchor is resolved: everything
  here operates in the PR-anchored shape and does not move the anchor.
