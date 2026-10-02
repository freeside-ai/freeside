# Standing Indicator for Stopped Unattended Operation

Issue #980 adds `unattended_operation` to the sync bootstrap snapshot and a
persistent indicator on the Mac menu bar and beside the freshness banner on
both platforms. The issue fixed the direction at planning (project the one
admission gate, no client inference, no new endpoint). This note records the
choices implementation had to make inside that direction.

## Decisions

1. **The gate returns its verdict as data, and the predicate reads the same
   halves.** Chose two store reads (the operator stop, the blocking items)
   that both `RequireUnattendedOperationOpen` and
   `ReadTx.UnattendedOperationGate` compose, over a second query in signet
   that re-derives "stopped" from the transition log and the open items. Two
   derivations would let the indicator and the admission refusal disagree,
   which is the failure the indicator exists to prevent. The predicate still
   answers an operator stop before it reads the items, so a stop with an
   unreadable item list is recorded as a stop hold, as before; the
   projection needs both halves, so there the read error fails the
   bootstrap.
2. **An operator stop is reported once, bound to its own notice.** The
   resume notice a stop raises is itself a blocking `system_health` item, so
   a literal projection would report every operator stop twice. The
   projection binds the operator stop to the notice (diagnostic
   `unattended_operation_stopped` that offers `resume_unattended`) and drops
   that item from the blocking list. Chose matching on the diagnostic code
   plus the offered action over matching the item id format, because the id
   embeds the command id and is not a contract.
   A notice written before the diagnostic existed (2026-08-30) would be
   reported twice; none can still be open unless a stop has stood since
   then, and the cost would be wording, not a wrong admission state.
3. **Every stop is listed, operator stop first.** Chose a `stops` array over
   a single cause, because resuming an operator stop while a blocking
   finding is open leaves the gate closed, and a single-cause field would
   show the indicator changing its story after an action that looked like it
   should have cleared it.
4. **`since` is nullable.** The planning comment made it required. An
   operator stop always has its transition time, but a blocking item's
   `created_at` is optional in the domain type, and inventing a time for the
   wire would be a daemon-authored falsehood. `item_id` and `command_id` are
   nullable for the same reason: an operator stop whose notice is gone has
   no item, and a blocking finding has no command.
5. **The disk cache format moves from 5 to 6.** The planning comment left
   this open. It is required: a format-5 cache carries a cursor and no
   operating state, and an unchanged revision would let the client skip the
   bootstrap that supplies it, so a stop in force before the upgrade would
   stay invisible until something else moved the revision. The migration
   drops the cursor to force that bootstrap and keeps everything the earlier
   legacy path kept, plus pending task submissions and stops, which format 5
   introduced and which must survive.
6. **A cached stop never reads as current.** Chose to keep the indicator
   visible under a non-current snapshot with wording that says so ("At the
   last successful refresh ... The current state is unknown."), over hiding
   it until the next refresh. Hiding a stop on a flaky connection reads as
   "running", which is the more harmful error; the freshness banner above it
   already says why the state is uncertain. "Current" uses the freshness
   banner's own rule (fresh and within the staleness threshold), so the two
   rows cannot disagree.
7. **The menu bar dot yields to daemon lifecycle.** The status item has one
   badge. A daemon-lifecycle dot (stopped, unavailable, mismatch) keeps it,
   because a stopped daemon makes the unattended state moot; the wax stopped
   dot shows only when the daemon is otherwise healthy. The panel card and
   the VoiceOver description carry the stop in every case.
8. **The mock's resume notice carries the diagnostic.** MockServer derives
   the same projection from its own items, which needed its stop notice to
   carry `unattended_operation_stopped` as the daemon's does. That is parity
   the mock was missing, not a new mock-only shape.

## Known Gap

A blocking item that carries a `BlockingSupersession` (the backup-encryption
waiver notice) blocks or not according to live backup health, which is read
from the checkpoint files and moves no sync revision. With that notice open,
a backup-health change flips the gate and a fresh client keeps showing the
earlier verdict until some other write moves the revision. An independent
pre-push review found this. Closing it needs a contract choice (the verdict
on the revision heartbeat, or a revision rule for gate changes), which is
outside this unit's plan. Follow-up: #1707.

## Rejected

- **Projecting only the operator transition log.** It would miss the
  daemon's own durable stop and every blocking health finding, which close
  the same gate.
- **A dedicated read endpoint or a field on the health endpoint.** The issue
  excludes a new endpoint, and the health endpoint is unauthenticated and
  reports no operational state.
- **A sync event for the transition.** Every transition already moves the
  revision through the item writes that accompany it, so a client that sees
  the revision move bootstraps and adopts the new state. The daemon signet
  test asserts the revision moves on stop and on resume.

## Revisit When

- A stop source appears that writes no attention item or transition (the
  revision would not move, and the "no sync event" choice stops holding).
- The health endpoint's no-operational-state rule changes, or a lock-screen
  surface needs the state without a paired session.
- The status item gains room for more than one badge.
