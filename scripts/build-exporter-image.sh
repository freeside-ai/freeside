#!/usr/bin/env bash
# Build the Freeside exporter image (issue #170).
#
# Cross-compiles the static freeside-export helper and assembles the OCI image
# that ships it at the contracted path (images/exporter/Containerfile). Prints a
# name@sha256:<digest> reference for ward's ExporterImage and the live
# conformance tests (FREESIDE_WARD_EXPORTER_IMAGE).
#
# Resolving the digest: ward requires a digest-pinned image, but Apple
# `container` 1.1.0 does not resolve a locally-built name@digest reference from
# its content store. --local-registry-port works around that lookup bug without
# weakening ward: it briefly runs a pinned registry:2 on loopback, pushes over
# explicit HTTP, pulls the exact digest reference into the local store, and
# removes the registry before printing the live-usable reference.
#
# The alpine base is pinned and the helper is a static -trimpath binary. Apple
# container's build metadata can still vary between invocations, so the script
# captures and verifies the exact digest produced by this invocation.
#
# Built with Apple `container` (ward's runtime); its content store is what ward
# resolves against. An external registry remains supported for shared images.
#
# Usage:
#   scripts/build-exporter-image.sh [--tag NAME]
#       [--registry HOST[/PATH] | --local-registry-port PORT] [--ref-tag TAG]
#
#   --registry HOST     tag as HOST/<name>:<ref-tag>, push via `container`, print
#                       the pushed digest reference (HOST/<name>@sha256:...).
#   --local-registry-port PORT
#                       seed the exact digest reference through a temporary
#                       127.0.0.1 registry, then remove the registry (PORT must
#                       be 1024-65535).
#   --ref-tag TAG       tag used for the push reference (default: v1).
set -euo pipefail

image_name=freeside-exporter
registry=""
local_registry_port=""
ref_tag=v1

require_value() {
	if [ "$#" -lt 2 ] || [ -z "$2" ]; then
		echo "build-exporter-image: $1 requires a value" >&2
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
		echo "build-exporter-image: unknown argument: $1" >&2
		exit 2
		;;
	esac
done

if [ -n "$registry" ] && [ -n "$local_registry_port" ]; then
	echo "build-exporter-image: --registry and --local-registry-port are mutually exclusive" >&2
	exit 2
fi
if [ -n "$local_registry_port" ]; then
	case "$local_registry_port" in
	*[!0-9]* | "")
		echo "build-exporter-image: local registry port must be an integer from 1024 to 65535" >&2
		exit 2
		;;
	esac
	if [ "$local_registry_port" -lt 1024 ] || [ "$local_registry_port" -gt 65535 ]; then
		echo "build-exporter-image: local registry port must be an integer from 1024 to 65535" >&2
		exit 2
	fi
fi

repo_root=$(cd "$(dirname "$0")/.." && pwd)
context="$repo_root/images/exporter"
binary="$context/freeside-export"
registry_container=""

remove_registry() {
	[ -n "$registry_container" ] || return 0
	if ! container delete --force "$registry_container" >/dev/null 2>&1; then
		echo "build-exporter-image: could not remove temporary registry ${registry_container}" >&2
		return 1
	fi
	registry_container=""
}

cleanup() {
	status=$?
	trap - EXIT
	rm -f "$binary"
	if ! remove_registry && [ "$status" -eq 0 ]; then
		status=1
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

echo "build-exporter-image: cross-compiling the static exporter helper" >&2
(
	cd "$repo_root/daemon"
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o "$binary" ./cmd/freeside-export
)

# digest_of prints the manifest-index digest of a locally-built image name.
digest_of() {
	container image inspect "$1" |
		grep -o '"digest"[[:space:]]*:[[:space:]]*"sha256:[0-9a-f]\{64\}"' |
		head -n 1 | grep -o 'sha256:[0-9a-f]\{64\}'
}

echo "build-exporter-image: building with Apple container" >&2
container build --tag "$image_name:local" "$context" >&2

digest=$(digest_of "$image_name:local")
[ -n "${digest:-}" ] || { echo "build-exporter-image: could not read the built image digest" >&2; exit 1; }

if [ -z "$registry" ] && [ -z "$local_registry_port" ]; then
	# No registry: report the local build's digest. Ward cannot resolve this by
	# digest from the local store alone on container 1.1.0 (see the header).
	echo "build-exporter-image: built ${image_name}:local (${digest}); pass --local-registry-port for a live-usable local reference" >&2
	echo "${image_name}@${digest}"
	exit 0
fi

scheme=auto
if [ -n "$local_registry_port" ]; then
	command -v curl >/dev/null 2>&1 || {
		echo "build-exporter-image: curl is required for local registry readiness" >&2
		exit 1
	}

	# The registry helper is pulled and executed by its reviewed multi-platform
	# index digest. A cached exact reference keeps this path usable offline.
	registry_image_digest=sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
	registry_image="docker.io/library/registry@${registry_image_digest}"
	actual_registry_digest=$(digest_of "$registry_image" 2>/dev/null || true)
	if [ "$actual_registry_digest" != "$registry_image_digest" ]; then
		echo "build-exporter-image: pulling the pinned registry helper" >&2
		container image pull --scheme https "$registry_image" >&2
		actual_registry_digest=$(digest_of "$registry_image")
	fi
	if [ "$actual_registry_digest" != "$registry_image_digest" ]; then
		echo "build-exporter-image: registry helper digest ${actual_registry_digest:-missing} does not match ${registry_image_digest}" >&2
		exit 1
	fi

	candidate_registry_container="freeside-exporter-registry-$(date +%s)-$$"
	if container inspect "$candidate_registry_container" >/dev/null 2>&1; then
		echo "build-exporter-image: temporary registry name already exists: $candidate_registry_container" >&2
		exit 1
	fi
	# Arm cleanup only after proving the generated name was unoccupied, but
	# before create so a partially bootstrapped container is still owned here.
	registry_container="$candidate_registry_container"
	registry="127.0.0.1:${local_registry_port}"
	scheme=http
	echo "build-exporter-image: starting pinned temporary registry at ${registry}" >&2
	container run --detach --name "$registry_container" \
		--label "freeside.exporter-seed=${registry_container}" \
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
		echo "build-exporter-image: temporary registry did not become ready" >&2
		container logs "$registry_container" >&2 || true
		exit 1
	fi
fi

ref="${registry}/${image_name}:${ref_tag}"
digest_ref="${registry}/${image_name}@${digest}"
echo "build-exporter-image: pushing ${ref} with scheme ${scheme}" >&2
container image tag "$image_name:local" "$ref" >&2
container image push --scheme "$scheme" "$ref" >&2

# Pulling the exact pre-push digest is both the registry-integrity check and the
# Apple container 1.1.0 workaround: it registers the name@digest lookup in the
# local store. Ward later uses the ordinary create path, with no HTTP override.
echo "build-exporter-image: seeding exact reference ${digest_ref}" >&2
container image pull --scheme "$scheme" "$digest_ref" >&2
seeded_digest=$(digest_of "$digest_ref")
if [ "$seeded_digest" != "$digest" ]; then
	echo "build-exporter-image: seeded digest ${seeded_digest:-missing} does not match built digest ${digest}" >&2
	exit 1
fi
# Successful local seeding includes successful registry removal. Remove it
# before printing the reference so a caller can never consume a success value
# while the setup-only registry may still be online.
if [ -n "$local_registry_port" ]; then
	remove_registry
fi
echo "$digest_ref"
