# Coordination Protocol

The mechanics behind AGENTS.md's coordination gates: the lane glossary, the
claim-lease protocol, the session-start queries, session end, deferral
escalation, and the tracking-issue format. Read this file before claiming a
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

## Claiming

A claim records occupancy only; authorization comes from scheduling or
fiat (see Pickup), never from the claim itself. Issue-backed implementation
work is claimed with an issue-comment lease that hands off to a real PR. The
planning stage never claims its issue: its guarded-write reservation under
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
   §11 three-state resolver over every pinned issue whose title matches the
   canonical wave-tracker pattern, and read the plan sections your unit's
   Affected interfaces/contracts field cites. In active-wave state (exactly one
   open match) read that tracker for phase, wave, and active front; inter-wave
   state (exactly one closed match) is a valid observed result with no active
   front, recorded rather than treated as a blocker; zero or multiple matches
   stop and escalate to the human as an invalid authority state.
2. When resuming an existing unit, read its issue or PR and any decision
   note it links (Decision notes section).
3. Status queries:
   - open PRs and their declared paths: overlap with yours means stop and
     coordinate via issue comment before claiming;
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

Planning reserves its assigned issue before it can change the authoritative
contract. Inside the conflict guard, the planner verifies that the issue has no
active work claim and writes one comment with this marker and visible line:

```text
<!-- freeside-planning-reservation:v1 -->
Plan: #N
```

The reservation blocks any implementation claim or scheduled pickup for that
issue and every direct `exclusive-with` partner. It does not authorize either
stage and is not a work claim. Inside the same guard, the planner verifies no
member of that direct conflict set has an active work claim or reservation
before posting it. On a completed plan, revise the reservation in place into
the single current implementation-plan comment; on a blocked attempt, revise
it in place with an explicit release marker. An implementation session pages
the comments of its entire direct conflict set and stops on an active
reservation before claiming or starting, even when its scheduling or fiat
authorization is otherwise valid.

A reservation is active only while unreleased and less than 48 hours past its
forge-issued `created_at`. Expiry ends the holder's planning-write authority.
The holder's own reservation deadline is a guarded input, reread immediately
before every planning write. A write may be issued only when enough reservation
margin remains to complete that write and its post-write verification before
the deadline. At or after the deadline the session makes no further planning
mutation; when the remaining margin is insufficient, it first obtains a fresh
reservation through the procedure below.

The margin is a pre-write fence, not an atomic visibility guarantee. If
verification nevertheless completes after expiry, the mutation remains visible
but unverified. The session makes no further planning mutation and does not
claim the planning finish line. Its sole post-expiry write is one recovery-only
comment on the assigned issue reporting the exact partial state; that comment
is not planning output or authority to continue planning. A successor includes
the report and every authoritative affected resource in the complete guarded
reread and recovery before writing.

Any owner-authorized `Plan #N` session, whether the original planner resuming
or a successor, may recover an expired reservation only inside the complete
guard. The original planner may use the same guard to replace its own
unexpired reservation when too little margin remains. In either case: re-read
the conflict set; stop if a successor claim or reservation is present; revise
the old reservation comment with an explicit release marker; then create a
fresh reservation. Existing claim and reservation arbitration decides recovery
races. Replacing one's own reservation is not a takeover; no session takes over
another holder's unexpired reservation, and it stops for the active planner or
owner to release it.

Before any planning write, derive the complete transaction. A Dependencies
change first discovers every open tracker that lists the unit and every input
needed to refresh each projection: tracker membership and Implementation order,
each listed unit's contract and Dependencies, prerequisite merge state, the
relevant open-PR set, and stacked base/child lifecycle and target state. The
conflict guard spans that discovery as well as the moving authoritative branch
reference, issue body, comment collection, current-plan set, the planner's own
reservation deadline, active planning reservations, and active work-claim
state. An `exclusive-with` change includes the active claims and planning
reservations of both proposed endpoints; the planner's own unexpired
reservation with sufficient write-and-verification margin on its assigned
issue is the only permitted active endpoint record.

Acquire and validate the complete guard before the first Dependencies write;
hold it through the issue-body change, every tracker repair, and post-write
verification. The guard must provide verified exclusive mutation ownership or
atomically reject the entire transaction when any guarded input changes. A
per-resource rejection after the issue change is unsafe because it can leave
the authoritative Dependencies ahead of its tracker projections. A fresh
reread alone does not close the cross-resource race. When the complete set
cannot be discovered or guarded, prepare ready-to-post artifacts from freshly
reread state but make no planning mutation and report the stage blocked.

Inside an available guard, freshly reread every guarded input immediately
before writing. Afterward, reread the authoritative representation of every
intended write while the guard still holds. Any intervening change, incomplete
result, or mismatch rejects the entire mutation set; report it instead of
claiming the planning finish line.

After implementation, the human merge gate remains unchanged. A session that
records a verified merge applies the tracker transition and projection refresh
under [Session End](#session-end) and [Tracking Issues](#tracking-issues), then
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
merges, tick your unit on every open tracker that lists it, re-marking its
diagram node with the merged double border when the tracker has a diagram
and refreshing the **Startable now** and **Mergeable next** projections in
each tracker's Implementation order in the same edit (Tracking Issues
below), or note partial state on the issue. Resolve the wave tracker through the §11 resolver: tick it
only in active-wave state when it lists the unit; in inter-wave state the sole
title match is the closed prior-wave tracker, which is never reopened or
mutated. No open containing tracker is a valid zero-work result, not an error.

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
  and planning reservations on both endpoints. A planning transaction may keep
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

