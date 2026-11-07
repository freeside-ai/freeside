# Review Guide on the Ready Card

Issue #2738; plan revision 112; tracker #2753. The owner decided on
2026-11-07 that the `ready_for_final_review` card carries an optional review
guide: an agent-written, ordered list of the places in the change where a
human's judgment matters, each with the real code it points at, placed after
the change summary and before the verification verdicts.

Chose an ordered reading guide written by the implementer as a labeled
claim, with daemon-read excerpts, over leaving the card at summary plus
View PR, because the summary says what changed and the PR files view says
where, but nothing tells the operator which few places decide whether the
change is right. That is the question the ready card exists to ask.

## Source

T3 Code's pull-request review guide (`pingdotgg/t3code` v0.1.0 to v0.1.7,
read 2026-11-07, summarized on tracker #2753). Its contract,
`packages/contracts/src/pullRequestReviewGuide.ts`, defines stops with a
title, a summary, an attention kind (tradeoff, contract, product, risk,
accessibility), and line-range excerpts with captions, bound to the head
SHA and marked stale when the head moves. Freeside adopts the shape, not
T3's layering: in T3 the guide is product output with no trust classes,
while Freeside's cards keep agent claims and daemon facts apart (plan §9,
Summary Provenance).

## Decisions

- **Chose a labeled claim over a daemon fact for the guide's prose.** The
  stops, summaries, captions, questions, and notes are judgment, so they
  are `producer_class: agent`, provenance-bound to the producing
  invocation's claim like the summary, and never enter `evidence_snapshot`
  (§5.15 rule 2). Rejected: a
  daemon-generated diff summary, which the daemon can produce from facts
  but which can only restate what changed; choosing where judgment matters
  is the agent's work, and the claim label keeps that visible.
- **Chose daemon-read excerpt text over agent-quoted code.** The agent
  supplies a location and a caption, never code text; the daemon reads
  the lines from the imported commit, or its base for removed code, and an
  excerpt it cannot read renders as unresolved. Rejected: the guide as free
  Markdown inside the summary claim, because code quoted by the agent
  would render as if it were the change's code, and a wrong or invented
  quotation is exactly what an operator skimming a card would not catch.
  Under the chosen split, a bad pointer shows as unresolved or as visibly
  different code under the agent's caption.
- **Chose binding the guide to the candidate head over keeping it across
  heads.** A head change already supersedes the ready item, and §5.15 rule
  2 already invalidates head-bound evidence on a remediation head, so the
  guide follows the item and no guide outlives its head. Rejected: a stale
  guide kept, possibly marked stale, across heads, as T3 does, because a
  superseded Freeside item is not acted on and a new item brings its own
  guide; carrying the old one forward would point the operator at code the
  PR no longer holds.
- **Chose the implementer writing the guide in its own handoff over a
  separate generator run.** The guide is reserved, launcher-declared
  evidence in the implementer's or remediator's export, so its provenance
  is the producing invocation and its head binding is the handoff head.
  Rejected: T3's separate generator agent run in a temporary checkout,
  which would need its own credential, its own head binding, and its own
  provenance, all of which the handoff already supplies.
- **Chose an optional guide held to the evidence rules over a required
  one or a lenient one.** A run that writes no guide still produces a
  ready item, because a missing reading aid does not make a change less
  ready. A guide that is present but malformed is invalid reserved
  evidence and fails the import closed, since the importer has no
  waivable-finding class for evidence; #2750's decision note records the
  reasoning for that choice. A well-formed excerpt the daemon declines to
  read (binary, too large) renders as unresolved, not as a failure.
- **Chose to keep Non-Goal 1 over adding review features.** The guide
  orders a reading and points into the PR, as View PR is navigation under
  revision 78. It adds no inline commenting, no per-stop resolution, and no
  verdict; review and merging stay on GitHub. Rejected: per-stop accept or
  reject, which would make the card a code-review surface.

## Left to the Units

The plan commits only to the layering. The wire shape, size caps, stop
limits, sensitivity class, and the unresolved-reason vocabulary belong to
the contract unit (#2739); the prompt, launcher declaration, import
validation, and excerpt resolution to the producer (#2750); the card
rendering to #2751.

Revisit when open-to-decision time or reversal rate on ready items (§9
Measurement) shows no gain where a guide is present, or when the ready
card gains a
second reading aid that competes with it.
