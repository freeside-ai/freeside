#!/usr/bin/env bash
# test-check-agent-image.sh — regression suite for check-agent-image.sh.
#
# Drives the checker against a scripted `container` stand-in (the checker
# takes the container executable as its second argument), so every runtime
# response — create, inspect, list, delete — is a per-case fixture and no
# real container runtime is needed. Covers the duplicate-key JSON defense
# (#353: jq is last-value-wins, so a duplicated key must reject before any
# field comparison or force-delete, mirroring ward.RejectDuplicateJSONKeys)
# and the cidfile-less ownership recovery (#355: an interrupted create that
# never produced the cidfile is recovered by listing containers and deleting
# only an inspected ownership-token match).
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECKER=$SCRIPT_DIR/check-agent-image.sh
REAL_RUN=$SCRIPT_DIR/run-real-work.sh

command -v jq >/dev/null 2>&1 || {
  echo "test-check-agent-image: jq is required" >&2
  exit 1
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Scripted Apple `container` stand-in. STUB_DIR selects the case fixtures:
#   create_mode        ok (registers probe-1, writes the cidfile)
#                      fail-no-cidfile (registers nothing on disk, exits 1)
#                      fail-with-cidfile (writes the cidfile, then exits 1)
#   inspect-<id>.json  inspect response; @TOKEN@ is replaced by the --label
#                      token captured from create; inspect-<id>-<n>.json
#                      (n = 1-based call count per id) overrides call n
#   list.json          list response (a missing file fails the list call)
#   drain_stdin        when present, inspect consumes its stdin first (a
#                      runtime that reads stdin inside the recovery loop)
#   hang-inspect-<id>-<n> when present, that inspect call never returns
#   ignore-term-inspect-<id>-<n> does the same while ignoring TERM and without
#                      invoking sleep, so watchdog sleeps can be recorded alone
#   fork-hang-inspect-<id>-<n> when present, that inspect starts a helper
#                      which retains the capture pipe after its parent exits
#   detach-hang-inspect-<id>-<n> does the same after moving the helper to a
#                      distinct process group
#   orphan-inspect-<id>-<n> starts a same-group helper with checker-facing
#                      descriptors closed, then returns the normal fixture
#   hang-delete        when present, delete never returns after logging
#   delete_fail        when present, delete exits 1 after logging
#   run_output         output for a `container run` preflight
#   run_status         exit status for a `container run` preflight
#   inspects.log       appended with the id of every inspect call
#   deletes.log        appended with the arguments of every delete call
STUB=$TMP/container
cat >"$STUB" <<'STUB_EOF'
#!/usr/bin/env bash
set -euo pipefail
dir=${STUB_DIR:?}
cmd=${1:-}
shift || true
case "$cmd" in
create)
  cidfile="" token=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --cidfile)
      cidfile=$2
      shift 2
      ;;
    --label)
      token=${2#*=}
      shift 2
      ;;
    *) shift ;;
    esac
  done
  printf '%s' "$token" >"$dir/token"
  case "$(cat "$dir/create_mode")" in
  ok) printf 'probe-1' >"$cidfile" ;;
  fail-no-cidfile) exit 1 ;;
  fail-with-cidfile)
    printf 'probe-1' >"$cidfile"
    exit 1
    ;;
  esac
  ;;
inspect)
  id=$1
  printf '%s\n' "$id" >>"$dir/inspects.log"
  [ ! -f "$dir/drain_stdin" ] || cat >/dev/null
  count_file=$dir/inspect-count-$id
  n=$(($(cat "$count_file" 2>/dev/null || echo 0) + 1))
  printf '%s' "$n" >"$count_file"
  [ ! -f "$dir/hang-inspect-$id-$n" ] || exec sleep 30
  if [ -f "$dir/ignore-term-inspect-$id-$n" ]; then
    trap '' TERM
    while :; do :; done
  fi
  if [ -f "$dir/fork-hang-inspect-$id-$n" ]; then
    sleep 30 &
    printf '%s' "$!" >"$dir/helper-pid"
    exit 0
  fi
  if [ -f "$dir/detach-hang-inspect-$id-$n" ]; then
    set -m
    sleep 30 &
    printf '%s' "$!" >"$dir/helper-pid"
    set +m
    exit 0
  fi
  if [ -f "$dir/orphan-inspect-$id-$n" ]; then
    sleep 30 </dev/null >/dev/null 2>&1 &
    printf '%s' "$!" >"$dir/helper-pid"
  fi
  template=$dir/inspect-$id-$n.json
  [ -f "$template" ] || template=$dir/inspect-$id.json
  [ -f "$template" ] || exit 1
  token=$(cat "$dir/token" 2>/dev/null || true)
  sed -e "s/@TOKEN@/${token}/g" "$template"
  ;;
list)
  [ -f "$dir/list.json" ] || exit 1
  cat "$dir/list.json"
  ;;
delete)
  printf '%s\n' "$*" >>"$dir/deletes.log"
  [ ! -f "$dir/hang-delete" ] || exec sleep 30
  [ ! -f "$dir/delete_fail" ]
  ;;
run)
  [ ! -f "$dir/run_output" ] || cat "$dir/run_output"
  [ ! -f "$dir/run_status" ] || exit "$(cat "$dir/run_status")"
  ;;
*)
  exit 1
  ;;
esac
STUB_EOF
chmod +x "$STUB"

