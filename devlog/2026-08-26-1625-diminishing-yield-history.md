# Diminishing Review Uses Typed Yield History

Chose to extend the existing typed `AttentionItem.YieldHistory` carrier to
`review_diminishing_returns` over summarizing review yield in the untyped
`Reason` field. The decision card needs the ordered per-round counts that
explain why review has converged, and the typed carrier already validates those
counts, preserves their order, and makes the full history immutable across item
transitions. Putting the same facts in prose would make each daemon and client
consumer parse a lossy summary and could let the displayed explanation diverge
from persisted review records.

The wire and persistence shapes do not change. Exactly
`ready_for_final_review` and `review_diminishing_returns` may carry the optional
history; nil remains valid on both so older records and diminishing items
created before #844's producer lands still reconstruct. Every other item type
continues to reject the field.

The macOS decision card intentionally omits the inline details module and
places those facts in its inspector. Therefore, the canonical diminishing card
baselines stay unchanged when the typed history arrives; the screenshot matrix
pins an expanded diminishing inspector instead. This preserves the intended
visual proof of the per-round rows without making the test-only phone proxy
pretend to compile the iOS details path.

Revisit when review convergence needs a structurally different digest from the
per-round finding and disposition counts shared by readiness and diminishing
returns.
