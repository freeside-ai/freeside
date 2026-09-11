#!/usr/bin/env bash
# Deterministic fixtures for real_work_bounded_rig's process-group discipline.
# They source scripts/real-work-lifecycle.sh directly and exercise the four
# properties the macOS group-kill race forced: the supervisor starts its
# TERM-ignoring reservation before the rig command (so it forks nothing after
# writing the result), its output lands in the cleanup log rather than the
# caller's capture, cancellation stays bounded, and a group that cannot be
# drained fails cleanup. See devlog/2026-09-11-*-cleanup-supervisor-drain.md.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# A sleep stub, first on PATH, that records when the supervisor's reservation
# starts. For the reservation's `sleep 3600` it prints a marker (which the
# subshell redirect must send to the log, never the caller) and touches the
# file the rig stub waits on; every other duration passes through to the real
# sleep so the parent's own 0.1s polling is unaffected.
mkdir "$tmp/bin"
cat >"$tmp/bin/sleep" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == 3600 ]]; then
	printf 'reservation-marker\n'
	printf 'reservation-marker\n' >&2
	: >"$RESERVATION_MARKER"
	exec /bin/sleep 3600
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
