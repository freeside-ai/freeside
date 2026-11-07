# Dispatch Hold Classification Becomes One Table

#2775 replaces the hand-kept sentinel lists in the dispatch path with one
ordered table, `holdClasses` in `daemon/internal/engine/hold_classification.go`.
Every classifier the lanes call (`dispatchHoldReason`,
`admissionRefusalDetail`, `invocationDispatchHold`,
`unattendedDispatchRefusal`, `MutableAdmissionPolicyRefusal`, the
operating-gate checks, the clear-on-start causes, and `refusalHoldReasons`)
now reads it. Behavior is unchanged: a characterization golden,
`testdata/hold_classification.golden`, landed against the old code and is
byte-identical after the switch. It is the first link of a chain; the plan
comment on #2775 proposes links 2 and 3, and this unit files neither.

## Decision

- **Chose one row per sentinel over one list per classifier.** A row names
  the sentinel, its dispatch action (operating gate, hold the invocation,
  hold the pass, or hold the pass as a mutable admission-policy refusal),
  its hold reason, its admission check, and whether starting the invocation
  clears its hold. Adding a hold is now one row; #2490 edited four lists in
  this package alone. The old guard list, `holdClassifiedSentinels`, had
  already drifted: it left out three sentinels the dispatch path held
  quietly, covered only because `dispatchHoldReason` happened to map them.
  The guard tests now iterate the table, so a held sentinel can't fall out
  of them.
- **Chose two resolution rules, because joined errors need both.** The
  reason and the admission check come from the first row that matches and
  has a reason, in table order, exactly as the old `switch` statements
  resolved them. The quiet-hold predicates match when any row of their
  action matches, as the old `||` chains did. One rule for both would
  change behavior: a fenced error joined with a capacity error must still
  hold the invocation (any match) and still record `machine_capacity`
  (first reasoned match).
- **Kept the fenced rows reasonless and last.** `store.ErrTaskCancellationFenced`
  and `store.ErrTaskWorkflowClosed` hold the invocation but mean "no more
  work", so no operator waits on them and they record no reason. They sit
  at the end so a reasoned cause joined with one still records its reason.
  The coverage test allows no other reasonless row.
- **Ordered the admission block by the old check order, not the old reason
  order.** Every row in it records `admission_policy_refused`, so its order
  decides only which check a joined error names. `ErrProjectImageUnserved`
  stays first with no check, and `ErrAgentNotAdmissible` stays last so a
  more specific cause names the check.
- **Kept `HoldIdentityParallelism` as a named exception** in the
  clear-on-start set. No sentinel produces it any more; a stored row clears
  with the pool hold that replaced its reason.

## What Stays Outside the Table

- **Typed hold details** (`holdDetailFor`). They come from `errors.As` on
  typed errors, and each needs its own field, pacing, and logging code, so a
  row can't own them.
- **`reviewCapacityWait` and `reviewCapacityRefusal`** in
  `production_review_gate.go`. They repeat the capacity class, but the file
  is Tier A; folding them in is a separate, Tier A unit.
- **The six invocation lanes** in `dispatchPendingInvocations`. Only the
  predicates they call changed. No lane has been added since 2026-08-31.
  Revisit when a seventh is scheduled.
- **#1646's specification skip.** The specification lanes skip pending
  intents with a bare `continue` when no specification driver is configured.
  No error is involved, so no sentinel table can see it. Fixing it needs a
  new hold reason or an attention item, which #1646 decides; it stays open.

## Chain

1. #2775: hold classification as one table (this note).
2. One definition each for the `"inv-operator-feedback-"` and
   `"execution-failure-"` ID prefixes. Tier A, because several call sites
   are.
3. The same for the specification family (`"spec-feedback-"`,
   `"spec-approval-"`, `"spec-discussion-"`), after link 2 to reuse its
   helper shape.

The planner files links 2 and 3; the plan comment on #2775 has their site
counts.
