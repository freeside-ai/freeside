# Record the Admitted Reviewer on Every Review

Work unit #898, plan §5.4 (Admission). Each routed and shadow review
invocation now gets one review admission snapshot, written before the review
source is asked and carried by the request, the result, and the record in
place of the provider, model configuration, and cost owner strings. The
issue body carries the contract. This note records the three owner decisions,
the planning defaults the owner let stand, the choices implementation made
where the issue was silent, what an upgrade costs, and the refute-first
record.

## Owner Decisions (2026-10-11)

**#1965 lands first.** The snapshot's model and effort come from the admitted
agent, so the agents had to name both first. Rejected: an interim rule for
values that still come from flags.

**Launch coverage (step 3) is its own unit.** This unit records the review
launch digest and leaves coverage unenforced for the review roles, as it was.
Enforcing it needs new adapter builds, so every adopted tree would have to be
adopted again. Follow-up: #1980.

**The provider label is the offer's lineage group.** The snapshot records
`lineage_group`, and the daemon fills the API's `provider` field from it.
Rejected: a new API field for the agent. The API schema and the app do not
change.

## What This Revises in the Cutover Note

`devlog/2026-10-01-1439-agent-selection-cutover.md` is frozen. Four of its
statements named this unit, and the owner decisions and the issue's non-goals
change each one:

- **"Review launch coverage waits for #898."** It does not arrive with the
  review record. It waits for #1980.
- **"A review has no attempt budget until #898."** This unit adds none. The
  offer and the credential are still held to the admission instant.
- **"Review records carry no identity until #898", with a revisit of the
  retirement ownership rule.** A record's snapshot now names an identity, and
  the rule stays as it is. A snapshot only ever names an identity the lineup
  selected, never a flag-era one, and a legacy record names none, so review
  records add nothing to who owns an open task from the flag era.
- **`devlog/2026-10-10-1514-task-line-admission.md` expected this unit to
  turn the reviewer-line refusal into selection.** It does not. A review
  source still runs one agent, fixed at composition. Follow-up: #1981.

## Planning Defaults the Owner Let Stand

The issue listed these as defaults the owner could veto before
implementation. None was vetoed, and the implementation keeps all of them.

**One snapshot per invocation, and the first one wins.** A restart, a retry,
or a lineup change during the invocation reads the stored snapshot back.
Rejected: admitting again on every pass, which would let one invocation be
attributed to two agents.

**The returned snapshot is checked, not trusted.** The engine compares the
snapshot on a result with the one it stored. A difference is a contradiction
failure and no record is written. The record is then built from the stored
snapshot, never from the returned one.

**A record is legacy or it carries a snapshot.** A stored record with the
three strings and no snapshot reads back exactly as stored. No code path
looks its agent up in the current lineup. A record with both or with neither
is refused at write and at read. Rejected: backfilling a snapshot onto old
rows, which would state an admission that never happened.

**A segment is one reviewer under one configuration.** Two consecutive
records share a convergence segment only when they share the configuration
digest and the agent digest. A legacy record and a snapshot record never
share one. Rejected: adding the agent to the review configuration digest,
which would invalidate every approved configuration for a change approval
does not cover.

**A request from before the upgrade is legacy.** A persisted request with no
snapshot is torn down and superseded by a new round under the existing
`ErrLegacyReviewRequest` handling. It is never accepted.

**The snapshot gets its own table.** `review_requests` is unique on run and
round and holds routed requests only, and the shadow arm has no engine-side
request row. One `review_admissions` table keyed by invocation serves both
arms.

## Choices Implementation Made

**An unrecorded lineage is legal and shows as `unknown`.** Plan §5.4 records
an unknown lineage as unknown, so the snapshot accepts an empty
`lineage_group`. Rejected: refusing admission for an offer with no lineage,
which would stop reviews over a label.

**The engine holds a decoded record's snapshot to the stored row.** Before
the latest routed record counts as current, the engine compares its snapshot
with the `review_admissions` row for that invocation and fails closed when
the row is missing or differs. Rejected: a store-level cross-check on every
read. Label readers show a record's own snapshot, and only the engine grants
authority from one.

**A legacy latest record is never current.** A task in flight across the
upgrade gets one more review round under a snapshot. Its request is a legacy
request the source no longer verifies, so a clean legacy record cannot be
re-gated. Rejected: keeping a legacy findings record current so its
adjudication can finish, and superseding only a clean one. That splits the
legacy rule by outcome for the sake of one upgrade window.

**The published disposition lineage leaves out a legacy round the first
admitted round reviewed again.** A legacy round superseded on its own head
may have findings nobody dispositioned, and the lineage requires a final
disposition for every finding it keeps. A legacy round on an earlier head
was remediated before the admitted round ran, so it stays and its
dispositions stay required. Rejected: dropping every legacy round once an
admitted one exists, which would erase fixes the published history should
show. Rejected: dropping only rounds that lack a disposition, which would
turn the fail-closed check into a filter.

**The ward checks the returned snapshot only when it decodes a retained
result.** The live `Poll` path has no check, because the engine holds the
only authoritative copy. Rejected: a second comparison in the source, which
would read the request from the journal for a check the engine repeats.

