---
name: review-wave
description: Generate the fresh-context adversarial review prompt for a merged wave of the docs/plan.md §11 coordination table — derive the wave's merged units, their PRs, and the plan sections their issues cite, then emit one self-contained, vendor-neutral prompt for the human to hand to an external reviewing agent (typically Codex). Use when the user asks to "review wave N", "generate the wave N review prompt", or "run the wave exit review". Not for executing the review itself, not for reviewing an individual PR, and not for the §7 production review stage (that is daemon runtime, not development process).
---

# Wave Exit Review Prompt (Spine)

You are the spine role (AGENTS.md, Coordination). This session derives
and emits a reviewer prompt, nothing else: it writes no code, files no
issues, posts no comments, and never performs the review itself.

The wave number N comes from the invocation argument. If it is missing,
or no pinned or recent "Wave N tracking" issue exists, stop and ask.

## Why a Prompt, Not a Review

The wave exit review (plan §11) is load-bearing only if the reviewer is
fresh: an agent given only the repository and its documents, never the
design history of the sessions that built the wave. The implementing
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

## Derive the Wave Before Prompting

Wave content is never hard-coded here; derive it at run time. Read, in
order:

1. The "Wave N tracking" issue, end to end: units, shape (serial,
   parallel lanes, integrated), preconditions, surfaced forks, close
   condition, and any comments after the body.
2. The wave's merged PRs, mapped from each PR's closing references,
   never from the tracker's checklist alone: a checklist can be stale,
   and a wave PR that closed an issue the tracker body recorded as
   deferred is still merged code in scope. Sweep merged PRs over the
   wave's date range for closing references to wave units and to any
   issue a wave PR pulled in.
3. Each unit issue end to end — the issue is the work contract, so
   its Acceptance fixture/test list and Non-goals are spec the hunt
   targets must carry even where docs/plan.md never repeats them —
   then the plan sections the unit issues cite, in full, plus §11's
   exit text for the wave. These state the guarantees the hunt
   targets pin. The speculation test governs claimed requirements
   only: a requirement neither a unit's contract nor the cited
   sections back is speculation, but an in-scope correctness,
   security, or edge-case defect is a finding whether or not any
   contract names it.
4. `docs/history/decisions.md` entries binding the wave, the devlog
   notes the unit issues and merged PRs link, plus a devlog search by
   the wave's affected paths, topics, and contract names for binding
   notes nothing linked (a settled owner choice can live in a note
   never promoted to the history file), and every open owner fork
   the tracker or the cited sections carry deliberately unresolved.
5. The open deferral queue (open + `deferral` label) filtered to the
   wave's surfaces, and where the tracker parked the wave's rescoped
   remainders: these feed the prompt's dedup paragraph so a known,
   tracked gap is not re-filed as a finding.
6. Each merged PR's touched paths, so hunt targets name real packages
   and files, not guessed ones.
7. The audited tree: fetch the default branch and resolve its exact
   tip SHA. Review evidence belongs to one base commit (AGENTS.md,
   Pull requests), so the derivation and the review both bind to that
   commit; the prompt embeds it rather than telling the reviewer to
   resolve a tip of their own.

## Preconditions (Stop on Failure)

- Every unit on the tracker is closed by a merged PR. Stragglers mean
  the wave has not exited; list them and stop.
- No findings summary from a prior exit review is already on the
  tracking issue. One present means the review ran; stop and report
  for resume-or-repair direction rather than generating a second
  prompt.

The wave's declared close condition need not be fully satisfied (a
close condition can depend on post-review events, e.g. a first
production run); the review gates the next wave's planning, not the
other way around. Flag any unmet close condition in the report, not as
a stop.

## Compose the Prompt

Fixed skeleton, derived content. The prompt must be self-contained:
the reviewer has no other context, so every issue number, PR number,
section cite, and dedup pointer it needs must appear in the prompt
text itself.

1. **Fresh-context preamble.** "You have this repository and its
   documents (docs/plan.md, AGENTS.md, the devlog directory,
   docs/history/decisions.md, docs/spikes/). You have no other context
   about the project's history." Then the assignment: adversarially
   review the wave's merged code — tracking issue number, unit issue
   numbers, PR numbers, dominant plan sections — against the sections
   the unit issues cite, at the exact `main` tip SHA the derivation
   resolved, embedded here in the prompt. The reviewer records that
   SHA in the findings summary; if `main` has advanced past it when
   the review starts, the prompt is stale — stop and hand back for
   re-derivation rather than resolving a newer tip.
