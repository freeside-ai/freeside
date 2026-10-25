#!/usr/bin/env bash
# run-convergence.sh — the §5.14 real-daemon convergence pass (issue #72).
#
# Usage: run-convergence.sh [--all-tests] [--skip-build]
#
# Builds the freeside-signet-dev harness from daemon/, launches it on
# loopback with a temporary state directory, reads the readiness line
# for the two bound URLs, and runs the FreesideConvergenceTests suite
# against it. Without those URLs in the environment the suite skips,
# so this script is the one entry point that actually exercises the
# client halves of the sixteen-test matrix against a real daemon, plus
# the cross-language enumeration that proves the Swift fixture matrix and
# the daemon's per-type action policy agree over every attention type (#204).
#
# --all-tests runs every Swift test suite, not only the convergence suite.
# CI uses it so one macOS job covers the whole Darwin-only suite
# (FreesideCoreTests, the screenshot regressions) and the convergence pass;
# the build is the same either way, since `--filter` selects at run time
# (#2248). It builds the test bundle once, then runs every suite but
# ScreenshotRegressionTests in one `swift test` while the screenshot matrix
# renders as concurrent slices beside it, each slice a `swift test` with
# FREESIDE_SCREENSHOT_SHARD=<index>/<count>. The matrix renders serially on
# the main actor, so one process left two of the runner's three cores idle
# for most of the job (#2292). Each slice runs from its own clone of
# app/.build through `--scratch-path` (`cp -c`: an APFS clone with no extra
# disk, or a full copy where the volume can't clone), because
# SwiftPM holds the app/.build lock for a whole `swift test` run, so
# concurrent runs on one scratch path would run one after another.
# CONVERGENCE_SCREENSHOT_SHARDS sets the slice count: default the smaller of
# the core count and 2, because on the three-core runner two slices beat
# three, whose extra process slowed the other suites more than it sped the
# matrix (#2292); 1 runs the suite whole in one process.
#
# --skip-build runs the test bundle already in app/.build instead of
# building one. CI sets it only on an exact hit of the Swift build cache,
# whose key hashes all of app/, so the bundle was built from this tree
# (#2291). The harness is always built here.
#
# Requires: Go (daemon toolchain), Swift (app toolchain), macOS.
set -euo pipefail

all_tests=""
skip_build=""
for argument in "$@"; do
  case "$argument" in
    --all-tests) all_tests=1 ;;
    --skip-build) skip_build=1 ;;
    *)
      echo "run-convergence: unknown argument: $argument" >&2
      exit 2
      ;;
  esac
done

# The convergence target exists only on Darwin (app/Package.swift), and
# SwiftPM's --filter exits 0 when nothing matches, so off macOS this
# script would report success having run zero tests.
if [[ "$(uname)" != "Darwin" ]]; then
  echo "run-convergence: requires macOS (FreesideConvergenceTests is Darwin-only)" >&2
  exit 1
fi

shards=""
if [[ -n "$all_tests" ]]; then
  cores="$(sysctl -n hw.ncpu)"
  shards="${CONVERGENCE_SCREENSHOT_SHARDS:-$((cores < 2 ? cores : 2))}"
  if [[ ! "$shards" =~ ^[1-9][0-9]*$ ]]; then
    echo "run-convergence: CONVERGENCE_SCREENSHOT_SHARDS must be a positive integer, got: $shards" >&2
    exit 2
  fi
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/supervised-paths.sh
source "$repo_root/scripts/supervised-paths.sh"
# mktemp puts the harness state under TMPDIR; refuse a supervised one before
# anything is created there.
refuse_supervised_path "run-convergence TMPDIR" "${TMPDIR:-/tmp}" || exit 2
workdir="$(mktemp -d)"
harness_pid=""
# The --all-tests runs, which write to logs under $workdir. Cleanup prints
# the logs if the script ends before it has, so a cancelled or timed-out
# run still shows its output.
run_pids=()
run_logs=()
logs_printed=""

