---
name: review-wave
description: Generate the fresh-context adversarial review prompt for a merged wave of the docs/plan.md §11 coordination table, or for an ad hoc tracker's merged units. Derive the tracker's merged units, their PRs, and the documents that state their guarantees, then emit one self-contained, vendor-neutral prompt for the human to hand to an external reviewing agent (typically Codex). Use when the user asks to "review wave N", "generate the wave N review prompt", "run the wave exit review", "review tracker #N", or "run another review round on wave N". Not for executing the review itself, not for reviewing an individual PR, and not for the §7 production review stage (that is daemon runtime, not development process).
---

# Exit Review Prompt

This session derives and emits a reviewer prompt, nothing else. It only
reads the forge and the repository: it writes no code, files no issues,
posts no comments, and never performs the review itself.

## Inputs

`/review-wave <N>` reviews wave N of the §11 table; `/review-wave #<N>`
reviews the tracker with that issue number, wave or ad hoc. A bare
number is a wave number. If the argument is missing, or no such tracker
exists, stop and ask. A wave tracker carries the `tracker` label and a
milestone and is titled `Wave N: <Name>`; an ad hoc tracker carries the
`tracker` label alone. The tracker may be open or closed; a review of a
closed tracker posts comments and never reopens it or edits its body.

The same procedure serves both kinds. Where a step below reads the §11
row, an ad hoc tracker's intro (its Goal, Plan, and Safety lines, as
far as it carries them) stands in for it.

## Why a Prompt, Not a Review

The wave exit review (plan §11) is worth running only if the reviewer is
fresh: an agent given only the repository and its documents, never the
design history of the sessions that built the work. The implementing
sessions are typically Claude, so the executing reviewer is typically a
different vendor for independence, and the human invokes it. This skill
therefore ends at a self-contained prompt the human can paste into any
agent; the session that generated the prompt must never also execute
it, and the prompt must never assume the reviewer can see this session.

This is the inverse of the plan-wave decision
(`devlog/2026-08-02-2007-plan-wave-skill.md` rejected a
prompt-generator because the planning session acts on its own
derivation): here the derivation and the action are different vendors'
sessions by design, and the prompt is consumed once, immediately, so it
cannot go stale the way a stored plan would.

The plan requires the review only at a wave's exit. Most merged work now
sits on ad hoc trackers, so the owner may ask for the same review on one
of those (`devlog/2026-11-09-1817-review-rounds-and-trackers.md`).

## Rounds

A tracker can be reviewed more than once: a review finds defects, the
fixes merge, and the owner asks for another round. Each round posts its
own findings summary and remediation proposal on the tracker, and both
comments open with a marker line:

- `<!-- freeside-<tracker>-exit-review:round-<K> -->` on the findings
  summary;
- `<!-- freeside-<tracker>-exit-review-plan:round-<K> -->` on the
  remediation proposal.

`<tracker>` is the tracker's issue number, as in the exit-run markers.

This run's round is one more than the highest round marker on the
tracker. A findings summary posted before the markers existed (a comment
headed `# Wave N Exit Review`) counts as round 1.

Round 1 covers every merged unit. A later round covers what the earlier
rounds left unsettled, unless the owner asks for a full review again:

- the PRs that closed earlier rounds' findings;
- units added to the tracker since the last audited commit;
- partitions an earlier coverage statement reported as sampled or not
  read.

A later round's prompt names every earlier finding and every earlier
risk disposition, so the reviewer raises one again only when the code
behind it changed or new evidence contradicts it.

## Derive the Tracker Before Prompting

Tracker content is never hard-coded here; derive it at run time. Read,
in order:

1. The tracking issue, end to end: the intro, Status (diagram and
   **Startable now**), Units, Exit, the Notes block with its surfaced
   forks and planning rationale, and every comment after the body,
   including earlier review rounds, exit-run records, and owner
   decisions. `docs/tracker-format.md` and `docs/coordination.md`
   (Tracking Issues) describe the sections.
