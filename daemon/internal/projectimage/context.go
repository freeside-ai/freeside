package projectimage

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const (
	nodeToolchainBaseImage     = "docker.io/library/debian:trixie-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd"
	nodeToolchainVersion       = "24.18.0"
	nodeToolchainArchiveSHA256 = "58c9520501f6ae2b52d5b210444e24b9d0c029a58c5011b797bc1fe7105886f6"
	nodeToolchainArchivePath   = "/opt/freeside/project-toolchain/node.tar.xz"
	nodeLauncherPath           = "/usr/local/bin/node"
	npmLauncherPath            = "/usr/local/bin/npm"
	npxLauncherPath            = "/usr/local/bin/npx"
)

const nodeToolchainLauncher = `#!/usr/bin/busybox sh
set -eu
owned_toolchain=false
if [ -n "${FREESIDE_PROJECT_NODE_ROOT:-}" ]; then
	toolchain=$FREESIDE_PROJECT_NODE_ROOT
	case "$toolchain" in
	/tmp/freeside-project-node.*) ;;
	*)
		echo "invalid inherited project Node root" >&2
		exit 126
		;;
	esac
	if [ ! -x "$toolchain/bin/node" ]; then
		echo "inherited project Node root is incomplete" >&2
		exit 126
	fi
else
	umask 077
	toolchain=$(/usr/bin/busybox mktemp -d /tmp/freeside-project-node.XXXXXX)
	owned_toolchain=true
	export FREESIDE_PROJECT_NODE_ROOT=$toolchain
	trap '/usr/bin/busybox rm -rf "$toolchain"' EXIT
	/usr/bin/busybox xz -dc /opt/freeside/project-toolchain/node.tar.xz |
		/usr/bin/busybox tar -xf - -C "$toolchain" --strip-components=1
fi
case "${0##*/}" in
node)
	set -- "$toolchain/bin/node" "$@"
	;;
npm)
	set -- "$toolchain/bin/node" "$toolchain/lib/node_modules/npm/bin/npm-cli.js" "$@"
	;;
npx)
	set -- "$toolchain/bin/node" "$toolchain/lib/node_modules/npm/bin/npx-cli.js" "$@"
	;;
*)
	echo "unknown project Node launcher name: ${0##*/}" >&2
	exit 127
	;;
esac
if [ "$owned_toolchain" = false ]; then
	exec "$@"
fi
"$@"
`

const prepareScript = `#!/usr/bin/busybox sh
set -eu
if ! /usr/bin/busybox cmp -s package.json /opt/freeside/project-seed/package.json ||
	! /usr/bin/busybox cmp -s package-lock.json /opt/freeside/project-seed/package-lock.json; then
	echo "project dependency manifests differ from the baked image inputs" >&2
	exit 42
fi
if [ -e npm-shrinkwrap.json ] || [ -e .npmrc ]; then
	echo "project contributes unsupported npm lock or configuration inputs" >&2
	exit 42
fi
export NPM_CONFIG_GLOBALCONFIG=/usr/local/etc/npmrc
export NPM_CONFIG_USERCONFIG=/dev/null
exec npm ci --ignore-scripts
`

