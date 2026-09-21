#!/usr/bin/env bash
# Shared bounded rig cleanup. The supervisor reserves its process group until
# cancellation finishes, including when a runtime child outlives freesided, and
# never outlives the caller that started it.
#
# Two rules defend against a macOS process-group SIGKILL that can miss a group
# member being forked at that instant. First, the supervisor forks its
# TERM-ignoring reservation before it launches the rig command; after writing
# the result it blocks in `wait` rather than forking a new sleep, so on the
# normal path no fresh `sleep` can be orphaned by the final kill and left
# holding the caller's output for an hour. Second, the post-kill drain re-sends
# the group KILL until no member other than the (still unreaped) leader
# survives; a member that escaped the first signal is caught here. The whole
# subshell's stdout and stderr go to the cleanup log so no reservation output
# reaches the caller's capture.
#
# The reservation is also the caller's deadman. It watches the calling process
# and a wall-clock limit (2*bound plus a margin); when the caller vanishes
# (SIGKILL, a tool timeout, a closed terminal) or the limit passes, it takes
# down the recorded process group itself, so an orphaned supervisor cannot live
# on with no bound. The group it takes down is recorded by the supervisor (which
# outlives the caller) and is always distinct from the caller's own group, so a
# self-kill never reaches the caller's rig holder. The parent's own group
# signals are guarded the same way: once the reservation may have taken the
# group down, the leader pid can be reaped and recycled, so a blind signal is
# withheld. See the devlog notes
# 2026-09-11-*-cleanup-supervisor-drain.md and
# 2026-09-21-*-cleanup-supervisor-deadman.md.

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

# Whether the watched caller has gone: fully absent (kill -0 fails) or a zombie
# (SIGKILLed but not yet reaped by a lazy parent; it still answers kill -0). A
# zombie counts as gone so the deadman fires promptly instead of waiting out
# the wall-clock limit. A live non-zombie state, or an unreadable process table
# while kill -0 still succeeds, counts as alive, so a transient ps failure never
# fires the deadman against a live caller (the limit stays the backstop).
real_work_caller_gone() {
	local pid=$1 stat
	kill -0 "$pid" 2>/dev/null || return 0
	read -r stat < <(ps -o stat= -p "$pid" 2>/dev/null) || return 1
	[[ "${stat:0:1}" == "Z" ]]
}

# Record the reservation's explicit arming decision. The group is resolved from
# a live member of it (`group_probe_pid`, a supervisor child) and armed only
# when distinct from the caller's own group (`caller_pgid`), so a reservation
# self-kill can never reach the caller's process group, where the rig holder
# runs. The supervisor writes this once from inside its own subshell, so a
# caller that dies right after the fork cannot leave the decision unwritten.
#
# The decision is always written, never left empty, so emptiness means "not yet
# decided" and the reservation waits for the decision rather than mistaking a
# slow write for disarming. The outcomes:
#
#   - armed: the supervisor's group id, when it is distinct from the caller's;
#     returns 0.
#   - disarmed (return 0): the group is shared with the caller (job control made
#     no new group), so a group signal would wrongly reach the caller's holder;
#     writing the token records the deliberate no-signal decision.
#   - unresolvable (return 1): the process table could not be read, so the group
#     is unknown. This fails closed: the token is written so no reader signals
#     an unknown group, and the nonzero return tells the supervisor to abort
#     before starting the rig rather than run it with a deadman that can never
#     fire, which would recreate the unbounded orphan.
real_work_disarmed_token=disarmed
real_work_selfkill_flag() {
	local group_probe_pid=$1 caller_pgid=$2 file=$3 supervisor_pgid
	supervisor_pgid=$(ps -o pgid= -p "$group_probe_pid" 2>/dev/null | tr -d ' ')
	if [[ -z "$supervisor_pgid" || -z "$caller_pgid" ]]; then
		printf '%s\n' "$real_work_disarmed_token" >"$file"
		return 1
	fi
	if [[ "$supervisor_pgid" != "$caller_pgid" ]]; then
		printf '%s\n' "$supervisor_pgid" >"$file"
	else
		printf '%s\n' "$real_work_disarmed_token" >"$file"
	fi
}

