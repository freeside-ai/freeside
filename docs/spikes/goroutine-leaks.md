# Goroutine Leaks in Daemon Tests

- Status: complete
- Tested: 2026-11-08
- Commit: `46492303e35a4ebbb071b66544bbf1ad1c1424a1` (`main`)
- Go: 1.27.3 darwin/arm64, the toolchain `daemon/go.mod` names
- Host: Mac Studio, Apple M1 Max, 10 cores, 32 GiB, macOS 27.2
- Issue: #2803

## Conclusion

**The daemon's tests leak one goroutine, and it's in test code.** Across 61
measured packages, the `goroutineleak` profile reported a single leaked
goroutine: a `database/sql` connection opener that
`TestApproveShadowReviewCommandTwoPass` (`cmd/freesided`) strands by never
closing a read-only store. It reproduced in all three runs. No production
leak was found, so no `kind:fix` issue was filed.

**Recommendation: no permanent check.** A leak check in every package's
`TestMain` costs little (a median 2.2 ms per test process, 31 ms at most), but
one test-only leak in 61 packages doesn't repay 61 hook files and the
`TestMain` collisions they bring. The profile also can't see the pattern this
run found more of: 16 connect proxies in `internal/ward` that tests leave
blocked in `Accept`, which is I/O wait. Rerun the measurement below instead
when a unit adds a long-lived goroutine owner (a new long-poll, server, or
background loop), or before a release.

## Method

1. A scratch branch, never pushed, added `zz_leakprofile_test.go` to each of
   the 61 packages with tests (Reproduction below). Its `TestMain` runs
   `m.Run()`, then writes `pprof.Lookup("goroutineleak").WriteTo(w, 2)` to
   `$FREESIDE_LEAKPROFILE_DIR`, timing the call, then exits with the saved
   code. `internal/store` already has a `TestMain` (`main_test.go`), so it got
   the writer only, called between `m.Run()` and `os.Exit`.
2. The method adds no GC. `WriteTo` for this profile calls
   `runtime_goroutineLeakGC` first (`writeGoroutineLeak` in `runtime/pprof`),
   a full GC cycle that marks unreachable blocked goroutines leaked, and then
   reads stacks.
3. `debug=2` prints every live user goroutine, tags a leaked one
   `[<wait reason> (leaked)]`, and keeps its `created by` frame. `debug=1`
   prints only leaked goroutines, grouped, but without the `created by`
   frame, so it can't name a creation site.
4. A calibration test that started `go func() { <-ch }()` on an unshared
   channel was reported `[chan receive (leaked)]` with the test as creator,
   then deleted.
5. One full run, then two reruns of the two packages with findings, with
   `GODEBUG=tracebackancestors=10` so each stack also carries its creator's
   stack at spawn time, which names the test.

The measurement is per test binary, after all its tests: a leak names its
creation site, not the test that leaked it, unless ancestors are traced.

## What the Profile Can't See

A goroutine is reported only when it's blocked on a channel operation,
`select`, `sync.Mutex`, `sync.RWMutex`, `sync.Cond`, or `sync.WaitGroup`, and
the GC finds the channel or lock unreachable from any runnable goroutine or
global (`isMaybeRunnable` in `runtime/mgc.go`). So a clean package means "no
unreachable blocked goroutines", not "no leaks":

- **I/O wait, syscalls, and `time.Sleep` are never candidates.** A stuck
  network long-poll or an unclosed listener's `Accept` loop is invisible.
- **Globally reachable objects keep a goroutine alive.** One blocked on a
  channel or lock that a package-level variable still reaches isn't reported.
- **Durable `testing/synctest` waits are excluded.** The 14 packages that use
  `testing/synctest` already fail a bubbled test that leaves blocked
  goroutines behind; the profile adds coverage outside bubbles.
- **A goroutine that hasn't parked yet when the leak GC runs is missed.** A
  test that starts a goroutine and returns at once can race the profile
  write. With the calibration test and `GOMAXPROCS=1`, 8 of 20 runs reported
  the goroutine as a plain `[chan receive]` because it parked after the GC.
  At the default `GOMAXPROCS`, a lone test binary caught it 40 of 40 times,
  but one `go test` of two packages at once missed it too.
- **Nil-channel sends and receives and an empty `select {}` are always
  reported,** reachable or not.

## Coverage

