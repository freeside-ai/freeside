# Coordination Protocol

The mechanics behind AGENTS.md's coordination gates: the lane glossary, the
work-unit issue shape and wave-state terms, the claim-lease protocol, the
session-start queries, session end, deferral escalation, and Freeside's
additions to the tracker format in `docs/tracker-format.md`. Read this file before claiming a
unit, filing a deferral, starting an issue-backed session, or creating or
updating a tracking issue.

AGENTS.md holds the binding gates and is the authority where the two
disagree; this file carries only the procedure that implements them. Section
names cited below that do not appear in this file (Branches, Commits, Work
units, Decision notes, the finish line, Monorepo scope discipline) refer to
AGENTS.md.

## Lane Glossary (Canonical)

Lane names are search keys and territory labels, defined canonically here;
subsystem-derived lane names (signet, gauntlet, publish is functional, ward)
also appear in docs/plan.md §15, which defines saddle and spine as
coordination vocabulary outside the subsystem register. They never appear in
code identifiers, package names, or API vocabulary, which stay functional
(the attention type is AttentionItem, not SignetItem).

| Lane | What it is | Owns (paths) | Plan |
|---|---|---|---|
| signet | Attention service: items, deliveries, conversations, sync, devices | daemon/internal/signet (api/ is shared contract territory: changes are `kind:contract`, drafted by the signet/saddle pair) | §4, §5.14 |
| gauntlet | Candidate path: export helper, hostile importer, clean verifier, evidence channel | daemon/internal/export, daemon/internal/importer, daemon/internal/verify | §5.6, §5.15 |
| publish | GitHub App auth, deterministic identities, reconciliation, EvidencePublisher | daemon/internal/publish | §5.5, §5.9, §5.11, §5.15 |
| ward | Runner backends, workspace-handoff gate, conformance, operating modes | daemon/internal/ward | §5.7 |
| saddle | SwiftUI clients (pipeline-exempt) | app/ | §5.14, §11 |
| spine | A ROLE, not a territory: serialized shared-contract changes (domain, migrations, interfaces, api/) and Wave 2 integration (workflow engine) | daemon/internal/domain, daemon/internal/store, daemon/internal/exec, daemon/internal/engine, daemon/migrations/, api/ | §11 |

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
- Dependencies: `starts-after`, `merges-after`, `stacked-on`, and
  `exclusive-with` (see Relationship Types), never untyped issue refs. Record
  an unknown or materially ambiguous relationship as `starts-after` until the
  spine resolves it.

Labels: `lane:*` for ownership area, `kind:*` for type, and `tracker` for an
issue that tracks other issues. Milestones carry the phase (1A, 1B). Each
wave has a tracking issue listing its units, maintained by the spine role.
Every tracker, wave or ad hoc, follows Tracking Issues below.

Wave state resolves per the §11 three-state resolver over open issues that
carry the `tracker` label and a milestone: exactly one is active-wave state,
none is inter-wave state, and more than one is an invalid authority state for
spine repair. The scheduling door exists only in active-wave state, because
it needs an open current tracker to list the unit. Fiat (`Plan #N`, `Handle
#N`) is independent of wave state and may proceed in either state after all
ordinary gates pass.

**Scheduled** means both a milestone and a listing on the current tracking
issue. The spine changes those fields as one planning operation; either field
alone is a spine-repair error and does not open the scheduling door (fiat
remains independent). The wave tracker carries the milestone without being a
unit, so it is neither scheduled nor a spine-repair error.

## Claiming

A claim records occupancy only; authorization comes from scheduling or
fiat (see Pickup), never from the claim itself. Issue-backed implementation
work is claimed with an issue-comment lease that hands off to a real PR. The
planning stage never claims its issue: its planning reservation under
Stages blocks implementation of that issue while planning is active, but does
not authorize planning or implementation. Direct no-issue work needs no claim:
it is not eligible for concurrent or multi-session execution, and gets promoted
to an issue before that changes (see Work units).

