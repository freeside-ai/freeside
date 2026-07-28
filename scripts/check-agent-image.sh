#!/usr/bin/env bash
# Check an agent image against the ward's post-create allowlist (issue #304).
#
# Ward's check 2 (control_plane_isolation, daemon/internal/ward/conformance.go
# verifyAgentAllowlist) compares the created container's inspected configuration
# against exactly the fixed PATH plus the daemon-supplied environment, a working
# directory of "/", and the daemon-supplied command. Apple `container` 1.1.0
# merges the *image's* environment and working directory into that report
# (measured: an image ENV appears in initProcess.environment and an image
# WORKDIR becomes initProcess.workingDirectory), so an image that carries either
# fails the gate at run time, in the daemon, far from the Containerfile that
# caused it.
#
# This script measures the image's own contribution: it creates a container with
# a fixed command, no supplied environment and no mounts, so anything observed
# beyond the fixed PATH, the root working directory and that command came from
# the image. It supplies no environment on purpose; the runtime reports supplied
# variables in a nondeterministic order, which is a ward-side concern and would
# make this check flaky without testing the image. An image-declared VOLUME is
# caught the same way: the gate compares the mount topology mount-for-mount, so
# an anonymous mount fails it at run time.
#
# Run it before pushing an agent image, and again against the pushed digest
# reference. Exit 0 means the image satisfies the allowlist's image-side
# preconditions; exit 1 means it does not; exit 2 is a usage error.
#
# Usage:
#   scripts/check-agent-image.sh <image-reference> [container-executable]
set -euo pipefail

# Duplicated from daemon/internal/ward/conformance.go (fixedContainerPathEnv).
# The gate's literal is the contract; this check exists to fail before it does.
fixed_path_env="PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ] || [ -z "${1:-}" ] || [ -z "${2-container}" ]; then
	echo "check-agent-image: usage: scripts/check-agent-image.sh <image-reference> [container-executable]" >&2
	exit 2
fi
case "$1" in
-h | --help)
	# Only the leading comment block is help; later comments are internal.
	awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
	exit 0
	;;
esac
image_ref="$1"
container_bin="${2:-container}"

command -v jq >/dev/null 2>&1 || {
	echo "check-agent-image: jq is required to compare the inspected configuration" >&2
	exit 1
}

probe_container=""
cidfile=""
runtime_json=""
create_attempted=0
ownership_label="ai.freeside.project-image.owner"
# The token gates a force-delete, so it must be unguessable: a predictable
# value would let a local process plant a container that passes the ownership
# check and steer the delete (mirrors the daemon's crypto/rand randomToken).
ownership_token="checker-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
if [ "${#ownership_token}" -ne 40 ]; then
	echo "check-agent-image: could not generate an ownership token" >&2
	exit 1
fi

valid_container_id() {
	case "$1" in
	"" | [!A-Za-z0-9]* | *[!A-Za-z0-9._-]*) return 1 ;;
	*) return 0 ;;
	esac
}

# Mirrors ward.RejectDuplicateJSONKeys (daemon/internal/ward/runtime_cli.go):
# jq's parser is silently last-value-wins on duplicate object keys, so a
# report with a duplicated initProcess, environment, or labels key could pass
# the field comparison below while the daemon's gate rejects it, and could
# steer the ownership checks that gate `delete --force` toward a container
# this run does not own. `jq --stream` emits parse events before that
# collapse, so this structural pass sees both occurrences: a leaf event
# completes its path, a close event completes its parent, and any event whose
# path (or a prefix of it) is already completed is a duplicate key or a
# second top-level value. Keys are folded with ascii_downcase to approximate
# the daemon's Unicode simple fold (foldJSONKey; its Go decoder matches
# struct fields case-insensitively); runtime JSON keys are ASCII, so the
# narrower fold covers the same input space. Empty input is rejected like the
# daemon's immediate EOF, and malformed or trailing input fails jq itself.
# Takes the evidence as a FILE, never a shell variable: command substitution
# silently strips raw NUL bytes, which can transform invalid runtime JSON
# into a valid-looking document before any validator sees it, so runtime
# output must land in a file and be validated from that file.
# Returns 0 only for one unambiguous top-level value.
reject_duplicate_json_keys() {
	jq -n --stream -e <"$1" '
		def fold: map(if type == "string" then ascii_downcase else . end);
		reduce inputs as $e (
			{done: {}, dup: false, n: 0};
			if .dup then .
			else
				.n += 1
				| ($e[0] | fold) as $p
				| . as $st
				| if ($e | length) == 2 then
					if any(range(0; ($p | length) + 1);
						$st.done[($p[0:.] | tojson)] != null)
					then .dup = true
					else .done[($p | tojson)] = true
					end
				else
					($p[0:(($p | length) - 1)]) as $q |
					if any(range(0; ($q | length) + 1);
						$st.done[($q[0:.] | tojson)] != null)
					then .dup = true
					else .done[($q | tojson)] = true
					end
				end
			end
		) | (.dup | not) and .n > 0' >/dev/null 2>&1
}

