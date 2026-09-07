#!/usr/bin/env bash
# Shared bounded rig cleanup. The supervisor reserves its process group until
# cancellation finishes, including when a runtime child outlives freesided.
real_work_bounded_rig() {
	local session=$1 bound=$2
	shift 2
	local supervisor result command_pid
	local result_file=$session/rig-command-status
	rm -f "$result_file"
	set -m
	(
		set +m
		set +e
		"$session/freesided" rig "$@" >>"$session/rig-cleanup.log" 2>&1 &
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
