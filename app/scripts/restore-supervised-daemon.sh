#!/usr/bin/env bash
# Restore after a production rig has released its lease. Registration belongs
# to the installed app's SMAppService, not to launchctl bootstrap or a reinstall.
set -euo pipefail
bound=${FREESIDE_REAL_RUN_RESTORE_TIMEOUT_SECONDS:-120}
[[ "$bound" =~ ^[1-9][0-9]*$ ]] || { echo 'restore: timeout must be a positive integer' >&2; exit 2; }
service="gui/$(id -u)/ai.freeside.daemon"
launchctl enable "$service" || { echo 'restore: could not clear launchd disable override' >&2; exit 1; }
cat >&2 <<'INSTRUCTIONS'
Restore the supervised daemon from the installed Freeside menu:
  Stopped / LaunchAgent unavailable: choose Start.
  Daemon unreachable: choose Stop, wait for Stopped, then choose Start.
  If approval is required, choose Open Login Items… and approve Freeside.
Registration errors or outstanding approval mean restoration is incomplete.
Client endpoint preferences stay as configured for the run.
INSTRUCTIONS
deadline=$((SECONDS + bound))
registered=false
while ((SECONDS < deadline)); do
	if launchctl print "$service" >/dev/null 2>&1; then
		registered=true
		if curl --fail --silent --show-error --max-time 2 http://127.0.0.1:7331/health; then
			echo 'Supervised daemon registered and /health succeeded.'
			exit 0
		fi
	else
		registered=false
	fi
	sleep 1
done
echo "restore: incomplete after ${bound}s (registered=$registered); inspect the app error or Login Items approval, then retry." >&2
exit 1