# A report satisfying every image-side precondition the checker compares.
compliant_report() { # <container-id>
  printf '%s' '[{"id":"@ID@","configuration":{"id":"@ID@","image":{"reference":"example.test/agent:1"},"labels":{"ai.freeside.project-image.owner":"@TOKEN@"},"initProcess":{"executable":"sh","arguments":["-c","true"],"environment":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],"workingDirectory":"/"},"ssh":false,"publishedPorts":[],"publishedSockets":[],"networks":[],"mounts":[]}}]' |
    sed "s/@ID@/$1/g"
}

pass=0
fail=0
CASE=''
CASE_DIR=''
OUT=''
RC=0
case_n=0

begin_case() {
  CASE=$1
  case_n=$((case_n + 1))
  CASE_DIR=$TMP/case-$case_n
  mkdir -p "$CASE_DIR"
  export STUB_DIR=$CASE_DIR
  unset FREESIDE_CHECK_AGENT_IMAGE_RUNTIME_BOUND_SECONDS
	unset FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS
  printf 'ok' >"$CASE_DIR/create_mode"
  echo "case: $CASE"
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

run_checker() {
  set +e
  OUT=$("$CHECKER" example.test/agent:1 "$STUB" 2>&1)
  RC=$?
  set -e
}

run_checker_with_sleep_log() {
  sleep_stub_dir=$CASE_DIR/sleep-bin
  mkdir -p "$sleep_stub_dir"
  cat >"$sleep_stub_dir/sleep" <<'SLEEP_STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${SLEEP_LOG:?}"
exec "${REAL_SLEEP:?}" "$@"
SLEEP_STUB
  chmod +x "$sleep_stub_dir/sleep"
  real_sleep=$(command -v sleep)
  set +e
  OUT=$(env \
    PATH="$sleep_stub_dir:$PATH" \
    REAL_SLEEP="$real_sleep" \
    SLEEP_LOG="$CASE_DIR/sleeps.log" \
    "$CHECKER" example.test/agent:1 "$STUB" 2>&1)
  RC=$?
  set -e
}

run_checker_then_term() {
  checker_output=$CASE_DIR/checker-output
  set +e
  "$CHECKER" example.test/agent:1 "$STUB" >"$checker_output" 2>&1 &
  checker_pid=$!
  for _ in $(seq 1 100); do
    [ ! -s "$CASE_DIR/helper-pid" ] || break
    kill -0 "$checker_pid" 2>/dev/null || break
    sleep 0.05
  done
  kill -TERM "$checker_pid" 2>/dev/null
  wait "$checker_pid"
  RC=$?
  set -e
  OUT=$(cat "$checker_output")
}

run_real_work() {
	go_stub_mode=${1:-test-fail}
	submit_shape=${2:-current}
	rig_hold_mode=${3:-ok}
	rig_cleanup_mode=${4:-ok}
	input_dir=$CASE_DIR/inputs
  stub_bin=$CASE_DIR/bin
  mkdir -p "$input_dir" "$stub_bin"
  : >"$input_dir/spec.md"
  : >"$input_dir/policy.json"
  : >"$input_dir/publication.json"
	cat >"$stub_bin/go" <<'GO_STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'called\n' >"${STUB_DIR:?}/go.log"
case " ${*} " in
*" build "*)
	if [ "${GO_STUB_MODE:-}" = build-fail ]; then
		exit 97
	fi
	output=""
	while [ "$#" -gt 0 ]; do
		if [ "$1" = -o ]; then
			output=$2
			break
		fi
		shift
	done
	[ -n "$output" ]
cat >"$output" <<'FREESIDED_STUB'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = rig ]; then
	case "${2:-}" in
	hold)
		if [ "${GO_STUB_RIG_HOLD_MODE:-ok}" = refuse ]; then
			echo 'production rig lease is held by other@host' >&2
			exit 1
		fi
		printf '%s\n' 'rig-hold' >>"${STUB_DIR:?}/lifecycle.log"
		printf '%s\n' '{"token":"test-token","manifest":{"version":1,"owner":{"user":"test","host":"host","pid":1},"acquired_at":"2026-08-15T12:00:00Z","resources":{"state_root":"/state","database_path":"/state/freeside.db","listen_address":"127.0.0.1:0","seed_root":"/seed","containers":[]},"token_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}'
		printf '%s\n' "$*" >"${STUB_DIR:?}/rig-hold.args"
		if [ "${GO_STUB_RIG_HOLD_MODE:-ok}" = release-fail ]; then
			trap 'exit 17' USR1
		elif [ "${GO_STUB_RIG_HOLD_MODE:-ok}" = release-hang ]; then
			trap '' USR1
		elif [ "${GO_STUB_RIG_HOLD_MODE:-ok}" = release-stop ]; then
			trap '' USR1
			kill -STOP "$$"
		else
			trap 'printf "%s\n" rig-release >>"${STUB_DIR:?}/lifecycle.log"; exit 0' USR1
		fi
		trap 'exit 0' TERM INT
		while :; do sleep 1; done
		;;
	bind)
		printf '%s\n' 'rig-bind' >>"${STUB_DIR:?}/lifecycle.log"
		printf '%s\n' "$*" >"${STUB_DIR:?}/rig-bind.args"
		printf '%s\n' '{}'
		;;
	check)
		;;
	resource)
		case "$*" in
		*"-name state-root"*) printf '%s\n' "${FREESIDE_REAL_RUN_STATE_ROOT:?}" ;;
		*"-name database-path"*) printf '%s/freeside.db\n' "${FREESIDE_REAL_RUN_STATE_ROOT:?}" ;;
		*"-name listen-address"*) printf '%s\n' "${FREESIDE_REAL_RUN_LISTEN:?}" ;;
		*"-name seed-root"*) printf '%s\n' "${FREESIDE_REAL_RUN_SEED_ROOT:?}" ;;
		*) exit 99 ;;
		esac
		;;
	cleanup)
		printf '%s\n' 'rig-cleanup' >>"${STUB_DIR:?}/lifecycle.log"
		printf '%s\n' "$*" >"${STUB_DIR:?}/rig-cleanup.args"
		if [ "${GO_STUB_RIG_CLEANUP_MODE:-ok}" = hang ]; then
			trap '' TERM INT
			while :; do sleep 1; done
		elif [ "${GO_STUB_RIG_CLEANUP_MODE:-ok}" = procbound-hang ]; then
			set -m
			(
				trap '' TERM INT
				while :; do sleep 1; done
			) &
			helper_pid=$!
			set +m
			printf '%s\n' "$helper_pid" >"${STUB_DIR:?}/helper-pid"
			trap 'kill -KILL -- "-$helper_pid" 2>/dev/null || true; exit 143' TERM
			while :; do sleep 1; done
		elif [ "${GO_STUB_RIG_CLEANUP_MODE:-ok}" = orphan ]; then
			(
				trap '' TERM INT
				printf '%s\n' "$BASHPID" >"${STUB_DIR:?}/helper-pid"
				while :; do sleep 1; done
			) &
			exit 19
		fi
		;;
	*) exit 99 ;;
	esac
	exit 0
