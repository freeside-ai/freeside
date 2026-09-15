#!/usr/bin/env bash
# Behavioral coverage for the daemon flag and private, retained operator bytes.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=scripts/real-work-manual-submission.sh
source "$root/scripts/real-work-manual-submission.sh"
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
mkdir "$temp/first" "$temp/second" "$temp/disabled" "$temp/legacy" "$temp/from-legacy"
for session in first second disabled from-legacy; do
  cp "$root/scripts/real-work-retained.py" "$temp/$session/"
done
printf '%s\n' '{"version":1,"projects":[]}' > "$temp/operator file.json"
real_work_stage_manual_submission "$temp/operator file.json" "" "$temp/first"
python3 - "$temp/first" "${manual_submission_args[@]}" <<'PY'
import json, os, stat, sys
from pathlib import Path
session = Path(sys.argv[1])
assert sys.argv[2:] == ['-manual-submission-config', str(session / 'submission-inputs/manual-submission.json')]
assert stat.S_IMODE((session / 'submission-inputs/manual-submission.json').stat().st_mode) == 0o600
assert stat.S_IMODE((session / 'manual-submission-input.json').stat().st_mode) == 0o600
assert stat.S_IMODE((session / 'submission-inputs').stat().st_mode) == 0o700
PY
printf 'changed original\n' > "$temp/operator file.json"
real_work_stage_manual_submission "" "$temp/first" "$temp/second"
cmp "$temp/first/submission-inputs/manual-submission.json" "$manual_submission_file"
real_work_stage_manual_submission "" "" "$temp/disabled"
[[ ${#manual_submission_args[@]} == 0 && -z "$manual_submission_file" ]]
real_work_stage_manual_submission "" "$temp/legacy" "$temp/from-legacy"
[[ ${#manual_submission_args[@]} == 0 && -z "$manual_submission_file" ]]
printf 'PASS: manual submission flag, private staging, retained reuse, and legacy absence\n'