func renderContainerfile(request Request, recipeDigest domain.Digest) string {
	// Every interpolated value has already passed validateRequest's restricted
	// grammar. The dependency manifests and exact recipe bytes enter through
	// COPY, never through instruction text.
	return fmt.Sprintf(`# Generated runtime artifact for %s at %s.
# It deliberately contributes no ENV, WORKDIR, ENTRYPOINT, CMD, USER, or
# VOLUME: project images pass the same ward realized-shape gate as agent bases.
ARG BASE_IMAGE
ARG PROJECT_TOOLCHAIN_BASE_IMAGE=%s
ARG PROJECT_NODE_VERSION=%s
ARG PROJECT_NODE_SHA256=%s

# Verification toolchains belong to the project image, not the provider base.
# This stage downloads and verifies the exact Node archive independently. The
# final image carries those compressed bytes plus a fixed launcher, so host-side
# provenance can hash the runtime source instead of trusting an extracted tree.
FROM ${PROJECT_TOOLCHAIN_BASE_IMAGE} AS project-toolchain

ARG PROJECT_NODE_VERSION
ARG PROJECT_NODE_SHA256

RUN set -eux; \
	apt-get update; \
	apt-get install -y --no-install-recommends ca-certificates curl xz-utils; \
	rm -rf /var/lib/apt/lists/*

RUN set -eux; \
	mkdir -p /opt/freeside/project-toolchain/root; \
	curl -fsSLo /opt/freeside/project-toolchain/node.tar.xz \
		"https://nodejs.org/dist/v${PROJECT_NODE_VERSION}/node-v${PROJECT_NODE_VERSION}-linux-arm64.tar.xz"; \
	echo "${PROJECT_NODE_SHA256}  /opt/freeside/project-toolchain/node.tar.xz" | sha256sum -c -; \
	tar -xJf /opt/freeside/project-toolchain/node.tar.xz \
		-C /opt/freeside/project-toolchain/root --strip-components=1 \
		--exclude CHANGELOG.md --exclude LICENSE --exclude README.md; \
	test "$(/opt/freeside/project-toolchain/root/bin/node --version)" = "v${PROJECT_NODE_VERSION}"

FROM ${BASE_IMAGE}

ARG BASE_DIGEST
ARG PROJECT_REPOSITORY
ARG PROJECT_REPOSITORY_ID
ARG PROJECT_COMMIT
ARG PROJECT_RECIPE_DIGEST
ARG PROJECT_NODE_VERSION
ARG PROJECT_NODE_SHA256

COPY --from=project-toolchain /opt/freeside/project-toolchain/node.tar.xz /opt/freeside/project-toolchain/node.tar.xz
COPY toolchain-launcher /usr/local/bin/node
COPY toolchain-launcher /usr/local/bin/npm
COPY toolchain-launcher /usr/local/bin/npx

RUN set -eux; \
	test "$(node --version)" = "v${PROJECT_NODE_VERSION}"; \
	npm --version; \
	mkdir -p /usr/local/etc /usr/local/share/freeside /opt/freeside/npm-cache /opt/freeside/project-seed; \
	printf '%%s\n' \
		'cache=%s' \
		'prefer-offline=true' \
		'audit=false' \
		'fund=false' \
		'update-notifier=false' > /usr/local/etc/npmrc; \
	test "$(NPM_CONFIG_GLOBALCONFIG=/usr/local/etc/npmrc \
		NPM_CONFIG_USERCONFIG=/dev/null npm config get cache)" = "%s"

COPY package.json package-lock.json /opt/freeside/project-seed/
COPY recipe.json %s
COPY prepare %s

# Cache warming executes no repository or dependency lifecycle scripts. The
# fresh verification workspace hydrates the same way: the image-owned
# preparation helper runs npm ci --ignore-scripts under --network none using
# only these cached tarballs.
RUN set -eux; \
	cd /opt/freeside/project-seed; \
	NPM_CONFIG_GLOBALCONFIG=/usr/local/etc/npmrc \
	NPM_CONFIG_USERCONFIG=/dev/null \
		npm ci --ignore-scripts; \
	rm -rf node_modules /tmp/freeside-project-node.*

LABEL org.opencontainers.image.title="freeside-project-image" \
	ai.freeside.base.digest="${BASE_DIGEST}" \
	ai.freeside.project.toolchain.node.version="${PROJECT_NODE_VERSION}" \
	ai.freeside.project.toolchain.node.archive-sha256="${PROJECT_NODE_SHA256}" \
	ai.freeside.project.repository="${PROJECT_REPOSITORY}" \
	ai.freeside.project.repository-id="${PROJECT_REPOSITORY_ID}" \
	ai.freeside.project.commit="${PROJECT_COMMIT}" \
	ai.freeside.project.recipe-digest="${PROJECT_RECIPE_DIGEST}"
`, request.Repository, request.CommitSHA,
		nodeToolchainBaseImage, nodeToolchainVersion, nodeToolchainArchiveSHA256,
		NPMCachePath, NPMCachePath, ward.ProjectRecipePath, PreparationPath) +
		"# Bound recipe digest: " + string(recipeDigest) + "\n"
}
