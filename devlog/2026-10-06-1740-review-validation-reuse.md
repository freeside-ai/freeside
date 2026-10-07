# Reuse Review Validation Within One Stable Read

Chose to retain successful cross-row validation marks for one SQLite read
snapshot, and for one complete disposition load inside a write transaction.
Repeated filing reads were reconstructing the same disposition, adjudication,
and review joins thousands of times. The pure-Go SQLite driver makes those
queries especially expensive under the race detector. This extends the
[read-snapshot authority decision](2026-09-09-1819-read-snapshot-authority-reuse.md)
for [#1810](https://github.com/freeside-ai/freeside/issues/1810).

## Equivalence Argument

Each lookup still selects its rows. A disposition load still reads the entire
table before filtering, so a forged copied key cannot hide a row. Every read
still checks the row's digest, decodes and validates its body, and compares
copied columns. Only successful checks against other rows can be reused.

Keys include all selected columns and the complete body, not just its digest.
Adjudications and dispositions also include the complete publication-read
ancestry. Review-record checks depend only on rows, so their keys need no
ancestry. A hit checks cancellation and returns a freshly decoded value;
no retained slice or pointer can be changed by a caller.

The other inputs are the transaction's rows and fixed policy. The reachable
reconstruction gates may consult approved recipes, admission policy,
requirement sets, waiver approvals, registry generation, and recommendation
rules. These fields are fixed for the callback. `ReadWithRecipeScope` narrows
recipes before invoking its callback. The migration-only `beforeTasks` flag
is fixed during each load and is false in ordinary `Store.Read` callbacks.
Backup health already retains one
verdict or error per transaction. The caller-mutable independent-review recipe
authorization is confined to readiness proof gating and is not an input to
these validations. The only store context values are the publication-read
links; their full length-prefixed path is part of the key.

`Store.Read` retains marks until its callback ends. Write transactions normally
have no marks. A disposition load temporarily installs marks and removes them
with `defer`, on success or failure. Nested loads share the outer marks and
do not remove them. No production `ReadTx` method issues a write, and the load
does not call a user callback, so no write can intervene before it returns.
A later read in the same write transaction reconstructs all cross-row authority
again. No marks survive a transaction.

Scope and final authority have separate marks. A decision-time boundary may
exclude a row after scope authentication; it cannot mark that row's final
authority as checked. The binding check consumes the scope result locally,
removing the duplicate scope check without retaining a decoded artifact.

## Rejected Options

- Reuse only inside `Store.Read`: it leaves repeated joins inside the
  many write-side loads that seed and advance filing state.
- Reuse between reads in a write transaction, clearing on every write: it
  would require every current and future SQL writer to invalidate authority.
  The read-only call boundary provides a smaller, checkable lifetime.
- Select dispositions by copied keys: it can hide a corrupted row before
  validation. Full-table reconstruction remains required.
- Retain decoded artifacts: their slices and pointers could leak caller
  mutations into later reads. Marks retain no returned object.
- Reduce filer fixtures alone: it would leave production reads paying the
  repeated validation cost. The issue permits fixture reductions only if
  reuse cannot be justified or the measured package still exceeds 600 seconds.
  Reuse alone met the local runtime target, so the existing fixtures were kept.

## Refute-First Findings

The new regression tests disprove reuse across transaction boundaries and
across writes in both public write transaction kinds. They also check newly
inserted dispositions, canceled hits, failed validations, mutable returned
values, publication-path separation, and scope-only decision-time reads.
Execution counters show five filing gates validate five disposition scopes,
one adjudication, and one review record. Each separate write-side load repeats
that work once and leaves no marks behind.

The generated corpus compares first and repeated snapshot reads with write-side
reads across 128 combinations of corrupted rows, two publication paths, and
four decision boundaries. The base-comparison and independent review results
agree with those tests: the original files reconstructed from `fc97f0b4` and
the changed implementation produced identical values and exact error text for
all 1,024 cases. The result digest is
`5c9e0614d4c302843852d3960bafae1e30763770b4528451d074ab578220b7fa`.
This covers the generated corruption corpus, not every possible store state.

A fresh-context static reviewer found no actionable correctness or safety
defect. The review traced the separate scope and binding marks, nested
diminishing-decision loads, publication ancestry, key completeness, and returned
object isolation. Existing store and publication tamper tests remain unchanged.

## Revisit When

Revisit if a validation gains a mutable nontransactional input, a `ReadTx`
method starts writing or calling user code, or one transaction supports
concurrent use. Also revisit retention costs if review history grows enough
that keeping each validated row body for a read snapshot becomes material.