# Whether the supervisor's group id (its pid) is still safe to signal. It is,
# while the supervisor is an unreaped running job of this shell, or while the
# group still has live members. Once both are false the leader has been reaped
# and its pid can be recycled as an unrelated group's leader, so a group signal
# could reach that group; report it (return 1) so the caller sends nothing.
# Return 2 when the process table cannot be read, so the caller can fail closed.
real_work_supervisor_signalable() {
	local supervisor=$1 members
	jobs -pr 2>/dev/null | grep -qx -- "$supervisor" && return 0
	members=$(real_work_group_members "$supervisor") || return 2
	[[ -n "$members" ]]
}

# real_work_local_address maps a daemon listen address to the address a
# same-host client should dial. A daemon bound to a Tailscale address also
# serves 127.0.0.1 at the same port (#1449), and a host VPN can drop the host's
# traffic to its own Tailscale address, so a same-host health wait must use
# loopback. A loopback listen address is returned unchanged. The port is the
# text after the last colon; the harness always passes an IP literal.
real_work_local_address() {
	local listen_address=$1
	case "$listen_address" in
		127.*|"[::1]:"*) printf '%s' "$listen_address" ;;
		*) printf '127.0.0.1:%s' "${listen_address##*:}" ;;
	esac
}

real_work_bounded_rig() {
	local session=$1 bound=$2
	shift 2
	local supervisor result command_pid reservation_pid
	local caller_pid=$$
	# The reservation self-terminates its group after this limit if the caller
	# never cancels it (a stopped or vanished caller). 2*bound leaves the
	# cancellation its full budget; the margin is the safety slack. Both are
	# overridable for tests; the margin is a signed offset so a test can force
	# an early limit, and the poll trades caller-death latency against fork rate.
	local reservation_margin=${real_work_reservation_margin:-60}
	local reservation_poll=${real_work_reservation_poll:-5}
	# A unique per-invocation record. A later cleanup on the same session
	# (recovery after the caller was SIGKILLed) must not make this invocation's
	# still-armed deadman read the later cleanup's group and take that down
	# instead of its own; a shared fixed path would let recovery's rewrite
	# retarget the orphan. mktemp gives each invocation its own immutable file.
	local selfkill_file
	selfkill_file=$(mktemp "$session/rig-selfkill-pgid.XXXXXX") || return 1
	# The caller's own process group, resolved while the caller is certainly
	# alive (it is running this code). The supervisor compares against it before
	# arming the deadman, so the deadman can never target the caller's group.
	local caller_pgid
	caller_pgid=$(ps -o pgid= -p "$caller_pid" 2>/dev/null | tr -d ' ')
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
		# subshell, so the rig command still receives TERM normally. It doubles
		# as the caller's deadman: it takes down the recorded group once the
		# caller is gone or the wall-clock limit passes, so no orphan outlives
		# the caller. SECONDS tracks wall-clock even while stopped, so a paused
		# caller still hits the limit.
		(
			trap '' TERM
			SECONDS=0
			reservation_limit=$((2 * bound + reservation_margin))
			while :; do
				real_work_caller_gone "$caller_pid" && break
				((SECONDS < reservation_limit)) || break
				sleep "$reservation_poll"
			done
			# Wait for the supervisor's explicit arming decision, which it writes
			# once just after this fork. On a slow host the deadman can reach here
			# before that write, so wait for the decision instead of reading
			# emptiness as disarmed; the same wall-clock limit bounds the wait so
			# a supervisor that died before deciding cannot hang the reservation.
			# The decision is a group id to kill, or the disarmed token (a shared
			# or unresolvable group), where a group signal would wrongly reach the
			# caller's own holder.
			while [[ ! -s "$selfkill_file" ]]; do
				((SECONDS < reservation_limit)) || break
				sleep 0.1
			done
			if [[ -s "$selfkill_file" ]]; then
				read -r reservation_pgid <"$selfkill_file" || reservation_pgid=""
				[[ -z "$reservation_pgid" || "$reservation_pgid" == "$real_work_disarmed_token" ]] ||
					kill -KILL -- "-$reservation_pgid" 2>/dev/null || true
			fi
		) &
		reservation_pid=$!
		# Arm the deadman from inside the supervisor, which stays alive even
		# if the caller dies now, so a caller killed right after the fork
		# cannot leave it disarmed. The reservation, a live group member,
		# resolves the group; record it only when distinct from the caller's.
		# If arming cannot resolve the group (unreadable process table) it
		# fails closed: skip the rig and report an error, rather than run
		# cleanup behind a deadman that can never fire and leave an orphan.
		if real_work_selfkill_flag "$reservation_pid" "$caller_pgid" "$selfkill_file"; then
			"$rig_daemon" rig "$@" &
			command_pid=$!
			trap '' TERM
			wait "$command_pid"
			printf '%s\n' "$?" >"$result_file"
		else
			echo "real-work: could not arm the cleanup deadman; aborting before the rig" >&2
			printf '%s\n' 1 >"$result_file"
		fi
		# Block on the reservation instead of forking a new sleep. The
		# reservation now ends on its own (caller gone or the time limit), so
		# the supervisor exits when it returns rather than looping forever.
		wait "$reservation_pid"
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
		if real_work_supervisor_signalable "$supervisor"; then
			kill -TERM -- "-$supervisor" 2>/dev/null || true
		fi
		for _ in $(seq 1 $((bound * 10))); do
			[[ ! -s "$result_file" ]] || break
			sleep 0.1
		done
	fi
	# Drain the group: re-KILL until an empty snapshot taken after a KILL and a
	# settle, for at most about 1s. Always KILL and settle before concluding
	# empty, because a fork that escaped the first signal (the macOS race this
	# guards) may not be visible to a snapshot taken the instant the kill
	# returns. Fail closed if the group still has members, or if the process
	# table cannot be read at all.
	#
	# Guard every KILL with real_work_supervisor_signalable. The reservation can
	# now take the group down on its own before this point (a stopped or
	# vanished caller), after which bash reaps the leader and its pid (the group
	# id) can be recycled as an unrelated group's leader. A blind KILL would
	# then reach that group, so the guard withholds it once the group is provably
	# gone, and treats that as a drained group rather than a failure.
	local members probe_ok=1 empty="" signalable
	for _ in $(seq 1 10); do
		# Capture in an `if` so a nonzero verdict never trips a caller's set -e.
		if real_work_supervisor_signalable "$supervisor"; then signalable=0; else signalable=$?; fi
		if [[ "$signalable" == 2 ]]; then
			probe_ok=""
			break
		fi
		if [[ "$signalable" != 0 ]]; then
			empty=1
			break
		fi
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
	rm -f "$result_file" "$selfkill_file"
	return "$result"
}

