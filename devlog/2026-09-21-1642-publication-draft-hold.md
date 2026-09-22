# Publication draft hold in the publisher (issue #1419 Part C)

The publisher can now open a pull request as a draft and converge its draft
state on every pass when it holds a managed draft intent for that PR. This is
the merge hold recipe v2 needs: a draft cannot be merged on GitHub, and no
internal Freeside signal can stop a person's direct merge (contract settled
item 4). Part C builds and verifies the mechanism but keeps it dormant: nothing
is closable yet, so the draft intent is unmanaged for every candidate and each
PR's draft state is left untouched (plan §5.15, "a PR with no closable source
is unaffected"). Part D supplies the managed intent for a closable source.

## Draft toggle uses GraphQL, chose fail-closed verification

REST cannot change a PR's draft state, so `setPRDraft` uses the
`markPullRequestReadyForReview` / `convertPullRequestToDraft` GraphQL
mutations, bound to the PR's `node_id`. The mutation response is a
returned-object trust boundary: the code trusts `isDraft` and `number` fields
from an external call.

Chose to verify the response and fail closed over trusting the mutation
succeeded. `setPRDraft` requires the response to name this pull number and
report the exact draft state requested; a GraphQL error, a wrong number, a
wrong `isDraft`, or a missing `pullRequest` returns an error rather than
reporting a hold it did not establish. `createPR` likewise verifies GitHub
echoed the requested `draft`. This follows the daemon rule that a field
returned by an external call is never trusted, and matches the existing
returned-object checks on `createPR`/`updatePR`/`getPR` in the same package.

The REST `draft` bit gets the same fail-closed treatment as the GraphQL
`isDraft`. It decodes through a `*bool` and is rejected when absent at every
decode site (`createPR`, `listPRsByHead`, `getPR`, `updatePR`). A plain `bool`
would decode an omitted or null field to `false`; under a managed not-draft
intent the create echo check and the convergence repair would then both pass,
reporting the hold released without evidence the PR is actually ready. Chose
the pointer guard over trusting GitHub to always send `draft`, matching the
`isDraft` and repository-`private` guards already in the package.

Refute-first findings (returned-object boundary): the fail-closed paths above
are exercised by fixtures that make the fake server echo GraphQL errors, a
wrong number, a wrong `isDraft`, an unknown node id, and an omitted REST
`draft` field on the create, list, and get paths; each is asserted to error.
The absent-REST-`draft` gap was found in review, not before, and is now
closed and pinned by test.

## `desiredDraftState` is a tri-state seam; unmanaged in Part C

`desiredDraftState(Candidate) *bool` is tri-state, threaded into `convergePR`
as `wantDraft *bool`: a non-nil pointer is a managed intent (its value is
whether the hold is active), and nil means unmanaged, so the publisher leaves
the PR's draft state untouched. In Part C it returns nil for every candidate,
so no PR's draft state is touched, created or existing (including one a person
manually converted to draft). Part D returns a non-nil pointer for a closable
source.

Rejected: returning a constant `false` (the first cut, and my initial spawn
instruction). That treats `false` as authorization to release a hold, so every
retry or `ConvergeOutcome` over an owned PR a human had drafted would mark it
ready, violating plan §5.15 "a PR with no closable source is unaffected."
Review caught this; the owner confirmed §5.15 is authoritative and overrides
the constant-`false` reading. The tri-state distinguishes "wants not-draft"
from "does not manage draft," which the bare `bool` could not express.

Content and draft are independent repairs in `convergePR`: either drift opens
the repair gate once, then each mutation runs only if its own state drifted.
Draft is reconciled only under a managed intent; an unmanaged (nil) PR's draft
state is never read as a repair target.

The cancellation fence, though, is checked before each forge write, not once
for the batch. A content PATCH is a forge round-trip during which the task can
be stopped; without a re-check, `setPRDraft` could then release the hold on a
cancelled task's PR after the durable fence. So the draft mutation re-checks
`taskEffectOpen` when a content patch preceded it (when it did not, the batch's
first check immediately precedes the write). This matches the per-write fence
already at every other forge-write site (`createRef`, `createPR`, the head
push, the successor patch). Found in review, not before; pinned by a fence test
that stops the task on the in-flight PATCH and asserts the draft mutation never
fires.

Revisit when: Part D makes `desiredDraftState` depend on the closure proposal
and approval, and adds the base-advance revalidation that keeps a stale-base
approval from releasing the hold (issue #1419 comment, follow-up 1).
