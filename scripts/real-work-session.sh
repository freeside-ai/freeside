#!/usr/bin/env bash
# Usage: real-work-session.sh complete|recover <retained-session-directory>
# Recovery never signals a stored PID. The rig command proves stale ownership,
# database/listener exclusion and exact-resource cleanup before clearing a gate.
set -euo pipefail
umask 077
action=${1:-}
session=${2:-}
if [[ "$action" != complete && "$action" != recover ]] || [[ ! -f "$session/status" ]]; then
	echo 'usage: real-work-session.sh complete|recover <session-directory>' >&2
	exit 2
fi
session=$(cd "$session" && pwd)
status=$(cat "$session/status")
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
source "$session/real-work-lifecycle.sh"
state_root=$(cat "$session/state-root")
bound=$(cat "$session/rig-timeout")
[[ "$bound" =~ ^[1-9][0-9]*$ ]] || exit 2
if [[ "$status" != rig-released && "$status" != completed ]]; then
	if ! real_work_bounded_rig "$session" "$bound" recover -state-root "$state_root" -confirm; then
		echo "Recovery refused or timed out. Gate and binary retained; see $session/rig-cleanup.log. A live holder must finish through its foreground harness." >&2
		exit 1
	fi
	printf 'rig-released\n' >"$session/status"
fi
[[ "$recovery_signal" == 0 ]] || exit "$recovery_signal"
if ! bash "$session/restore-supervised-daemon.sh" 2>&1 | tee -a "$session/restore.log"; then
	echo "Rig released; supervised restoration incomplete. Retry recover for $session." >&2
	exit 1
fi
printf 'completed\n' >"$session/status"
[[ "$recovery_signal" == 0 ]] || exit "$recovery_signal"
echo "Recovery and supervised daemon health verified. Logs retained: $session"
