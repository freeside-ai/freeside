# First-Acceptance Task Naming in the Plan

Chose to record the first-acceptance naming rule in `docs/plan.md` (revision
59) over reverting the shipped code to agent-only refinement. The plan's §5.12
lifecycle paragraph now says the first accepted specification's `title` may
replace an agent-produced name **or the identifier fallback**, once, before
approval; it never replaces an operator name, and a revised specification does
not rename.

## Why

#1208 shipped in PR #1326 with `acceptSpecification`
(`daemon/internal/engine/specification.go:2996-2998`) applying the specifier's
title on first acceptance to an identifier-only task, not only to an agent
name. The plan still described the narrower rule, so Codex finding 3998113048
on PR #1326 flagged the conflict. PR #1326 kept the owner-selected #1208
contract and deferred the document reconciliation here. This unit made the
plan follow the contract rather than narrow the code.

## What this replaces

Revision 48, item 3 (elaborated in
`devlog/2026-09-07-1136-task-design-decisions.md` §Stored Name) had retained
the approval-time first name for an identifier-only task:

> Retained the approval-time first name for an identifier-only task because
> #1203 explicitly requires that fallback at approval. #1208's submission hook
> refines an existing agent name; it does not supply this fallback.

The condition that changed: the owner's #1208 contract (its 2026-09-12
planning reconciliation) chose the wider rule, and PR #1326 shipped it. The
plan now widens what the single pre-approval refinement may replace.

#1203's approval fallback still holds. When the approved body opens with a
heading, approval still names a task that is still on its identifier from that
heading (`startApprovedImplementation`, `specification.go:3868-3877`), so a
task that never received a specifier title is named at approval from it.

## Rejected

Narrowing `acceptSpecification` back to agent-only refinement, as the PR #1326
review proposed. That would overturn an explicit owner decision; the owner
chose the wider rule.

## No ADR

Revision 48's decision stands: operator heading first, advisory namer, one
pre-approval refinement, approval freeze, operator rename after approval. This
change only widens the target set of that one refinement (agent name plus
identifier fallback), so it is recorded as revision 59 citing revision 48, not
promoted to an ADR. If the owner wants an ADR, that promotion is its own
reviewed change and this reconciliation starts after it.

## Revisit when

Real use shows the specifier's first title is a worse name than waiting for
the approved heading, or #1321 (task name history) needs a different
producer-transition model for the identifier-to-title step.
