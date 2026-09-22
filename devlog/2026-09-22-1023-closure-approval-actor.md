# Closure approval actor, outcome matrix, and fallback notice

Work unit: #1487 (kind:contract). Grounded on PR #1484 (plan revision 68).
Source queue item: `devlog/2026-09-21-2151-closure-policy-approval.md`.

## Decisions

- **Named the recorder, not a new authority.** `ClosureApproval` gains a
  `ClosureApprovalActor` (`human`, `policy`); the five-field merge binding and
  `AuthorizesClose` are unchanged. The actor only tells the publisher who
  approved (so a recommended provenance can mark the Source issue line) and lets
  the store refuse a second recorder. Chose this over a separate policy-approval
  type because the binding and its supersession rule are identical; only the
  recorder differs. The zero actor is invalid, so an approval reconstructed
  without a recorder fails closed through `AuthorizesClose`.

- **One outcome matrix in the domain, not two copies of prose.** `ClosureOutcomeFor`
  maps (proposal, human gate, approval state) to reference / hold / item, plus a
  `Recommended` marker that is orthogonal (it never changes the other three).
  The engine and the #1419 publisher consume this one tested function; this is
  the "tested check" the #1484 review asked for, at the qualified-prose altitude
  the plan deliberately kept. The reduction of durable state to the four
  approval states (a stale approval or a superseded decision becomes `none`)
  stays with the caller.

- **Second-actor rule: the store refuses, nothing flips.** Plan §11 revision 68
  item 3 says the gate-on card lets a person "decline or flip a policy-approved
  closure". Read as #1419's rescope does: under the gate the card's decision *is*
  the approval and no policy row is written, so nothing flips; the store refuses
  the second recorder in both directions (`ErrClosureApprovalActorConflict`) and
  the reconstruction fails closed if both rows somehow exist. Revisit when the
  owner wants a human decision to override a recorded policy approval: then the
  decision would replace the policy row instead of being refused, and the gate-on
  `policy` outcome cell becomes undecided.

- **Fallback notice reuses the exceptional interruption class, no new enum.**
  The API schema exposes `InterruptionClass` as a closed enum, so a new member
  would be an `api/` change (a non-goal). The daemon_fallback notice is the same
  effect_proposal card with `InterruptionExceptional` (plan §3.2 tracks that rate
  as a health signal) and a reason that says the pull request is not held. Its
  approval sits on a `resolves=false` proposal, so `AuthorizesClose` is always
  false and no close is ever written. Known limit: a gate flip between two passes
  for one merge leaves the first variant open until the merge moves.

## Refute-first (returned-object trust boundary)

`ClosureApprovalForInstance` reconstructs a decoded row into a trusted
`ClosureApproval`, and the policy write re-gates external-derived state. Each
attack vector below is defended and covered by a store test; none survived.

- **Decoded row trusted as authority** (proposal digest, publication identity,
  merge columns): the policy arm re-reads the instance through
  `GetProposalInstance`, requires the row digest to equal the current proposal
  digest, requires a `propose_site` origin, and revalidates the full
  `ClosureApproval`. Disproved by the wrong-digest and malformed-column tests.
- **Two recorders → ambiguous authority**: both writes refuse the other actor,
  and reconstruction fails closed when both rows exist. Disproved by the
  conflict tests.
- **Stale merge after a base advance**: the write upserts one row and the
  reconstruction binds the current one; `AuthorizesClose` rejects the old merge.
  Disproved by the moved-merge test.
- **Source no longer closable**: the re-gate fails closed on both write and read.
  Disproved by the tampered-project test.
- **daemon_fallback carrying a policy approval**: refused at write and, via the
  origin check, at read; and its `resolves=false` proposal makes `AuthorizesClose`
  false regardless. Disproved by the fallback-refusal test.

## Non-goals carried forward

The policy switch key and its parsing, the engine call sites, the publisher's
reference/decision/draft handling, and any `api/` change (exposing the actor over
the facts endpoint) are #1419 Part D and #1478. #1483's contract half is absorbed
here.
