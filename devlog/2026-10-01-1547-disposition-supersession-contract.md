# Review Drift Contract: Disposition Supersession Record

Work unit #1666, plan §7 "Review Drift". This note covers the third part of
the contract that #1048 split: the record that says a drift audit's reversal
undid a fix, its table and store accessor, and the reader that returns each
finding's effective latest disposition. It adds types and persistence only.
#1051 adds the routing that writes the record and changes the recurrence rule
to read through it. The second part's note is
`2026-10-01-1220-drift-audit-contract.md`.

## Owner Decision: The Store Supplies the Fix Binding, Not the Model

The owner decided (2026-10-01, on the Codex round-8 question in #1048, "Bind
reversals to their fixing changes") that the model does not say which change a
reversal undoes. A `fixed` disposition row already names its
`RemediationInvocationID`. The supersession record copies that invocation
from the row it supersedes, and the store compares the copy with the row on
write and on every read. A reversal entry's "what to undo" text stays advisory
prose; #1051's engine derives the files to touch.

Rejected: a model-supplied reference to the fixing change. It would be model
output that the store could only check against the same disposition row, so
it adds a field that can be wrong and no information.

## Chose a Plain Record Over a Digest-Addressed Artifact

`FindingDispositionSupersession` is shaped like `ReviewDispositionRecord`: no
content digest and no encoding version. Nothing refers to a supersession by
digest. `DriftAudit` needs a digest because this record and the card facts
name it; nothing names this record that way.

Revisit when a consumer needs to bind to a specific supersession, for example
an approval that must name the reversal it approved.

## Chose the Superseded Disposition's Key as the Table Key

The table's primary key is (finding, superseded round), so a disposition has
at most one record. A second, different record for the same disposition is an
immutable conflict and an identical replay converges.

Rejected: keying by (run, reversing round, finding). It would let one fixed
disposition be reversed twice by two rounds, and the reader would then need a
rule for which record applies.

## Chose the Audited Round as the Reversing Round

The reversing round is the review round whose remediation performs the
reversal, and the store requires it to be the named audit's round. Plan §7
runs the simplification round as that round's remediation, and a human
`continue_under_policy` is a decision on that round's item, so both
authorities point at the same round. Without the equality an audit of one
round could justify a reversal recorded against another.

## Chose a Data-Free Automatic Authority, Refused Until Its Gate Exists

The authority is a closed kind, `auto_route` or `human_command`. The human
kind carries the item, the item version the command was issued against, and
the command identity. The automatic kind carries nothing: plan §7 calls it
"the re-derived auto-route gate record", and #1051's gate re-derives it from
the audit, the run's policy, and the stored dispositions.

Until that gate exists nothing can re-prove an automatic authority, so the
store refuses it on write and on read with `ErrSupersessionAutoRouteUnproven`.
The kind is in the enum now so the golden and the wire shape don't change when
#1051 lands.

Revisit when #1051's gate turns out to need a stored reference, for example
the second adjudication plan §7 requires before reversing a critical or high
fix. That would be another contract change.

## Chose Two Checks So the Scope Can Be Read Without the Authority

The accessor's checks are two functions. The scope check proves what was
reversed: the disposition is this run's, is `fixed`, and names the copied
remediation; the audit is this run's, is `over_hardened`, lists the finding,
and judged the reversing round. The authority check proves who ordered it.

Every read in this unit runs both. They are separate because #1051 will make
the convergence rebuild read these records, and a record's human authority
names the item for its own reversing round. Loading that item rebuilds its
convergence state, which would read the record again. The disposition load
already avoids the same loop by authenticating a row's scope before it follows
command authority (`loadFindingDispositionsAtDecision`). #1051 needs the same
order here. Nothing in this unit calls that path, so no test here shows the
loop.

## Chose to Refuse a New Record Once the Run Has Moved On

A new record is refused once the run has a recorded or failed round after the
reversing round. An identical replay still succeeds. Reads do not repeat the
check, because later rounds are expected by the time a record is read.

#1051 will re-derive each stored item's stop cause through these records. A
record that appeared late for an earlier round would change a cause an item
already stored, and that item would stop loading. `PutDriftAudit` has the same
rule for the same reason.

## A Missing Parent Is a Mismatch, Never Not-Found

A caller takes `ErrNotFound` from a supersession read to mean the fix was
never reversed. The nested loads (the disposition, the audit, the item) return
`ErrNotFound` when their row is gone, and passing that up would make a record
with a missing parent read as no record. The accessor converts a nested
not-found to `ErrParentKeyMismatch` and keeps the cause as text.

## The Reader Applies a Record Only to a Finding's Latest Row

`EffectiveFindingDispositions` takes a round. For each finding it returns the
latest stored disposition at or before that round, and reads it as `declined`
when a record targets that row and its reversing round is at or before the
same round. A reversed fix therefore reads `fixed` before its reversing round
and `declined` from it on.

A finding can be listed by several rounds and so have several rows. A record
that targets an older row changes nothing once a later row exists: the later
disposition stands. `EvaluateReviewConvergence` is unchanged in this unit.

The store does not check that the superseded row is the finding's effective
latest disposition when the record is written. Plan §7 gives that check to the
route gate (#1051), which also refuses a finding from the round's own batch.

## Known Gap: A Human Authority Is Not Yet Tied to a Drift Stop

The store accepts any authenticated `continue_under_policy` decision on the
reversing round's item. It cannot require that the item parked on cause
`drift_audit`, because #1667 adds that cause. Nothing writes a supersession
record before #1051, so nothing can use the gap in between. #1051 or #1667
must add the cause check.

Follow-up: #1051 (handoff comment).

## Accepted: A Legacy Policy Migration Makes the Record Unreadable

`MigrateLegacyTrustProfileRunPolicy` changes a run's policy digest, which
makes that run's audits fail their binding, and a record that names one fails
with it. The drift-audit note accepts this for audits because legacy runs have
no audit; the same reasoning covers this record.

## Implemented the Whole Unit Without the Planned Split

The plan estimated about 1,600 changed lines against a budget of about 1,000
and proposed moving the reader (about 300 lines) into #1051. It recommended
against the split: the only clean seam leaves the rest over budget, and the
reader reuses the accessor's test fixtures. The owner's fiat named #1666 as
one unit and no split was applied on the forge, so the agent implemented the
whole issue contract. The reader is its own commit, so the pull request can
still be cut there.

## Refute-First Findings

An independent pass tried to break the accessor in a scratch copy, and ran 33
single-condition mutants against the accessor's tests.

Confirmed and fixed (test gaps; the accessor was correct):

- **The item's run and round were only tested together.** One case changed
  both, so dropping either comparison survived. The round comparison is the
  only check that stops a record for round N from citing the same run's
  earlier `continue_under_policy` decision. Each now has its own case.
- **A replay over a tampered row was refused by the wrong rule.** Every tamper
  case recorded a later round first, so the written-once rule refused the
  replay and the existing-row reconstruction was never the reason. Tamper
  cases now record a later round only where the tamper needs one.

Disproved by a check:

- **A missing parent reading as not-found.** Raw deletes of the disposition,
  the finding, the item, either review record, the run, the adjudication, and
  the audit all left get, list, replay, and a different record refused, and
  none wrapped `ErrNotFound`.
- **A different record slipping in when the existing row fails to load.** It
  never reaches the insert.
- **Write and read diverging.** The marshal round trip is the identity for
  every validated field: identifiers are checked for valid UTF-8, the time is
  UTC, and digests are lowercase hex.

Allowed by decision:

- **A raw rewrite of the copied disposition key hides the row from the keyed
  read.** The whole-table list, which the reader uses, still fails closed. A
  writer with raw access can delete the row outright. The drift-audit accessor
  behaves the same.
- **Three checks no reachable state can isolate.** The disposition-is-`fixed`,
  audit-verdict, and audit-run comparisons each follow from another check in
  the same function: only a fixed disposition carries a remediation
  invocation, only an `over_hardened` audit carries reversals, and an audit's
  reversals name its own run's findings. They stay because they state the
  contract and cost nothing; no test can fail on them alone.
- **The copied superseded-round column has no tamper case of its own.** With
  the fixture's reversing round of 2, the table's check constraint leaves no
  other legal value. The neighbouring column comparisons are tested.
- **`CreatedAt` is not compared with the audit's or the command's time.** The
  contract doesn't ask for it and nothing reads the order.

Not pursued: the shared `decode` rewrites legacy vocabulary tokens after the
digest check, so an identifier that is exactly such a token would read back
different. It predates this unit, applies to every record the store decodes,
and fails closed.
