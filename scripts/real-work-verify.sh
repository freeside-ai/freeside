#!/usr/bin/env bash
# A retained, read-only verifier. Failure leaves the owned daemon running.
set -euo pipefail
umask 077
session=$(cd "$(dirname "$0")" && pwd)
mode=${1:-publication}
[[ "$mode" == publication || "$mode" == retained ]] || exit 2
# This file is generated with shell-quoted, explicitly selected configuration
# by the harness, never copied from an operator's arbitrary launch script.
# shellcheck source=/dev/null
source "$session/verification-env.sh"
report=$(mktemp "$session/verification.XXXXXX")
checkpoint="$report.json"
retained=0
[[ "$mode" != retained ]] || retained=1
if ! env -u FREESIDE_REAL_RUN_RUN_ID -u FREESIDE_REAL_RUN_INVOCATION \
    FREESIDE_REAL_RUN_LIVE_TEST=1 FREESIDE_REAL_RUN_RETAINED="$retained" \
    FREESIDE_REAL_RUN_CHECKPOINT_PATH="$checkpoint" \
    "$session/verify-real-run" -test.run '^TestRealWorkItemCompletesProductionPipeline$' \
    -test.count=1 -test.v >"$report" 2>&1; then
    echo "Publication verification failed; diagnostics retained: $report" >&2
    exit 1
fi
if [[ ! -s "$checkpoint" ]] || ! python3 "$session/real-work-retained.py" remote "$checkpoint" >>"$report" 2>&1; then
    echo "Remote publication not verified; diagnostics retained: $report" >&2
    exit 1
fi
cat "$checkpoint"
echo "Verification evidence: $report"
