#!/usr/bin/env bash
# Deterministic fixtures for real_work_bounded_rig's process-group discipline.
# They source scripts/real-work-lifecycle.sh directly and exercise the
# properties the macOS group-kill race and the caller-deadman forced: the
# supervisor starts its TERM-ignoring reservation before the rig command (so it
# forks nothing after writing the result), its output lands in the cleanup log
# rather than the caller's capture, cancellation stays bounded, a group that
# cannot be drained fails cleanup, the reservation takes its group down when the
# caller dies or the time limit passes, a foreign holder in the caller's own
# group survives, and the parent sends no group signal once the group is gone or
# is the caller's own. See devlog/2026-09-11-*-cleanup-supervisor-drain.md and
# 2026-09-21-*-cleanup-supervisor-deadman.md.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)

# Track everything the run starts so teardown leaves nothing alive: individual
# helper pids (callers, holders, rig children) and the supervisor group ids the
# reservation is meant to take down. Group signals target only these recorded
# supervisor groups, never the test's own group.
tracked_pids=()
tracked_pgids=()
kill_tracked() {
	local p
	for p in ${tracked_pids[@]+"${tracked_pids[@]}"}; do
		[[ -n "$p" ]] && kill -KILL "$p" 2>/dev/null || true
	done
	for p in ${tracked_pgids[@]+"${tracked_pgids[@]}"}; do
		[[ -n "$p" ]] && kill -KILL -- "-$p" 2>/dev/null || true
	done
}
trap 'kill_tracked; rm -rf "$tmp"' EXIT INT TERM

# A sleep stub, first on PATH, that records when the supervisor's reservation
# starts. For the reservation's poll sleep (real_work_reservation_poll, default
# 5) it prints a marker (which the subshell redirect must send to the log,
# never the caller) and touches the file the rig stub waits on; every other
# duration (the parent's 0.1s polling, the deadman's grace sleeps) passes
# through to the real sleep. The marker only fires when a case set
# RESERVATION_MARKER, so callers that run without it use real sleep untouched.
mkdir "$tmp/bin"
cat >"$tmp/bin/sleep" <<'STUB'
#!/usr/bin/env bash
if [[ -n "${RESERVATION_MARKER:-}" && "${1:-}" == "${real_work_reservation_poll:-5}" ]]; then
	printf 'reservation-marker\n'
	printf 'reservation-marker\n' >&2
	: >"$RESERVATION_MARKER"
	exec /bin/sleep "$1"
fi
exec /bin/sleep "$@"
STUB
chmod +x "$tmp/bin/sleep"
export PATH="$tmp/bin:$PATH"

source "$root/scripts/real-work-lifecycle.sh"

new_case() {
	session=$tmp/$1
	mkdir -p "$session"
	export RESERVATION_MARKER=$session/reservation-started
	export RIG_VERDICT=$session/rig-verdict
}

# A rig stub that waits for the reservation marker, so a run proves the
# reservation started before the rig command finished.
reservation_probe_daemon() {
	cat >"$session/freesided" <<'STUB'
#!/usr/bin/env bash
deadline=$((SECONDS + 5))
while [[ ! -e "$RESERVATION_MARKER" ]]; do
	if ((SECONDS >= deadline)); then
		printf 'missing\n' >"$RIG_VERDICT"
		exit 0
	fi
	/bin/sleep 0.05
done
printf 'ok\n' >"$RIG_VERDICT"
exit 0
STUB
	chmod +x "$session/freesided"
}

# Reservation-first: the reservation must exist while the rig command runs.
new_case reservation-first
reservation_probe_daemon
set +e
real_work_bounded_rig "$session" 10 cleanup
rc=$?
set -e
[[ "$rc" == 0 ]]
[[ "$(cat "$session/rig-verdict")" == ok ]]

# Output routing: reservation output reaches the cleanup log, not the caller.
new_case output
reservation_probe_daemon
set +e
out=$(real_work_bounded_rig "$session" 10 cleanup 2>&1)
set -e
if grep -q reservation-marker <<<"$out"; then
	echo "FAIL: reservation output reached the caller capture" >&2
	exit 1
fi
grep -q reservation-marker "$session/rig-cleanup.log"

