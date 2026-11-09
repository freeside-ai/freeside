package projectimage

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// The Swift toolchain path (#2183). The builder pins one Swift release, the
// Debian 12 aarch64 archive from download.swift.org, which runs on the trixie
// agent base once the shared libraries it links are installed. Those come
// from a fixed snapshot.debian.org timestamp, so the installed package set
// never moves; the timestamp must not predate the agent base's own build, or
// an exact-version dependency such as libc6-dev on libc6 would ask apt for a
// downgrade it refuses.
const (
	swiftToolchainVersion       = "6.3.1"
	swiftToolchainArchiveSHA256 = "41daa682341df209ca7dfe8cc137c6bd1f4bcde47f9ff6d5c66fd83acf6b5dcd"
	swiftToolchainArchivePath   = "/opt/freeside/project-toolchain/swift.tar.gz"
	swiftLauncherPath           = "/usr/local/bin/swift"
	swiftAptSnapshot            = "20261108T000000Z"

	seedPackageSwiftPath    = "/opt/freeside/project-seed/Package.swift"
	seedPackageResolvedPath = "/opt/freeside/project-seed/Package.resolved"
)

// swiftAptPackages is the swift.org Debian 12 dependency list in trixie's
// names, less what the agent base already carries (git) and what serves only
// lldb (libpython3-dev, whose 3.11 soname trixie lacks), plus the C library
// headers and zlib gcc only recommends, and the project's declared system
// libraries: libsqlite3-dev, which the Freeside app needs.
var swiftAptPackages = []string{
	"binutils-gold", "gcc", "libc6-dev", "libcurl4-openssl-dev", "libedit-dev",
	"libicu-dev", "libncurses-dev", "libsqlite3-dev", "libstdc++-14-dev",
	"libxml2-dev", "pkg-config", "tzdata", "uuid-dev", "zlib1g-dev",
}

// swiftOfflineRefusal is git's answer when a dependency fetch reaches for the
// network: with the mirrors masked, the launcher's insteadOf configuration is
// gone with them, and git tries the dependency's own URL.
var swiftOfflineRefusal = regexp.MustCompile(
	`(Could not resolve host|unable to access|Failed to connect|Could not read from remote repository)`)

// swiftToolchainLauncher is installed at swiftLauncherPath. It extracts the
// verified archive once per room into a fixed /tmp tree, as the Go launcher
// does for its tool families: the extracted toolchain is several gigabytes
// and takes tens of seconds to unpack, so a per-process-tree extraction
// would repeat that for every swift a recipe runs. Concurrent first calls
// each extract into a staging directory and rename it into place; the
// rename fails onto a populated tree, and the loser discards its copy.
//
// It owns the Swift environment of a project room, which passes only HOME and
// LC_ALL: GIT_CONFIG_GLOBAL points every git SwiftPM runs, submodule clones
// included, at the mirror redirection the image baked. The configuration
// lives inside the mirrors directory, so masking the mirrors masks the
// redirection too and git reaches for the network.
const swiftToolchainLauncher = `#!/usr/bin/busybox sh
set -eu
if [ "${0##*/}" != swift ]; then
	echo "unknown project Swift launcher name: ${0##*/}" >&2
	exit 127
fi
toolchain=/tmp/freeside-project-swift
if [ ! -x "$toolchain/usr/bin/swift" ]; then
	staging=$(/usr/bin/busybox mktemp -d /tmp/freeside-project-swift.XXXXXX)
	/usr/bin/busybox gzip -dc /opt/freeside/project-toolchain/swift.tar.gz |
		/usr/bin/busybox tar -xf - -C "$staging" --strip-components=1
	/usr/bin/busybox mv -T "$staging" "$toolchain" 2>/dev/null ||
		/usr/bin/busybox rm -rf "$staging"
	if [ ! -x "$toolchain/usr/bin/swift" ]; then
		echo "project Swift root is incomplete" >&2
		exit 126
	fi
fi
export GIT_CONFIG_GLOBAL=/opt/freeside/swiftpm-mirrors/gitconfig
exec "$toolchain/usr/bin/swift" "$@"
`