# Bounded runtime capture, mirroring the daemon's maxRuntimeOutput cap: a
# wedged runtime must not fill the TMPDIR filesystem through an uncapped
# redirect. head stops reading at the cap plus one byte (a wedged writer
# then takes SIGPIPE), so overflow is detectable by size. Every pipeline
# status is checked, not just the writer's: a full filesystem, quota, or
# file-size limit fails the sink after a truncated prefix that can look
# like complete evidence, while the writer still exits 0. Overflow, a
# failed command, or a failed sink fails the capture, and the caller
# treats each like any other runtime failure.
runtime_json_cap=16777216
capture_runtime_json() { # <file> <command...>
	capture_file=$1
	shift
	"$@" | head -c "$((runtime_json_cap + 1))" >"$capture_file"
	capture_status=("${PIPESTATUS[@]}")
	capture_size=$(wc -c <"$capture_file")
	if [ "${capture_status[0]}" -ne 0 ] || [ "${capture_status[1]}" -ne 0 ] ||
		[ "$capture_size" -gt "$runtime_json_cap" ]; then
		return 1
	fi
}

# The single definition of the ownership predicate: reads the inspection
# in $runtime_json and prints the id only when it is exactly one entry
# whose id, configuration id, and ownership-token label all match. Every
# `delete --force` in this script is gated on this predicate.
inspected_owned_id() { # <expected-id>
	jq -er <"$runtime_json" --arg id "$1" \
		--arg label "$ownership_label" \
		--arg token "$ownership_token" '
		if length == 1
			and .[0].id == $id
			and .[0].configuration.id == $id
			and .[0].configuration.labels[$label] == $token
		then .[0].id
		else empty
		end' 2>/dev/null
}

# Mirrors deleteOwnedContainer (daemon/internal/projectimage/apple.go): the
# ownership evidence that selected a recovery candidate is re-verified by a
# fresh inspection immediately before the destructive act, shrinking the
# window in which a replacement under the same runtime ID could be deleted
# on stale evidence.
recovery_delete_owned() { # <id>
	if ! capture_runtime_json "$runtime_json" \
		"$container_bin" inspect "$1" </dev/null 2>/dev/null; then
		echo "check-agent-image: could not re-inspect container $1 before recovery deletion" >&2
		return 1
	fi
	if ! reject_duplicate_json_keys "$runtime_json"; then
		echo "check-agent-image: refusing recovery deletion of container $1: ambiguous runtime JSON" >&2
		return 1
	fi
	confirmed_id=$(inspected_owned_id "$1") || confirmed_id=""
	if [ "$confirmed_id" != "$1" ]; then
		echo "check-agent-image: refusing recovery deletion of container $1: ownership no longer verified" >&2
		return 1
	fi
	if ! "$container_bin" delete --force "$1" </dev/null >/dev/null 2>&1; then
		echo "check-agent-image: could not remove recovered probe container $1" >&2
		return 1
	fi
}