2. The tracker's merged PRs, mapped from each PR's closing references,
   never from the tracker's checklist alone: a checklist can be stale,
   and a PR that closed an issue the tracker body recorded as deferred
   is still merged code in scope. Sweep merged PRs over the tracker's
   date range for closing references to its units and to any issue a
   unit PR pulled in. In a later round, also map the merged PRs that
   closed earlier rounds' findings; steps 6 and 7 cover them too.
3. Each unit issue end to end. The issue is the work contract, so its
   Acceptance fixture/test list and Non-goals are spec the hunt targets
   must carry even where docs/plan.md never repeats them. Then read
   where the units' guarantees are written:
   - the plan sections the unit issues cite, in full, plus §11's row
     and exit text for a wave;
   - the package `doc.go` comments and component READMEs for the
     packages the PRs touched, because the plan holds commitments and
     mechanism lives at its point of use (AGENTS.md, What Belongs in
     the Plan).

   The speculation test governs claimed requirements only: a
   requirement that no unit contract, cited section, or point-of-use
   document backs is speculation, but an in-scope correctness,
   security, or edge-case defect is a finding whether or not any
   contract names it.
4. `docs/history/decisions.md` entries and `docs/decisions/` records
   binding the work, the devlog notes the unit issues and merged PRs
   link, plus a devlog search by the affected paths, topics, and
   contract names for binding notes nothing linked (a settled owner
   choice can live in a note never promoted to the history file), and
   every open owner fork the tracker or the cited sections carry
   deliberately unresolved.
5. What is already tracked, for the prompt's dedup paragraph, so a
   known gap is not re-filed as a finding: the unscheduled queue (open,
   `deferral` label, no milestone) filtered to the tracker's surfaces;
   where the tracker parked its rescoped remainders; the exit-run
   findings recorded on the tracker; and every earlier round's findings
   with their current state.
6. Each merged PR's touched paths, so hunt targets name real packages
   and files, not guessed ones. Mark which touched files are on the
   Tier A Paths list (AGENTS.md, Assurance Tiers); `_test.go` and
   `testdata/` files don't count by path.
7. How the work landed. Units start without comparing scope and
   conflicts resolve at landing (AGENTS.md, Coordination), so list:
   - each package or file that more than one unit PR changed where
     neither unit lists the other as `starts-after`;
   - each unit PR that carries a rebase record: the author's
     `git range-diff` bullet in the PR's Verification section, or the
     lander's range-diff comment (`docs/coordination.md`, Reading a
     Rebase).
8. The audited tree: fetch the default branch and resolve its exact
   tip SHA. The review binds to that one commit, so the prompt embeds
   it rather than telling the reviewer to resolve a tip of their own.
9. For a wave only, the unreviewed remainder: trackers with the
   `tracker` label and no milestone that closed since the previous
   wave tracker closed and carry no findings summary. These go in the
   report (Emit and Stop), never in the prompt.

## Preconditions (Stop on Failure)

- Every unit in the tracker's Units list is closed by a merged PR.
  Stragglers mean the work isn't ready for review; list them and stop.
  Entries under `### Owner-run` aren't units: report an open one as an
  unresolved maintainer item and continue.

A wave's declared close condition need not be fully satisfied (a close
condition can depend on post-review events, e.g. a first production
run); the review gates the next wave's planning, not the other way
around. Flag any unmet close condition in the report, not as a stop.

An earlier round is not a stop. Report the rounds found, the commit each
audited, and how many of their findings are still open, then generate
the next round.

## Compose the Prompt

Fixed skeleton, derived content. The prompt must be self-contained:
the reviewer has no other context, so every issue number, PR number,
section cite, file path, and dedup pointer it needs must appear in the
prompt text itself.

