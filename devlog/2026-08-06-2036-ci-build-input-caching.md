# CI Build-Input Caching: What the Caches Actually Buy

Work unit: #539 (parent tracker #541).

## The Measurement That Changes the Model

**app-ci's cost is compile-bound, not fetch-bound.** #539 was filed on
the premise that re-cloning twelve SwiftPM dependencies dominates
`convergence-ci` and `app-ci`, citing the package job's 171s "Verify
generated API client" step. Measured from the job logs, the whole
dependency-setup phase (first fetch line to last working-copy line) is
**9.4s** in the package job, **21.0s** under `xcodebuild` in the apps
job, and **15.1s** in convergence. The rest of that step compiles
`swift-openapi-generator`, which the unit's non-goal deliberately
excludes from caching.

Caching the mirrors drops that phase to 0.6s / 1.6s / 3.1s, against a 1
to 8s cache-restore step. So the SwiftPM half of this unit is worth
roughly 5 to 15s per job, and over four steady-state runs only the
`apps` median moved outside runner noise (186s to 154s); `package`
(188s to 192s) and `convergence` (170s to 166s) did not. Their per-run
spread across runs with identical cache hits is 154 to 232s, which
dwarfs the saving.

The Go half is where the win is: `linux` 118s to 70s, `macos` 161s to
89s, because `setup-go`'s go.sum-only key meant the daemon's own
packages recompiled on every run.

**Consequence for anyone re-reading #541:** its "the same cost
dominates app-ci" line overstates the fetch component. Narrowing
`convergence-ci`'s path trigger, which #541 declined on
break-detection grounds, would still be the larger lever there, and
caching would not have substituted for it as #541 assumed.

## Decisions

**Chose two Go cache entries over one, rejecting the issue's literal
single `actions/cache` over both paths** (agent judgment, on
measurement): the module cache is ~80 MB and changes only with
go.sum, so folding it under the rolling per-commit key would re-upload
it on every push, and that upload lands inside the job wall-clock the
unit exists to cut.

**Chose `go env GOCACHE`/`GOMODCACHE` over the issue's literal
`~/.cache/go-build`**: that path is Linux-only and two of the three Go
jobs are macOS (`~/Library/Caches/go-build`), where the literal would
have cached nothing while still appearing configured.

**Chose to cache SwiftPM in the `apps` job too**, beyond the two the
acceptance names: its `xcodebuild` steps re-cloned the same twelve
packages, and it turned out to be the only Swift job whose median
moved.

**Kept the shared `go-mod` key across same-OS Go jobs**, accepting one
benign `Failed to save: Unable to reserve cache with key ...` per
go.sum change, rather than tripling storage for three identical
entries.

**Storage cost accepted, not mitigated.** The rolling key adds ~240 MB
per push (three build-cache entries) against a 10 GB repo budget.
GitHub's LRU eviction discards the coldest per-commit entries first,
which are exactly the disposable ones, and a miss costs a cold run,
never a wrong result.

## Verification Findings

Both correctness gates were run as a throwaway commit on the PR branch
and reverted:

- A changed `app/Package.resolved` (swift-algorithms re-pinned 1.2.1 to
  1.2.0) changed the key, fell back through `restore-keys`, resolved
  all twelve packages from the local mirror with zero network fetches,
  built green, and saved a refreshed entry.
- A changed daemon source (`authorizationEncodingVersion`) in a run
  that **restored** the prior commit's 88 MB build cache still
  recompiled: `TestGolden/candidate_authorization` failed, which is the
  outcome a stale object would have hidden.

Incidentally confirmed: `actions/cache` does not save on a failed job,
so red runs never populate the cache namespace.

Revisit when: the SwiftPM mirror cache stops paying for its restore
step (if the restore grows past the ~9 to 21s acquisition phase it
replaces), or if `app/.build` caching is reconsidered, in which case
the compile-bound finding above, not the fetch cost, is the number that
matters.
