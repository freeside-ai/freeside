# Coordination Protocol

The mechanics behind AGENTS.md's coordination gates: the work-unit issue
shape and wave-state terms, the claim-lease protocol, the session-start
queries, session end, deferral escalation, the relationship types, the
migration protocol, reading a rebase, and Freeside's additions to the tracker
format in `docs/tracker-format.md`. Read this file before claiming a unit,
filing a deferral, starting an issue-backed session, or creating or updating
a tracking issue.

AGENTS.md holds the binding gates and is the authority where the two
disagree; this file carries only the procedure that implements them. Section
names cited below that do not appear in this file (Branches, Commits, Work
units, Decision notes, the finish line, Monorepo scope discipline) refer to
AGENTS.md.

## Work-Unit Issues

AGENTS.md (Work Units) says which work needs an issue; this section holds
the issue's shape and the wave-state terms the gates use.

One issue per issue-backed work unit, created from the work-unit template:

- Source devlog entry (optional; cite the originating decision note's filename
  only when the issue genuinely started there).
- Objective.
- Non-goals (`none` allowed).
- Affected interfaces/contracts: the interface surfaces the unit touches, not
  the whole work contract; the issue as a whole is the contract.
- Acceptance: the fixture/test list is the spec.
- Scope / declared paths.
- Dependencies: `starts-after`, `merges-after`, and `stacked-on` (see
  Relationship Types), never untyped issue refs. Record an unknown or
  materially ambiguous relationship as `starts-after` until the planner
  resolves it.

Labels: `kind:*` for type, and `tracker` for an issue that tracks other
issues. Milestones carry the phase (1A, 1B). Each wave has a tracking issue
listing its units, maintained by the planner (AGENTS.md, Work Units).
Every tracker, wave or ad hoc, follows Tracking Issues below.

Wave state resolves per the §11 three-state resolver over open issues that
carry the `tracker` label and a milestone: exactly one is active-wave state,
none is inter-wave state, and more than one is an invalid authority state for
planner repair. The scheduling door exists only in active-wave state, because
it needs an open current tracker to list the unit. Fiat (`Plan #N`, `Handle
#N`) is independent of wave state and may proceed in either state after all
ordinary gates pass.

**Scheduled** means both a milestone and a listing on the current tracking
issue. The planner changes those fields as one planning operation; either
field alone is a planner-repair error and does not open the scheduling door
(fiat remains independent). The wave tracker carries the milestone without
being a unit, so it is neither scheduled nor a planner-repair error.

## Claiming

A claim records occupancy only; authorization comes from scheduling or
fiat (see Pickup), never from the claim itself. Issue-backed implementation
work is claimed with an issue-comment lease that hands off to a real PR. The
planning stage never claims its issue: its planning reservation under
Stages blocks implementation of that issue while planning is active, but does
not authorize planning or implementation. Direct no-issue work needs no claim:
it is not eligible for concurrent or multi-session execution, and gets promoted
to an issue before that changes (see Work units).

Claim arbitration covers the unit's own issue and nothing else (AGENTS.md,
Coordination Gates). Claims conflict only when they name the same unit, and a
planning reservation blocks a claim only on its own issue. After posting,
every contender rereads the issue and orders its non-expired, unreleased
claim comments by `created_at`, with the numeric comment ID as the
tie-breaker. The earliest wins. A competing claim either predates a
contender's reread and is seen there, or is posted later and sees the first
claim on its own reread.

To claim a unit:

1. Confirm the issue is authorized (scheduled or fiat-assigned), then fully
   page its comments and claiming PRs. If it has an active planning
   reservation, stop until the reservation is replaced by a current plan or an
   explicit release marker. If it has an active claim, pick another unit.
2. Choose the branch name (per the Branches section) and post a claim
   comment on the issue: the versioned marker line plus one visible
   `Claim:` line naming that branch.

   ```text
   <!-- freeside-work-claim:v1 -->
   Claim: feat/example-slug
   ```

3. Re-read the issue's comments and claiming PRs. An active planning
   reservation on the issue blocks the claim. Among non-expired, unreleased
   claims, the earliest `created_at` wins; the numeric comment ID is the
   deterministic tie-breaker (lower wins). Ordering is by creation time;
   comment edits do not reorder claims.