fi
if [ "${1:-}" = submit ]; then
	printf '%s\n' 'submit' >>"${STUB_DIR:?}/lifecycle.log"
	: >"${STUB_DIR:?}/submit.called"
	if [ "${GO_STUB_SUBMIT_SHAPE:-current}" = legacy ]; then
		printf '%s\n' '{"run_id":"impl-run","project_id":"freeside","invocation_id":"impl-inv","stage_id":"impl-stage","implementation_run_id":"impl-run","implementation_invocation_id":"impl-inv","implementation_stage_id":"impl-stage"}'
	else
		printf '%s\n' '{"run_id":"impl-run","elaboration_run_id":"elab-run","project_id":"freeside","invocation_id":"impl-inv","stage_id":"impl-stage","implementation_run_id":"impl-run","implementation_invocation_id":"impl-inv","implementation_stage_id":"impl-stage","elaboration_invocation_id":"elab-inv","elaboration_stage_id":"elab-stage"}'
	fi
	exit 0
fi
printf '%s\n' 'daemon-start' >>"${STUB_DIR:?}/lifecycle.log"
printf '%s\n' "$@" >"${STUB_DIR:?}/daemon.args.tmp"
mv "${STUB_DIR:?}/daemon.args.tmp" "${STUB_DIR:?}/daemon.args"
: >"${STUB_DIR:?}/daemon.args.ready"
trap 'printf "%s\n" daemon-stop >>"${STUB_DIR:?}/lifecycle.log"; exit 0' TERM INT
while :; do sleep 1; done
FREESIDED_STUB
	chmod +x "$output"
	;;
*" test "*)
	if [ "${GO_STUB_MODE:-}" != lifecycle ]; then
		exit 97
	fi
	if [ -z "${FREESIDE_REAL_RUN_INVOCATION:-}" ]; then
		exit 0
	fi
	attempts=0
	while [ ! -f "${STUB_DIR:?}/daemon.args.ready" ] && [ "$attempts" -lt 100 ]; do
		sleep 0.01
		attempts=$((attempts + 1))
	done
	[ -f "${STUB_DIR:?}/daemon.args.ready" ]
	printf '%s\t%s\n' "$FREESIDE_REAL_RUN_RUN_ID" \
		"$FREESIDE_REAL_RUN_INVOCATION" >>"${STUB_DIR:?}/verification-identities.log"
	[ "$FREESIDE_REAL_RUN_RUN_ID" = impl-run ]
	[ "$FREESIDE_REAL_RUN_INVOCATION" = impl-inv ]
	printf '%s\n' 'real production pipeline verified: PR #7'
	;;
*)
	exit 98
	;;