# Cancellation: a TERM-immune rig command is cancelled within the bound.
new_case cancel
cat >"$session/freesided" <<'STUB'
#!/usr/bin/env bash
trap '' TERM
while :; do /bin/sleep 1; done
STUB
chmod +x "$session/freesided"
set +e
err=$(real_work_bounded_rig "$session" 1 cleanup 2>&1 >/dev/null)
rc=$?
set -e
[[ "$rc" == 124 ]]
grep -q "exact-resource cleanup exceeded 1s; cancelling it" <<<"$err"

# ---------------------------------------------------------------------------
# Caller-deadman cases. The reservation watches the process that called
# real_work_bounded_rig, so these run the function in a separate `bash` process
# whose pid the test can signal. The shared caller script sets up a
# never-exiting, TERM-immune rig with a child in the supervisor group and,
# optionally, a foreign holder in the caller's own process group (the way
# run-real-work.sh holds the rig). It uses real sleep only, so the test stub
# never touches it.
# ---------------------------------------------------------------------------

# A live process (leader or member) shares this group. Zombies (Z) are already
# dead, but a stopped caller cannot reap its killed children, so they must not
# count as the group surviving.
group_present() {
	ps -A -o pgid=,stat= 2>/dev/null |
		awk -v pgid="$1" '$1 == pgid && substr($2, 1, 1) != "Z" { found = 1 } END { exit found ? 0 : 1 }'
}

# Whether a tracked pid is a live leftover. A zombie (Z) still answers kill -0
# but is a dead process awaiting reap; on a container whose PID 1 does not reap
# adopted children, a killed orphan lingers as a zombie and must not count as
# surviving (group_present excludes Z the same way).
pid_alive() {
	# read strips any leading space in the stat column, so the state test sees
	# the real first character; empty means the pid is already gone.
	local stat
	read -r stat < <(ps -o stat= -p "$1" 2>/dev/null) || true
	[[ -n "$stat" && "${stat:0:1}" != "Z" ]]
}

cat >"$tmp/caller.sh" <<'CALLER'
#!/usr/bin/env bash
PATH=/usr/bin:/bin
set +e
source "$ROOT/scripts/real-work-lifecycle.sh"
if [[ -n "${ARM_DELAY:-}" ]]; then
	# Simulate a slow arm write (process-table contention): wrap the real
	# function with a delay so the reservation reaches its arming wait before
	# the decision lands. The reservation must wait for it, not give up.
	eval "real_work_selfkill_flag_orig() $(declare -f real_work_selfkill_flag | tail -n +2)"
	real_work_selfkill_flag() { /bin/sleep "$ARM_DELAY"; real_work_selfkill_flag_orig "$@"; }
fi
session=$SESSION
mkdir -p "$session"
if [[ "${WITH_HOLDER:-}" == 1 ]]; then
	/bin/sleep 300 &
	printf '%s\n' "$!" >"$session/holder-pid"
fi
cat >"$session/freesided" <<'RIG'
#!/usr/bin/env bash
trap '' TERM
( trap '' TERM; exec /bin/sleep 300 ) &
printf '%s\n' "$!" >"$SESSION/rig-child-pid"
while :; do /bin/sleep 1; done
RIG
chmod +x "$session/freesided"
printf '%s\n' "$$" >"$session/caller-pid"
real_work_bounded_rig "$session" "$BOUND" cleanup
printf '%s\n' "$?" >"$session/caller-rc"
CALLER

# The armed deadman's recorded pgid file. real_work_bounded_rig names it
# uniquely per invocation (mktemp), so match the prefix and return the first
# non-empty one; each case runs in a fresh session, so only one is present.
selfkill_file_for() {
	local session=$1 f
	for f in "$session"/rig-selfkill-pgid.*; do
		[[ -s "$f" ]] && { printf '%s\n' "$f"; return 0; }
	done
	return 1
}