1. **Fresh-context preamble.** "You have this repository and its
   documents (docs/plan.md, AGENTS.md, docs/coordination.md, the devlog
   directory, docs/history/decisions.md, docs/decisions/, docs/spikes/,
   and the package doc.go comments and component READMEs). You have no
   other context about the project's history." Then the assignment:
   adversarially review the tracker's merged code (tracking issue
   number, round number and its scope, unit issue numbers, PR numbers,
   dominant plan sections) against the documents the derivation found,
   at the exact `main` SHA the derivation resolved, embedded here in
   the prompt. Then how to work at that commit:
   - Audit a detached checkout of that SHA in its own worktree, and
     record the SHA in the findings summary. `main` takes many merges a
     day, so a newer tip is expected and is no reason to stop.
   - Before filing a finding, confirm the defect is still present at
     the current `main` tip. One a later merge already fixed is listed
     in the summary as fixed since the audited commit, with the PR,
     and is not filed.
   - Run checks the way AGENTS.md (Build, Test, Run) says:
     `bash scripts/check.sh <component> [step...]`, and selected daemon
     packages with `bash scripts/check.sh daemon test --
     ./internal/<package>/...`, never a bare `go test`.
2. **Hunt targets.** Partition by guarantee. Each bullet in the
   tracker's Exit section that names evidence units is one partition
   holding those units; a unit that is evidence for several bullets is
   hunted once and named in the others. Group the remaining units by
   the component or package cluster their PRs touched. In a later
   round, the PRs that closed earlier findings form partitions of
   their own, each naming the finding it claims to fix. For each
   partition, name the guarantees the derived documents state, phrased
   as implemented, not as tested: the enforcement paths, the failure
   and edge cases, the negative probes, and whether the tests pin the
   guarantees or pass around them. Every partition also carries a
   standing correctness, security, and edge-case hunt over its merged
   code, not only conformance to the written contracts.

   Set depth by Assurance Tier (AGENTS.md), and name each partition's
   Tier A files in the prompt. Tier A files are read in full. Tier B
   code is reviewed for behavior: validated on write, tested, and loud
   on a bad row. Tier C code is reviewed for best effort: a bad row is
   skipped and logged, not a failed listing.

   Layer on, where the derivation shows they apply:
   - the Tier A lens for any partition that touches a Tier A path or
     does what a Tier A category names. State the threat model: the
     daemon's own database, artifact store, and binary are trusted;
     agent output and workspaces, GitHub and provider API responses,
     client requests, CI results, research fetches, and
     repository-supplied config are not. Instruct the reviewer to
     hunt:
     - a stored authority bit (a publish eligibility, a recipe
       approval, a provenance head) trusted without the re-gate
       against current policy, or a re-gate that fails open;
     - untrusted input that reaches an authority grant, a credential,
       or what gets published, imported, or verified without
       validation, and the missing tamper test for it;
     - a credential that can reach a log, an error, a workspace, or
       agent-visible output;
     - a delete or cleanup that can act outside the names it owns;
     - a new Tier A gate whose doc comment doesn't name what it
       re-checks;
     - a file that does Tier A work and is missing from the Tier A
       Paths list.

     Two things are not findings, because the tiers put them out of
     scope: a missing column-versus-body cross-check or tamper test on
     a Tier B row, and tampering with the daemon's own database. A
     unit merged on or after 2026-11-07, when that rule was adopted,
     that changed a Tier B row's decode and extended such checks
     instead of removing them is a finding, unless a Tier A gate
     relies on them. Require the disposition record: the findings
     summary lists each Tier A candidate as confirmed,
     rejected-by-verification (so a later round does not re-raise
     it), or accepted-by-decision;
   - a concurrency and lifecycle lens for any partition that launches
     goroutines, owns a background loop, or coordinates teardown:
     goroutine ownership (every launch either joined, typically via
     WaitGroup+context or done-channel+cancel, or deliberately unjoined
     under an explicit concurrency bound it releases when it returns;
     none merely orphaned), cancellation that actually propagates past
     a context deadline or cancel, teardown that leaves no unbounded
     goroutine and double-closes no channel, and effects that stay
     single and correctly ordered under concurrent or retried
     execution; instruct the reviewer to hunt the missing join or
     bound, the unpropagated cancel, and the duplicated effect, not to
     confirm the happy path; this lens carries no disposition-record
     requirement, unlike the Tier A lens above;
   - a client lens for any partition that touches `app/` or `api/`:
     `api/openapi.yaml` against the generated client and the daemon
     handlers that serve it (a field one side renamed, made optional,
     or stopped sending); how each changed surface shows absent,
     stale, loading, and failed data, hunting old data shown as
     current; a task that outlives its view or state changed off its
     actor in changed Swift code; whether tests pin the changed states
     on both Mac and iPhone; and whether `app/SURFACES.md` was updated
     by the PR that changed what the app shows;
   - a conventions check for daemon Go code the tracker's PRs added or
     substantially revised, against AGENTS.md (Daemon Coding
     Conventions). The conventions are a ratchet: a deviation that
     predates these PRs is not a finding;
   - an open-fork guard for each owner decision deliberately carried
     unresolved: instruct the reviewer to check the code did not
     quietly resolve it, not to re-litigate it;
   - contract seams: contracts versus their consumers; one unit's
     guarantees as assumed by another; a PR that changed paths outside
     its declared Scope; a shared package (the AGENTS.md Contract
     Changes list) edited outside a `kind:contract` PR; and the
     Contract Changes rules themselves (a breaking change fixes every
     user in the same PR, a change of meaning is a rename, a
     stored-data change is added first and then migrated, a migration
     declares what it drops);
   - landing seams, naming the shared files and PRs the derivation
     listed: where two units changed one package or file without a
     `starts-after` between them, check that the combination behaves,
     because the required check proves only what it tests; for each
     PR with a rebase record, read what the resolution kept,
     combined, or dropped, and check that nothing another unit landed
     was lost.
