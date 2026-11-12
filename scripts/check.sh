#!/usr/bin/env bash
# check.sh — the single entry point for a component's standard checks.
#
# Usage: check.sh <component> [step...]
#        check.sh daemon test -- <go test argument>...
#        check.sh --list
#
# Runs the named steps for one component, in the order given, or every
# step for that component when none is named. CI runs the same steps
# through this script, so the step list here is the authoritative
# definition of "the standard lint, build, and test checks" for a
# component; the per-component README documents operator tools that
# are not checks (installers, image builds, live runs).
#
# Components and steps (`--list` prints the same table):
#   daemon       build test vet lint
#   app          generate format test linux build-mac build-ios
#   api          lint digest policy compat
#   scripts      syntax shellcheck suites vocabulary tier-a-paths trackercollect
#   convergence  run
#   docs         plan-links
#
# Tool overrides, for CI runners that install pinned binaries and for
# local machines with a tool outside PATH:
#   GOLANGCI_LINT  command for golangci-lint (default: golangci-lint;
#                  CI pins v2.15.0 through golangci-lint-action)
#   SHELLCHECK     command for shellcheck (default: shellcheck; CI pins a
#                  checksum-verified binary in .github/workflows/scripts-ci.yml)
#   VACUUM         command for the OpenAPI linter (default: `go run`
#                  of the pinned vacuum module, ~7 minutes cold)
#   OASDIFF        command for the breaking-change differ the `api compat`
#                  step runs (default: `go run` of the oasdiff module that
#                  scripts/api-compatibility-check.sh pins; CI installs a
#                  checksum-verified binary). The step also needs jq and
#                  origin/main.
#
# macOS verification executor:
#   APP_MACVERIFY  non-empty runs `app test`, `app build-mac`, and
#                  `app build-ios` the way a slot of the macOS verification
#                  executor needs (daemon/README.md, Install the macOS
#                  Verification Executor): `swift test` gains
#                  --disable-sandbox, and xcodebuild reads the job's seeded
#                  ../source-packages through git, writes ../derived-data,
#                  and runs with SwiftPM's and Xcode's sandboxes off, since
#                  neither nests inside the room's. Unset or empty, the
#                  steps run exactly as CI runs them. The trusted recipe
#                  sets it because it can't name a `..` path itself
#                  (devlog/2026-11-10-1040-app-steps-executor-switch.md).
#
# Daemon Go flags:
#   Every daemon step appends -trimpath to GOFLAGS: unset becomes
#   -trimpath, and GOFLAGS=-p=2 becomes "-p=2 -trimpath". Without it Go
#   hashes each package's absolute directory into its cache keys, so
#   checkouts at different paths share none of the daemon module's
#   compiled packages or test results. The environment variable shadows a
#   GOFLAGS set only with `go env -w`, so pass extra flags in the
#   environment. A bare `go test` without the flag shares no daemon
#   package entries with these steps. Under -trimpath, runtime.Caller
#   returns import paths, not file paths, so tests find repository files
#   from the working directory, which go test sets to the package
#   directory.
#
# Go build cache cap:
#   FREESIDE_GOCACHE_LIMIT_GB  cap on the Go build cache in whole gigabytes
#                              of 10^9 bytes (default: 40, or 0 when CI is
#                              non-empty); 0 turns the trim off
# Go deletes cache entries only after 5 unused days and has no size limit,
# so once per `check.sh daemon` invocation, before the first step, the cache
# `go env GOCACHE` names is trimmed by scripts/go-cache-trim.py. It walks the
# cache at most once an hour (state in
# ${XDG_CACHE_HOME:-$HOME/.cache}/freeside/go-cache-trim), and over the cap
# deletes least recently used entries until the cache is at or under 75% of
# it. It keeps every entry modified in the last 2 hours, which covers every
# entry used in the last hour (Go refreshes an entry's modification time only
# once it is an hour old); when only those remain, it warns. It skips an
# entry it can't delete. A missing python3, no cache directory, or a failed
# trim prints a notice and the steps run anyway. A bare `go test` gets no
# cap.
#
# Local daemon lint runs:
#   GOLANGCI_LINT_CACHE  defaults to
#     ${XDG_CACHE_HOME:-$HOME/.cache}/freeside/golangci-lint<absolute checkout root>
# Separate checkout caches prevent golangci-lint from reporting cached paths
# from sibling worktrees. Explicit values pass through unchanged, including
# an empty value (the linter's native default); shared or empty overrides give
# up automatic isolation. CI lint runs separately through golangci-lint-action.
#
# Local daemon test runs:
#   DAEMON_TEST_MAX_RUNS   simultaneous test steps (default: 1, or 0 when
#                          CI is non-empty); 0 turns the limit off
#   DAEMON_TEST_STATE_DIR  shared per-user state directory (default:
#                          ${XDG_CACHE_HOME:-$HOME/.cache}/freeside/daemon-test)
#   DAEMON_TEST_INTEGRATION_PARTITIONS
#                          concurrent -run partitions of internal/integration
#                          in a full run (default: 3, CI's shard count); 1
#                          runs go test ./... whole with no discovery pass
# A full run with more than one partition needs python3: it lists the
# packages and compiles the integration families (daemon-test-shards.py
# local), then starts one go test of every other package beside one go test
# per partition, all inside the one slot, and prints each partition's output
# whole after the run. A failed discovery fails the step with Go's build
# output; it never falls back to a partial run. Shard runs and `--` runs
# ignore the setting. A run holding a slot under a limit above 1 runs whole
# unless the setting is present, since each partition would take the slot's
# whole GOMAXPROCS share.
# A queued run names the limit, state directory, and holders every 30 seconds.
# The local (non-shard) path passes -timeout 30m before the package
# arguments, so a -timeout after `--` wins. While it holds a slot, the test
# child gets TMPDIR=<slot>.tmp.noindex, emptied at the start and removed at
# exit; Spotlight skips a directory named *.noindex. Under a limit above 1 it
# also gets GOMAXPROCS of cores / limit rounded up, minimum 2, unless
# GOMAXPROCS is already set. A run without a slot keeps both as inherited.
# Outside CI on macOS, when git resolves to /usr/bin/git, the test step uses
# xcrun's git through a stable directory containing only git on PATH, with
# its helpers and config preserved. Other git installations and later steps
# are unchanged.
# The test step clears every repository variable git names (rev-parse
# --local-env-vars), then runs the tests with GIT_DIR naming the linked
# worktree of a decoy repository, as `git rebase -x` does from a worktree. It
# fails, printing the difference, when the decoy's .git changed, even if every
# test passed: a fixture git helper that inherits GIT_* instead of starting
# from gitenv.Scrubbed() wrote to it. The decoy sits beside the held slot so
# GIT_DIR stays stable for Go's test cache; without a slot it is a fresh
# temporary directory. The shard runner passes the environment through to go
# test unchanged; a change that sanitizes it would disarm this guard. A test
# result cached by any earlier run, decoy or not, does not rerun, and opt-in
# live suites skip, so only an uncached run (CI's shards) covers every default
# test. The earlier run can be in another checkout of the same source, so a
# new checkout no longer guarantees a first real run. Go rechecks files a
# test read inside daemon/ by their absolute path, so those tests rerun per
# checkout, but it never rechecks files outside daemon/: a test that reads
# api/openapi.yaml or prompts/ can replay a pass recorded before that file
# changed.
# CI shard runs require all of DAEMON_TEST_SHARD_KIND (rest, freesided,
# integration, or darwin), DAEMON_TEST_SHARD_INDEX, DAEMON_TEST_SHARD_COUNT,
# DAEMON_TEST_INTEGRATION_SHARDS, and DAEMON_TEST_EVIDENCE_DIR. Indices are
# one-based; the rest, freesided, and darwin shards use 1/1.
# Shards disable test-result caching and retain full execution evidence.
# Arguments after `daemon test --` replace ./... as the go test arguments,
# run from daemon/, under the same slot, git shim, and decoy as a full run;
# name at least one package. Shard runs refuse them, and the PASS line names
# them so a narrow run never reads as a full one.
#
# Convergence runs:
#   CONVERGENCE_ALL_TESTS  non-empty runs every Swift test suite in the
#                          convergence pass (run-convergence.sh --all-tests),
#                          not only FreesideConvergenceTests; CI sets it when
#                          the app component is selected
#   CONVERGENCE_SKIP_BUILD non-empty runs `swift test --skip-build` against
#                          the test bundle already in app/.build
#                          (run-convergence.sh --skip-build); CI sets it
#                          only on an exact Swift build cache hit for a run
#                          without the app component
#   CONVERGENCE_SCREENSHOT_SHARDS
#                          with CONVERGENCE_ALL_TESTS, the number of
#                          concurrent slices the screenshot regression
#                          matrix renders as; default the smaller of the
#                          core count and 2, and 1 runs it whole
#
# The daemon's opt-in live suites are skipped by `go test` unless their
# environment is set (FREESIDE_PUBLISH_LIVE_TEST, FREESIDE_WARD_LIVE_TEST,
# FREESIDE_CLAUDE_TOKEN_LIVE_TEST, FREESIDE_CODEX_ENROLLMENT_LIVE_TEST,
# FREESIDE_MARKDOWN_LIVE_TEST, FREESIDE_DRIFT_REPLAY_LIVE_TEST); each test's skip message lists the rest
# of its environment. They are CI-blind by design.
#
# Exit codes:
#   0  every requested step passed
#   1  a step failed (its command's output says which)
#   2  usage error: unknown component or step, malformed daemon test limit,
#      partition count, or Go cache limit, or a misplaced `--` (anywhere
#      but after `daemon test`, with nothing after it, or in a shard run)
set -euo pipefail