# Launch a caller and block until its deadman is armed (supervisor up, group
# recorded). Sets `launched_caller` and `launched_pgid`, and tracks both for
# teardown. POLL, MARGIN, and WITH_HOLDER tune the run.
launch_caller() {
	local session=$1 bound=$2 selfkill_file=""
	SESSION="$session" ROOT="$root" BOUND="$bound" \
		real_work_reservation_poll="${POLL:-5}" \
		real_work_reservation_margin="${MARGIN:-60}" \
		WITH_HOLDER="${WITH_HOLDER:-}" \
		bash "$tmp/caller.sh" >"$session/caller.log" 2>&1 &
	launched_caller=$!
	tracked_pids+=("$launched_caller")
	for _ in $(seq 1 100); do
		selfkill_file=$(selfkill_file_for "$session") && break
		selfkill_file=""
		/bin/sleep 0.05
	done
	if [[ -z "$selfkill_file" ]]; then
		echo "FAIL: deadman never armed for $session" >&2
		exit 1
	fi
	launched_pgid=$(tr -d ' ' <"$selfkill_file")
	tracked_pgids+=("$launched_pgid")
}

reap_caller() {
	kill -KILL "$launched_caller" 2>/dev/null || true
	wait "$launched_caller" 2>/dev/null || true
}

await_group_gone() {
	local pgid=$1
	for _ in $(seq 1 240); do # up to 12s, well past any test's limit
		if ! group_present "$pgid"; then return 0; fi
		/bin/sleep 0.05
	done
	return 1
}

# Caller killed: within 5s no supervisor, reservation, rig, or rig child
# outlives the SIGKILLed caller.
new_case caller-killed
POLL=0.2 launch_caller "$session" 30
kill -KILL "$launched_caller" 2>/dev/null || true
if ! await_group_gone "$launched_pgid"; then
	echo "FAIL: supervisor group $launched_pgid outlived the killed caller" >&2
	exit 1
fi
wait "$launched_caller" 2>/dev/null || true

# Delayed arm: the caller is SIGKILLed while the arm write is still stalled, so
# the reservation reaches its arming wait before the decision lands. It must
# wait for the decision rather than give up, so the deadman still fires once the
# slow arm completes. ARM_DELAY (3s) exceeds the old fixed 2s arming wait, so
# the pre-fix code would exit disarmed here and leave the orphan alive.
new_case arm-delay
SESSION="$session" ROOT="$root" BOUND=30 \
	real_work_reservation_poll=0.2 real_work_reservation_margin=60 ARM_DELAY=3 \
	bash "$tmp/caller.sh" >"$session/caller.log" 2>&1 &
arm_caller=$!
tracked_pids+=("$arm_caller")
# Wait until real_work_bounded_rig has started (its mktemp flag file exists),
# then a beat to clear the supervisor fork, then kill during the stalled arm.
for _ in $(seq 1 100); do
	for f in "$session"/rig-selfkill-pgid.*; do [[ -e "$f" ]] && break 2; done
	/bin/sleep 0.05
done
/bin/sleep 0.2
kill -KILL "$arm_caller" 2>/dev/null || true
# The decision lands ~3s later; read the armed pgid, then require it taken down.
arm_file=""
for _ in $(seq 1 120); do
	arm_file=$(selfkill_file_for "$session") && break
	arm_file=""
	/bin/sleep 0.05
done
if [[ -z "$arm_file" ]]; then
	echo "FAIL: arm-delay decision was never written" >&2
	exit 1
fi
arm_pgid=$(tr -d ' ' <"$arm_file")
# The reservation must still be alive when the slow arm resolves the group, so
# the decision is a real pgid, not the disarmed token the arm writes when the
# reservation has already given up and its group can no longer be resolved.
if [[ ! "$arm_pgid" =~ ^[0-9]+$ ]]; then
	echo "FAIL: deadman disarmed under a slow arm (reservation gave up early): [$arm_pgid]" >&2
	pkill -f "$session/freesided" 2>/dev/null || true
	exit 1
fi
tracked_pgids+=("$arm_pgid")
if ! await_group_gone "$arm_pgid"; then
	echo "FAIL: deadman did not take down the group after the slow arm" >&2
	exit 1
fi
wait "$arm_caller" 2>/dev/null || true

