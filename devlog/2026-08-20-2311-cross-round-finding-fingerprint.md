# Cross-Round Finding Fingerprint

Work unit: #702. Mandatory note: `kind:contract` change to the
ReviewSource normalized-finding contract and the §7 fixed-disposition
completion semantics.

## Decision

The cross-round semantic finding identity is a **deterministic,
recompute-on-demand fingerprint over the immutable persisted finding
fields**, not a stored field and not a model judgment. `Finding.Fingerprint()`
hashes, under an embedded `fpv1` version tag, the review `Source`, the
`Location.Path`, and the whitespace-normalized `Message` (trim, then collapse
each internal whitespace run to a single space; no case folding), rendering
`fpv1-<24 hex>`. `FindingIdentityAbsent(prior, current)` is the §7
fixed-disposition primitive: a finding is a `fixed` candidate only when its
identity is absent from the remediation review.

Excluded by design, each because it legitimately changes across the same-base,
different-head remediation rounds of one unit (or is the per-invocation key the
Non-goals preserve): `InvocationID`/`RunID`, `BaseSHA`/`HeadSHA`, `FindingID`,
`Severity` (a round may re-tag), the line range (edits shift lines), and
`CreatedAt`.

Fail-closed: no fingerprint (`ErrUnfingerprintableFinding`) when `Location ==
nil`, `Path == ""`, or the normalized `Message` is empty. A finding with no
computable identity can never satisfy the absence proof, so it is never
declared fixed — the safe direction. `FindingIdentityAbsent` validates every
current finding before deciding, so an unfingerprintable current finding fails
the comparison closed regardless of where it sits (a match before it cannot
mask it).

This unit lands the identity vocabulary, the comparator primitive, and the
fixtures only. No engine wiring: the absence proof's consumers are the wave-6
engine units (#840/#842) and the disposition-aware completion check.

## Rejected alternatives

1. **Reviewer-assigned identity** (feed prior-round findings to the reviewer,
   have it tag matches): makes `fixed` depend on model output, which §7/§5.13
   forbid ("its output is … never … proof that a finding is fixed"), and
   contaminates the fresh-context review with prior rounds.
2. **Repointing `FindingID`** (dropping invocation/head from its hash): the
   issue's Non-goal; it would also collide the immutable per-round raw-finding
   rows keyed by ID.
3. **A persisted fingerprint field**: forces a migration, store/domain golden
   regeneration, and cross-binary-version skew (rows fingerprinted under
   different derivation versions could mismatch spuriously). A pure derivation
   recomputes both rounds under one version and covers pre-existing rows for
   free.
4. **Including line range or severity**: both change across remediation heads,
   recreating exactly the per-round instability this unit removes.

## Verification finding: where the empty-explanation exclusion is enforced

The plan's "codex_local structurally cannot emit such a finding" premise is
correct, but not for the reason the plan's parenthetical implies. The ward JSON
schema (`daemon/internal/ward/codex_review.go`) requires `explanation` *present*
as a string but sets no `minLength`, and `normalizeCollection` applies no
non-empty check, so an empty-string explanation is schema-valid and survives
normalization with `Message == ""`. The exclusion is enforced one step later:
the production source calls `outcome.Validate()` immediately after
`normalizeCollection` (`codex_review_source.go:646`), and
`exec.ReviewResult.Validate` rejects any finding with an empty `Message`
(`daemon/internal/exec/review.go:258`), converting the payload into a
`ReviewFailureContradiction`. So `codex_local` never emits an
unfingerprintable finding through its full path — a Codex review-loop finding
correction, not caught until PR review.

Consequence for this unit: the fingerprint's empty-message fail-closed branch
is unreachable from `codex_local` (Validate excludes it upstream), but it stays
as a domain-level safety net for any ReviewSource with weaker Message
guarantees (native review, the future Claude shadow source #845). The ward test
drives through the validated source path (`normalize` + `outcome.Validate`) and
asserts the empty-explanation payload is rejected as a contradiction, not that
it "survives"; the earlier direct-`normalizeCollection` assertion was
misleading because it skipped the production Validate gate.

## Recorded fail-safe limitations

Both are directional: the derivation may conflate or split findings in edge
cases, but always over-reports *not-fixed* and never declares a persisting
defect fixed, so publication safety holds while loop liveness degrades toward
the emergency bound (a durable AttentionItem, never a silent bad publish).

1. **Reworded re-emission under-matches**: a genuinely reworded explanation of
   the same defect gets a new fingerprint and enters as a fresh finding rather
   than a failed fix. Only the recurring-vs-new signal degrades.
2. **Duplicate same-message findings in one file conflate** (Codex P2, PR
   #870): two distinct findings sharing source, path, and normalized
   explanation but differing only in line range collapse to one fingerprint,
   because the line range is deliberately excluded. If one is fixed and the
   other persists, the fixed one reads as still-present and cannot take its
   `fixed` disposition — the loop over-remediates. No deterministic
   line-independent disambiguator exists (any ordinal/count scheme breaks when
   the occurrence count changes between rounds), and the line range cannot
   return without recreating the per-round instability this unit removes. This
   does not violate the §7 property (a re-emission never becomes a fresh
   finding that strands the original); it only over-blocks.

Revisit when #844/#846 convergence telemetry shows material recurrence or
duplicate-conflation misclassification, or if the owner wants a severity-stable
identity instead of the severity-excluded derivation chosen here.