PROG=$(basename "$0")
ROOT=$(cd "$(dirname "$0")/.." && pwd)

GOLANGCI_LINT=${GOLANGCI_LINT:-golangci-lint}
SHELLCHECK=${SHELLCHECK:-shellcheck}
VACUUM=${VACUUM:-go run github.com/daveshanley/vacuum@v0.29.9}
APP_MACVERIFY=${APP_MACVERIFY:-}

COMPONENTS='daemon app api scripts convergence docs'
steps_for() { # <component>; prints the component's steps in default order
  case $1 in
    daemon) echo 'build test vet lint' ;;
    app) echo 'generate format test linux build-mac build-ios' ;;
    api) echo 'lint digest policy compat' ;;
    scripts) echo 'syntax shellcheck suites vocabulary tier-a-paths trackercollect' ;;
    convergence) echo 'run' ;;
    docs) echo 'plan-links' ;;
    *) return 1 ;;
  esac
}

usage() {
  echo "usage: $PROG <component> [step...] | $PROG daemon test -- <go test argument>... | $PROG --list" >&2
  exit 2
}

list() {
  local c
  for c in $COMPONENTS; do
    printf '%-12s %s\n' "$c" "$(steps_for "$c")"
  done
}

run() { # <command...>; echoes then runs it from the current directory
  printf '+ %s\n' "$*" >&2
  "$@"
}