# Zombie caller counts as gone: a caller SIGKILLed under a parent that does not
# reap it stays a zombie that still answers kill -0, so the deadman must read
# the process state (Z) rather than wait out the wall-clock limit. Stage a
# durable zombie (a child exited under a parent exec'd into sleep, which never
# reaps) and check the caller-liveness helper directly.
new_case caller-zombie
zpid_file="$session/zpid"
/bin/bash -c "/bin/sleep 0.2 & echo \$! >\"$zpid_file\"; exec /bin/sleep 30" &
zombie_parent=$!
tracked_pids+=("$zombie_parent")
for _ in $(seq 1 100); do [[ -s "$zpid_file" ]] && break; /bin/sleep 0.02; done
zpid=$(cat "$zpid_file" 2>/dev/null)
/bin/sleep 0.4 # let the child exit and become a zombie under the sleep parent
if [[ -z "$zpid" ]] || ! kill -0 "$zpid" 2>/dev/null; then
	echo "FAIL: could not stage a zombie caller (pid missing or already reaped)" >&2
	exit 1
fi
if ! real_work_caller_gone "$zpid"; then
	echo "FAIL: a zombie caller was not treated as gone" >&2
	exit 1
fi
kill -KILL "$zombie_parent" 2>/dev/null || true
wait "$zombie_parent" 2>/dev/null || true
# A live caller is not gone.
/bin/sleep 5 &
live_caller=$!
tracked_pids+=("$live_caller")
if real_work_caller_gone "$live_caller"; then
	echo "FAIL: a live caller was treated as gone" >&2
	exit 1
fi
kill -KILL "$live_caller" 2>/dev/null || true
wait "$live_caller" 2>/dev/null || true

# Time limit: a caller that is stopped (never runs its own cancellation) still
# has its group taken down by the deadman once 2*bound+margin passes.
new_case time-limit
# limit = 2*1 + 5 = 7s from supervisor start: generous headroom over arming so
# the group is provably present when stopped, then vanishes on the limit alone.
POLL=0.2 MARGIN=5 launch_caller "$session" 1
kill -STOP "$launched_caller" 2>/dev/null || true
if ! group_present "$launched_pgid"; then
	echo "FAIL: group vanished before the time limit" >&2
	kill -CONT "$launched_caller" 2>/dev/null || true
	exit 1
fi
if ! await_group_gone "$launched_pgid"; then
	kill -CONT "$launched_caller" 2>/dev/null || true
	echo "FAIL: group survived past the time limit with a stopped caller" >&2
	exit 1
fi
kill -CONT "$launched_caller" 2>/dev/null || true
reap_caller

# Foreign holder survives: a holder in the caller's own group is untouched by
# normal cleanup, timeout cancellation (124), and the caller-killed case.
new_case holder-cleanup
reservation_probe_daemon
/bin/sleep 300 &
holder=$!
tracked_pids+=("$holder")
set +e
real_work_bounded_rig "$session" 10 cleanup
rc=$?
set -e
[[ "$rc" == 0 ]]
if ! kill -0 "$holder" 2>/dev/null; then
	echo "FAIL: holder killed during normal cleanup" >&2
	exit 1
fi
kill -KILL "$holder" 2>/dev/null || true
wait "$holder" 2>/dev/null || true

new_case holder-timeout
cat >"$session/freesided" <<'STUB'
#!/usr/bin/env bash
trap '' TERM
while :; do /bin/sleep 1; done
STUB
chmod +x "$session/freesided"
/bin/sleep 300 &
holder=$!
tracked_pids+=("$holder")
set +e
err=$(real_work_bounded_rig "$session" 1 cleanup 2>&1 >/dev/null)
rc=$?
set -e
[[ "$rc" == 124 ]]
if ! kill -0 "$holder" 2>/dev/null; then
	echo "FAIL: holder killed during timeout cleanup" >&2
	exit 1
fi
kill -KILL "$holder" 2>/dev/null || true
wait "$holder" 2>/dev/null || true

new_case holder-killed
WITH_HOLDER=1 POLL=0.2 launch_caller "$session" 30
holder=$(cat "$session/holder-pid")
tracked_pids+=("$holder")
kill -KILL "$launched_caller" 2>/dev/null || true
if ! await_group_gone "$launched_pgid"; then
	echo "FAIL: supervisor group outlived the killed caller (holder case)" >&2
	exit 1
fi
if ! kill -0 "$holder" 2>/dev/null; then
	echo "FAIL: foreign holder in the caller's group was killed" >&2
	exit 1
