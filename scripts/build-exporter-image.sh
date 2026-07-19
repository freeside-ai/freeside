#!/usr/bin/env bash
# Build the Freeside exporter image (issue #170).
#
# Cross-compiles the static freeside-export helper and assembles the OCI image
# that ships it at the contracted path (images/exporter/Containerfile). Prints a
# name@sha256:<digest> reference for ward's ExporterImage and the live
# conformance tests (FREESIDE_WARD_EXPORTER_IMAGE).
#
# Resolving the digest: ward requires a digest-pinned image, and Apple
# `container` 1.1.0 resolves a digest reference only through a REGISTRY (a
# locally-built image runs by tag but not by digest). So a live run needs the
# image in a registry `container` can pull from. Pass --registry to tag and push
# there; without it the script prints the locally-built index digest for
# reference and a CI build smoke check only.
#
#   Known limitation: `container image push` to a plain HTTP registry:2 fails on
#   1.1.0 ("Network is down" at 100%). Use a registry `container` can push to
#   (e.g. ghcr over HTTPS), or push the built image with another tool
#   (docker/skopeo) that can read this image and write your registry.
#
# The pinned alpine base and the static -trimpath binary make the image content
# deterministic, so re-running reproduces the digest.
#
# Built with Apple `container` (ward's runtime); its content store is what ward
# resolves against. Push the built image to your registry with a tool that can
# read the container store and write that registry (see the limitation note),
# then pin the pushed digest.
#
# Usage:
#   scripts/build-exporter-image.sh [--tag NAME] [--registry HOST[/PATH]]
#                                   [--ref-tag TAG]
#
#   --registry HOST     tag as HOST/<name>:<ref-tag>, push via `container`, print
#                       the pushed digest reference (HOST/<name>@sha256:...).
#   --ref-tag TAG       tag used for the push reference (default: v1).
set -euo pipefail

image_name=freeside-exporter
registry=""
ref_tag=v1

while [ "$#" -gt 0 ]; do
	case "$1" in
	--tag)
		image_name="${2:-}"
		shift 2
		;;
	--registry)
		registry="${2:-}"
		shift 2
		;;
	--ref-tag)
		ref_tag="${2:-}"
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

repo_root=$(cd "$(dirname "$0")/.." && pwd)
context="$repo_root/images/exporter"
binary="$context/freeside-export"

cleanup() { rm -f "$binary"; }
trap cleanup EXIT

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

if [ -z "$registry" ]; then
	# No registry: report the local build's digest. Ward cannot resolve this by
	# digest from the local store alone (see the header); use --registry for a
	# live-usable reference.
	digest=$(digest_of "$image_name:local")
	[ -n "${digest:-}" ] || { echo "build-exporter-image: could not read the built image digest" >&2; exit 1; }
	echo "build-exporter-image: built ${image_name}:local (${digest}); pass --registry for a digest-resolvable reference" >&2
	echo "${image_name}@${digest}"
	exit 0
fi

ref="${registry}/${image_name}:${ref_tag}"
echo "build-exporter-image: pushing ${ref}" >&2
container image tag "$image_name:local" "$ref" >&2
container image push "$ref" >&2

# Read the pushed manifest digest back from the registry: that is the content
# address ward pins and `container create` resolves.
digest=$(container image inspect "$ref" |
	grep -o '"digest"[[:space:]]*:[[:space:]]*"sha256:[0-9a-f]\{64\}"' |
	head -n 1 | grep -o 'sha256:[0-9a-f]\{64\}')
[ -n "${digest:-}" ] || { echo "build-exporter-image: could not read the pushed image digest" >&2; exit 1; }
echo "${registry}/${image_name}@${digest}"
