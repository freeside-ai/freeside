# Verification State Algebra and Readiness Re-Gate

Date: 2026-08-10. Tracking: #653. Authority: plan §6 at revision 29.

## Decisions

**Chose an explicit three-class verdict over a ready boolean.** The domain
evaluator starts from the complete current requirement-resolution set and
returns `Blocked`, `ReadyClean`, or `ReadyDegraded`. A missing state is
committed as an absent required `NotRun` result and blocks. The production
engine carries clean and degraded counts separately; its older aggregate
ready-item event count remains operational telemetry, never readiness
authority.

**Chose constructor-hidden waiver attachment over an exported nullable waiver
slot.** Go has no native sum types. `CheckFailure` therefore keeps its waiver
field unexported and exposes read-only access; only domain construction can
attach a waiver, and that construction requires a required resolution in the
closed waiver-eligible check-class registry. JSON reconstruction still
re-runs the same gate, so storage bytes cannot forge an otherwise
unrepresentable waiver.

**Chose an append-only waiver lifecycle over a mutable current-state row.** A
grant binds the digest of its first `granted` event. Revocation or expiry is a
new chained event. Reconstruction reads the current frontier and refuses a
grant unless that frontier remains exactly the bound active event. This keeps
historical run-time digests intact and makes revocation fail closed without
rewriting evidence.

**Chose migration 0038, not the implementation plan's provisional 0037.** The
dependency #652 merged first and consumed 0037 for finding dispositions, as
the contract-chain ordering required. Historical resolutions are not unique
by requirement-set/key: policy changes may produce a new resolution digest
for the same definition, so the lookup is a non-unique index rather than a
constraint that would erase history.

**Chose the existing production publication seam as this unit's concrete
effect boundary.** The engine reconstructs the complete current verdict after
the required review and immediately before the first publication effect, then
reconstructs it again on recovery before creating readiness. The proofs bind
candidate head, exact base, recipe or review-configuration/instruction digest,
resolution digest, current policy digest, requirement-set digest, and the
tighten-only floor/registry generation. There is no distinct
readiness-consuming admission effect in the current pre-#654 engine; #654 can
consume these persisted types when it retrofits `run_proposal`. Adding a
speculative admission callback here would create authority with no current
owner.

**Kept API and app out of scope.** Neither surface currently renders check
state or verdict classes, and #657 is the already-sequenced cross-component
unit for the runs list and timeline. Domain goldens pin the wire shapes now;
the API and generated consumers move together in #657.

**Chose one trusted evaluation context over per-axis validation.** Review
rounds 1–4 surfaced the same returned-object trust class on a new coordinate
each round: floor generation, set mixing, candidate head and base,
applicability and kind, recipe approval, waiver lifecycle, readiness by
omission. Rather than a fifth point patch, the boundary moved once: the
evaluator takes an explicit `EvaluationTarget` that every proof must cover
and that joins the evaluation-set identity, and the store re-resolves every
policy-bearing resolution field against a daemon-owned requirement registry
(an Open option mirroring `WaiverGrantApprovals`, always seeded with the
compiled production set from one shared domain derivation) and re-runs the
class-dispatched recipe authority on proof write and read. Rejected
alternative: validating only the cited coordinate per round, which had
already sustained four rounds without converging.

## Refute-First Findings

**Confirmed and fixed:** a proposed unique `(requirement_set_digest,
requirement_key)` index would have rejected a later policy-bound resolution
while preserving the same requirement definition. The final migration keeps
that historical axis non-unique.

**Rejected by verification:** a base advance cannot reuse a base-dependent
proof. The base revision is inside the proof digest and the complete state is
inside the evaluation-set digest; changing only base changes both identities.

**Rejected by verification:** omission cannot produce readiness. Evaluation
with a complete resolution and no corresponding state returns one explicit
absent `NotRun` blocking reason.

**Rejected by verification:** a revoked waiver cannot remain effective. Store
reconstruction reads the latest append-only event, returns the inactive
lifecycle error, and a higher current floor/registry generation also rejects
the historical resolution. The real-process SIGKILL matrix reconstructs both
the immutable resolution and active waiver after abrupt daemon death.

**Confirmed and fixed:** structural validation alone could accept a decoded
or cached waiver after its lifecycle frontier changed, and a self-described
grant authority was not sufficient evidence of a daemon-owned grant. Every
evaluation that contains a waiver now requires a current re-gate callback;
the store supplies one by rereading the latest lifecycle event and resolving
the authority/digest pair against its immutable Open-time approval registry.
An absent or unapproved grant fails closed.