fi
wait "$launched_caller" 2>/dev/null || true
kill -KILL "$holder" 2>/dev/null || true
for _ in $(seq 1 40); do
	kill -0 "$holder" 2>/dev/null || break
	/bin/sleep 0.05
done

# No signal to a group that is gone: a forced early limit lets the deadman take
# the group down before the parent's cancellation, and the parent then sends no
# group signal. Only the deadman signals the group, and it uses KILL, never the
# parent's timeout TERM.
new_case no-signal-when-gone
cat >"$session/freesided" <<'STUB'
#!/usr/bin/env bash
trap '' TERM
while :; do /bin/sleep 1; done
STUB
chmod +x "$session/freesided"
killlog=$session/kill-calls
: >"$killlog"
(
	kill() { printf '%s\n' "$*" >>"$killlog"; builtin kill "$@"; }
	# limit = 2*4 - 7 = 1s deadman fire; parent timeout at bound=4s. The ~3s
	# cushion keeps the group provably gone before the parent's timeout signal.
	real_work_reservation_poll=0.2 real_work_reservation_margin=-7 \
		real_work_bounded_rig "$session" 4 cleanup >/dev/null 2>&1
) || true # the run returns 124 (timeout); the killlog is what this case checks
if ! grep -Eq -- '^-KILL -- -[0-9]+' "$killlog"; then
	echo "FAIL: the deadman never took the group down" >&2
	cat "$killlog" >&2
	exit 1
fi
if grep -q -- '-TERM' "$killlog"; then
	echo "FAIL: the parent signalled a group that was already gone" >&2
	cat "$killlog" >&2
	exit 1
fi

# No group kill without its own group: real_work_selfkill_flag arms a group
# (resolved from a live member) only when it is distinct from the caller's; a
# supervisor sharing the caller's group writes the explicit disarmed token and
# succeeds, while a group it cannot resolve writes the token but fails (return
# 1) so the supervisor aborts before the rig. Emptiness is never a decision.
new_case selfkill-flag
caller_pgid=$(ps -o pgid= -p "$$" | tr -d ' ')
# A member of the caller's own group is disarmed (return 0), never armed.
/bin/sleep 5 &
same=$!
tracked_pids+=("$same")
real_work_selfkill_flag "$same" "$caller_pgid" "$session/flag-same" && same_rc=0 || same_rc=$?
if [[ "$(cat "$session/flag-same" 2>/dev/null)" != "$real_work_disarmed_token" || "$same_rc" != 0 ]]; then
	echo "FAIL: a shared caller group was not a clean disarm (rc=$same_rc)" >&2
	exit 1
fi
kill -KILL "$same" 2>/dev/null || true
wait "$same" 2>/dev/null || true
# An unresolvable group (a dead probe pid) fails closed: token written, return 1.
/bin/sleep 5 &
gone=$!
kill -KILL "$gone" 2>/dev/null || true
wait "$gone" 2>/dev/null || true
real_work_selfkill_flag "$gone" "$caller_pgid" "$session/flag-gone" && gone_rc=0 || gone_rc=$?
if [[ "$(cat "$session/flag-gone" 2>/dev/null)" != "$real_work_disarmed_token" || "$gone_rc" == 0 ]]; then
	echo "FAIL: an unresolvable group did not fail closed (rc=$gone_rc)" >&2
	exit 1
fi
# A member of a distinct group records exactly that group.
set -m
/bin/sleep 5 &
distinct=$!
set +m
tracked_pids+=("$distinct")
tracked_pgids+=("$distinct")
real_work_selfkill_flag "$distinct" "$caller_pgid" "$session/flag-distinct"
if [[ ! -s "$session/flag-distinct" ]]; then
	echo "FAIL: did not record a distinct self-kill group" >&2
	exit 1
fi
if [[ "$(cat "$session/flag-distinct")" != "$(ps -o pgid= -p "$distinct" | tr -d ' ')" ]]; then
	echo "FAIL: recorded the wrong self-kill group id" >&2
	exit 1
fi
kill -KILL "$distinct" 2>/dev/null || true
wait "$distinct" 2>/dev/null || true