// swiftPrepareScript renders the Swift preparation helper for one package
// directory. It holds every fresh workspace to the manifests the image
// baked; refuses the inputs no image serves, including a version-specific
// manifest (Package@swift-6.swift and the like), which SwiftPM would load in
// place of the bound Package.swift, and a path dependency declared by the
// manifest SwiftPM evaluates here; then resolves the pinned closure from the
// baked mirrors and refuses a resolve that rewrote Package.resolved, which
// would mean the workspace resolved to something the mirrors were not built
// from. packageDir has
// passed domain.ValidProjectSwiftPackageDir, whose grammar admits no quote or
// shell metacharacter, so single quotes carry it and the paths built from
// it verbatim.
func swiftPrepareScript(packageDir string) string {
	unsupported := swiftUnsupportedEntries(packageDir)
	for index, entry := range unsupported {
		unsupported[index] = "'" + entry + "'"
	}
	return `#!/usr/bin/busybox sh
set -eu
package_dir='` + cmp.Or(packageDir, ".") + `'
if ! /usr/bin/busybox cmp -s "$package_dir/Package.swift" /opt/freeside/project-seed/Package.swift ||
	! /usr/bin/busybox cmp -s "$package_dir/Package.resolved" /opt/freeside/project-seed/Package.resolved; then
	echo "project Swift package manifests differ from the baked image inputs" >&2
	exit 42
fi
for entry in ` + strings.Join(unsupported, " ") + ` "$package_dir"/Package@swift*; do
	if [ -e "$entry" ] || [ -L "$entry" ]; then
		echo "project contributes unsupported Swift input $entry" >&2
		exit 42
	fi
done
manifest=$(/usr/bin/busybox mktemp /tmp/freeside-swift-manifest.XXXXXX)
swift package dump-package --package-path "$package_dir" >"$manifest"
if ! jq -e '[.dependencies[] | select(has("fileSystem"))] | length == 0' "$manifest" >/dev/null; then
	echo "project Swift package declares a local path dependency" >&2
	exit 42
fi
/usr/bin/busybox rm -f "$manifest"
swift package resolve --package-path "$package_dir" --only-use-versions-from-resolved-file
if ! /usr/bin/busybox cmp -s "$package_dir/Package.resolved" /opt/freeside/project-seed/Package.resolved; then
	echo "project Swift package resolved differently from the baked mirrors" >&2
	exit 42
fi
`
}

// swiftMaskedProbe is the cache-masked negative probe for a Swift image: the
// resolve the preparation helper runs, with the mirrors masked. It bypasses
// SwiftPM's shared dependency cache, which a room's fresh HOME holds empty
// anyway: through the cache, SwiftPM 6.3.1 reports only a failed read of the
// cache directory and drops git's own refusal to reach the host, the one
// signal swiftOfflineRefusal accepts.
func swiftMaskedProbe(packageDir string) []string {
	return []string{
		"swift", "package", "resolve", "--package-path", cmp.Or(packageDir, "."),
		"--only-use-versions-from-resolved-file", "--disable-dependency-cache",
	}
}

// swiftSnapshotSources renders the apt sources list both stages install
// from: trixie, its updates, and its security archive, each at the pinned
// snapshot. Plain http is what the pinned Debian bases already use through
// the build proxy; apt still verifies every index against the archive keys.
func swiftSnapshotSources() string {
	return strings.Join([]string{
		"deb http://snapshot.debian.org/archive/debian/" + swiftAptSnapshot + "/ trixie main",
		"deb http://snapshot.debian.org/archive/debian/" + swiftAptSnapshot + "/ trixie-updates main",
		"deb http://snapshot.debian.org/archive/debian-security/" + swiftAptSnapshot + "/ trixie-security main",
	}, "' '")
}

// swiftAptInstall renders one RUN body that installs packages from the
// pinned snapshot alone. A private sources list and an empty parts directory
// replace whatever sources the stage's base names, and the lists are removed
// afterwards so the image keeps no index. A snapshot's Release files are past
// their Valid-Until by design.
func swiftAptInstall(packages string) string {
	return `set -eux; \
	apt_dir=$(mktemp -d /tmp/freeside-swift-apt.XXXXXX); \
	mkdir "$apt_dir/parts"; \
	printf '%s\n' '` + swiftSnapshotSources() + `' > "$apt_dir/sources.list"; \
	set -- -o "Dir::Etc::SourceList=$apt_dir/sources.list" \
		-o "Dir::Etc::SourceParts=$apt_dir/parts" -o Acquire::Check-Valid-Until=false; \
	apt-get "$@" update; \
	DEBIAN_FRONTEND=noninteractive apt-get "$@" install -y --no-install-recommends ` + packages + `; \
	rm -rf "$apt_dir" /var/lib/apt/lists/*`
}

