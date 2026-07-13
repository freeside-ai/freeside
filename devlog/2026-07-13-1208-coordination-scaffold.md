---
run: manual
stage: coordination-scaffold
date: 2026-07-13
branch: chore/coordination-scaffold
---

# Coordination scaffold

The coordination conventions for building Freeside with agents landed,
implementing plan §11's "Implementation coordination": the work-unit
issue form (`.github/ISSUE_TEMPLATE/work-unit.yml`) and AGENTS.md's
project-owned Coordination section (lane glossary, draft-PR claiming,
contract-change serialization, session start/end queries). Stacked on
the plan-v7 PR because the section cites §5.15 and §11 subsections
that exist only in revision 7.

- **The issue-body format is deliberately the future elaborator output
  contract**: Contract / Acceptance / Declared paths / Dependencies is
  what the 1B elaborator will emit, so the 1A backlog doubles as
  elaborator fixtures (plan §11). Changing the form's fields later is
  a contract change, not a cosmetic edit.
- **Lane names are search keys, never code vocabulary** (glossary
  rule): the attention type stays AttentionItem, not SignetItem.
- **Dropped the template's `labels: ["kind:feature"]` default** (Codex
  P2 on PR #5): a contract unit filed from the only committed template
  would start mislabeled, and the session-start `kind:contract` query
  would miss it. The Coordination section already requires explicit
  `lane:*`/`kind:*` labeling, so the default bought nothing. Chose
  removal over per-kind templates (premature: one template, four
  kinds, no evidence the split earns its upkeep).
- Repo-level state created outside the diff (via gh, per the approved
  plan): the ten `lane:*`/`kind:*` labels, milestones 1A and 1B, and
  pinned issue #4 "Wave 0 tracking" with an empty checklist, to be
  populated when the Wave 0 work unit is filed.
- **The runner lane follows the ward rename** (plan §13 item 15,
  decider: user): the glossary row and its owned path become
  ward / daemon/internal/ward, and the `lane:envelope` label was
  renamed `lane:ward` in place via `gh label edit`.
- **The contract-blocker query exempts the unit being claimed and
  serializes contract claims** (two Codex P2s on PR #5): the
  session-start "block on open `kind:contract` issues touching your
  Contract" would otherwise make every legitimate contract unit look
  blocked by itself, and, once exempted, would let two non-overlapping
  contract units run concurrently despite the exclusive/serialized
  rule; a contract-unit claim now blocks on every other open contract
  unit.
- **The glossary intro claims only what is true** (user, corrected
  scaffold): lane names are canonical here; subsystem-derived names
  also appear in plan §15, which now defines saddle and spine as
  coordination vocabulary (the earlier intro cited §15 for names it
  did not contain).
- **Owns-paths spelled repo-relative; cross-lane needs keep their
  kind** (two Codex P2s on PR #5): the table's `/importer`-style
  shorthand read as root paths and broke mechanical declared-path
  overlap checks, so sibling paths are spelled out; and session-end's
  "cross-lane needs become `kind:contract`" over-serialized ordinary
  lane work once contract claims serialize, so `kind:contract` is
  reserved for shared-contract changes.
- **A claim opens with an empty claim commit, dropped before merge**
  (two Codex P2s on PR #5): a fresh branch has no PRable diff, so the
  draft-PR claim was mechanically impossible before the first work
  commit, defeating the two-agent race it prevents. Chose
  `git commit --allow-empty` over issue-side claims: it keeps
  coordination state in git and preserves the draft-PR staleness rule
  (counted in work commits). Because merges are real merge commits
  (never squash), the no-op commit would land on main, so claim
  commits never merge: dropped in the next branch rewrite once work
  commits exist, the PR's close keyword carrying the claim from then
  on.
- **Contract PRs carry their generated consumers** (Codex P2 on
  PR #5): "merged before dependents adapt" read as permission to land
  a schema change that strands generated clients, contradicting the
  monorepo cross-component one-work-unit rule; the section now says
  required generated consumers and mechanical adapters move in the
  contract PR, and only downstream feature work waits.
- **The active claim is any open PR that explicitly claims the unit**
  (two Codex P2s on PR #5, second widening the first's fix): the
  draft-only wording stopped recognizing a claim the moment its PR was
  marked ready, inviting duplicate work; the first fix ("any open PR
  referencing the issue") then over-matched incidental `Refs #N`
  cross-references, falsely blocking unclaimed units. The rule now
  keys on an explicit claim (a `Claim #N` commit or a close keyword),
  never a bare cross-reference, with 48h staleness limited to
  claim-stage drafts.

## Verification

- Both artifacts diffed against the source scaffold's fenced blocks:
  the Coordination section verbatim except the ward lane row and the
  contract-query exemption (decisions above), the template verbatim
  except the
  dropped default-label line (the decision above); managed AGENTS.md
  blocks extracted before/after and diffed
  byte-identical (the Coordination section sits after the managed
  `done` block, outside every managed region).
- `work-unit.yml` parses as YAML (ruby YAML.load_file); GitHub's
  issue-form rendering only works from the default branch, so live
  rendering is deferred to post-merge (explicit gap in the PR body).
- Labels, milestones, and the pinned issue re-read via gh after
  creation (issue #4: pinned, `lane:spine`, milestone 1A).
- `grep envelope AGENTS.md` after the lane rename: zero matches;
  managed-block extraction re-diffed byte-identical after the row
  edit; issue #4's body checked for envelope references (none).
