#!/usr/bin/env bash
# Hermetic completion, stale recovery and supervised restoration fixtures.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
export PATH="$tmp/bin:$PATH"
export FREESIDE_REAL_RUN_RESTORE_TIMEOUT_SECONDS=1
cat >"$tmp/bin/launchctl" <<'STUB'
#!/usr/bin/env bash
printf 'launchctl %s\n' "$*" >>"$FIXTURE/events"
case "$1:$RESTORE_MODE" in
enable:failure|print:approval|print:registration-failure) exit 1 ;;
esac
STUB
cat >"$tmp/bin/curl" <<'STUB'
#!/usr/bin/env bash
printf 'health\n' >>"$FIXTURE/events"
[[ "$RESTORE_MODE" != timeout ]] || exit 1
printf '{"status":"ok","version":"%s"}\n' "${HEALTH_BUILD:-reviewed-build}"
STUB
chmod +x "$tmp/bin/launchctl" "$tmp/bin/curl"

new_session() {
	export FIXTURE=$tmp/$1 RESTORE_MODE=ok
	mkdir "$FIXTURE"
	cp "$root/scripts/real-work-session.sh" "$root/scripts/real-work-lifecycle.sh" \
		"$root/app/scripts/restore-supervised-daemon.sh" "$FIXTURE/"
	printf 'recovery-required\n' >"$FIXTURE/status"
	printf '%s\n' "$FIXTURE/state" >"$FIXTURE/state-root"
	printf '1\n' >"$FIXTURE/rig-timeout"
	printf 'earlier cleanup diagnostic\n' >"$FIXTURE/rig-cleanup.log"
	printf 'earlier restore diagnostic\n' >"$FIXTURE/restore.log"
	touch "$FIXTURE/manifest"
	cat >"$FIXTURE/freesided" <<'STUB'
#!/usr/bin/env bash
printf 'rig %s\n' "$*" >>"$FIXTURE/events"
echo 'recovery diagnostic'
[[ "$*" == "rig recover -state-root $FIXTURE/state -confirm" ]] || exit 2
for refusal in live-database live-listener live-holder resource-remains; do
	[[ ! -f "$FIXTURE/$refusal" ]] || exit 1
done
if [[ -f "$FIXTURE/hang" ]]; then
	printf '%s\n' "$BASHPID" >"$FIXTURE/cleanup.pid"
	trap '' TERM
	while :; do sleep 1; done
fi
rm -f "$FIXTURE/manifest"
STUB
	chmod +x "$FIXTURE/freesided"
}

run_recovery() {
	set +e
	bash "$FIXTURE/real-work-session.sh" recover "$FIXTURE" >"$FIXTURE/output" 2>&1
	rc=$?
	set -e
}

new_session complete
printf 'walkthrough\n' >"$FIXTURE/status"
bash "$FIXTURE/real-work-session.sh" complete "$FIXTURE" >"$FIXTURE/output"
bash "$FIXTURE/real-work-session.sh" complete "$FIXTURE" >>"$FIXTURE/output"
[[ -f "$FIXTURE/complete.request" && -f "$FIXTURE/manifest" && ! -f "$FIXTURE/events" ]]

new_session stale
run_recovery
[[ "$rc" == 0 && ! -f "$FIXTURE/manifest" && -x "$FIXTURE/freesided" ]]
[[ "$(cat "$FIXTURE/status")" == completed ]]
grep -q '^earlier cleanup diagnostic$' "$FIXTURE/rig-cleanup.log"
grep -q '^earlier restore diagnostic$' "$FIXTURE/restore.log"
events=$(tr '\n' ' ' <"$FIXTURE/events")
[[ "$events" == "rig rig recover -state-root $FIXTURE/state -confirm launchctl enable gui/$(id -u)/ai.freeside.daemon launchctl print gui/$(id -u)/ai.freeside.daemon health " ]]
run_recovery
[[ "$rc" == 0 && "$(grep -c '^rig ' "$FIXTURE/events")" == 1 ]]
bash "$FIXTURE/real-work-session.sh" complete "$FIXTURE" >>"$FIXTURE/output"

new_session reviewed-recovery-binary
cp "$FIXTURE/freesided" "$FIXTURE/reviewed-freesided"
printf '#!/usr/bin/env bash\nexit 91\n' >"$FIXTURE/freesided"
printf 'exit 92\n' >"$FIXTURE/real-work-lifecycle.sh"
FREESIDE_REAL_RUN_RECOVERY_DAEMON="$FIXTURE/reviewed-freesided" \
  bash "$root/scripts/real-work-session.sh" recover "$FIXTURE" >"$FIXTURE/output" 2>&1
[[ "$(cat "$FIXTURE/status")" == completed && ! -f "$FIXTURE/manifest" ]]
grep -q 'exit 91' "$FIXTURE/freesided"
grep -q 'exit 92' "$FIXTURE/real-work-lifecycle.sh"

new_session invalid-recovery-binary
if FREESIDE_REAL_RUN_RECOVERY_DAEMON=relative-binary \
  bash "$root/scripts/real-work-session.sh" recover "$FIXTURE" >"$FIXTURE/output" 2>&1; then
  echo 'relative recovery executable accepted' >&2
  exit 1
