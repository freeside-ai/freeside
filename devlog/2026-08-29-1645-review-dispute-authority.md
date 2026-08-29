# Authenticate Review Dispute Card Facts

Issue: #724

Chose to authenticate every populated `ReviewDisputeBinding` against an
immutable routed `ReviewRecord` or observation-only `ShadowReviewRecord` on
persistence and on both attention-item read tiers. The binding's run and round
must resolve to the routed candidate, then its finding IDs and completion
evidence must exactly match one authority lane's record. Routed and shadow
finding membership is structurally disjoint, so the exact match identifies the
authority without adding a wire discriminator. Finding order is
presentation-neutral, so the gate compares set membership rather than
requiring the binding to copy canonical record order.

The implementation plan assumed the new body-only facts needed no store
re-gate. Automated review identified a reachable counterexample: a caller can
submit, or a stored JSON body can be changed to contain, locally valid but
invented review coordinates. Shape validation cannot make copied authority
authentic. The immutable review records already carry the complete authority,
so no lookup column or migration is needed.

Rejected shape validation alone because it would let fabricated daemon-authored
facts cross the sync boundary. Rejected gating only current snapshots because
terminal recovery would still reconstruct corrupted immutable history.

Automated review also proposed re-gating every other new card fact. Declined
for this contract unit: no production caller populates those fields, and none
copies an immutable store authority comparable to `ReviewRecord`. Issue #1003
owns their source derivation. Re-gating creation-time snapshots against mutable
current state would invalidate history; detecting arbitrary rewrites of
body-only presentation data would instead require an aggregate-integrity
contract and migration, not per-field joins in this carrier change.

Follow-up: #1003

Revisit when a review-dispute card is intentionally allowed to name a strict
subset of a review round's findings, when review completion authority moves
away from `ReviewRecord`, or when #1003 adds a stable immutable source record
for another card fact.
