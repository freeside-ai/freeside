# Compare a Consumed Answer's Item Record to Record

For #1556, operator-feedback reconciliation now checks an answered item
against its stored record, not against the gated, projected read. The gated
read still runs in the same transaction, so its gates still fail closed; only
its result is dropped from the comparison. The planner chose this shape and
the owner let it stand at `Handle #1556`.

Evidence: in the #1445 run-114 exit session, a composer sketch took two
clarification answers, and the specification that followed opened
`spec_approval` and renamed the task to its title. On the next pass,
reconciliation rejected both consumed answers with `ErrParentKeyMismatch`,
and the daemon entered a durable stop.

The cause was a tier mix. `reconcileOperatorFeedbackActions` loads each
answered item with `GetAttentionItemRecord`, which returns the display names
stored with the item. The write transactions in `enqueueSpecificationAnswer`,
`enqueueSpecificationRevisionCampaign`, and `persistImplementationFeedback`
re-read it with `GetAttentionItem`, which projects the task's current name
(and can re-gate `Recommendation`), then required `reflect.DeepEqual`. The
two tiers agree only until the task is renamed, and every rename source (a
specification title, the approved heading, the background namer) breaks
them apart for good. Because reconciliation rechecks every consumed answer on
every pass, one rename turned into a permanent stop.

`requireUnchangedFeedbackItem` now serves all three sites: gated read,
record read, record-to-record equality. The stored command's equality and
`operatorFeedbackCommandMatchesItem` are unchanged, and the latter now reads
the record, which the equality has just proved identical to the loaded
item. A search of the engine found no other site that compares a gated read
with a record-tier item; the remaining `DeepEqual` item checks compare a
gated read with an item rebuilt from current display names.

Rejected:

- **Dropping the gated read.** It would let a consumed answer proceed past
  evidence, recipe-approval, and other current-policy gates the record tier
  deliberately skips.
- **Ignoring `DisplayNames` in the comparison.** It fixes today's field
  only; the next projected or re-gated field (`Recommendation` already is
  one) would repeat the bug.
- **Short-circuiting on the existing next outbox entry before the check.** A
  larger change, and a rename between the answer and its first delivery
  still passes through the same check.

Refute-first findings (an independent reviewer tried to prove the change
wrong):

- **Record-to-record accepts a real change: disproved.** Both tiers decode
  the same row through `scanAttentionItemRecord`. The gated tier only adds
  fail-closed gates, `gateRecommendation` (which can only clear
  `Recommendation`), and `projectTaskName` (which only overwrites
  `DisplayNames.Task`). Any change to the stored row still fails the new
  equality. The one behavior change: a stored `Recommendation` that current
  policy no longer derives used to fail the pass and is now tolerated;
  nothing in the three transactions reads `Recommendation` or
  `DisplayNames`.
- **A gate stops running: disproved.** The gated read still runs at the
  same point in each transaction and its error still returns.
- **Matching the command against the record changes a decision:
  disproved.** `operatorFeedbackCommandMatchesItem` reads no projected
  field.
- **Another engine site mixes tiers: disproved** by a targeted search.
- **The tests miss the bug: disproved.** Every new or changed test fails
  on the unfixed code with the field error.
- **No rename test on the revision-campaign path: allowed.** An
  implementation-stage question exists only after the specification was
  approved, and approval names the task from the approved heading with the
  `specification` source, which `SetTaskName` never overwrites. A rename
  there needs a heading that fails to parse and a later agent-sourced name,
  so the path shares the fixed helper without a dedicated test.

Revisit when a gate inside `GetAttentionItem` can start failing for an
already-consumed answer (for example an evidence recipe unapproved after the
answer). The gated read would then fail on every pass and could still cause
a durable stop; that failure class predates #1556 and is out of its scope.
