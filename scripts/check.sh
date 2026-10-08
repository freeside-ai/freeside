#!/usr/bin/env bash
# check.sh — the single entry point for a component's standard checks.
#
# Usage: check.sh <component> [step...]
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
#   app          generate format test build-mac build-ios
#   api          lint digest
#   scripts      syntax shellcheck suites vocabulary trackercollect
#   convergence  run
#   docs         plan-links
#
# Tool overrides, for CI runners that install pinned binaries and for
# local machines with a tool outside PATH:
#   GOLANGCI_LINT  command for golangci-lint (default: golangci-lint;
#                  CI pins v2.12.2 through golangci-lint-action)
#   SHELLCHECK     command for shellcheck (default: shellcheck; CI pins a
#                  checksum-verified binary in .github/workflows/scripts-ci.yml)
#   VACUUM         command for the OpenAPI linter (default: `go run`
#                  of the pinned vacuum module, ~7 minutes cold)
#
# Local daemon test runs:
#   DAEMON_TEST_MAX_RUNS   simultaneous test steps (default: 1, or 0 when
#                          CI is non-empty); 0 turns the limit off
#   DAEMON_TEST_STATE_DIR  shared per-user state directory (default:
#                          ${XDG_CACHE_HOME:-$HOME/.cache}/freeside/daemon-test)
# A queued run names the limit, state directory, and holders every 30 seconds.
# Outside CI on macOS, when git resolves to /usr/bin/git, the test step uses
# xcrun's git through a stable directory containing only git on PATH, with
# its helpers and config preserved. Other git installations and later steps
# are unchanged.
#
# The daemon's opt-in live suites are skipped by `go test` unless their
# environment is set (FREESIDE_PUBLISH_LIVE_TEST, FREESIDE_WARD_LIVE_TEST,
# FREESIDE_CLAUDE_TOKEN_LIVE_TEST, FREESIDE_CODEX_ENROLLMENT_LIVE_TEST,
# FREESIDE_REAL_RUN_LIVE_TEST, FREESIDE_MARKDOWN_LIVE_TEST,
# FREESIDE_DRIFT_REPLAY_LIVE_TEST); each test's skip message lists the rest
# of its environment. They are CI-blind by design.
#
# Exit codes:
#   0  every requested step passed
#   1  a step failed (its command's output says which)
#   2  usage error: unknown component or step, or malformed daemon test limit
set -euo pipefail

PROG=$(basename "$0")
ROOT=$(cd "$(dirname "$0")/.." && pwd)

GOLANGCI_LINT=${GOLANGCI_LINT:-golangci-lint}
SHELLCHECK=${SHELLCHECK:-shellcheck}
VACUUM=${VACUUM:-go run github.com/daveshanley/vacuum@v0.29.9}

COMPONENTS='daemon app api scripts convergence docs'
steps_for() { # <component>; prints the component's steps in default order
  case $1 in
    daemon) echo 'build test vet lint' ;;
    app) echo 'generate format test build-mac build-ios' ;;
    api) echo 'lint digest' ;;
    scripts) echo 'syntax shellcheck suites vocabulary trackercollect' ;;
    convergence) echo 'run' ;;
    docs) echo 'plan-links' ;;
    *) return 1 ;;
  esac
}

usage() {
  echo "usage: $PROG <component> [step...] | $PROG --list" >&2
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
daemon_test_run() { in_dir "$ROOT/daemon" go test ./...; }

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

daemon_test() (
  daemon_test_use_real_git
  daemon_test_take_slot
  # Bash 3.2 saves a redirection on another descriptor if applied to the
  # function call. Close it inside a subshell so orphaned children can't
  # inherit either the slot descriptor or a saved copy.
  ( exec 9>&-; daemon_test_run )
)
daemon_vet() { in_dir "$ROOT/daemon" go vet ./...; }
daemon_lint() {
  # shellcheck disable=SC2086 # GOLANGCI_LINT may carry arguments
  in_dir "$ROOT/daemon" $GOLANGCI_LINT run
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
app_test() { in_dir "$ROOT/app" swift test --only-use-versions-from-resolved-file; }
xcodebuild_scheme() { # <scheme> <destination> [build-setting...]
  local scheme=$1 destination=$2
  shift 2
  in_dir "$ROOT/app" xcodebuild -project Freeside.xcodeproj -scheme "$scheme" \
    -destination "$destination" -onlyUsePackageVersionsFromResolvedFile \
    -skipPackageUpdates -skipPackagePluginValidation \
    CODE_SIGNING_ALLOWED=NO COMPILER_INDEX_STORE_ENABLE=NO "$@" build
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
  in_dir "$ROOT" $VACUUM lint -r api/vacuum.ruleset.yaml --details \
    --fail-severity warn api/openapi.yaml
}
api_digest() { in_dir "$ROOT" bash scripts/api-contract-digest.sh --check; }

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
scripts_trackercollect() {
  in_dir "$ROOT" go -C scripts/trackercollect build ./...
  in_dir "$ROOT" go -C scripts/trackercollect test ./...
  in_dir "$ROOT" go -C scripts/trackercollect vet ./...
}

# --- convergence -----------------------------------------------------------

convergence_run() { in_dir "$ROOT" bash scripts/run-convergence.sh; }

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

for step in "$@"; do
  echo "== $component $step" >&2
  "${component}_${step//-/_}"
done
echo "PASS: $component ${*}"