esac
GO_STUB
  chmod +x "$stub_bin/go"
  digest="sha256:$(printf 'a%.0s' {1..64})"

  set +e
	OUT=$(env \
		PATH="$stub_bin:$TMP:$PATH" \
		GO_STUB_MODE="$go_stub_mode" \
		GO_STUB_SUBMIT_SHAPE="$submit_shape" \
		GO_STUB_RIG_HOLD_MODE="$rig_hold_mode" \
		GO_STUB_RIG_CLEANUP_MODE="$rig_cleanup_mode" \
		FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS="${FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS:-30}" \
    FREESIDE_REAL_RUN_STATE_ROOT="$CASE_DIR/state" \
    FREESIDE_REAL_RUN_LISTEN=127.0.0.1:8677 \
    FREESIDE_REAL_RUN_AGENT_IMAGE="example.test/agent@$digest" \
    FREESIDE_WARD_EXPORTER_IMAGE="example.test/exporter@$digest" \
    FREESIDE_REAL_RUN_REVIEW_IMAGE="example.test/reviewer@$digest" \
    FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT="$CASE_DIR/review-input" \
    FREESIDE_REAL_RUN_REVIEW_AUTH_MODE=subscription \
    FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY=reviewer \
    FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT=auth.json \
    FREESIDE_REAL_RUN_REVIEW_INSTRUCTIONS=AGENTS.md \
    FREESIDE_REAL_RUN_REVIEW_MODEL=review-model \
    FREESIDE_REAL_RUN_REVIEW_REASONING=high \
    FREESIDE_REAL_RUN_REVIEW_COST_OWNER=operator \
    FREESIDE_REAL_RUN_SEED_ROOT="$CASE_DIR/seed" \
    FREESIDE_REAL_RUN_AUTH_IDENTITY=implementer \
    FREESIDE_REAL_RUN_AUTH_VOLUME=auth-volume \
    FREESIDE_REAL_RUN_REPO=freeside-ai/freeside \
    FREESIDE_REAL_RUN_REPOSITORY_ID=1 \
    FREESIDE_REAL_RUN_BASE_REF=main \
    FREESIDE_REAL_RUN_BASE_SHA=0123456789012345678901234567890123456789 \
    FREESIDE_REAL_RUN_PROMPT_PACKAGE="$input_dir/prompts.json" \
    FREESIDE_REAL_RUN_ELABORATION_PROMPT_PACKAGE="$input_dir/elaborator.md" \
    FREESIDE_REAL_RUN_INSTRUCTIONS="$input_dir/CLAUDE.md" \
    FREESIDE_REAL_RUN_APPROVED_RECIPE="$digest" \
    FREESIDE_REAL_RUN_APP_STATE="$CASE_DIR/app-state" \
    FREESIDE_REAL_RUN_APP_CREDS="$CASE_DIR/app-creds" \
    FREESIDE_REAL_RUN_PROJECT=freeside \
    FREESIDE_REAL_RUN_ALLOWED_PATHS=scripts/ \
    "$REAL_RUN" "$input_dir/spec.md" "$input_dir/policy.json" \
    "$input_dir/publication.json" 2>&1)
  RC=$?
  set -e
}

assert_rc() {
  if [ "$RC" -eq "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "expected rc=$1, got rc=$RC"
  fi
}

assert_contains() {
  case "$OUT" in
  *"$1"*) pass=$((pass + 1)) ;;
  *) report_failure "output does not contain: $1" ;;
  esac
}

assert_lacks() {
  case "$OUT" in
  *"$1"*) report_failure "output unexpectedly contains: $1" ;;
  *) pass=$((pass + 1)) ;;
  esac
}

