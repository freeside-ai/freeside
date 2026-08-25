# Ready Review-Yield History

Work unit: #839.

## Decision

Chose a creation-immutable `ReviewYieldHistory` sibling to `ReadinessSummary`
over extending the summary or recomputing presentation from live review state.
The summary remains its narrow two-scalar readiness verdict; the history owns
the ordered, per-round yield digest and an explicit terminal outcome. The
engine derives it only from persisted routed-review records, their findings'
cross-round fingerprints, and persisted dispositions when the ready item is
created.

New production ready items always carry a nonempty history. Legacy and fake
publication items may carry `null`: fake publication has no production review
lineage, and backfilling a historical item would make its immutable payload
depend on a later software version. Crash recovery may rederive the expected
history from immutable review records to authenticate an existing ready item,
but it never mutates that item.

## Returned-Object Boundary

Chose a store-owned canonical `yield_history` column beside the attention-item
body over trusting the decoded JSON alone. Structural validation can prove the
history is internally possible, but cannot prove it is the history written at
creation. Every attention-item reconstruction therefore compares canonical
column and body encodings and fails closed if either is stripped or altered.
Lifecycle transitions independently compare canonical history encodings, and
construction plus sync projection clone the rounds slice so callers cannot
mutate a trusted value through aliasing.

The refute-first cases cover body/column disagreement on both single and list
reads, restart preservation, non-ready carriers, invalid totals and outcomes,
unordered rounds, and terminal/final-round disagreement. Swift mock transport
also inserts an explicit `yield_history: null` because synthesized optional
encoding otherwise omits an OpenAPI-required nullable property.

## Yield Semantics

Chose finding fingerprints against the union of prior routed rounds in the
same reviewer-configuration segment over finding IDs or only the immediately
preceding round. Finding IDs are pass-local, while the #702 fingerprint is the
durable semantic identity this metric needs. A fingerprint repeated within the
same round is not recurring until a prior round in that segment has established
it; all current-round fingerprints join the seen set only after that round is
classified. Shadow-review records remain outside the derivation because they
persist in a separate store surface.

An unfingerprintable finding is counted as new in each round and never enters
the cross-round seen set. Declined and deferred findings may validly lack the
location or normalized message required by the fixed-disposition absence
proof, so missing identity cannot block creation of an otherwise-ready item.
Classifying each occurrence as new is conservative: recurrence requires a
positive semantic identity, while a pass-local finding ID cannot supply one.

Recurrence state resets when `ReviewRecord.ConfigurationDigest` changes. That
digest binds the effective reviewer provider, model, auth identity, cost
owner, and execution configuration, so it is the persisted boundary for the
plan's convergence-segment rule: a new reviewer's first pass cannot inherit
recurrence from the reviewer it replaced. Round order and terminal outcome
remain global so the ready item still carries the complete routed history.

Disposition totals stay attributed to the producing round. Their sum may be
less than findings ingested because the digest describes persisted facts, not
a fabricated terminal disposition for every finding.

Revisit when routed review records permit multiple records for one run/round,
finding identity semantics replace `Finding.Fingerprint`, or fake publication
begins running the production review pipeline.
