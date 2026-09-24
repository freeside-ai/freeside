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
# select-target records the operator's client-task choice for the foreground
# client-target harness. It only applies while awaiting a target, writes the
# request atomically, refuses bad IDs, and never allows a second selection.
sel=$tmp/select
mkdir "$sel"
cp "$root/scripts/real-work-session.sh" "$sel/"
printf 'walkthrough\n' >"$sel/status"
if bash "$sel/real-work-session.sh" select-target "$sel" task-1 >"$sel/out" 2>&1; then
	echo 'select-target accepted in walkthrough status' >&2; exit 1
fi
[[ ! -f "$sel/target.request" ]]
printf 'awaiting-target\n' >"$sel/status"
for bad in '' 'has space' $'tab\tid'; do
	if bash "$sel/real-work-session.sh" select-target "$sel" "$bad" >"$sel/out" 2>&1; then
		echo "select-target accepted a bad task id" >&2; exit 1
	fi
	[[ ! -f "$sel/target.request" ]]
done
bash "$sel/real-work-session.sh" select-target "$sel" task-42 >"$sel/out"
[[ "$(cat "$sel/target.request")" == task-42 ]]
if bash "$sel/real-work-session.sh" select-target "$sel" task-43 >"$sel/out" 2>&1; then
	echo 'second selection accepted while one was pending' >&2; exit 1
fi
[[ "$(cat "$sel/target.request")" == task-42 ]]
rm -f "$sel/target.request"
printf '{"task_id":"task-42"}\n' >"$sel/target.json"
if bash "$sel/real-work-session.sh" select-target "$sel" task-99 >"$sel/out" 2>&1; then
	echo 'selection accepted after a target was already saved' >&2; exit 1
fi
[[ ! -f "$sel/target.request" ]]

# The source proof uses only the retained configuration and exact accepted
# target, leaves selection/approval untouched, and never reuses an old receipt.
source_session=$tmp/source
mkdir "$source_session"
cp "$root/scripts/real-work-session.sh" "$source_session/"
printf 'awaiting-specification\n' >"$source_session/status"
printf 'client-target\n' >"$source_session/mode"
printf '{"task_id":"task-selected","project":"project-selected","specification_run_id":"spec-selected"}\n' >"$source_session/target.json"
cat >"$source_session/verification-env.sh" <<'CONFIG'
export FREESIDE_REAL_RUN_STATE_ROOT=/fixture/state
export FREESIDE_REAL_RUN_PROJECT=project-selected
export FREESIDE_REAL_RUN_APPROVED_RECIPE=fixture-recipe
export FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID=spec-seed
export FREESIDE_REAL_RUN_TARGET_TASK_ID=task-seed
CONFIG
cat >"$source_session/verify-real-run" <<'STUB'
#!/usr/bin/env python3
import json, os, pathlib, sys
assert sys.argv[1:] == ["-test.run", "^TestRealRunClientTarget$", "-test.count=1"]
assert os.environ["FREESIDE_REAL_RUN_CLIENT_TARGET"] == "verify-source"
assert os.environ["FREESIDE_REAL_RUN_STATE_ROOT"] == "/fixture/state"
assert os.environ["FREESIDE_REAL_RUN_APPROVED_RECIPE"] == "fixture-recipe"
assert os.environ["FREESIDE_REAL_RUN_TARGET_TASK_ID"] == "task-selected"
assert os.environ["FREESIDE_REAL_RUN_PROJECT"] == "project-selected"
assert os.environ["FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID"] == "spec-selected"
assert os.environ["FREESIDE_REAL_RUN_EXPECTED_SOURCE_ISSUE"] == "https://github.com/example/project/issues/82"
mode = os.environ.get("SOURCE_FIXTURE_MODE", "success")
if mode == "error": sys.exit(9)
if mode in ("missing-result", "older-verifier"): sys.exit(0)
decision = dict(outcome="source-verified", task_id="task-selected", project="project-selected",
                specification_run_id="spec-selected", repository="example/project",
                source_issue="https://github.com/example/project/issues/82", provenance="recommended")
if mode == "refusal": decision = dict(outcome="refused", reason="saved publication has no source issue")
if mode == "error-result": decision = dict(outcome="error")
if mode.startswith("wrong-"): decision[mode[6:]] = "wrong"
pathlib.Path(os.environ["FREESIDE_REAL_RUN_TARGET_PATH"]).write_text(
    "corrupt" if mode == "corrupt-result" else json.dumps(decision))
STUB
chmod +x "$source_session/verify-real-run"
cp "$source_session/target.json" "$source_session/target.original"
cp "$source_session/verification-env.sh" "$source_session/config.original"
run_source() {
	FREESIDE_REAL_RUN_STATE_ROOT=/ambient/incorrect FREESIDE_REAL_RUN_PROJECT=ambient-project \
	FREESIDE_REAL_RUN_APPROVED_RECIPE=ambient-recipe FREESIDE_REAL_RUN_TARGET_TASK_ID=ambient-task \
	FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID=ambient-spec \
		bash "$source_session/real-work-session.sh" verify-source "$source_session" \
		'https://github.com/example/project/issues/82' >"$source_session/output" 2>&1
}
run_source
grep -q 'recommended provenance' "$source_session/output"
receipt=$(sed -n 's/^Preapproval source receipt: //p' "$source_session/output")
[[ -s "$receipt" ]]
python3 - "$receipt" <<'PY'
import os, sys
assert os.stat(sys.argv[1]).st_mode & 0o777 == 0o600
PY
for mode in refusal error error-result missing-result older-verifier corrupt-result \
	wrong-task_id wrong-project wrong-specification_run_id wrong-repository wrong-source_issue wrong-provenance; do
	export SOURCE_FIXTURE_MODE=$mode
	if run_source; then echo "source verifier accepted $mode" >&2; exit 1; fi
	grep -q 'leave specification approval pending' "$source_session/output"
	# A valid receipt already exists; every failed attempt must still fail.
	[[ -s "$receipt" ]]
done
unset SOURCE_FIXTURE_MODE
for file in mode target.json verification-env.sh verify-real-run; do
	mv "$source_session/$file" "$source_session/$file.saved"
	if run_source; then echo "source verifier accepted missing $file" >&2; exit 1; fi
	mv "$source_session/$file.saved" "$source_session/$file"
done
printf 'ordinary\n' >"$source_session/mode"
if run_source; then echo 'source verifier accepted ordinary mode' >&2; exit 1; fi
printf 'client-target\n' >"$source_session/mode"
for invalid in '{}' 'malformed' '[]' '{"task_id":"task-selected","project":"other","specification_run_id":"spec-selected"}'; do
	printf '%s\n' "$invalid" >"$source_session/target.json"
	if run_source; then echo 'source verifier accepted invalid target' >&2; exit 1; fi
done
cp "$source_session/target.original" "$source_session/target.json"
: >"$source_session/verification-env.sh"
if run_source; then echo 'ambient environment filled missing configuration' >&2; exit 1; fi
cp "$source_session/config.original" "$source_session/verification-env.sh"
run_source
cmp "$source_session/target.json" "$source_session/target.original"
cmp "$source_session/verification-env.sh" "$source_session/config.original"
[[ "$(cat "$source_session/status")" == awaiting-specification ]]
[[ ! -f "$source_session/target.request" && ! -f "$source_session/complete.request" ]]

echo 'PASS: session completion, recovery, restoration, and read-only source proof'
