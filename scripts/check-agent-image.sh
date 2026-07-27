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
#   scripts/check-agent-image.sh <image-reference>
set -euo pipefail

# Duplicated from daemon/internal/ward/conformance.go (fixedContainerPathEnv).
# The gate's literal is the contract; this check exists to fail before it does.
fixed_path_env="PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

if [ "$#" -ne 1 ] || [ -z "${1:-}" ]; then
	echo "check-agent-image: usage: scripts/check-agent-image.sh <image-reference>" >&2
	exit 2
fi
case "$1" in
-h | --help)
	grep '^#' "$0" | sed 's/^# \{0,1\}//'
	exit 0
	;;
esac
image_ref="$1"

command -v jq >/dev/null 2>&1 || {
	echo "check-agent-image: jq is required to compare the inspected configuration" >&2
	exit 1
}

probe_container=""

cleanup() {
	status=$?
	trap - EXIT
	if [ -n "$probe_container" ]; then
		container delete --force "$probe_container" >/dev/null 2>&1 ||
			echo "check-agent-image: could not remove probe container ${probe_container}" >&2
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

candidate="freeside-agent-image-check-$(date +%s)-$$"
if container inspect "$candidate" >/dev/null 2>&1; then
	echo "check-agent-image: probe container name already exists: $candidate" >&2
	exit 1
fi
# Arm cleanup before create so a partially created container is still owned here.
probe_container="$candidate"

# The container is created, never started: the allowlist is verified on a
# stopped container, exactly as the gate does it.
echo "check-agent-image: creating a probe container from ${image_ref}" >&2
container create --name "$probe_container" -- "$image_ref" sh -c true >&2

report=$(container inspect "$probe_container")

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
$(printf '%s' "$report" | jq -r '
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