in_dir() { # <dir> <command...>; runs the command inside <dir>
  local dir=$1
  shift
  printf '+ (cd %s && %s)\n' "$dir" "$*" >&2
  (cd "$dir" && "$@")
}

# --- daemon ---------------------------------------------------------------

daemon_build() {
  in_dir "$ROOT/daemon" go build ./...
  # The exporter must stay a static cross-compilable binary for the
  # exporter image (#73 acceptance 4); a creeping cgo dependency fails
  # here instead of at image-build time.
  in_dir "$ROOT/daemon" env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -o /dev/null ./cmd/freeside-export
}
daemon_test_shard_mode() {
  [[ ${DAEMON_TEST_SHARD_KIND+x}${DAEMON_TEST_SHARD_INDEX+x}${DAEMON_TEST_SHARD_COUNT+x}${DAEMON_TEST_INTEGRATION_SHARDS+x}${DAEMON_TEST_EVIDENCE_DIR+x} ]]
}

# daemon_test_spawn <command...>: starts the command in the background from
# daemon/. Bash starts a background command with SIGINT and SIGQUIT ignored,
# and go test's compilers and test binaries would inherit that, so Ctrl-C
# would leave them running outside the slot; restoring the defaults lets it
# end every invocation, as it ended the single foreground go test. $! names
# the command itself, since each layer execs the next.
daemon_test_spawn() {
  (cd "$ROOT/daemon" && exec python3 -c 'import os, signal, sys
signal.signal(signal.SIGINT, signal.SIG_DFL)
signal.signal(signal.SIGQUIT, signal.SIG_DFL)
os.execvp(sys.argv[1], sys.argv[1:])' "$@") &
}

# daemon_test_partitioned <count> <log dir>: runs a full local test step as
# one go test of every package but internal/integration, beside <count>
# concurrent -run partitions of that package, which alone would set the wall
# time. The partition logs print whole after every invocation ends, so
# concurrent failures never interleave. Returns the first non-zero exit code
# in launch order.
daemon_test_partitioned() {
  local count=$1 logs=$2 plan kind package pattern integration='' rest=() patterns=()
  local go_test=(go test -timeout 30m) pids=() pid i code rc=0
  plan=$(in_dir "$ROOT" python3 scripts/daemon-test-shards.py local --count "$count") || return 1
  while read -r kind package pattern; do
    case $kind in
      rest) rest+=("$package") ;;
      integration)
        integration=$package
        patterns+=("$pattern")
        ;;
      *) echo "daemon test: unexpected partition line: $kind $package" >&2; return 1 ;;
    esac
  done <<<"$plan"
  if ((${#rest[@]} == 0)) || [[ ${#patterns[@]} != "$count" ]]; then
    echo "daemon test: the partition plan lacks a package list or an integration partition" >&2
    return 1
  fi
  mkdir -p "$logs" || return 1
  echo "daemon test: $integration runs as $count -run partitions; their output follows the run" >&2
  printf '+ (cd %s && %s)\n' "$ROOT/daemon" "${go_test[*]} ${rest[*]}" >&2
  daemon_test_spawn "${go_test[@]}" "${rest[@]}"
  pids+=($!)
  # No command echo: a partition's -run pattern can run to kilobytes.
  for ((i = 0; i < count; i++)); do
    daemon_test_spawn "${go_test[@]}" -run "${patterns[i]}" "$integration" \
      >"$logs/$((i + 1)).log" 2>&1
    pids+=($!)
  done
  for pid in "${pids[@]}"; do
    code=0
    wait "$pid" || code=$?
    ((rc != 0)) || rc=$code
  done
  for ((i = 1; i <= count; i++)); do
    echo "== $integration partition $i/$count" >&2
    cat "$logs/$i.log"
  done
  return "$rc"
}

# daemon_test_run <decoy>: runs the selected tests; a partitioned full run
# keeps its logs under <decoy>, which the decoy comparison never reads.
daemon_test_run() {
  local args=(./...)
  ((${#daemon_test_args[@]} == 0)) || args=("${daemon_test_args[@]}")
  if ! daemon_test_shard_mode; then
    # A slot under a limit above 1 owns only its share of the machine, and
    # each partition would take that whole share; an explicit count wins.
    local partitions=$daemon_test_partitions
    if [[ -n $daemon_test_slot && $daemon_test_limit != 1 && ! ${DAEMON_TEST_INTEGRATION_PARTITIONS+x} ]]; then
      partitions=1
    fi
    if [[ $partitions != 1 ]]; then
      daemon_test_partitioned "$partitions" "$1/partitions"
    else
      # A caller's -timeout after -- comes later and wins.
      in_dir "$ROOT/daemon" go test -timeout 30m "${args[@]}"
    fi
    return
  fi
  if ((${#daemon_test_args[@]} != 0)); then
    echo "daemon test: go test arguments after '--' don't apply in shard mode" >&2
    return 2
  fi
  local kind=${DAEMON_TEST_SHARD_KIND:-} index=${DAEMON_TEST_SHARD_INDEX:-}
  local count=${DAEMON_TEST_SHARD_COUNT:-} integration=${DAEMON_TEST_INTEGRATION_SHARDS:-}
  if [[ ! $kind =~ ^(rest|freesided|integration|darwin)$ || ! $index =~ ^[1-9][0-9]*$ ||
        ! $count =~ ^[1-9][0-9]*$ || ! $integration =~ ^[1-9][0-9]*$ ||
        -z ${DAEMON_TEST_EVIDENCE_DIR:-} ]]; then
    echo 'daemon test: require shard kind, index, count, integration count, and evidence directory together' >&2
    return 2
  fi
  in_dir "$ROOT" python3 scripts/daemon-test-shards.py run --kind "$kind" \
    --index "$index" --count "$count" --integration-count "$integration" \
    --evidence-dir "$DAEMON_TEST_EVIDENCE_DIR"
}

daemon_test_take_slot() {
  local limit=${DAEMON_TEST_MAX_RUNS-1}
  if [[ ! ${DAEMON_TEST_MAX_RUNS+x} && -n ${CI:-} ]]; then limit=0; fi
  if [[ ! $limit =~ ^[0-9]+$ ]]; then
    echo 'daemon test: DAEMON_TEST_MAX_RUNS must be a non-negative integer' >&2
    return 2
  fi
  # Strip leading zeroes so Bash never treats the limit as octal.
  limit=${limit#"${limit%%[!0]*}"}
  limit=${limit:-0}
  [[ $limit != 0 ]] || return 0
  local state=${DAEMON_TEST_STATE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/freeside/daemon-test}
  local slot file rc pid waited=0
  if ! command -v python3 >/dev/null; then
    echo 'daemon test: python3 unavailable; running without the limit' >&2
    return 0
  fi
  if ! mkdir -p "$state/slots" 2>/dev/null; then
    echo "daemon test: cannot create $state/slots; running without the limit" >&2
    return 0
  fi
  while :; do
    for ((slot=1; ; slot++)); do
      # Compare decimal strings so a large valid limit cannot overflow Bash.
      # shellcheck disable=SC2071 # Equal-length decimal strings sort numerically.
      [[ ${#slot} -lt ${#limit} || ( ${#slot} -eq ${#limit} &&
        ( $slot < "$limit" || $slot == "$limit" ) ) ]] || break
      file=$state/slots/$slot
      if ! ( : >>"$file" ) 2>/dev/null; then
        echo "daemon test: cannot open $file; running without the limit" >&2
        return 0
      fi
      exec 9>>"$file"
      # flock belongs to the open file description: Python can exit while
      # this shell holds the lock. lockf would release it with Python.
      if python3 -c 'import errno, fcntl, sys
try:
    fcntl.flock(9, fcntl.LOCK_EX | fcntl.LOCK_NB)
except OSError as e:
    sys.exit(75 if e.errno in (errno.EACCES, errno.EAGAIN) else 1)
' 2>/dev/null; then
        # $$ names the outer script, not this lock-holding subshell. Bash
        # 3.2 lacks BASHPID; an exec'd child can report its parent's PID.
        pid=${BASHPID:-$(exec sh -c 'echo "$PPID"')}
        printf 'run PID %s: %s\n' "$pid" "$ROOT" >"$file"
        daemon_test_slot=$file
        daemon_test_limit=$limit
        return 0
      else
        rc=$?
      fi
      exec 9>&-
      if [[ $rc != 75 ]]; then
        echo "daemon test: cannot lock $file; running without the limit" >&2
        return 0
      fi
    done
    if ((waited % 30 == 0)); then
      echo "daemon test: waiting for a slot (limit $limit, state $state); DAEMON_TEST_MAX_RUNS=0 disables the limit" >&2
      for ((slot=1; ; slot++)); do
        # shellcheck disable=SC2071 # Equal-length decimal strings sort numerically.
        [[ ${#slot} -lt ${#limit} || ( ${#slot} -eq ${#limit} &&
          ( $slot < "$limit" || $slot == "$limit" ) ) ]] || break
        printf '  slot %s: %s\n' "$slot" "$(cat "$state/slots/$slot")" >&2
      done
    fi
    sleep 1
    waited=$((waited + 1))
  done
}

# daemon_test_gomaxprocs: exports GOMAXPROCS as the held slot's share of the
# cores, cores / limit rounded up with a floor of 2, so go's -p, each test
# binary's -parallel, and its runtime threads shrink together. A caller's
# GOMAXPROCS wins, and a limit of 1 keeps Go's default.
daemon_test_gomaxprocs() {
  local cores limit=$daemon_test_limit
  [[ -n $daemon_test_slot && $limit != 1 && ! ${GOMAXPROCS+x} ]] || return 0
  cores=$(getconf _NPROCESSORS_ONLN 2>/dev/null) || cores=''
  if [[ ! $cores =~ ^[1-9][0-9]*$ ]]; then
    echo "daemon test: cannot count cores; keeping Go's GOMAXPROCS" >&2
    return 0
  fi
  # Compare decimal strings: a valid limit can overflow Bash arithmetic.
  # shellcheck disable=SC2071 # Equal-length decimal strings sort numerically.
  if [[ ${#limit} -gt ${#cores} || ( ${#limit} -eq ${#cores} &&
      ( $limit > "$cores" || $limit == "$cores" ) ) ]]; then
    GOMAXPROCS=2
  else
    GOMAXPROCS=$(( (cores + limit - 1) / limit ))
    ((GOMAXPROCS >= 2)) || GOMAXPROCS=2
  fi
  export GOMAXPROCS
}

daemon_test_use_real_git() {
  [[ -z ${CI:-} ]] || return 0
  [[ $(uname -s) == Darwin && $(command -v git) == /usr/bin/git ]] || return 0
  local real prefix state dir entry helpers
  real=$(xcrun -f git 2>/dev/null) || return 0
  [[ $real == */bin/git && -x $real ]] || return 0
  prefix=${real%/bin/git}
  state=${DAEMON_TEST_STATE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/freeside/daemon-test}
  dir=$state/git$prefix
  if ! mkdir -p "$dir/bin" 2>/dev/null; then
    echo "daemon test: cannot create $dir/bin; keeping git PATH" >&2
    return 0
  fi
  # Git locates helpers/config relative to its invocation path, without
  # resolving symlinks. Mirror its prefix, not just the binary. Never remove
  # entries: concurrent builders add only missing links, with bin/git last.
  for entry in "$prefix"/*; do
    [[ ${entry##*/} != bin ]] || continue
    if [[ ! -e $dir/${entry##*/} && ! -L $dir/${entry##*/} ]]; then
      if ! ln -s "$entry" "$dir/${entry##*/}" 2>/dev/null &&
          [[ ! -L $dir/${entry##*/} ]]; then
        echo "daemon test: cannot link $entry; keeping git PATH" >&2
        return 0
      fi
    fi
  done
  if [[ ! -e $dir/bin/git ]]; then
    if ! ln -s "$real" "$dir/bin/git" 2>/dev/null && [[ ! -L $dir/bin/git ]]; then
      echo "daemon test: cannot link $real; keeping git PATH" >&2
      return 0
    fi
  fi
  helpers=$("$dir/bin/git" --exec-path 2>/dev/null) || helpers=''
  if [[ ! -x $helpers/git ]]; then
    echo 'daemon test: git helper directory unavailable; keeping git PATH' >&2
    return 0
  fi
  # A stable PATH lets Go reuse cached tests that read this environment input.
  export PATH="$dir/bin:$PATH"
}

# daemon_test_decoy <dir>: builds the decoy under <dir>: a repository with one
# empty commit and a linked worktree, plus a copy of its .git to compare with.
# Test builds stamp version control through `git status`, which reads the
# developer's global config; the local pins keep its index caches from
# writing to the decoy.
daemon_test_decoy() {
  local dir=$1
  local git=(env GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git
    -c init.defaultBranch=main -c user.name=decoy -c user.email=decoy@test.invalid)
  rm -rf "$dir" && mkdir -p "$dir" &&
    "${git[@]}" init -q "$dir/repo" &&
    "${git[@]}" -C "$dir/repo" config core.untrackedCache false &&
    "${git[@]}" -C "$dir/repo" config core.fsmonitor false &&
    "${git[@]}" -C "$dir/repo" config core.splitIndex false &&
    "${git[@]}" -C "$dir/repo" commit -q --allow-empty -m decoy &&
    "${git[@]}" -C "$dir/repo" worktree add -q "$dir/wt" -b wt &&
    cp -R "$dir/repo/.git" "$dir/before"
}

# daemon_test_remove_tmp <dir>: removes a slot's test TMPDIR. A test killed by
# its timeout skips its cleanup and can leave read-only directories, which
# would make rm fail on every later run in that slot. chmod -R follows no
# symbolic links.
daemon_test_remove_tmp() {
  chmod -R u+rwX "$1" 2>/dev/null || :
  rm -rf "$1"
}

# daemon_test_read_partitions: sets daemon_test_partitions, the integration
# partition count of a full local run; shard and narrow runs ignore the
# setting and run as one invocation. Dispatch calls it before any daemon
# step, so a bad value stops `check.sh daemon` before go build runs.
daemon_test_read_partitions() {
  daemon_test_partitions=1
  if daemon_test_shard_mode || ((${#daemon_test_args[@]} != 0)); then return 0; fi
  local count=${DAEMON_TEST_INTEGRATION_PARTITIONS-3}
  # Strip leading zeroes so Bash never treats the count as octal.
  [[ ! $count =~ ^[0-9]+$ ]] || count=${count#"${count%%[!0]*}"}
  if [[ ! $count =~ ^[1-9][0-9]*$ ]]; then
    echo 'daemon test: DAEMON_TEST_INTEGRATION_PARTITIONS must be a positive integer' >&2
    return 2
  fi
  daemon_test_partitions=$count
}

# daemon_gocache_read_limit: sets daemon_gocache_limit, the Go build cache
# cap in gigabytes, 0 for off. Dispatch calls it before any daemon step, so
# a bad value stops `check.sh daemon` before go runs.
daemon_gocache_read_limit() {
  local limit=${FREESIDE_GOCACHE_LIMIT_GB-40}
  if [[ ! ${FREESIDE_GOCACHE_LIMIT_GB+x} && -n ${CI:-} ]]; then limit=0; fi
  if [[ ! $limit =~ ^[0-9]+$ ]]; then
    echo 'daemon: FREESIDE_GOCACHE_LIMIT_GB must be a non-negative integer' >&2
    return 2
  fi
  # Strip leading zeroes so Bash never treats the limit as octal.
  limit=${limit#"${limit%%[!0]*}"}
  daemon_gocache_limit=${limit:-0}
}

# daemon_trim_go_cache: trims the Go build cache to daemon_gocache_limit
# (see the header). It is upkeep, not a check, so every failure is a notice
# and never fails the run.
daemon_trim_go_cache() {
  [[ $daemon_gocache_limit != 0 ]] || return 0
  local cache
  if ! command -v python3 >/dev/null; then
    echo 'daemon: python3 unavailable; skipping the Go cache trim' >&2
    return 0
  fi
  if ! cache=$(cd "$ROOT/daemon" && go env GOCACHE) || [[ -z $cache || $cache == off ]] ||
      [[ ! -d $cache ]]; then
    echo "daemon: no Go build cache directory (go env GOCACHE: '${cache:-}'); skipping the trim" >&2
    return 0
  fi
  # Nine zeroes appended as text: Bash arithmetic would overflow a large
  # limit, and Python's integers don't.
  if ! python3 "$ROOT/scripts/go-cache-trim.py" --cache "$cache" \
      --limit-bytes "${daemon_gocache_limit}000000000" \
      --state "${XDG_CACHE_HOME:-$HOME/.cache}/freeside/go-cache-trim"; then
    echo 'daemon: the Go cache trim failed; continuing' >&2
  fi
}

daemon_test() (
  local vars decoy tmp='' rc=0
  daemon_test_slot='' daemon_test_limit=''
  daemon_test_use_real_git
  daemon_test_take_slot
  # An ambient repository (GIT_DIR from `git rebase -x`) must reach neither
  # the decoy's setup nor the tests.
  vars=$(git rev-parse --local-env-vars) || return 1
  # shellcheck disable=SC2086 # One variable name per line.
  unset $vars
  if [[ -n $daemon_test_slot ]]; then
    decoy=$daemon_test_slot.decoy
    # Spotlight skips a directory whose name ends in .noindex.
    tmp=$daemon_test_slot.tmp.noindex
  else
    decoy=$(mktemp -d "${TMPDIR:-/tmp}/freeside-decoy.XXXXXX") || return 1
  fi
  # Each fixture resolves a relative GIT_DIR against its own directory.
  [[ $decoy == /* ]] || decoy=$PWD/$decoy
  [[ -z $tmp || $tmp == /* ]] || tmp=$PWD/$tmp
  trap 'rm -rf "$decoy"; [[ -z $tmp ]] || daemon_test_remove_tmp "$tmp"' EXIT
  if ! daemon_test_decoy "$decoy"; then
    echo "daemon test: cannot build the decoy repository in $decoy" >&2
    return 1
  fi
  if [[ -n $tmp ]] && ! { daemon_test_remove_tmp "$tmp" && mkdir -p "$tmp"; }; then
    echo "daemon test: cannot create the test TMPDIR $tmp" >&2
    return 1
  fi
  # Bash 3.2 saves a redirection on another descriptor if applied to the
  # function call. Close it inside a subshell so orphaned children can't
  # inherit either the slot descriptor or a saved copy.
  ( exec 9>&-; export GIT_DIR=$decoy/repo/.git/worktrees/wt
    [[ -z $tmp ]] || export TMPDIR=$tmp
    daemon_test_gomaxprocs
    daemon_test_run "$decoy" ) || rc=$?
  if ! diff -r "$decoy/before" "$decoy/repo/.git" >"$decoy/diff" 2>&1; then
    echo 'daemon test: a test wrote to the decoy repository through the inherited GIT_DIR; build fixture git environments from gitenv.Scrubbed():' >&2
    cat "$decoy/diff" >&2
    ((rc != 0)) || rc=1
  fi
  return "$rc"
)
daemon_vet() { in_dir "$ROOT/daemon" go vet ./...; }
daemon_lint() {
  local cache=${GOLANGCI_LINT_CACHE-${XDG_CACHE_HOME:-$HOME/.cache}/freeside/golangci-lint$ROOT}
  # shellcheck disable=SC2086 # GOLANGCI_LINT may carry arguments
  in_dir "$ROOT/daemon" env GOLANGCI_LINT_CACHE="$cache" $GOLANGCI_LINT run
}

# --- app -------------------------------------------------------------------

app_generate() { in_dir "$ROOT/app" ./scripts/generate-api-client.sh; }
app_format() (
  cd "$ROOT/app"
  local files=(Package.swift)
  while IFS= read -r -d '' file; do
    files+=("$file")
  done < <(find Sources Tests Apps -name '*.swift' \
    -not -path 'Sources/FreesideAPI/GeneratedSources/*' -print0)
  run xcrun swift-format lint --strict "${files[@]}"
)
app_test() {
  local flags=(--only-use-versions-from-resolved-file)
  [[ -z $APP_MACVERIFY ]] || flags+=(--disable-sandbox)
  in_dir "$ROOT/app" swift test "${flags[@]}"
}
# The Linux-clean targets alone, as the Linux CI job runs them in the
# swift image: FreesideModel and its tests beside FreesideAPI's. On macOS
# it runs the same suites but proves less: an Apple-only import in the
# model still compiles there, so only the Linux job catches one.
app_linux() {
  in_dir "$ROOT/app" swift build --only-use-versions-from-resolved-file --target FreesideModel
  in_dir "$ROOT/app" swift test --only-use-versions-from-resolved-file \
    --filter 'FreesideAPITests|FreesideModelTests'
}
xcodebuild_scheme() { # <scheme> <destination> [build-setting...]
  local scheme=$1 destination=$2
  shift 2
  local options=(-project Freeside.xcodeproj -scheme "$scheme"
    -destination "$destination" -onlyUsePackageVersionsFromResolvedFile
    -skipPackageUpdates -skipPackagePluginValidation)
  local settings=(CODE_SIGNING_ALLOWED=NO COMPILER_INDEX_STORE_ENABLE=NO)
  if [[ -n $APP_MACVERIFY ]]; then
    # The executor's job directory holds the workspace, the seeded
    # source-packages/, and room for derived-data/ beside it.
    local job
    job=$(dirname "$ROOT")
    options+=(-derivedDataPath "$job/derived-data"
      -clonedSourcePackagesDirPath "$job/source-packages"
      -disablePackageRepositoryCache -scmProvider system -skipMacroValidation
      -IDEPackageSupportDisableManifestSandbox=YES
      -IDEPackageSupportDisablePluginExecutionSandbox=YES)
    # shellcheck disable=SC2016 # $(inherited) is xcodebuild's, not the shell's
    settings+=('OTHER_SWIFT_FLAGS=$(inherited) -disable-sandbox')
  fi
  in_dir "$ROOT/app" xcodebuild "${options[@]}" "${settings[@]}" "$@" build
}
app_build_mac() { xcodebuild_scheme FreesideMac 'platform=macOS'; }
app_build_ios() {
  # The generic simulator destination compiles dependencies for both arm64
  # and x86_64 unless both settings are given; this gate verifies source
  # compatibility and linkage on an arm64 host, not simulator distribution
  # (devlog/2026-07-15-2040-app-ci-runtime.md).
  xcodebuild_scheme FreesideIOS 'generic/platform=iOS Simulator' \
    ARCHS=arm64 ONLY_ACTIVE_ARCH=YES
}

# --- api -------------------------------------------------------------------

api_lint() {
  # shellcheck disable=SC2086 # VACUUM may carry arguments (the go run form)
  in_dir "$ROOT" $VACUUM lint -r api/vacuum.ruleset.yaml -f api/functions \
    --details --fail-severity warn api/openapi.yaml
}
api_digest() { in_dir "$ROOT" bash scripts/api-contract-digest.sh --check; }
# api_compat fails a breaking contract change that leaves the spec's
# x-compatibility-level unchanged (api/README.md, Compatibility Level).
api_compat() { in_dir "$ROOT" bash scripts/api-compatibility-check.sh; }
# api_policy proves the lint's custom rules still fire: each fixture under
# api/testdata/policy/ names on its first line (`# expects-rule: <id>`) the
# one rule it breaks, and the step fails unless linting the fixture fails
# and reports that rule. Without it, a rule that silently stopped matching
# would pass the real spec forever. Fixture names never contain a rule id,
# because vacuum's output prints the file path.
api_policy() {
  local fixture rule out found=0
  for fixture in "$ROOT"/api/testdata/policy/*.yaml; do
    [ -e "$fixture" ] || continue
    found=1
    rule=$(sed -n '1s/^# expects-rule: //p' "$fixture")
    if [ -z "$rule" ]; then
      printf 'policy fixture %s has no expects-rule line\n' "$fixture" >&2
      return 1
    fi
    # shellcheck disable=SC2086 # VACUUM may carry arguments (the go run form)
    if out=$(in_dir "$ROOT" $VACUUM lint -r api/vacuum.ruleset.yaml \
      -f api/functions --details --fail-severity warn \
      "${fixture#"$ROOT"/}" 2>&1); then
      printf 'policy fixture %s passed the lint; it must fail rule %s\n' \
        "$fixture" "$rule" >&2
      return 1
    fi
    case $out in
      *"$rule"*) printf 'policy fixture %s fails rule %s\n' "$fixture" "$rule" ;;
      *)
        printf '%s\n' "$out" >&2
        printf 'policy fixture %s failed without reporting rule %s\n' \
          "$fixture" "$rule" >&2
        return 1
        ;;
    esac
  done
  if [ "$found" -eq 0 ]; then
    echo 'no policy fixtures under api/testdata/policy/' >&2
    return 1
  fi
}

# --- scripts ---------------------------------------------------------------

shell_files() { # prints every maintained shell file
  printf '%s\n' "$ROOT"/scripts/*.sh "$ROOT"/app/scripts/*.sh "$ROOT"/.githooks/*
}
scripts_syntax() {
  local f
  while IFS= read -r f; do run bash -n "$f"; done < <(shell_files)
}
scripts_shellcheck() {
  local files=()
  while IFS= read -r f; do files+=("$f"); done < <(shell_files)
  # shellcheck disable=SC2086 # SHELLCHECK may carry arguments
  run $SHELLCHECK "${files[@]}"
}
scripts_suites() {
  # Every scripts/test-*.sh is a hermetic regression suite (synthetic
  # repos and command stand-ins, no network); a new suite joins by name.
  local t
  for t in "$ROOT"/scripts/test-*.sh; do run bash "$t"; done
}
scripts_vocabulary() { in_dir "$ROOT" bash scripts/check-vocabulary.sh; }
scripts_tier_a_paths() { in_dir "$ROOT" bash scripts/check-tier-a-paths.sh; }
scripts_trackercollect() {
  in_dir "$ROOT" go -C scripts/trackercollect build ./...
  in_dir "$ROOT" go -C scripts/trackercollect test ./...
  in_dir "$ROOT" go -C scripts/trackercollect vet ./...
}

# --- convergence -----------------------------------------------------------

convergence_run() {
  in_dir "$ROOT" bash scripts/run-convergence.sh ${CONVERGENCE_ALL_TESTS:+--all-tests} ${CONVERGENCE_SKIP_BUILD:+--skip-build}
}

# --- docs ------------------------------------------------------------------

docs_plan_links() {
  # Every "Section N" citation in docs/plan.md must link to its heading
  # anchor and no citation or existing link may be broken; drop --check to
  # rewrite the links after a plan edit.
  in_dir "$ROOT" bash scripts/link-plan-section-refs.sh --check
}

# --- dispatch --------------------------------------------------------------

if [ "$#" -eq 0 ]; then usage; fi
if [ "$1" = --list ]; then
  [ "$#" -eq 1 ] || usage
  list
  exit 0
fi

component=$1
shift
if ! all_steps=$(steps_for "$component"); then
  echo "$PROG: unknown component '$component' (one of: $COMPONENTS)" >&2
  exit 2
fi
# Everything after the first `--` passes to go test verbatim, so it is split
# off before the step names are read.
daemon_test_args=()
for ((i = 1; i <= $#; i++)); do
  if [ "${!i}" = -- ]; then
    daemon_test_args=("${@:i+1}")
    set -- "${@:1:i-1}"
    if [ "$component" != daemon ] || [ "$#" -ne 1 ] || [ "$1" != test ] ||
        [ "${#daemon_test_args[@]}" -eq 0 ]; then
      echo "$PROG: '--' must follow 'daemon test' as its only step and precede at least one go test argument" >&2
      exit 2
    fi
    break
  fi
done
if [ "$#" -eq 0 ]; then
  # shellcheck disable=SC2086 # deliberate word split of the step list
  set -- $all_steps
fi
for step in "$@"; do
  case " $all_steps " in
    *" $step "*) ;;
    *)
      echo "$PROG: unknown step '$step' for $component (one of: $all_steps)" >&2
      exit 2
      ;;
  esac
done

if [ "$component" = daemon ]; then
  # Shares compiled packages and test results across checkouts; see the
  # header. Appending keeps a caller's flags, such as a ward recipe's -p=2.
  export GOFLAGS="${GOFLAGS:+$GOFLAGS }-trimpath"
  case " $* " in
    *' test '*) daemon_test_read_partitions ;;
  esac
  daemon_gocache_read_limit
  daemon_trim_go_cache
fi

for step in "$@"; do
  echo "== $component $step" >&2
  "${component}_${step//-/_}"
done
if [ "${#daemon_test_args[@]}" -eq 0 ]; then
  echo "PASS: $component ${*}"
else
  echo "PASS: $component ${*} -- ${daemon_test_args[*]}"
fi