assert_deletes() { # exact expected deletes.log content; '' means no deletes
  actual=$(cat "$CASE_DIR/deletes.log" 2>/dev/null || true)
  if [ "$actual" = "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "deletes.log: expected '$1', got '$actual'"
  fi
}

assert_never_inspected() { # <id> must not appear in inspects.log
  if grep -qxF "$1" "$CASE_DIR/inspects.log" 2>/dev/null; then
    report_failure "candidate '$1' was inspected"
  else
    pass=$((pass + 1))
  fi
}

assert_helper_stopped() {
  helper_pid=$(cat "$CASE_DIR/helper-pid" 2>/dev/null || true)
  if [ -z "$helper_pid" ]; then
    report_failure "forked runtime helper pid is missing"
    return
  fi
  if ! kill -0 "$helper_pid" 2>/dev/null; then
    pass=$((pass + 1))
    return
  fi
  helper_state=""
  if [ -r "/proc/$helper_pid/status" ]; then
    if helper_state=$(awk '$1 == "State:" { print substr($2, 1, 1); exit }' \
      "/proc/$helper_pid/status"); then
      :
    else
      helper_state=""
    fi
  else
    if helper_state=$(ps -o state= -p "$helper_pid" 2>/dev/null |
      awk 'NR == 1 { print substr($1, 1, 1) }'); then
      :
    else
      helper_state=""
    fi
  fi
  if [ "$helper_state" = Z ]; then
    pass=$((pass + 1))
  elif [ -z "$helper_state" ] && ! kill -0 "$helper_pid" 2>/dev/null; then
    pass=$((pass + 1))
  else
    report_failure "forked runtime helper is still alive: $helper_pid (state ${helper_state:-unknown})"
  fi
}

assert_not_exists() {
  if [ -e "$1" ]; then
    report_failure "unexpected path exists: $1"
  else
    pass=$((pass + 1))
  fi
}

assert_exists() {
  if [ -e "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "expected path does not exist: $1"
  fi
}

# ------------------------------------------------- existing behavior holds
begin_case "1 compliant image passes and the probe is removed"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 0
assert_contains "satisfies the ward allowlist"
assert_deletes "--force probe-1"

begin_case "2 image-contributed environment still fails the allowlist"
compliant_report probe-1 |
  jq '.[0].configuration.initProcess.environment += ["EXTRA=1"]' \
    >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "environment beyond the fixed PATH"
assert_deletes "--force probe-1"

begin_case "3 cidfile written by a failing create still recovers the probe"
printf 'fail-with-cidfile' >"$CASE_DIR/create_mode"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_lacks "ownership recovery"
assert_deletes "--force probe-1"

begin_case "4 failing delete is reported"
printf '' >"$CASE_DIR/delete_fail"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "could not remove probe container probe-1"

# --------------------------------------------- cleanup ownership gating
begin_case "5 probe carrying a foreign ownership token is refused"
compliant_report probe-1 | sed 's/@TOKEN@/someone-else/' \
  >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "refusing to remove unowned probe container probe-1"
assert_deletes ""

begin_case "6 probe whose configuration id disagrees is refused"
compliant_report probe-1 |
  sed 's/"configuration":{"id":"probe-1"/"configuration":{"id":"probe-2"/' \
    >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "refusing to remove unowned probe container probe-1"
assert_deletes ""

begin_case "7 multi-entry probe inspection is refused"
compliant_report probe-1 | jq -c '. + .' >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "refusing to remove unowned probe container probe-1"
assert_deletes ""

# --------------------------------------- #353: duplicate-key JSON defense
begin_case "8 duplicated top-level key rejects before field comparison"
compliant_report probe-1 |
  sed 's/^\[{"id":"probe-1",/[{"id":"probe-1","id":"probe-1",/' \
    >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "runtime inspection JSON is ambiguous"
assert_lacks "would fail the ward allowlist"
assert_deletes ""

begin_case "9 duplicated nested key rejects before field comparison"
compliant_report probe-1 |
  sed 's/"executable":"sh",/"executable":"sh","executable":"sh",/' \
    >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "runtime inspection JSON is ambiguous"
assert_deletes ""

begin_case "10 duplicated labels object rejects and blocks the delete"
compliant_report probe-1 |
  sed 's/"labels":{[^}]*},/&&/' >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "runtime inspection JSON is ambiguous"
assert_contains "refusing to remove probe container probe-1: ambiguous runtime JSON"
assert_deletes ""

begin_case "11 duplicate keys appearing only at cleanup refuse the delete"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1-1.json"
compliant_report probe-1 |
  sed 's/"labels":{[^}]*},/&&/' >"$CASE_DIR/inspect-probe-1-2.json"
run_checker
assert_rc 1
assert_contains "refusing to remove probe container probe-1: ambiguous runtime JSON"
assert_deletes ""

# ------------------------------------- #355: cidfile-less probe recovery
begin_case "12 recovery deletes only the ownership-token match"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}},{"id":"foreign-1","configuration":{"id":"foreign-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
printf '%s' '[{"id":"foreign-1","configuration":{"id":"foreign-1","labels":{"ai.freeside.project-image.owner":"someone-else"}}}]' \
  >"$CASE_DIR/inspect-foreign-1.json"
run_checker
assert_rc 1
assert_contains "recovered orphaned probe container owned-1"
assert_deletes "--force owned-1"

begin_case "13 recovery without an owned container deletes nothing"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"foreign-1","configuration":{"id":"foreign-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"foreign-1","configuration":{"id":"foreign-1","labels":{"ai.freeside.project-image.owner":"someone-else"}}}]' \
  >"$CASE_DIR/inspect-foreign-1.json"
run_checker
assert_rc 1
assert_lacks "recovered orphaned probe container"
assert_deletes ""

begin_case "14 recovery skips a candidate whose inspected id disagrees"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"other-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
run_checker
assert_rc 1
assert_lacks "recovered orphaned probe container"
assert_deletes ""

begin_case "15 recovery skips a candidate whose inspected configuration id disagrees"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"other-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
run_checker
assert_rc 1
assert_lacks "recovered orphaned probe container"
assert_deletes ""

begin_case "16 recovery refuses an ambiguous container listing"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "refusing ownership recovery on ambiguous container listing"
assert_deletes ""

begin_case "17 recovery refuses a non-array container listing"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '{"k":{"id":"owned-1","configuration":{"id":"owned-1"}}}' \
  >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "container listing returned an unexpected object"
assert_never_inspected "owned-1"
assert_deletes ""

begin_case "18 recovery flags a listed identity with hostile characters"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"bad id!","configuration":{"id":"bad id!"}}]' \
  >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "ownership recovery listed an invalid container identity"
assert_never_inspected "bad id!"
assert_deletes ""

begin_case "19 recovery flags a listed identity disagreeing with its configuration"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"mismatch-a","configuration":{"id":"mismatch-b"}}]' \
  >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "ownership recovery listed an invalid container identity"
assert_never_inspected "mismatch-a"
assert_deletes ""

begin_case "20 recovery flags a non-object listing entry"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '["garbage"]' >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "ownership recovery listed an invalid container identity"
assert_deletes ""

