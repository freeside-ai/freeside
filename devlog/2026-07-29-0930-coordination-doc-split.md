---
run: manual
stage: coordination-doc-split
date: 2026-07-29
branch: docs/coordination-lazy-load
---

# Coordination Mechanics Split Out of AGENTS.md

Direct user assignment, no issue (session-contained; the contract rode
in the prompt and the PR). Declared paths: `AGENTS.md`, `docs/`,
`devlog/`. Not `kind:contract` (see below). The overlap check against
open PRs ran and is recorded in the PR; it produced no decision to keep
here.

## Context

`CLAUDE.md` imports `AGENTS.md`, so all 56,562 chars (about 14.1k est.
tokens) were resident in every session regardless of what the session
was doing. A `/doctor` pass surfaced it as the largest single resident
cost in the setup, well above the ~40,000-char threshold at which Claude
Code warns about a memory file.

## Decisions

- **Split by shape, not by section boundary.** The first proposal moved
  the whole Coordination section. A prompt-crafter review rejected that:
  the section contains gates that must fire *before* a session would
  know to open a protocol file at all, and moving them out with the
  mechanics is the half-ported-guard failure (cross-variant drift) in
  that skill's defect taxonomy. The gates stay in `AGENTS.md`; the lane
  glossary, claim-lease protocol, session-start queries, session end,
  and deferral escalation move to `docs/coordination.md`.
  Rejected alternative: a 12-line stub, which cannot carry the gates
  without dropping qualifiers.
- **Not `kind:contract`** (decider: maintainer). The Contract changes
  section enumerates what is contract-governed: shared packages,
  migrations, the StageDriver/ReviewSource/RunnerBackend interfaces, the
  API schema. `AGENTS.md` prose is not in that set.
  `2026-07-14-1619-comment-claim-lease` filed the claiming change as
  contract because it changed claiming *semantics* (mechanism, tie-break,
  expiry); this unit changes location only, and acceptance criteria 1 and
  2 exist to prove the zero semantic delta mechanically. Consequence: the
  unit did not serialize behind open contract units #305 and #274.
- **Daemon coding conventions stay in `AGENTS.md`** (rejected: moving
  them to `daemon/CLAUDE.md` + `daemon/AGENTS.md`).
  `2026-07-15-0043-wave0-convention-promotion` promoted that section
  *into* `AGENTS.md` at #27, explicitly overriding the source note's
  "point-of-use docs suffice" disposition, because "convergence should
  not depend on each lane happening to read `domain/doc.go`" with four
  daemon lanes fanning out. That condition is still live, and the section
  is already the compressed pointer layer at 1,797 chars, so the move
  would have overturned a standing owner decision for about 449 est.
  tokens. A supporting claim in the rejected proposal (that Codex CLI
  loads nested `AGENTS.md`) was never verified against primary docs; it
  is moot here but should be checked before any future nested-file plan.
- **The lane glossary moved rather than being deleted.** The plan to
  delete it rested on "it duplicates `docs/plan.md` §15", which is false:
  §15 carries no lane table and said "`AGENTS.md` owns the canonical lane
  glossary". Deleting would have lost the lane-to-owned-paths mapping
  that deferral routing depends on and left §15 pointing at a file
  pointing back at §15. §15's sentence is re-pointed instead.
- **Integration ordering and Work units stay resident.** Both are
  triggers rather than mechanics, and Integration ordering gates every
  handoff.
- **Gates are restated in full, not compressed.** The first draft of the
  contract-serialization gate dropped three qualifiers from the
  session-start query it replaced: the dependency-chain exclusion (Codex
  P1 on PR #379, which would have deadlocked the serialized contract
  queue by making chained units block each other), the rule that any
  contract unit touching your Affected interfaces/contracts blocks you,
  and the fact that "ignore until scheduled or claimed" scopes to
  `deferral`-labelled units only. All three were found and swept
  together. This is the cross-variant-drift class the prompt-crafter
  taxonomy names, arriving exactly where it predicted, and it is why the
  gates block reads long: a gate that compresses is a gate that changes
  meaning.
