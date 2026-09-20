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
