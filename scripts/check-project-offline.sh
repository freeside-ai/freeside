#!/usr/bin/env bash
# Prove a project image runs its verification recipe with no network (issue #304).
#
# Plan §5.4 makes `provider_only` the default egress profile and plan §5.6 gives
# the clean verifier no network at all, so a project image has to carry the
# project's dependency closure. This script is the proof, in two runs against a
# real checkout of the project at the commit the image baked:
#
#   positive  the verbatim recipe under --network none must succeed;
#   negative  the recipe's dependency install alone, with the baked npm cache
#             masked by an empty tmpfs, must fail by reaching for the registry.
#
# The negative probe is what makes the positive one mean something: without it,
# a recipe that quietly reached a registry, or one that never needed
# dependencies at all, would look identical. It is narrowed to `npm ci`, and its
# failure has to be the one being probed for: a container that never started, or
# a lint or test step failing for its own reasons, is a nonzero exit that says
# nothing about where the dependencies came from. Masking with tmpfs rather than
# permissions is deliberate; the recipe runs as root, for which a chmod proves
# nothing.
#
# The recipe is run verbatim, not rewritten for the offline case: `npm ci`
# works offline because of the image's baked cache and npmrc, so what is proved
# is the recipe the project actually uses. The proof is bound to the lockfile
# the image baked; a checkout that changes dependencies is expected to fail.
#
# The host bind mount here would be refused by the ward (check 2 forbids host
# binds in an agent VM). This is a build-time verification tool run from the
# host, not the ward's execution path, and it stands in for the workspace volume
# the daemon supplies at run time.
#
# Usage:
#   scripts/check-project-offline.sh <image-reference> [--commit SHA]
#
#   --commit SHA        check out this commit instead of the one pinned in
#                       images/project-gh-imgup/Containerfile.
set -euo pipefail

repository=https://github.com/freeasinbird/gh-imgup.git
recipe='cd /workspace && npm ci && npm run lint && npm run typecheck && npm test'
cache_dir=/opt/freeside/npm-cache

image_ref=""
commit=""

require_value() {
	if [ "$#" -lt 2 ] || [ -z "$2" ]; then
		echo "check-project-offline: $1 requires a value" >&2
		exit 2
	fi
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--commit)
		require_value "$@"
		commit="$2"
		shift 2
		;;
	-h | --help)
		grep '^#' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	-*)
		echo "check-project-offline: unknown argument: $1" >&2
		exit 2
		;;
	*)
		if [ -n "$image_ref" ]; then
			echo "check-project-offline: unexpected argument: $1" >&2
			exit 2
		fi
		image_ref="$1"
		shift
		;;
	esac
done

if [ -z "$image_ref" ]; then
	echo "check-project-offline: usage: scripts/check-project-offline.sh <image-reference> [--commit SHA]" >&2
	exit 2
fi

repo_root=$(cd "$(dirname "$0")/.." && pwd)
if [ -z "$commit" ]; then
	commit=$(sed -n 's/^ARG GH_IMGUP_COMMIT=\(.*\)$/\1/p' \
		"$repo_root/images/project-gh-imgup/Containerfile" | head -n 1)
fi
if [ -z "$commit" ]; then
	echo "check-project-offline: could not read the pinned project commit from the Containerfile" >&2
	exit 1
fi
# A branch, tag, or abbreviated SHA is a moving reference, and what the image
# baked is one exact commit; the identity assertion after the checkout would
# fail confusingly instead of saying that.
if ! printf '%s' "$commit" | grep -Eq '^[0-9a-f]{40}$'; then
	echo "check-project-offline: the project commit must be a full 40-character SHA, not '${commit}'" >&2
	exit 2
fi

# The checkout lives under the repo root's parent-independent temporary area of
# the invoking user, because Apple container shares the user's home; a system
# temporary directory is not necessarily bindable into the VM.
workdir=$(mktemp -d "${TMPDIR:-/tmp}/freeside-offline-proof.XXXXXX")

cleanup() {
	status=$?
	trap - EXIT
	rm -rf "$workdir"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

echo "check-project-offline: cloning ${repository} at ${commit}" >&2
git clone --quiet --filter=blob:none --no-checkout "$repository" "$workdir/positive" >&2
git -C "$workdir/positive" checkout --quiet --detach "$commit" >&2
observed_commit=$(git -C "$workdir/positive" rev-parse HEAD)
if [ "$observed_commit" != "$commit" ]; then
	echo "check-project-offline: checkout is at ${observed_commit}, expected ${commit}" >&2
	exit 1
fi
# The negative probe gets its own pristine copy: a workspace left behind by the
# positive run would let installed dependencies stand in for the cache.
cp -a "$workdir/positive" "$workdir/negative"

# Both legs read a nonzero exit as evidence, so first prove the image starts at
# all under this runtime: otherwise a container that never ran would read as a
# recipe that legitimately failed for want of dependencies.
if ! container run --rm --network none -- "$image_ref" sh -c true >&2; then
	echo "check-project-offline: the image does not start under this runtime; neither leg's result would mean anything" >&2
	exit 1
fi

echo "check-project-offline: positive run (recipe under --network none)" >&2
if ! container run --rm --network none \
	--volume "$workdir/positive:/workspace" \
	-- "$image_ref" sh -c "$recipe" >&2; then
	echo "check-project-offline: the recipe failed with networking disabled; the image does not carry the project's dependency closure" >&2
	exit 1
fi

# The negative leg is narrowed to the dependency install, and its failure has to
# be the one being probed for. A run that died creating the container, or a lint
# or test step failing for its own reasons, is a nonzero exit that proves nothing
# about where the dependencies came from.
echo "check-project-offline: negative probe (npm ci alone, baked cache masked)" >&2
negative_log="$workdir/negative.log"
if container run --rm --network none \
	--tmpfs "$cache_dir" \
	--volume "$workdir/negative:/workspace" \
	-- "$image_ref" sh -c 'cd /workspace && npm ci' >"$negative_log" 2>&1; then
	echo "check-project-offline: npm ci succeeded with the baked cache masked; the offline result does not prove the cache is what supplied the dependencies" >&2
	exit 1
fi
if ! grep -Eq 'EAI_AGAIN|ENOTFOUND|ECONNREFUSED|ETIMEDOUT|request to https://registry' "$negative_log"; then
	echo "check-project-offline: the negative probe failed without reaching for the registry, so it does not isolate the cache; its output was:" >&2
	tail -n 20 "$negative_log" >&2
	exit 1
fi

echo "check-project-offline: ${image_ref} runs the recipe offline from its baked cache (commit ${commit})" >&2