fi
[[ -f "$FIXTURE/manifest" && ! -f "$FIXTURE/events" ]]

for refusal in live-database live-listener live-holder resource-remains hang; do
	new_session "$refusal"
	touch "$FIXTURE/$refusal"
	run_recovery
	[[ "$rc" != 0 && -f "$FIXTURE/manifest" && -x "$FIXTURE/freesided" ]]
	if grep -q '^launchctl' "$FIXTURE/events"; then
		echo 'restoration ran after refused recovery' >&2
		exit 1
	fi
	[[ ! -d "$FIXTURE/recovery.lock" ]]
	grep -q '^earlier cleanup diagnostic$' "$FIXTURE/rig-cleanup.log"
done

for signal in TERM INT; do
	new_session "interrupt-$signal"
	touch "$FIXTURE/runtime-upgrade-started"
	touch "$FIXTURE/hang"
	python3 - "$signal" <<'PY'
import os
import pathlib
import signal
import subprocess
import sys
import time

session = pathlib.Path(os.environ["FIXTURE"])
with (session / "output").open("w") as output:
    helper = subprocess.Popen(["bash", str(session / "real-work-session.sh"), "recover", str(session)],
                              stdout=output, stderr=subprocess.STDOUT)
    try:
        deadline = time.monotonic() + 5
        while not (session / "cleanup.pid").exists():
            assert helper.poll() is None, "recovery exited before cleanup"
            assert time.monotonic() < deadline, "cleanup did not start"
            time.sleep(0.01)
        helper.send_signal(getattr(signal, "SIG" + sys.argv[1]))
        assert helper.wait(timeout=8) != 0, "interrupted recovery claimed success"
        assert not (session / "recovery.lock").exists(), "finished helper retained its lock"
        assert (session / "manifest").exists(), "interruption erased the gate"
        child = (session / "cleanup.pid").read_text().strip()
        state = subprocess.run(["ps", "-o", "stat=", "-p", child], capture_output=True, text=True).stdout.strip()
        assert not state or state.startswith("Z"), "cleanup survived its owner's interruption"
    finally:
        if helper.poll() is None:
            helper.kill()
            helper.wait()
PY
done

for mode in failure approval registration-failure timeout; do
	new_session "restore-$mode"
	export RESTORE_MODE=$mode
	run_recovery
	[[ "$rc" != 0 && "$(cat "$FIXTURE/status")" == rig-released ]]
	[[ ! -f "$FIXTURE/manifest" && -x "$FIXTURE/freesided" ]]
	grep -q 'restoration incomplete' "$FIXTURE/output"
	export RESTORE_MODE=ok
	run_recovery
	[[ "$rc" == 0 && "$(cat "$FIXTURE/status")" == completed ]]
	[[ "$(grep -c '^rig ' "$FIXTURE/events")" == 1 ]]
	grep -q '^earlier restore diagnostic$' "$FIXTURE/restore.log"
done

cat >"$tmp/bin/go" <<'STUB'
#!/usr/bin/env bash
[[ "$1 $2" == 'tool buildid' ]] || exit 2
if [[ "$3" == */installed && -f "$FIXTURE/old-installed" ]]; then
  echo old-build
else
  echo reviewed-build
fi
STUB
chmod +x "$tmp/bin/go"
new_session upgraded-recovery
cp "$FIXTURE/freesided" "$FIXTURE/installed"
cp "$root/scripts/real-work-retained.py" "$FIXTURE/"
cat >"$FIXTURE/verify-real-run" <<'STUB'
#!/usr/bin/env bash
[[ "$FREESIDE_REAL_RUN_SCHEMA_TEST" == 1 && "$FREESIDE_REAL_RUN_STATE_ROOT" == "$FIXTURE/state" ]]
[[ ! -f "$FIXTURE/schema-refusal" ]]
STUB
chmod +x "$FIXTURE/verify-real-run"
printf '%s\n' reviewed-build >"$FIXTURE/build-version"
touch "$FIXTURE/runtime-upgrade-started" "$FIXTURE/old-installed"
export FREESIDE_REAL_RUN_RESTORE_DAEMON="$FIXTURE/installed"
run_recovery
[[ "$rc" != 0 && "$(cat "$FIXTURE/status")" == recovery-required && -f "$FIXTURE/rig-release-verified" ]]
if grep -q '^launchctl ' "$FIXTURE/events"; then echo 'old binary was restored' >&2; exit 1; fi
rm "$FIXTURE/old-installed"
touch "$FIXTURE/schema-refusal"
run_recovery
[[ "$rc" != 0 && "$(cat "$FIXTURE/status")" == recovery-required ]]
if grep -q '^launchctl ' "$FIXTURE/events"; then echo 'incompatible schema was restored' >&2; exit 1; fi
rm "$FIXTURE/schema-refusal"
export HEALTH_BUILD=old-build
run_recovery
[[ "$rc" != 0 && "$(cat "$FIXTURE/status")" == recovery-required ]]
export HEALTH_BUILD=reviewed-build
run_recovery
[[ "$rc" == 0 && "$(cat "$FIXTURE/status")" == completed ]]
[[ "$(grep -c '^rig ' "$FIXTURE/events")" == 1 ]]
echo 'PASS: session completion, stale recovery, retained diagnostics and restoration'