**Rejected by verification:** a proof or waiver row cannot borrow another
resolution. Immutable writes and reads resolve the referenced resolution,
cross-check copied SQL columns against canonical JSON, and re-run the current
registry and floor gates before returning the object.

**Confirmed and fixed (rounds 3–5):** an empty resolution set evaluated
trivially clean (now rejected); individually valid proofs could mix candidate
heads or bases into one verdict (every proof must now cover the explicit
evaluation target); persistence trusted a caller-supplied resolution's
applicability, kind, and class (now re-resolved against the trusted
requirement registry, unknown sets failing closed); and a clean-verification
proof's recipe skipped the approved-recipe registry at write and read (now
class-dispatched, with repo-change-policy proofs failing closed until a
trusted recipe authority exists and independent-review authority re-derived
at the engine's evaluation boundary, where profile-scoped approval lives).

**Confirmed and fixed (round 6):** the post-publication recovery path
re-derived and persisted the clean-verification proof before deciding recipe
approval, so a recipe revoked at the store returned `ErrUnapprovedRecipe` as a
lane-fatal reconcile error instead of the durable `HoldRecipeRevoked` the path
already intends when readiness does not yet exist. Persistence is now a
deferred closure the recovery path runs only when it creates new readiness
under a still-approved recipe, so a revoked recipe holds before any proof
write and an already-ready candidate reports its pure in-memory verdict
without re-deriving proofs. Separately, the store trusted a caller-supplied
independent-review recipe (the class gate returned a bare nil): it now fails
closed unless the caller asserts the run-scoped authority it re-derived
(`AuthorizeIndependentReviewRecipe`), extending the returned-object trust
boundary to independent review, not only clean verification. That assertion
is exposed on the read transaction so a future authenticated reconstruction
can assert it too, so it catches a caller that forgets to establish trust,
not one that fabricates within its own transaction: the honest limit, since
the authority itself lives in the engine's run trust context, not a store
registry.

**Confirmed, guarded, and deferred (round 6, #688):** `EvaluateReadiness`
rejects only an empty resolution set, not a nonempty strict subset, so
omitting a requirement could evaluate clean (a different axis than the
already-rejected missing-state case). It is not reachable through the sole
production caller, which builds the complete set from
`ProductionRequirementDefinitions`; a production-path completeness assertion
guards against drift and the general evaluator fix is deferred to #688.

**Confirmed and deferred (round 7):** two findings were confirmed real but are
not reachable through the production caller, so they are tracked as follow-up
units rather than folded into this already deep PR (owner-confirmed; neither
gates wave 5, whose §6 consumers depend on the verdict surface, not these
internals). #690 (`kind:contract`): `gateRequirementResolution` re-gates class,
kind, applicability, and base dependence but not `ResolvedPolicyDigest` or
`SamplingDecision`, the latest field in the recurring per-field trust class; the
durable fix re-derives the whole canonical resolution against trusted inputs so
no field can escape, which needs a trusted current policy/sampling context the
store lacks today. #689 (`kind:fix`): an already-ready candidate cannot finalize
under a fully revoked recipe (below); a clean hold is not viable because an
already-ready run plus a publish-blocked item is contradictory, so the fix is to
finalize through the `GetAttentionItemRecord` recovery read.

## Revisit When

- Follow-up #689: an already-ready production candidate cannot finish under a
  fully revoked verification recipe. The round-6 fix stops the check-proof gate
  from blocking it, but arming its watches re-reads the ready item (via the
  re-gating `GetAttentionItem`, including inside the shared `armPublicationWatches`
  helper), whose evidence artifacts fail the publish-eligibility re-gate. The fix
  is to finalize through the recovery-only `GetAttentionItemRecord` read, a
  trust-sensitive change to a shared helper; narrow and operator-recoverable.

- #654 wires `run_proposal` admission: consume the verdict type at that real
  admission boundary without introducing a boolean projection.
- #657 exposes run readiness through sync: add the API schema and both
  generated consumers in the same cross-component work unit.
- #241 or #360 becomes active: extend the canonical proof/resolution binding
  with completion or supersession provenance without changing the existing
  requirement identity.
- A repo-policy requirement deriver lands: register its derived sets in the
  store's trusted requirement registry and replace the fail-closed
  repo-change-policy proof placeholder with that class's real recipe
  authority.