begin_case "21 recovery flags invalid listed identities, still recovers the owned probe"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"bad id!","configuration":{"id":"bad id!"}},{"id":"mismatch-a","configuration":{"id":"mismatch-b"}},{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
run_checker
assert_rc 1
assert_contains "ownership recovery listed an invalid container identity"
assert_contains "recovered orphaned probe container owned-1"
assert_never_inspected "bad id!"
assert_never_inspected "mismatch-a"
assert_deletes "--force owned-1"

begin_case "22 recovery refuses an ambiguous candidate inspection"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"k":"v"},"labels":{"k":"v"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
run_checker
assert_rc 1
assert_contains "refusing to judge ownership of container owned-1"
assert_deletes ""

begin_case "23 recovery reports an uninspectable candidate and deletes nothing"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "could not inspect container owned-1 for ownership recovery"
assert_deletes ""

begin_case "24 recovery refuses a listing that fails"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
run_checker
assert_rc 1
assert_contains "could not list containers for ownership recovery"
assert_deletes ""

begin_case "25 recovery survives a runtime that drains stdin"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '' >"$CASE_DIR/drain_stdin"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}},{"id":"owned-2","configuration":{"id":"owned-2"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
printf '%s' '[{"id":"owned-2","configuration":{"id":"owned-2","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-2.json"
run_checker
assert_rc 1
assert_deletes "$(printf -- '--force owned-1\n--force owned-2')"

begin_case "26 failed identity-file creation runs no recovery"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1.json"
set +e
OUT=$(TMPDIR=$CASE_DIR/no-such-dir "$CHECKER" example.test/agent:1 "$STUB" 2>&1)
RC=$?
set -e
assert_rc 1
assert_lacks "ownership recovery"
assert_deletes ""

begin_case "27 raw NUL byte inside the inspect report rejects"
# A raw NUL between members is invalid JSON, but command substitution
# strips it, leaving a compliant document; the checker must validate the
# raw bytes from a file, never a NUL-stripped shell variable.
whole=$(compliant_report probe-1)
left=${whole%%'"publishedPorts"'*}
right=${whole#"$left"}
{
  printf '%s' "$left"
  printf '\000'
  printf '%s' "$right"
} >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "runtime inspection JSON is ambiguous"
assert_deletes ""

begin_case "28 raw NUL byte inside the container listing refuses recovery"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
{
  printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}'
  printf '\000'
  printf '%s' ']'
} >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "refusing ownership recovery on ambiguous container listing"
assert_never_inspected "owned-1"
assert_deletes ""

# Runtime output past the daemon-parity 16 MiB cap must fail the capture
# before any parse, not fill the filesystem (17 MiB of 'x' here).
oversized_report() { head -c 17000000 /dev/zero | tr '\0' 'x'; }

begin_case "29 oversized inspect report fails the capture"
oversized_report >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "could not capture the probe inspection"
assert_deletes ""

begin_case "30 oversized container listing refuses recovery"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
oversized_report >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "could not list containers for ownership recovery"
assert_deletes ""

begin_case "31 capture truncated by a failing sink is rejected"
# Under a file-size limit the capture's sink (head) dies after writing a
# truncated prefix; with a compliant report padded past the limit, that
# prefix is a complete, compliant document, so only checking the sink's
# pipeline status rejects it. The 2-block (1024-byte) limit truncates the
# runtime_json capture while every other file the run writes stays small.
{
  compliant_report probe-1
  head -c 5000 /dev/zero | tr '\0' ' '
} >"$CASE_DIR/inspect-probe-1.json"
set +e
OUT=$( (
  ulimit -f 2
  "$CHECKER" example.test/agent:1 "$STUB"
) 2>&1)
RC=$?
set -e
assert_rc 1
assert_contains "could not capture the probe inspection"
assert_deletes ""

begin_case "32 ownership revoked between selection and deletion is refused"
# The pre-delete re-inspection (recovery_delete_owned) must catch a
# candidate whose ownership changed after the selection inspection: the
# first inspection shows this run's token, the second a foreign one.
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1-1.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"someone-else"}}}]' \
  >"$CASE_DIR/inspect-owned-1-2.json"
run_checker
assert_rc 1
assert_contains "refusing recovery deletion of container owned-1: ownership no longer verified"
assert_lacks "recovered orphaned probe container"
assert_deletes ""

# ------------------------------------------ #368: wall-clock runtime bounds
begin_case "33 a hung main-flow inspection fails within the configured bound"
printf '' >"$CASE_DIR/hang-inspect-probe-1-1"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1-2.json"
export FREESIDE_CHECK_AGENT_IMAGE_RUNTIME_BOUND_SECONDS=1
run_checker
assert_rc 1
assert_contains "runtime call exceeded 1s"
assert_contains "could not capture the probe inspection"
assert_deletes "--force probe-1"

begin_case "34 a hung cleanup deletion fails closed within the configured bound"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1.json"
printf '' >"$CASE_DIR/hang-delete"
export FREESIDE_CHECK_AGENT_IMAGE_RUNTIME_BOUND_SECONDS=1
run_checker
assert_rc 1
assert_contains "runtime call exceeded 1s"
assert_contains "could not remove probe container probe-1"
assert_deletes "--force probe-1"

begin_case "35 a descendant retaining the capture pipe is bounded"
printf '' >"$CASE_DIR/fork-hang-inspect-probe-1-1"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1-2.json"
export FREESIDE_CHECK_AGENT_IMAGE_RUNTIME_BOUND_SECONDS=1
run_checker
assert_rc 1
assert_contains "runtime call exceeded 1s"
assert_contains "could not capture the probe inspection"
assert_deletes "--force probe-1"
assert_helper_stopped

begin_case "36 TERM reaches a runtime descendant during capture"
printf '' >"$CASE_DIR/fork-hang-inspect-probe-1-1"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1-2.json"
run_checker_then_term
assert_rc 130
assert_deletes "--force probe-1"
assert_helper_stopped