func renderSwiftContainerfile(request Request, recipeDigest domain.Digest) string {
	packages := strings.Join(swiftAptPackages, " ")
	// Every interpolated value is a builder constant or has passed
	// validateRequest's restricted grammar. The package manifests and exact
	// recipe bytes enter through COPY, never through instruction text.
	return fmt.Sprintf(`# Generated runtime artifact for %s at %s.
# It deliberately contributes no ENV, WORKDIR, ENTRYPOINT, CMD, USER, or
# VOLUME: project images pass the same ward realized-shape gate as agent bases.
ARG BASE_IMAGE
ARG PROJECT_TOOLCHAIN_BASE_IMAGE=%s
ARG PROJECT_SWIFT_VERSION=%s
ARG PROJECT_SWIFT_SHA256=%s
ARG PROJECT_SWIFT_APT_SNAPSHOT=%s
ARG PROJECT_SWIFT_APT_PACKAGES="%s"

# Verification toolchains belong to the project image, not the provider base.
# This stage downloads and verifies the exact Swift archive independently. The
# final image carries those compressed bytes plus a fixed launcher, so
# host-side provenance can hash the runtime source instead of trusting an
# extracted tree. The archive runs only beside the libraries the runtime stage
# installs, so the runtime stage proves the version.
FROM ${PROJECT_TOOLCHAIN_BASE_IMAGE} AS project-toolchain

ARG PROJECT_SWIFT_VERSION
ARG PROJECT_SWIFT_SHA256

RUN %s

RUN set -eux; \
	mkdir -p /opt/freeside/project-toolchain; \
	curl -fsSLo /opt/freeside/project-toolchain/swift.tar.gz \
		"https://download.swift.org/swift-${PROJECT_SWIFT_VERSION}-release/debian12-aarch64/swift-${PROJECT_SWIFT_VERSION}-RELEASE/swift-${PROJECT_SWIFT_VERSION}-RELEASE-debian12-aarch64.tar.gz"; \
	echo "${PROJECT_SWIFT_SHA256}  /opt/freeside/project-toolchain/swift.tar.gz" | sha256sum -c -

# The Swift runtime both later stages start from.
FROM ${BASE_IMAGE} AS project-swift-runtime

ARG PROJECT_SWIFT_VERSION
ARG PROJECT_SWIFT_APT_PACKAGES

COPY --from=project-toolchain /opt/freeside/project-toolchain/swift.tar.gz %s

# The shared libraries the toolchain links and the headers a build compiles
# against, from the pinned snapshot.
RUN %s

# Clear the launcher path with the base's own already-trusted static BusyBox
# before the COPY, so COPY lands a fresh regular file and never writes
# through a symlink an approved base may carry there (the Node path's #518).
RUN ["/usr/bin/busybox", "rm", "-f", "%s"]
COPY toolchain-launcher %s

RUN set -eux; \
	swift --version 2>&1 | grep -F "Swift version ${PROJECT_SWIFT_VERSION} (swift-${PROJECT_SWIFT_VERSION}-RELEASE)"; \
	rm -rf /tmp/freeside-project-swift*

# Discovery evaluates Package.swift and every dependency's manifest, as any
# resolve does, and resolves the closure Package.resolved pins through the
# build proxy; it builds and tests nothing. It lists every repository that
# resolve cloned, submodules included, mirrors them, and proves a fresh
# resolve succeeds from the mirrors alone with every proxy pointed at a
# closed port. Manifest code runs unsandboxed, so this stage never ships:
# only its URL list leaves it.
FROM project-swift-runtime AS project-swift-discovery

COPY Package.swift Package.resolved /opt/freeside/project-seed/
COPY bake-swift-mirrors %s
COPY discover-swift-mirrors %s
RUN ["/usr/bin/busybox", "sh", "%s"]

FROM project-swift-runtime

ARG BASE_DIGEST
ARG PROJECT_REPOSITORY
ARG PROJECT_REPOSITORY_ID
ARG PROJECT_COMMIT
ARG PROJECT_RECIPE_DIGEST
ARG PROJECT_SWIFT_VERSION
ARG PROJECT_SWIFT_SHA256
ARG PROJECT_SWIFT_APT_SNAPSHOT
ARG PROJECT_SWIFT_APT_PACKAGES

# The shipped mirrors are cloned again here, from the URL list, by a script
# that validates it and runs no project or dependency code. The list is read
# through a bind mount, never copied, so nothing manifest code wrote in the
# discovery stage, a directory or setuid file in the list's place included,
# becomes part of the image. The fresh verification workspace hydrates
# offline from the mirrors through the image-owned preparation helper, under
# the launcher's insteadOf redirection.
COPY bake-swift-mirrors %s
RUN --mount=type=bind,from=project-swift-discovery,source=%s,target=%s ["/usr/bin/busybox", "sh", "%s", "%s"]

COPY Package.swift Package.resolved /opt/freeside/project-seed/
COPY recipe.json %s
COPY prepare %s

LABEL org.opencontainers.image.title="freeside-project-image" \
	ai.freeside.base.digest="${BASE_DIGEST}" \
	ai.freeside.project.toolchain.swift.version="${PROJECT_SWIFT_VERSION}" \
	ai.freeside.project.toolchain.swift.archive-sha256="${PROJECT_SWIFT_SHA256}" \
	ai.freeside.project.toolchain.swift.apt-snapshot="${PROJECT_SWIFT_APT_SNAPSHOT}" \
	ai.freeside.project.toolchain.swift.apt-packages="${PROJECT_SWIFT_APT_PACKAGES}" \
	ai.freeside.project.repository="${PROJECT_REPOSITORY}" \
	ai.freeside.project.repository-id="${PROJECT_REPOSITORY_ID}" \
	ai.freeside.project.commit="${PROJECT_COMMIT}" \
	ai.freeside.project.recipe-digest="${PROJECT_RECIPE_DIGEST}"
`, request.Repository, request.CommitSHA,
		nodeToolchainBaseImage, swiftToolchainVersion, swiftToolchainArchiveSHA256,
		swiftAptSnapshot, packages,
		swiftAptInstall("ca-certificates curl"),
		swiftToolchainArchivePath,
		swiftAptInstall("${PROJECT_SWIFT_APT_PACKAGES}"),
		swiftLauncherPath, swiftLauncherPath,
		swiftMirrorBakePath, swiftMirrorDiscoveryPath, swiftMirrorDiscoveryPath,
		swiftMirrorBakePath, swiftMirrorURLsPath, swiftMirrorListMount,
		swiftMirrorBakePath, swiftMirrorListMount,
		ward.ProjectRecipePath, PreparationPath) +
		"# Bound recipe digest: " + string(recipeDigest) + "\n"
}

