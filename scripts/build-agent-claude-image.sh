#!/usr/bin/env bash
# Build the Freeside Claude agent base image (issue #304).
#
# Assembles images/agent-claude/Containerfile: a pinned Debian slim base, a
# pinned Node, and the pinned Claude CLI version plan §5.7 requires a golden
# image to carry. Prints a registry-resolvable name@sha256:<digest> reference
# for the agent stage's ImageRef.
#
# The pin is asserted, not assumed: after the build, the script runs
# `claude --version` inside the built image with networking disabled and fails
# unless it reports the pinned version. A label alone would only record what the
# build intended.
#
# The ward allowlist check is a separate step, scripts/check-agent-image.sh, and
# applies to every agent image: this base, and any image later built on it (a
# project image onboarding derives from it is an agent image too).
#
# Resolving the digest: an agent image must be digest-pinned, but Apple
# `container` 1.1.0 does not resolve a locally-built name@digest reference from
# its content store. --local-registry-port works around that lookup bug: it
# briefly runs a pinned registry:2 on loopback, pushes over explicit HTTP, pulls
# the exact digest reference into the local store, and removes the registry
# before printing the usable reference.
#
# The temporary-registry lifecycle here is deliberately duplicated from
# scripts/build-exporter-image.sh rather than shared: that script is live-verified
# and out of this unit's scope. Keep the two in sync; see the follow-up issue for
# extracting a shared helper once a third caller exists.
#
# Apple container's build metadata can vary between invocations, so "reproducible"
# means pinned inputs plus a recorded digest, not a bit-identical image: the
# script captures and verifies the exact digest produced by this invocation.
#
# With HTTPS_PROXY set, the build forwards it (and HTTP_PROXY, defaulting to
# HTTPS_PROXY) into RUN steps as the predefined proxy build args; unset, the
# invocation is unchanged. See images/README.md (Building Behind a VPN) for the
# host-proxy recipe a guest-NAT-blocking VPN requires.
#
# Usage:
#   scripts/build-agent-claude-image.sh [--tag NAME] [--claude-version VERSION]
#       {--registry HOST[/PATH] | --local-registry-port PORT} [--ref-tag TAG]
#
#   --claude-version VERSION
#                       override the Claude CLI version pinned in the
#                       Containerfile; the assertion follows the override.
#   --dns IP            nameserver for the build, repeatable. The build fetches
#                       from Debian's archive, nodejs.org and the npm registry;
#                       on a host whose container gateway does not answer DNS
#                       (observed with a VPN client installed), the builder
#                       resolves nothing without this.
#   --registry HOST     tag as HOST/<name>:<ref-tag>, push via `container`, print
#                       the pushed digest reference (HOST/<name>@sha256:...).
#   --local-registry-port PORT
#                       seed the exact digest reference through a temporary
#                       127.0.0.1 registry, then remove the registry (PORT must
#                       be 1024-65535).
#   --ref-tag TAG       tag used for the push reference (default: v1).
#
# One of --registry or --local-registry-port is required. A local-only digest
# is not runnable by ward on the supported Apple container runtime.
set -euo pipefail

image_name=freeside-agent-claude
registry=""
local_registry_port=""
ref_tag=v1
claude_version=""
dns_args=()

require_value() {
	if [ "$#" -lt 2 ] || [ -z "$2" ]; then
		echo "build-agent-claude-image: $1 requires a value" >&2
		exit 2
	fi
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--tag)
		require_value "$@"
		image_name="$2"
		shift 2
		;;
	--claude-version)
		require_value "$@"
		claude_version="$2"
		shift 2
		;;
	--dns)
		require_value "$@"
		dns_args+=(--dns "$2")
		shift 2
		;;
	--registry)
		require_value "$@"
		registry="$2"
		shift 2
		;;
	--local-registry-port)
		require_value "$@"
		local_registry_port="$2"
		shift 2
		;;
	--ref-tag)
		require_value "$@"
		ref_tag="$2"
		shift 2
		;;
	-h | --help)
		grep '^#' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "build-agent-claude-image: unknown argument: $1" >&2
		exit 2
		;;
	esac