- **Session start held a gate, not only queries.** The first gate
  enumeration derived from Claiming, Deferral escalation, and Pickup, and
  treated Session start as pure query mechanics. Its step 4, "verify each
  dependency's PR is merged", is a gate: skipping it starts work against
  an unresolved dependency, and a direct session-contained assignment
  reaches it without claiming anything, so none of the pointer's original
  triggers fired for it (Codex P2 on PR #379). Added as a seventh gate,
  and the pointer now also triggers on work carrying dependencies. A
  sweep of the rest of the moved Session start and Session end found no
  other misclassified gate: steps 1 and 2 are reading procedure, step 2
  and the note rule are already resident in the managed finish-line and
  devlog blocks, and the tracking-issue tick is housekeeping.
- **A resident gate must be action-shaped and scope-explicit.** Three
  review rounds landed on the same class before the real invariant was
  named. `AGENTS.md`'s Coordination section had been doing double duty:
  stating each gate *and* supplying the discovery step that makes it
  actionable. Splitting them left gates phrased as conditions ("an open
  PR whose declared paths overlap yours means stop") that presume
  something else already told you to look, and scoped to claiming when
  they bind every work unit, direct assignments included (Codex P2,
  round 3). Patching the cited gate a third time would have kept the
  class alive, so all seven were audited against the rule instead: a
  gate says what to check and for which work, or it is inert without the
  procedure. Three failed and were rewritten (one claim per unit,
  declared-path overlap, contract serialization); the rule now heads the
  gates block so later edits keep the shape. This is the widen-the-
  boundary response to a class recurring past its first sweep, not a
  third one-off fix.
- **Widening a gate's scope obliges defining its terms.** Scoping the
  contract gate to "whatever shape the work takes" left it citing
  Affected interfaces/contracts, an issue-template field a direct
  no-issue assignment never fills, so the comparison had no input for
  exactly the work shape the widening was meant to cover (Codex P2,
  round 4, a consequence of the round-3 fix). The gate now names the
  underlying thing, the shared-package surfaces the work will change,
  with the issue field as one way of declaring them and derivation from
  declared scope as the other. General lesson for the gates block: a
  term borrowed from issue-backed procedure needs a direct-work referent
  before a gate may bind every work shape.

## Verification

Acceptance criteria, all mechanically checked before commit:

1. **Verbatim lift.** `main:AGENTS.md` lines 762-851 and 869-939, minus
   `###` headings and with blank runs normalized, diff empty against
   `docs/coordination.md` body. The one intentional exception is Contract
   changes (lines 853-867), retained in `AGENTS.md`.
2. **Gates resident.** Seven greps against post-change `AGENTS.md`, one
   per gate: all present. Six at first draft; the seventh is the
   dependency gate review added.
3. **Managed blocks untouched.** All 589 lines inside
   `agents-md:managed:*` markers byte-identical before and after, so the
   agent-setup sync has nothing to overwrite. (Coordination is unmanaged;
   managed ranges end at the `done` block.)
4. **Pointers resolve.** Every section `docs/coordination.md` cites that
   is not in that file (Branches, Commits, Work units, Decision notes,
   the finish line, Monorepo scope discipline, Stacked PRs) exists in
   `AGENTS.md`; zero dangling references remain in `AGENTS.md` to moved
   sections. `Work units`' citation of Pickup is re-pointed.
5. **Character accounting.** `AGENTS.md` 56,562 -> 50,801 (-5,761 chars,
   about 1,440 est. tokens per session); `docs/coordination.md` 9,512
   chars. The saving fell from an initial 1,742 est. tokens across three
   review rounds, each restoring a gate to residency: the trade working
   as designed, not drift, but a thinner margin than the plan claimed.

Measured while scoping, and worth recording: dependency between
Coordination and the rest of `AGENTS.md` runs one direction only. Zero
references from lines 1-721 point into the moved subsections; twelve
references run outward from them. That is what made the cut safe.

The saving does not clear the ~40,000-char memory warning; `AGENTS.md`
remains at 50,801. Clearing it would require moving content that earns
its residency (Build/test/run, the short behavior-shaping sections) or
content inside managed blocks, and neither is worth it.

No independent fresh-context review ran on the prompt-crafter
assessment; session policy did not permit an unrequested subagent. The
findings there are same-context self-review.

## Revisit When

- `docs/coordination.md` and the resident gates drift. Nothing compares
  them: the agent-setup comparator covers managed blocks only, and this
  file is neither managed nor generated. If a gate's wording changes in
  one place and not the other, the file header's "AGENTS.md is the
  authority" line is the tie-break, not a mechanism.
- A future session reports missing the claim protocol because it never
  opened the pointer. That would mean the gates are not doing the
  triggering job this split assumes, and the mechanics belong back
  inline.