// stageSwiftInputs refuses a Swift package no project image supports and
// copies its Package.swift and Package.resolved into the build context.
// packageDir has passed domain.ValidProjectSwiftPackageDir.
func stageSwiftInputs(contextDir, sourceDir, packageDir string) error {
	pkg := sourceDir
	if packageDir != "" {
		// Each segment must be a real directory: a symlinked segment would
		// let the bound manifests live outside the path the record names.
		for _, segment := range strings.Split(packageDir, "/") {
			pkg = filepath.Join(pkg, segment)
			info, err := os.Lstat(pkg)
			if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
				return fmt.Errorf("swift package directory %s is not a directory at the commit", packageDir)
			} else if err != nil {
				return fmt.Errorf("inspect swift package directory %s: %w", packageDir, err)
			}
		}
	}
	for _, name := range swiftUnsupportedEntries(packageDir) {
		present, err := entryExists(filepath.Join(sourceDir, filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("inspect unsupported Swift input %s: %w", name, err)
		}
		if present {
			return fmt.Errorf("source contains unsupported Swift input %s", name)
		}
	}
	entries, err := os.ReadDir(pkg)
	if err != nil {
		return fmt.Errorf("list swift package directory: %w", err)
	}
	for _, entry := range entries {
		if isSwiftVersionSpecificManifest(entry.Name()) {
			return fmt.Errorf("source contains unsupported Swift input %s", path.Join(packageDir, entry.Name()))
		}
	}
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		return fmt.Errorf("create image build context: %w", err)
	}
	for _, name := range []string{"Package.swift", "Package.resolved"} {
		if err := copyRegularFile(
			filepath.Join(pkg, name), filepath.Join(contextDir, name), maxManifestBytes,
		); err != nil {
			return fmt.Errorf("copy %s from exact commit: %w", path.Join(packageDir, name), err)
		}
	}
	resolved, err := os.ReadFile(filepath.Join(contextDir, "Package.resolved")) //nolint:gosec // G304: fixed manifest name inside the private build context
	if err != nil {
		return fmt.Errorf("read baked Package.resolved: %w", err)
	}
	if err := checkSwiftPackageResolved(resolved); err != nil {
		return fmt.Errorf("%s: %w", path.Join(packageDir, "Package.resolved"), err)
	}
	// Executable by design: the build runs these fixed, builder-owned
	// scripts, the discovery in a stage the image does not ship and the bake
	// in both.
	for name, script := range map[string]string{
		"discover-swift-mirrors": swiftMirrorDiscovery, "bake-swift-mirrors": swiftMirrorBake,
	} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(script), 0o700); err != nil { //nolint:gosec // G306
			return fmt.Errorf("write Swift mirror script %s: %w", name, err)
		}
	}
	return nil
}