3. **Standing discipline paragraphs**, numbers filled in:
   - consult `docs/history/decisions.md`, and the devlog notes the
     prompt names, before flagging a design choice as wrong: settled
     decisions with named deciders are re-litigated only with new
     evidence;
   - search open issues before filing, not only deferral-labeled
     ones: a defect already tracked anywhere open (a deferral, a
     scheduled unit, a `needs-human` prerequisite, an earlier round's
     finding, ordinary backlog) gets no duplicate issue, and only the
     filing is skipped. A confirmed, still-present tracked defect
     stays in the findings summary and the remediation proposal,
     linked to the existing issue (name where this tracker parked its
     remainders); if its severity is understated, comment on the
     existing issue as well;
   - file one issue per confirmed finding from the work-unit
     template, with its severity (P1 to P3, from the defect's reach)
     in the Objective. Label it `kind:*` by the nature of the fix:
     `kind:fix` for a defect, and `kind:contract` when the fix changes
     a shared-package surface (the AGENTS.md Contract Changes list).
     `kind:contract` invokes contract review and implies no order. A
     finding whose remediation is maintainer-only (repository
     settings, credentials, forge-app administration) is filed
     `needs-human` instead, fiat-only and never a proposed unit. No
     finding gets the `deferral` label or a milestone: a wave's Exit
     counts a deferral-labeled finding as dispositioned, and the
     review only proposes dispositions. The findings summary says
     that its findings stay outside the unscheduled queue until the
     owner dispositions them. Write
     the Dependencies field in the one shape
     `docs/coordination.md` (Work-Unit Issues) allows: `starts-after`
     only when the fix must be written against another unit's result,
     otherwise a bare `none`;
   - write the posted findings summary and remediation proposal for a
     human reader, per AGENTS.md (Writing for Humans): bottom line
     first, plain language, front-loaded headings, layered so the
     skim carries the decision and the evidence sits beneath it. The
     dense hunt and disposition vocabulary is the reviewer's working
     register, not the posted artifact's;
   - shape the findings summary as: the round's marker line first;
     then (a) a one-line verdict on whether the tracker's written
     close condition is met and what blocks it;
     (b) a findings table keyed by issue number, giving the gap in
     plain terms, severity, current tracking status, and proposed
     disposition;
     (c) what blocks closure, stated against the tracker's actual
     written close rule. List literal blockers first: each unmet
     clause and its missing proof. Then propose a disposition for
     every finding: **Deferred beyond exit**, with the reason and a
     "revisit when" trigger, and on the findings the reviewer would
     fix before exit, the marker **Suggested to block on**, with a
     one-line risk reason that names the close clause, or the
     activity the fix must precede and whether the exit exercise uses
     it. Severity alone does not make a finding an exit blocker. The
     owner's answer makes a finding required, deferral-labeled, or
     declined with a note; the reviewer proposes and records none of
     those as decided. A recommendation does not amend the written
     close rule; required run or alert evidence remains required even
     if findings are deferred. For a tracker that is already closed
     there is no exit left to block: the verdict says whether the
     merged work needs fixes, each finding is proposed **Fix now** or
     **Deferred**, and the blocker list is omitted. Then (d) the
     per-finding detail; (e) a coverage statement, one line per
     partition: read in full, sampled (say what was left out), or not
     read; and (f)
     the required Tier A disposition record, kept as a compact table
     or appendix beneath the summary, never as the lead;
   - post beneath it a proposed remediation plan in tracker shape, so
     the owner can adopt it as a tracker (a proposal, not a schedule).
     Its first line is the round's plan marker. Have the reviewer read
     the current `docs/tracker-format.md` and `docs/coordination.md`
     (Tracking Issues) before composing the comment, and use their
     template, section order, diagram rules, and structural
     **Startable now** projection. The intro names the proposed exit
     fixes and distinguishes literal requirements from reviewer
     recommendations. Select `## Units` from the findings marked
     **Suggested to block on** (**Fix now** on a closed tracker), not
     the full findings list, and carry
     forward the units earlier rounds proposed that are still open,
     so the latest proposal is complete by itself. Inclusion in the
     findings table does not make a fix an exit condition.
     `## Status` starts with the Mermaid diagram, followed immediately
     by the verbatim legend and the labeled status bullets. `## Units`
     is one flat list of bare task-list references (`- [ ] #N`), with
     `needs-human` entries under `### Owner-run` after it; no title,
     objective, kind, or dependency text goes on a unit line. Write a
     **Startable now** entry as a bare `#N`, or `#N (contract)` for a
     contract unit. Keep the written tracker-level gate in `## Exit`;
     identify any additional recommended checks as proposals, not
     adopted requirements. Put rationale, activity-specific
     prerequisites, proposed relationship changes, and explicit
     deferrals with revisit conditions in the dated, collapsed
     `## Notes` block. Other findings stay linked there, not as
     optional tail units in the exit chain or on its critical path.
     Derive diagram edges from the issues' Dependencies fields; a
     preferred review order is not an existing dependency or fence.
     Record proposed changes as proposals without silently making them
     the diagram's authority. Propose no phases, groups, or
     concurrency limits: units start freely (AGENTS.md, Coordination),
     and a contract unit takes no place in any order. Before posting,
     check the rendered structure against the current tracker
     template;
   - the mandatory decision note: an adversarial audit whose
     confirmed findings change policy or implementation direction is
     on the AGENTS.md mandatory-note list, so when that trigger
     applies, write the decision note per `devlog/README.md` and open
     a PR carrying only it (scope: `devlog/`), left open for the
     owner; where the reviewer cannot push branches or open PRs, the
     findings summary must instead state explicitly that the note is
     owed, so the human or the first remediation unit creates it
     before remediation begins;
   - prohibitions: do not create a tracker issue, do not schedule
     anything (no milestone, no tracker listing), do not edit the
     tracking issue's body or reopen a closed one, and do not claim
     or fix anything (the
     mandatory decision note above is the one carve-out).

## Emit and Stop

- Output the complete prompt as one fenced block, ready to paste.
- State what the executing reviewer needs: a clone that can check out
  the exact `main` SHA the prompt embeds, and permission to file
  issues and comment on the tracking issue.
- Report what the human should know before or alongside the review:
  the round number, its scope, and the earlier rounds found; the
  review's size in units and PRs; an unmet close condition; an open
  `needs-human` prerequisite; a stale tracker checklist worth a
  planner repair; and, for a wave, the closed ad hoc trackers that
  have had no review.
- Do not run the review, file issues, or post to the tracker.

This skill restates the finding-filing and remediation-proposal rules
above so the emitted prompt is self-contained; AGENTS.md,
`docs/coordination.md`, and `docs/tracker-format.md` stay authoritative,
and a change to those rules there must be mirrored here.
