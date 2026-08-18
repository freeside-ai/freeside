#!/usr/bin/env bash
# Read-only lifecycle supervision shared by run-real-work.sh and its fixtures.

real_work_snapshot_field() {
	local path=$1 field=$2
	sed -n "s/.*\"${field}\":\"\([^\"]*\)\".*/\1/p" "$path" | tail -1
}

real_work_report_failure() {
	local lane=$1 run_id=$2 snapshot=$3
	local outcome reason terminal attention_id attention_type
	outcome=$(real_work_snapshot_field "$snapshot" outcome)
	reason=$(real_work_snapshot_field "$snapshot" outcome_reason)
	terminal=$(real_work_snapshot_field "$snapshot" terminal)
	attention_id=$(real_work_snapshot_field "$snapshot" id)
	attention_type=$(real_work_snapshot_field "$snapshot" type)
	printf 'run-real-work: %s run=%s ended outcome=%s' "$lane" "$run_id" "$outcome" >&2
	[[ -z "$reason" ]] || printf ' reason=%s' "$reason" >&2
	[[ -z "$terminal" ]] || printf ' terminal=%s' "$terminal" >&2
	[[ -z "$attention_id" ]] || printf ' attention=%s type=%s' "$attention_id" "$attention_type" >&2
	printf '\n' >&2
}

# real_work_supervise follows the elaboration identity until it binds the exact
# implementation run, then follows that run to publication or actionable
# attention. It keeps reconciliation live through actionable attention, and
# returns 0 once publication is durably accepted, 124 for the global timeout,
# and 1 for terminal or observation failure.
real_work_supervise() {
	local freesided=$1 db_path=$2 elaboration_run_id=$3 implementation_run_id=$4
	local daemon_pid=$5 timeout_seconds=$6 snapshot_path=$7 interval_seconds=${8:-1}
	local lane run_id state previous_state="" state_changed deadline
	deadline=$((SECONDS + timeout_seconds))
	if [[ -n "$elaboration_run_id" ]]; then
		lane=elaboration
		run_id=$elaboration_run_id
	else
		lane=implementation
		run_id=$implementation_run_id
	fi

	while ((SECONDS < deadline)); do
		if declare -F require_live_rig >/dev/null && ! require_live_rig; then
			return 1
		fi
		if [[ -n "$daemon_pid" ]] && ! kill -0 "$daemon_pid" 2>/dev/null; then
			echo "run-real-work: the daemon exited before $lane run=$run_id finished" >&2
			return 1
		fi
		local snapshot_args=(follow -db "$db_path" -run "$run_id" -snapshot)
		if [[ -n "${FREESIDE_REAL_RUN_APPROVED_RECIPE:-}" ]]; then
			snapshot_args+=(-approved-recipe "$FREESIDE_REAL_RUN_APPROVED_RECIPE")
		fi
		if ! "$freesided" "${snapshot_args[@]}" >"$snapshot_path"; then
			echo "run-real-work: could not observe $lane run=$run_id" >&2
			return 1
		fi
		state=$(real_work_snapshot_field "$snapshot_path" state)
		if [[ -z "$state" ]]; then
			echo "run-real-work: snapshot for $lane run=$run_id has no state" >&2
			return 1
		fi
		state_changed=false
		if [[ "$state" != "$previous_state" ]]; then
			state_changed=true
			echo "run-real-work: $lane run=$run_id state=$state" >&2
			previous_state=$state
		fi
		case "$lane:$state" in
		elaboration:implementation_bound)
			lane=implementation
			run_id=$implementation_run_id
			previous_state=""
			continue
			;;
		elaboration:waiting_for_specification_approval|elaboration:pending|elaboration:unobserved|implementation:pending|implementation:unobserved|implementation:publication_ready)
			;;
		implementation:published)
			return 0
			;;
		implementation:attention_required)
			if [[ "$state_changed" == true ]]; then
				echo "run-real-work: attention required for implementation run=$run_id; daemon reconciliation remains active" >&2
				printf 'observe exact run: freesided resume -db %q -run %q\n' "$db_path" "$run_id" >&2
			fi
			;;
		elaboration:attention_required|elaboration:failed|elaboration:lost|elaboration:blocked|implementation:failed|implementation:lost|implementation:blocked)
			real_work_report_failure "$lane" "$run_id" "$snapshot_path"
			return 1
			;;
		*)
			echo "run-real-work: unsupported $lane supervision state=$state for run=$run_id" >&2
			return 1
			;;
		esac
		sleep "$interval_seconds"
	done
	echo "run-real-work: timed out supervising $lane run=$run_id" >&2
	return 124
}