# Mirrors appleBackend.recoverOwnedContainer (daemon/internal/projectimage/
# apple.go): when `container create` registers the probe but terminates
# before producing the cidfile, the ownership token is the only remaining
# binding to the orphan. Apple container's list output omits labels, so
# ownership is read per candidate from its own inspection, and deletion
# re-gates through recovery_delete_owned's fresh inspection; anything
# unparseable refuses deletion rather than guessing. Returns nonzero when
# any step failed.
recover_owned_containers() {
	recovery_failed=0
	if [ -z "$runtime_json" ] ||
		! capture_runtime_json "$runtime_json" \
			"$container_bin" list --all --format json 2>/dev/null; then
		echo "check-agent-image: could not list containers for ownership recovery" >&2
		return 1
	fi
	if ! reject_duplicate_json_keys "$runtime_json"; then
		echo "check-agent-image: refusing ownership recovery on ambiguous container listing" >&2
		return 1
	fi
	# A non-array listing (the daemon's decodeStrictJSON rejects those at the
	# type level) fails jq and refuses recovery outright; non-object entries,
	# non-string identities, and identities carrying an escaped NUL (which a
	# shell variable cannot hold losslessly) project to empty fields, which
	# the per-line identity checks below flag.
	candidates=$(jq -r <"$runtime_json" '
		def usable: type == "string" and (contains("\u0000") | not);
		(if type == "array" then . else error("not an array") end)
		| .[]
		| (if type == "object" then . else {} end)
		| [
			(.id | if usable then . else "" end),
			(.configuration
				| if type == "object" then .id else null end
				| if usable then . else "" end)
		]
		| @tsv' 2>/dev/null) || {
		echo "check-agent-image: container listing returned an unexpected object" >&2
		return 1
	}
	[ -n "$candidates" ] || return 0
	# The runtime calls inside the loop read /dev/null explicitly: the loop's
	# stdin is the candidate list, and a runtime that drains stdin would
	# otherwise swallow the remaining candidates.
	while IFS=$'\t' read -r candidate_id config_id; do
		if ! valid_container_id "$candidate_id" || [ "$candidate_id" != "$config_id" ]; then
			echo "check-agent-image: ownership recovery listed an invalid container identity" >&2
			recovery_failed=1
			continue
		fi
		if ! capture_runtime_json "$runtime_json" \
			"$container_bin" inspect "$candidate_id" </dev/null 2>/dev/null; then
			echo "check-agent-image: could not inspect container ${candidate_id} for ownership recovery" >&2
			recovery_failed=1
			continue
		fi
		if ! reject_duplicate_json_keys "$runtime_json"; then
			echo "check-agent-image: refusing to judge ownership of container ${candidate_id} from ambiguous runtime JSON" >&2
			recovery_failed=1
			continue
		fi
		owned_id=$(inspected_owned_id "$candidate_id") || owned_id=""
		if [ "$owned_id" = "$candidate_id" ]; then
			if recovery_delete_owned "$candidate_id"; then
				echo "check-agent-image: recovered orphaned probe container ${candidate_id}" >&2
			else
				recovery_failed=1
			fi
		fi
	done <<EOF
$candidates
EOF
	return "$recovery_failed"
}

