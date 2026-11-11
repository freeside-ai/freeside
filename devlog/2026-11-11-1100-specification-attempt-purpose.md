# Specification Attempts Record Their Purpose

Decision record for #3044: each specification attempt records why the
daemon started it, and the run timeline names that purpose instead of
`Specification · Invocation N`. The note is mandatory because `Attempt`,
a shared domain type and API schema, gains a field.

## Decided at Dispatch From the Chain Check

Chose to derive the purpose in `verifySpecificationChain` over storing
it in the specification request payload. The chain walk already takes
exactly one branch per transition (answer, retry, research, or
`request_changes`) before every dispatch, and it re-derives each request
from durable state instead of trusting the payload. Reporting the branch
it took adds no new trusted input and changes no check. A payload field
would have been a second, self-reported copy of what the walk proves,
and would need its own cross-check against the walk. A Discuss reply
runs on its own lane, so `loadSpecificationDiscussionBinding`'s binding
sets `discussion_reply`; every other lane leaves the purpose nil.

The purpose is written only when `recordAttempt` appends the attempt. A
replayed dispatch finds the attempt already recorded and leaves it, so a
recorded purpose never changes, and `stagesExtend` compares it by value
so a changed or cleared purpose fails `ValidateRunTransition`.

## Old Attempts Stay Null

Chose to leave attempts recorded before the field with no purpose over
backfilling them. A backfill would re-walk every stored chain and
rewrite run bodies, which makes it a data-rewriting migration (Tier A)
for a display label. The field is an added JSON key: an old body decodes
with none, the API reports `null`, and the timeline keeps
`Specification · Invocation N` for it. A body re-encoded after the change
gains `"purpose": null` on its old attempts; nothing compares stored run
bodies byte for byte.

## A Revision Campaign Opens With `revision`

A revision campaign's run (#1083) starts at iteration 2 and revises the
blocked run's approved specification with the operator's answer, so its
first attempt records `revision`, not `first_draft`. It is the same kind
of work as a `request_changes` revision: a prior specification plus
feedback.

Revisit when another stage needs a purpose, such as telling an
implementation remediation pass from an operator-feedback pass; that
would widen the vocabulary and its owning stage rule.
