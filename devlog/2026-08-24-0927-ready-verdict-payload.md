# Ready Verdict Payload

## Decision

Chose a narrow, creation-immutable readiness summary on
`ready_for_final_review` items over copying the full readiness verdict. The
class and evaluation-set digest are the durable facts later composition needs;
check states remain the authority for reasons, waivers, and advisories.

New production items always carry the summary. Legacy and fake-mode items may
carry `null`: fake publication does not run the production verification model,
and manufacturing a clean verdict would be less honest than preserving
absence. Recovery accepts that authenticated legacy absence in one direction
but never backfills it through the item lifecycle or replaces a present value.

## Returned-Object Boundary

Chose a store-owned canonical summary column over trusting the mutable item
JSON or recomputing current policy on every read. An independent refute-first
pass showed that structural validation alone would accept a changed class,
arbitrary nonempty evaluation digest, or stripped summary after restart. The
new column distinguishes a row created without the field from a current row
whose body was altered; single-item and collection reconstruction reject body
or store-column divergence.

The same pass found that Swift's synthesized encoder omitted a nil optional
despite OpenAPI requiring the property with a nullable value. Mock responses
now insert explicit `readiness: null`, matching the daemon wire contract and
making raw-wire parity testable.

## Presentation

Chose an explicit `(degraded)` title plus labeled class and evaluation-set
detail rows over a new card layout. This is the smallest distinction that
prevents degraded readiness from looking clean while leaving the broader
readiness and waiver presentation to its existing work.

## Rejected Risks

Blocked and invented summary classes fail domain validation, caller aliasing is
removed by construction cloning, lifecycle mutation compares the complete
summary, and conflicting present summaries fail crash recovery. The current
verification registry has no alternate generation that could change a verdict
between item creation and recovery, so generation-aware historical replay is
not part of this contract.

Revisit when verification registry generations can change across a ready-item
crash frontier, or fake publication begins running the production readiness
model.
