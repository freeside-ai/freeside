#!/usr/bin/env bash
# Build the Freeside project image for freeasinbird/gh-imgup (issue #304).
#
# Extends the Claude agent base with this project's dependency closure, baked
# from the lockfile vendored at images/project-gh-imgup/, so a stage under the
# default `provider_only` egress profile needs no package-registry egress and
# the clean verifier can run the project's recipe with no network at all.
# Prints a name@sha256:<digest> reference.
#
# The base is consumed by tag, not by digest: the builder runs in its own VM, so
# a loopback temporary registry is unreachable from it, and Apple `container`
# 1.1.0 cannot resolve a locally-built name@digest anyway. The base's digest is
# read here and recorded in the image as ai.freeside.base.digest, which is the
# provenance the tag cannot carry. Build the base first with
# scripts/build-agent-claude-image.sh.
#
# The checks are separate steps, and a project image needs both: it is an agent
# image too, so scripts/check-agent-image.sh applies to it exactly as it does to
# the base (it can inherit a noncompliant --base, or grow an image-side setting
# of its own, and the offline proof would notice neither), and
# scripts/check-project-offline.sh proves the baked dependency closure.
#
# The temporary-registry lifecycle here is deliberately duplicated from
# scripts/build-exporter-image.sh rather than shared: that script is live-verified
# and out of this unit's scope. Keep them in sync; see the follow-up issue for
# extracting a shared helper once a third caller exists.
#
# Usage:
#   scripts/build-project-gh-imgup-image.sh [--tag NAME] [--base REF]
#       [--registry HOST[/PATH] | --local-registry-port PORT] [--ref-tag TAG]
#
#   --base REF          agent base image to extend
#                       (default: freeside-agent-claude:local).
#   --dns IP            nameserver for the build, repeatable. Warming the npm
#                       cache fetches from the npm registry; on a host whose
#                       container gateway does not answer DNS (observed with a
#                       VPN client installed), the builder resolves nothing
#                       without this.
#   --registry HOST     tag as HOST/<name>:<ref-tag>, push via `container`, print
#                       the pushed digest reference (HOST/<name>@sha256:...).
#   --local-registry-port PORT
#                       seed the exact digest reference through a temporary
#                       127.0.0.1 registry, then remove the registry (PORT must
#                       be 1024-65535).
#   --ref-tag TAG       tag used for the push reference (default: v1).
set -euo pipefail

image_name=freeside-project-gh-imgup
base_image=freeside-agent-claude:local
registry=""
local_registry_port=""
ref_tag=v1
dns_args=()

require_value() {
	if [ "$#" -lt 2 ] || [ -z "$2" ]; then
		echo "build-project-gh-imgup-image: $1 requires a value" >&2
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
	--base)
		require_value "$@"
		base_image="$2"
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
		echo "build-project-gh-imgup-image: unknown argument: $1" >&2
		exit 2
		;;
	esac
done

if [ -n "$registry" ] && [ -n "$local_registry_port" ]; then
	echo "build-project-gh-imgup-image: --registry and --local-registry-port are mutually exclusive" >&2
	exit 2
fi
if [ -n "$local_registry_port" ]; then
	case "$local_registry_port" in
	*[!0-9]* | "")
		echo "build-project-gh-imgup-image: local registry port must be an integer from 1024 to 65535" >&2
		exit 2
		;;
	esac
	if [ "$local_registry_port" -lt 1024 ] || [ "$local_registry_port" -gt 65535 ]; then
		echo "build-project-gh-imgup-image: local registry port must be an integer from 1024 to 65535" >&2
		exit 2
	fi
fi

repo_root=$(cd "$(dirname "$0")/.." && pwd)
context="$repo_root/images/project-gh-imgup"
registry_container=""

