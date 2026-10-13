#!/usr/bin/env bash
# Restore after a production rig has released its lease. Registration belongs
# to the installed app's SMAppService, not to launchctl bootstrap or a reinstall.
set -euo pipefail
bound=${FREESIDE_REAL_RUN_RESTORE_TIMEOUT_SECONDS:-120}
[[ "$bound" =~ ^[1-9][0-9]*$ ]] || { echo 'restore: timeout must be a positive integer' >&2; exit 2; }
installed=${1:-${FREESIDE_REAL_RUN_RESTORE_DAEMON:-$HOME/Applications/Freeside.app/Contents/Resources/freesided}}
app=${FREESIDE_REAL_RUN_RESTORE_APP:-}
if [[ -z "$app" ]]; then
	case "$installed" in
	/*/Contents/Resources/freesided) app="${installed%/Resources/freesided}/MacOS/FreesideMac" ;;
	*) echo 'restore: installed daemon must be an absolute app-bundle Contents/Resources/freesided path; install the current app, then retry' >&2; exit 1 ;;
	esac
fi
# Restoration targets the production label and port regardless of the shell's tier.
export FREESIDE_ENV=prod
# A process deadline also covers an older app that ignores command arguments
# and stays open. Kill only the child this helper launched, never the GUI app.
exec python3 - "$bound" "$app" <<'PY'
import math
import os
import subprocess
import sys
import time

bound, app = int(sys.argv[1]), sys.argv[2]
deadline = time.monotonic() + bound
service = f"gui/{os.getuid()}/ai.freeside.daemon"


def fail(message):
    print(f"restore: {message}", file=sys.stderr)
    sys.exit(1)


def run(args, limit=None):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        fail(f"incomplete after {bound}s")
    try:
        return subprocess.run(args, capture_output=True, text=True,
                              timeout=min(remaining, limit or remaining))
    except subprocess.TimeoutExpired:
        if args[0] == app:
            fail("app command did not return within its bound; install the current app, then retry")
        fail(f"incomplete after {bound}s: {args[0]} did not return")
    except OSError:
        fail(f"could not execute {args[0]}; install the current app, then retry")


def command(action):
    # Reserve time for one stop/start retry and the independent final check.
    timeout = max(1, min(30, math.floor((deadline - time.monotonic()) / 3)))
    result = run([app, "daemon", action, "--timeout", str(timeout)], timeout + 2)
    line = result.stdout.strip()
    fields = dict(field.split("=", 1) for field in line.split() if "=" in field)
    if (len(line.splitlines()) != 1 or fields.get("action") != action
            or fields.get("registration") not in
            {"enabled", "not_registered", "requires_approval", "not_found", "unknown"}
            or fields.get("health") not in {"reachable", "unreachable", "unknown"}):
        fail("installed app does not support the lifecycle command; install the current app, then retry")
    print(line, file=sys.stderr)
    if result.returncode == 3:
        fail("Login Items approval is required; approve Freeside, then retry")
    return result.returncode, fields


if not os.path.isabs(app) or not os.path.isfile(app) or not os.access(app, os.X_OK):
    fail("installed app command is missing; install the current app, then retry")
if run(["launchctl", "enable", service]).returncode:
    fail("could not clear launchd disable override")
code, fields = command("start")
if code and fields.get("registration") == "enabled" and fields.get("health") == "unreachable":
    code, _ = command("stop")
    if code:
        fail("could not stop the unresponsive daemon; restoration incomplete")
    code, _ = command("start")
if code:
    fail("start failed; restoration incomplete")
registered = run(["launchctl", "print", service]).returncode == 0
healthy = run(["curl", "--fail", "--silent", "--show-error", "--max-time", "2",
               "http://127.0.0.1:7331/health"]).returncode == 0
if not registered or not healthy:
    fail(f"restoration incomplete (registered={str(registered).lower()}, healthy={str(healthy).lower()})")
print("Supervised daemon registered and /health succeeded.")
PY
