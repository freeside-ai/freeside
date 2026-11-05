# Stage Summary Handoff Shape

Work unit: #2615, with the client parser in #2614. Extends
`devlog/2026-10-10-1745-agent-summary-presentation.md`, whose presentation
rules this keeps and whose summary headings it replaces.

## Decision

Chose the development workflow's report shapes for the Phase 1A stage
summaries over the `## Change` and `## Remaining concerns` pair, on the
owner's direction of 2026-11-04. An owner already reads a session handoff in
those parts, so a card that uses them reads the same way. The specifier's
summary takes the plan-work-unit report's `## Result`, `## Your call`, and
`## Not verified`; the implementer's and remediator's take the handoff
report's `## Result`, `## Your call`, `## Left for you`, and `## Skipped`
(`docs/agent-workflow.md` §handing-off step 7). Both keep an optional
`## Details` tail.

Chose to leave Status out of both. The card's daemon facts state the stage,
the run, and the PR from records Freeside checked; an agent's Status line
would restate them as an unverified claim and could contradict them. The
plan-work-unit report's What planning changed and Kickoff parts are left out
for the same reason: the specification body and the card's actions carry
them.

Every concern the old `## Remaining concerns` held keeps a home, so "never
omit a concern" still has somewhere to put each one:

- **Specifier.** `## Your call` takes each assumption, which the prompt
  already limits to bounded implementation details, and each point where the
  specification disagrees with the work item, with its default. `## Not
  verified` takes claims read but not run and other uncertainty. A question
  that blocks the specification still goes to `decisions`, never into the
  summary.
- **Implementer and remediator.** The prompt still names "uncertainty,
  unresolved questions, dissent, and unfinished obligations" and says each
  goes in a part after `## Result`. Dissent and a finding declined on
  judgment go under `## Your call`, with the default the writer chose. An
  uncertainty only the owner can settle and a finding the remediator could
  not resolve go under `## Left for you`. The handoff report has no Not
  verified part, so plain uncertainty lands there rather than in a part the
  implementer's summary lacks.

Chose to tell the writer to leave out an empty part. #2614's parser reads a
summary with only `## Result` as having no concerns, and treats a present
part with an empty body as malformed, so an empty heading would drop the card
to the excerpt view with concerns unknown.

The presentation rules from #1378 and #1461 are unchanged: the 80–120 word
target, every concern listed whatever the length, a verifiable outcome
asserted only by naming its command or artifact, headings as advisory writing
guidance the daemon never validates, no client-written summary, no silent
cut, and unknown concerns never read as no concerns. The specifier keeps the
"Under `## Result`, ..." form the 2026-10-10 note chose, so its text sits
beneath each heading.

The specifier prompt kept its 4 KiB budget. Its summary line grew 126
bytes, and the file held 4,095 of 4,096, so seven other lines were
tightened without dropping a rule or a pinned string; it now holds 4,094.
The trims removed only text another line already states or the writer
cannot act on: "Do not implement it" (the authority section forbids
workspace edits and commits), "with recommendations" on sketch decisions
(the `recommendation` field is required), the empty-`addressals` rule's
restated JSON (shortened to "with none, `[]`"), and the `version` and
`digest` fields of a prior-artifact block (the writer acts on neither).
Three more lines were reworded shorter with the same rule. A first cut also
reworded the authority line on vendor and repository instructions; review
found the new word order let "consistent with it" restrict which
instructions could not authorize implementation, so that line is
unchanged.
The next addition to this prompt again has no room.

This answers the 2026-10-10 note's revisit condition about `## Change`
misleading the writer: "Change" was a stretch for a proposal, and `## Result`
names what both a specification and an implementation report lead with.

## Rejected

- **Keeping both shapes in the prompts.** Offering the writer either pair
  would let one stage's summaries vary run to run, and the card would draw
  two layouts for the same stage. The client accepts the old shape only so
  stored summaries keep rendering (#2614); the prompts name one.
- **Stage-specific lead headings.** A specifier lead such as `## Proposal`
  would need its own recognizer path; `## Result` reads correctly for both a
  proposed and a built change.
- **Raising the specifier prompt's budget.** The new part list fit by
  removing restatements, so the question the 2026-10-10 note left open did
  not have to go to the owner this time.

## Revisit When

Revisit when observed summaries put concerns in `## Result` or leave an empty
part heading; when the handoff report or the plan-work-unit report changes
its parts; or when the specifier prompt needs another rule and the budget has
to be raised or the prompt restructured.
