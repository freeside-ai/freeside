# Narrate Onboarding's Image Build and Keep Its Whole Output on Disk

**Chose step lines and a one-minute heartbeat on stderr, plus one tail-able
`0600` file per command step under the state directory, over streaming the
build's raw output to stderr, because an hour of raw `container build` and
`go test` output buries the few lines an operator needs, while a step line
names the file to `tail -f` when they want the rest.** #3442 carries the
contract; the friction came from #2382's exit run (F49, F63).

## What Happened

- **F49:** `onboard -approve` printed "building …" and then nothing for about
  45 minutes. The operator checked processes and containers to tell the build
  was alive.
- **F63:** a proof failed with `recipe command 0 "env" exited 1 (…)`. The
  error's 2 KiB tail started partway through the package list, so the failing
  test was cut off, and nothing kept the full output. Telling a flake from a
  regression meant rerunning the step by hand.

## Decisions

- **Logs live under `<state-dir>/project-image-logs`, not the
  content-addressed store.** They are operator diagnostics, not evidence:
  nothing reads them back except the builder's own `steps.json`, and nothing
  binds a digest to them. Putting them in the store would make them look like
  evidence and give them its retention.
- **No pruning.** An onboarding build is rare and operator-run. Pruning would
  need a retention policy nobody has asked for; the README tells the operator
  the directories accumulate.
- **A 256 MiB cap per step file.** It is far above any output the exit run
  saw and bounds a runaway build; past it the file ends with a line saying so
  and the tail and failing-test excerpt still see everything.
- **Failing Go tests are listed in the error everywhere, logs or not.** The
  excerpt reads the whole stream, so the daemon's rebuild and
  `freeside-project-image` get it without turning on files or the heartbeat,
  which stay onboarding-only (a non-goal of #3442).

## Why Logging Can't Change a Verdict

Every writer handed to `os/exec` reports full consumption and no error. A
writer error would end the copy, `Wait` would fail, and the proof would read
as a runtime fault instead of a failed proof. So a failed or capped log write
is recorded on the step and named in its error ("full output incomplete"),
never returned. The one log failure that stops a build is creating its log
directory, which happens before any step runs, so it can't interrupt a proof
either. No field of `projectimage.Request` or the onboarding review changed,
so approval digests are unchanged.

## Refute-First Findings

One fresh-context reviewer tried to break the change; it found no blocker.

- **Disproved by trace: a log write can flip a verdict.** Every writer in the
  chain returns `len(p), nil`, the tail buffer comes first, and every error
  keeps its `%w` class. A write after the runtime's wait delay takes the
  step writer's lock and stops at the closed file.
- **Disproved by trace: untrusted text reaches a log path.** The image name
  passed `imageNamePattern`, step names are fixed strings, and files open
  `O_EXCL`.
- **Allowed: one early failure blanks the next build's estimates.** The next
  build reads only the newest earlier record, as #3442 states, so a build
  that failed before `container build` leaves the following one without that
  step's estimate. Only the heartbeat's estimate is lost.
- **Allowed: a damaged newest record stops the search** instead of falling
  back to an older one; it costs only the estimate.
- **Allowed: disk use is unbounded across builds.** Pruning is a non-goal of
  #3442 (Decisions, above).

## Revisit When

- Log directories grow enough that an operator asks for pruning.
- The daemon's rebuild needs the same diagnostics; then decide where its logs
  live, since it has no operator watching stderr.
- A recipe reformats `go test` output so the excerpt finds nothing.