begin_case "37 a detached descendant cannot hold the capture open"
printf '' >"$CASE_DIR/detach-hang-inspect-probe-1-1"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1-2.json"
export FREESIDE_CHECK_AGENT_IMAGE_RUNTIME_BOUND_SECONDS=1
run_checker
assert_rc 1
assert_contains "runtime call exceeded 1s"
assert_contains "could not capture the probe inspection"
assert_deletes "--force probe-1"
detached_helper_pid=$(cat "$CASE_DIR/helper-pid" 2>/dev/null || true)
[ -z "$detached_helper_pid" ] || kill "$detached_helper_pid" 2>/dev/null || true

begin_case "38 normal completion ignores a descriptor-free same-group helper"
printf '' >"$CASE_DIR/orphan-inspect-probe-1-1"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1.json"
export FREESIDE_CHECK_AGENT_IMAGE_RUNTIME_BOUND_SECONDS=1
run_checker
assert_rc 0
assert_contains "satisfies the ward allowlist"
assert_lacks "runtime call exceeded"
assert_deletes "--force probe-1"
orphan_helper_pid=$(cat "$CASE_DIR/helper-pid" 2>/dev/null || true)
if [ -n "$orphan_helper_pid" ] && kill -0 "$orphan_helper_pid" 2>/dev/null; then
  pass=$((pass + 1))
else
  report_failure "descriptor-free runtime helper did not survive to prove normal completion"
fi
[ -z "$orphan_helper_pid" ] || kill "$orphan_helper_pid" 2>/dev/null || true

begin_case "39 the configured bound includes timeout termination"
printf '' >"$CASE_DIR/ignore-term-inspect-probe-1-1"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1-2.json"
export FREESIDE_CHECK_AGENT_IMAGE_RUNTIME_BOUND_SECONDS=1
run_checker_with_sleep_log
assert_rc 1
assert_contains "runtime call exceeded 1s"
if grep -qv '^0\.1$' "$CASE_DIR/sleeps.log" 2>/dev/null; then
  report_failure "watchdog scheduled a non-polling sleep: $(cat "$CASE_DIR/sleeps.log")"
else
  pass=$((pass + 1))
fi
assert_deletes "--force probe-1"

# -------------------------------------- #523: pinned-exporter tool preflight
begin_case "40 every observer executable is recognized as a required tool"
for required_tool in \
  sh git env mkdir rm cat head find xargs sort cmp sha256sum cut readlink stat ls sync; do
  printf 'freeside-missing-tool:%s\n' "$required_tool"
done >"$CASE_DIR/run_output"
printf '127' >"$CASE_DIR/run_status"
run_real_work
assert_rc 2
assert_contains "example.test/exporter@sha256:"
assert_contains "missing required observer tools: sh git env mkdir rm cat head find xargs sort cmp sha256sum cut readlink stat ls sync"
assert_exists "$CASE_DIR/go.log"
assert_exists "$CASE_DIR/rig-hold.args"

begin_case "41 a current exporter pin passes preflight and reaches the build"
run_real_work
assert_rc 1
assert_contains "building freesided"
assert_lacks "missing required observer tools"
assert_exists "$CASE_DIR/go.log"
assert_contains "could not record the auth identity binding"

begin_case "42 an unrecognized missing-tool marker is a generic runtime failure"
printf '%s\n' 'freeside-missing-tool:not-from-observer-script' \
  >"$CASE_DIR/run_output"
printf '127' >"$CASE_DIR/run_status"
run_real_work
assert_rc 2
assert_contains "could not preflight exporter image"
assert_lacks "missing required observer tools"
assert_exists "$CASE_DIR/go.log"

begin_case "43 a non-command-not-found exit cannot convict the image contents"
printf '%s\n' 'freeside-missing-tool:git' >"$CASE_DIR/run_output"
printf '1' >"$CASE_DIR/run_status"
run_real_work
assert_rc 2
assert_contains "could not preflight exporter image"
assert_lacks "missing required observer tools"
assert_exists "$CASE_DIR/go.log"

begin_case "44 oversized exporter preflight output is capped before buffering"
{
  printf '%s\n' 'freeside-missing-tool:git'
  head -c 131072 /dev/zero | tr '\0' 'x'
} >"$CASE_DIR/run_output"
printf '127' >"$CASE_DIR/run_status"
run_real_work
assert_rc 2
assert_contains "exporter preflight output exceeded the 65536-byte cap"
assert_contains "could not preflight exporter image"
assert_lacks "missing required observer tools"
assert_exists "$CASE_DIR/go.log"

begin_case "45 a NUL-bearing marker cannot convict the exporter image"
{
  printf '%s' 'freeside-missing-tool:git'
  printf '\000\n'
} >"$CASE_DIR/run_output"
printf '127' >"$CASE_DIR/run_status"
run_real_work
assert_rc 2
assert_contains "could not preflight exporter image"
assert_lacks "missing required observer tools"
assert_exists "$CASE_DIR/go.log"

begin_case "46 the gated real-work harness keeps both lane identities distinct"
run_real_work lifecycle
assert_rc 0
assert_contains "submitted elaboration run=elab-run invocation=elab-inv"
assert_contains "reserved implementation run=impl-run invocation=impl-inv"
assert_contains "gated-unattended: waiting for an operator"
if grep -qx $'impl-run\timpl-inv' "$CASE_DIR/verification-identities.log" &&
  ! grep -q 'elab-inv' "$CASE_DIR/verification-identities.log"; then
	pass=$((pass + 1))
