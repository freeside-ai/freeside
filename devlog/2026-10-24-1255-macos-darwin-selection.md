# Run Only The Darwin Selection On macOS PRs

Choose a declared darwin selection on macOS PRs over the full rest shard,
and move the full rest shard to `macos-main` after the merge. The full
shard repeated Linux and held a macOS slot for 6 to 7 minutes per daemon
PR, while the evidence shows it never caught a darwin bug that Linux
missed. The owner decided this on 2026-10-24 (#2274).

The evidence against the full shard on PRs:

- **No darwin failure on PRs.** In 139 PR runs from 2026-10-14 to
  2026-10-24 with a macOS rest result, the macOS rest shard never failed
  when the Linux rest shard passed.
- **Earlier macOS-only failures were timeouts.** About 11 daemon CI runs
  from 2026-09-24 to 2026-10-21 failed only on macOS. Every readable log
  was a 10-minute `go test` timeout in the integration or publish package,
  or a cancelled integration shard, not a darwin behaviour bug.
- **The shard repeats Linux.** It ran the same 5,419 non-integration tests
  as the Linux rest shard, about 1.25 times slower; 4,225 of them take
  under 0.1 s.
- **Little daemon code is darwin-specific,** and local development runs on
  macOS, so those paths also run before each push.

The selection lives in `scripts/daemon-test-shards.py` as
`DARWIN_SELECTION`: whole packages where they are cheap
(`internal/atomicfile`, `internal/operations`, `cmd/freeside-signet-dev`,
`internal/export`) and named top-level test families in the packages too
slow to take whole (`cmd/freesided`, `internal/publish`, `internal/importer`,
`internal/ward`, `internal/verify`, `internal/exec/claude`). Each entry
names its reason: a build tag, case-insensitive APFS or the `/var` to
`/private` link, or loopback bind rules. The shard fails closed when a
listed package or family is missing, so a rename updates the list rather
than silently shrinking it. Rejected: selecting whole packages, because
`cmd/freesided` (213 s) and `internal/publish` (117 s) alone take longer,
run in parallel, than most of the 318 s test step; and deriving the set
from build tags alone, which misses the filesystem and network behaviour
that differs on macOS without a tag.

This revises `2026-10-23-1335-macos-ci-contention.md`, which kept the full
rest shard on every PR because it held the darwin-specific code. Its
integration-shard decision stands: the three integration shards still run
only after the merge, and `macos-main` stays the one macOS job a push
requests and the one macOS cache writer. That job now also runs the full
rest shard, so the macOS gate verifies full default coverage on a push
and the declared selection on a PR. The accepted exposure is the one the
owner already took for the integration shards: a darwin-only regression
outside the selection surfaces as a failed `daemon CI` run on main after
its merge. That includes a test file outside the selection that compiles
on Linux but not on darwin: the PR job builds the daemon and the selected
test binaries only, so such a break fails `macos-main` at its compile step,
which then withholds the push's macOS evidence until a fix lands. Local
development on macOS compiles those tests too. Adding a darwin-sensitive
test means adding its family to the selection; nothing enforces that.

Revisit when a darwin-only failure on main lands more than once a month,
or when the org's plan or the macOS concurrency cap changes.
