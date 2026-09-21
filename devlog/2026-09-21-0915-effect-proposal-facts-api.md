# Effect-Proposal Facts and Revision Over the API

Work unit #1468 (Part C of #1443's split). Adds the
`GET /attention/items/{item_id}/effect-proposal` facts read, the
`effect_proposal_revision` decision-payload field, their daemon projection and
wire mapping, contract goldens, and MockServer parity. This note records the
two lasting decisions and the refute-first findings for the facts read (a
returned-object trust boundary). The acceptance contract itself lives in the
#1468 issue body.

## Wire Revision Is a Kind-Keyed Arm; the Stored Command Message Stays Flat

Chose to keep the daemon-internal `EffectProposalRevisionInput` flat
(`{"resolves":...}`) and map the kind-keyed wire arm
(`effect_proposal_revision: {source_issue_closure: {resolves}}`) into it in
`daemon/internal/signet/http.go`, rather than reshape the internal type to
match the wire. The store's revise path accepts a revision only when the
durable command message is exactly the canonical `{"resolves":...}`
(`decodeCanonicalRevision`, `store/effect_proposal.go`), and #1443 already
records that flat message. Reshaping the internal struct to the kind-keyed arm
would change the stored bytes and break the revise path's digest re-binding.
The wire arm is kind-keyed so a future effect kind adds its own arm without a
flag-day; the daemon owns target, provenance, and origin, so the arm carries
only `resolves`. The mock's `CommandResultTrust.recordedMessage` returns the
same flat string, so the client matches a genuine daemon result; a test pins
the string.

Rejected: a flat wire `effect_proposal_revision: {resolves}`. It would drop the
kind discriminator the task/effect split uses everywhere else and give a later
effect kind no place to put its own parameters.

## The `recommended` Fixture Stays Out of the Default Inbox

Chose to serve `verified` closure facts for the default inbox's
`item-effect_proposal` and add a second named fixture,
`item-effect_proposal-recommended`, that stays out of `defaultInbox()`.
`defaultInbox()` is one open item per type and the inbox screenshots pin that
list, so a second effect item there would churn `ScreenshotDigests.json` for no
card-rendering reason (the card is #1444). Tests and screenshot surfaces opt
into the recommended item to exercise the second trust level.

## Refute-First: the Facts Read Rebuilds Trusted Facts From Stored Rows

The read is a returned-object trust boundary. Findings, disproved by checks
except the merge-head re-gate, which review confirmed and the fix added:

- **Hidden-item leak.** The type check (`!= AttentionEffectProposal` →
  `ErrNotFound`) and the snooze check precede the proposal read, mirroring
  `GetTaskProposalFacts`. A snoozed item, a `task_proposal` item, and an
  unknown id all read as 404 (behaviour tests and the HTTP unauth-route table).
- **Untrusted field served.** Target, provenance, and origin come from
  `ProposalForItemWithRevisionContext`, which re-gates the closure subject; the
  merge comes from `ProspectiveMergeForItem`, which validates and fails closed
  on a partial row. A nil merge on a closure item is `ErrInvalidSyncSnapshot`.
- **Merge head not re-gated (confirmed).** `ProspectiveMergeForItem` fails
  closed on a partial row but does not compare the merge candidate head to the
  item head. `ClosureApprovalForInstance` enforces that equality at approval
  time, so the ungated read could serve a candidate the approval path would
  reject as row-inconsistent. The read now re-gates candidate head against item
  head and fails closed with `ErrInvalidSyncSnapshot`, matching the approval
  boundary. The invariant holds for items opened through the service (the item
  head is derived from the merge candidate head), so this is a fail-closed
  defense against a persisted divergence, consistent with the daemon
  reconstruction-boundary convention.
- **Digest binding.** The projection requires `Kind == EffectSourceIssueClosure`,
  a non-nil `ClosureProposal`, and the item's single artifact digest equal to
  the rendered proposal digest, before it builds any fact.
- **Handle/policy leak.** The wire type omits the opaque subject handle and
  policy identities; a test asserts the marshalled JSON carries no
  `subject_handle`, `resolved_policy`, or `policy_run`.

Revisit when: a second effect kind needs a facts arm (the snapshot's
`source_issue_closure` becomes one of several nullable arms keyed by
`effect_kind`), or when #1419 makes the closure target daemon-resolved from
GitHub rather than fixture-supplied.
