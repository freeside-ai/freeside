# Review Slot Lifetime

A pool's limit counted executions only, so a reviewer drew on the same quota
without being counted, and a slot ended on a record written before the launch's
containers stopped. This is the second half of #1596, recorded in #2441;
`2026-10-30-1339-usage-pool-limits.md` holds the first.

## A Slot Lasts Until the Launch Is Proved Gone

Every ward role that draws on a pool now holds a slot, reviews included, until
every container is proved absent. Chose that proof over three earlier release
points:

- **The outcome row.** A cancelled review records its outcome before its
  containers stop (`CodexReviewSource.CancelReview`), so the pool would
  undercount while they run.
- **The terminal record.** An execution's terminal record has the same gap,
  so an execution now also waits for its handoff journal to close.
- **Any closed intent.** A launch that fails before it starts closes its intent
  with no outcome, and the engine retries the same invocation. `Inspect`
  relaunches that recorded request, and because the admission is stored, nothing
  asks the pool. Another launch could take the freed slot during the retry
  delay and put the pool one over its limit. The saved branch had this bug;
  planning found it.

So a review slot ends on the ready marker, or on a closed intent behind an
outcome. A closed intent with no outcome falls through to the checks for an
unlaunched admission: its record, its failure, its release, or a run that can
no longer launch.

## The Proof Is Authenticated Against Its Key

A journal row decides a release only after its digest and the key and state in
its body match. The digest alone covers state and body but not the row's key, so
a finished review's row copied whole onto a live one passed it; the fresh-context
review reproduced that, count 1 to 0 with no error. A rewritten row now reads as
an error, as the ward's own reader already treated it.

The count runs on every gate, in the write transaction, and it reads every
review admission. Chose to read a closed intent first and ask only whether an
outcome exists, rather than hash every outcome body, which can run to about
9 MiB. Measured with synthetic bodies, hashing them all took 366 ms at 1,000
reviews of 200 KB, inside a write transaction on a 100 ms loop; real body sizes
are unmeasured. Behind an authenticated closed intent no outcome state changes
the answer, so a rewritten outcome there is unread rather than an error, and it
releases nothing that wasn't already released. Rejected a filter on review
records, which would have let an unauthenticated row end a live launch.

## Why `review_slot_releases` Exists

A shadow review abandoned before it launches leaves no ward record and no
review record, so nothing else could end its slot. Abandoning it writes a row
here. Migration 0114 backfills the shadow admissions already abandoned that way.
A release does not outrank the ward: a launched review keeps its slot until the
ward's proof.

A release also ends the observation for good. The gate re-enters while a run is
parked, and a released admission that stayed eligible would launch past the pool
gate, because a stored admission asks nothing. So the shadow pass stops at a
release, and the release goes through `WriteInternal`: it isn't client-visible,
and a `Write` would move the revision on every pass.

## The Count Is Renamed and the Column Is Not

`ActiveUsagePoolExecutionCount` became `ActiveUsagePoolSlotCount`, because it
now counts reviews, and a change of meaning is a rename (AGENTS.md, Contract
Changes). The stored `max_parallel_executions` keeps its name: plan §5.4 item 2
already defines it as the inference limit for the reviewer and shadow reviewer
alike.

## Clearing a Hold Resets Its Pace

The same hold returning inside the 10-second refresh interval is a new wait, and
skipping it would leave the run showing no hold (#727). The dispatch path and
the review path reset separate paces. The review path resets only when a wait
ended, because that key also paces the attended clear, and a parked run's repeat
tick must move no revision. The other sites that clear a hold keep their pace;
a hold missed there returns within the refresh interval.

## Reviews Now Queue on Existing Codex Pools

Every migrated Codex pool sits at limit 1: enrollment wrote 1, and
`set-identity-limit` refused non-Claude identities. Reviews on one Codex identity
ran with no limit before this, and now run one at a time until the owner raises
the limit with `freesided set-pool-limit`. A shadow review at a busy pool is
abandoned once its 15-minute window ends, so a full pool never holds the routed
gate longer than a slow shadow review would. Raising a limit stayed out of this
unit.

## Known Gaps

Each of these holds a slot under a visible `usage_pool_exhausted` hold on the
waiting run, and none lets the pool go over its limit.

- **A stuck journal pins a slot.** `handoff_journal_records.run_id` is not the
  invocation id, so the proof keys on `lease_holder` and `read_hold_holder`. A
  journal record that never closes holds its slot until it does.
- **A shadow admission can outlive its pass.** One written before a later pass
  skips shadow stays held until the task is cancelled or closes. On a limit-1
  pool shared with the routed reviewer, the next round then waits on its own
  task until that happens or the limit rises.
- **A closed intent the engine never revisits stays counted.** A closed intent
  with no outcome on an open run ends only through the engine's next pass.
- **The backfill skips shadow admissions with a recorded request.** At upgrade
  they can't be told from a live review, so one abandoned after its request
  holds its slot until its task closes.
- **A contradicted review keeps its slot until a restart.** A started intent
  that hits a contradiction is never inspected again, and recovery runs only at
  startup, so the next round on a limit-1 pool waits. Holding the slot is right,
  since the containers may still run. Follow-up: #2472.
- **One bad row stops launches on every pool.** The count reads all of
  `review_admissions` and `codex_review_intents`, and the outcome of each review
  without a closed intent, and fails closed, so a single inconsistent row refuses
  every admission until it is repaired. Chose that over counting around a row
  whose meaning is unknown.

## Refuted

The fresh-context review traced these and found them safe, so they needn't be
asked again: a review record with an unclosed intent (`Poll` requires ready
first); a relaunch after an outcome (the outcome fence blocks it); a closed intent
or ready marker written before its containers are gone (closure follows removal
in every writer); a failed invocation relaunching (the round advances to a new
id, which passes the gate); and the migration releasing a live review (it covers
only shadow admissions with no request and no record, and one live at upgrade is
a one-time case).

Revisit when a review source lands that doesn't write the Codex review journal,
or when the shadow gap costs a real round.