done

if [ -n "$registry" ] && [ -n "$local_registry_port" ]; then
	echo "build-agent-claude-image: --registry and --local-registry-port are mutually exclusive" >&2
	exit 2
fi
if [ -z "$registry" ] && [ -z "$local_registry_port" ]; then
	echo "build-agent-claude-image: one of --registry HOST[/PATH] or --local-registry-port PORT is required; local-only digests are not runnable by ward" >&2
	exit 2
fi
if [ -n "$local_registry_port" ]; then
	case "$local_registry_port" in
	*[!0-9]* | "")
		echo "build-agent-claude-image: local registry port must be an integer from 1024 to 65535" >&2
		exit 2
		;;
	esac
	if [ "$local_registry_port" -lt 1024 ] || [ "$local_registry_port" -gt 65535 ]; then
		echo "build-agent-claude-image: local registry port must be an integer from 1024 to 65535" >&2
		exit 2
	fi
fi

repo_root=$(cd "$(dirname "$0")/.." && pwd)
context="$repo_root/images/agent-claude"
registry_container=""

# The Containerfile is the single source of the pin; an override still has to
# name a version, so the assertion below always compares against something the
# caller chose.
if [ -z "$claude_version" ]; then
	claude_version=$(sed -n 's/^ARG CLAUDE_CODE_VERSION=\(.*\)$/\1/p' "$context/Containerfile" | head -n 1)
fi
if [ -z "$claude_version" ]; then
	echo "build-agent-claude-image: could not read the pinned Claude CLI version from the Containerfile" >&2
	exit 1
fi
# npm reads a partial version as a range, so `2.1` would install whichever
# concrete release matches today while the label and the assertion below both
# recorded `2.1`: a pin in name only. Only an exact version is a pin.
if ! printf '%s' "$claude_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$'; then
	echo "build-agent-claude-image: the Claude CLI version must be exact (x.y.z), not the range npm would resolve from '${claude_version}'" >&2
	exit 2
fi

remove_registry() {
	[ -n "$registry_container" ] || return 0
	if ! container delete --force "$registry_container" >/dev/null 2>&1; then
		echo "build-agent-claude-image: could not remove temporary registry ${registry_container}" >&2
		return 1
	fi
	registry_container=""
}

