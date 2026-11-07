# Take The Rest Shard's Inventory From Its Own Run

Chose to derive the rest shard's family inventory from its own `go test
-json` run events over a separate compiled `go test -json -list` pass,
because that pass linked all 62 rest test binaries a second time before the
first test could start. It cost 37 to 85 seconds on every daemon PR, and 11
seconds even with every test binary already compiled (#2743). The owner
set this direction in #2743's contract.

This revises the evidence model in
`2026-10-19-1115-daemon-ci-sharding.md` ("compiled test discovery and raw
execution accounting") for the rest shard only. That note stays frozen as
the record of the earlier choice.

## Why The Run Gives The Same Inventory

Every top-level test, example with an output comment, and fuzz target emits
exactly one `run` event whose name has no `/`. Subtests and fuzz seeds sit
under a `/`. A package with no test files, or with test files but no tests,
emits no `run` event at all. The rest shard runs every package unfiltered,
so these events name the same families `-list` names, minus benchmarks,
which neither inventory keeps. A probe on go1.26 output confirmed each
shape, and the suite's fixtures pin them.

The package set still comes from `go list ./...` through `packages.json`,
so a package missing from the run still fails accounting. The gate
re-derives the rest inventory from the raw `tests.jsonl` instead of
trusting the saved `inventory.json`.

## What This Gives Up

The rest inventory is no longer produced independently of the run it
checks. Before, a family that the run silently dropped would still appear
in the compiled inventory and fail accounting. Now such a family would be
absent from both. The loss is small:

- `-list` runs the same test binary through the same `TestMain`, so a
  `TestMain` that skips tests was never caught by the old inventory either.
- A listed test that never emits `run` in a passing package needs the
  binary to stop early, and `go test` already fails a package whose binary
  exits before its tests finish.
- `compare` still checks shard evidence against an independent compiled
  baseline from an unsharded `-list` pass, but only when someone runs it
  by hand; no workflow runs it.
- A `-run`, `-skip`, or `-list` in `GOFLAGS` would shrink a run-derived
  inventory without failing it, so the rest shard and `verify` reject one.

A failed rest run now leaves no `inventory.json`. Nothing reads one on
failure, since the gate requires every child to succeed first, but someone
debugging a failed rest job from its artifacts loses that file.

## What Stays Compiled

The integration shards and the darwin selection keep compiled discovery:
they need the inventory before the run, to partition families into `-run`
patterns or to check that each selected family exists. The `freesided`
shard, which #2767 carved out of rest while this change was in review, also
keeps compiled discovery. It runs `cmd/freesided` unfiltered, so the same
run-derived inventory would fit it; this change leaves it as #2767 built it.

Revisit when CI needs to filter the rest run, when the rest shard splits
into several commands, or when a Go release changes test2json so a
top-level test can finish without a `run` event.
