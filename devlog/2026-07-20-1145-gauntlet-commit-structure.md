# Agent Commit Structure Crosses the Gauntlet as a Proposed Commit Plan

Gauntlet unit #193 (decide whether agent commit structure survives the
export), raised by the owner in PR #192 review. Basis: plan §5.6 (two
channels, hostile importer, clean-commit authoring), the export
manifest contract (2026-07-16-1237-export-manifest-contract.md), and
the hostile importer design (2026-07-16-1515-hostile-importer.md).
This is a design decision; implementation is a separate unit per the
issue's non-goals.

This note supersedes the serialized-history design that PR #213
carried to near-completion before the owner rejected it; #213 is
closed as superseded and its review threads (~50 findings plus two
external refute rounds, all accepted) remain that deliberation
record. The owner's Direction Settled comment on #193 (2026-07-20)
fixed the commit-plan direction. The design was then deliberately
**narrowed for V1** after three external review rounds and eight
automated rounds showed the accumulated policy surface, not the core
mechanism, generating the findings (Verification Findings); the
deferred pieces are listed under Deferred From V1 and the PR #224
threads are the finding-level record.

## Decision

**Adopted (owner decision): the agent proposes a commit plan over the
final validated change set, and the daemon re-authors one clean
commit per resolved non-empty plan group.** The agent works and
commits however it likes; before finishing, it may write a plan at
the reserved workspace path `.freeside-commit-plan.json`: an ordered
list of named groups, each a commit message plus the set of changed
paths it covers, optionally closed by one explicit remainder group
that collects every remaining derived change. The plan carries only
grouping, ordering, and messages, never content: every tree byte that
publishes comes from the trusted base or the validated final
snapshot, and the one genuinely new published surface is bounded,
screened, labeled commit-message text. No trusted component ever
reads workspace `.git`; the agent may well have derived its proposal
from its own `git log`, and that changes nothing, because the plan
crosses only as untrusted, screened claim data whatever its
provenance.

V1 ships exactly two modes: `single_commit` (the conservative
default: the plan is not decoded or honored) and `plan_preferred` (a
valid plan structures a non-empty import; only absence or an
enumerated agent-caused structural or non-secret screening rejection
falls back to today's single clean commit with a daemon-derived notice
naming the reason class). A zero-change import under `plan_preferred` takes
the deliberate empty-commit path after the tolerant scan and surfaces a
present plan as present-but-not-honored, so consuming the reserved channel is
never silent.
Under `plan_preferred`, a decoded secret anywhere in the plan's text is
publish-blocking until remediated per the §3.1 non-waivable class;
`single_commit` never decodes the plan and retains only the ward's literal
best-effort scan for those bytes;
lift, trusted-base preflight, and operational failures remain
blocking as the Pipeline Contract specifies. One built-in, versioned
GitHub message ruleset screens publishing messages. The full contract
is stated once, as the Pipeline Contract below; every other document
summarizes it.

What deliberately does not survive: agent commit SHAs, timestamps,
author/committer identity, and the agent's actual intermediate
states. Re-authoring makes new SHAs; identity and timestamps are
daemon-controlled; the agent's messages survive as validated,
labeled, untrusted text. Path granularity is the honest floor: a
group moves whole changed paths, never hunks, so intertwined concerns
collapse into one commit rather than being guessed apart, and the
daemon never repairs a plan (no reordering, no merging, no
splitting): repair would be inventing intent the agent did not state.

### Deferred From V1

Explicitly deferred by the owner until the basic feature has run
against real repositories, with revisit conditions at the end of
this note: `plan_required` (needs agent-side preflight support and a
demonstrated need); inline `message_ruleset` extensions and any
unknown-forge fallback (V1 validates the ruleset identifier against
the built-in registry at profile review, where an unknown identifier
fails closed); the run-scoped plan-drop override (V1's unified
fallback-with-notice removes its role); and an agent-facing
preflight for exact cover (the remainder group is the V1 answer).

## Rejected Alternatives

Four directions were worked through, the first two to full design
depth on PR #213:

- **Serialized real history, hardened in-process reader** (the #213
  design: the exporter walks `base..HEAD` out of workspace `.git`
  with an owned loose-object-plus-pack reader; the daemon re-authors
  one commit per non-empty first-parent transition). Rejected by the
  owner: it put a hostile-format git parser inside trusted compute,
  and since the agent can rewrite `.git` freely, all that hardening
  bought availability but zero provenance; the "history" read was
  never more trustworthy than any other agent claim. The long review
  tail (raw object caps, `refs/replace`, alternates, pack metadata
  bounds, containment) was the symptom, not incidental cost.
- **Isolated untrusted history generator** (same serialized payload,
  but ordinary git runs in a disposable credential-free container and
  the importer validates everything). A sounder boundary, rejected
  because it kept the design's other structural cost: the publication
  surface widens from the final state to every intermediate state, so
  content that existed only mid-chain (a credential added in commit 2
  and removed in commit 5) publishes forever under a best-effort
  scan, and the delta-decode and per-state content-screening
  machinery all remains.
- **Daemon-constructed commits from the final diff** (the daemon or
  an LLM stage partitions the change set itself). Rejected: the
  daemon would be guessing intent, and a plausible-but-wrong split is
  worse for review and bisect than one honest commit.
- **Declining entirely** (keep the single clean commit). Rejected:
  boundaries, ordering, and messages are real review and diagnostic
  signal, the repo's own history conventions exist because that
  signal matters, and the adopted carrier adds validated surface, not
  trust.