# Arming that cannot resolve the group fails closed: cleanup aborts before the
# rig rather than running it behind a deadman that can never fire. Force the
# unresolvable path with a temporary override, restored right after so it does
# not leak into later cases.
new_case arm-unresolvable
cat >"$session/freesided" <<'STUB'
#!/usr/bin/env bash
: >"$0.started"
trap '' TERM
while :; do /bin/sleep 1; done
STUB
chmod +x "$session/freesided"
# Override arming to report an unresolvable group inside a subshell, so the
# stub is scoped to this run and never leaks into later cases.
arm_rc=0
(
	real_work_selfkill_flag() { printf '%s\n' "$real_work_disarmed_token" >"$3"; return 1; }
	real_work_bounded_rig "$session" 4 cleanup >/dev/null 2>&1
) || arm_rc=$?
if [[ "$arm_rc" == 0 ]]; then
	echo "FAIL: cleanup did not fail closed when the deadman could not be armed" >&2
	exit 1
fi
if [[ -e "$session/freesided.started" ]]; then
	echo "FAIL: rig launched despite an unarmable deadman" >&2
	exit 1
fi

# Drain failure: a group that never empties fails cleanup. Overriding the
# member lookup keeps the failure path deterministic. Run last so the override
# does not leak into earlier cases.
new_case drain-failure
cat >"$session/freesided" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$session/freesided"
real_work_group_members() { printf '999999\n'; }
set +e
err=$(real_work_bounded_rig "$session" 10 cleanup 2>&1 >/dev/null)
rc=$?
set -e
[[ "$rc" != 0 ]]
grep -q "still has members after cancellation" <<<"$err"

# Probe failure: if the group cannot be inspected, cleanup fails closed rather
# than treating the empty reading as a drained group.
new_case probe-failure
cat >"$session/freesided" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$session/freesided"
real_work_group_members() { return 2; }
set +e
err=$(real_work_bounded_rig "$session" 10 cleanup 2>&1 >/dev/null)
rc=$?
set -e
[[ "$rc" != 0 ]]
grep -q "could not inspect process group" <<<"$err"

echo "test-real-work-lifecycle: all cases passed"

# Credential recovery uses the harness's real cleanup trap. Extract that trap
# without executing the production entry point, and stub only its rig and
# supervised-service boundaries. A real child process proves daemon shutdown.
python3 - "$root/scripts/run-real-work.sh" "$tmp/recovery-cleanup.sh" <<'PY'
import pathlib, sys
source = pathlib.Path(sys.argv[1]).read_text()
start = source.index('\ncleanup() {\n') + 1
end = source.index("trap 'exit 143' TERM", start) + len("trap 'exit 143' TERM")
pathlib.Path(sys.argv[2]).write_text(source[start:end] + '\n')
PY
python3 - "$root/scripts/run-real-work.sh" "$tmp/recovery-args.sh" <<'PY'
import pathlib, sys
source = pathlib.Path(sys.argv[1]).read_text()
start = source.index('retained_session=""')
end = source.index('spec_file=', start)
pathlib.Path(sys.argv[2]).write_text('set -euo pipefail\n' + source[start:end] +
    'printf "%s\\n" ${recovery_approved_recipe_args[@]+"${recovery_approved_recipe_args[@]}"}\n')
PY
recipe_a="sha256:$(printf '%064d' 1)"
recipe_b="sha256:$(printf '%064d' 2)"
actual=$(bash "$tmp/recovery-args.sh" --recover-codex-credentials \
  --approved-recipe "$recipe_a" --approved-recipe "$recipe_b" --approved-recipe "$recipe_a")
expected=$(printf '%s\n' -approved-recipe "$recipe_a" -approved-recipe "$recipe_b" -approved-recipe "$recipe_a")
[[ "$actual" == "$expected" ]]
[[ -z "$(bash "$tmp/recovery-args.sh" --recover-codex-credentials)" ]]
for invalid in '' sha256:short "SHA256:${recipe_a#sha256:}" " $recipe_a" "$recipe_a " "$recipe_a/extra"; do
  if bash "$tmp/recovery-args.sh" --recover-codex-credentials --approved-recipe "$invalid" >/dev/null 2>&1; then
    echo 'FAIL: recovery accepted a malformed recipe digest' >&2
    exit 1
  fi
