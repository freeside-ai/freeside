#!/usr/bin/env bash
# Shared bounded rig cleanup. The supervisor reserves its process group until
# cancellation finishes, including when a runtime child outlives freesided.
#
# Two rules defend against a macOS process-group SIGKILL that can miss a group
# member being forked at that instant. First, the supervisor forks its
# TERM-ignoring reservation before it launches the rig command; after writing
# the result it blocks in `wait` rather than forking a new sleep, so on the
# normal path no fresh `sleep` can be orphaned by the final kill and left
# holding the caller's output for an hour (the only later fork is a teardown
# fallback reached as the group is already dying). Second, the post-kill drain
# re-sends the group KILL until
# no member other than the (still unreaped) leader survives; a member that
# escaped the first signal is caught here. The whole subshell's stdout and
# stderr go to the cleanup log so no reservation output reaches the caller's
# capture. See devlog/2026-09-11-*-cleanup-supervisor-drain.md.

# Live process-group members other than the leader. Split out so a fixture can
# override it. Zombies (Z state) are already dead and do not count. Returns
# nonzero if the process table cannot be read, so the caller fails closed
# instead of mistaking an inspection failure for an empty group.
real_work_group_members() {
	local pgid=$1 snapshot
	snapshot=$(ps -A -o pid=,pgid=,stat= 2>/dev/null) || return 2
	printf '%s\n' "$snapshot" | awk -v pgid="$pgid" '
		$2 == pgid && $1 != pgid && substr($3, 1, 1) != "Z" { print $1 }'
}

real_work_bounded_rig() {
	local session=$1 bound=$2
	shift 2
	local supervisor result command_pid reservation_pid
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
		# Reserve the group before anything else and never fork after the
		# result is written. The reservation ignores TERM inside its own
		# subshell, so the rig command still receives TERM normally.
		( trap '' TERM; while :; do sleep 3600; done ) &
		reservation_pid=$!
		"$rig_daemon" rig "$@" &
		command_pid=$!
		trap '' TERM
		wait "$command_pid"
		printf '%s\n' "$?" >"$result_file"
		# Block on the reservation instead of forking a new sleep. The fallback
		# loop only runs if that wait returns, so the supervisor still never
		# exits on its own.
		wait "$reservation_pid"
		while :; do sleep 3600; done
	) >>"$session/rig-cleanup.log" 2>&1 &
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
	# Drain the group: re-KILL until an empty snapshot taken after a KILL and a
	# settle, for at most about 1s. Always KILL and settle before concluding
	# empty, because a fork that escaped the first signal (the macOS race this
	# guards) may not be visible to a snapshot taken the instant the kill
	# returns. Fail closed if the group still has members, or if the process
	# table cannot be read at all.
	#
	# The group id equals the leader pid. Bash reaps the killed leader
	# asynchronously, before the wait below, so in principle that pid could be
	# recycled as an unrelated group's leader mid-drain and be signalled here.
	# Reaching that needs the OS to cycle its entire pid space within this ~1s
	# window, which does not happen at the scale this runs (an operator shell
	# or CI). The caller-visible hang this function fixes is already closed by
	# the log redirect above; the drain is best-effort cleanup of stray kin.
	local members probe_ok=1 empty=""
	for _ in $(seq 1 10); do
		kill -KILL -- "-$supervisor" 2>/dev/null || true
		sleep 0.1
		if ! members=$(real_work_group_members "$supervisor"); then
			probe_ok=""
			break
		fi
		[[ -n "$members" ]] || {
			empty=1
			break
		}
	done
	if [[ -z "$probe_ok" ]]; then
		echo "real-work: could not inspect process group $supervisor after cancellation" >&2
		[[ "$result" != 0 ]] || result=1
	elif [[ -z "$empty" ]]; then
		echo "real-work: process group $supervisor still has members after cancellation" >&2
		[[ "$result" != 0 ]] || result=1
	fi
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