Claim arbitration includes the current unit and every unit directly related by
a forward or reverse `exclusive-with` declaration. Claims conflict when they
name the same unit or directly exclusive units; an active planning reservation
blocks a claim across that same set. After posting, every contender rechecks
the whole set and orders conflicting, non-expired, unreleased claim comments
by `created_at`, with the numeric comment ID as the tie-breaker. The earliest
wins. A conflicting claim either predates a contender's recheck and is seen
there, or is posted later and sees the first claim on its own recheck; the
total order prevents livelock without making exclusivity transitive.

To claim a unit:

1. Confirm the issue is authorized (scheduled or fiat-assigned). Page every
   open work-unit issue body to find forward and reverse `exclusive-with`
   declarations, then fully page comments and claiming PRs for the current
   issue and every directly related unit. If any member has an active planning
   reservation, stop until it is replaced by a current plan or an explicit
   release marker. If any has a conflicting active claim, pick another unit.
2. Choose the branch name (per the Branches section) and post a claim
   comment on the issue: the versioned marker line plus one visible
   `Claim:` line naming that branch.

   ```text
   <!-- freeside-work-claim:v1 -->
   Claim: feat/example-slug
   ```

3. Re-page the open work-unit issue bodies, rebuild the direct exclusivity set,
   and re-read every set member's comments and claiming PRs. An active planning
   reservation on any member blocks the claim. Among conflicting, non-expired,
   unreleased claims, the earliest `created_at` wins; the numeric comment ID is
   the deterministic tie-breaker (lower wins). Ordering is by creation time;
   comment edits do not reorder claims. A new declaration appearing between
   reads is a relationship edit: stop until that edit completes, then repeat
   the relationship and claim reads. The edit protocol below prevents the
   relationship from changing while both endpoints are actively claimed.

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
   - active work-unit claims and open PRs: inspect overlapping scopes under
     Shared-Path Coordination below; a shared path alone does not block a claim
     or implementation;
   - active claims on any unit you intend to claim: the paginated reads and
     deterministic direct-exclusivity-set arbitration under Claiming;
   - reverse exclusivity declarations: page every open work-unit issue body,
     find every `exclusive-with` declaration that names the current unit, and
     verify that none of those declaring units has an active claim or planning
     reservation; a
     declaration on either unit applies symmetrically, so checking only the
     current unit's Dependencies field is insufficient;
   - wave state per the §11 resolver above: in active-wave state, the open
     current tracker; in inter-wave state, no current tracker (fiat may still
     proceed; scheduled self-selection may not);
   - open `kind:contract` issues, ignoring a `deferral` issue until it is
     scheduled or has an active claim, then excluding the unit you are claiming
     and any unit whose `starts-after` chain includes it (a
     `starts-after` chain of contract units keeps at most one
     claimable at a time, so downstream chain members may stay filed
     without blocking their chain head): among the remainder, if one
     touches your Affected interfaces/contracts, block on it; when claiming a
     `kind:contract` unit, block on every other remaining open contract unit
     (contract work is serialized).
4. Resolve every typed relationship before starting:
   - verify each `starts-after` prerequisite's PR is merged;
   - record each `merges-after` prerequisite for the handoff and integration
     checks; it does not block start;
   - for each `stacked-on` relation, use the named branch explicitly while its
     base PR is open, and verify any existing child PR still names that base.
     If the base has merged, treat the relation as satisfied: start unbegun
     work from the current default branch, or verify an existing child PR was
     retargeted there. A base closed unmerged fails closed until it reopens or
     the spine repairs the relationship; record the relation for the handoff
     and integration checks; and
   - verify no `exclusive-with` unit is active, including the reverse
     declarations found by the status query above.
   An unknown or materially ambiguous relationship is `starts-after` until the
   spine resolves it.

## Shared-Path Coordination

Apply the ordinary authorization, claim, reservation, typed-relationship, and
contract gates first. This procedure resolves incidental path overlap; it does
not override those gates or reassign another unit's work.

