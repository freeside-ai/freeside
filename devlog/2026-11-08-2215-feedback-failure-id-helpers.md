# Operator-Feedback and Execution-Failure ID Prefixes Live in Domain

Work unit: #2865. Mandatory note: the change touches Tier A paths
(`engine/admission.go`, `engine/remediation.go`,
`engine/production_export_record.go`, `engine/production_publication_task.go`,
`engine/production_workflow.go`). It is the first link of the identity chain
proposed on #2775 and recorded in `2026-11-07-1701-hold-classification-table.md`;
#2866 takes the specification prefixes next.

`"inv-operator-feedback-"` and `"execution-failure-"` are now spelled once, in
`daemon/internal/domain/operator_feedback_identity.go`. Every non-test site in
`daemon/` mints or matches them through its helpers. Stored IDs are unchanged.

## Decision

- **Chose pure prefix predicates over parse-and-validate.** The Tier A gates
  matched with `strings.HasPrefix`, which accepts the bare prefix.
  `HasOperatorFeedbackInvocationPrefix`, `HasExecutionFailureItemPrefix`, and
  `IsOperatorFeedbackExecutionFailureItemID` keep exactly that match, so no
  gate accepts or refuses a different ID. A stricter predicate that rejects
  the bare prefix would be a behavior change at those gates and belongs in its
  own unit, if anyone wants it. Only the marker quarantine site, which already
  rejected the bare prefix, uses the cutting helper `OperatorFeedbackCommandID`.
- **Kept the prefix check from proving a feedback invocation.** A client-chosen
  specification discussion ID can carry the feedback prefix, so the helper's
  doc says callers needing proof also check the stage, as `admission.go`
  already does.
- **Kept the engine wrappers as delegates.** `operatorFeedbackInvocationID`,
  `specificationFailureItemID`, and `specificationRevisionFailureItemID` now
  call the domain helpers instead of being removed, because tests call them
  and the contract left test files unchanged.
- **Kept the two specification revision cards distinct.** The per-iteration
  card (`...spec-revision-<run>-<n>`) and the per-run iterations-exhausted card
  (`...spec-revision-<run>`) get separate helpers; one shape for both would let
  a replay check find the wrong card.
- **Left the new domain file off the Tier A list.** It mints and matches
  strings and grants nothing alone; the gates that use it stay listed.
  `domain/specification_identity.go` is unlisted for the same reason.

## Refute-First Findings

- **Disproved by check: a swapped site matches different IDs.** A throwaway
  differential test compared each old literal expression with its helper over
  two million generated strings, built from prefix fragments, format verbs,
  invalid UTF-8, and extreme iteration values. Every mint and match agreed,
  including the bare-prefix and quarantine-cut cases. Weakening
  `OperatorFeedbackCommandID` to accept the bare prefix made it fail, so the
  check can detect a narrowing or widening. A fresh-context review of the
  diff, comparing each site with `main`, found no site whose match, mint,
  types, or operator precedence changed.
- **Disproved by check: a seeded or golden ID moves.** The seed fixture keys
  its implementation failure card by run ID; it now passes that run ID through
  `ExecutionFailureItemID` unchanged, and the engine, store, and seedfixture
  goldens pass without edits.

Revisit when a prefix gains a second spelling (a legacy family, as the
specification lane has), or when a gate needs to reject the bare prefix.