**The record fields are `Legacy*` with `omitempty`, and `admission` is
omitted when absent.** This departs from the golden convention that an
optional pointer renders an explicit null. A legacy record has to encode to
its stored bytes, because the store compares each row with its re-encoding.

**The review derivation check omits two joins of the execution check.** It
omits launch capabilities (#1980) and egress against the route's authorities
(#1677). Each is a recorded gap, not a relaxation of the execution check.

**The selection gate is not consulted, and the store read does not re-gate
enrollment.** The gate holds new writer work, and a review of work already
written is not new writer work. The stored snapshot is a record of what was
admitted, so reading it back never re-runs admission.

**The snapshot is written in its own transaction, before the request.** A
crash between the two leaves an admission row with no request. The next pass
reads the row back and writes the request under it.

**The agent digest alone tells reviewers apart in a segment.** A launch
digest, an enrollment generation, or an auth identity can move while the
same reviewer keeps reviewing, and none of them opens a segment.

**The disposition lineage does not split on the reviewer.** A reviewer
change on one configuration keeps every round in the published history, each
with its own attribution. The lineage is not a segment walk: an admitted
record stays current whatever the lineup says later, so its findings are
always dispositioned.

**The source's own admission check compares what the source can vary.**
Before a launch, a review source holds the request's snapshot to its role,
identity, model, and native effort. Rejected: comparing the agent digest. A
lineup change during an invocation would then refuse the review, when the
first-snapshot rule says the invocation keeps the agent it was admitted
under. The launch a source makes is fixed by those four values and by code.

**The shadow arm does not hold a stored shadow record's snapshot to the
admission row.** Nothing reads that snapshot to grant anything: the path
re-gates the record's run, round, source, instructions, base, and head, and
the snapshot is a label. The routed arm differs because its snapshot feeds
the request-authority re-gate.

**The growth rule still compares a segment's first round with the round
before it.** Growth measures the candidate's size between consecutive
rounds, whoever reviewed them. The round counts as the first of the new
reviewer's streak, never as a continuation of the prior reviewer's.

## What an Upgrade Costs

- **A task in flight gets one more review round.** Its latest record is
  legacy, so it is not current. If that round had findings, they are
  reviewed again by the admitted reviewer; the legacy round's own findings
  are not adjudicated further and do not appear in the published history.
- **A round completed before the upgrade loses its retained reviewer
  evidence.** The result evidence version moved (Codex v4 to v5, Claude v1
  to v2), so a retained result written before this unit no longer decodes
  and its evidence availability reads `unknown`. Earlier version moves did
  the same. A failed round keeps its evidence.
- **The operator-local `observe` output embeds the whole snapshot** for each
  shadow review, including the auth identity id. That id is an identifier,
  not a credential.
- **The engine's shadow cost-owner setting is gone.** The snapshot names the
  identity that paid.

## Refute-First Record

Run on the legacy reconstruction boundary and the returned-snapshot check
(`docs/agent-workflow.md` §refute-first), and again on the whole diff before
the first push (§pre-push-review), each time by a reviewer with no view of
the authoring session.

- Confirmed and fixed: the engine trusted the snapshot on a decoded review
  record. A row rewritten to carry another well-formed snapshot would have
  counted as the current reviewer's pass. The engine now holds it to the
  stored admission row (above).
- Confirmed and fixed: two store tests passed a method value where they
  meant its result, so their assertions passed on a driver error instead of
  the schema trigger they name.
- Confirmed by a run and fixed: a legacy round with findings and no
  dispositions, superseded by a clean admitted round on the same head, held
  the publication on every pass, because the disposition lineage kept the
  legacy round. The lineage now leaves it out (above).
- Confirmed and allowed: a completed legacy round loses its retained
  reviewer evidence (above).
- Traced, not run, and left: on the recovery path, where a ready record or
  a finalized intent already exists, the latest clean record supports
  readiness without passing the review gate again, whether it is legacy or
  carries a snapshot the stored row was never compared with. That path
  predates this unit and re-checks the candidate, not the reviewer. The
  task passed the gate before the path can run.
- Traced and declined, each with its reason above: the source's admission
  check does not compare the agent digest, the shadow arm does not compare a
  stored shadow record's snapshot with its admission row, and the growth
  rule compares a segment's first round with the round before it.
- Not run end to end: a legacy round whose dispositions were written but
  whose remediation had not started at the upgrade. The lineage test covers
  its shape (a superseded legacy round with a partial disposition).
- Disproved by a check: a record with both attributions or with neither
  reads back; a snapshot for another invocation, run, or role reads back; a
  shadow snapshot on a routed row reads back; a non-UTC admission time or
  non-canonical egress reads back; a legacy row fails to re-encode to its
  stored bytes; either arm writes a record before it compares the returned
  snapshot; a missing admission row lets a second admission replace the
  first; published text prints a legacy cost owner or an auth identity.

Revisit the recovery-path item when a unit next touches readiness recovery.
Revisit the retained-evidence cost when a result evidence version next
moves: a reader for the prior version would keep evidence across an upgrade.
Revisit the segment identity when #1981 lets a task line choose the
reviewer, since a line then moves the agent digest inside one run.

Follow-up: #1980, #1981.
