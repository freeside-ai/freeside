# Save The SwiftPM Mirror From The Linux Generate Job

Chose app CI's Linux `generate` job as the push-side writer of a shared
SwiftPM mirror over a push-only macOS job, because it already runs on every
push to `main` that touches app CI's paths, which includes every
`Package.resolved` change, and fetches every dependency, so the PR's macOS
`convergence` job gets a warm mirror without spending a macOS slot per
merge (#2273). A daemon-only push doesn't rewrite the mirror; while PR
runs keep restoring it, eviction should pass it over, and #2273 records
any miss that eviction causes.

This corrects an assumption in `2026-10-23-1335-macos-ci-contention.md`.
That note held that the PR jobs' `actions/cache` steps save the mirror
anyway. GitHub scopes a PR's save to that PR, though, so those saves
helped only later pushes to the same PR, and every new PR fetched cold.
Its price for the mirror, 10 to 20 seconds per PR job, held up: fetching
and checkouts took a median of 16.6 seconds over ten cold `convergence`
runs. Its decision stands: the push run still keeps no Swift job, since
the writer runs on Linux.

The owner adopted the mirror although it missed #2273's original bar, a
25-second drop in the span from the first fetch to `Building for
debugging...` (39 s median). About 23 seconds of that span is manifest
evaluation and package-plugin compilation, which no source mirror
removes, so no writer could have met the bar. Measured on #2281, fetching
and checkouts fell to 1.7 seconds and the restore cost 5.1, a net saving
of about 10 seconds of macOS time per app PR at no macOS cost. One shared
entry also replaces a roughly 220 MB PR-scoped entry per app PR in a
cache already over its 10 GB budget. The criterion now states that net
saving.

The writer can't be the `linux` job. Its `swift:6.3` container has no
`zstd`, so `actions/cache` falls back to gzip there, and the compression
method and the literal `path` are part of an entry's version: a gzip entry
is invisible to the macOS reader under the same key. Both jobs therefore
cache the workspace-relative `.swiftpm-mirror` and link it into the
platform's SwiftPM cache, with a key that names no OS or toolchain because
bare clones don't depend on either.

Revisit when `generate` moves into a container or off the Ubuntu host,
or when the post-merge measurement on #2273 shows the macOS reader missing
`main`'s entry for a reason other than eviction.
