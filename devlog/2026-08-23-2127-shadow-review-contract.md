# Shadow Review Persistence Contract (#838)

Chose a separate `ShadowReviewRecord` type and separate
`shadow_review_records` persistence surface over a discriminator on
`ReviewRecord` / `review_records`. Required-review readiness, round derivation,
finding adjudication, and remediation already derive their evidence from
`review_records`; keeping shadow results out of that table makes their
ineligibility structural. A discriminator would instead require every present
and future authority query to remember an exclusion predicate.

Chose a registered `ShadowReviewSource` binding over a decoded boolean marker.
Reconstruction validates the source against the current registered set and
cross-checks every shadow finding against it, so malformed or substituted
source state fails closed. `ReviewMode` remains unchanged because its members
name selectable routed authority, while this contract is observation-only.

Chose immutable classifier-accuracy samples keyed by run, shadow invocation,
finding, and classification version. The store re-runs the shadow-result,
shadow-finding, and versioned-classification joins at write and reconstruction
boundaries; the sampled assessment therefore remains attached to the exact
evidence it evaluated.

Chose the routed `ReviewRecord` for the claimed run and round as the candidate
authority for every shadow pass. Both write and reconstruction load that
record and require the shadow base and head to match it. A shadow worker's
copied round and SHAs therefore cannot silently attach findings or accuracy
samples to a different candidate. Rejected allowing a shadow-only round: it
would preserve scheduling flexibility by giving up the experiment's same-head
comparison invariant.

The refute-first review confirmed one reachable dual-link path: the original
separate-table shape still allowed one `FindingID` to appear in both shadow and
routed finding joins, after which routed adjudication could consume it. The
contract therefore rejects that relationship in both store write directions,
re-gates it on both reconstruction paths, and uses reciprocal insert/update
database triggers for direct SQL writers. Dual-link fixtures pin both
insertion orders and the damaged-store read boundary.

Automated review then found two more identity ambiguities in the new lane: an
invocation ID could name both a shadow result and a routed result or failure,
and a finding ID could name more than one shadow parent. Public writes,
reconstruction, and database constraints now enforce an exclusive invocation
namespace across those terminal accounts and exactly one shadow parent per
finding. These joins are identities, not reusable content references.

A follow-up review exposed that "terminal accounts" was too narrow: a live
`review_retries` row reserves the same routed invocation before a terminal
record exists. The invocation gate now covers terminal results, failures, and
pending retries in both directions. The provider runtime journal deliberately
stays outside that set because routed and shadow sources both use it; its rows
describe execution, not required-review authority.

Shadow finding reconstruction also re-runs the registered source's normalized
output schema. The generic `Finding` contract intentionally admits native
review observations without a severity or concrete line range, but Claude
shadow evidence requires a P0-P3 severity, a concrete location, and a non-empty
explanation. Persisting against the source-specific contract keeps permissive
native ingest from weakening classifier-accuracy evidence.

## Revisit When

An owner authorizes a shadow source for routed production review (#397). That
promotion should add routed authority through the review-source admission
contract, not reinterpret historical shadow rows as required-review evidence.
