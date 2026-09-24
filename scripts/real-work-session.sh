#!/usr/bin/env bash
# Usage: real-work-session.sh complete|recover|verify <retained-session-directory> [installed-daemon-path]
#        real-work-session.sh select-target <client-target-session-directory> <task-id>
#        real-work-session.sh verify-source <client-target-session-directory> <expected-issue-url>
# Recovery never signals a stored PID. The rig command proves stale ownership,
# database/listener exclusion and exact-resource cleanup before clearing a gate.
# select-target only records the operator's chosen client task; the foreground
# client-target harness validates and reports it. It never touches the database.
set -euo pipefail
umask 077
action=${1:-}
session=${2:-}
if [[ "$action" != complete && "$action" != recover && "$action" != verify && "$action" != select-target && "$action" != verify-source ]] || [[ ! -f "$session/status" ]]; then
	echo 'usage: real-work-session.sh complete|recover|verify <session-directory>' >&2
	echo '       real-work-session.sh select-target <session-directory> <task-id>' >&2
	echo '       real-work-session.sh verify-source <session-directory> <expected-issue-url>' >&2
	exit 2
fi
session=$(cd "$session" && pwd)
status=$(cat "$session/status")
if [[ "$action" == verify-source ]]; then
	if [[ -z "${3:-}" || $# != 3 ]]; then
		echo 'usage: real-work-session.sh verify-source <session-directory> <expected-issue-url>' >&2
		exit 2
	fi
	[[ -f "$session/mode" && "$(cat "$session/mode")" == client-target ]] || {
		echo 'Source verification requires a client-target session.' >&2; exit 1
	}
	[[ -f "$session/target.json" ]] || {
		echo 'Source verification requires an accepted target.json; leave specification approval pending.' >&2; exit 1
	}
	[[ -x "$session/verify-real-run" && -f "$session/verification-env.sh" ]] || {
		echo 'This session lacks a retained verifier or configuration; use a reviewed runtime/session.' >&2; exit 1
	}
	# Only the harness-generated configuration and accepted target select the
	# store and identities. Ambient variables must not fill missing saved inputs.
	unset FREESIDE_REAL_RUN_STATE_ROOT FREESIDE_REAL_RUN_PROJECT FREESIDE_REAL_RUN_APPROVED_RECIPE
	# shellcheck source=/dev/null
	source "$session/verification-env.sh"
	python3 - "$session" "$3" <<'PY'
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

session, expected = Path(sys.argv[1]), sys.argv[2]
def refuse(reason):
    raise SystemExit(reason + "; leave specification approval pending.")

try:
    target = json.loads((session / "target.json").read_text())
    keys = ("task_id", "project", "specification_run_id")
    if any(not isinstance(target.get(k), str) or not target[k] or
           any(c.isspace() for c in target[k]) for k in keys):
        refuse("Saved target has missing or invalid identities")
except (OSError, ValueError, AttributeError):
    refuse("Saved target is unreadable or malformed")
if any(not os.environ.get(k) for k in (
        "FREESIDE_REAL_RUN_STATE_ROOT", "FREESIDE_REAL_RUN_PROJECT",
        "FREESIDE_REAL_RUN_APPROVED_RECIPE")):
    refuse("Saved verification configuration is incomplete")
if target["project"] != os.environ["FREESIDE_REAL_RUN_PROJECT"]:
    refuse("Saved target and session project disagree")

# Each invocation needs a fresh result. A prior receipt is historical evidence
# only, including when an older verifier skips this unsupported mode.
fd, log = tempfile.mkstemp(prefix="source-verification.", dir=session)
receipt = log + ".json"
env = dict(os.environ, FREESIDE_REAL_RUN_CLIENT_TARGET="verify-source",
           FREESIDE_REAL_RUN_TARGET_TASK_ID=target["task_id"],
           FREESIDE_REAL_RUN_PROJECT=target["project"],
           FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID=target["specification_run_id"],
           FREESIDE_REAL_RUN_EXPECTED_SOURCE_ISSUE=expected,
           FREESIDE_REAL_RUN_TARGET_PATH=receipt)
with os.fdopen(fd, "w") as output:
    try:
        result = subprocess.run([str(session / "verify-real-run"),
                                 "-test.run", "^TestRealRunClientTarget$", "-test.count=1"],
                                env=env, stdout=output, stderr=subprocess.STDOUT)
    except OSError:
        refuse("Could not start retained verifier; diagnostics: " + log)
if result.returncode:
    refuse("Source verifier failed; diagnostics: " + log)
try:
    decision = json.loads(Path(receipt).read_text())
    if decision.get("outcome") != "source-verified":
        refuse("Source check refused: " + str(decision.get("reason", "no successful proof")))
    if (any(decision.get(k) != target[k] for k in keys) or
            decision.get("source_issue") != expected or
            decision.get("provenance") != "recommended" or
            not isinstance(decision.get("repository"), str) or
            not expected.startswith("https://github.com/" + decision["repository"] + "/issues/")):
        refuse("Source receipt does not match the selected task and expected issue")
except (OSError, ValueError, AttributeError):
    refuse("Retained verifier returned no readable source receipt; use a reviewed runtime/session. Diagnostics: " + log)
print("Source eligibility verified with recommended provenance; this grants no closing authority.")
print("Preapproval source receipt: " + receipt)
PY
	exit 0
fi
if [[ "$action" == select-target ]]; then
	target_task_id=${3:-}
	# A task ID with whitespace can never name a stored task and would corrupt
	# the single-line request file the harness reads, so refuse it up front.
	if [[ -z "$target_task_id" || "$target_task_id" =~ [[:space:]] ]]; then
		echo 'usage: real-work-session.sh select-target <session-directory> <task-id>' >&2
		exit 2
	fi
	if [[ "$status" != awaiting-target ]]; then
		echo "Session is $status, not awaiting a client target; select-target only applies while the client-target harness waits." >&2
		exit 1
	fi
	# One selection per session: the saved target is authoritative, and a
	# pending request is already the operator's live choice for this session.
	if [[ -f "$session/target.json" ]]; then
		echo "Session already bound to a client target; a saved selection cannot be replaced. Start a new client-target session to choose another." >&2
		exit 1
	fi
	if [[ -f "$session/target.request" ]]; then
		echo "A target selection is already pending; the foreground harness reports its result. Session: $session" >&2
		exit 1
	fi
	# Publish the request atomically so the watching harness never reads a
	# half-written task ID. rename within the session directory is atomic.
	request_tmp=$(mktemp "$session/.target-request.XXXXXX")
	printf '%s\n' "$target_task_id" >"$request_tmp"
	mv -- "$request_tmp" "$session/target.request"
	echo "Target selection requested for task $target_task_id; the foreground harness validates and reports it. Session: $session"
	exit 0
fi
if [[ "$action" == verify ]]; then
  [[ -x "$session/verify-real-run" && -f "$session/real-work-verify.sh" ]] || {
    echo 'This older session has no retained verifier; use a reviewed runtime restart.' >&2
    exit 1
  }
  exec bash "$session/real-work-verify.sh" publication
fi
if [[ "$action" == complete ]]; then
	case "$status" in
	walkthrough)
		: >"$session/complete.request"
		echo "Completion requested; the foreground harness reports cleanup and restoration. Session: $session"
		;;
	completed) echo "Session already completed: $session" ;;
	*) echo "Session is $status; use recover after the foreground harness has stopped." >&2; exit 1 ;;
	esac
	exit 0
