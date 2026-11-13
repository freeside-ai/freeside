# A Run's Outcome List Is Read From Existing Records

Issue #3149. A `kind:contract` unit (it adds a required field to
`RunTimeline`), so a mandatory note. Tier B: the list only feeds display
and `inspect task`; it grants nothing and posts nothing.

Chose to assemble the list on read from the records that already exist
(proposal instances, the filing and comment intent ledgers, the
dispatchers' terminal notices, and the effective and external finding
dispositions) over a new outcome table the dispatchers write, because
every fact the list shows is already stored once. A second copy would
need its own migration, its own writes in two Tier A dispatchers, and a
rule for when it disagrees with the ledger it copies.

## Decision

- **The list rides the run timeline, not the run summary.** The summary
  syncs for every run on every revision; the list is wanted only when a
  card or task page opens one run, which is when the timeline is read.
- **The field is optional in the schema, and the daemon always serves
  it.** A required field would make an app built from this contract fail
  to decode every run timeline from an older daemon at the same
  compatibility level, and the compatibility rules keep such a pair
  syncing (`api/README.md`, Compatibility Level). `/health`'s `features`
  is optional for the same reason. An absent list means an older daemon,
  not an empty one; the mock refuses a timeline without it.
- **"Capped" comes from the dispatcher's notice.** A cap is stored as a
  `precondition_failed` refusal, the same as other refused preconditions,
  so only the notice's code tells them apart. The read takes the notice
  as the source rather than adding a capped reason to the ledger, and
  publish tests drive both dispatchers to every terminal notice, so a
  renamed code fails a test instead of turning every cap into a plain
  refusal or dropping a notice from the list.
- **A cap with no notice reads refused.** `proposalNotices.put` writes
  nothing once the proposal's workflow is closed, so such a cap has no
  notice. Showing it as `refused` with `precondition_failed` is accurate
  to what was recorded; inferring a cap by counting posts against the
  limit was rejected because the limit is policy, and policy can change
  after the post was refused.
- **Anything not final reads pending, until the workflow closes.** An
  approved proposal whose intent is not yet open, and an open intent with
  no terminal, both read `pending`. A comment approved by policy (#3150)
  has no card and no decision, so the read checks for its policy approval
  row; presence is enough here, because the dispatcher re-gates it before
  posting. A card closed with no decision reads
  `withdrawn`; a declined card reads `card_declined`. Once the run's
  workflow closes, the dispatchers open no new intent, and closing it does
  not close open cards, so a proposal with no intent then reads
  `withdrawn`, approved or not; otherwise it would read `pending` forever.
- **One broken entry does not fail the timeline.** An instance the read
  cannot reconstruct stays in the list as `unreadable`, carrying only its
  kind, and the list says `complete: false`; the daemon logs why. Any
  read error counts, not only rows that fail their checks: a transient
  store error heals on the next read, and listing only some error classes
  would fail the timeline on one the list missed. Failing
  the read would hide the run's milestones, hold, and review for one bad
  row. The one exception is a cancelled context, which fails the request
  as any other read does.
- **A reversed fix is listed as declined in the reversing round.** The
  effective disposition already treats a fix a later round found reversed
  as declined; the list says so in daemon-authored words ("Review round 3
  found the round 1 fix reversed.") rather than the original fix's
  reason, which would read as a contradiction.
- **External findings show their latest round only.** An external finding
  can be dispositioned in several rounds; only its last word says whether
  it is still set aside.
- **Agent text is bounded.** Finding messages and reasons are the first
  free agent-authored text on `RunTimeline`; each is cut at 4 KiB on a
  rune boundary, matching the schema's `maxLength`.
- **The project is read only when the run has posts.** The repository's
  name comes from the run's current project. Reading it for every run
  would mark the list incomplete for a run whose project is missing even
  though it has nothing to name.

Revisit when a third posting effect joins filings and comments, or when
a cap gains its own refusal reason in the ledger; either removes the
reason to read the notice.
