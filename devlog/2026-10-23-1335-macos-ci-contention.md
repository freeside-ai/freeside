# Take Every Non-Essential Job Off The macOS Runners

Choose fewer macOS jobs per PR and per merge over faster individual jobs,
because the CI wait was runner contention, not test time. The org's free
plan runs at most five macOS jobs at once across every workflow. A daemon
PR requested four of them (one rest shard and three integration shards),
an app PR requested four (style, core-tests, apps, convergence), and every
merge to main requested the same set again through the component push
triggers. Runs queued behind each other, and the 10 to 15 minute waits
the owner reported were mostly that queue: the macOS rest job in the
sharding measurement waited 212 seconds before it ran for 355. Runner size
and paid minutes were off the table, so the lever is the count of macOS
jobs each event requests. The owner asked for the wait to be halved and
accepted this layout on 2026-10-23 (#2248).

A pull request now requests three macOS jobs instead of five: the daemon
rest shard, the convergence job, and the `apps` build. A push to main
requests one instead of five: a daemon job that compiles every test
binary, runs the three integration shards in sequence, and saves the one
macOS build cache. The two decisions behind that, and what they reject:

- **Run the daemon's macOS integration shards after the merge, not on the
  PR.** The darwin-specific code (`internal/atomicfile`, `cmd/freesided`'s
  pairing peer and launchd handling) sits in the rest shard, which still
  runs on every PR; the integration package's macOS delta is timing and
  filesystem behaviour. A darwin-only integration regression therefore
  surfaces as a failed `daemon CI` run on main after its merge, which
  GitHub reports to the account that pushed the merge. That is the whole
  signal: the lander reads a PR's own checks, the landing record names no
  base-advance check, and a later push in a landing batch cancels an
  earlier push's run (`cancel-in-progress` on main), so the batch's last
  tip carries the evidence for the whole batch. Keeping every push's run
  would hold a macOS slot per merge for the same tree. The owner accepted
  that exposure over keeping three macOS jobs on every daemon PR. Rejected:
  keeping the shards on PRs as one sequential job, because one job still
  waits in the same queue and gains nothing a PR needs; and per-shard
  macOS jobs on push, which would hold three slots per merge for the same
  evidence.
- **Make the convergence job the PR's one Swift job.** `style` and
  `core-tests` each built the same Swift package the convergence job
  builds, and `swift test --filter` selects at run time, so the same
  invocation runs every Darwin-only suite at the cost of the extra test
  execution only. The convergence selection is a superset of the app
  selection, so every app PR still gets the tests, and the swift-format
  gate runs first in that job with its Xcode pin on the format steps
  only, so the tests keep the image's default toolchain.

The push run keeps no Swift job. The SwiftPM mirror cache it would have
saved is worth 10 to 20 seconds per PR job, against a five-minute macOS
slot per merge; the `actions/cache` steps on PR jobs save that mirror
anyway when `Package.resolved` changes. The convergence job restores the
daemon's macOS rest build namespace instead of keeping one of its own,
because `macos-main` now fills that namespace with every compiled
package. The platform gate keeps its name on both events and verifies the
shard set declared for the event; a gate that only required success
could not tell a skipped macOS job from a tested one.

This revises two earlier records. `2026-10-19-1115-daemon-ci-sharding.md`
chose three integration shards and one rest shard per platform; the
Linux layout and the evidence model stand, and only the macOS schedule
changes. `2026-08-06-2036-ci-build-input-caching.md` gave each workflow
its own push-time cache writer; the macOS writers are now one job. The
#1877 timing tool still expects per-platform shard jobs in a candidate
cohort, so a timing cohort collected after this change needs a layout
field; that stays with #1877 and #2251.

Revisit when the org's plan or the macOS concurrency cap changes, when a
darwin-only integration failure hands a landing back more than once a
month, or when the measured PR wait no longer tracks the macOS queue.