// swiftUnsupportedEntries lists, as repository-relative paths, the entries
// whose presence in any shape makes a Swift package unsupported: a SwiftPM
// mirrors file, which would redirect dependency fetches past the mirrors the
// record binds, and the legacy .swiftpm/config, which SwiftPM moves into
// that place when a command starts. A version-specific manifest is refused
// too, but its name is a pattern, not a fixed path; see
// isSwiftVersionSpecificManifest.
func swiftUnsupportedEntries(packageDir string) []string {
	return []string{
		path.Join(packageDir, ".swiftpm/configuration/mirrors.json"),
		path.Join(packageDir, ".swiftpm/config"),
	}
}

// isSwiftVersionSpecificManifest reports whether name, an entry of the
// package directory, is one SwiftPM may load in place of Package.swift
// (Package@swift-6.swift, Package@swift-5.9.swift). Any such name is
// refused, however its version is spelled: the record binds Package.swift.
// The build and the preparation helper refuse it; admission and the rebuild
// gate read fixed paths only, so a candidate that adds one fails in the
// room's preparation.
func isSwiftVersionSpecificManifest(name string) bool {
	return strings.HasPrefix(name, "Package@swift")
}

// swiftUnsupportedResolvedInput names the unsupported input a
// Package.resolved checkSwiftPackageResolved refuses reports.
func swiftUnsupportedResolvedInput(packageDir string) string {
	return path.Join(packageDir, "Package.resolved") + " with an unsupported pin"
}

// swiftPackageResolved is the part of a version 2 or 3 Package.resolved the
// builder reads. Version 1 nests its pins under "object" and so decodes with
// none, which the version check refuses first.
type swiftPackageResolved struct {
	Version int        `json:"version"`
	Pins    []swiftPin `json:"pins"`
}

type swiftPin struct {
	Identity string `json:"identity"`
	Kind     string `json:"kind"`
	Location string `json:"location"`
	State    struct {
		Revision string  `json:"revision"`
		Branch   *string `json:"branch"`
	} `json:"state"`
}

var gitRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkSwiftPackageResolved refuses a Package.resolved whose closure the
// baked mirrors cannot serve or the record would not bind: a version other
// than 2 or 3; a pin that is not remote source control (a registry or local
// pin); a location that is not an https URL, the only transport the build
// clones over; a pin without an exact revision; and a pin that follows a
// branch, which a later resolve may move.
func checkSwiftPackageResolved(resolved []byte) error {
	var file swiftPackageResolved
	if err := strictjson.DecodeAllowingUnknownFields(
		resolved, &file, strictjson.RejectInvalidUTF8, maxManifestBytes,
	); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if file.Version != 2 && file.Version != 3 {
		return fmt.Errorf("version %d is unsupported; only versions 2 and 3 are", file.Version)
	}
	for _, pin := range file.Pins {
		switch {
		case pin.Kind != "remoteSourceControl":
			return fmt.Errorf("pin %q has kind %q; only remoteSourceControl is supported", pin.Identity, pin.Kind)
		case !strings.HasPrefix(pin.Location, "https://") ||
			strings.ContainsFunc(pin.Location, func(r rune) bool { return r <= ' ' || r == 0x7f }):
			return fmt.Errorf("pin %q location %q is not an https URL", pin.Identity, pin.Location)
		case pin.State.Branch != nil:
			return fmt.Errorf("pin %q follows branch %q; only revision pins are supported", pin.Identity, *pin.State.Branch)
		case !gitRevisionPattern.MatchString(pin.State.Revision):
			return fmt.Errorf("pin %q has no exact revision", pin.Identity)
		}
	}
	return nil
}
