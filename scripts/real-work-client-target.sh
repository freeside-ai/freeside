#!/usr/bin/env bash
# Client-target mode for run-real-work.sh. The operator submits the real work
# item from a Freeside client; the harness follows and verifies exactly that
# task and never its own CLI visibility seed. Sourced by run-real-work.sh; it
# relies on that script's child_job_exists and require_live_rig helpers, the
# retained verifier binary ($session/verify-real-run), and the resolver
# TestRealRunClientTarget. It never writes to the database.

# real_work_await_target waits for the operator to name a client task with
# `real-work-session.sh select-target`, validates it against the live store, and
# saves the selection in $session/target.json before following anything. A
# refused request prints its reason and the wait continues, so the operator can
# correct the ID. Like the walkthrough it has no deadline; interruption runs the
# caller's normal cleanup. Returns 0 once a target is saved, 1 if the daemon or
# rig is lost first.
real_work_await_target() {
	local session=$1 daemon=$2
	printf 'awaiting-target\n' >"$session/status"
	echo "run-real-work: awaiting a client target" >&2
	printf 'Select the client task: bash %q select-target %q <task-id>\n' \
		"$session/real-work-session.sh" "$session" >&2
	local task_id decision outcome reason rc
	decision="$session/target-decision.json"
	while :; do
		if ! child_job_exists "$daemon" || ! require_live_rig; then
			echo 'run-real-work: daemon or rig lost while awaiting a client target' >&2
			return 1
		fi
		if [[ ! -f "$session/target.request" ]]; then
			sleep 1
			continue
		fi
		task_id=$(<"$session/target.request")
		task_id=${task_id//[$'\t\r\n ']/}
		rm -f "$decision"
		if [[ -z "$task_id" ]]; then
			echo 'run-real-work: ignoring an empty target request' >&2
			rm -f "$session/target.request"
			sleep 1
			continue
		fi
		rc=0
		# Capture without toggling set -e (a set -e leak would trip callers).
		env FREESIDE_REAL_RUN_CLIENT_TARGET=select \
			FREESIDE_REAL_RUN_TARGET_TASK_ID="$task_id" \
			FREESIDE_REAL_RUN_SEED_SPECIFICATION_RUN_ID="$(cat "$session/seed-specification-run" 2>/dev/null || true)" \
			FREESIDE_REAL_RUN_DAEMON_STARTED_AT="$(cat "$session/daemon-started-at")" \
			FREESIDE_REAL_RUN_TARGET_PATH="$decision" \
			"$session/verify-real-run" -test.run '^TestRealRunClientTarget$' -test.count=1 \
			>"$session/select-target.log" 2>&1 || rc=$?
		if [[ "$rc" -ne 0 || ! -s "$decision" ]]; then
			echo "run-real-work: could not evaluate target task $task_id; see $session/select-target.log" >&2
			rm -f "$session/target.request"
			sleep 1
			continue
		fi
		outcome=$(real_work_client_target_field "$decision" outcome)
		if [[ "$outcome" == selected ]]; then
			real_work_client_target_save "$decision" "$session/target.json"
			rm -f "$session/target.request"
			echo "run-real-work: selected client target task $task_id" >&2
			return 0
		fi
		reason=$(real_work_client_target_field "$decision" reason)
		echo "run-real-work: target task $task_id refused: $reason" >&2
		rm -f "$session/target.request"
		sleep 1
	done
}

# real_work_bind_target resolves the selected task's implementation run and adds
# it to target.json with an atomic replace, refusing a task or specification-run
# change since selection. Return: 0 bound, 3 not bound yet (keep polling), 1 a
# binding error (two paired runs, wrong owner). An infrastructure failure is
# reported as not-bound so supervision keeps polling within its own deadline.
real_work_bind_target() {
	local session=$1
	local target="$session/target.json" decision="$session/bind-decision.json"
	local task_id project spec_run outcome rc
	task_id=$(real_work_client_target_field "$target" task_id)
	project=$(real_work_client_target_field "$target" project)
	spec_run=$(real_work_client_target_field "$target" specification_run_id)
	rm -f "$decision"
	rc=0
	# Capture without toggling set -e (a set -e leak would trip callers).
	env FREESIDE_REAL_RUN_CLIENT_TARGET=bind \
		FREESIDE_REAL_RUN_TARGET_TASK_ID="$task_id" \
		FREESIDE_REAL_RUN_PROJECT="$project" \
		FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID="$spec_run" \
		FREESIDE_REAL_RUN_TARGET_PATH="$decision" \
		"$session/verify-real-run" -test.run '^TestRealRunClientTarget$' -test.count=1 \
		>"$session/bind-target.log" 2>&1 || rc=$?
	if [[ "$rc" -ne 0 || ! -s "$decision" ]]; then
		return 3
	fi
	outcome=$(real_work_client_target_field "$decision" outcome)
	case "$outcome" in
	bound)
		real_work_client_target_merge_binding "$target" "$decision" "$task_id" "$spec_run"
		return 0
		;;
	pending) return 3 ;;
	*)
		echo "run-real-work: implementation binding error: $(real_work_client_target_field "$decision" reason)" >&2
		return 1
		;;
	esac
}

real_work_client_target_field() {
	python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$1" "$2"
}

# real_work_client_target_save writes the selection to target.json, no-clobber,
# so a saved selection can never be replaced in the same session.
real_work_client_target_save() {
	python3 - "$1" "$2" <<'PY'
import json, os, sys
decision = json.load(open(sys.argv[1]))
record = {
    "task_id": decision["task_id"],
    "project": decision["project"],
    "specification_run_id": decision["specification_run_id"],
}
fd = os.open(sys.argv[2], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w") as out:
    out.write(json.dumps(record) + "\n")
PY
}

# real_work_client_target_merge_binding adds the implementation identity to
# target.json atomically, refusing if the task or specification run changed
# since bind read them.
real_work_client_target_merge_binding() {
	python3 - "$1" "$2" "$3" "$4" <<'PY'
import json, os, sys, tempfile
target_path, decision_path, expected_task, expected_spec = sys.argv[1:5]
target = json.load(open(target_path))
if target.get("task_id") != expected_task or target.get("specification_run_id") != expected_spec:
    raise SystemExit("target selection changed under the binding step")
decision = json.load(open(decision_path))
target["implementation_run_id"] = decision["implementation_run_id"]
target["implementation_invocation_id"] = decision["implementation_invocation_id"]
directory = os.path.dirname(target_path)
fd, tmp = tempfile.mkstemp(dir=directory)
try:
    with os.fdopen(fd, "w") as out:
        os.fchmod(out.fileno(), 0o600)
        out.write(json.dumps(target) + "\n")
    os.replace(tmp, target_path)
except BaseException:
    os.unlink(tmp)
    raise
PY
}