4. A losing claimant posts a release comment bound to its own claim and
   stops (it may re-claim later with a new comment). A release comment
   releases exactly the claim comment whose numeric ID its
   `Releases-claim:` line names, never other claims: branch names do not
   identify a claim, since concurrent claimants following the same slug
   convention can choose the same one. The `Release:` line repeats the
   branch for human readability only.

   ```text
   <!-- freeside-work-release:v1 -->
   Release: feat/example-slug
   Releases-claim: 1234567890
   ```

5. The winner creates its dedicated worktree/branch from the freshly
   updated default-branch tip (per Branches) and begins work. No empty
   claim commit: the branch's first commit is real work.

The lease expires 48 hours after the claim comment's creation if no open PR
from the claimed branch carries the issue's close keyword by then; an expired
lease is dead, and re-claiming needs a new comment. Once an open PR from the
same branch contains the close keyword, that PR is the active claim and the
comment lease is subsumed (no further expiry), retaining the comment's
`created_at` and numeric ID when arbitration needs its ordering key. Closing
that PR unmerged releases the claim; merging closes the issue normally.

The active claim for a unit is therefore: a non-expired, unreleased comment
lease; or an open PR from the lease's branch with the issue's close keyword;
or, during the transition from the previous protocol, a legacy open PR
claiming the unit with a `Claim #N` commit or close keyword. A legacy open PR
with no claim comment uses its PR `created_at` and numeric PR ID only when
deterministic ordering is needed; a lease-backed PR always retains its claim
comment's key. A bare cross-reference (`Refs #N`) is never a claim. One claim
per unit: if an active claim exists, pick another unit. Do not create new empty
claim commits; drop any legacy one in the next branch rewrite (the fold-fix
rules under Commits). Claim state is verified, never assumed: a comment or PR
API read or write failure at any step fails closed, and work does not begin (or
continue past the failed step) while claim state cannot be verified.
Collaborator comments are trusted; adversarial comment editing is outside this
protocol's threat model.

