# Cache The Exact Swift Build For Daemon-Only PRs

Chose a push-time macOS job that saves `app/.build`, test bundle included,
under a key that names the git tree of `app/`, over keeping the push run
free of Swift jobs, because the convergence job was the sole bound on
every daemon PR and about 80 percent of it compiled app code the PR did
not touch (#2291). On an exact hit for a run without the app component, the job runs
`swift test --skip-build`. The owner accepted the trade on 2026-10-24: one
macOS build of about 5.2 minutes per app-path merge to `main`, against a
daemon-only convergence job that took 377 to 387 seconds on a miss and a
median of about 85 seconds on a hit, over eight warm runs. The restore
of the 409 MB entry (1.0 GB on disk) took 7 to 13 seconds. App PRs miss by
construction and never skip the build, so they are unchanged.

This revises `2026-10-23-1335-macos-ci-contention.md`, whose push run
kept no Swift job because the only push-side cache on offer was the
SwiftPM mirror, worth 10 to 20 seconds per PR job against a macOS slot
per merge. A build cache keyed on the whole tree is worth about five
minutes per daemon PR, and the writer runs only on pushes in app CI's
paths, so it costs a checkout and a lookup when `app/` did not change.

It also answers the stale-artifact objection that kept `app/.build` out
of `2026-08-06-2036-ci-build-input-caching.md` and #2249's cache. #2249's
check showed a warm `app/.build` recompiles exactly the changed file, but
keyed on `Package.resolved` alone it still rescanned and recompiled every
project module and saved under a minute, so there is no fallback key
here. This key never serves an entry for a different `app/` tree: a
one-line change to an `app/Sources` file changed the key, missed, built,
and passed. The tree is `git rev-parse HEAD:app`, not
`hashFiles('app/**')`, which hashes contents without names, so a pure
rename could keep the key while changing the build. The harness is still
built from the PR's own `daemon/` on every run.

The key names the image OS, the default Xcode's build number, and its
`swiftlang` token, not `$ImageVersion`. The contract named the image
version, but GitHub rolls the `macos-26` image about weekly, usually
keeping Xcode, and during a rollout runs land on either image: under that
key two of six warm runs missed on an identical toolchain. Keyed on the
Xcode build instead, runs on both `20261013.0424.1` and `20261021.0445.8`
hit the same entry. The owner chose this on 2026-10-24.

The cache was at its 10 GB budget at planning (10,090 MB). The entry adds
about 410 MB per app-path merge and evicts the least recently used
entries, which every daemon PR's restore keeps this one ahead of.

One trust gap stays open: `actions/cache` restores from a PR's own scope
before `main`'s, so a PR that once ran a workflow saving a fake
`app/.build` under the exact key, then reverted it, could pass the
convergence gate on its final daemon-only diff. Closing it needs an
`actions: read` check that the PR scope holds no entry for the key.
Collaborators are trusted and the repository has had no fork PR, so this
change does not add one; the owner's review of each PR remains the
gate.

The fallback, a separate SwiftPM package for `FreesideConvergenceTests`,
was not built: it saves about a minute and nothing on a hit.

Revisit when an Xcode or toolchain change goes long without an app-path
merge to refresh the entry, when eviction starts missing it, when GitHub
changes the runner workspace path the entry's absolute paths assume, or
when the macOS concurrency cap changes.