done
if bash "$tmp/recovery-args.sh" --recover-codex-credentials --approved-recipe >/dev/null 2>&1 ||
  bash "$tmp/recovery-args.sh" --recover-codex-credentials --unknown "$recipe_a" >/dev/null 2>&1; then
  echo 'FAIL: recovery accepted missing or unknown arguments' >&2
  exit 1
fi
cat >"$tmp/recovery-case.sh" <<'CASE'
set -euo pipefail
source "$ROOT/scripts/real-work-lifecycle.sh"
workdir=$CASE_DIR
mkdir -p "$workdir"
printf 'starting\n' >"$workdir/status"
db_path="$workdir/freeside.db"
listen_address=${LISTEN_ADDRESS:-127.0.0.1:7339}
FREESIDE_REAL_RUN_STATE_ROOT="$workdir/state"
FREESIDE_REAL_RUN_APPROVED_RECIPE=sha256:fixture
rig_release_timeout=1
composition_evidence_tmp=""
rig_acquired=true
daemon_pid=""
# A held rig is an external service; the stub records exact cleanup and release.
rig_pid=""
rig_child_exists() { [[ -f "$workdir/rig-held" ]]; }
run_rig_cleanup() { rm "$workdir/rig-held"; touch "$workdir/rig-cleaned"; }
real_work_restore_supervised() { touch "$workdir/supervised-restored"; }
write_diagnostic() { return 0; }
child_job_exists() { jobs -pr | grep -qx -- "$1"; }
require_live_rig() { rig_child_exists; }
curl() { printf '%s\n' "${@: -1}" >>"$workdir/curl-urls"; [[ -f "$workdir/daemon-started" ]]; }
sleep() {
  if [[ "$(cat "$workdir/status")" == walkthrough ]]; then
    if [[ "$MODE" == interrupt ]]; then kill -TERM "$$"; else
      bash "$ROOT/scripts/real-work-session.sh" complete "$workdir" >/dev/null
    fi
  fi
  /bin/sleep 0.05
}
cat >"$workdir/freesided" <<'DAEMON'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$CASE_DIR/daemon-args"
trap 'exit 0' TERM
: >"$CASE_DIR/daemon-started"
while :; do /bin/sleep 0.05; done
DAEMON
chmod +x "$workdir/freesided"
cat >"$workdir/verify-real-run" <<'VERIFIER'
#!/usr/bin/env bash
set -euo pipefail
[[ "$FREESIDE_REAL_RUN_SCHEMA_TEST" == 1 && "$FREESIDE_REAL_RUN_STATE_ROOT" == "$CASE_DIR/state" ]]
[[ "$*" == '-test.run ^TestRealRunRetainedSchema$ -test.count=1' ]]
: >"$CASE_DIR/schema-checked"
[[ "$MODE" != schema-mismatch ]]
VERIFIER
chmod +x "$workdir/verify-real-run"
# The rig waiter completes after the cleanup boundary removes its held marker.
touch "$workdir/rig-held"
(while [[ -f "$workdir/rig-held" ]]; do /bin/sleep 0.05; done) &
rig_pid=$!
source "$CLEANUP"
real_work_recover_codex_credentials "$workdir" "$db_path" "$listen_address" \
  -approved-recipe sha256:second-approved
CASE
for mode in complete interrupt schema-mismatch; do
  case_dir="$tmp/recovery-$mode"
  set +e
  ROOT="$root" CASE_DIR="$case_dir" MODE="$mode" CLEANUP="$tmp/recovery-cleanup.sh" \
    bash "$tmp/recovery-case.sh" >"$tmp/recovery-$mode.log" 2>&1
  rc=$?
  set -e
  expected_rc=0
  [[ "$mode" != interrupt ]] || expected_rc=143
  [[ "$mode" != schema-mismatch ]] || expected_rc=1
  if [[ "$rc" != "$expected_rc" ]]; then
    cat "$tmp/recovery-$mode.log" >&2
    echo "FAIL: recovery $mode exited $rc, expected $expected_rc" >&2
    exit 1
  fi
  [[ "$(cat "$case_dir/status")" == completed ]]
  [[ -f "$case_dir/rig-cleaned" && -f "$case_dir/rig-release-verified" && -f "$case_dir/supervised-restored" ]]
  [[ -f "$case_dir/schema-checked" ]]
  if [[ "$mode" == schema-mismatch ]]; then
    [[ ! -f "$case_dir/daemon-started" && ! -f "$case_dir/daemon-args" ]]
    continue
  fi
  python3 - "$case_dir" <<'PY'