remove_registry() {
	[ -n "$registry_container" ] || return 0
	if ! container delete --force "$registry_container" >/dev/null 2>&1; then
		echo "build-project-gh-imgup-image: could not remove temporary registry ${registry_container}" >&2
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

base_digest=$(digest_of "$base_image" 2>/dev/null || true)
if [ -z "${base_digest:-}" ]; then
	echo "build-project-gh-imgup-image: could not read the digest of base image ${base_image}; build it first with scripts/build-agent-claude-image.sh" >&2
	exit 1
fi

echo "build-project-gh-imgup-image: building on ${base_image} (${base_digest})" >&2
container build ${dns_args[@]+"${dns_args[@]}"} \
	--build-arg "BASE_IMAGE=${base_image}" \
	--build-arg "BASE_DIGEST=${base_digest}" \
	--tag "$image_name:local" "$context" >&2

digest=$(digest_of "$image_name:local")
[ -n "${digest:-}" ] || {
	echo "build-project-gh-imgup-image: could not read the built image digest" >&2
	exit 1
}

if [ -z "$registry" ] && [ -z "$local_registry_port" ]; then
	echo "build-project-gh-imgup-image: built ${image_name}:local (${digest}); pass --local-registry-port for a live-usable reference" >&2
	echo "${image_name}@${digest}"
	exit 0
fi

scheme=auto
if [ -n "$local_registry_port" ]; then
	command -v curl >/dev/null 2>&1 || {
		echo "build-project-gh-imgup-image: curl is required for local registry readiness" >&2
		exit 1
	}

	# The registry helper is pulled and executed by its reviewed multi-platform
	# index digest. A cached exact reference keeps this path usable offline.
	registry_image_digest=sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
	registry_image="docker.io/library/registry@${registry_image_digest}"
	actual_registry_digest=$(digest_of "$registry_image" 2>/dev/null || true)
	if [ "$actual_registry_digest" != "$registry_image_digest" ]; then
		echo "build-project-gh-imgup-image: pulling the pinned registry helper" >&2
		container image pull --scheme https "$registry_image" >&2
		actual_registry_digest=$(digest_of "$registry_image")
	fi
	if [ "$actual_registry_digest" != "$registry_image_digest" ]; then
		echo "build-project-gh-imgup-image: registry helper digest ${actual_registry_digest:-missing} does not match ${registry_image_digest}" >&2
		exit 1
	fi

	candidate_registry_container="freeside-project-gh-imgup-registry-$(date +%s)-$$"
	if container inspect "$candidate_registry_container" >/dev/null 2>&1; then
		echo "build-project-gh-imgup-image: temporary registry name already exists: $candidate_registry_container" >&2
		exit 1
	fi
	# Arm cleanup only after proving the generated name was unoccupied, but
	# before create so a partially bootstrapped container is still owned here.
	registry_container="$candidate_registry_container"
	registry="127.0.0.1:${local_registry_port}"
	scheme=http
	echo "build-project-gh-imgup-image: starting pinned temporary registry at ${registry}" >&2
	container run --detach --name "$registry_container" \
		--label "freeside.project-gh-imgup-seed=${registry_container}" \
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
		echo "build-project-gh-imgup-image: temporary registry did not become ready" >&2
		container logs "$registry_container" >&2 || true
		exit 1
	fi
fi

ref="${registry}/${image_name}:${ref_tag}"
digest_ref="${registry}/${image_name}@${digest}"
echo "build-project-gh-imgup-image: pushing ${ref} with scheme ${scheme}" >&2
container image tag "$image_name:local" "$ref" >&2
container image push --scheme "$scheme" "$ref" >&2

# Pulling the exact pre-push digest is both the registry-integrity check and the
# Apple container 1.1.0 workaround: it registers the name@digest lookup in the
# local store.
echo "build-project-gh-imgup-image: seeding exact reference ${digest_ref}" >&2
container image pull --scheme "$scheme" "$digest_ref" >&2
seeded_digest=$(digest_of "$digest_ref")
if [ "$seeded_digest" != "$digest" ]; then
	echo "build-project-gh-imgup-image: seeded digest ${seeded_digest:-missing} does not match built digest ${digest}" >&2
	exit 1
fi
# Successful local seeding includes successful registry removal. Remove it
# before printing the reference so a caller can never consume a success value
# while the setup-only registry may still be online.
if [ -n "$local_registry_port" ]; then
	remove_registry
fi
echo "$digest_ref"