cleanup() {
	status=$?
	trap - EXIT
	cleanup_failed=0
	if [ -z "$probe_container" ] && [ -n "$cidfile" ] && [ -f "$cidfile" ]; then
		# Bounded like the JSON captures; any identity past the cap is
		# invalid anyway, and a truncated one fails the inspection gate.
		recovered_id=$(head -c 4096 "$cidfile" 2>/dev/null | tr -d '\r\n') || recovered_id=""
		if valid_container_id "$recovered_id"; then
			probe_container="$recovered_id"
		fi
	fi
	if [ -n "$probe_container" ]; then
		if [ -n "$runtime_json" ] &&
			capture_runtime_json "$runtime_json" \
				"$container_bin" inspect "$probe_container" 2>/dev/null; then
			if ! reject_duplicate_json_keys "$runtime_json"; then
				echo "check-agent-image: refusing to remove probe container ${probe_container}: ambiguous runtime JSON" >&2
				cleanup_failed=1
			else
				owned_id=$(inspected_owned_id "$probe_container") || owned_id=""
				if [ "$owned_id" = "$probe_container" ]; then
					if ! "$container_bin" delete --force "$owned_id" >/dev/null 2>&1; then
						echo "check-agent-image: could not remove probe container ${owned_id}" >&2
						cleanup_failed=1
					fi
				else
					echo "check-agent-image: refusing to remove unowned probe container ${probe_container}" >&2
					cleanup_failed=1
				fi
			fi
		else
			echo "check-agent-image: could not inspect probe container ${probe_container} for cleanup" >&2
			cleanup_failed=1
		fi
	elif [ "$create_attempted" -ne 0 ]; then
		if ! recover_owned_containers; then
			cleanup_failed=1
		fi
	fi
	if [ -n "$cidfile" ] && ! rm -f "$cidfile"; then
		echo "check-agent-image: could not remove probe identity file" >&2
		cleanup_failed=1
	fi
	if [ -n "$runtime_json" ] && ! rm -f "$runtime_json"; then
		echo "check-agent-image: could not remove runtime evidence file" >&2
		cleanup_failed=1
	fi
	if [ "$cleanup_failed" -ne 0 ] && [ "$status" -eq 0 ]; then
		status=1
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

cidfile=$(mktemp "${TMPDIR:-/tmp}/freeside-image-checker-id.XXXXXX")
rm -f "$cidfile"
# Runtime JSON evidence lands here, never in a shell variable: command
# substitution strips raw NUL bytes, which would let malformed runtime
# output masquerade as valid JSON (see reject_duplicate_json_keys).
runtime_json=$(mktemp "${TMPDIR:-/tmp}/freeside-image-checker-json.XXXXXX")

# The container is created, never started: the allowlist is verified on a
# stopped container, exactly as the gate does it.
echo "check-agent-image: creating a probe container from ${image_ref}" >&2
create_attempted=1
"$container_bin" create --cidfile "$cidfile" \
	--label "${ownership_label}=${ownership_token}" \
	-- "$image_ref" sh -c true >&2
runtime_id=$(head -c 4096 "$cidfile" | tr -d '\r\n')
if ! valid_container_id "$runtime_id"; then
	echo "check-agent-image: runtime returned invalid probe container ID" >&2
	exit 1
fi
probe_container="$runtime_id"

if ! capture_runtime_json "$runtime_json" "$container_bin" inspect "$probe_container"; then
	echo "check-agent-image: could not capture the probe inspection (runtime failure or output past the ${runtime_json_cap}-byte cap)" >&2
	exit 1
fi
if ! reject_duplicate_json_keys "$runtime_json"; then
	echo "check-agent-image: runtime inspection JSON is ambiguous (duplicate keys, empty, or malformed); not comparing fields" >&2
	exit 1
fi

failures=0
fail() {
	echo "check-agent-image: $1" >&2
	failures=$((failures + 1))
}

# The projection mirrors cliContainer.allowlistFieldsPresent()
# (daemon/internal/ward/runtime_cli.go) field for field: the gate refuses an
# inspection that omits any of them (InspectReport.AllowlistFieldsObserved), so
# every one must read as "absent" here rather than as a clean value when the
# runtime's report drifts. A field the gate requires but this check does not
# compare (networks, the image reference) is still observed for presence: the
# gate rejects the image on the absence alone, whatever its content says.
#
# Presence is `!= null`, not `has`: the gate decodes these into pointers, so an
# explicit JSON null is an absent field there and must be one here too.
IFS=$'\t' read -r image_reference env_count env_first working_dir command_line \
	ssh publications networks mount_count <<EOF
$(jq -r <"$runtime_json" '
	.[0].configuration as $c |
	$c.initProcess as $p |
	[
		(if ($c.image != null) and ($c.image.reference // "") != "" then $c.image.reference else "absent" end),
		(if ($p.environment != null) then ($p.environment | length | tostring) else "absent" end),
		(if ($p.environment != null) then ($p.environment[0] // "") else "absent" end),
		(if ($p.workingDirectory != null) then $p.workingDirectory else "absent" end),
		(if ($p.executable // "") != "" and ($p.arguments != null)
			then ([$p.executable] + $p.arguments | join(" ")) else "absent" end),
		(if ($c.ssh != null) then ($c.ssh | tostring) else "absent" end),
		(if ($c.publishedPorts != null) and ($c.publishedSockets != null)
			then (($c.publishedPorts | length) + ($c.publishedSockets | length) | tostring) else "absent" end),
		(if ($c.networks != null) then "present" else "absent" end),
		(if ($c.mounts != null) then ($c.mounts | length | tostring) else "0" end)
	] | @tsv')
EOF

for observed in "image reference=$image_reference" "environment=$env_count" \
	"environment=$env_first" "working directory=$working_dir" \
	"command=$command_line" "ssh=$ssh" "publications=$publications" \
	"networks=$networks"; do
	case "$observed" in
	*=absent) fail "inspection omitted the ${observed%%=*} the gate requires" ;;
	esac
done

# The probe supplies no environment, so one entry, the fixed PATH, is the only
# compliant observation; anything else was contributed by the image.
if [ "$env_count" != "1" ] || [ "$env_first" != "$fixed_path_env" ]; then
	fail "image contributes environment beyond the fixed PATH: ${env_count} entry/entries, first '${env_first}'"
fi

if [ "$working_dir" != "/" ]; then
	fail "working directory is '${working_dir}', not the fixed image root '/'"
fi

if [ "$command_line" != "sh -c true" ]; then
	fail "command is '${command_line}', not the supplied command"
fi

# The probe supplies no mounts, so an observed one is an image-declared VOLUME.
# The gate compares the topology mount-for-mount against the spec, so such an
# image is rejected at run time however clean its environment is. Mounts are
# absent from the gate's presence list on purpose: an unreported mounts key
# decodes there as an empty topology, which is a compliant observation, so it
# is read as zero above rather than as a missing field.
if [ "$mount_count" != "0" ]; then
	fail "image declares ${mount_count} mount(s); the gate compares the mount topology exactly"
fi

# Neither is image-settable today, but the gate reads both, and this check is
# meant to fail before the daemon does rather than to model why it cannot happen.
if [ "$ssh" != "false" ]; then
	fail "container reports SSH forwarding configured"
fi
if [ "$publications" != "0" ]; then
	fail "container reports ${publications} published port(s) or socket(s)"
fi

if [ "$failures" -ne 0 ]; then
	echo "check-agent-image: ${image_ref} would fail the ward allowlist (${failures} problem(s))" >&2
	exit 1
fi

echo "check-agent-image: ${image_ref} satisfies the ward allowlist's image-side preconditions" >&2
