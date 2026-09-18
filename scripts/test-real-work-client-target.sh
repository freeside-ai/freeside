#!/usr/bin/env bash
# Hermetic client-target await/bind fixtures. A stand-in verify-real-run stands
# for the resolver; no daemon, store, or network. It exercises the wait loop's
# refusal/success handling, daemon/rig loss, and the bind decision mapping.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
await_pid=""
# Reap a still-running background await before removing the fixtures, so a
# failed assertion cannot leave an orphan waiting forever.
trap '[[ -z "$await_pid" ]] || kill "$await_pid" 2>/dev/null || true; rm -rf "$tmp"' EXIT
# shellcheck source=scripts/real-work-client-target.sh
source "$root/scripts/real-work-client-target.sh"

# The harness normally defines these; the fixture controls liveness via files.
daemon_live=1
rig_live=1
child_job_exists() { [[ "$daemon_live" == 1 ]]; }
require_live_rig() { [[ "$rig_live" == 1 ]]; }

new_session() {
	session=$tmp/$1
	mkdir -p "$session"
	printf 'run-spec-seed\n' >"$session/seed-specification-run"
	printf '%s\n' "$(python3 -c 'import time; print(time.time_ns())')" >"$session/daemon-started-at"
	cat >"$session/verify-real-run" <<'STUB'
#!/usr/bin/env bash
sess=$(dirname "$FREESIDE_REAL_RUN_TARGET_PATH")
case "$FREESIDE_REAL_RUN_CLIENT_TARGET" in
select)
	if [[ "$FREESIDE_REAL_RUN_TARGET_TASK_ID" == good ]]; then
		printf '{"outcome":"selected","task_id":"good","project":"proj","specification_run_id":"run-spec"}\n' >"$FREESIDE_REAL_RUN_TARGET_PATH"
	else
		printf '{"outcome":"refused","reason":"refused %s"}\n' "$FREESIDE_REAL_RUN_TARGET_TASK_ID" >"$FREESIDE_REAL_RUN_TARGET_PATH"
	fi
	;;
bind)
	case "$(cat "$sess/bind-mode" 2>/dev/null || echo pending)" in
	bound) printf '{"outcome":"bound","implementation_run_id":"run-impl","implementation_invocation_id":"inv-implement-run-impl"}\n' >"$FREESIDE_REAL_RUN_TARGET_PATH" ;;
	error) printf '{"outcome":"error","reason":"two paired runs"}\n' >"$FREESIDE_REAL_RUN_TARGET_PATH" ;;
	fail) exit 1 ;;
	*) printf '{"outcome":"pending"}\n' >"$FREESIDE_REAL_RUN_TARGET_PATH" ;;
	esac
	;;
esac
STUB
	chmod +x "$session/verify-real-run"
}

poll_until() {
	local deadline=$((SECONDS + 10))
	until "$@"; do
		[[ "$SECONDS" -lt "$deadline" ]] || { echo "poll timed out: $*" >&2; exit 1; }
		sleep 0.1
	done
}

# A refusal deletes the request and keeps waiting, leaving no target.json; a
# later valid request is accepted and saved exactly once.
new_session await
daemon_live=1
rig_live=1
# Clear the inherited EXIT trap in the child so its own exit never removes the
# fixtures out from under the parent.
( trap - EXIT; real_work_await_target "$session" 999 ) &
await_pid=$!
printf 'bad\n' >"$session/target.request"
poll_until test ! -f "$session/target.request"
[[ ! -f "$session/target.json" ]] || { echo 'refusal left a target.json' >&2; exit 1; }
printf 'good\n' >"$session/target.request"
wait "$await_pid"
await_pid=""
[[ "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["task_id"])' "$session/target.json")" == good ]]
# The saved selection cannot be rewritten (no-clobber).
if real_work_client_target_save "$session/target-decision.json" "$session/target.json" 2>/dev/null; then
	echo 'a saved selection was overwritten' >&2; exit 1
fi

# Losing the daemon (or rig) ends the wait with failure rather than hanging.
new_session daemon-loss
daemon_live=0 rig_live=1
if real_work_await_target "$session" 999 >"$session/out" 2>&1; then
	echo 'await returned success after daemon loss' >&2; exit 1
fi
new_session rig-loss
daemon_live=1 rig_live=0
if real_work_await_target "$session" 999 >"$session/out" 2>&1; then
	echo 'await returned success after rig loss' >&2; exit 1
fi
daemon_live=1 rig_live=1

# Binding: pending keeps polling (3), bound adds the run (0), error is terminal (1).
new_session bind
printf '{"task_id":"good","project":"proj","specification_run_id":"run-spec"}\n' >"$session/target.json"
printf 'pending\n' >"$session/bind-mode"
rc=0; real_work_bind_target "$session" || rc=$?
[[ "$rc" == 3 ]] || { echo "pending bind rc=$rc, want 3" >&2; exit 1; }
printf 'bound\n' >"$session/bind-mode"
rc=0; real_work_bind_target "$session" || rc=$?
[[ "$rc" == 0 ]] || { echo "bound bind rc=$rc, want 0" >&2; exit 1; }
[[ "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["implementation_run_id"])' "$session/target.json")" == run-impl ]]
[[ "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["implementation_invocation_id"])' "$session/target.json")" == inv-implement-run-impl ]]
new_session bind-error
printf '{"task_id":"good","project":"proj","specification_run_id":"run-spec"}\n' >"$session/target.json"
printf 'error\n' >"$session/bind-mode"
rc=0; real_work_bind_target "$session" || rc=$?
[[ "$rc" == 1 ]] || { echo "error bind rc=$rc, want 1" >&2; exit 1; }
new_session bind-infra
printf '{"task_id":"good","project":"proj","specification_run_id":"run-spec"}\n' >"$session/target.json"
printf 'fail\n' >"$session/bind-mode"
rc=0; real_work_bind_target "$session" || rc=$?
[[ "$rc" == 3 ]] || { echo "infra-failure bind rc=$rc, want 3 (keep polling)" >&2; exit 1; }

# The atomic merge refuses when the saved selection changed under it.
new_session merge-guard
printf '{"task_id":"good","specification_run_id":"run-spec"}\n' >"$session/target.json"
printf '{"implementation_run_id":"run-impl","implementation_invocation_id":"inv-x"}\n' >"$session/bind-decision.json"
if real_work_client_target_merge_binding "$session/target.json" "$session/bind-decision.json" other-task run-spec 2>/dev/null; then
	echo 'merge accepted a changed task selection' >&2; exit 1
fi
if real_work_client_target_merge_binding "$session/target.json" "$session/bind-decision.json" good run-other 2>/dev/null; then
	echo 'merge accepted a changed specification run' >&2; exit 1
fi
real_work_client_target_merge_binding "$session/target.json" "$session/bind-decision.json" good run-spec
[[ "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["implementation_run_id"])' "$session/target.json")" == run-impl ]]

echo 'PASS: client-target await, bind, and merge fixtures'