1. **Inspect the intended changes.** Read both work contracts, current plans,
   and available diffs. A component, directory, or shared filename is a search
   boundary, not an exclusive lock. An unfinished PR diff does not exhaust its
   unit's planned scope. Compare behavior and inputs as well as edit locations:
   different files or a clean merge do not prove independence.
2. **Proceed when the edits are independent.** Name the functions, test cases,
   baseline keys, or document sections each unit will change and why neither
   relies on the other's unfinished result. Record this on the work-unit issue
   before starting the overlapping work. When the existing scopes already
   establish that separation, this is an evidence record, not a permission
   request: no reply, release, or merge is required. Do not narrow another
   unit's scope unilaterally. Use isolated checkouts and preserve both changes
   during integration.
3. **Limit a real conflict to the affected work.** If two units make competing
   changes to the same behavior or content, or one needs the other's unfinished
   result, identify that dependency and propose a concrete boundary or order.
   Pause the affected edits until the conflict is resolved; continue work whose
   correctness does not depend on that resolution. Block the whole unit only
   when an existing gate requires it or no independent work remains. If the
   available scope is too vague to decide, name the missing fact and seek that
   clarification rather than treating every shared file as owned in full.
4. **Recheck when the scope or base changes.** Broader edits can invalidate an
   earlier separation. Resolve textual conflicts while preserving both units'
   intent, and repeat the required integration checks against the new base.
   A predicted textual conflict or baseline refresh is not by itself a reason
   to serialize implementation.

Examples:

| Overlap | Treatment |
| --- | --- |
| Different rows in `app/SURFACES.md` | Record the rows and proceed. |
| Separate screenshot cases and baseline keys with unchanged shared rendering inputs | Record the cases and keys and proceed. |
| A shared screenshot renderer changes while another unit adds baselines that depend on its output | Coordinate the renderer-dependent work; unrelated edits can continue. |
| Competing changes to the same lifecycle rule, even in different files | Resolve the behavioral conflict before the affected work proceeds. |

Plans and blocker reports must name the contested behavior or content. Do not
turn “these units share files” into “shared files need one writer” or an implied
`starts-after` relationship. The unknown-relationship fallback concerns an
actual unresolved dependency, not path overlap alone. Existing explicit
relationships remain authoritative until changed through their normal protocol.

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
contract. The planner reads the issue and its direct conflict set using the
paginated claim, reservation, and forward/reverse relationship queries under
Claiming. If no member has an active work claim or planning reservation, write
one comment on the assigned issue with this marker and visible line:

```text
<!-- freeside-planning-reservation:v1 -->
Plan: #N
```

The reservation blocks any implementation claim or scheduled pickup for that
issue and every direct `exclusive-with` partner. It does not authorize either
stage and is not a work claim. Verify the saved reservation and recheck the
conflict set before editing the contract. If a competing claim or reservation
appears, release this reservation and coordinate before continuing. On a
completed plan, revise the reservation in place into the single current
implementation-plan comment; on a blocked attempt, revise
it in place with an explicit release marker. An implementation session pages
the comments of its entire direct conflict set and stops on an active
reservation before claiming or starting, even when its scheduling or fiat
authorization is otherwise valid.

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

Any owner-authorized `Plan #N` session, whether the original planner resuming
or a successor, may recover an expired reservation using the same conflict
checks as a new reservation. The original planner may also replace its own
unexpired reservation when too little margin remains. In either case: re-read
the conflict set; stop if a successor claim or reservation is present; revise
the old reservation comment with an explicit release marker; then create a
fresh reservation and verify it as above. Replacing one's own reservation is
not a takeover; no session takes over another holder's unexpired reservation,
and it stops for the active planner or owner to release it.

Before changing Dependencies, discover every open tracker that lists the unit
and the inputs needed to refresh its Status section: tracker membership and
diagram, each listed unit's contract and Dependencies,
prerequisite merge state, relevant open PRs, and stacked base/child lifecycle
and target state. An `exclusive-with` change still checks both proposed
endpoints under Relationship Types. If a required input cannot be verified,
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

