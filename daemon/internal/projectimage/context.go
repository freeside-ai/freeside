package projectimage

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
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
	// The dependency seed the Containerfile bakes and prepareScript compares
	// each fresh workspace against.
	seedPackageJSONPath = "/opt/freeside/project-seed/package.json"
	seedPackageLockPath = "/opt/freeside/project-seed/package-lock.json"

	// v2 added the Go toolchain sources to the digested set; v3 added the
	// vacuum, shellcheck, Python, and procps pins a Go image bakes (#2376);
	// v4 added the Swift toolchain sources (#2183); v5 added the Go and
	// Swift helper and Containerfile (#2881).
	preparationDigestVersion = "freeside.project-image-preparation/v5"
)

// nodeToolchainLauncher is installed at node, npm, and npx and dispatches on
// the name it was run as. It extracts the verified Node archive once per
// process tree into a private directory, and every descendant reuses that
// tree through FREESIDE_PROJECT_NODE_ROOT. It owns the npm configuration of a
// project room: the room passes only HOME and LC_ALL, and the image may set no
// ENV, so without these exports a later npm would use an empty cache under
// HOME and the public registry. NPM_CONFIG_GLOBALCONFIG names the baked npmrc
// (the baked cache, prefer-offline), and NPM_CONFIG_USERCONFIG=/dev/null keeps
// a file under HOME from overriding it. A writer's npm ci then reinstalls from
// the baked cache with no network instead of deleting node_modules and failing
// at the registry (#3179).
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
export NPM_CONFIG_GLOBALCONFIG=/usr/local/etc/npmrc NPM_CONFIG_USERCONFIG=/dev/null
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

// PreparationDigest addresses this binary's fixed toolchain and preparation
// implementation for every toolchain it builds: each preparation helper,
// launcher, and Containerfile template (which carry the toolchain base image
// and the npm configuration), and each pinned toolchain version and archive
// hash. One digest covers every toolchain, so an edit to any path retires
// cross-commit reuse of every image built before it. A project image records the
// value it was built with, and reuse at another commit requires the running
// binary's to equal it: these sources decide what an image's environment is as
// much as the project's manifests do.
//
// It is deliberately coarse. Any edit to those sources, a comment included,
// changes it, so an image built by a different builder is never assumed
// equivalent.
func PreparationDigest() domain.Digest {
	return preparationDigest(preparationSources())
}

// preparationSources lists, in digest order, every builder source
// PreparationDigest addresses. The Go, Swift, and Go and Swift preparation
// helpers and Containerfiles enter as templates rendered without a module or package
// directory or a request: the directory is recorded in the image's
// environment on its own.
func preparationSources() []string {
	return []string{
		preparationDigestVersion,
		prepareScript,
		nodeToolchainLauncher,
		renderNPMContainerfile(Request{}, ""),
		nodeToolchainVersion,
		nodeToolchainArchiveSHA256,
		goPrepareScript(""),
		goToolchainLauncher,
		renderGoContainerfile(Request{}, ""),
		goToolchainVersion,
		goToolchainArchiveSHA256,
		golangciLintVersion,
		golangciLintArchiveSHA256,
		vacuumVersion,
		vacuumArchiveSHA256,
		shellcheckVersion,
		shellcheckArchiveSHA256,
		pythonVersion,
		pythonArchiveSHA256,
		procpsVersion,
		procpsSnapshot,
		procpsArchiveSHA256,
		libproc2ArchiveSHA256,
		swiftPrepareScript(""),
		swiftToolchainLauncher,
		swiftMirrorDiscovery,
		swiftMirrorBake,
		renderSwiftContainerfile(Request{}, ""),
		swiftToolchainVersion,
		swiftToolchainArchiveSHA256,
		swiftAptSnapshot,
		strings.Join(swiftAptPackages, " "),
		goSwiftPrepareScript("", ""),
		renderGoSwiftContainerfile(Request{}, ""),
	}
}

func preparationDigest(parts []string) domain.Digest {
	// Length-prefixed parts, so no two source sets share a preimage.
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(strconv.Itoa(len(part)) + ":" + part))
	}
	return domain.Digest(contentaddr.Format(hash.Sum(nil)))
}

// projectLabels are the provenance label lines every project image carries
// after its toolchain's, ending the LABEL instruction.
const projectLabels = `	ai.freeside.project.repository="${PROJECT_REPOSITORY}" \
	ai.freeside.project.repository-id="${PROJECT_REPOSITORY_ID}" \
	ai.freeside.project.commit="${PROJECT_COMMIT}" \
	ai.freeside.project.recipe-digest="${PROJECT_RECIPE_DIGEST}"
`

// renderContainerfile renders the generated Containerfile for the layout's
// toolchain.
func renderContainerfile(
	layout domain.ProjectImageInputLayout, request Request, recipeDigest domain.Digest,
) string {
	switch layout.Toolchain {
	case domain.ProjectToolchainNPM:
		return renderNPMContainerfile(request, recipeDigest)
	case domain.ProjectToolchainGo:
		return renderGoContainerfile(request, recipeDigest)
	case domain.ProjectToolchainSwift:
		return renderSwiftContainerfile(request, recipeDigest)
	case domain.ProjectToolchainGoSwift:
		return renderGoSwiftContainerfile(request, recipeDigest)
	}
	return ""
}

func renderNPMContainerfile(request Request, recipeDigest domain.Digest) string {
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
# An approved base may legitimately carry Node at these launcher paths (the
# Claude base extracts the official tarball, whose bin/npm and bin/npx are
# symlinks), and COPY follows a pre-existing symlink at the destination,
# writing the launcher bytes through it rather than replacing the link. Clear
# the three paths first with the base's own already-trusted static BusyBox so
# each COPY lands a fresh regular file while trusting nothing at the target:
# rm -f unlinks whatever entry exists, and unlinking a symlink never touches
# what it points at. Exec form needs no shell in the base; rm -f (not -rf)
# fails the build loudly should an approved base ever ship a directory here.
RUN ["/usr/bin/busybox", "rm", "-f", "/usr/local/bin/node", "/usr/local/bin/npm", "/usr/local/bin/npx"]
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
# only these cached tarballs. npm writes the cache owner-only, so the cache is
# opened for reading: the unprivileged writer's own npm ci reads it through
# the launcher's npm configuration, and without read access npm fails with
# EACCES after it has already removed node_modules. It stays root-owned and
# unwritable to the writer.
RUN set -eux; \
	cd /opt/freeside/project-seed; \
	NPM_CONFIG_GLOBALCONFIG=/usr/local/etc/npmrc \
	NPM_CONFIG_USERCONFIG=/dev/null \
		npm ci --ignore-scripts; \
	rm -rf node_modules /tmp/freeside-project-node.*; \
	chmod -R a+rX %s

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
		NPMCachePath, NPMCachePath, ward.ProjectRecipePath, PreparationPath, NPMCachePath) +
		"# Bound recipe digest: " + string(recipeDigest) + "\n"
}