fi
# Serialize repeated recovery requests. A killed helper leaves this directory
# for inspection; it never licenses a concurrent destructive retry.
if ! mkdir "$session/recovery.lock" 2>/dev/null; then
	echo "Recovery already active or interrupted; inspect $session/recovery.lock before retrying." >&2
	exit 1
fi
trap 'rmdir "$session/recovery.lock"' EXIT
# Keep the bounded supervisor owned until its process group has been reaped.
# A signal requests failure; it cannot detach cleanup or release the lock early.
recovery_signal=0
trap 'recovery_signal=130' INT
trap 'recovery_signal=143' TERM
# shellcheck source=scripts/real-work-lifecycle.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/real-work-lifecycle.sh"
state_root=$(cat "$session/state-root")
bound=$(cat "$session/rig-timeout")
[[ "$bound" =~ ^[1-9][0-9]*$ ]] || exit 2
if [[ "$status" != rig-released && "$status" != completed && ! -f "$session/rig-release-verified" ]]; then
	if ! real_work_bounded_rig "$session" "$bound" recover -state-root "$state_root" -confirm; then
		echo "Recovery refused or timed out. Gate and binary retained; see $session/rig-cleanup.log. A live holder must finish through its foreground harness." >&2
		exit 1
	fi
	printf 'rig-released\n' >"$session/status"
fi
: >"$session/rig-release-verified"
[[ "$recovery_signal" == 0 ]] || exit "$recovery_signal"
if ! real_work_restore_supervised "$session" "${3:-${FREESIDE_REAL_RUN_RESTORE_DAEMON:-$HOME/Applications/Freeside.app/Contents/Resources/freesided}}"; then
	if [[ -f "$session/runtime-upgrade-started" || -f "$session/runtime-upgrade-inherited" ]]; then printf 'recovery-required\n' >"$session/status"; fi
	echo "Rig released; supervised restoration incomplete. Retry recover for $session." >&2
	exit 1
fi
printf 'completed\n' >"$session/status"
[[ "$recovery_signal" == 0 ]] || exit "$recovery_signal"
echo "Recovery and supervised daemon health verified. Logs retained: $session"
