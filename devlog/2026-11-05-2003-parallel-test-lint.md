# Lint Daemon Tests for Missing t.Parallel

Issue #2668. The owner split the parallel-test sweep on 2026-11-05: this
unit adds the lint ratchet, and #2676 (ward), #2677 (engine), #2678
(cmd/freesided), and #2679 (store, publish, importer, projectimage, verify)
make those packages' tests parallel. The choices below are the planner's,
carried out here.

Chose `paralleltest` with `ignore-missing-subtests: true` over `tparallel`
because only `paralleltest` stops a new test from defaulting to serial.
`tparallel` flags a test that mixes parallel and serial use, such as a
parallel parent with serial subtests; a fully serial test passes it. The
slow daemon packages are slow because of fully serial top-level tests
(macOS CI run 42080257028: cmd/freesided about 100 s, engine about 98 s),
so `tparallel` would leave the cause unchecked.

## Top-Level Tests Only

Subtests aren't linted. A package's wall time is set by its serial phase,
and only top-level tests make up that phase: Go runs a package's
non-parallel top-level tests one at a time before releasing the parallel
ones. A serial table of subtests inside a parallel test costs that one test
its own time, not the package's. Requiring `t.Parallel()` in every subtest
would add churn across thousands of table tests for no measured gain.

## The Ratchet

Every package with findings when this unit lands is listed in one
`paralleltest` exclusion in `daemon/.golangci.yml`; every other package is
checked. At the branch base (`d4fd9456c`) that was 50 of 63 packages and
3,713 top-level tests. The list only shrinks: a unit that clears a package
deletes its line, and no package is ever added. A new package starts
checked. The path pattern matches files directly in a listed directory, so
a subpackage of a listed package (for example `internal/store/storetest`)
is still checked.

Chose one shrinking list over enabling the linter everywhere with a
`//nolint` on each serial test, because 3,700 directives would be noise
that the sibling units then delete, and each would carry a boilerplate
reason nobody had checked, which hollows out `nolintlint`'s
`require-explanation`.

A top-level test that must stay serial carries
`//nolint:paralleltest // Serial: <reason>` on the line above its `func`,
so the reason sits next to the code that needs it. A test that calls
`t.Setenv` isn't flagged and needs no directive, because Go forbids
`t.Setenv` in a parallel test.

Revisit when the exclusion list is empty (delete the rule), or if a
measured package wall time shows serial subtests mattering after the
top-level drain.