`scripts/trackercollect unit` (its README) may collect the evidence these
reads use: the issue's typed Dependencies, its claim and reservation state
with each claim comment's ordering key, and the open trackers listing the
issue. It is optional and replaces none of the reads or gates here. Run it
afresh for the read after posting a claim. An `UNKNOWN` entry in its report
means that state is not established. Its `exclusive-with` output and its
`contracts` subcommand's cap and label checks predate the current rules and
gate nothing (#2357).

`needs-human` deferrals use the fiat door defined under Deferral escalation,
never self-selection: after the maintainer acts, fiat assigns the issue to a
session; the session verifies the external state and records the audit
diff in the ordinary close-keyword PR, adding a decision note only when
the outcome hits a Decision notes trigger or the mandatory-note list.

## Session Start

1. Read docs/plan.md front matter (revision), resolve wave state through the
   §11 three-state resolver over open issues carrying the `tracker` label and
   a milestone, and read the plan sections your unit's Affected
   interfaces/contracts field cites. In active-wave state (exactly one match)
   read that tracker for phase, wave, and active front; inter-wave state (no
   match) is a valid observed result with no active front, recorded rather
   than treated as a blocker; more than one match stops and escalates to the
   human as an invalid authority state.
2. When resuming an existing unit, read its issue or PR and any decision
   note it links (Decision notes section).
3. Status queries:
   - claims and the planning reservation on the unit you intend to claim:
     the paginated reads under Claiming;
   - wave state per the §11 resolver above: in active-wave state, the open
     current tracker; in inter-wave state, no current tracker (fiat may still
     proceed; scheduled self-selection may not).
4. Resolve every typed relationship before starting:
   - verify each `starts-after` prerequisite's PR is merged;
   - record each `merges-after` prerequisite for the handoff report; it does
     not block start for any unit (Relationship Types);
   - for each `stacked-on` relation, use the named branch explicitly while its
     base PR is open, and verify any existing child PR still names that base.
     If the base has merged, treat the relation as satisfied: start unbegun
     work from the current default branch, or verify an existing child PR was
     retargeted there. A base closed unmerged fails closed until it reopens or
     the planner repairs the relationship; record the relation for the
     handoff and integration checks.
   An unknown or materially ambiguous relationship is `starts-after` until the
   planner resolves it.

Nothing else gates the start: a session doesn't compare its scope with other
claims or open PRs, doesn't wait on another unit's reservation, and doesn't
count open contract units (AGENTS.md, Coordination Gates).

## Stages

AGENTS.md declares the work-unit stages and their binding mutation boundaries.
Planning is optional: an implementation unit whose issue has no planning-stage
handoff follows the ordinary implementation workflow. When planning does run,
its issue-body contract and plan comment carry the handoff across sessions.

Exactly one implementation-plan comment is current for a planned unit. Revise
that comment in place or mark it explicitly non-current before publishing its
replacement; never leave two comments that both appear current. The issue-body
work contract remains authoritative over its plan. AGENTS.md remains
authoritative project policy, and dependencies and current code reality may
invalidate a plan assumption. Implementation executes the plan rather than
replanning it, but surfaces any such conflict and follows the authoritative
source.

Apply AGENTS.md's Forge Edits policy throughout planning and recovery. The
existing reservation coordinates cooperating sessions; it is not a forge lock.

Planning reserves its assigned issue before it can change the authoritative
contract. The planning session reads the issue's claims and reservations
using the paginated queries under Claiming. If the issue has no active work
claim or planning reservation, write one comment on it with this marker and
visible line:

```text
<!-- freeside-planning-reservation:v1 -->
Plan: #N
```

The reservation blocks any implementation claim or scheduled pickup for its
own issue and no other unit. It does not authorize either stage and is not a
work claim. Verify the saved reservation and recheck the issue before
editing the contract. If a competing claim or reservation appears, release
this reservation and coordinate before continuing. On a completed plan,
revise the reservation in place into the single current implementation-plan
comment; on a blocked attempt, revise it in place with an explicit release
marker. An implementation session pages its issue's comments and stops on an
active reservation before claiming or starting, even when its scheduling or
fiat authorization is otherwise valid.

A reservation is active only while unreleased and less than 48 hours past its
forge-issued `created_at`. Expiry ends the holder's planning-write authority.
Read the holder's reservation and check its deadline immediately before each
planning write. A write may be issued only when enough reservation margin
remains to complete that write and its post-write verification before
the deadline. At or after the deadline the session makes no further planning
mutation; when the remaining margin is insufficient, it first obtains a fresh
reservation through the procedure below.

The margin is a pre-write fence, not an atomic visibility guarantee. If
verification nevertheless completes after expiry, the mutation remains visible
but unverified. The session makes no further planning mutation and does not
claim the planning finish line. Its sole post-expiry write is one recovery-only
comment on the assigned issue reporting the exact partial state; that comment
is not planning output or authority to continue planning. A successor reads
the report and the affected authoritative records during recovery before
writing.

Any owner-authorized `Plan #N` session, whether the original planning
session resuming or a successor, may recover an expired reservation using the
same checks as a new reservation. The original planning session may also
replace its own unexpired reservation when too little margin remains. In
either case: re-read the issue's claims and reservations; stop if a successor
claim or reservation is present; revise the old reservation comment with an
explicit release marker; then create a fresh reservation and verify it as
above. Replacing one's own reservation is not a takeover; no session takes
over another holder's unexpired reservation, and it stops for the active
planning session or owner to release it.

Before changing Dependencies, discover every open tracker that lists the unit
and the inputs needed to refresh its Status section: tracker membership and
diagram, each listed unit's contract and Dependencies,
prerequisite merge state, relevant open PRs, and stacked base/child lifecycle
and target state. If a required input cannot be verified,
stop the dependent writes and report the missing evidence. Do not require
exclusive control of those inputs or an atomic multi-record update.

Ground source claims in one immutable default-branch commit and name it in the
plan. At handoff, check whether the branch advanced and report any advance as
a freshness gap for the implementing session to assess. A known change to a
plan assumption requires revalidation; unrelated branch movement does not
require restarting planning.

Retire and verify any invalidated current plan before changing its contract.
Read each target's current text before editing, preserve unrelated content,
and verify the saved result. Update the authoritative issue first, refresh
every affected tracker, then publish the single current plan by replacing the
reservation. These writes are separate and may temporarily disagree. If a
write fails or conflicts with another edit, reconcile from current records
within the active reservation and authorized scope. If the work remains
incomplete, put the successful writes and remaining repairs in the
reservation's release edit, and do not claim planning complete.
The expiry rules above govern recovery-only reporting after the deadline.

After implementation, the human merge gate remains unchanged. A session that
records a verified merge applies the tracker refresh under
[Session End](#session-end) and [Tracking Issues](#tracking-issues), then
reports the post-merge results required by AGENTS.md.

## Unit Sizing

A work unit's expected pull request stays around or under 1,000 authored
changed lines. The budget is soft, not a gate: merged-PR review history
shows automated-review convergence flat below roughly that size and
multiplying above it (devlog 2026-08-18-0818-unit-size-budget.md, which
counted every added line; devlog 2026-10-05-1014-unit-sizing-revision.md
narrows the count to authored lines), so a unit kept deliberately larger
records its reason on the issue and proceeds.

The budget counts authored lines because each one is a decision a
reviewer has to weigh. One test tells authored lines from regenerated
output:

- **Authored:** someone, agent or person, decided the line, and a
  reviewer has to weigh it.
- **Regenerated:** a documented command reproduces the file from tracked
  inputs. A reviewer confirms that the command reproduces it, then scans
  the diff for change the authored lines don't explain.

The estimate declares regenerated output beside the authored count and
leaves it out of the count. That output is still verified, and a
golden's diff is still reviewed (`daemon/README.md`):

- the generated API client under
  `app/Sources/FreesideAPI/GeneratedSources/`, with the schema mirror and
  the contract-digest constants regenerated alongside it
  (`bash scripts/check.sh app generate`);
- golden files rewritten by `go test -update`
  (`daemon/internal/golden`);
- any other fixture a documented command rewrites. A fixture edited line
  by line is authored and counts.

Estimate the authored lines against the repository's known size
amplifiers, not the core change alone; a unit touching two or more is
presumed over budget:

- a new migration, which also joins every migration-subset exclusion
  list;
- a sync-carried contract field, which is `kind:contract` and adds the
  API schema edit; its generated app client is regenerated output and
  stays out of the count;
- new mock state, whose MockServer daemon parity lands in the first
  push.

Split along seams that keep each part a logical, independently passing
unit:

- persistence first: migration, store accessors, and goldens as one
  unit, with the behavior consuming them following;
- contract first: a new field and the behavior using it are two units
  by default, even when the field alone looks trivial; a `kind:contract`
  unit may instead carry its first behavioral consumer under the
  conditions below;
- happy path, then hardening: the working skeleton with its tests lands
  first; failure, recovery, and drift-tolerance behaviors follow as
  their own units;
- daemon projection versus app presentation;
- in-wave sequential dependency recorded as `starts-after` by default,
  or as an intentionally declared `stacked-on` pull request on a
  non-contract base; a contract-first split's dependent always waits
  for the contract unit to merge.

**A contract unit may carry its first behavioral consumer.** One unit
then holds the contract change and the first behavior that uses it,
when both conditions hold:

- **The two can be understood and verified together.** A reviewer reads
  the contract change and the behavior as one change, and one
  verification run exercises both.
- **No other unit is waiting on the contract alone.** The combined
  unit's pull request stays open longer than the contract change alone
  would, so the fold doesn't apply when another scheduled or reserved
  unit `starts-after` this unit and needs only the contract change.

The split stays the default. Keep it when the parts are each valuable
alone, when the contract serves several consumers, or when splitting
isolates design uncertainty.

The combined unit owes everything a contract unit owes. It stays
`kind:contract`, declares every component it touches, and gets contract
review and regenerated-output verification. Every further consumer
waits for its merge. The budget applies to it as to any other unit.

Check both conditions where the unit is shaped: at wave decomposition,
and again at planning. The Dependencies fields supply the evidence.
Folding a consumer into a contract unit is a planner or owner action, as
executing a split is; planning proposes the fold in the plan comment.

The budget applies at three checkpoints. Wave decomposition estimates
coarsely and splits the obvious cases. The planning stage refines the
estimate against real code and, over budget, proposes a concrete split
in the plan comment; executing the split remains a planner or owner
action. Once a plan proposes a split, the unit is not picked up
through the scheduling door until the planner applies the split or
records the deliberately-larger reason on the issue; fiat remains
independent. An implementation whose actual diff blows well past its
estimate stops growing the pull request: remainder work outside the
issue's acceptance criteria defers as a tracked issue, while deferring
acceptance-required work first needs the owner or planner to rescope the
unit's contract.

## Session End

Write or update the unit's decision note only when a Decision notes
trigger or the mandatory-note list applies. Additionally: deferrals
discovered mid-unit follow Deferral escalation below; when your PR
merges, apply the refresh in `docs/tracker-format.md` (§refresh) to every
open tracker that lists the unit, as one edit per tracker (Tracking Issues
below), or note partial state on the issue. A closed tracker, including a
completed wave's, is never reopened or mutated. No open containing tracker is
a valid zero-work result, not an error.

For post-merge reconciliation, `scripts/trackercollect` may collect the merged
unit's advisory forge evidence into a stamped `snapshot.json` and compact
`report.md` before judgment begins. Apply AGENTS.md's Forge Edits policy before
using them to edit a tracker. Recheck containing-tracker membership, the target
text, and the unit, dependency, and PR facts that determine the projection.
Inventory or `updatedAt` changes are reasons to inspect the affected evidence,
not automatic reasons to repeat the whole collection. Refresh that evidence
and recompute **Startable now** when contributing facts changed; use a fresh
collection when needed to recover a reliable baseline. The artifacts replace no required
claim, relationship, or integration check.

Before final handoff, check every `merges-after` prerequisite and name any
that has not merged in the handoff report, so the owner can order landing
(AGENTS.md, Integration Ordering and Merge-Result Audit). A stacked child also
remains non-mergeable until its base PR merges and the forge retargets the
child PR to the default branch; verify both facts from the current PR object.
These checks do not replace the review or verification gates in AGENTS.md.

## Deferral Escalation

Actionable work deferred out of a unit's scope gets a tracker issue
before handoff (per the finish line); the escalation follows these
rules:

- **Provenance when a note exists**: the issue form's optional
  `Source devlog entry` field cites the originating decision note's
  filename; the note may carry a plain `Follow-up: #N` historical link.
  Most escalations originate in the work itself and leave the field
  blank. Historical entries are frozen: never write markers or other
  mutations back to them.
- **Shared-package needs use `kind:contract`** plus the **`deferral`**
  origin label.
- **For non-contract work, `kind:*` by the work's nature** (deferred scope:
  feature; known gap: fix; hygiene: chore), plus the **`deferral`** origin
  label.
- **Maintainer-only actions** (repo settings, credentials, App
  administration) get **`needs-human`**. Self-selecting
  sessions and future scan initiators never pick up `needs-human` issues.
- **No milestone at escalation.** Open + `deferral` + no milestone is the
  unscheduled queue; the planner schedules eligible items during wave planning's
  deferral sweep and skips `needs-human`, which remains unmilestoned and
  fiat-only. Do not add status labels; milestone presence is the status.
- Closure is ordinary: a work-unit PR with a close keyword; the issue
  carries the item's whole status lifecycle.

**Pickup: labels never authorize work.** An issue (deferral, adversarial
finding, or anything else except `needs-human`) becomes agent-actionable
through exactly two doors: **scheduling** (a planner sweep assigns its
milestone and lists it on the current tracking issue, from which sessions
self-select; this door is open only in active-wave state per the §11 resolver,
since it needs an open current tracker) or **fiat** (the human hands its number
to a work-unit session, which covers urgent items and is independent of wave
state). A `needs-human` issue uses only fiat after the
maintainer acts, as Claiming defines. A session must never select work
directly by label or by browsing open issues. Sweep cadence: at every
planning session while waves exist; at phase boundaries after; ad hoc
whenever the human runs one. Between sweeps the unscheduled queue is dormant
by design; the Phase 1B scan initiator is the intended replacement for
human-cadence sweeping.

## Relationship Types

The Dependencies field in each unit issue is authoritative for relationships;
trackers derive views from it. A claim remains an occupancy signal only and
never creates a relationship or authorizes work.

- **`starts-after`:** A prerequisite's PR must merge before the dependent unit
  starts. Record it only when the dependent must be written against the
  prerequisite's result (AGENTS.md, Coordination, rule 3): working on the
  same subject or expecting a textual conflict needs no order. Example: wave
  5 unit #653 `starts-after` #652 in the contract chain.
- **`merges-after`:** A unit may start independently, but its PR must merge
  after the prerequisite's PR. This constrains landing order, never start
  order, for any unit, a contract unit included. Example: two disjoint
  documentation units may proceed in parallel while the later vocabulary
  consumer declares `merges-after: #791` so the defining change lands first.
  No tool reads the relation at landing: `land-approved-prs` orders a batch
  by approval time or by the owner's `--order`, so the relation holds only
  through the order the owner approves PRs or types. The planner tells the
  owner at handoff when a unit needs one, and the unit's handoff report
  names a prerequisite that has not merged (Session End).
- **`stacked-on`:** A unit intentionally bases its branch and PR on another
  unit's open PR branch. Example: #65 was stacked on #91's
  `feat/store-snapshot-meta` branch so it could use the unmerged store snapshot
  reads. The Stacked PRs section in AGENTS.md defines the branch and PR
  mechanics. Once the base merges, the relation is satisfied; unbegun work
  starts from the current default branch, while an existing child resumes only
  after the forge retargets it there. A base closed unmerged leaves the
  relation unsatisfied until it reopens or the planner repairs it.

**Retired relations.** #2020 retired `exclusive-with` and the contract
assessment without converting them: a pair that must be ordered gets a
`starts-after`, and everything else resolves at landing. Such a line left in
an older issue's Dependencies field is historical and gates nothing; the
planner deletes it when it next edits that field.

Unknown or materially ambiguous relationships serialize as `starts-after`
until the planner resolves and records the intended type. Coupled work still
forms one unit, an explicit relationship chain, or a declared stack; worktree
isolation alone never establishes independence.

## Migrations

Migrations keep their contiguous numbering: the store pins each applied
migration by name and digest, so every database has the same history and a
renamed or rewritten applied file is refused. Two open migration PRs
therefore land by renumbering, not by ordering their units:

1. Take the next number after the highest one on your base.
2. If another migration lands first, rename yours behind it at rebase with
   `git mv`, replace every reference to the old file name under `daemon/`,
   and rerun `bash scripts/check.sh daemon`. The rename is safe because the
   only persistent database that runs branch builds is the `dev` tier, whose
   state is disposable. A `dev` tier that ran the branch refuses the renamed
   history; clearing it is the operator's act, so report it rather than
   delete it.
3. Declare every name a migration after 0099 removes (a table, column,
   index, trigger, or view) with one `-- drop:` line each.
   `TestMigrationsDeclareDrops` fails on an undeclared removal, on a declared
   name that is still there, and on a malformed line; migrations 0001 to
   0099 predate the line.

The commands are in `daemon/README.md` (Adding a Migration). The test
doesn't see a constraint change lost in a table rebuild, or two migrations
rewriting the same rows. In both cases the second must be written against
the first's result, so the planner orders them with `starts-after` (rule 3).

## Reading a Rebase

Tests can't detect everything a rebase changes. A conflict resolution is
folded into the branch's own commits, where no diff shows it separately, and
it can drop another unit's change without failing a check. So after a rebase
that conflicted, whoever rebased reads what the resolution changed:

1. Keep the branch head from before the rebase (`git rev-parse HEAD` first,
   or `ORIG_HEAD` and the reflog afterward) and its old base.
2. Run `git range-diff <old-base>..<old-head> <new-base>..HEAD` and read
   every commit pair it marks as changed.
3. Record, for each conflicted commit, what the resolution kept, combined,
   or dropped.

The record has two forms, and `scripts/landing-measures.py` counts both as
rebase records, with the limits its header states: a lander comment counts
only when posted before the merge (#2362).

- **The author's:** after a rebase the author ran, the reading goes in the
  PR's Verification section, as a bullet that names `git range-diff` and
  says what each resolution changed.
- **The lander's:** when a `land-approved-prs` batch rebased the PR through
  a conflict or reworked it beyond a clean rebase, the batch posts a PR
  comment that says what changed and why and includes the range-diff (that
  skill's Comments and the Batch Report).

A clean rebase needs no record, but it can still change the branch's diff,
for example by following an upstream rename. The merge-result audit's scope
check catches a path that moved out of scope (AGENTS.md, Integration Ordering
and Merge-Result Audit), and the `required` check reruns on the pushed
result.

## Tracking Issues

Every tracker takes the shape in [`docs/tracker-format.md`](tracker-format.md):
a wave tracker, and an ad hoc tracker over a set of units (a feature or
backlog tracker) alike. That file is the owner's shared format, copied
verbatim from the agent-setup skill; don't edit it here. Read it before
creating, rewriting, or refreshing a tracker. It fixes the title, intro,
Status diagram and legend, Units, Exit, Notes, the merge-time refresh, and
style. A tracker carries start order only: it has no **Mergeable next**
bullet and no Implementation order section. The rules below are Freeside's
own additions to that format.

- **Labels and milestone.** Every tracker carries the `tracker` label. A wave
  tracker also carries the phase milestone its units get at scheduling, and
  no other tracker carries a milestone, because the §11 resolver identifies
  the wave tracker by the label plus a milestone. A wave tracker is titled
  `Wave N: <Name>`; tooling never reads the title. An ad hoc tracker's intro
  says that it isn't a wave tracker and that its units start by fiat.
- **Units.** Units form one flat list. A `needs-human` unit is owner-run: it
  goes under `### Owner-run` after that list, takes the `owner` class, and
  never appears in **Startable now**. An Owner-run entry isn't a scheduling
  listing: the unit stays unmilestoned and fiat-only, and the half-scheduled
  check skips it.
- **Relationships in the diagram.** The diagram draws `starts-after` edges as
  the format says. Freeside's other typed relationships appear this way:
  - `merges-after` draws as the format's dotted `-.->` edge, because it never
    blocks start.
  - `stacked-on` draws as a labeled arrow, `A -- stacked-on --> B`.
  - The `contract` class marks the classification and implies no relation.
  - When the diagram carries a dotted or `stacked-on` edge, an **Edges:**
    bullet under Status says what each means, because the verbatim legend
    covers only `starts-after`.
- **Critical path.** When a tracker is planned or a Dependencies change
  redraws its diagram, draw `==>` along the longest chain of unmerged units
  linked by `starts-after`, counted in units, and along every chain that
  ties it. A merge doesn't move it, because the format's refresh leaves
  edges alone. With no unmerged chain left, draw no thick arrows.
- **Startable now.** A structural projection: an open unit whose
  `starts-after` prerequisites have all merged and that no fence holds. A
  `stacked-on` unit also needs its named base PR open with any existing
  child still based there, or merged with no child yet or with its existing
  child retargeted to the default branch; a base closed unmerged has to
  reopen or have its relationship repaired. The projection leaves out
  volatile claim occupancy: as the format says, a claim or an open PR
  doesn't remove a unit, only its merge does, so every session queries
  claims live before claiming or starting. Write each entry as a bare `#N`,
  or `#N (contract)` for a contract unit. The projection never authorizes
  work or replaces a PR's verification and review gates.
- **Exit.** A tracker's close sentence requires its units merged and
  its Owner-run entries resolved, because an owner-run item may close
  without a PR. A wave tracker's Exit carries the §11 adversarial review as
  a bullet ending `Evidence: findings summary on this issue`: the review
  runs after every unit merges, so no unit supplies its proof.
- **Fences.** The `fenced` class and **Fenced:** bullet mark a unit held by
  something outside its Dependencies field, such as an owner decision or an
  external event. Name the fence in the bullet.
- **Repair with the change, as one operation.** Whoever changes a tracked
  unit's Dependencies field (a rescope, a planner repair, a new unit) updates
  the diagram and **Startable now** of every open tracker listing the unit
  in the same operation, mirroring the milestone-plus-listing rule under
  Work units in AGENTS.md. A session that opens, reopens, closes unmerged, or
  manually retargets a tracked `stacked-on` unit's PR refreshes **Startable
  now** the same way. When a merge should retarget stacked children
  automatically, the session recording that merge waits for and verifies
  each retarget before refreshing; if the retarget isn't observable yet, it
  records partial tracker state, and the child session refreshes once it
  verifies the retarget. A merge itself triggers only the format's refresh
  (§refresh). Apply Forge Edits to all of these: "one operation" means
  completing the related repairs in this work unit, not an atomic forge
  transaction. Coordinate with known writers to the same tracker, verify
  each saved result, and report any repair that remains incomplete.
- **Pins carry no authority.** The §11 resolver reads the `tracker` label and
  milestone, never pins, so an interrupted pin change can't misstate wave
  state. Wave planning pins the new wave tracker and unpins the prior one for
  visibility. Standing ad hoc trackers stay pinned for their own purposes;
  GitHub caps pins at three per repository, so when no slot is free the
  owner picks which tracker to unpin. The plan-wave skill's artifact check
  catches an interrupted wave-planning run: it stops for resume-or-repair
  direction rather than creating a second wave tracker.