An issue that tracks other issues (a wave tracker, or any ad hoc tracker
over a set of units) carries an **Implementation order** section:
implementation order is the question a tracker's readers bring to it, and
per-unit Dependencies fields scattered across the tracked issues do not
answer it at a glance. Wave 5's tracker (#651) is the reference example.

- **Prose digest first.** State **Startable now**, **Mergeable next**, each
  typed relationship chain, the cross-cutting gates, and the critical path as
  scannable text. **Startable now** is a structural projection: it contains
  unfinished units whose `starts-after` prerequisites are merged; a
  `stacked-on` unit also needs its named base PR to be open with any existing
  child still based there, or merged with no child yet or with its existing
  child retargeted to the default branch. A base closed unmerged needs to
  reopen or have its relationship repaired. The projection deliberately omits
  volatile claim and active-`exclusive-with` occupancy, which every session
  must query live before claiming or starting. **Mergeable next**
  contains open PRs whose `merges-after` prerequisites are merged, in spine
  integration order; a stacked child remains excluded until its base PR is
  merged and the forge has retargeted the child to the default branch. State
  `none` when either projection is empty. Neither projection authorizes work
  or replaces the PR's verification and review gates. The digest is the
  record; a reader who never renders the diagram still gets the order.
- **Diagram when the graph is nontrivial.** When the relationship graph is
  more than a single chain, follow the digest with a Mermaid
  `flowchart LR` (the forge renders it inline). A strictly linear sequence
  states its chain in prose and skips the diagram: a mandatory chart
  everywhere trains readers to skip charts.
- **Fixed edge semantics, stated in a legend line.** Nodes are issue numbers.
  Each relationship has one Mermaid edge and no generic arrow is overloaded:
  `A --> B` means B `starts-after` A; `A -.-> B` means B `merges-after` A;
  `A ==> B` means B is `stacked-on` A; and the symmetric `A -.- B` means A is
  `exclusive-with` B. The legend below every diagram states these exact
  meanings, including unused styles so readers never infer semantics from
  appearance. A `classDef` may highlight a category such as contract units,
  but never encodes a relationship.
- **Merged units carry a double border.** A tracked unit whose closing PR
  has merged renders as a double-bordered node (Mermaid's subroutine
  shape: `679[["#679"]]`); an unfinished unit stays a plain node. The
  legend states the marking alongside the edge meanings. Like a
  `classDef` highlight, the border encodes unit state, never a
  relationship.
- **Transitive reduction.** Draw only direct edges; an ordering already
  implied through drawn paths is not repeated as its own arrow.
- **Authority disclaimer.** The digest and diagram are a derived view;
  each unit issue's Dependencies field is the authority. Say so on the
  tracker: where they diverge, the unit issue wins and the tracker gets
  repaired.
- **Repair with the change, as one operation.** Whoever changes a tracked
  unit's Dependencies field (a rescope, a spine repair, a new unit)
  updates the digest and diagram of every open tracker listing the unit
  in the same operation, mirroring the milestone-plus-listing rule under
  Work units in AGENTS.md.
  A session that opens, reopens, closes unmerged, or manually retargets a
  tracked unit's PR refreshes every affected projection in the same operation.
  When a merge should retarget stacked children automatically, the session
  recording that merge waits for and verifies each retarget before refreshing
  the affected projections. If the retarget is not yet observable, it records
  partial tracker state; the child session refreshes when it later verifies the
  retarget.
  Merges advance the order the same way: the session recording a merged
  unit on a tracker, wave or ad hoc (the Session End tick), ticks the
  unit in the tracker's unit list, re-marks its diagram node with the
  merged double border when the tracker has a diagram, and refreshes
  that tracker's **Startable now** and **Mergeable next** projections,
  all in the same edit, so routine
  progress never strands a digest or diagram at its publication state.
  A stale diagram misleads where no diagram merely omits.
- **Wave-boundary pinning keeps exactly one wave-title match pinned, executed
  recovery-safely by the spine.** The §11 resolver counts only pinned issues
  whose titles match the canonical wave-tracker pattern; unrelated trackers (for
  example the standing ad hoc audit and reliability trackers, currently #799 and
  #578) stay pinned for their own purposes and never count toward wave state.
  Among the wave-title matches the settled count is exactly one open in
  active-wave state, or exactly one closed, the inter-wave marker, between a
  wave's close and the next wave's planning; closing a wave leaves its closed
  tracker pinned as that sole marker until the next wave is planned. Moving the
  wave-title match from the closed marker to the new open tracker is the spine's
  wave-tracker maintenance (the spine role maintains the pinned tracking issue
  per Work Units in AGENTS.md), a wave-planning action distinct from the
  per-issue `Plan #N` Planning stage, so it is authorized outside that stage's
  allowed-mutation surface. GitHub caps pins at three per repository and those
  standing non-wave trackers occupy slots, so the wave tracker effectively holds
  a single swappable pin slot: GitHub will not pin a fourth issue, so the
  outgoing and incoming wave trackers cannot both be pinned at once, and with no
  atomic pin swap the transition is necessarily non-atomic and multi-step. The
  spine's wave-planning operation therefore performs it idempotently and
  recovery-safely, in particular discovering and reusing any orphaned
  open-unpinned wave-title tracker left by an interrupted prior transition rather
  than creating a second. An invalid wave-title cardinality (zero or multiple
  wave-title matches) is what the resolver escalates on, never guesses through.
  The detailed interruption-safe procedure is owned and hardened by its executor,
  the spine's wave-planning operation; see #828.