- **Measured:** 61 of 65 packages, every package with tests. The four
  without tests are `internal/exec/contract`, `internal/exec/fake/forge`,
  `internal/inference/contract`, and `migrations`. None was skipped. Test
  binaries that re-exec themselves (`cmd/freesided` five times,
  `internal/projectimage` once) ran `TestMain` again, giving 67 profiles.
- **Not built on macOS:** `cmd/freesided/setup_process_linux_test.go` and
  `internal/claudeinference/fork_linux_test.go`. Linux would instead skip
  `cmd/freesided/setup_process_other_test.go`,
  `internal/procbound/killgroup_darwin_test.go`, and
  `internal/verify/procroom_darwin_test.go`.
- **Not run:** the opt-in live suites, including all 23 `*_live_test.go`
  files; no live-suite variable was set.
- **Suite result with hooks:** passed, 61 packages `ok`. Wall time 25 min 31
  s, about 10 min of it queued for a test slot behind other checkouts' runs.

## Results

| Package | Leaked | Created by | Runs |
| --- | --- | --- | --- |
| `cmd/freesided` | 1, `[select (leaked)]` | `database/sql.OpenDB` | 3 of 3 |

The leaked goroutine is `(*DB).connectionOpener`. Its ancestor stack:
`store.openReadOnlyDB` (`internal/store/store.go:491`) <
`store.OpenReadOnly` (`store.go:177`) <
`TestApproveShadowReviewCommandTwoPass`
(`cmd/freesided/approve_shadow_review_test.go:62`). Ancestor traces print
return addresses, so the raw stacks show each line plus one. The test opens
the store
to check the approval gate and never calls `Close`, so the `*sql.DB`, its
opener goroutine, and its SQLite file handle outlive the test.

Because `debug=2` dumps every live user goroutine, it also shows what the
detector passed over. Apart from the writer itself, only the survivors below
were alive at any package's end. The only channel waits among them are
`net/http` connection loops whose transport still holds them, so the parking
race above hid nothing in these runs:

- **`internal/ward`: 16 goroutines in `(*connectProxy).serve`, blocked in
  `Accept` (I/O wait),** in all three runs. Every one is created by
  `ward.startRegistryConnectProxy` (`internal/ward/egress.go:164`), called
  from `(*CodexReviewLifecycle).codexReview`, under 12 tests: the shared
  ReviewSource contract run for Codex and Claude (three cases each), and ten
  review-source and cleanup tests. Production closes a launch's proxy in
  `CodexReviewSource.finishCleanup`, and every one of these tests stops short
  of it:
  - Four contract cases, Start and CrashBeforeResult for each provider,
    simulate a restart by building a new source and abandoning the old one.
  - Four tests end while the review still runs: the two PollBeforeReady
    contract cases, `TestCodexReviewSourceRetriesOperationalBindingRead`,
    and `TestClaudeReviewLaunchesUnderAReadCredential`.
  - Four `CodexReviewCleanup*` tests call the lifecycle's
    `AbortCodexReview` directly, bypassing the source.
  - Three rejected-request tests rewrite the journal's intent to `preparing`
    or `starting` after the launch completed, so the abort takes the
    pre-start branch, which reaps containers but never closes the source's
    launch.
  - `TestAbortRecoveredReviewRetriesAnUnreadRequest` fails the journal read,
    leaving the abort for a retry that never comes.
- **`cmd/freesided` re-exec children:** `os/signal` receivers and idle
  `net/http` keep-alive connections, which a process holds until exit.

## Test Leaks and Production Leaks

- **Test leak: the `cmd/freesided` store.** The production API requires
  `Close` on a store from `OpenReadOnly`; the test skipped it.
- **Test-side, outside the profile: the `internal/ward` proxies.** Each
  test stops before the terminal path the source requires, or builds a state
  production can't reach while it holds a launch: the source's run gate
  serializes a launch and its rejection, so a held launch's intent is always
  `started`, whose abort calls `finishCleanup`. A real restart ends the
  proxy with the process.
- **Production leaks: none found.**

Neither test-side finding is filed: the issue scopes forge writes to
production leaks. Each fix is small: close the store in the test, and close
abandoned review launches in a test cleanup.

## Recommendation and Cost

**No permanent check;** rerun the measurement below when a unit adds a
long-lived goroutine owner, or before a release. If a check is adopted
later, it costs:

- **Runtime:** one `WriteTo` per test process, a full GC plus a stack dump:
  median 2.2 ms, p90 9.2 ms, max 31.2 ms, 274 ms summed over all 67
  processes (61 test binaries and 6 re-exec children).
  It grows with heap size and goroutine count, not test count.
- **Upkeep:** a `TestMain` in every package, merged by hand into any package
  that already has one, plus a shared helper to fail the run on a leak.
- **A false positive** would be a goroutine reported leaked whose waker still
  holds its channel or lock, but only through memory the GC doesn't trace,
  such as a `uintptr` or memory outside the Go heap. None appeared:
  `internal/store`, whose `modernc.org/sqlite` keeps C-side memory outside
  the Go heap, reported nothing, and the one report was a real unclosed
  `*sql.DB`. A schedule-dependent leak would show as a report that comes and
  goes between reruns; none did.
- **A false negative** comes from the parking race: a check that fails the
  run should yield briefly before writing, or flag a channel or lock wait
  that survives without the `(leaked)` tag.

## Reproduction

Generate the hooks from `daemon/` in a scratch checkout, add
`zzLeakProfileWrite()` between `m.Run()` and `os.Exit` in
`internal/store/main_test.go`, then run from the repository root:

```sh
out=$(mktemp -d)
FREESIDE_LEAKPROFILE_DIR="$out" bash scripts/check.sh daemon test -- -count=1 ./...
grep -l '(leaked)' "$out"/*.txt
```

`-count=1` matters: a cached package result never runs `TestMain`. To name
the test behind a leak, rerun its package with
`GODEBUG=tracebackancestors=10` set and read the `[originating from
goroutine N]` frames. The generator:

```bash
#!/usr/bin/env bash
# Run from daemon/. A package that already declares TestMain gets the
# writer only; call zzLeakProfileWrite() from its TestMain by hand.
set -euo pipefail
go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.Dir}} {{.Name}} {{.ImportPath}}{{end}}' ./... |
while read -r dir name path; do
  rm -f "$dir/zz_leakprofile_test.go"
  pkg=$name with_main=true
  if main=$(grep -ls '^func TestMain(' "$dir"/*_test.go); then
    pkg=$(sed -n 's/^package //p' "$(head -1 <<<"$main")" | head -1) with_main=false
  fi
  {
    printf 'package %s\n\nimport (\n' "$pkg"
    printf '\tzzfmt "fmt"\n\tzzos "os"\n\tzzfilepath "path/filepath"\n'
    printf '\tzzpprof "runtime/pprof"\n\tzzstrings "strings"\n\tzztime "time"\n'
    if $with_main; then printf '\tzztesting "testing"\n'; fi
    printf ')\n\nconst zzLeakProfilePackage = "%s"\n\n' "$path"
    if $with_main; then
      printf 'func TestMain(m *zztesting.M) {\n\tcode := m.Run()\n'
      printf '\tzzLeakProfileWrite()\n\tzzos.Exit(code)\n}\n\n'
    fi
    cat <<'GO'
// zzLeakProfileWrite writes the goroutineleak profile at debug=2 to
// $FREESIDE_LEAKPROFILE_DIR/<import path, / as _>.<pid>.txt, headed by how
// long WriteTo took. WriteTo runs its own leak-detection GC first.
func zzLeakProfileWrite() {
	dir := zzos.Getenv("FREESIDE_LEAKPROFILE_DIR")
	if dir == "" {
		return
	}
	name := zzstrings.ReplaceAll(zzLeakProfilePackage, "/", "_")
	f, err := zzos.Create(zzfilepath.Join(dir, zzfmt.Sprintf("%s.%d.txt", name, zzos.Getpid())))
	if err != nil {
		zzfmt.Fprintln(zzos.Stderr, "leakprofile:", err)
		return
	}
	defer f.Close()
	var body zzstrings.Builder
	start := zztime.Now()
	err = zzpprof.Lookup("goroutineleak").WriteTo(&body, 2)
	zzfmt.Fprintf(f, "package: %s\nwrite: %s\nerr: %v\n\n%s",
		zzLeakProfilePackage, zztime.Since(start), err, body.String())
}
GO
  } > "$dir/zz_leakprofile_test.go"
done
```

The imports carry a `zz` prefix so they can't collide with a package-level
name in any package.
