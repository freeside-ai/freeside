#!/usr/bin/env bash
# Shared bounded rig cleanup. The supervisor reserves its process group until
# cancellation finishes, including when a runtime child outlives freesided.
real_work_bounded_rig() {
	local session=$1 bound=$2
	shift 2
	local supervisor result command_pid
	local rig_daemon="$session/freesided"
	if [[ "${1:-}" == recover && -n "${FREESIDE_REAL_RUN_RECOVERY_DAEMON:-}" ]]; then
		rig_daemon=$FREESIDE_REAL_RUN_RECOVERY_DAEMON
		[[ "$rig_daemon" == /* && -x "$rig_daemon" ]] || {
			echo 'Recovery daemon must be an absolute executable path.' >&2
			return 1
		}
	fi
	local result_file=$session/rig-command-status
	rm -f "$result_file"
	set -m
	(
		set +m
		set +e
		"$rig_daemon" rig "$@" >>"$session/rig-cleanup.log" 2>&1 &
		command_pid=$!
		trap '' TERM
		wait "$command_pid"
		printf '%s\n' "$?" >"$result_file"
		while :; do sleep 3600; done
	) &
	supervisor=$!
	for _ in $(seq 1 $((bound * 10))); do
		[[ ! -s "$result_file" ]] || break
		sleep 0.1
	done
	if [[ -s "$result_file" ]]; then
		read -r result <"$result_file" || result=1
	else
		echo "real-work: exact-resource cleanup exceeded ${bound}s; cancelling it" >&2
		result=124
		kill -TERM -- "-$supervisor" 2>/dev/null || true
		for _ in $(seq 1 $((bound * 10))); do
			[[ ! -s "$result_file" ]] || break
			sleep 0.1
		done
	fi
	kill -KILL -- "-$supervisor" 2>/dev/null || true
	wait "$supervisor" 2>/dev/null || true
	set +m
	rm -f "$result_file"
	return "$result"
}

# No stdin or deadline can complete a walkthrough. Liveness is checked before
# the completion request, so a dead service never counts as a successful tour.
real_work_walkthrough() {
	local session=$1 daemon=$2
	printf 'walkthrough\n' >"$session/status"
	while :; do
		if ! child_job_exists "$daemon" || ! require_live_rig; then
			echo 'run-real-work: daemon or rig lost during walkthrough' >&2
			return 1
		fi
		[[ ! -f "$session/complete.request" ]] || return 0
		sleep 1
	done
}

# A writable runtime upgrade cannot fall back to an unverified older service.
# Installation remains the operator's job; recovery never rolls back durable data.
real_work_restore_supervised() {
	local session=$1 installed=${2:-${FREESIDE_REAL_RUN_RESTORE_DAEMON:-$HOME/Applications/Freeside.app/Contents/Resources/freesided}}
	local expected actual daemon="$session/freesided" verifier="$session/verify-real-run" version="$session/build-version"
	if [[ ! -f "$session/runtime-upgrade-started" && -f "$session/runtime-upgrade-inherited" ]]; then
		daemon="$session/restore-freesided"
		verifier="$session/restore-verifier"
		version="$session/restore-build-version"
	fi
	if [[ -f "$session/runtime-upgrade-started" || -f "$session/runtime-upgrade-inherited" ]]; then
		if [[ ! -x "$installed" ]] ||
			! expected=$(go tool buildid "$daemon") ||
			! actual=$(go tool buildid "$installed") ||
			[[ -z "$expected" || "$actual" != "$expected" ]]; then
			echo 'Recovery requires installing the session daemon with install-mac-app.sh --daemon-path before restoring the service.' >&2
			return 1
		fi
		if ! FREESIDE_REAL_RUN_SCHEMA_TEST=1 \
			FREESIDE_REAL_RUN_STATE_ROOT="$(cat "$session/state-root")" \
			"$verifier" -test.run '^TestRealRunRetainedSchema$' -test.count=1 \
			>"$session/verify-schema.log" 2>&1; then
			echo "Retained schema compatibility refused; inspect $session/verify-schema.log. No data was restored." >&2
			return 1
		fi
	fi
	bash "$session/restore-supervised-daemon.sh" 2>&1 | tee -a "$session/restore.log" || return 1
	if [[ -f "$session/runtime-upgrade-started" || -f "$session/runtime-upgrade-inherited" ]]; then
		curl --fail --silent --show-error --max-time 2 http://127.0.0.1:7331/health >"$session/restored-health.json" &&
			python3 "$session/real-work-retained.py" health "$session/restored-health.json" "$(cat "$version")"
	fi
}
