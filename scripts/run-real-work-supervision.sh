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

# real_work_waits_on_human reports whether a supervision state is a decision
# gate: the daemon is idle and only a human can advance the run. A hold is not
# one here, because the follow snapshot does not show it.
real_work_waits_on_human() {
	case "$1:$2" in
	specification:waiting_for_specification_approval | implementation:attention_required)
		return 0
		;;
	esac
	return 1
}

# real_work_supervise follows the specification identity until it binds the exact
# implementation run, then follows that run to publication or actionable
# attention. It keeps reconciliation live through actionable attention, and
# returns 0 once publication is durably accepted or the implementation has
# completed, 124 once executing time reaches the timeout, 1 for any other
# terminal state (including specification completion) or observation that
# stays unreadable across FREESIDE_REAL_RUN_MAX_OBSERVATION_FAILURES
# consecutive attempts (default 10), and 2 for an invalid numeric
# configuration. The timeout bounds a runaway unattended run, so its clock
# pauses while the run waits on a human (real_work_waits_on_human) and resumes
# when the run leaves that state; startup, holds, and unreadable observations
# still count. Liveness checks run on every observation, paused or not.
real_work_supervise() {
	local freesided=$1 db_path=$2 specification_run_id=$3 implementation_run_id=$4
	local daemon_pid=$5 timeout_seconds=$6 snapshot_path=$7 interval_seconds=${8:-1}
	local lane run_id state previous_state="" state_changed resolved
	local spent=0 last_tick now gated=false waiting
	local observation_failures=0
	local max_observation_failures=${FREESIDE_REAL_RUN_MAX_OBSERVATION_FAILURES:-10}
	# Both counters drive Bash arithmetic below. A malformed value aborts under
	# nounset and a zero-padded one parses as invalid octal, so an unchecked
	# operator override would either kill supervision or error on every
	# observation until the global timeout. Reject anything but a positive
	# base-10 integer up front, matching run-real-work.sh's sibling rig-timeout
	# guard, so the operator sees the mistake immediately.
	if [[ ! "$timeout_seconds" =~ ^[1-9][0-9]*$ ]]; then
		echo "run-real-work: supervision timeout must be a positive integer, got: $timeout_seconds" >&2
		return 2
	fi
	if [[ ! "$max_observation_failures" =~ ^[1-9][0-9]*$ ]]; then
		echo "run-real-work: FREESIDE_REAL_RUN_MAX_OBSERVATION_FAILURES must be a positive integer, got: $max_observation_failures" >&2
		return 2
	fi
	if [[ -n "$specification_run_id" ]]; then
		lane=specification
		run_id=$specification_run_id
	else
		lane=implementation
		run_id=$implementation_run_id
	fi

	last_tick=$SECONDS
	while true; do
		# Each interval is charged to the state observed at its start, so a
		# gate's wait, including its sleeps, never reaches the budget.
		now=$SECONDS
		[[ "$gated" == true ]] || spent=$((spent + now - last_tick))
		last_tick=$now
		((spent < timeout_seconds)) || break
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
			# An observation runs against the same SQLite file the daemon is
			# writing, so a lock collision (SQLITE_BUSY) is an expected
			# transient, not a run failure. Retry on the observation cadence
			# and fail only when the reader cannot get through at all.
			observation_failures=$((observation_failures + 1))
			if ((observation_failures >= max_observation_failures)); then
				echo "run-real-work: could not observe $lane run=$run_id after $observation_failures consecutive attempts" >&2
				return 1
			fi
			echo "run-real-work: transient observation failure $observation_failures/$max_observation_failures for $lane run=$run_id; retrying" >&2
			if [[ "$gated" == true ]]; then
				gated=false
				echo "run-real-work: supervision deadline resumed for $lane run=$run_id; ${spent}s of ${timeout_seconds}s executing time used" >&2
			fi
			sleep "$interval_seconds"
			continue
		fi
		observation_failures=0
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
		if real_work_waits_on_human "$lane" "$state"; then
			waiting=true
		else
			waiting=false
		fi
		if [[ "$waiting" != "$gated" ]]; then
			gated=$waiting
			if [[ "$gated" == true ]]; then
				echo "run-real-work: supervision deadline paused at $lane run=$run_id state=$state; ${spent}s of ${timeout_seconds}s executing time used" >&2
			else
				echo "run-real-work: supervision deadline resumed for $lane run=$run_id; ${spent}s of ${timeout_seconds}s executing time used" >&2
			fi
		fi
		case "$lane:$state" in
		specification:implementation_bound)
			# CLI mode supplies the implementation run up front. Client-target
			# mode leaves it empty and resolves it from the selected task's run
			# list once the run is recorded, matching the require_live_rig hook
			# convention. An empty result is "not bound yet": keep following the
			# specification lane. A resolver failure is terminal.
			if [[ -z "$implementation_run_id" ]]; then
				if ! declare -F real_work_resolve_implementation >/dev/null; then
					echo "run-real-work: implementation run unresolved and no resolver is defined" >&2
					return 1
				fi
				if ! resolved=$(real_work_resolve_implementation); then
					return 1
				fi
				if [[ -z "$resolved" ]]; then
					sleep "$interval_seconds"
					continue
				fi
				implementation_run_id=$resolved
			fi
			lane=implementation
			run_id=$implementation_run_id
			previous_state=""
			continue
			;;
		specification:waiting_for_specification_approval|specification:pending|specification:unobserved|implementation:pending|implementation:unobserved|implementation:publication_ready)
			;;
		implementation:published|implementation:completed)
			return 0
			;;
		implementation:attention_required)
			if [[ "$state_changed" == true ]]; then
				echo "run-real-work: attention required for implementation run=$run_id; daemon reconciliation remains active" >&2
				printf 'observe exact run: freesided resume -db %q -run %q\n' "$db_path" "$run_id" >&2
			fi
			;;
		specification:attention_required|specification:completed|specification:failed|specification:lost|specification:blocked|implementation:failed|implementation:lost|implementation:blocked)
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
	echo "run-real-work: timed out supervising $lane run=$run_id after ${spent}s executing time" >&2
	return 124
}
