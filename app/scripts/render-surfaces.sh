#!/usr/bin/env bash
# Render only the named screenshot surfaces, to look at a visible change
# while iterating without paying for the whole matrix. Wraps the suite's
# selected-key probe: it never records baselines, compares digests, or
# touches the manifest, so the full suite stays the check before push
# (app/README.md, Iterating on a Visible Change).
# Usage: render-surfaces.sh OUTPUT_DIR SURFACE...
# SURFACE is a primary-surface name without its text-size suffix, such as
# decision-blocked; supplemental fixtures are not probe inputs.
# FREESIDE_RENDER_SIZES is a comma-separated list of the suite's text sizes
# (xsmall, large, xxxlarge, ax1, ax3, ax5) and defaults to large.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
if [[ $# -lt 2 ]]; then
  echo 'usage: render-surfaces.sh OUTPUT_DIR SURFACE...' >&2
  exit 2
fi
[[ $(uname -s) == Darwin ]] || { echo 'screenshot rendering requires macOS' >&2; exit 2; }
[[ ${FREESIDE_RECORD_SCREENSHOTS:-0} != 1 ]] || { echo 'recording is forbidden for a selected render' >&2; exit 2; }
mkdir -p "$1"
OUTPUT=$(cd "$1" && pwd -P)
shift

IFS=, read -r -a sizes <<< "${FREESIDE_RENDER_SIZES:-large}"
keys=()
for surface in "$@"; do
  for size in "${sizes[@]}"; do keys+=("$surface-$size"); done
done
KEYS=$(IFS=,; echo "${keys[*]}")

# Pin the probe to one plain capture per key, so an exported diagnostic
# setting cannot change which pixels or file names come back.
LOG=$OUTPUT/render-surfaces.log
status=0
env -u FREESIDE_SCREENSHOT_PROBE_CONTRAST \
  FREESIDE_SCREENSHOT_PROBE_KEYS="$KEYS" FREESIDE_SCREENSHOT_PROBE_MODE=selected \
  FREESIDE_SCREENSHOT_PROBE_REPETITIONS=1 FREESIDE_SCREENSHOT_OUTPUT="$OUTPUT" \
  swift test --package-path "$ROOT/app" --only-use-versions-from-resolved-file \
  --filter ScreenshotRegressionTests/probeScreenshotDeterminism > "$LOG" 2>&1 || status=$?

# The probe prints one result line per capture and then writes its image.
missing=()
for key in "${keys[@]}"; do
  if ! grep -qF "SCREENSHOT_RESULT $key " "$LOG" || [[ ! -f $OUTPUT/$key.png ]]; then
    missing+=("$key")
  fi
done
if [[ $status -ne 0 || ${#missing[@]} -ne 0 ]]; then
  {
    echo "render failed: swift test exit $status, ${#missing[@]} of ${#keys[@]} keys not rendered"
    if [[ ${#missing[@]} -ne 0 ]]; then
      printf '  not rendered: %s\n' "${missing[@]}"
      echo '  the probe rejects a key that names no primary surface at that text size'
    fi
    grep -E 'error:|invalidDiagnosticSelection' "$LOG" | head -n 10 || true
    echo "log: $LOG"
  } >&2
  exit 1
fi
for key in "${keys[@]}"; do echo "$OUTPUT/$key.png"; done
