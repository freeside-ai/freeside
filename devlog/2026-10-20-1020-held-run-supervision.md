# Expose Holds Separately In Supervision Snapshots

Chose an optional `hold_reason` in `freesided follow -snapshot` over a new
lifecycle state or a broader attention list, following the planning decision
in [#2045](https://github.com/freeside-ai/freeside/issues/2045). A run can stay
`pending` while an admission hold appears or clears. Its own attention items
cannot explain a blocking system-health notice belonging to a prior run.

The existing observation already carries the hold in the snapshot's read
transaction. Projecting its closed reason code preserves that evidence
without another read or any free-form text. `outcome_reason` keeps its
terminal meaning; a temporary hold is a separate fact.

The supervisor recomputes its pause condition on every readable snapshot:
either a visible hold or an existing human gate pauses the same executing-time
accumulator. Missing `hold_reason` means no visible hold for older snapshots.
Unreadable observations resume charging, and liveness and terminal handling
continue while held. No second budget or hold-policy change is needed.

Revisit when the snapshot consumer or hold lifetime changes, including the
planned harness retirement in #2081.