else
	report_failure "verification did not stay bound to the future implementation identity"
fi
if grep -qx -- '-prompt-package' "$CASE_DIR/daemon.args" &&
  grep -qx -- '-elaboration-prompt-package' "$CASE_DIR/daemon.args"; then
	pass=$((pass + 1))
else
	report_failure "daemon invocation omitted one of the stage prompt flags"
fi
if grep -q -- '-rig-token-file' "$CASE_DIR/daemon.args" &&
  grep -q -- 'rig cleanup -state-root' "$CASE_DIR/rig-cleanup.args"; then
	pass=$((pass + 1))
else
	report_failure "daemon did not receive dynamic rig binding authority and clean up through the live lease"
fi
rig_lifecycle=$(tr '\n' ' ' <"$CASE_DIR/lifecycle.log")
case "$rig_lifecycle" in
*"rig-hold submit daemon-start daemon-stop rig-cleanup rig-release "*)
	pass=$((pass + 1)) ;;
*) report_failure "rig lifecycle was out of order: $rig_lifecycle" ;;
esac

begin_case "47 a legacy production-only replay remains runnable across upgrade"
run_real_work lifecycle legacy
assert_rc 0
assert_contains "legacy production-only replay: no elaboration approval gate"
assert_contains "reserved implementation run=impl-run invocation=impl-inv"
assert_lacks "gated-unattended: waiting for an operator"
if grep -qx $'impl-run\timpl-inv' "$CASE_DIR/verification-identities.log"; then
	pass=$((pass + 1))
else
	report_failure "legacy replay verification lost the implementation identity"
fi
if grep -q -- '-rig-token-file' "$CASE_DIR/daemon.args" &&
  [ ! -e "$CASE_DIR/rig-bind.args" ]; then
	pass=$((pass + 1))
else
	report_failure "legacy replay did not delegate per-invocation rig binding to the daemon"
fi

begin_case "48 a competing rig refuses before preflight or submit"
run_real_work lifecycle current refuse
assert_rc 1
assert_contains "production rig lease is held by other@host"
assert_not_exists "$CASE_DIR/submit.called"
if ! grep -q '^run --rm --network none' "$CASE_DIR/calls.log" 2>/dev/null; then
	pass=$((pass + 1))
else
	report_failure "refused rig still launched the exporter preflight"
fi

begin_case "49 a failed clean release fails the campaign"
run_real_work lifecycle current release-fail
assert_rc 1
assert_contains "rig holder failed during release"

begin_case "50 a wedged clean release is killed within its bound"
export FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS=1
run_real_work lifecycle current release-hang
assert_rc 1
assert_contains "rig holder did not exit within 1s; sending SIGKILL"

begin_case "51 a stopped clean release is killed within its bound"
export FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS=1
run_real_work lifecycle current release-stop
assert_rc 1
assert_contains "rig holder did not exit within 1s; sending SIGKILL"

begin_case "52 a wedged exact-resource cleanup is cancelled within its bound"
export FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS=1
run_real_work lifecycle current ok procbound-hang
assert_rc 1
assert_contains "exact-resource cleanup exceeded 1s; cancelling it"
assert_contains "exact-resource cleanup failed; preserving the stale rig manifest"
assert_helper_stopped

begin_case "53 failed cleanup cannot orphan a mutating descendant"
export FREESIDE_REAL_RUN_RIG_RELEASE_TIMEOUT_SECONDS=1
run_real_work lifecycle current ok orphan
assert_rc 1
assert_contains "exact-resource cleanup failed; preserving the stale rig manifest"
assert_helper_stopped

# --------------------- detector battery: adversarial input-space fixtures
# Each document stands in for the whole inspect report. Duplicates of every
# shape must reject with the ambiguity error; structurally valid documents
# must never trip it, and must instead reach the field comparison (the
# pre-existing failure mode for a non-compliant report).
for doc in \
  '{"a":1,"a":2}' \
  '{"a":{"x":1},"a":{"y":2}}' \
  '{"a":{"x":1},"a":{"x":2}}' \
  '{"a":{"x":1},"a":2}' \
  '{"a":2,"a":{"x":1}}' \
  '{"a":{},"a":{}}' \
  '{"a":[1],"a":[2]}' \
  '{"A":1,"a":2}' \
  '{"a":1} {"b":2}' \
  '' \
  'not json' \
  '{"a":1,}'; do
  begin_case "detector rejects: ${doc:-<empty>}"
  printf '%s' "$doc" >"$CASE_DIR/inspect-probe-1.json"
  run_checker
  assert_rc 1
  assert_contains "runtime inspection JSON is ambiguous"
done

for doc in \
  '{"a":1,"b":2}' \
  '{"a":{"x":1},"b":{"x":2}}' \
  '[{"a":1},{"a":2}]' \
  '{"a":{"b":{"c":1}}}' \
  '{"a":{},"b":[]}' \
  '{"a":[[1],[2]],"b":[{"x":[]},{"x":{}}]}' \
  '42'; do
  begin_case "detector passes: $doc"
  printf '%s' "$doc" >"$CASE_DIR/inspect-probe-1.json"
  run_checker
  assert_rc 1
  assert_lacks "runtime inspection JSON is ambiguous"
  assert_contains "would fail the ward allowlist"
done

# ---------------------------------------------------------------- summary
echo
echo "passed $pass assertion(s), failed $fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