cleanup() {
	status=$?
	trap - EXIT
	if ! remove_registry && [ "$status" -eq 0 ]; then
		status=1
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

# digest_of prints the manifest-index digest of a locally-built image name.
digest_of() {
	container image inspect "$1" |
		grep -o '"digest"[[:space:]]*:[[:space:]]*"sha256:[0-9a-f]\{64\}"' |
		head -n 1 | grep -o 'sha256:[0-9a-f]\{64\}'
}

# Forward standard proxy environment into RUN steps as the predefined proxy
# build args: `container build` does not auto-forward its own environment, and
# behind a guest-NAT-blocking VPN the proxy is the only egress a RUN step has
# (see images/README.md, Building Behind a VPN).
proxy_args=()
if [ -n "${HTTPS_PROXY:-}" ]; then
	proxy_args+=(--build-arg "HTTPS_PROXY=$HTTPS_PROXY" \
		--build-arg "HTTP_PROXY=${HTTP_PROXY:-$HTTPS_PROXY}")
fi

echo "build-agent-claude-image: building with Apple container (Claude CLI ${claude_version})" >&2
container build ${dns_args[@]+"${dns_args[@]}"} ${proxy_args[@]+"${proxy_args[@]}"} \
	--build-arg "CLAUDE_CODE_VERSION=${claude_version}" \
	--tag "$image_name:local" "$context" >&2

# Assert the shipped CLI, with networking disabled so nothing can be fetched to
# satisfy the check. The image also disables the auto-updater through managed
# settings, so this version is what a run gets.
echo "build-agent-claude-image: asserting the shipped Claude CLI version" >&2
reported=$(container run --rm --network none "$image_name:local" claude --version)
# `claude --version` prints "<version> (Claude Code)"; compare the parsed
# version for equality rather than looking for the pin as a substring, which
# would accept 2.1.220 for a pin of 2.1.2.
reported_version=${reported%% *}
if [ "$reported_version" != "$claude_version" ]; then
	echo "build-agent-claude-image: built image reports '${reported}', expected the pinned ${claude_version}" >&2
	exit 1
fi
echo "build-agent-claude-image: image reports: ${reported}" >&2

digest=$(digest_of "$image_name:local")
[ -n "${digest:-}" ] || {
	echo "build-agent-claude-image: could not read the built image digest" >&2
	exit 1
}

scheme=auto
if [ -n "$local_registry_port" ]; then
	command -v curl >/dev/null 2>&1 || {
		echo "build-agent-claude-image: curl is required for local registry readiness" >&2
		exit 1
	}

	# The registry helper is pulled and executed by its reviewed multi-platform
	# index digest. A cached exact reference keeps this path usable offline.
	registry_image_digest=sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
	registry_image="docker.io/library/registry@${registry_image_digest}"
	actual_registry_digest=$(digest_of "$registry_image" 2>/dev/null || true)
	if [ "$actual_registry_digest" != "$registry_image_digest" ]; then
		echo "build-agent-claude-image: pulling the pinned registry helper" >&2
		container image pull --scheme https "$registry_image" >&2
		actual_registry_digest=$(digest_of "$registry_image")
	fi
	if [ "$actual_registry_digest" != "$registry_image_digest" ]; then
		echo "build-agent-claude-image: registry helper digest ${actual_registry_digest:-missing} does not match ${registry_image_digest}" >&2
		exit 1
	fi

	candidate_registry_container="freeside-agent-claude-registry-$(date +%s)-$$"
	if container inspect "$candidate_registry_container" >/dev/null 2>&1; then
		echo "build-agent-claude-image: temporary registry name already exists: $candidate_registry_container" >&2
		exit 1
	fi
	# Arm cleanup only after proving the generated name was unoccupied, but
	# before create so a partially bootstrapped container is still owned here.
	registry_container="$candidate_registry_container"
	registry="127.0.0.1:${local_registry_port}"
	scheme=http
	echo "build-agent-claude-image: starting pinned temporary registry at ${registry}" >&2
	container run --detach --name "$registry_container" \
		--label "freeside.agent-claude-seed=${registry_container}" \
		--publish "127.0.0.1:${local_registry_port}:5000" \
		"$registry_image" >&2

	ready=0
	for _ in $(seq 1 30); do
		if curl --silent --show-error --fail "http://${registry}/v2/" >/dev/null 2>&1; then
			ready=1
			break
		fi
		sleep 1
	done
	if [ "$ready" -ne 1 ]; then
		echo "build-agent-claude-image: temporary registry did not become ready" >&2
		container logs "$registry_container" >&2 || true
		exit 1
	fi
fi

ref="${registry}/${image_name}:${ref_tag}"
digest_ref="${registry}/${image_name}@${digest}"
echo "build-agent-claude-image: pushing ${ref} with scheme ${scheme}" >&2
container image tag "$image_name:local" "$ref" >&2
container image push --scheme "$scheme" "$ref" >&2

# Pulling the exact pre-push digest is both the registry-integrity check and the
# Apple container 1.1.0 workaround: it registers the name@digest lookup in the
# local store.
echo "build-agent-claude-image: seeding exact reference ${digest_ref}" >&2
container image pull --scheme "$scheme" "$digest_ref" >&2
seeded_digest=$(digest_of "$digest_ref")
if [ "$seeded_digest" != "$digest" ]; then
	echo "build-agent-claude-image: seeded digest ${seeded_digest:-missing} does not match built digest ${digest}" >&2
	exit 1
fi
# Successful local seeding includes successful registry removal. Remove it
# before printing the reference so a caller can never consume a success value
# while the setup-only registry may still be online.
if [ -n "$local_registry_port" ]; then
	remove_registry
fi
echo "$digest_ref"