print_logs() {
  logs_printed=1
  local log
  for log in "${run_logs[@]}"; do
    echo "==> ${log##*/} <=="
    cat "$log"
    # A run killed mid-line leaves no final newline.
    if [[ -n "$(tail -c 1 "$log")" ]]; then
      echo
    fi
  done
}

cleanup() {
  if ((${#run_pids[@]} > 0)); then
    local pid
    for pid in "${run_pids[@]}"; do
      pkill -P "$pid" 2>/dev/null || true
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    done
  fi
  if [[ -z "$logs_printed" ]] && ((${#run_logs[@]} > 0)); then
    print_logs
  fi
  if [[ -n "$harness_pid" ]] && kill -0 "$harness_pid" 2>/dev/null; then
    kill "$harness_pid" 2>/dev/null || true
    wait "$harness_pid" 2>/dev/null || true
  fi
  rm -rf "$workdir"
}
trap cleanup EXIT

echo "building freeside-signet-dev" >&2
(cd "$repo_root/daemon" && go build -o "$workdir/freeside-signet-dev" ./cmd/freeside-signet-dev)

# Pairing now grants a usable ntfy subscription. The convergence matrix does
# not publish notifications, so a valid loopback sink is enough to compose
# the same required contract without introducing network access.
"$workdir/freeside-signet-dev" -db "$workdir/signet.db" -ntfy-url "http://127.0.0.1:1" > "$workdir/readiness.json" &
harness_pid=$!

# The harness prints one JSON readiness line on stdout once both
# listeners are bound; give it a bounded window. Wait for the trailing
# newline, not just first bytes, so a partially flushed line never
# reaches the extraction below.
ready=""
for _ in $(seq 1 50); do
  if [[ -s "$workdir/readiness.json" ]] && grep -q '}' "$workdir/readiness.json"; then
    ready=1
    break
  fi
  if ! kill -0 "$harness_pid" 2>/dev/null; then
    echo "run-convergence: harness exited before readiness" >&2
    exit 1
  fi
  sleep 0.1
done
if [[ -z "$ready" ]]; then
  echo "run-convergence: no readiness line within 5s" >&2
  exit 1
fi

api_url="$(sed -n 's/.*"api_url":"\([^"]*\)".*/\1/p' "$workdir/readiness.json")"
control_url="$(sed -n 's/.*"control_url":"\([^"]*\)".*/\1/p' "$workdir/readiness.json")"
if [[ -z "$api_url" || -z "$control_url" ]]; then
  echo "run-convergence: malformed readiness line: $(cat "$workdir/readiness.json")" >&2
  exit 1
fi
echo "harness ready: api=$api_url control=$control_url" >&2

# A filter that matches nothing, or a suite that skips (env not seen),
# still exits 0, so demand positive evidence that the suite ran and
# passed before this command counts as a convergence pass. The same
# evidence is demanded of an --all-tests run, where the suite is one of
# many. The command line goes to stderr so a CI log shows whether the
# build was skipped.
swift_args=(--only-use-versions-from-resolved-file)
if [[ -z "$all_tests" ]]; then
  swift_args+=(--filter FreesideConvergenceTests)
fi
if [[ -n "$skip_build" ]]; then
  swift_args+=(--skip-build)
fi
if [[ -z "$all_tests" ]]; then
  echo "running: swift test ${swift_args[*]}" >&2
  test_log="$workdir/swift-test.log"
  (
    cd "$repo_root/app" &&
      FREESIDE_CONVERGENCE_URL="$api_url" \
      FREESIDE_CONVERGENCE_CONTROL_URL="$control_url" \
      swift test "${swift_args[@]}"
  ) 2>&1 | tee "$test_log"
  if ! grep -q "Suite RealDaemonConvergenceTests passed" "$test_log"; then
    echo "run-convergence: the convergence suite did not run to a pass (zero tests matched, or the suite skipped)" >&2
    exit 1
  fi
  exit 0
fi

# --all-tests: build once, then run every suite but the screenshot
# regressions on app/.build while the regressions run as slices, each in
# its own APFS clone of app/.build (see the header). Every run takes
# swift_args, so a flag added there reaches each of them.
run_args=("${swift_args[@]}")
if [[ -z "$skip_build" ]]; then
  echo "running: swift build --build-tests --only-use-versions-from-resolved-file" >&2
  (
    cd "$repo_root/app" &&
      swift build --build-tests --only-use-versions-from-resolved-file
  ) 2>&1 | tee "$workdir/swift-build.log"
  run_args+=(--skip-build)
fi
main_args=("${run_args[@]}" --skip ScreenshotRegressionTests)

# Wait for every clone before failing, so cleanup never removes a
# directory a clone is still writing.
clone_start=$SECONDS
clone_pids=()
for ((i = 0; i < shards; i++)); do
  cp -c -R "$repo_root/app/.build" "$workdir/shard-$i" &
  clone_pids+=($!)
done
clone_failed=""
for pid in "${clone_pids[@]}"; do
  if ! wait "$pid"; then
    clone_failed=1
  fi
done
if [[ -n "$clone_failed" ]]; then
  echo "run-convergence: cloning app/.build failed" >&2
  exit 1
fi
echo "cloned app/.build $shards times in $((SECONDS - clone_start))s" >&2

echo "$(date +%T) starting: swift test ${main_args[*]}" >&2
(
  cd "$repo_root/app" &&
    FREESIDE_CONVERGENCE_URL="$api_url" \
    FREESIDE_CONVERGENCE_CONTROL_URL="$control_url" \
    swift test "${main_args[@]}"
) >"$workdir/main.log" 2>&1 &
run_pids+=($!)
run_logs+=("$workdir/main.log")
for ((i = 0; i < shards; i++)); do
  # Slice 0 runs the whole suite, so its other tests run once; the rest
  # run only the matrix comparison.
  filter="ScreenshotRegressionTests/surfacesMatchRecordedPixels"
  if ((i == 0)); then
    filter="ScreenshotRegressionTests"
  fi
  shard_args=("${run_args[@]}" --scratch-path "$workdir/shard-$i" --filter "$filter")
  selector=""
  if ((shards > 1)); then
    selector="$i/$shards"
  fi
  echo "$(date +%T) starting: ${selector:+FREESIDE_SCREENSHOT_SHARD=$selector }swift test ${shard_args[*]}" >&2
  (
    cd "$repo_root/app"
    unset FREESIDE_SCREENSHOT_SHARD
    if [[ -n "$selector" ]]; then
      export FREESIDE_SCREENSHOT_SHARD="$selector"
    fi
    swift test "${shard_args[@]}"
  ) >"$workdir/shard-$i.log" 2>&1 &
  run_pids+=($!)
  run_logs+=("$workdir/shard-$i.log")
done

# Wait for every run before printing, so one failure doesn't cut the
# other logs short, then print the logs one after another.
failed=()
for index in "${!run_pids[@]}"; do
  if ! wait "${run_pids[$index]}"; then
    failed+=("${run_logs[$index]##*/}")
  fi
done
run_pids=()
echo "$(date +%T) all runs finished" >&2
print_logs

if ((${#failed[@]} > 0)); then
  echo "run-convergence: swift test failed in ${failed[*]}; see the logs above" >&2
  exit 1
fi
# A filter that matches nothing still passes, so every run must show its
# suite's pass line.
if ! grep -q "Suite RealDaemonConvergenceTests passed" "$workdir/main.log"; then
  echo "run-convergence: the convergence suite did not run to a pass (zero tests matched, or the suite skipped)" >&2
  exit 1
fi
for ((i = 0; i < shards; i++)); do
  if ! grep -q "Suite ScreenshotRegressionTests passed" "$workdir/shard-$i.log"; then
    echo "run-convergence: screenshot slice $i did not run to a pass" >&2
    exit 1
  fi
done