The adopted design is a revival of a direction the superseded note
had itself rejected (there called "a proposed commit-partition
manifest"), so the reversal is recorded explicitly. That rejection
made two arguments. First, the interpolated intermediate trees are
synthetic states the agent never built or tested, which undermines
bisect. True, but the serialized design conceded the same ground from
the other side: its intermediate commits were unattested (no
verification runs on them; see Green-per-Commit Stance), and an
unattested "real" state is, at the trust boundary, exactly as
heuristic as an unattested synthetic one; what review actually
consumes is grouping and narrative, which the plan preserves
honestly. Second, it invents a bespoke agent-facing format where git
is the interface every agent already speaks. Also true, and now
judged the cheaper cost by far: one small JSON file the agent writes
replaces a hostile-git reader (or an isolated generator), the
invocation-contract plumbing, serialized-history transport with its
delta decode, arbitrary intermediate content, and per-state content
screening; the honest cost that stays is constructing every
intermediate tree and commit, which any multi-commit import needs.
The agent still commits with git as it pleases; only the proposal
crosses.

## Prior Decisions Revised

Dispositions recomputed for the adopted design, all by the owner in
this unit; the superseded note's revisions of the second and third
items are themselves superseded:

- **Plan §5.6 "The daemon authors a new clean commit"** (singular).
  The never-trust half of that paragraph stands verbatim; the
  one-commit framing becomes one clean commit per resolved non-empty
  plan group, with the single commit remaining the default and the
  fallback.
- **Hostile importer: "Commit messages come only from the
  daemon-supplied `Options.CommitMessage`, never the manifest"**
  (2026-07-16-1515-hostile-importer.md, Verified non-findings). That
  rule was correct for a contract with no message channel. The
  revision adds one: messages may come from the validated, screened
  commit plan, never from the snapshot manifest, and the
  daemon-supplied message remains the fallback and the no-plan
  default.
- **Issue #193's non-goal "importing workspace `.git` objects,
  hooks, configuration, or history in any form"** now stands
  unrevised. The superseded design had narrowed "history in any
  form" to "history as raw git state"; the adopted design needs no
  narrowing, because the enforceable invariant holds as written for
  everything the non-goal governs: no trusted component reads or
  imports workspace git state, and the plan is a proposal over the
  daemon's own derived change set, untrusted claim data regardless
  of how the agent produced it (Decision).
- **Revision 5 decision 7 "agent commit history not preserved"** is
  upheld and clarified, not superseded: the agent's real history, as
  git state, still never crosses. What the candidate branch gains is
  agent-proposed structure over validated content.

## The Pipeline Contract

The normative statement of the V1 contract, in pipeline order; other
sections and documents summarize it and add rationale, never a
second copy of the rules.

| # | Stage (owner) | Behavior | On failure |
|---|---|---|---|
| 1 | Agent writes the plan (workspace) | Optional `.freeside-commit-plan.json` at the workspace top level, a sibling of `.freeside-evidence/`; version `freeside.commit-plan/v1`; ordered groups, each exactly one of a non-empty `paths` array or `remainder: true` (at most one remainder, last). | Absence is simply absence: the export proceeds as today. |
| 2 | Export helper lift (`daemon/internal/export`, `images/exporter`) | Opaque bytes, no parsing, no `.git` access, no per-run parameters (the fixed argv stands). Symlink-safe regular-file open at the reserved path; one total byte cap in the manifest cap's class. The workspace walk reserves the whole namespace, the exact path and every descendant, from the derived change set, using a path-component boundary rather than a raw string prefix (`.freeside-commit-plan.json.bak` is ordinary content); the plan crosses as a **declared member of the handoff output**, metadata in the repo-change channel. | Irregular inode at the reserved path, including a directory containing descendants: fail closed, like the evidence descriptor, never degrade to absence after walk exclusion. Over-cap: **fail closed at the lift, every mode**, the same class as the irregular inode: a legitimate plan never approaches the manifest-class cap, and converting over-cap to absence would silently drop a reserved-path file the stage-5 presence rule exists to surface. Absence therefore always means the file did not exist. The helper cannot detect a base-namespace collision: it has no base input and never reads `.git`; that check belongs to stage 4. |
| 3 | Ward check 7 (`daemon/internal/ward`) | Unchanged and unweakened, every mode: the stray rule admits the declared plan member (`verifyNoStrays` rejects everything unreferenced), and the §5.4 whole-output secret scan covers the plan like every other handoff byte. **The ward owns export scanning.** | A scan hit fails the handoff, every mode, before any import; remediation happens at the workspace. There is no second raw-plan secret machine in the importer. |
| 4 | Importer preflight, both modes (`daemon/internal/importer`) | After ward admission, before mode dispatch: the importer, which holds the trusted base, checks whether the base tracks the reserved plan path **or any descendant beneath it**, with the same path-component boundary as stage 2. Git can represent the reserved name as a tree even though the plan channel itself is one regular file, so an exact-path-only check is incomplete; a near-prefix such as `.freeside-commit-plan.json.bak` is not a collision. | A base that tracks the reserved path or a descendant: construction-blocking collision failure in **both** V1 modes, no candidate until the repository migrates that namespace (silent retention would discard real tracked content, and the walk's namespace exclusion means no constructed commit could represent it). |
| 5 | Importer, `single_commit` | The plan is not decoded and not honored; today's single-commit import runs unchanged, and the daemon surfaces presence as a notice (plan present, not honored by mode) through the same #222 notice vocabulary, no decoding involved. The reserved namespace is never repo content, by the same contract as the reserved evidence subtree, so a workspace file at the exact plan path is consumed as the plan channel in every mode; the notice is what keeps that from ever being silent. | n/a |
| 6 | Importer, `plan_preferred`: tolerant decoded-string secret scan | If a plan is present, before the zero-change bypass or strict schema/structural validation, a bounded syntactic JSON pass parses the whole document. On a successful syntactic parse it unescapes and secret-scans **every JSON string token, member names and values alike** (names are attacker-controlled bytes too), in full and bounded by the lift cap. It applies no schema semantics and selects no publishing groups, so the scan runs regardless of unknown fields, wrong shapes, eventual group emptiness, or change-set emptiness. Only `single_commit` opts out of plan processing entirely (its documented residual). | A secret hit is **publish-blocking until remediated** per the §3.1 non-waivable class; no later rejection may launder it into fallback. Invalid JSON yields no decoded tokens and does not block at this stage (the raw ward scan remains the best-effort layer); ordinary dispatch follows, with a zero-change import taking stage 7 and a non-empty import classifying the plan structurally at stage 8. An operational or internal scan failure fails the import. |
| 7 | Importer, `plan_preferred`, empty derived change set | The plan has nothing to group: after stage 6 has processed any present plan, the import takes today's deliberate empty-commit path (`commit_test.go`) with the plan ignored for grouping and message-screening purposes. If a plan is present, the daemon surfaces the same present-but-not-honored notice class as stage 5. The notice records consumption of the reserved channel without pretending the unused plan was structurally accepted. | n/a |
| 8 | Importer, `plan_preferred`, non-empty change set: decode and validate | An absent plan takes the absent-class fallback. A present plan follows the hostile-input decode discipline (2026-07-16-1515, #180): `utf8.Valid` pre-check, streaming pre-bounds matching decoder field semantics (case-folded, duplicate-counting), strict unknown-field rejection, version first. Then: path caps before set work; every path names a derived change; exact cover (each derived change in exactly one group, the remainder collecting the rest); named groups non-empty; group count cap (default 100); discriminator exact (`remainder: false`/`null`, `paths: null`/`[]`, both fields, or neither: invalid). Resolve the remainder and mark the groups that contain changes. Each interpolated intermediate tree passes the existing v1 structural rules (canonical paths, case-fold and ancestor collisions, kinds); a transiently colliding order is invalid, never repaired. | **Agent-caused invalidity in these enumerated classes only** takes the **unified fallback**, today's single clean commit plus a daemon-derived notice naming the reason class (absent or structural). Operational failures are not fallback material: I/O errors, failure to read trusted base state, resource exhaustion, and internal invariant violations **fail the import**, never converting into an apparently healthy candidate. Nothing from the plan publishes; nothing degrades silently. |
| 9 | Importer, `plan_preferred` publishing-message screening | For each **resolved non-empty group only**: per-message byte cap (default 8 KiB); byte-level rules (non-empty, non-whitespace, no control or format code points except LF, by category); the built-in versioned GitHub ruleset (Message Screening below). A remainder that resolves empty authors no commit, so its message skips these non-secret publication checks; under `plan_preferred`, all its string tokens were still secret-scanned at stage 6. | A byte-rule or ruleset (automation-control) failure takes the unified fallback with the notice carrying the screening class: the flagged message never publishes and the flag is never silent. An operational or internal screening failure fails the import. |
| 10 | Importer, construction | The existing scratch-index construction iterated: each commit's tree is the previous tree plus its group's full base-to-final changes; parents chain from the exact base; daemon author/committer identity; validated agent text gets the fixed agent-proposed trailer, daemon-authored text never does. **Construct all, swap once**: every tree and commit object is built without moving the import ref (today's `buildCommit` advances it per call and must not be iterated naively), the final tree is asserted equal to the validated head tree, then the ref updates in one compare-and-swap; failure at any group leaves the ref unmoved and orphaned objects unreferenced. | Construction failure leaves no candidate; the aggregated findings are the outcome. |
| 11 | Verification and publication (unchanged) | One candidate head; verification, evidence, and publication identities bind to it (§5.15 rule 2); the publisher creates the remote branch exactly once at that head (`daemon/internal/publish/publisher.go`), so intermediate commits are never exposed group-by-group. | Existing head-mismatch machinery. |

Honesty clauses. The ward's scanner matches literal byte patterns. Under
`plan_preferred`, a JSON-escaped token inside a file that is not
syntactically valid JSON is a residual the pipeline may miss; any present
plan that does parse has every string token, member names included, unescaped
and secret-scanned by the stage-6 tolerant pass, independent of schema,
structure, publishing-group selection, and change-set emptiness. Under
`single_commit`, stage 6 never runs because the plan is deliberately not
decoded, so escaped credentials remain a residual even in syntactically valid
JSON and only the ward's literal scan applies. The §5.4 promise stands as
written, a scan reduces risk and cannot prove absence, and no absolute
no-laundering claim is made (under `plan_preferred`, a token in any JSON
string that parses is caught by the tolerant pass and blocks until
remediated; publishing messages receive their non-secret checks at stage 9).
The
fallback path reuses today's single-commit code unchanged; no
end-to-end byte-identity is promised, since the export handoff itself
gains a declared member.

## Message Screening

Messages are an automation-control and credential-leak surface:
re-authoring puts agent text into published git history, which forges
act on and the file-content scan never sees. V1 screens against
**one built-in, versioned GitHub ruleset** (`github/1`), named by the
profile's `message_ruleset` key and validated against the built-in
registry when the profile is reviewed, where an unknown identifier
fails closed; there is no import-time fallback machinery and no
inline extension surface in V1 (Deferred From V1). The `github/1`
corpus: forge close keywords (the full documented keyword set, every
conjugation of close/fix/resolve, case-insensitive, optional trailing
colon, crossed with every actionable reference form: `#N`,
cross-repository `owner/repo#N`, and the full issue URL); CI-skip
directives (`[skip ci]`, `[ci skip]`, `[no ci]`, `[skip actions]`,
`[actions skip]`, `skip-checks:` trailers); identity and attestation
trailers (`Signed-off-by`, `Co-authored-by`, `Reviewed-by` and kin);
and the daemon's own reserved provenance trailer key, screened like
the other spoofable trailers so the daemon-appended trailer stays
unique by construction. The exact `FindingKind` mapping and directive
list are pinned at implementation as an enumerated test corpus, and
the pinned version is the whole promise: a static list cannot
honestly catch "any content a forge acts on" (the #213 review
history, close-keyword forms arriving over four rounds, demonstrated
the drift). Only resolved non-empty groups can publish messages, so
only those messages receive the byte rules and `github/1` screening;
an empty remainder's message is ignored because it authors no commit.
Under `plan_preferred`, every plan string, including that ignored message,
has already passed the stage-6 tolerant secret scan. At stage 9, a ruleset or
byte-rule hit follows the unified fallback: the message never publishes, and
the notice names the screening class. Under `plan_preferred`, a decoded-secret
hit at stage 6 does not: it is publish-blocking until remediated, because §5.4
secret
detection is a §3.1 non-waivable class and discarding a message does
not remediate a credential that already escaped the workspace.

## Green-per-Commit Stance

Intermediate commits are interpolations of validated content, not
states the agent built or tested, and they are **unattested**:
verification recipes run at the candidate head only, and nothing
asserts an intermediate commit builds or passes tests. Their bisect
and review value is heuristic, in the same trust class as any agent
claim, and one notch weaker than the superseded design's real-history
chain would have been; the owner accepted that trade because the
real-history chain was equally unattested, so the difference was
never mechanically checkable, and grouping plus narrative is the
signal review actually consumes. Per-group clean-room verification
was considered and declined for cost: it multiplies clean-room runs
by chain length for states no reviewer gates on.

## Policy Gating

Commit-plan import is gated per repository through the same
trust-profile surface that governs publish (§5.5). This unit
finalizes the V1 field vocabulary the contract unit (#222)
implements:

```yaml
commit_plan: single_commit | plan_preferred
message_ruleset: github/1
```

- `single_commit`: today's behavior; a present plan is not decoded
  or honored, and its presence surfaces as a daemon-derived notice
  so nothing at the reserved name disappears silently (Pipeline
  Contract stage 5).
- `plan_preferred`: a valid plan structures a non-empty import; an
  absent plan or one rejected for an enumerated agent-caused structural
  or non-secret screening reason takes the unified fallback with the
  daemon-derived notice (stages 8-9). A zero-change import takes stage
  7's empty-commit path after the stage-6 tolerant scan, with a
  present plan surfaced as present-but-not-honored. A decoded
  secret anywhere in the plan's text, message or not, is
  publish-blocking until remediated at stage 6, per §3.1. The fact the
  notice carries is trustworthy:
  the daemon observed absence or classified the enumerated rejection,
  and the reason class is never supplied by the workspace.

The importer preflight (stage 4) applies before either mode: a
trusted base that tracks the reserved plan path or any descendant is a
construction-blocking collision failure in both.

The recommended default is conservative (`single_commit`) until
message screening has been exercised against real repositories. Both
keys are carried in the profile's reviewed, digest-bound content with
the encoding version bumped by the `kind:contract` unit: a policy
flip must change the profile digest so stale approvals fail closed,
and **existing stored profiles acquire the `single_commit` default
only through that version bump and owner re-approval**, never by
silent injection into an already-approved digest. Adding a deferred
mode or field later (`plan_required`, ruleset extensions) is another
encoding-version bump through the same reviewed path.

## Contract Classification

Issue #193 asked whether the manifest shape is a shared contract
surface. Position, carried over from the #213 review: the plan
**payload** is gauntlet-internal (`daemon/internal/export`,
`daemon/internal/ward`, `daemon/internal/importer`,
`images/exporter`), but the **policy surface is domain contract
territory**: `commit_plan` and `message_ruleset` land in the trust
profile's canonical, digest-versioned encoding
(`daemon/internal/domain`), and the **complete plan-notice
vocabulary** needs domain, API, and app representation: all four
reason classes (absent, structural, screening,
present-but-not-honored), the last being what keeps reserved-channel
consumption under `single_commit` and the zero-change
`plan_preferred` path from being silent (Pipeline Contract stages 5
and 7). Implementation is **two ordered
units**:

1. the **`kind:contract` policy unit** (#222, spine-owned,
   serialized): the two V1 profile fields with the encoding-version
   bump and re-approval migration, plus the notice vocabulary across
   domain, API, and app per the cross-component rule;
2. the plan schema, reserved-path lift, the ward's stray-rule
   admission and scan-order fixtures, importer validation,
   re-authoring, and ruleset screening and gating behavior (#212,
   `lane:gauntlet`), depending on it. The exporter image, the ward
   handoff verification, and the importer move together within this
   unit (`daemon/internal/export`, `daemon/internal/importer`,
   `daemon/internal/ward`, `images/exporter`).

The superseded design's third unit, the hostile-git reader and
exporter invocation contract (#221), is closed as superseded: with
no `.git` reading there is no reader, and with a fixed helper argv
there is no ward recorded-command change.

## Importer Fixture List

Enumerated adversarially up front, in the existing table-driven
`name:` plus golden style; this list is the test spec for #212's
acceptance. The valid multi-group case doubles as the
validation-positive golden per convention.

- Plan shape: empty group list; named group with an empty path set;
  path not in the derived change set; path duplicated within one
  group; path duplicated across two groups; derived change covered
  by no group with no remainder (incomplete cover); group count over
  cap; single-group plan (degenerate, valid); whole-change-set-in-
  one-group equivalence against the no-plan import (same tree,
  different message source); a group whose only path is a deletion
  (valid); plan naming the reserved plan path or a descendant beneath it
  (invalid), contrasted with a changed near-prefix path such as
  `.freeside-commit-plan.json.bak` (ordinary content, valid); a
  `.freeside-evidence/` path or a `git_dir` entry (invalid);
  over-deep or over-long path (path caps before set work).
- Remainder: named groups plus a remainder collecting the leftovers
  (positive); a remainder that collects nothing (positive, authors
  no commit, and its empty/control-character/over-cap/ruleset-hit
  message does not force fallback, while under `plan_preferred` an escaped
  secret in that message still blocks at stage 6); two remainder groups
  (invalid); a
  remainder that is not last (invalid); a remainder-only plan
  (positive).
- Discriminator: a group carrying both `paths` and `remainder`;
  `remainder: false`; `remainder: null`; `paths: null`; `paths: []`;
  neither field (each invalid).
- Ordering and construction: case-only rename split across groups in
  the colliding order (invalid) and the non-colliding order (valid);
  file-to-directory change split both ways; final-tree assertion; a
  fault-injection fixture failing construction after an earlier
  group's commit object exists (import ref never moved, no partial
  candidate); an operational fault injected during decode or
  validation (an I/O or trusted-base read error fails the import,
  never the fallback); a trusted base already tracking either the exact
  reserved plan path or a descendant beneath it, each tested under
  **both** modes (importer-preflight construction-blocking collision
  failure, no candidate); a trusted base tracking
  `.freeside-commit-plan.json.bak` under both modes (no collision).
- Ward handoff: a workspace directory at the exact reserved name containing a
  child (irregular plan node, export fails closed rather than reporting plan
  absence after the walk excludes the namespace); the near-prefix workspace
  file `.freeside-commit-plan.json.bak` remains in the derived change set; the
  lifted plan admitted by `verifyNoStrays` as a
  declared member and transported downstream (positive); a stray
  file beside the declared members still rejected; a literal token
  in the plan failing the handoff at check 7 under both modes.
- Decode discipline: plan present as a symlink; as a FIFO
  (fail-closed at the lift, not absent); over the total byte cap
  (fails the export, every mode, never treated as absent); raw
  invalid UTF-8; unknown version string;
  unknown field under strict decode; group and path counts over
  their streaming pre-bounds; over-cap count split across a
  case-varied duplicate key (`groups` plus `Groups`).
- Messages, for resolved non-empty groups: empty; whitespace-only;
  NUL; CR; ESC; DEL (U+007F); bidi
  override (U+202E); C1 control; zero-width format character; over
  byte cap; close keywords across the full conjugation-and-colon
  grid (`Fixes #1`, `Fix #1`, `Closed: #1`, uppercase `CLOSES #1`);
  cross-repository close keyword (`Resolved owner/other#1`);
  full-URL close keyword; CI-skip directive (`[skip ci]` in the
  subject, `[no ci]` mid-body); `skip-checks:` trailer; spoofed
  `Signed-off-by` trailer; message carrying the daemon's reserved
  provenance trailer key; under `plan_preferred`, a JSON-escaped secret in a
  message that decodes (publish-blocking until remediated, never the fallback);
  the same directive in two groups
  yielding two findings distinguished by group ordinal; the
  agent-proposed trailer present on agent text and absent on the
  daemon-authored fallback message (positive pair).
- Modes and fallback: absent plan under `single_commit` (today's
  import, unchanged path); malformed present plan under
  `single_commit` (not decoded, no finding, presence notice); a new
  workspace file at the reserved path over a clean base (consumed as
  the plan channel in every mode, never silent content omission:
  presence notice under `single_commit`, ordinary plan handling
  under `plan_preferred`); absent plan on a **non-empty** import under
  `plan_preferred` (fallback plus notice, reason class absent); absent plan on
  a zero-change import under `plan_preferred` (today's empty commit, no plan
  notice because no reserved channel was consumed); one
  structural-failure and one non-secret screening-failure fixture
  under
  `plan_preferred` (fallback plus notice with the matching reason
  class); a decoded secret under `plan_preferred` (publish-blocking,
  no fallback authored until remediation); under `plan_preferred`, an escaped
  secret inside
  a parsed but structurally invalid plan, e.g. one with an unknown
  path or an unknown field (the tolerant scan still runs and
  dominates: publish-blocking,
  not the structural fallback); under `plan_preferred`, an escaped credential
  positioned
  past the per-message cap boundary in an over-cap publishing message
  (scanned through the lift cap, publish-blocking, not the screening
  fallback); under `plan_preferred`, an escaped credential in an unknown
  member name
  (tolerant scan covers keys: publish-blocking); empty derived change set
  with a valid present plan under `plan_preferred` (today's empty commit,
  plan ignored after the tolerant scan, and a present-but-not-honored
  notice); a syntactically valid, non-secret, structurally invalid plan on a
  zero-change run under `plan_preferred` (stage 8 is skipped: empty commit plus
  present-but-not-honored, not structural fallback); under `plan_preferred`,
  syntactically valid, non-secret zero-change plans with an over-cap,
  byte-rule-invalid, or
  ruleset-hit message (stage 9 is skipped: the same empty commit and notice,
  not screening fallback); an escaped credential in a present plan on a
  zero-change run under `plan_preferred` (publish-blocking, no
  empty-commit candidate until remediation); a malformed, non-secret
  plan on a zero-change run under `plan_preferred` (stage 6 yields no decoded
  tokens, then
  stage 7 takes today's empty-commit path with the
  present-but-not-honored notice, not a structural-fallback notice).

## Verification Findings

Checked against the existing implementation while designing: the v1
snapshot manifest, blob store, and importer state validation are
reused unchanged (the interpolated-tree checks are the existing
per-state validators over trees built from already-validated
entries); the single-commit path (`daemon/internal/importer/
commit.go`, `options.go`) remains the fallback; the head-binding
machinery (§5.15 rule 2, head-mismatch refusal, deterministic
publication identities) needs no change because the re-authored
chain still has exactly one candidate head; the reserved-namespace lift
reuses the evidence channel's existing machinery class (symlink-safe
resolution, size caps, fail-closed irregular inodes, walk exclusion:
`daemon/internal/export/evidence_emit.go`, `walk.go`); the ward's
check 7 already owns whole-output verification and scanning
(`daemon/internal/ward/export_verify.go`), which the design joins
rather than duplicates; and the exporter's fixed `HelperCommand`
argv and the ward's exact-command conformance stand untouched
because the helper takes no per-run input.

The deliberation that produced this design ran on PR #213: the
serialized-history design survived ~50 accepted review findings and
two external architectural refute rounds before the owner rejected
its premise: a hostile-format parser inside trusted compute whose
hardening bought availability but no provenance, since the agent can
rewrite `.git` freely. The Direction Settled comment on #193 records
the rejection rationale and the adopted direction; #213's threads
remain the finding-level record. What survives of that work
unchanged in substance: the GitHub message-screening corpus, the
digest-bound gating with re-approval migration, the contract
classification, the decode discipline, and the green-per-commit
stance.

The commit-plan draft then went through three owner-relayed external
review rounds and repeated automated-reviewer rounds on PR #224, every
finding accepted; the PR threads and body carry the per-finding
dispositions. The externally reshaped areas, in order: the explicit
per-mode outcome rules and atomic construct-all/swap-once ref update
with its fault-injection fixture; the wire schema and the
agent-assigned remainder group; the construction-blocking reserved-
namespace collision; the zero-change bypass preserving the importer's
deliberate empty commit (later surfaced when it consumes a present plan);
the adversarially pinned group
discriminator; and the ward handoff gate the draft had omitted (the
stray rule and whole-output scan-before-release), which added
`daemon/internal/ward` to #212's scope and refuted an absolute
no-laundering claim for raw-byte scanning (the scanner is literal;
JSON escapes evade it).

That third round's tail prompted the owner's **structural
verdict**: the repeated findings were tracking self-inflicted policy
surface, not an unsound mechanism. The core kept: hostile-input
plan, ward-owned export scanning, exact cover with the remainder
group, structural intermediate checks, bounded message validation,
construct-all/swap-once. The surface cut from V1: `plan_required`,
the three-mode interactions, the run-scoped plan-drop override, the
block-versus-degrade split for byte-rule and automation-control
screening (unified into fallback-with-notice), ruleset extensions
and import-time fallback (the
identifier validates against the built-in registry at profile
review), the importer's duplicate raw-byte secret scan, and every
"unobservable"/"byte-identical" absolute. The contract is stated
once as the Pipeline Contract table; summaries elsewhere link to it.

A fourth relayed round against the narrowed draft (reviewed head
`96155cb`) accepted the simplification and held three corrections,
all accepted. The unification had over-reached on one §3.1
non-waivable class: under `plan_preferred`, a decoded secret hit in the
proposed plan must
block until remediated, never take the fallback, because discarding
plan text does not remediate a credential that escaped the workspace;
that is the one screening distinction V1 keeps. The then-stage-7
"any failure" fallback was scoped to the enumerated
agent-caused invalidity classes, with operational failures (I/O,
trusted-base reads, resources, internal invariants) failing the
import rather than producing a healthy-looking candidate, pinned by
a fault-injection fixture. The base-path collision check moved to
its real owner: the export helper has no base input, so the importer
preflight (stage 4, before mode dispatch) detects it and blocks both
modes.

Later automated rounds generalized that first message-secret case under
`plan_preferred` to every decoded JSON string token (member names and values),
made the tolerant scan an explicit pre-validation stage so schema rejection and
zero-change dispatch cannot skip it, and limited non-secret byte and
ruleset checks to resolved non-empty groups whose messages can actually
publish. An empty remainder's message never authors a commit and cannot
force non-secret fallback, but under `plan_preferred` its strings remain
inside the plan-wide secret scan. This is why the final table separates
stage 6 from stage 9.

A later automated round caught the clean-base twin of the collision
case: a workspace file newly added at the reserved path would be
consumed as the plan channel while `single_commit` promised no
finding, silently dropping what a user may have meant as content.
Resolved by contract plus surfacing: the reserved name is never repo
content (the evidence-subtree precedent), and presence now surfaces
as a daemon-derived notice in `single_commit`, deliberately
reversing this note's earlier presence-record drop because the #222
notice vocabulary now supplies the defined carrier that objection
was about.

The next automated round found the two remaining edges of that same
reserved-channel class. First, `plan_preferred` also consumed a present plan
on a zero-change import without saying so; stage 7 now emits the same
present-but-not-honored notice while preserving the deliberate empty commit.
Second, Git may track the reserved name as a tree: the both-mode trusted-base
preflight now blocks the exact path **and every descendant**, matched by the
workspace walk's reservation of that whole namespace and by fixtures for
both collision shapes.

Revisit when: real usage demands `plan_required` and agents have a
preflight or equivalent; a real repository needs a ruleset extension
or a non-GitHub forge ruleset; review practice demands hunk-level
granularity; per-group clean-room verification becomes affordable
enough to attest intermediate commits; or the conservative default
has blocked enough wanted plans to justify flipping it.

Follow-up: #222 (trust-profile policy contract, `kind:contract`),
#212 (schema, lift, ward admission, importer validation,
re-authoring, and ruleset/gating behavior; depends on #222). #221 is
closed as superseded by this design.
