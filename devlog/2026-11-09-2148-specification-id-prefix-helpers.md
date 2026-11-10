# Specification Feedback, Discussion, and Approval ID Prefixes Live in Domain

Work unit: #2866. Mandatory note: the change touches Tier A paths
(`store/entities.go`, the spec revision item gate, and
`store/production_attempt.go`). It is the second link of the identity chain
proposed on #2775 and recorded in `2026-11-07-1701-hold-classification-table.md`,
after #2865 (`2026-11-08-2215-feedback-failure-id-helpers.md`), whose helper
shape it reuses.

`"spec-feedback-"`, `"spec-discussion-"`, and `"spec-approval-"` are now
spelled once, in `daemon/internal/domain/specification_identity.go`. Every
non-test site in `daemon/` mints or matches them through its helpers. Stored
IDs are unchanged, and no gate accepts or refuses a different ID. This is the
family where the specification rename's review found a release lookup under
the wrong prefix (`2026-09-01-2256-specification-vocabulary.md`).

## Decision

- **Chose plain prefix predicates plus rejecting cut helpers, as #2865 did.**
  The literal `HasPrefix` sites accepted the bare prefix, so
  `HasSpecificationFeedbackArtifactPrefix` and
  `HasSpecificationDiscussionArtifactPrefix` still do. Every site that cut a
  prefix also refused an empty remainder, so `SpecificationFeedbackCommandID`
  and `CutSpecificationApprovalPrefix` fold that refusal in. A plain cut that
  left the empty check at each caller would keep one more thing to forget at
  the Tier A gate.
- **Gave `CutSpecificationApprovalPrefix` a string, not an `ItemID`.** The
  spec revision gate cuts both a whole item ID and the item ID with its
  iteration already removed, which is not an item ID.
- **Rewrote the gate's bare-run check by value, not by a new helper.**
  `gateSpecRevisionItem` refused `currentPrefix == "spec-approval-"`. Given
  the iteration suffix it has just cut, that holds exactly when the item ID
  is `SpecificationApprovalItemID("", iteration)`, so the gate compares with
  that instead of exporting the bare prefix. The refusal stays at the same
  point, before the claim gate runs. An earlier refusal through the cut
  helper was rejected: it would refuse `spec-approval-<n>` at this check
  instead of at the later run-ID cut, so the claim gate's error could replace
  today's.
- **Rebuilt the blocked-wait terminal's expected approval ID from its
  parts.** After the iteration cut, `spec-approval-<implementationIteration>`
  and `SpecificationApprovalItemID(<run>, <iteration>)` are the same bytes.
- **Added a refusal to `readSpecRevisionComment`.** It trimmed the feedback
  prefix without checking it. It now refuses an ID with no command. That
  refuses nothing new: its one caller, `buildSpecRevision`, runs only after
  `validateSpecificationAddressals`, which already refuses an unprefixed or
  bare feedback ID.
- **Kept the engine wrapper `specDiscussionArtifactID` as a delegate.** Tests
  call it, and the contract left test files unchanged.
- **Named the discussion helpers with "Artifact".** `SpecificationDiscussion`
  `InvocationID` and `CommandID` already name the discussion invocation, a
  different ID for the same command.
- **Left `specification_identity.go` off the Tier A list.** It mints and
  matches strings and grants nothing alone; the gates that use it stay
  listed.

## Refute-First Findings

- **Disproved by check: a swapped site matches different IDs.** A throwaway
  differential test compared each old literal expression with its helper
  over 116,159 generated strings, built from prefix fragments, separators,
  digits, NUL, and non-ASCII bytes, at iterations from `math.MinInt` to
  `math.MaxInt`. It covered every mint, both predicates, the envelope ID, the
  addressal cuts, the gate's header checks on arbitrary and well-formed item
  IDs, the prior-item and run-ID cuts, and the blocked-wait terminal match.
  Every result agreed. Weakening `CutSpecificationApprovalPrefix` to accept
  the bare prefix made both it and the committed helper test fail, so the
  check detects a widening.
- **Disproved by trace: the `readSpecRevisionComment` refusal changes an
  outcome.** The differential test also confirmed that every ID the new
  refusal rejects is one `validateSpecificationAddressals` rejects first. A
  fresh-context review traced the one call path
  (`acceptSpecification` to `buildSpecRevision`) to the same conclusion, and
  found no site whose minted bytes, match, refusal point, or types changed.
- **Allowed by decision: no committed gate test for the empty-run ID.** The
  review noted that no store test feeds the spec revision gate
  `spec-approval--<n>`, a gap this unit found, not made. The contract leaves
  existing tests unchanged, so the helper test pins only the empty-run ID the
  gate compares with.
- **Disproved by check: a seeded or golden ID moves.** The engine, store,
  signet, and seedfixture goldens pass without edits.

Revisit when one of these prefixes gains a second spelling, or when a gate
needs to reject the bare prefix.
