---
run: manual
stage: wave0-decomposition
date: 2026-07-13
branch: docs/wave0-decomposition
---

# Wave 0 decomposition

Spine-role planning session: Wave 0 (plan §11, Implementation
coordination) decomposed into five work-unit issues, #6 → #7 → #8 →
#9 → #11, filed from the work-unit template (`lane:spine`, milestone
1A; kinds below), strictly sequential, with the pinned tracking
issue #4's checklist populated. Preconditions verified
first: plan front-matter reads revision 7, AGENTS.md carries the
Coordination section and lane glossary, tracking issue #4 exists and
is pinned. No code written; the repo diff is this entry plus the
review-round session-start amendment in AGENTS.md (the
`kind:contract` bullet below).

The suggested five-unit split was adopted without structural
deviation: it is plan §11's own Wave 0 list (module, dual-platform
CI, domain package, schema and migrations, outbox, interfaces, fakes,
provisional API schema) grouped so each unit lands one coherent
contract layer. Decisions inside that frame:

- **The shared-package units (#7 #8 #9 #11) are `kind:contract`; the
  bootstrap unit (#6) is `kind:feature`** — revised on Codex review
  (P2 on this PR). Filed initially as `kind:feature` on the reasoning
  that Wave 0's strict serial dependency chain already provides the
  serialization the label buys; Codex pointed out the label is also
  the session-start **visibility** key: a contract claim made during
  Wave 0 blocks on "every other open contract unit", and
  `kind:feature` labels would hide the active Wave 0 contract work
  from that query, letting concurrent shared-package work be claimed.
  The dependency chain orders only these five issues, not unrelated
  future contract claims, so the units are relabeled to match
  AGENTS.md's plain rule (shared packages change only through
  `kind:contract` units). The relabel then hit the mutual-block rule
  literally (Codex P1, second round): session-start says a contract
  claim blocks on every other open contract unit, so with #8/#9/#11
  open a compliant session could never claim #7, deadlocking Wave 0,
  and a claim-order note on #4 cannot override protocol text. The
  session-start rule itself is therefore amended in this PR (called
  out in the body; it is the direct subject of the review round): a
  contract unit whose Dependencies chain includes the unit being
  claimed does not block the claim. A dependency-ordered chain keeps
  at most one unit claimable at a time, preserving the serialization
  intent while letting the backlog stay filed (the
  backlog-as-elaborator-fixtures value from the 2026-07-13-1208
  entry). The exemption filters the query before *both* blockers
  (Codex P2, third round): attached to the mutual-block clause alone,
  the unconditional touches-your-Contract overlap check would still
  block the chain head, since a downstream unit's Contract naturally
  references the surfaces its predecessor creates. Rejected: filing or relabeling each contract unit only when
  its predecessor merges (complies literally but destroys the standing
  backlog and adds stateful label churn). #6 touches no shared package
  and keeps `kind:feature`.
- **Acceptance sections are enumerable fixture/test lists**, per the
  template's own contract-with-the-future-elaborator rationale
  (2026-07-13-1208 entry): validation tables keyed to plan text (the
  ten §4 attention types, the five §5.7 capabilities, the §5.2
  pragmas), fake-scenario enumerations (crash-before/after, duplicate
  result, stale head), and golden-file coverage of every serialized
  shape (§11 Wave 0).
- **New package paths chosen: `daemon/internal/store` (unit 3) and
  `daemon/internal/exec` with `exec/fake` (unit 4).** The lane
  glossary's spine row lists domain, engine, migrations, api but no
  home for storage or the execution interfaces; engine was rejected
  (it is Wave 2 territory and would couple ward/gauntlet consumers to
  engine internals). Each unit's declared paths include the one-line
  glossary-row addition so the spine territory list stays true.
- **`migrations/` is not scaffolded by the bootstrap unit**: an empty
  directory before the mechanism exists records nothing; it lands
  with unit 3, which defines the migrations mechanism it holds.
- **Unit 1 carries the AGENTS.md build-table row** for `daemon/`, per
  the standing convention that commands land with the component's
  first PR.
- **Concurrent session noted, no conflict**: PR #10 (workspace-handoff
  spike, user session, scope `docs/` + `devlog/`) opened mid-filing,
  which is why the units are #6–#9 and #11. Its capability evidence
  informs unit 4's eventual implementation choices but changes no
  contract text: unit 4's RunnerBackend set stays the plan §5.7 named
  capabilities. Declared-path overlap with this PR is additive only
  (distinct devlog files).

Devlog queue swept before this docs PR: the scaffold-phase0
devlog-contracts item is marked resolved, and the licensing
ADR-candidate carries a same-day `-> re-deferred` marker (plan-v7
entry; PR #10 re-defers it again); nothing to drain in this scope.

## Verification

- Checked: preconditions (revision 7 in plan front-matter; AGENTS.md
  Coordination section and lane glossary present; issue #4 pinned via
  the GraphQL pinnedIssues query, milestone 1A, `lane:spine`).
- Checked: issues #6, #7, #8, #9, #11 created with all four template
  sections, `lane:spine`, milestone 1A; each Dependencies section
  names its predecessor and the merged-PR satisfaction rule. Labels
  re-read after the review-round relabel: #6 `kind:feature`; #7, #8,
  #9, #11 `kind:contract`.
- Checked: tracking issue #4 body re-read after edit; checklist
  references resolve to the five filed issues.
- Checked: session-start status queries ran before filing: no open
  contract issues; the only open PR (#10) declares `docs/` +
  `devlog/`, no overlap with the filed units' declared paths.
- Checked: the AGENTS.md session-start amendment sits outside every
  `agents-md:managed:*` block (the last managed block closes well
  before the Coordination section), and `git diff` on AGENTS.md shows
  only the single session-start hunk.
- Not run: no build/test/lint (no code changed; the diff is this
  entry and one AGENTS.md convention hunk).
