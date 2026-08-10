# Shared Path-Fold Primitive

Issue [#567](https://github.com/freeside-ai/freeside/issues/567) identified a
prospective fail-open risk: the importer and verifier separately implemented
the same filesystem-alias and path-matching decisions.

Chose a neutral `internal/pathfold` leaf over either package owning shared
logic because both trust boundaries must consume one decision body. The leaf
contains only standard-library and `x/text` dependencies. It preserves the
existing NFC plus Unicode full fold, HFS-ignorable stripping, NTFS alias
normalization, glob matching, and lowercase SHA-1 validation rules. Chose the
`pathfold` name despite the SHA validator riding along because path handling
is the package's dominant concern and splitting one unrelated validator into
a second new package would add abstraction without another consumer shape.

Kept only the two compatibility forwarders named by the issue:
`importer.CheckoutFoldComponent`, consumed by `publish`, and importer's
unexported `matchAny`, pinned by its policy test. The implementation plan was
verified against `main` at `b2d6f77`, but current base `cdf5b66` contains a
later `collision_test.go` assertion that called the removed
`foldedComponents` helper directly. Chose to migrate that assertion to
`pathfold.FoldPath` rather than retain a third forwarder, preserving the
stronger structural invariant that no extra package-local decision surface
remains. This is the sole revision to the issue's assumption that existing
tests could stay byte-unchanged. The issue's broad acceptance grep also finds
the pre-existing `export.commitPlanPathFold`, which folds JSON field names and
is unrelated to repository-path decisions; the decision-body definition sweep
therefore uses the named functions in addition to recording that raw grep.

A temporary refutation harness reconstructed the old implementation and
compared it with the leaf over 27 adversarial components, 756 one- and
two-component paths, 10,584 folded and exact `MatchAny` decisions, 15 glob
patterns, and seven SHA inputs. Every old/new decision and validation error
matched. The harness was removed after the pass; the permanent single-table
test retains the boundary and alias cases that define the contract.

Revisit when path alias semantics change, or when unrelated validators grow
beyond the single SHA-1 shape required by both consumers.