A work unit's expected pull request stays around or under 1,000 changed
lines. The budget is soft, not a gate: merged-PR review history shows
automated-review convergence flat below roughly that size and
multiplying above it (devlog 2026-08-18-0818-unit-size-budget.md), so a
unit kept deliberately larger records its reason on the issue and
proceeds.

Estimate against the repository's known size amplifiers, not the core
change alone; a unit touching two or more is presumed over budget:

- a new migration, which also joins every migration-subset exclusion
  list;
- store plus domain golden regeneration;
- a sync-carried contract field, which is `kind:contract` and drags the
  API schema plus the generated app client;
- new mock state, whose MockServer daemon parity lands in the first
  push.

Split along seams that keep each part a logical, independently passing
unit:

- persistence first: migration, store accessors, and goldens as one
  unit, with the behavior consuming them following;
- contract first: a new field and the behavior using it are two units
  even when the field alone looks trivial;
- happy path, then hardening: the working skeleton with its tests lands
  first; failure, recovery, and drift-tolerance behaviors follow as
  their own units;
- daemon projection versus app presentation;
- in-wave sequential dependency recorded as `starts-after` by default,
  or as an intentionally declared `stacked-on` pull request on a
  non-contract base; a contract-first split's dependent always waits
  for the contract unit to merge.

The budget applies at three checkpoints. Wave decomposition estimates
coarsely and splits the obvious cases. The planning stage refines the
estimate against real code and, over budget, proposes a concrete split
in the plan comment; executing the split remains a spine or owner
action. Once a plan proposes a split, the unit is not picked up
through the scheduling door until the spine applies the split or
records the deliberately-larger reason on the issue; fiat remains
independent. An implementation whose actual diff blows well past its
estimate stops growing the pull request: remainder work outside the
issue's acceptance criteria defers as a tracked issue, while deferring
acceptance-required work first needs the owner or spine to rescope the
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

Before final handoff and again immediately before integration, verify every
`merges-after` prerequisite is merged. A stacked child also remains
non-mergeable until its base PR merges and the forge retargets the child PR to
the default branch; verify both facts from the current PR object. These checks
do not replace the base-freshness, review, or verification gates in AGENTS.md.

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
- **Lane label routes by owner, not discoverer**: the lane whose Scope /
  declared paths contain the work. Shared-package needs use
  `kind:contract` plus the **`deferral`** origin label.
- **For non-contract work, `kind:*` by the work's nature** (deferred scope:
  feature; known gap: fix; hygiene: chore), plus the **`deferral`** origin
  label.
- **Maintainer-only actions** (repo settings, credentials, App
  administration) get **`needs-human`** and no lane label. Self-selecting
  sessions and future scan initiators never pick up `needs-human` issues.
- **No milestone at escalation.** Open + `deferral` + no milestone is the
  unscheduled queue; the spine schedules eligible items during wave planning's
  deferral sweep and skips `needs-human`, which remains unmilestoned and
  fiat-only. Do not add status labels; milestone presence is the status.
- Closure is ordinary: a work-unit PR with a close keyword; the issue
  carries the item's whole status lifecycle.

