# Agent Commit Structure Survives the Gauntlet as Serialized History

Gauntlet unit #193 (decide whether agent commit structure survives the
export), raised by the owner in PR #192 review. Basis: plan §5.6 (two
channels, hostile importer, clean-commit authoring), the export
manifest contract (2026-07-16-1237-export-manifest-contract.md), and
the hostile importer design (2026-07-16-1515-hostile-importer.md).
This is a design decision; implementation is a separate unit per the
issue's non-goals.

## Decision

**Adopted (owner decision, this session): the agent's commit
structure crosses the gauntlet as serialized data, and the daemon
re-authors one clean commit per non-empty normalized first-parent
state transition of the agent's history.** The agent
commits normally in its workspace; the export helper walks the
workspace history from the handed base to HEAD and emits it as an
ordered list of commit messages and tree states in the existing
normalized-manifest vocabulary; the hostile importer validates every
commit in the chain exactly as it validates the head today; the daemon
authors fresh commits, one per chain entry, onto the exact base. No
workspace git object, hook, or configuration ever crosses, and the
importer's never-trust-workspace-`.git` invariant is unchanged: what
crosses is a description of the history, through the repo-change
channel's existing validation machinery, not the history itself.

What deliberately does not survive: agent commit SHAs, timestamps, and
author/committer identity. Re-authoring makes new SHAs by construction;
identity and timestamps are daemon-controlled so the candidate branch
carries clean provenance. The agent's messages survive as validated,
labeled, untrusted text.

Chose this over three rejected alternatives:

- **A proposed commit-partition manifest** (the agent partitions the
  final diff into ordered path sets). Rejected: the intermediate trees
  it produces are synthetic states the agent never built or tested, so
  the structure it preserves actively undermines bisect, the main
  value commit structure serves; it also invents a bespoke agent-facing
  format when git itself is the interface every agent already speaks.
- **Message-only** (single clean commit, agent proposes its message).
  Rejected as the whole answer: it loses commit boundaries, which the
  owner judged worth preserving; it survives inside the adopted design
  as the degenerate single-commit chain.