2. **Hunt targets.** Partition by lane when the wave ran parallel
   lanes, by unit when it ran serial. For each partition, name the
   guarantees the cited sections state, phrased as implemented, not as
   tested: the enforcement paths, the failure and edge cases, the
   negative probes, and whether the tests pin the guarantees or pass
   around them. Every partition also carries a standing correctness,
   security, and edge-case hunt over its merged code, not only
   conformance to the written contracts. Layer on, where the
   derivation shows they apply:
   - the risk-class lenses from the AGENTS.md finish line for any
     partition on a destructive path, credential-leak surface, or
     returned-object trust boundary (a decoded or caller-supplied
     trust bit — an eligibility, approval, or provenance claim —
     never trusted without the re-gate against current state;
     ordinary returned data carries no such gate), with the required
     disposition record: the findings summary lists each risk-class
     candidate as confirmed, rejected-by-verification (so a later
     audit does not re-raise it), or accepted-by-decision;
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
     requirement, unlike the risk-class lenses above;
   - an open-fork guard for each owner decision deliberately carried
     unresolved: instruct the reviewer to check the code did not
     quietly resolve it, not to re-litigate it;
   - cross-partition seams: contracts versus their consumers, one
     unit's guarantees as assumed by another, lane edits inside
     another lane's declared paths or shared packages outside a
     `kind:contract` PR.
3. **Standing discipline paragraphs**, numbers filled in:
   - consult `docs/history/decisions.md`, and the devlog notes the
     prompt names, before flagging a design choice as wrong: settled
     decisions with named deciders are re-litigated only with new
     evidence;
   - search open issues before filing, not only deferral-labeled
     ones: a defect already tracked anywhere open (a deferral, a
     scheduled unit, a `needs-human` prerequisite, ordinary backlog)
     gets no duplicate issue, and only the filing is skipped — a
     confirmed, still-present tracked defect stays in the findings
     summary and the remediation proposal, linked to the existing
     issue (name where this wave parked its remainders); if its
     severity is understated, comment on the existing issue as well;
   - file one issue per confirmed finding with severity; label it
     `kind:fix` with the owning lane per the canonical lane table in
     `docs/coordination.md`, with three routing exceptions: a finding
     whose fix requires changing a shared-package surface (the
     AGENTS.md Contract changes list) is filed `kind:contract` and
     `lane:spine`, so scheduling it enters the serialized contract
     chain rather than a lane queue; a finding whose remediation is
     maintainer-only (repository settings, credentials, forge-app
     administration) is filed `needs-human` with no lane and no
     milestone, fiat-only, excluded from the schedulable remediation
     phases; and where the canonical table maps none of the affected
     paths, the lane is assigned explicitly with a one-line rationale
     on the issue, never guessed silently or omitted;
   - post the findings summary as a comment on the tracking issue,
     and beneath it a proposed remediation plan: a proposal, not a
     schedule; findings grouped into proposed units in the full
     work-unit format (Objective, Non-goals, Affected
     interfaces/contracts, Acceptance referencing the finding issues
     the unit closes, Scope / declared paths, Dependencies); ordered
     as a serial
     `kind:contract` phase first (anything touching
     `daemon/internal/domain`, `daemon/migrations`, `api/`, or shared
     interfaces, sequenced by dependency), then parallel per-lane
     sequential fix phases; explicit deferral proposals with reasons;
   - the mandatory decision note: an adversarial audit whose
     confirmed findings change policy or implementation direction is
     on the AGENTS.md mandatory-note list, so when that trigger
     applies, write the decision note per `devlog/README.md` and open
     a PR carrying only it (scope: `devlog/`); where the reviewer
     cannot push branches or open PRs, the findings summary must
     instead state explicitly that the note is owed, so the human or
     the first remediation unit creates it before remediation begins;
   - prohibitions: do not file the proposed units as issues, do not
     modify the tracking issue's checklist, and do not fix anything
     (the mandatory decision note above is the one carve-out).

## Emit and Stop

- Output the complete prompt as one fenced block, ready to paste.
- State what the executing reviewer needs: a checkout of the exact
  `main` SHA the prompt embeds, and permission to file issues and
  comment on the tracking issue.
- Report anything the derivation surfaced that the human should know
  before or alongside the review: an unmet close condition, an open
  `needs-human` prerequisite, a stale tracker checklist worth a spine
  repair comment.
- Do not run the review, file issues, or post to the tracker.

This skill restates the finding-filing and remediation-proposal rules
above so the emitted prompt is self-contained; AGENTS.md and
`docs/coordination.md` stay authoritative, and a change to those rules
there must be mirrored here (the plan-wave synchronization obligation,
extended to this skill).