**Pickup: labels never authorize work.** An issue (deferral, adversarial
finding, or anything else except `needs-human`) becomes agent-actionable
through exactly two doors: **scheduling** (a spine sweep assigns its
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
  starts. Example: wave 5 unit #653 `starts-after` #652 in the contract chain.
- **`merges-after`:** A unit may start independently, but its PR must merge
  after the prerequisite's PR. This constrains integration order, never start
  order. Example: two disjoint documentation units may proceed in parallel
  while the later vocabulary consumer declares `merges-after: #791` so the
  spine integrates the defining change first.
- **`stacked-on`:** A unit intentionally bases its branch and PR on another
  unit's open PR branch. Example: #65 was stacked on #91's
  `feat/store-snapshot-meta` branch so it could use the unmerged store snapshot
  reads. The Stacked PRs section in AGENTS.md defines the branch and PR
  mechanics. Once the base merges, the relation is satisfied; unbegun work
  starts from the current default branch, while an existing child resumes only
  after the forge retargets it there. A base closed unmerged leaves the
  relation unsatisfied until it reopens or the spine repairs it.
- **`exclusive-with`:** The named units may not be active concurrently; the
  relation is symmetric, even when only one unit declares it, and does not
  otherwise impose start or merge order. Session Start therefore checks both
  the current unit's declarations and reverse declarations in every open
  work-unit issue, then rechecks all directly conflicting claims after posting
  its own. Before adding a declaration, the editor fully queries active claims
  and planning reservations on both endpoints. A planner may keep
  its own unexpired reservation on its assigned issue; every claim and every
  other active reservation blocks the declaration, and its own reservation
  must have sufficient write-and-verification margin. The editor coordinates
  through issue comment and waits until the blocking record is released, then
  re-runs the claim and reservation reads before editing. A claimant that
  observes a relationship edit stops until it completes and then repeats both
  relationship, claim, and reservation reads.
  Example: wave 5 unit #680 was `exclusive-with` #448 and #492 while their
  declared paths overlapped on the Codex review sources.

Unknown or materially ambiguous relationships serialize as `starts-after`
until the spine resolves and records the intended type. Coupled work still
forms one unit, an explicit relationship chain, or a declared stack; worktree
isolation alone never establishes independence.

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
- **Lanes.** Units group under `### lane:<name>` by the unit's first lane
  label (see Lane Glossary). A `needs-human` unit is owner-run: it goes under
  `### Owner-run`, takes the `owner` class, and never appears in **Startable
  now**. An Owner-run entry isn't a scheduling listing: the unit stays
  unmilestoned and fiat-only, and the half-scheduled check skips it.
- **Relationships in the diagram.** The diagram draws `starts-after` edges as
  the format says. Freeside's other typed relationships appear this way:
  - `merges-after` draws as the format's dotted `-.->` edge, because it never
    blocks start.
  - `stacked-on` draws as a labeled arrow, `A -- stacked-on --> B`.
  - `exclusive-with` isn't drawn. The `contract` class already implies the
    repo-wide regime among contract units. A non-contract pair gets an
    **Exclusive:** bullet under Status while both units are open.
  - When the diagram carries a dotted or `stacked-on` edge, an **Edges:**
    bullet under Status says what each means, because the verbatim legend
    covers only `starts-after`.
- **Critical path.** When a tracker is planned or a Dependencies change
  redraws its diagram, draw `==>` along the longest chain of unmerged units
  linked by `starts-after`, counted in units, and along every chain that
  ties it. A merge doesn't move it, because the format's refresh leaves
  edges alone. This is a structural stand-in until #674 settles whether an
  estimate source exists. With no unmerged chain left, draw no thick arrows.
- **Startable now.** A structural projection: an open unit whose
  `starts-after` prerequisites have all merged and that no fence holds. A
  `stacked-on` unit also needs its named base PR open with any existing
  child still based there, or merged with no child yet or with its existing
  child retargeted to the default branch; a base closed unmerged has to
  reopen or have its relationship repaired. The projection leaves out
  volatile claim and active `exclusive-with` occupancy: as the format says,
  a claim or an open PR doesn't remove a unit, only its merge does, so every
  session queries those live before claiming or starting. Write each entry
  as `#N (lane)`, the lane without its `lane:` prefix, adding `contract` for
  a contract unit: `#N (lane, contract)`. The projection never authorizes
  work or replaces a PR's verification and review gates.
- **Exit.** A tracker's close sentence requires its lane units merged and
  its Owner-run entries resolved, because an owner-run item may close
  without a PR. A wave tracker's Exit carries the §11 adversarial review as
  a bullet ending `Evidence: findings summary on this issue`: the review
  runs after every unit merges, so no unit supplies its proof.
- **Fences.** The `fenced` class and **Fenced:** bullet mark a unit held by
  something outside its Dependencies field, such as an owner decision or an
  external event. Name the fence in the bullet.
- **Repair with the change, as one operation.** Whoever changes a tracked
  unit's Dependencies field (a rescope, a spine repair, a new unit) updates
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