- **Importing workspace git objects verbatim** (validate, then adopt
  the agent's own commits to preserve SHAs). Rejected: it puts git's
  object and packfile parsers on the trust boundary in the daemon's
  own repository for no review value; SHAs carry no information a
  reviewer needs.

Declining entirely was also on the table and rejected: boundaries and
messages are real review and diagnostic signal, the repo's own history
conventions exist because that signal matters, and the safe carrier
described here was judged to add validated surface, not trust.

## Prior Decisions Revised

Three recorded decisions are revised, all by the owner in this unit;
the trust rationale that motivated each is preserved, the phrasing
that overreached is narrowed:

- **Plan §5.6 "The daemon authors a new clean commit"** (singular).
  The never-trust half of that paragraph stands verbatim; the
  one-commit framing becomes one clean commit per non-empty
  normalized first-parent state transition. What changed: the owner's #192 review question separated
  what can be normalized and validated (the history's semantic
  content, an ordered list of tree states and messages, agent
  controlled like all channel input but checkable by the existing
  machinery) from what can never be safely adopted at all (`.git`'s
  objects, hooks, and configuration). Both are untrusted; only the
  first has a validation path.
- **Hostile importer: "Commit messages come only from the
  daemon-supplied `Options.CommitMessage`, never the manifest"**
  (2026-07-16-1515-hostile-importer.md, Verified non-findings). That
  rule was correct for a contract with no message channel: any message
  in the v1 manifest would have been smuggled, unvalidated input. The
  revision adds a validated, screened message channel; the narrow rule
  becomes "never from the *snapshot manifest*, only from the validated
  history payload, and the daemon-supplied message remains the
  fallback and the no-history default".
- **Issue #193's own non-goal "importing workspace `.git` objects,
  hooks, configuration, or history in any form"**. The objects/hooks/
  configuration two-thirds stand permanently. "History in any form"
  is revised by the owner to "history as raw git state"; history as
  validated serialized data is exactly what this design admits.

## Export Design Draft

The handoff gains one optional sidecar, `history.json`, beside the
unchanged `manifest.json`, `blobs/`, and evidence payloads. The v1
snapshot manifest keeps its role and its version string: it remains
the authoritative head state, and a handoff without `history.json`
imports exactly as today. This makes the degenerate case absence, not
a special encoding, and gives the importer an independent head record
to cross-check the chain against.

- **Shape** (`freeside.export.history/v1`): an ordered list of
  commits, oldest first. The first entry carries the full entry list
  of that commit's tree in the existing `Entry` vocabulary; each
  subsequent entry carries a delta against the previous entry's
  materialized state (changed/added entries in the same vocabulary,
  plus explicit removals by canonical path). Every entry carries its
  message as a string field. Blobs stay in the single content-addressed
  `blobs/` store; a blob referenced by any state in the chain ships
  once. Delta encoding here is compression of untrusted data, not a
  trust decision: the importer materializes every state and derives
  all change sets itself, so the v1 full-snapshot rationale (never
  diff against untrusted parentage in the helper) is not reopened.
- **Walk**: first-parent from HEAD, terminating at the daemon-handed
  base SHA, which the exporter receives as an invocation input the
  same way the blob caps are pinned. Merge commits are represented by
  their first-parent delta (side-branch content appears in the merge
  entry's delta), matching the first-parent narrative reading of a
  branch. Commit-tree entries map into the existing `Entry`
  vocabulary under the existing normalization (gitlink to submodule,
  symlinks, unusual modes), so every non-regular kind hits the same
  publish-blocking rules per state that the head walk applies, with
  one history-specific extension: a submodule entry in a history
  state also carries its gitlink target OID, because the v1
  vocabulary records only presence, and without the OID an
  intermediate pointer move (changed, then restored before HEAD)
  would publish undetected; the importer compares each state's
  gitlink set and OIDs against the trusted base and raises the
  existing submodule-change publish-blocking finding on any
  difference, while rejecting gitlinks outright would break every
  repository that merely contains one. The
  reserved evidence subtree (`.freeside-evidence/`, the
  evidence-channel staging the workspace walk skips entirely) is
  elided from every serialized state under the same rule: it leaves
  the workspace only through the evidence channel, never inside
  published history. Commits whose delta is empty, including commits
  that only touched the reserved subtree, are dropped from the
  serialization; their messages do not cross.
- **When no usable history exists**, the helper omits `history.json`
  entirely and the export succeeds as a v1 export. The unusable
  cases: HEAD does not reach the base by first-parent; the walk trips
  a cap; the reader fails; the chain is empty, either because HEAD
  is the base or because every walked commit dropped as an empty
  delta after elision (a branch that only touched the reserved
  subtree); or
  the workspace is dirty, meaning the normalized HEAD tree fails the
  **same content-entry comparison the importer's equality check
  uses** (walk rules applied, reserved subtree elided, the
  snapshot's single inert workspace `git_dir` entry excluded; one
  shared definition so neither side can drift), which covers
  uncommitted edits, untracked files, and work never committed at
  all. The dirty check is
  exporter-side convenience, not enforcement: a helper that emits
  mismatched history anyway still dies at the importer's fail-closed
  equality check, where a mismatch is indistinguishable from
  tampering. Omission is safe because structure is presentation, not
  security: the head snapshot still imports under full validation. A
  *present but invalid* history is an integrity failure in the
  importer, never a silent downgrade (below).
- **Hardened reading, and the trusted-computing-base decision**: the
  exporter's no-execution invariant stands. The export package's
  structural test forbids `os/exec`, the exporter image ships no git
  binary, and this design does not change either: bundling git would
  put its config-driven execution surface and a large binary inside
  trusted compute to read hostile input. The reader is therefore
  **in-process Go object reading**: either an existing git-object
  library or an owned minimal loose-object-plus-pack reader, the
  final selection recorded as the reader unit's first decision, with
  the criteria fixed here: minimal parsing surface, bounded
  allocation under the raw-object caps below, no transitive
  execution, and the dependency surfaced as a design decision, not
  folded in silently. The git redirection mechanisms enumerated in
  earlier drafts (`refs/replace`, grafts, `objects/info/alternates`,
  the git-local environment: `GIT_DIR`, `GIT_OBJECT_DIRECTORY`,
  `GIT_COMMON_DIR`, `GIT_WORK_TREE` and kin) are thereby closed **by
  construction**, since the reader implements none of them; the list
  survives as the hazard enumeration the reader must never grow into.
  **Containment**: every read is confined to the workspace root. A
  `.git` whose resolution points outside it, a linked-worktree
  gitfile, `commondir` indirection, or alternates, is an unsupported
  layout that omits history explicitly, never followed (the head
  walk already records a linked-worktree `.git` file unexplored).
  This is the one place the design touches workspace `.git` at all,
  and its risk is bounded by the existing posture: the helper runs in
  the credential-free export context and its output is fully
  distrusted by the importer, so a compromised reader is equivalent to
  a forged export, which the hostile importer already assumes.
- **Invocation contract**: the daemon-handed base SHA and the
  history mode reach the helper **only through the daemon-controlled
  invocation surface**, never derived from workspace git state (a
  hostile workspace must not choose its own base or enablement).
  Today that surface has no such path: `HelperCommand` is a fixed
  argv, and the ward's exporter conformance requires exact command
  equality, so per-run parameters are a change to the ward's
  recorded-command contract, extending the recorded shape to a fixed
  argv plus typed per-run parameters (or a daemon-written input file
  on a trusted mount) that conformance still verifies. That plumbing
  touches `daemon/internal/ward` and belongs to the reader and
  invocation-contract unit (Contract Classification below), not to
  the schema/importer unit.
- **Caps**, flag-set and pinned by the exporter invocation like the
  existing ones: a total `history.json` byte cap in the same class
  as the manifest's existing byte cap, the bound every later
  "byte-bounded read" refers to, since under-cap counts with
  near-maximum paths could otherwise grow the file past the budget
  the manifest intake accepts; maximum commit count (default 100),
  charged against **every first-parent commit the walk visits,
  before elision**, so a branch of millions of empty or
  reserved-only commits trips the omit path instead of being chased
  to the end just to serialize nothing; maximum message
  bytes (default 8 KiB), raw object byte caps on the input side for
  **every object class the walk reads, commits, trees, and blobs
  alike**, with the two blob bounds kept distinct because a
  `blob_omitted` entry still requires the content digest, and a
  digest can only come from consuming the whole object: an object
  whose raw inflated size exceeds the hostile-input cap is never
  fully consumed, so it cannot be digested and the **entire history
  sidecar is omitted** (preferred mode falls back with the notice,
  required mode blocks), while an object under the raw cap that
  merely exceeds the storage or output budget is safely consumed,
  hashed, and recorded `blob_omitted` exactly as the v1 walk treats
  an over-budget workspace file,
  enforced under streaming reads, since the reader takes raw object
  bytes (a hostile commit with a huge header or signature block, or
  a crafted tree or blob object, must trip its cap before unbounded
  CPU, memory, or I/O is spent, never be read whole just to discover
  it is over cap); byte caps on the **ref and pack metadata** the
  resolution path must parse before any object (`.git/HEAD`, the one
  ref it names, `packed-refs`, pack indexes), bounded by the same
  rule since they are read first, with an over-cap or malformed
  metadata file treated as an unsupported layout that omits history
  (the reader resolves exactly HEAD and never enumerates refs, so
  this surface stays small by design); and the existing entry cap
  applied to
  the
  initial state plus the sum of delta entries **and removals**, so
  importer materialization work stays bounded by the same budget the
  v1 manifest already accepts. The exporter's cap-and-omit decision
  charges exactly the quantities the importer pre-bounds, one shared
  cap set by definition: any quantity capped on intake but uncharged
  at export would make an honest helper emit history the importer
  then fail-closes, where omission was the safe answer. Blob bytes charge through the **single
  existing per-file cap and aggregate blob budget**, head snapshot
  first and then chain states oldest-first, dedup free as today: a
  blob referenced only by intermediate states (touched then deleted
  before HEAD) is a real chain cost the head manifest never shows,
  so it competes for the same budget deterministically, the head's
  own `blob_omitted` decisions can never depend on history presence,
  and a budget-omitted blob that an intermediate state needs hits
  the existing needed-but-omitted publish-blocking rule.
- **Messages**: extracted as the **raw commit-object bytes**, never
  through transcoding plumbing: `git log`-class surfaces recode a
  message per the commit's `encoding` header before the caller sees
  it, so a non-UTF-8 raw message could arrive transcoded and pass a
  boundary its real bytes should fail; the reader takes the object's
  message bytes directly and a commit whose raw message is not UTF-8
  fails the rule below honestly. Then: exact bytes minus trailing
  newlines, required
  to contain at least one non-whitespace code point (whitespace-only
  is rejected, matching the fixture), valid UTF-8, and **no Unicode
  control characters except
  LF**, defined by category rather than enumerated ranges so nothing
  sits between them (NUL, the C0 range, DEL, the C1 range), plus no
  Unicode display/format controls (bidi embeddings, overrides, and
  isolates such as U+202E, zero-width and other invisible format
  characters), the exact code-point classes
  pinned at implementation as an enumerated corpus: every one of
  these can visually spoof or inject rendered commit text, and the
  boundary rejects them rather than trusting downstream renderers.

## Import Design Draft

The importer treats `history.json` as hostile input under the same
decode discipline the snapshot manifest earned the hard way. The
open itself uses the intake's regular-file rule (no symlink, FIFO,
or device, the `openRegular` class the manifest open already
enforces), and a present-but-irregular inode is fail-closed exactly
as the optional evidence payload treats it, never read as absent: a
bare open could hang the gauntlet or redirect the read outside the
handoff before any validation runs. Then: raw
`utf8.Valid` pre-check before any typed decode (the #180 lesson:
`json.Unmarshal` folds invalid UTF-8 to U+FFFD, so two distinct
hostile payloads can decode identically), streaming pre-bounds for
**every** capped count before building the typed value (commit
count and the aggregate entry count charging the first state's full
entry list plus every delta entry and removal, the same quantity the
export cap charges: the exporter-side
caps are untrusted, and a post-decode check would let an over-cap
payload force full typed allocation first, the resource-exhaustion
class the manifest intake's entry pre-count exists to avoid), with
the pre-bound scanner matching the typed decoder's own field
semantics exactly as the manifest intake does: keys counted
case-insensitively and duplicate keys included, since Go's decoder
folds `Commits` onto `commits` and takes later duplicates, so a
small canonical key beside a huge case-varied or duplicate one would
otherwise slip the bound,
per-message byte caps enforced within the byte-bounded read, strict
decoding that rejects unknown fields, and version-string check
first.

- **Chain validation**: materialize each state in order. History
  states use the v1 entry vocabulary plus the history-specific
  extensions (the submodule gitlink OID), so validation is
  two-layered by definition: a history-entry validator first checks
  the extension fields themselves (a well-formed OID required on
  every submodule entry, forbidden on every other kind), then
  projects each entry down to its exact v1 form, and the **projected
  state** passes the full v1 state validation (canonical paths,
  collision rules, kind/mode rules) unchanged; without the
  projection, the v1 submodule validator's no-extra-fields rule
  would reject every conforming history that contains a submodule.
  Run that validation on every materialized state, not just the
  head, including
  the intake's path byte/depth caps applied to every state entry and
  every delta/removal path **before** any collision or
  change-derivation work, since one over-deep path under the byte
  and entry caps can force superlinear work and history states can
  carry paths the head manifest never shows. Per-state validation is
  **incremental by construction**: the first state validates in
  full, and each subsequent state validates only its delta and
  removal paths against maintained collision/ancestor indexes
  updated in place, so total validation work scales with the same
  quantity the entry cap charges (first state plus deltas plus
  removals), never with commit count times tree size, which an
  under-cap payload could otherwise weaponize (100 commits over a
  large unchanged tree); require the
  final materialized state to equal the `manifest.json` snapshot
  exactly, comparing the **complete entry value**, every field of
  the entry vocabulary including kind-specific ones (symlink target,
  size), never an enumerated field subset a new field could silently
  escape,
  after excluding the snapshot's single inert workspace `git_dir`
  entry from the comparison: the workspace walk always records
  top-level `.git` as that inert entry, commit trees never contain
  one, and the head import already handles it outside the change
  set, so equality binds the content entries while history states
  still reject any `git_dir` of their own. Per-commit change
  derivation applies the head import's **bidirectional**
  reserved-path rule to every state, not just elision on the way
  out: base content tracked under the reserved subtree is retained
  as consumed/opaque exactly as the single-commit derivation retains
  it, so a materialized state that lacks a base-tracked
  `.freeside-evidence/` path never derives a deletion of it in any
  re-authored commit. Any
  state entry under the reserved evidence subtree or naming a
  `git_dir` entry is likewise an integrity failure: a conforming
  exporter never emits either, and admitting the former would launder
  evidence staging into published history. Any
  mismatch, malformed delta (removal of an absent path, entry that
  changes nothing, empty delta, or any duplicate path within a
  delta: each delta's changed-entry and removal path sets must each
  be unique and mutually disjoint, since an overlap makes the
  materialized state depend on apply order), or cap violation is an
  **integrity
  failure, fail closed**: a present-but-invalid history discards the
  import, never silently collapses to a single commit, because silent
  degradation would mask chain corruption and change what a reviewer
  sees without a signal.
- **Per-commit policy screening**: each commit's derived change
  records (adds, modifications, and deletions against the previous
  state) pass through the **same complete per-change gate the
  single-commit importer applies to the head change set**, by
  construction one shared code path rather than an enumerated
  subset, so no individual gate can be forgotten: allowlists and
  path-scope restrictions, control-plane classification including
  deletion of protected paths, size and non-regular-kind findings,
  and §5.4 secret scanning. Publish-blocking findings aggregate
  across the whole chain, because every commit in the chain
  publishes, and every history-derived finding (delta and message
  alike) carries its commit ordinal in the finding identity: the
  existing flat machinery rejects exact duplicates and has no commit
  dimension, so without the locator the same gate tripping in two
  commits would collapse or duplicate-fail, and a remediator could
  not tell which commit to amend. This is the load-bearing new rule: a hook file or a
  credential added in commit 2 and removed in commit 5, or a
  protected path deleted and later restored, is invisible in the
  head snapshot but permanent in pushed history, so head-only
  screening would be unsound the moment history crosses.
- **Message screening**: beyond the byte-level limits, every
  serialized message also passes the §5.4 best-effort secret scanner
  before publication, because re-authoring puts message text into
  published git history, a credential-leak surface the file-content
  scan never sees; a hit is publish-blocking and resolves by
  remediation like the directive findings below (secret detection is
  itself a §3.1 non-waivable class). Messages are further
  screened as automation-control surface through a **versioned,
  forge- and CI-specific ruleset carried in the trust-profile
  digest**, widen-only per repository: a static list cannot honestly
  promise to catch "any content a forge acts on" (this unit's own
  review history, close-keyword forms arriving over four rounds,
  demonstrates the enumeration drift), so the promise is scoped to
  the pinned ruleset version, an unknown forge or CI configuration
  falls back **deterministically, one result per mode**: under
  `serialized_preferred` and `serialized_required` alike, commit
  boundaries are preserved and **every message is daemon-authored**
  (a fixed per-commit form), since structure is what the modes
  request and messages are the part that cannot be screened;
  preferred additionally surfaces the fallback as a notice, and
  nothing silently drops to single-commit. Ruleset growth changes
  the digest like any other profile change: the ruleset identity is
  an explicit `message_ruleset` key in the §5.5 profile shape beside
  `history_import`, naming a reviewed base ruleset version, and
  repository-specific widen-only additions are carried inline in the
  profile itself, so both are digest-bound by construction. The initial
  GitHub ruleset: forge close
  keywords (GitHub's full documented keyword set, every conjugation
  of close/fix/resolve matched **case-insensitively** with the
  optional trailing colon, crossed
  with every reference form documented as actionable: `#N`, the
  cross-repository `owner/repo#N`, and the full issue URL, all of
  which act at merge, the cross-repository forms on other
  repositories), CI-skip
  directives (`[skip ci]`,
  `[ci skip]`, `[no ci]`, `[skip actions]`, `[actions skip]`, and
  `skip-checks:` trailers, which suppress the push/pull_request
  workflow runs the trust profile relies on), and
  identity/attestation trailers (`Signed-off-by`, `Co-authored-by`,
  `Reviewed-by` and kin, which forges and tools attribute), plus the
  daemon's own reserved provenance trailer key: an agent message
  carrying it would sit beside the daemon-appended label as a
  duplicate trailer and forge the trust marking, so the reserved key
  is screened like the other spoofable trailers and the appended
  daemon trailer stays unique by construction. All are
  publish-blocking findings routed through the existing finding
  machinery, exact `FindingKind` mapping and the pinned directive
  list chosen at implementation as an enumerated test corpus.
  Resolution is **remediation, never waiver**: a CI-skip directive
  conflicts with the non-waivable CI trust-profile gate (§3.1), so no
  approval can publish it, and close keywords and attribution
  trailers follow the same remediation path for consistency. The
  offending message is amended in a remediation round, or the owner
  explicitly re-imports with history dropped (a run-scoped override
  recorded on the item, per Policy Gating, never a profile change
  and not a silent fallback); the directive text itself never lands
  in published history. Amendment is always available because the
  message is agent-proposed presentation text, not content under
  test.
- **Re-authoring**: construction inherits the single-commit path's
  commit-blocker semantics per state: a history-derived finding of
  the construction-blocking class (non-regular change, invalid path
  entry, needed-but-omitted blob) **withholds chain construction
  entirely**, no commit of the chain is authored, the import outcome
  is the aggregated findings, and remediation is amending the
  history or an owner-recorded history drop, never re-authoring a
  tree the single-commit path would refuse to build. When no state
  carries a construction blocker, the daemon builds each commit's
  tree from the **previous commit's tree** (the trusted base for the
  first commit) plus that commit's validated delta, equivalently the
  trusted base plus the state's full derived change set, never the
  base plus a later commit's delta alone, which would drop every
  earlier commit's content and fail the exact-tree check; this is
  the existing scratch-index construction iterated, with the
  per-state
  exact-tree acceptance check; parents chain from the exact base;
  author and committer are the daemon's identity; each message is the
  validated agent text plus a fixed daemon-appended trailer marking it
  agent-proposed (exact trailer token chosen at implementation), so
  the text is labeled as a claim in the artifact itself, matching the
  evidence channel's labeled-claim convention. Absent or omitted
  history keeps today's behavior byte-for-byte: one commit,
  `Options.CommitMessage`.

## Green-per-Commit Stance

Intermediate commits are the agent's actual recorded states, not
synthetic partitions, but they are **unattested**: verification
recipes run at the candidate head only, evidence and publication
identities bind to the single candidate head exactly as §5.15 rule 2
and the head-mismatch machinery require today (intermediate commits
are ancestry; the tip is still the one candidate head), and nothing
asserts an intermediate commit builds or passes tests. The chain's
bisect and review value is therefore heuristic, in the same trust
class as any agent claim. Per-commit verification was considered and
declined for cost: it multiplies clean-room runs by chain length for
states no reviewer gates on.

## Policy Gating

History import is gated per repository through the same trust-profile
surface that governs publish (§5.5), because it widens the exported
surface from the final state to every intermediate state under a
best-effort secret scan. The gate is a **three-mode key in the §5.5
machine-readable trust profile** (`history_import: single_commit |
serialized_preferred | serialized_required`, in the documented
`repository_security` shape), because a two-state knob would let
"serialized" silently become "single commit" through the exporter's
legitimate omission cases (dirty workspace, caps, unsupported
layout), hiding exactly the state the owner asked to see:

- `single_commit`: today's behavior; history is neither requested
  nor re-authored.
- `serialized_preferred`: absence degrades to the single-commit
  import **and the daemon surfaces a requested-but-absent notice on
  the item**. The fact is daemon-derived and trustworthy (the
  profile requested history; the sidecar is absent); any
  helper-supplied omission reason rides along only as an untrusted
  diagnostic label.
- `serialized_required`: absence or a history-drop remediation is
  publish-blocking attention; nothing degrades silently.

Recommended default is conservative (`single_commit`) until
per-commit screening has been exercised against real repositories.
The key is carried in the profile's reviewed, digest-bound content
with the encoding version bumped by the `kind:contract` policy unit:
a policy flip must change the profile digest so stale approvals fail
closed, a knob outside the digest would be neither reviewable nor
actually conservative, and **existing stored profiles acquire the
`single_commit` default only through that version bump and owner
re-approval**, never by silent injection into an already-approved
digest. The history-drop remediation the message
screening names is a **run-scoped owner override recorded on the
item**, never a profile change; the mode selects whether the daemon
requests and re-authors history, while importer validation of
whatever is present stays unconditional.

## Contract Classification

Issue #193 asked whether the manifest shape is a shared contract
surface. Position, revised by the second external review: the
history **payload** is gauntlet-internal (`daemon/internal/export`,
`daemon/internal/importer`, `images/exporter`), but the **policy
surface is domain contract territory**: `history_import` and
`message_ruleset` land in the trust profile's canonical,
digest-versioned encoding (`daemon/internal/domain`), and the
preferred-mode notice plus the run-scoped history-drop override need
domain, API, and app representation (the existing
alternate-profile action is a profile change, exactly what the
run-scoped override must not be). Implementation therefore splits
into **three ordered units**:

1. the hostile-git reader and invocation contract (#221,
   `lane:gauntlet`; owns loose and packed object access for commits,
   trees, **and blobs**, delta reconstruction with depth/cycle/
   expansion limits, metadata bounds, containment, and the ward
   recorded-command plumbing);
2. a **`kind:contract` policy unit** (spine-owned, serialized): the
   two profile fields with an encoding-version bump, so every stored
   profile acquires the conservative `single_commit` default through
   re-approval and a stale digest fails closed, never a silent
   default injection; the run-scoped override and omission-notice
   vocabulary across domain, API, and app per the cross-component
   rule;
3. the history schema, importer validation, re-authoring, ruleset
   screening, and gating behavior (#212, `lane:gauntlet`), depending
   on both. The exporter image and importer move together within
   this unit; #221 pairs the exporter image with the ward instead.

## Importer Fixture List

Enumerated adversarially up front, in the existing table-driven
`name:` plus golden style; the valid multi-commit case doubles as the
validation-positive golden per convention.

- Chain shape: empty commit list; single-commit chain (degenerate,
  valid); commit count over cap; empty delta; delta entry that changes
  nothing; removal of an absent path; removal duplicated in one delta;
  path duplicated in one delta's changed entries; the same path in
  both a delta's changed and removal sets;
  delta touching an invalid or non-canonical path; over-deep or
  over-long path in a history state (path caps before collision
  work).
- Head binding: materialized head missing a snapshot entry; carrying
  an extra entry; digest mismatch; mode mismatch; kind mismatch;
  omission-bit mismatch; symlink-target mismatch; size mismatch;
  valid chain against a snapshot carrying the
  inert workspace `git_dir` entry (equality excludes it; positive
  case).
- Blobs: content referenced only by an intermediate state with blob
  absent; intermediate-only content with `blob_omitted` set.
- Per-state validity: case-fold collision introduced by one delta and
  resolved by a later one; control-plane path (hook, workflow) present
  only in an intermediate delta; secret-bearing blob present only in
  an intermediate delta; reserved evidence subtree path
  (`.freeside-evidence/x`) in a state; `git_dir` entry in a state;
  gitlink/symlink/unusual-mode entry appearing only in an
  intermediate state; protected path (workflow, AGENTS.md) deleted in
  one delta and restored before head; out-of-scope path added in one
  delta and removed before head; base-tracked reserved-subtree path
  retained untouched through every re-authored commit (positive, the
  bidirectional rule); gitlink OID differing from the base
  only in an intermediate state (publish-blocking); unchanged gitlink
  present across all states (positive); submodule entry in a history
  state missing its target OID (integrity failure).
- Gating modes: absence under `serialized_preferred` (single-commit
  import plus a requested-but-absent notice); absence under
  `serialized_required` (publish-blocking attention).
- Decode discipline: `history.json` present as a symlink; as a FIFO
  (fail-closed, not absent); over the total byte cap; raw invalid
  UTF-8 in `history.json`; unknown
  version string; unknown field under strict decode; entry count,
  commit count, and removal count each over their streaming
  pre-bound; single commit whose first full state alone exceeds the
  entry pre-bound; over-cap count split across a case-varied
  duplicate key (`commits` plus `Commits`); over-cap message rejected
  within the bounded read.
- Messages: empty; whitespace-only; NUL; invalid UTF-8; CR; ESC;
  over byte cap; DEL (U+007F); bidi override (U+202E); C1 control;
  zero-width format character; close keywords across the full
  conjugation-and-colon grid (`Fixes #1`, `Fix #1`, `Closed: #1`,
  `Resolved owner/other#1`, uppercase `CLOSES #1`);
  cross-repository close keyword (`Fixes owner/other#1`); full-URL
  close keyword; CI-skip
  directive
  (`[skip ci]` in the subject, `[no ci]` mid-body); `skip-checks:`
  trailer; spoofed `Signed-off-by` trailer; message carrying the
  daemon's reserved provenance trailer key; non-UTF-8 raw message
  behind an `encoding` header (raw-bytes extraction, rejected);
  secret-bearing message
  (token in the body); the same directive in two different commits
  yielding two findings distinguished by commit ordinal.

## Verification Findings

Checked against the existing implementation while designing: the v1
snapshot manifest, blob store, and importer state validation are
reused unchanged as the head record and the per-state validator; the
head-binding machinery (§5.15 rule 2, head-mismatch refusal,
deterministic publication identities) needs no change because the
re-authored chain still has exactly one candidate head. The
genuinely new obligations are the in-process hostile-git reader and
its invocation contract, per-delta policy screening, the
chain-to-snapshot equality check, and the profile policy contract
(the two digest-bound fields, the omission notice, and the
run-scoped override).

An owner-relayed external architectural review then ran a refute
pass against the near-final draft; its findings were verified
against the code and accepted, reshaping four areas. Confirmed: the
fixed exporter argv and the ward's exact-command conformance leave
no path for the per-run base SHA or history mode, so the invocation
contract became explicit reader-unit scope; the export package's
structural no-`os/exec` invariant and the git-less exporter image
contradicted an implied git-binary reader, so the in-process-reader
trusted-computing-base decision is now recorded; two-state gating
could silently degrade a requested serialized history, replaced by
the three-mode key with surfaced omission; and the "any content a
forge acts on" promise overreached a static list, replaced by the
versioned digest-bound ruleset with conservative fallback. Its
documentation-language corrections (untrusted-content phrasing, the
one-for-one replay overstatement) were also accepted; no finding
was rejected.

A second external round then refuted the amended draft's own
internal consistency, all findings verified and accepted: the
over-cap blob rule contradicted `blob_omitted`'s digest requirement
(a digest needs full consumption, so a raw-cap breach now omits the
whole sidecar while only a post-consumption budget breach records
`blob_omitted`); the "no spine shared package" classification was
wrong for the policy surface, since the trust profile is a
digest-versioned domain contract, producing the three-unit split and
the re-approval migration stance; blob-object reading had no owner
(#221 now owns loose/packed access and delta reconstruction, with a
dependency-graph no-execution audit beyond the direct-import test);
the ruleset fallback was made deterministic per mode with inline
digest-bound extensions; and the remaining "per validated agent
commit" phrasings were swept to "per non-empty normalized
first-parent state transition".

Revisit when: per-commit clean-room verification becomes affordable
enough to attest intermediate commits; a real repository's review
practice demands agent authorship identity rather than a labeled
message; the conservative default has blocked enough wanted history
imports to justify flipping it; or implementation finds the delta
encoding's validation cost exceeding what the fixture list prices in.

Follow-up: #221 (reader and invocation contract), #222
(trust-profile policy contract, `kind:contract`), #212 (schema,
importer, re-authoring, and ruleset/gating behavior; depends on
both).
