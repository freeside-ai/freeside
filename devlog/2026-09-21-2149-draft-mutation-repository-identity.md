# Draft Mutation Verifies Repository Identity (Issue #1481)

`setPRDraft` now requires the GraphQL draft-mutation response to name the
target repository, and fails closed on a mismatch or a missing repository
field. This closes a returned-object trust boundary deferred from PR #1477
(issue #1419 Part C), where Codex raised it as a P2.

## Verify `nameWithOwner` and Fail Closed, Chose Over Trusting the Node ID

The `markPullRequestReadyForReview` / `convertPullRequestToDraft` mutations act
on whatever pull request the `pullRequestId` names, in any repository the
installation can reach. That `node_id` is decoded from a REST response, so it
is a returned-object trust boundary. The response was checked only against the
repository-local pull `number` and the requested `isDraft`. Pull numbers are
repository-local, so a wrong or malformed node id naming a same-numbered pull
in another repository would be mutated and reported as converged.

Chose to select `pullRequest { ... repository { nameWithOwner } }` in both
mutations and reject any response whose `nameWithOwner` is not the target
`owner/name`, or that omits the repository field. The two guards run after the
pull-number check and before the draft-state checks: a nil `repository` errors
first, then a differing `nameWithOwner`, so no partial body reaches the draft
guard on a foreign object.

Chose `nameWithOwner` with an exact string comparison over a numeric repository
id. This matches the existing REST head/base repository check in the same
package (`publisher.go`, the `pr.HeadRepo == repo.path()` / `pr.BaseRepo ==
repo.path()` comparison), keeping one repository-identity convention across the
REST and GraphQL paths. A numeric id would be a second, divergent notion of
repository identity for no gain here.

Casing: GitHub returns `nameWithOwner` in canonical casing; `repo.path()` comes
from the candidate's `owner/name`. A casing difference fails closed, the same
behavior the REST `full_name` check already has. If that check is ever relaxed
to case-insensitive, relax this one the same way.

## Refute-First Findings (Returned-Object Boundary)

- Right number, foreign repository still passes: **disproved by check.** The
  `foreign repository` unit case and a `convergePR`-level case with the
  foreign-repository knob both fail closed.
- Absent `repository` field passes: **disproved by check.** The `omitted
  repository` case fails closed at the nil guard.
- Guard order lets a partial body through: **disproved by check.** The nil
  `repository` guard precedes the `nameWithOwner` and draft guards, and the
  `omitted draft state` case (repository present, draft absent) still errors at
  the draft guard, so neither omission slips past.
- The fake could pass with the selection dropped from the query: **disproved by
  check.** The fake's GraphQL handler now fails the test when the query omits
  `repository { nameWithOwner }`; removing the selection from `setPRDraft` makes
  `TestForgeSetPRDraftTogglesState` fail.

Field existence on real GitHub was confirmed with a read-only query:
`PullRequest.repository.nameWithOwner` exists and returns the canonical
`freeside-ai/freeside`. No unit test can prove the field name; the live query
does, without running a mutation.

Revisit when: Part D of #1419 wires the managed draft intent, making
`setPRDraft` a production caller under a real closable source.
