# Recipe Switch, Intake Recipe, and the Human-Gate Convergence Trigger (Issue #1419, Part E)

Part E of #1419, the last part: new client submissions freeze
`freeside.client-publication/v2`, new label-initiated runs freeze
`freeside.intake-publication/v1`, and the engine revisits a human-gate held pull
request once its closure decision lands. The Part A to D notes stand; this note
records the replay and trust-boundary decisions this part adds.

## New Records Only; Replay Never Recomposes

Chose to change only what a new command or a first admission composes, over
migrating or re-deriving stored records. Records freeze their recipe, so the
switch is safe exactly where nothing rebuilds the publication from current code:

- **Client commands.** The signet boundary replays a recorded command before it
  calls `SubmitTask`, so a command recorded under v1 returns its v1 record
  untouched. `SubmitTask` composes v2 only for a new command.
- **Label occurrences.** `startSpec` already decodes the admitted attempt's
  stored bytes. Two paths did rebuild from code and are now pinned to stored or
  literal bytes:
  - `admit` re-runs when a crash lands between the reserve transaction and
    `BindIntakeAdmission`. A pre-change daemon may have stored the literal
    record in that window, and `PutProductionAttempt` refuses any
    non-identical row. The re-run now converges on the stored attempt's
    publication bytes; every other attempt field must still match.
  - `startSpec`'s legacy-reservation branch (a reservation with no attempt,
    which predates attempts) rebuilds the recipe-free literal record, because
    that is what such a reservation froze.

## The Intake Recipe Is a Literal Record With an Author Pass

Chose to make `freeside.intake-publication/v1` carry the literal title and body
and validate through the literal checks (including the ban on `source_issue`),
over giving it an empty-text shape like the client recipes. The literal text is
its fallback (plan §5.15), so it must be stored and screened like a literal
record, and the fallback renders it without reading the agent's publication
claim. Every step that keyed on v2 (the author run, the closure step, the
source-reference section, and the prose `Source issue:` suppression) now keys
on one predicate, `authoredPublicationRecipe`, so the two recipes cannot drift
apart.

The author input takes the source issue reference from the task's bound
issue subject when there is one. An intake record bans `source_issue`, so
without this both author sites, including the closure propose site, ran with
no issue reference at all. The issue's title and body still reach neither
site, for either recipe: that needs a trusted forge issue read outside this
part's scope (Follow-up: #1493). Until then, under the default policy the
propose site's `resolves` answer is approved without the issue text.

## The Convergence Trigger Is a Separate Wait Row, Not a Held Task Row

Part D deferred this: under the human gate the publisher holds the PR draft
until the `effect_proposal` item is decided, but the decision lands in signet,
which never touches the forge. No existing pass revisits a published PR: the
task's outbox row is retired on publish, and the active-resource reconciler
only observes.

Chose an engine-private outbox row (`production_publication_closure_wait`)
beside the retired task row, over keeping the task row pending until the
decision. The first design kept the row pending. An audit of the row's readers
found that a pending row after publish refuses operator return-to-agent
feedback on the ready item, refuses a publication continuation, and keeps the
run reported as publishing and holding its WIP slot for as long as the card
waits. The wait row avoids all three: the task row retires exactly as before.

Each pass lists pending waits. While the ready item and the closure item are
both open, a wait costs two store reads and no forge call. Once the ready item
concludes (the pull request merged, closed, or was invalidated), the wait
retires without touching the task: there is no hold left to release. Once the
decision lands while the pull request is still live, the pass re-lists the
retired task row, which re-enters the existing published-recovery path. That
path reconverges the pull request from current store state (so the publisher's
own approval re-check still decides `Closes`), and `completePublishedTask`
retires the wait. Rejected re-running only the publisher's convergence call: it
needs the full candidate, which only the recovery path rebuilds. The wait row's
fields only choose when to re-enter; the re-entered task re-derives every
authority from its own row and the store.

A wait must never stop the publication lane, because any non-retryable pass
error ends the reconcile loop:

- **Unreadable wait row.** A row this daemon cannot decode, or one naming a
  missing or foreign task row, is skipped and left pending for a daemon that can
  read it. Skipping only leaves the pull request a draft, which withholds
  `Closes` and merging rather than granting them.
- **Failed re-entry.** A re-entered task whose repair fails non-retryably
  retires its wait before the error is joined. The pass still fails loud, once,
  instead of failing on every pass and every restart.
- **Wait and task row settle together.** The wait is armed or retired in the
  same transaction that retires the task row. With two writes, a crash between
  them left the task pending beside an armed wait; after a later decision the
  task re-entered as an ordinary pending row, outside the fail-once rule, so a
  PR merged meanwhile failed it on every pass. Dispatched outbox rows never
  return to pending, so the two rows cannot disagree that way again.

Scope limits, each accepted:

- **Only `production_publication_requested` rows arm a wait** (a run's first
  publication or a successor). A reevaluation or continuation intent
  authenticates against state that holds only while it is pending, so it cannot
  be re-entered once retired. Such a PR under the human gate is not revisited
  after its decision.
- **Latency.** The trigger is the reconcile cadence, not an event, so a decided
  PR flips on the next pass.
- **Early human merge.** A person can still mark a held PR ready and merge it
  (Part D's accepted limit). If the decision lands before the active-resource
  reconciler observes the merge, re-entry meets a closed PR whose body differs;
  the pass fails loud once and the wait retires.
- **Re-entry re-runs the recovery gates.** A wait can last days, and re-entry
  re-checks the recipe approval and reviewer configuration as they stand then.
  A revocation in that window takes the existing paced, visible hold instead of
  writing `Closes`. That matches the rule that the publisher never acts under
  withdrawn trust.

## Refute-First Findings

An independent reviewer tried to break the change before commit:

- **Confirmed and fixed:** a released wait whose PR a human already merged or
  closed failed every pass and every restart; a completed successor's wait was
  re-entered on every pass forever; and one unreadable wait row aborted the
  whole pending-list read. The concluded-retire, skip, and fail-once rules above
  answer all three, each pinned by an integration fixture.
- **Confirmed and fixed in review:** the crash window between arming the wait
  and retiring the task row (Codex, PR #1492); the single-transaction rule
  above answers it.
- **Confirmed and accepted:** the re-run recovery gates (above).
- **Plausible, deferred to issues:** a reevaluation publication under the human
  gate is held draft but never arms a wait (Follow-up: #1490); the intake
  literal title `Resolve owner/repo#N` is a GitHub closing keyword, so a squash
  merge of a declined (`Refs`) intake PR would still close the issue
  (Follow-up: #1491). The literal title predates this part and is frozen in
  existing records.
- **Disproved:** the wait row grants no authority (it only picks which task row
  to re-list, pinned to `production_publication_requested`). Intake replay
  keeps pre-change bytes: `startSpec` decodes stored attempts, the legacy branch
  rebuilds recipe-free bytes, run IDs don't depend on the publication, and
  nothing else compares a freshly built intake publication to stored state. No
  other code treats `Recipe == ""` as the only literal form.

Revisit when: the reevaluation or continuation path starts publishing
human-gate closable sources in practice, or signet gains an engine-facing
decision event that could replace the per-pass wait scan.
