# Elapsed Publication Deadlines on the Ready Card

Issue #2752 makes the two publication deadlines the daemon already arms
(`pr_checks_deadline`, 30 minutes; `review_wait_threshold`, 24 hours) reach
the `ready_for_final_review` item they watch. Before it, a still-open item's
deadline ended its schedule `deadline_elapsed` and changed nothing an
operator saw outside the run timeline. The unit is Tier A by path: one
normalization line in `daemon/internal/engine/publication_core.go`.

## Decisions

- **The elapse is a fact on the item, not a new item or a re-sort.** The
  scheduler's deadline handler (`deadlineRegistration` in
  `daemon/cmd/freesided/scheduler.go`) appends an `ElapsedDeadline` to the
  item it already re-reads inside the consuming transaction, advances
  `item_version`, and puts it through the store, so the sync projection
  carries it. Chose this over an attention type or a notification because
  the deadline carries no new decision: Freeside reads no GitHub check or
  review state (plan §5.11, §5.16), so the record can only say the wait ran
  out. Whether to observe checks is an owner decision and its own Tier A
  unit in `internal/publish/`.
- **`armed_at` comes from the schedule's nominal `FireAt`, not its
  `CreatedAt`.** `armed_at` is `FireAt` minus the kind's fixed duration,
  read from `engine.PublicationWatchDeadline`. A re-armed generation (the
  item's version moved, often because the other deadline just fired) keeps
  `FireAt`, so both generations record the same deadline;
  `TestDeadlineRecordsBothElapsesInFiringOrder` pins it. The duration is the
  engine constant, not a schedule field, so a configurable deadline would
  have to carry its length on the schedule first.
- **The list is append-only and validated, nil when empty.**
  `ValidateAttentionItemTransition` keeps the stored entries as an ordered
  prefix; `Validate` admits the list only on `ready_for_final_review`, each
  publication kind once, with UTC instants and an elapse no earlier than the
  deadline. The elapse-time rule holds by construction: the store lists a
  one-shot schedule due on `fire_at <= now` in Unix nanoseconds, and the
  event's `FiredAt` is that pass's `now`.
- **Recovery treats a stored elapse as compatible (the Tier A line).**
  `compatibleTerminalItem` compares a stored ready item with one rebuilt on
  the crash-seam recovery passes, which never carries the list. A deadline
  that fired while a publication task sat between arming and recording its
  terminal result would otherwise make the stored item "disagree" and hold
  the task. The compare copies `ElapsedDeadlines` from the expected item,
  as it does `ReadinessInvalidation`; the transition check after it still
  requires the stored list to be a legal append. Every caller only compares
  and never writes the rebuilt item over the stored one, so the list is not
  dropped there. Rejected: leaving the seam open to keep the unit Tier B.

- **The fake-publication terminal digest shadows the list.** A bound
  terminal item stores `fakepublication.TerminalDigest` in its reason, and
  `ValidateTerminalBinding` recomputes it from the stored item. The list
  is lifecycle state appended after binding, like `status` and
  `conclusion`, so the digest omits its key. Including it would change
  every bound item's digest (a `null` key) and fail validation once a
  deadline elapsed on a bound item.

- **A deadline the item cannot take still concludes; it never fails the
  handler.** Before #2752 the consumption wrote nothing to the item. Now it
  puts the item, so the put's gates run, and a handler error stops the
  scheduler's loop (doctor and janitor too) and repeats on every
  redelivery. Two reachable states were guarded in the handler:
  - An open item in a closed task workflow episode, where abandonment
    closed the episode and retirement has not landed, refuses every put
    with `ErrTaskWorkflowClosed`. The schedule concludes
    `deadline_elapsed` without the item write, as before.
  - A redelivery after the wall clock stepped back can fire before the
    nominal deadline. It abandons with `ErrStaleConsumption` and
    redelivers once the clock passes the deadline. It doesn't stamp the
    deadline as the elapse instant, because that would record an instant
    nobody observed.

## Refute-First Record

A fresh-context reviewer tried to refute the diff before commit.

- **Confirmed and fixed:** the two handler stops above.
- **Allowed by decision:** each elapse advances `item_version`, so a client
  decision rendered before the fire gets a version conflict and re-renders.
  The issue contract requires the bump, so the sync projection delivers the
  fact.
- **Allowed by decision:** the mock's `Date` arithmetic could misjudge an
  elapse exactly at a sub-second deadline. That case is mock-only, and the
  fixtures use whole seconds.
- **Disproved:**
  - Recovery never writes the rebuilt, list-less item over the stored one.
  - The normalization doesn't admit a foreign item, because every other
    field is still compared.
  - Every writer of an open ready item reads it, modifies it, and writes
    it back, so none trips the append-only rule.
  - A kind can't repeat, because the schedule conclusion and the item put
    commit together.
  - A re-armed generation records the same `armed_at`.
  - A concluded item gets no append.
  - nil and `[]` can't diverge.
  - The other put gates (decision surface, recommendation, PR anchoring)
    re-derive from the stored item that every other read-modify-write
    writer re-puts the same way, and no path makes them fail here.

## Rejected Options

- **Restoring a dropped list in the store's read-modify-write replay
  (`entities.go`).** Every writer of an open ready item reads the stored row
  first, and the transition rule makes a drop fail loudly, so a restore
  would defend a path no writer takes and widen the Tier A surface.

## Revisit When

- A writer re-puts an open ready item from a constructor-built copy, which
  would now fail with `ErrImmutableTransition` or `ErrStaleWrite`.
- The deadline lengths become configurable, or Freeside starts reading
  check runs or review state.