# The caller holds the rig and owns the EXIT/INT/TERM cleanup traps. Keep the
# PID in its shell so normal completion and interruption stop the same daemon.
real_work_recover_codex_credentials() {
	local workdir=$1 db_path=$2 listen_address=$3
	shift 3
	if ! FREESIDE_REAL_RUN_SCHEMA_TEST=1 \
		FREESIDE_REAL_RUN_STATE_ROOT="$FREESIDE_REAL_RUN_STATE_ROOT" \
		"$workdir/verify-real-run" -test.run '^TestRealRunRetainedSchema$' -test.count=1 \
		>"$workdir/verify-schema.log" 2>&1; then
		echo 'Credential recovery requires a schema-compatible daemon; use the supported runtime upgrade before retrying. No recovery daemon was started.' >&2
		return 1
	fi
	"$workdir/freesided" -listen "$listen_address" -db "$db_path" \
		-state-dir "$FREESIDE_REAL_RUN_STATE_ROOT" -driver disabled \
		-approved-recipe "$FREESIDE_REAL_RUN_APPROVED_RECIPE" "$@" >>"$workdir/daemon.log" 2>&1 &
	daemon_pid=$!
	local health_address
	health_address=$(real_work_local_address "$listen_address")
	local healthy=false
	for _ in $(seq 1 60); do
		child_job_exists "$daemon_pid" && require_live_rig || return 1
		if curl --fail --silent --max-time 2 "http://$health_address/health" >/dev/null; then
			healthy=true
			break
		fi
		sleep 1
	done
	[[ "$healthy" == true ]] || { echo 'Credential recovery daemon did not become healthy' >&2; return 1; }
	printf 'Credential recovery endpoint: http://%s (execution disabled)\n' "$listen_address" >&2
	printf 'Pairing code: %q pairing-code -state-dir %q\n' "$workdir/freesided" "$FREESIDE_REAL_RUN_STATE_ROOT" >&2
	echo 'Inspect the re-enrollment hold in a paired client and choose Resolve re-enrollment.' >&2
	printf 'Complete explicitly: bash %q complete %q\n' "$workdir/real-work-session.sh" "$workdir" >&2
	real_work_walkthrough "$workdir" "$daemon_pid"
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
