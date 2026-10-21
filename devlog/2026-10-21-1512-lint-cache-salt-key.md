# Key the Daemon Lint Cache by Every golangci-lint Salt Input

Chose to cache golangci-lint's directory with `actions/cache` under a key
that covers every input golangci-lint salts its entries with (its version,
`daemon/.golangci.yml`, and `daemon/go.mod`) plus the resolved Go version,
with no fallback across them and saves only from `main` pushes. The rejected
alternative was golangci-lint-action's built-in cache, which falls back to
any entry from the same week. Its fallback produced false staticcheck SA5011
reports on #2116 (Go 1.26.9 to 1.26.10), and would have done the same on
`main` once that PR merged (#2127).

## Mechanism

golangci-lint keeps a Go-style content-addressed cache: action entries
(`-a`) point at output files (`-d`) named by their content hash. Every
action ID is salted with the binary version, the linter settings, and the
`go.mod` content, so an entry saved under other inputs never hits. What
does survive a cross-salt restore are its outputs, and that is the harm:

1. A run under the new salt rewrites outputs with the same content as the
   restored ones (most facts and issue lists don't change with a patch
   toolchain). The writer sees the file already there and skips it, so
   the file keeps the restored entry's old mtime.
2. When the run closes, it trims. Trim runs at most daily and deletes
   entries unused for five days plus an hour. `main`'s entry was six days
   old, so trim deleted those outputs while the run's new action entries
   still pointed at them.
3. The action saves that cache. The next run restores it, misses on the
   dangling entries, and lints with part of its dependency facts reloaded
   and part recomputed. In that state staticcheck reports SA5011 at
   `if x == nil { t.Fatal(...) }` guards, as if `t.Fatal` returned.

Reproduced on GitHub-hosted Ubuntu with the unchanged action: a push run
with `go.mod` at 1.26.10 restored `main`'s week-2963 entry, passed, and
saved a cache in which 3,362 of 3,776 action entries had no output. Its
rerun restored that cache and reported the same three SA5011 sites #2116
did. Not traced: which missing facts make staticcheck drop `t.Fatal`'s
no-return property. A local macOS run with dangling entries did not report
SA5011, so the exact trigger may depend on which outputs trim deletes.

## Why Saves Come Only From Main

Within one key the hazard needs a dependency package whose hash changes.
Dependency hashes cover their source paths and contents, which change only
with the Go toolchain or the module versions in `go.mod`, and the key
carries both. The resolved Go version is in the key, even though `go.mod`
pins the patch today, so an unpinned `go` directive can't reopen the gap.
Daemon packages are always analyzed from source, so dangling entries
for them cost a re-analysis, not a wrong result. With one writer, `main`'s
entry is refreshed on every push, matching the Go build cache's policy in
the same job. PR runs restore `main`'s entry or lint cold; a cold run costs
about 25 seconds more.

## Rejected Options

- **Skip saving on PRs only (`skip-save-cache`).** `main`'s first push
  after a toolchain change still restores across the salt and saves the
  dangling cache that every later run reads.
- **Disable lint caching entirely.** Safe, but every run pays the cold
  cost to avoid a hazard that a correct key removes. (The change does set
  the action's `skip-cache`, but only to hand caching to `actions/cache`.)
- **A shorter `cache-invalidation-interval`.** Fewer stale entries, but the
  fallback still crosses `go.mod` changes.
- **Edit test code to return after `t.Fatal`.** Treats one symptom; any
  other code staticcheck reasons about through dependency facts stays
  exposed.

## Revisit When

golangci-lint changes what it salts or how it hashes dependency packages
(the key must cover every such input),
golangci-lint-action stops restoring across `go.mod` changes, or
golangci-lint refreshes mtimes on identical rewrites.
