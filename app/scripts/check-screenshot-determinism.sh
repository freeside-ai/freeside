#!/usr/bin/env bash
# Keep every outcome from the #1698 fixed-commit experiment. Never records
# baselines or retries a failed capture. Requires the local baseline Mac.
# Usage: check-screenshot-determinism.sh OUTPUT_DIR LOAD_CHECKOUT
#        check-screenshot-determinism.sh --probes-only OUTPUT_DIR
# FREESIDE_DETERMINISM_RUNS defaults to 5; smaller values are smoke checks,
# not the five-quiet/five-loaded acceptance evidence.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
PROBES_ONLY=0
if [[ ${1:-} == --probes-only ]]; then
  PROBES_ONLY=1
  shift
fi
if [[ $# -ne $((2 - PROBES_ONLY)) ]]; then
  echo 'usage: check-screenshot-determinism.sh OUTPUT_DIR LOAD_CHECKOUT | --probes-only OUTPUT_DIR' >&2
  exit 2
fi
OUTPUT=$1
RUNS=${FREESIDE_DETERMINISM_RUNS:-5}
[[ $RUNS =~ ^[1-9][0-9]*$ ]] || { echo 'run count must be a positive integer' >&2; exit 2; }
[[ $(uname -s) == Darwin ]] || { echo 'screenshot evidence requires macOS' >&2; exit 2; }
[[ ${FREESIDE_RECORD_SCREENSHOTS:-0} != 1 ]] || { echo 'recording is forbidden in this experiment' >&2; exit 2; }
if [[ $PROBES_ONLY == 0 ]]; then
  LOAD_ROOT=$(git -C "$2" rev-parse --show-toplevel)
  [[ $(cd "$LOAD_ROOT" && pwd -P) != $(cd "$ROOT" && pwd -P) ]] || {
    echo 'load builds require another checkout' >&2; exit 2;
  }
  [[ -z $(git -C "$ROOT" status --porcelain) ]] || {
    echo 'commit the implementation before collecting fixed-commit evidence' >&2; exit 2;
  }
fi
mkdir "$OUTPUT"
OUTPUT=$(cd "$OUTPUT" && pwd -P)
cd "$ROOT"
TESTED_HEAD=$(git rev-parse HEAD)
{
  git rev-parse HEAD
  git status --short
  sw_vers
  xcodebuild -version
  swift --version
  printf 'runs=%s probes_only=%s\n' "$RUNS" "$PROBES_ONLY"
  printf 'timestamp_unit=Unix epoch nanoseconds\n'
  if [[ $PROBES_ONLY == 0 ]]; then
    printf 'load_checkout=%s\n' "$LOAD_ROOT"
    git -C "$LOAD_ROOT" rev-parse HEAD
    git -C "$LOAD_ROOT" status --short
  fi
} > "$OUTPUT/environment.txt"
FAILED=0

monitored_command() {
  python3 - "$@" <<'PY'
import json
import pathlib
import subprocess
import sys
import time

report, mode, *command = sys.argv[1:]
child = subprocess.Popen(command)
contaminated = False
with open(report, 'a') as output:
    while child.poll() is None:
        listing = subprocess.check_output(['ps', '-Ao', 'pid,ppid,comm'], text=True)
        processes = [line.strip().split(None, 2) for line in listing.splitlines()[1:]]
        descendants = {child.pid}
        while True:
            expanded = descendants | {int(pid) for pid, parent, _ in processes if int(parent) in descendants}
            if expanded == descendants:
                break
            descendants = expanded
        external = [(int(pid), command) for pid, _, command in processes
                    if int(pid) not in descendants and pathlib.Path(command).name in
                    {'swift-frontend', 'swift-driver', 'swift-build', 'clang', 'xcodebuild'}]
        compilers = [(int(pid), command) for pid, _, command in processes
                     if int(pid) in descendants and pathlib.Path(command).name in
                     {'swift-frontend', 'clang'}]
        output.write(json.dumps({'time_ns': time.time_ns(), 'external_builders': external,
                                 'active_compilers': compilers}) + '\n')
        output.flush()
        contaminated |= mode == 'quiet' and bool(external)
        time.sleep(1 if mode == 'quiet' else 0.1)
status = child.wait()
if contaminated:
    print('Quiet run invalid: an external build was observed during the command.', file=sys.stderr)
sys.exit((status if status >= 0 else 128 - status) or (1 if contaminated else 0))
PY
}

run_log() {
  local name=$1 started ended result
  shift
  started=$(python3 -c 'import time; print(time.time_ns())')
  { printf 'command:'; printf ' %q' "$@"; printf '\n'; } > "$OUTPUT/$name.command"
  if [[ $name == quiet-* ]]; then
    if monitored_command "$OUTPUT/$name.processes" quiet "$@" > "$OUTPUT/$name.log" 2>&1; then result=0; else result=$?; fi
  elif [[ $name == build-load-* ]]; then
    if monitored_command "$OUTPUT/$name.processes" load "$@" > "$OUTPUT/$name.log" 2>&1; then result=0; else result=$?; fi
  elif "$@" > "$OUTPUT/$name.log" 2>&1; then result=0; else result=$?; fi
  ended=$(python3 -c 'import time; print(time.time_ns())')
  printf '%s\t%s\t%s\t%s\n' "$name" "$started" "$ended" "$result" >> "$OUTPUT/results.tsv"
  printf '%s: exit %s (%sms)\n' "$name" "$result" "$(((ended - started) / 1000000))"
  return "$result"
}

probe() {
  local name=$1 keys=$2 mode=$3
  run_log "$name" env FREESIDE_SCREENSHOT_PROBE_KEYS="$keys" \
    FREESIDE_SCREENSHOT_TRACE=1 \
    FREESIDE_SCREENSHOT_PROBE_MODE="$mode" FREESIDE_SCREENSHOT_OUTPUT="$OUTPUT/$name" \
    swift test --package-path "$ROOT/app" --skip-build \
    --filter ScreenshotRegressionTests/probeScreenshotDeterminism
}

run_log build swift test --package-path "$ROOT/app" --only-use-versions-from-resolved-file \
  --filter 'ScreenshotCaptureTests|explicitContrastReachesCapture|screenshotContrastLeavesExisting' || exit 1

# #1698 surfaces; replace these only with keys from the current supported
# primary-surface manifest. Supplemental fixtures are not probe inputs.
KEYS='decision-execution_failure-xsmall,decision-ready_for_final_review-inspector-xsmall,decision-ready_for_final_review-inspector-xxxlarge,attachment-states-ax1,attachment-states-ax5,message-composer-ax3,tasks-names-light-standard-large,tasks-names-light-increased-large,task-progress-0-390-light-large,task-progress-0-390-dark-large,task-history-revised-390-light-xsmall,new-task-sheet-phone-ax5'
for mode in selected reverse construct render; do
  probe "$mode" "$KEYS" "$mode" || FAILED=1
done
IFS=, read -r -a individual_keys <<< "$KEYS"
for key in "${individual_keys[@]}"; do
  probe "alone-$key" "$key" selected || FAILED=1
done

# SwiftPM locks a scratch directory throughout testing. Clone the completed
# build into two separate scratch directories, then run both without building.
# Each process has its own outputs; all package/source inputs stay identical.
for contrast in standard increased; do
  cp -cR "$ROOT/app/.build" "$OUTPUT/process-$contrast-build"
done
run_contrast() {
  local contrast=$1
  # Keep capturing across SwiftPM's variable startup time. The verifier below
  # still requires measured render overlap, regardless of this repetition count.
  run_log "process-$contrast" env \
    FREESIDE_SCREENSHOT_TRACE=1 \
    FREESIDE_SCREENSHOT_PROBE_KEYS="tasks-names-light-$contrast-large" \
    FREESIDE_SCREENSHOT_PROBE_MODE=selected \
    FREESIDE_SCREENSHOT_PROBE_REPETITIONS=500 \
    FREESIDE_SCREENSHOT_OUTPUT="$OUTPUT/process-$contrast" \
    swift test --package-path "$ROOT/app" --scratch-path "$OUTPUT/process-$contrast-build" \
    --skip-build --filter ScreenshotRegressionTests/probeScreenshotDeterminism
}
run_contrast standard &
standard_pid=$!
run_contrast increased &
increased_pid=$!
wait "$standard_pid" || FAILED=1
wait "$increased_pid" || FAILED=1

if [[ $PROBES_ONLY == 0 ]]; then
  for ((run = 1; run <= RUNS; run++)); do
    ps -Ao pid,ppid,comm > "$OUTPUT/quiet-$run.before-processes"
    if grep -E '/(swift-frontend|swift-driver|swift-build|clang|xcodebuild)$' "$OUTPUT/quiet-$run.before-processes"; then
      echo "quiet-$run blocked: another build is running" >&2
      FAILED=1
      break
    fi
    run_log "quiet-$run" env FREESIDE_SCREENSHOT_TRACE=1 \
      FREESIDE_SCREENSHOT_OUTPUT="$OUTPUT/quiet-$run" bash scripts/check.sh app test || FAILED=1
  done
  for ((run = 1; run <= RUNS; run++)); do
    # A new, explicit build directory makes each load run perform real work.
    run_log "build-load-$run" swift build --package-path "$LOAD_ROOT/app" \
      --scratch-path "$OUTPUT/load-$run-build" --configuration release --jobs 2 \
      --only-use-versions-from-resolved-file &
    load_pid=$!
    run_log "loaded-$run" env FREESIDE_SCREENSHOT_TRACE=1 \
      FREESIDE_SCREENSHOT_OUTPUT="$OUTPUT/loaded-$run" bash scripts/check.sh app test || FAILED=1
    wait "$load_pid" || FAILED=1
  done
  if [[ $(git rev-parse HEAD) != "$TESTED_HEAD" || -n $(git status --porcelain) ]]; then
    echo 'implementation changed during the experiment; fixed-commit evidence is invalid' >&2
    FAILED=1
  fi
fi

# Compare diagnostic output to the same manifest the complete local matrix
# checks, and require measured overlap rather than inferring it from load.
if ! python3 - "$OUTPUT" "$ROOT" "$RUNS" "$PROBES_ONLY" <<'PY'
import json
import pathlib
import sys

output, root = map(pathlib.Path, sys.argv[1:3])
runs, probes_only = map(int, sys.argv[3:5])
expected = json.loads((root / 'app/Tests/FreesideCoreTests/Resources/ScreenshotDigests.json').read_text())
rows = {fields[0]: tuple(map(int, fields[1:])) for fields in
        (line.split('\t') for line in (output / 'results.tsv').read_text().splitlines())}
probes = ['selected', 'reverse', 'construct', 'render', 'process-standard', 'process-increased']
probes += [name for name in rows if name.startswith('alone-')]
for name in probes:
    results = [line.split() for line in (output / f'{name}.log').read_text().splitlines()
               if line.startswith('SCREENSHOT_RESULT ')]
    assert results, f'{name}: no captured keys'
    for _, key, dimensions, digest in results:
        assert expected[key] == digest, f'{name}: {key} differs from the complete matrix baseline'
def overlap(first, second):
    a, b = rows[first], rows[second]
    assert max(a[0], b[0]) < min(a[1], b[1]), f'no overlap: {first}, {second}'
overlap('process-standard', 'process-increased')
def captures(name):
    return [tuple(map(int, fields[2:])) for fields in
            (line.split() for line in (output / f'{name}.log').read_text().splitlines())
            if fields and fields[0] == 'SCREENSHOT_CAPTURE']
assert any(max(a[0], b[0]) < min(a[1], b[1])
           for a in captures('process-standard') for b in captures('process-increased')), \
    'opposite-contrast processes never captured concurrently'
if not probes_only:
    for run in range(1, runs + 1):
        assert f'quiet-{run}' in rows, f'quiet-{run} was not run'
        overlap(f'build-load-{run}', f'loaded-{run}')
        observations = [json.loads(line) for line in
                        (output / f'build-load-{run}.processes').read_text().splitlines()]
        assert any(start <= observation['time_ns'] <= end
                   for start, end in captures(f'loaded-{run}')
                   for observation in observations if observation['active_compilers']), \
            f'loaded-{run}: no active load compiler observed during a capture'
        for phase in ['quiet', 'loaded']:
            name = f'{phase}-{run}'
            samples = [line.split() for line in (output / f'{name}.log').read_text().splitlines()
                       if line.startswith('SCREENSHOT_SAMPLE ')]
            final = {key: digest for _, key, _, _, digest in samples if key in expected}
            assert final == expected, f'{name}: full matrix evidence is missing or differs'
assert all(row[2] == 0 for row in rows.values()), 'one or more commands failed'
print('All recorded captures, full test runs, and overlap checks passed.')
PY
then FAILED=1; fi
exit "$FAILED"