import pathlib, sys
root = pathlib.Path(sys.argv[1])
args = (root / 'daemon-args').read_text().splitlines()
assert args == ['-listen', '127.0.0.1:7339', '-db', str(root / 'freeside.db'),
                '-state-dir', str(root / 'state'), '-driver', 'disabled',
                '-approved-recipe', 'sha256:fixture',
                '-approved-recipe', 'sha256:second-approved'], args
PY
  # A loopback lease is polled unchanged.
  if ! grep -qx 'http://127.0.0.1:7339/health' "$case_dir/curl-urls" ||
    grep -vqx 'http://127.0.0.1:7339/health' "$case_dir/curl-urls"; then
    echo "FAIL: recovery $mode polled $(cat "$case_dir/curl-urls"), want the loopback address" >&2
    exit 1
  fi
done
echo 'PASS: credential recovery completion, interruption, and schema refusal clean up daemon and rig'

# A Tailscale lease is polled on the loopback twin at the same port, so the
# same-host health wait survives a host VPN dropping the Tailscale route.
tailscale_dir="$tmp/recovery-tailscale"
set +e
ROOT="$root" CASE_DIR="$tailscale_dir" MODE=complete CLEANUP="$tmp/recovery-cleanup.sh" \
  LISTEN_ADDRESS=100.64.0.1:8677 \
  bash "$tmp/recovery-case.sh" >"$tmp/recovery-tailscale.log" 2>&1
rc=$?
set -e
if [[ "$rc" != 0 ]]; then
  cat "$tmp/recovery-tailscale.log" >&2
  echo "FAIL: tailscale recovery exited $rc, expected 0" >&2
  exit 1
fi
if ! grep -qx 'http://127.0.0.1:8677/health' "$tailscale_dir/curl-urls" ||
  grep -vqx 'http://127.0.0.1:8677/health' "$tailscale_dir/curl-urls"; then
  echo "FAIL: tailscale recovery polled $(cat "$tailscale_dir/curl-urls"), want the loopback twin" >&2
  exit 1
fi
echo 'PASS: the same-host health wait dials the loopback twin for a Tailscale lease'

# Direct cases for the same-host address rule, including IPv6 forms.
for pair in \
  '100.64.0.1:8677=127.0.0.1:8677' \
  '[fd7a:115c:a1e0::1]:8677=127.0.0.1:8677' \
  '127.0.0.1:8677=127.0.0.1:8677' \
  '[::1]:8677=[::1]:8677'; do
  input=${pair%%=*}
  want=${pair#*=}
  got=$(real_work_local_address "$input")
  if [[ "$got" != "$want" ]]; then
    echo "FAIL: real_work_local_address($input) = $got, want $want" >&2
    exit 1
  fi
done
echo 'PASS: real_work_local_address maps a Tailscale address to loopback and leaves loopback unchanged'

# No leftovers: every pid and process group the run started is dead. Poll to
# absorb reaping latency (a group leader's pid frees only once bash and init
# reap it). The EXIT trap is the backstop; this proves the run itself was clean.
leftover=""
for _ in $(seq 1 100); do
	leftover=""
	for p in ${tracked_pids[@]+"${tracked_pids[@]}"}; do
		[[ -n "$p" ]] && pid_alive "$p" && leftover="$leftover pid:$p"
	done
	for p in ${tracked_pgids[@]+"${tracked_pgids[@]}"}; do
		[[ -n "$p" ]] && group_present "$p" && leftover="$leftover pgid:$p"
	done
	[[ -z "$leftover" ]] && break
	/bin/sleep 0.05
done
if [[ -n "$leftover" ]]; then
	echo "FAIL: processes from the run are still alive:$leftover" >&2
	exit 1
fi
echo 'PASS: the run left no live processes'
