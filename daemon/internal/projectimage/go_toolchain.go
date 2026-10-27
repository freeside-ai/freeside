package projectimage

import (
	"bufio"
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"go/version"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// The Go toolchain path (plan §5.7, Toolchains). The builder pins one Go
// release and the golangci-lint release CI pins
// (.github/workflows/daemon-ci.yml); a module whose go.mod asks for anything
// newer is refused rather than letting Go fetch a toolchain.
const (
	goToolchainVersion       = "1.26.10"
	goToolchainArchiveSHA256 = "3f4227ce7b206a513e794f1393adb473a07b46e94d18bd15d1c684bbb7319797"
	goToolchainArchivePath   = "/opt/freeside/project-toolchain/go.tar.gz"

	golangciLintVersion       = "2.12.2"
	golangciLintArchiveSHA256 = "44cd40a8c76c86755375adfeea52cfd3533cb43d7bd647771e0ae065e166df3a"
	golangciLintArchivePath   = "/opt/freeside/project-toolchain/golangci-lint.tar.gz"

	goLauncherPath           = "/usr/local/bin/go"
	gofmtLauncherPath        = "/usr/local/bin/gofmt"
	golangciLintLauncherPath = "/usr/local/bin/golangci-lint"

	seedGoModPath = "/opt/freeside/project-seed/go.mod"
	seedGoSumPath = "/opt/freeside/project-seed/go.sum"

	// goModuleProxyHost is the one host a Go image's build fetches modules
	// from, with no direct fallback to a module's VCS host. The rebuild gate
	// reads it as the authority every Go module resolves from.
	goModuleProxyHost = "proxy.golang.org"
)

// The tools a Go image bakes beside Go so Freeside's own api/ and scripts/
// checks run offline (#2376): the OpenAPI linter, shellcheck, a Python for
// the scripts and their suites, and procps for the process-group checks
// BusyBox's ps cannot run (it has no -p). vacuum and shellcheck are the
// releases CI pins; Python is a python-build-standalone build, named by its
// CPython version and release tag. procps has no upstream release binary, so
// it is Debian trixie's package and the library it links, fetched from a
// fixed snapshot.debian.org timestamp so the URL never moves.
const (
	vacuumVersion       = "0.29.9"
	vacuumArchiveSHA256 = "058c377bb837faf24a940b41c15be8775f2c4770df910a71dc608fdd96c93c1b"
	vacuumArchivePath   = "/opt/freeside/project-toolchain/vacuum.tar.gz"

	shellcheckVersion       = "0.11.0"
	shellcheckArchiveSHA256 = "12b331c1d2db6b9eb13cfca64306b1b157a86eb69db83023e261eaa7e7c14588"
	shellcheckArchivePath   = "/opt/freeside/project-toolchain/shellcheck.tar.xz"

	pythonVersion       = "3.12.16+20261026"
	pythonArchiveSHA256 = "bb4b6392670c5d3f93876996fcaa19858a4a413a0c7720144c18b0d6f1f0db2d"
	pythonArchivePath   = "/opt/freeside/project-toolchain/python.tar.gz"

	procpsVersion         = "4.0.4-9"
	procpsSnapshot        = "20250730T150507Z"
	procpsArchiveSHA256   = "9afd4ad2e8da92322715114fb9555c63ce92f6c72bf72867e76ab2d6a7a3b541"
	procpsArchivePath     = "/opt/freeside/project-toolchain/procps.deb"
	libproc2ArchiveSHA256 = "e81069b58fe82223c71fcd74c760cb0fd3b0e61792fbaf2e5675fd948dc16dba"
	libproc2ArchivePath   = "/opt/freeside/project-toolchain/libproc2.deb"

	vacuumLauncherPath     = "/usr/local/bin/vacuum"
	shellcheckLauncherPath = "/usr/local/bin/shellcheck"
	pythonLauncherPath     = "/usr/bin/python3"
	psLauncherPath         = "/usr/bin/ps"
)

// goLauncherPaths is every path a Go image installs goToolchainLauncher at.
var goLauncherPaths = []string{
	goLauncherPath, gofmtLauncherPath, golangciLintLauncherPath,
	vacuumLauncherPath, shellcheckLauncherPath, pythonLauncherPath, psLauncherPath,
}

// goToolchainLauncher is installed at every Go image launcher path and
// dispatches on the name it was run as.
//
// For go, gofmt, and golangci-lint it works like the Node launcher: it
// extracts the verified archives once per process tree into a private
// directory, and every descendant reuses that tree through
// FREESIDE_PROJECT_GO_ROOT. It owns the Go environment of a project room: the
// room passes only HOME and LC_ALL, and the image may set no ENV.
// GOTOOLCHAIN=local forbids Go from fetching a toolchain, GOPROXY=off makes a
// missing module an immediate offline refusal instead of a network attempt,
// GOWORK=off ignores any go.work, and GOMODCACHE is the baked module cache.
// GOCACHE keeps a caller's value and otherwise lives under HOME, so build
// output never lands in the image's own paths. The umask is left alone: the
// extraction directory is already private, and a recipe's tests may assert
// the modes they create.
//
// vacuum, shellcheck, python3, and ps instead extract their own archives once
// per room into a fixed /tmp tree and exec the tool, so it keeps the
// launcher's PID: scripts/test-check.sh runs python3 to setsid and then
// signals the group named by the PID it started. Concurrent first calls each
// extract into a staging directory and rename it into place; the rename
// fails onto a populated tree, and the loser discards its copy. A per-tree
// root would need the launcher to outlive the tool to remove it, and
// scripts call python3 many times, each extraction costing about a second.
const goToolchainLauncher = `#!/usr/bin/busybox sh
set -eu
case "${0##*/}" in
go | gofmt | golangci-lint) family=go ;;
vacuum) family=vacuum ;;
shellcheck) family=shellcheck ;;
python3) family=python ;;
ps) family=procps ;;
*)
	echo "unknown project Go launcher name: ${0##*/}" >&2
	exit 127
	;;
esac
if [ "$family" != go ]; then
	toolchain=/tmp/freeside-project-$family
	complete() {
		case "$family" in
		vacuum) [ -x "$toolchain/vacuum" ] ;;
		shellcheck) [ -x "$toolchain/shellcheck" ] ;;
		python) [ -x "$toolchain/bin/python3" ] ;;
		procps) [ -x "$toolchain/usr/bin/ps" ] && [ -f "$toolchain/usr/lib/aarch64-linux-gnu/libproc2.so.0" ] ;;
		esac
	}
	if ! complete; then
		staging=$(/usr/bin/busybox mktemp -d "/tmp/freeside-project-$family.XXXXXX")
		case "$family" in
		vacuum)
			/usr/bin/busybox gzip -dc /opt/freeside/project-toolchain/vacuum.tar.gz |
				/usr/bin/busybox tar -xf - -C "$staging" vacuum
			;;
		shellcheck)
			/usr/bin/busybox xz -dc /opt/freeside/project-toolchain/shellcheck.tar.xz |
				/usr/bin/busybox tar -xf - -C "$staging" --strip-components=1
			;;
		python)
			/usr/bin/busybox gzip -dc /opt/freeside/project-toolchain/python.tar.gz |
				/usr/bin/busybox tar -xf - -C "$staging" --strip-components=1
			;;
		procps)
			/usr/bin/busybox dpkg-deb -x /opt/freeside/project-toolchain/procps.deb "$staging"
			/usr/bin/busybox dpkg-deb -x /opt/freeside/project-toolchain/libproc2.deb "$staging"
			;;
		esac
		/usr/bin/busybox mv -T "$staging" "$toolchain" 2>/dev/null ||
			/usr/bin/busybox rm -rf "$staging"
		if ! complete; then
			echo "project $family root is incomplete" >&2
			exit 126
		fi
	fi
	case "$family" in
	vacuum) exec "$toolchain/vacuum" "$@" ;;
	shellcheck) exec "$toolchain/shellcheck" "$@" ;;
	python) exec "$toolchain/bin/python3" "$@" ;;
	procps)
		export LD_LIBRARY_PATH="$toolchain/usr/lib/aarch64-linux-gnu"
		exec "$toolchain/usr/bin/ps" "$@"
		;;
	esac
fi
owned_toolchain=false
if [ -n "${FREESIDE_PROJECT_GO_ROOT:-}" ]; then
	toolchain=$FREESIDE_PROJECT_GO_ROOT
	case "$toolchain" in
	/tmp/freeside-project-go.*) ;;
	*)
		echo "invalid inherited project Go root" >&2
		exit 126
		;;
	esac
	if [ ! -x "$toolchain/go/bin/go" ] || [ ! -x "$toolchain/golangci-lint/golangci-lint" ]; then
		echo "inherited project Go root is incomplete" >&2
		exit 126
	fi
else
	toolchain=$(/usr/bin/busybox mktemp -d /tmp/freeside-project-go.XXXXXX)
	owned_toolchain=true
	export FREESIDE_PROJECT_GO_ROOT=$toolchain
	trap '/usr/bin/busybox rm -rf "$toolchain"' EXIT
	/usr/bin/busybox mkdir "$toolchain/golangci-lint"
	/usr/bin/busybox gzip -dc /opt/freeside/project-toolchain/go.tar.gz |
		/usr/bin/busybox tar -xf - -C "$toolchain"
	/usr/bin/busybox gzip -dc /opt/freeside/project-toolchain/golangci-lint.tar.gz |
		/usr/bin/busybox tar -xf - -C "$toolchain/golangci-lint" --strip-components=1
fi
export GOTOOLCHAIN=local GOPROXY=off GOWORK=off GOMODCACHE=/opt/freeside/go-mod-cache
export GOCACHE="${GOCACHE:-${HOME:-/tmp}/.cache/go-build}"
case "${0##*/}" in
go)
	set -- "$toolchain/go/bin/go" "$@"
	;;
gofmt)
	set -- "$toolchain/go/bin/gofmt" "$@"
	;;
golangci-lint)
	set -- "$toolchain/golangci-lint/golangci-lint" "$@"
	;;
esac
if [ "$owned_toolchain" = false ]; then
	exec "$@"
fi
"$@"
`

// goPrepareScript renders the Go preparation helper for one module
// directory. It holds every fresh workspace to the inputs the image baked,
// the same way the npm helper does, and then proves the baked module cache
// serves the module offline. moduleDir has passed
// domain.ValidProjectGoModuleDir, whose grammar admits no quote or shell
// metacharacter, so single quotes carry it and the paths built from it
// verbatim.
func goPrepareScript(moduleDir string) string {
	unsupported := goUnsupportedEntries(moduleDir)
	for index, entry := range unsupported {
		unsupported[index] = "'" + entry + "'"
	}
	return `#!/usr/bin/busybox sh
set -eu
module_dir='` + cmp.Or(moduleDir, ".") + `'
if ! /usr/bin/busybox cmp -s "$module_dir/go.mod" /opt/freeside/project-seed/go.mod ||
	! /usr/bin/busybox cmp -s "$module_dir/go.sum" /opt/freeside/project-seed/go.sum; then
	echo "project Go module manifests differ from the baked image inputs" >&2
	exit 42
fi
for entry in ` + strings.Join(unsupported, " ") + `; do
	if [ -e "$entry" ] || [ -L "$entry" ]; then
		echo "project contributes unsupported Go input $entry" >&2
		exit 42
	fi
done
cd "$module_dir"
exec go mod download
`
}

// goMaskedProbe is the cache-masked negative probe for a Go image: the
// module download the preparation helper runs, from the module directory,
// with the module cache masked. go -C changes directory before anything
// else, so the probe never depends on the room's working directory.
func goMaskedProbe(moduleDir string) []string {
	if moduleDir == "" {
		return []string{"go", "mod", "download"}
	}
	return []string{"go", "-C", moduleDir, "mod", "download"}
}

func renderGoContainerfile(request Request, recipeDigest domain.Digest) string {
	var launcherCopies strings.Builder
	for _, launcher := range goLauncherPaths {
		launcherCopies.WriteString("COPY toolchain-launcher " + launcher + "\n")
	}
	// Every interpolated value is a builder constant or has passed
	// validateRequest's restricted grammar. The module manifests and exact
	// recipe bytes enter through COPY, never through instruction text.
	return fmt.Sprintf(`# Generated runtime artifact for %s at %s.
# It deliberately contributes no ENV, WORKDIR, ENTRYPOINT, CMD, USER, or
# VOLUME: project images pass the same ward realized-shape gate as agent bases.
ARG BASE_IMAGE
ARG PROJECT_TOOLCHAIN_BASE_IMAGE=%s
ARG PROJECT_GO_VERSION=%s
ARG PROJECT_GO_SHA256=%s
ARG PROJECT_GOLANGCI_LINT_VERSION=%s
ARG PROJECT_GOLANGCI_LINT_SHA256=%s
ARG PROJECT_VACUUM_VERSION=%s
ARG PROJECT_VACUUM_SHA256=%s
ARG PROJECT_SHELLCHECK_VERSION=%s
ARG PROJECT_SHELLCHECK_SHA256=%s
ARG PROJECT_PYTHON_VERSION=%s
ARG PROJECT_PYTHON_SHA256=%s
ARG PROJECT_PROCPS_VERSION=%s
ARG PROJECT_PROCPS_SNAPSHOT=%s
ARG PROJECT_PROCPS_SHA256=%s
ARG PROJECT_LIBPROC2_SHA256=%s

# Verification toolchains belong to the project image, not the provider base.
# This stage downloads and verifies every pinned archive independently and
# proves each runs. The final image carries those compressed bytes plus a
# fixed launcher, so host-side provenance can hash the runtime source instead
# of trusting an extracted tree.
FROM ${PROJECT_TOOLCHAIN_BASE_IMAGE} AS project-toolchain

ARG PROJECT_GO_VERSION
ARG PROJECT_GO_SHA256
ARG PROJECT_GOLANGCI_LINT_VERSION
ARG PROJECT_GOLANGCI_LINT_SHA256
ARG PROJECT_VACUUM_VERSION
ARG PROJECT_VACUUM_SHA256
ARG PROJECT_SHELLCHECK_VERSION
ARG PROJECT_SHELLCHECK_SHA256
ARG PROJECT_PYTHON_VERSION
ARG PROJECT_PYTHON_SHA256
ARG PROJECT_PROCPS_VERSION
ARG PROJECT_PROCPS_SNAPSHOT
ARG PROJECT_PROCPS_SHA256
ARG PROJECT_LIBPROC2_SHA256

RUN set -eux; \
	apt-get update; \
	apt-get install -y --no-install-recommends ca-certificates curl xz-utils; \
	rm -rf /var/lib/apt/lists/*

RUN set -eux; \
	mkdir -p /opt/freeside/project-toolchain/go-root /opt/freeside/project-toolchain/golangci-lint-root; \
	curl -fsSLo /opt/freeside/project-toolchain/go.tar.gz \
		"https://go.dev/dl/go${PROJECT_GO_VERSION}.linux-arm64.tar.gz"; \
	echo "${PROJECT_GO_SHA256}  /opt/freeside/project-toolchain/go.tar.gz" | sha256sum -c -; \
	tar -xzf /opt/freeside/project-toolchain/go.tar.gz \
		-C /opt/freeside/project-toolchain/go-root --strip-components=1; \
	test "$(GOTOOLCHAIN=local /opt/freeside/project-toolchain/go-root/bin/go env GOVERSION)" = \
		"go${PROJECT_GO_VERSION}"; \
	curl -fsSLo /opt/freeside/project-toolchain/golangci-lint.tar.gz \
		"https://github.com/golangci/golangci-lint/releases/download/v${PROJECT_GOLANGCI_LINT_VERSION}/golangci-lint-${PROJECT_GOLANGCI_LINT_VERSION}-linux-arm64.tar.gz"; \
	echo "${PROJECT_GOLANGCI_LINT_SHA256}  /opt/freeside/project-toolchain/golangci-lint.tar.gz" | sha256sum -c -; \
	tar -xzf /opt/freeside/project-toolchain/golangci-lint.tar.gz \
		-C /opt/freeside/project-toolchain/golangci-lint-root --strip-components=1; \
	/opt/freeside/project-toolchain/golangci-lint-root/golangci-lint --version | \
		grep -F "version ${PROJECT_GOLANGCI_LINT_VERSION} "

# The tools the scripts and API checks run. The python-build-standalone asset
# is named by the full version, whose part after + is the release tag; ps
# runs against the Debian library it links, as the launcher runs it.
RUN set -eux; \
	cd /opt/freeside/project-toolchain; \
	mkdir vacuum-root shellcheck-root python-root procps-root; \
	curl -fsSLo vacuum.tar.gz \
		"https://github.com/daveshanley/vacuum/releases/download/v${PROJECT_VACUUM_VERSION}/vacuum_${PROJECT_VACUUM_VERSION}_linux_arm64.tar.gz"; \
	echo "${PROJECT_VACUUM_SHA256}  vacuum.tar.gz" | sha256sum -c -; \
	tar -xzf vacuum.tar.gz -C vacuum-root vacuum; \
	test "$(vacuum-root/vacuum version)" = "${PROJECT_VACUUM_VERSION}"; \
	curl -fsSLo shellcheck.tar.xz \
		"https://github.com/koalaman/shellcheck/releases/download/v${PROJECT_SHELLCHECK_VERSION}/shellcheck-v${PROJECT_SHELLCHECK_VERSION}.linux.aarch64.tar.xz"; \
	echo "${PROJECT_SHELLCHECK_SHA256}  shellcheck.tar.xz" | sha256sum -c -; \
	tar -xJf shellcheck.tar.xz -C shellcheck-root --strip-components=1; \
	shellcheck-root/shellcheck --version | grep -Fx "version: ${PROJECT_SHELLCHECK_VERSION}"; \
	curl -fsSLo python.tar.gz \
		"https://github.com/astral-sh/python-build-standalone/releases/download/${PROJECT_PYTHON_VERSION#*+}/cpython-${PROJECT_PYTHON_VERSION}-aarch64-unknown-linux-gnu-install_only_stripped.tar.gz"; \
	echo "${PROJECT_PYTHON_SHA256}  python.tar.gz" | sha256sum -c -; \
	tar -xzf python.tar.gz -C python-root --strip-components=1; \
	test "$(python-root/bin/python3 --version)" = "Python ${PROJECT_PYTHON_VERSION%%+*}"; \
	curl -fsSLo procps.deb \
		"https://snapshot.debian.org/archive/debian/${PROJECT_PROCPS_SNAPSHOT}/pool/main/p/procps/procps_${PROJECT_PROCPS_VERSION}_arm64.deb"; \
	echo "${PROJECT_PROCPS_SHA256}  procps.deb" | sha256sum -c -; \
	curl -fsSLo libproc2.deb \
		"https://snapshot.debian.org/archive/debian/${PROJECT_PROCPS_SNAPSHOT}/pool/main/p/procps/libproc2-0_${PROJECT_PROCPS_VERSION}_arm64.deb"; \
	echo "${PROJECT_LIBPROC2_SHA256}  libproc2.deb" | sha256sum -c -; \
	dpkg-deb -x procps.deb procps-root; \
	dpkg-deb -x libproc2.deb procps-root; \
	test "$(LD_LIBRARY_PATH=procps-root/usr/lib/aarch64-linux-gnu procps-root/usr/bin/ps --version)" = \
		"ps from procps-ng ${PROJECT_PROCPS_VERSION%%-*}"

FROM ${BASE_IMAGE}

ARG BASE_DIGEST
ARG PROJECT_REPOSITORY
ARG PROJECT_REPOSITORY_ID
ARG PROJECT_COMMIT
ARG PROJECT_RECIPE_DIGEST
ARG PROJECT_GO_VERSION
ARG PROJECT_GO_SHA256
ARG PROJECT_GOLANGCI_LINT_VERSION
ARG PROJECT_GOLANGCI_LINT_SHA256
ARG PROJECT_VACUUM_VERSION
ARG PROJECT_VACUUM_SHA256
ARG PROJECT_SHELLCHECK_VERSION
ARG PROJECT_SHELLCHECK_SHA256
ARG PROJECT_PYTHON_VERSION
ARG PROJECT_PYTHON_SHA256
ARG PROJECT_PROCPS_VERSION
ARG PROJECT_PROCPS_SHA256
ARG PROJECT_LIBPROC2_SHA256

COPY --from=project-toolchain /opt/freeside/project-toolchain/go.tar.gz %s
COPY --from=project-toolchain /opt/freeside/project-toolchain/golangci-lint.tar.gz %s
COPY --from=project-toolchain /opt/freeside/project-toolchain/vacuum.tar.gz %s
COPY --from=project-toolchain /opt/freeside/project-toolchain/shellcheck.tar.xz %s
COPY --from=project-toolchain /opt/freeside/project-toolchain/python.tar.gz %s
COPY --from=project-toolchain /opt/freeside/project-toolchain/procps.deb %s
COPY --from=project-toolchain /opt/freeside/project-toolchain/libproc2.deb %s
# Clear the launcher paths with the base's own already-trusted static BusyBox
# before each COPY, so COPY lands a fresh regular file and never writes
# through a symlink an approved base may carry there (the Node path's #518).
RUN ["/usr/bin/busybox", "rm", "-f", %s]
%s
RUN set -eux; \
	test "$(go env GOVERSION)" = "go${PROJECT_GO_VERSION}"; \
	test "$(go env GOTOOLCHAIN GOPROXY GOWORK GOMODCACHE)" = "$(printf '%%s\n' local off off %s)"; \
	golangci-lint --version | grep -F "version ${PROJECT_GOLANGCI_LINT_VERSION} "; \
	test "$(vacuum version)" = "${PROJECT_VACUUM_VERSION}"; \
	shellcheck --version | grep -Fx "version: ${PROJECT_SHELLCHECK_VERSION}"; \
	test "$(python3 --version)" = "Python ${PROJECT_PYTHON_VERSION%%+*}"; \
	test "$(ps --version)" = "ps from procps-ng ${PROJECT_PROCPS_VERSION%%-*}"; \
	mkdir -p %s /opt/freeside/project-seed; \
	rm -rf /tmp/freeside-project-*

COPY go.mod go.sum /opt/freeside/project-seed/
COPY recipe.json %s
COPY prepare %s

# Cache warming builds and runs nothing from the repository: it downloads the
# module closure go.mod and go.sum pin into the module cache, verifying every
# module against go.sum, and anything go.sum lacks against the checksum
# database. The launcher pins GOPROXY=off for every later process, so this one
# step runs the same verified archive directly. It fetches from one proxy with
# no direct fallback, so no module comes from its own VCS host: GOENV=off
# drops any go env file the base carries, which an empty variable would not
# override, and the private and insecure lists are cleared. The fresh
# verification workspace hydrates offline from this cache through the
# image-owned preparation helper.
RUN set -eux; \
	warm=$(mktemp -d /tmp/freeside-go-warm.XXXXXX); \
	tar -xzf %s -C "$warm"; \
	cd /opt/freeside/project-seed; \
	env GOENV=off GOTOOLCHAIN=local GOWORK=off GOFLAGS= GOPROXY=https://%s \
		GONOPROXY= GOPRIVATE= GOINSECURE= GONOSUMDB= GOSUMDB=sum.golang.org \
		GOMODCACHE=%s GOCACHE="$warm/cache" \
		"$warm/go/bin/go" mod download; \
	rm -rf "$warm"

LABEL org.opencontainers.image.title="freeside-project-image" \
	ai.freeside.base.digest="${BASE_DIGEST}" \
	ai.freeside.project.toolchain.go.version="${PROJECT_GO_VERSION}" \
	ai.freeside.project.toolchain.go.archive-sha256="${PROJECT_GO_SHA256}" \
	ai.freeside.project.toolchain.golangci-lint.version="${PROJECT_GOLANGCI_LINT_VERSION}" \
	ai.freeside.project.toolchain.golangci-lint.archive-sha256="${PROJECT_GOLANGCI_LINT_SHA256}" \
	ai.freeside.project.toolchain.vacuum.version="${PROJECT_VACUUM_VERSION}" \
	ai.freeside.project.toolchain.vacuum.archive-sha256="${PROJECT_VACUUM_SHA256}" \
	ai.freeside.project.toolchain.shellcheck.version="${PROJECT_SHELLCHECK_VERSION}" \
	ai.freeside.project.toolchain.shellcheck.archive-sha256="${PROJECT_SHELLCHECK_SHA256}" \
	ai.freeside.project.toolchain.python.version="${PROJECT_PYTHON_VERSION}" \
	ai.freeside.project.toolchain.python.archive-sha256="${PROJECT_PYTHON_SHA256}" \
	ai.freeside.project.toolchain.procps.version="${PROJECT_PROCPS_VERSION}" \
	ai.freeside.project.toolchain.procps.archive-sha256="${PROJECT_PROCPS_SHA256}" \
	ai.freeside.project.toolchain.libproc2.version="${PROJECT_PROCPS_VERSION}" \
	ai.freeside.project.toolchain.libproc2.archive-sha256="${PROJECT_LIBPROC2_SHA256}" \
	ai.freeside.project.repository="${PROJECT_REPOSITORY}" \
	ai.freeside.project.repository-id="${PROJECT_REPOSITORY_ID}" \
	ai.freeside.project.commit="${PROJECT_COMMIT}" \
	ai.freeside.project.recipe-digest="${PROJECT_RECIPE_DIGEST}"
`, request.Repository, request.CommitSHA,
		nodeToolchainBaseImage, goToolchainVersion, goToolchainArchiveSHA256,
		golangciLintVersion, golangciLintArchiveSHA256,
		vacuumVersion, vacuumArchiveSHA256, shellcheckVersion, shellcheckArchiveSHA256,
		pythonVersion, pythonArchiveSHA256,
		procpsVersion, procpsSnapshot, procpsArchiveSHA256, libproc2ArchiveSHA256,
		goToolchainArchivePath, golangciLintArchivePath,
		vacuumArchivePath, shellcheckArchivePath, pythonArchivePath,
		procpsArchivePath, libproc2ArchivePath,
		`"`+strings.Join(goLauncherPaths, `", "`)+`"`,
		launcherCopies.String(),
		GoModuleCachePath, GoModuleCachePath,
		ward.ProjectRecipePath, PreparationPath,
		goToolchainArchivePath, goModuleProxyHost, GoModuleCachePath) +
		"# Bound recipe digest: " + string(recipeDigest) + "\n"
}

// stageGoInputs refuses a Go module no project image supports and copies
// its go.mod and go.sum into the build context. moduleDir has passed
// domain.ValidProjectGoModuleDir.
func stageGoInputs(contextDir, sourceDir, moduleDir string) error {
	module := sourceDir
	if moduleDir != "" {
		// Each segment must be a real directory: a symlinked segment would
		// let the bound manifests live outside the path the record names.
		for _, segment := range strings.Split(moduleDir, "/") {
			module = filepath.Join(module, segment)
			info, err := os.Lstat(module)
			if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
				return fmt.Errorf("go module directory %s is not a directory at the commit", moduleDir)
			} else if err != nil {
				return fmt.Errorf("inspect go module directory %s: %w", moduleDir, err)
			}
		}
	}
	for _, name := range goUnsupportedEntries(moduleDir) {
		present, err := entryExists(filepath.Join(sourceDir, filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("inspect unsupported Go input %s: %w", name, err)
		}
		if present {
			return fmt.Errorf("source contains unsupported Go input %s", name)
		}
	}
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		return fmt.Errorf("create image build context: %w", err)
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		if err := copyRegularFile(
			filepath.Join(module, name), filepath.Join(contextDir, name), maxManifestBytes,
		); err != nil {
			return fmt.Errorf("copy %s from exact commit: %w", path.Join(moduleDir, name), err)
		}
	}
	goMod, err := os.ReadFile(filepath.Join(contextDir, "go.mod")) //nolint:gosec // G304: fixed manifest name inside the private build context
	if err != nil {
		return fmt.Errorf("read baked go.mod: %w", err)
	}
	if err := checkGoModule(goMod); err != nil {
		return fmt.Errorf("%s: %w", path.Join(moduleDir, "go.mod"), err)
	}
	return nil
}

// goUnsupportedEntries lists, as repository-relative paths, the entries
// whose presence in any shape makes a Go module unsupported: the module's
// vendor directory, which would replace the module cache the record binds,
// and a go.work in the module directory or any directory above it up to
// the root, since no image supports a workspace.
func goUnsupportedEntries(moduleDir string) []string {
	entries := []string{path.Join(moduleDir, "vendor")}
	for dir := moduleDir; ; dir = path.Dir(dir) {
		if dir == "." {
			dir = ""
		}
		entries = append(entries, path.Join(dir, "go.work"))
		if dir == "" {
			return entries
		}
	}
}

// goLocalReplaceInput names the unsupported input a go.mod holding a replace
// whose target is a filesystem path reports.
func goLocalReplaceInput(moduleDir string) string {
	return path.Join(moduleDir, "go.mod") + " replace with a local path"
}

// goModHasLocalReplace reports whether goMod holds a replace whose target is
// a filesystem path. A go.mod this reader cannot parse reports false: its
// hash differs from any baked one, which refuses it on its own.
func goModHasLocalReplace(goMod []byte) bool {
	directives, err := goModDirectives(goMod)
	if err != nil {
		return false
	}
	for _, directive := range directives {
		if directive.verb != "replace" {
			continue
		}
		for index, arg := range directive.args {
			if arg == "=>" && index+1 < len(directive.args) &&
				goReplacementIsLocalPath(directive.args[index+1]) {
				return true
			}
		}
	}
	return false
}

// checkGoModule refuses a go.mod the baked toolchain cannot serve offline or
// whose inputs the image record would not bind:
//
//   - A go directive above the baked Go version: GOTOOLCHAIN=local refuses
//     it at run time.
//   - A toolchain directive naming any version but the baked one. Under
//     GOTOOLCHAIN=local Go would ignore it, but the owner's pin (#2115) is
//     that a module declaring another toolchain is not one the image serves.
//   - A replace whose target is a filesystem path. One outside the module
//     would make the image depend on files neither go.mod nor go.sum binds,
//     and one inside cannot warm the cache, which is filled from a seed
//     holding only go.mod and go.sum.
//
// It reads only the go, toolchain, and replace directives, in their line and
// block forms; anything else is left to Go, which reads the same file inside
// the image.
func checkGoModule(goMod []byte) error {
	directives, err := goModDirectives(goMod)
	if err != nil {
		return err
	}
	for _, directive := range directives {
		switch directive.verb {
		case "go":
			if len(directive.args) != 1 || !version.IsValid("go"+directive.args[0]) {
				return fmt.Errorf("go.mod line %d: malformed go directive", directive.line)
			}
			if version.Compare("go"+directive.args[0], "go"+goToolchainVersion) > 0 {
				return fmt.Errorf("go.mod requires go %s, above the baked Go %s",
					directive.args[0], goToolchainVersion)
			}
		case "toolchain":
			if len(directive.args) != 1 || directive.args[0] != "go"+goToolchainVersion {
				return fmt.Errorf("go.mod line %d: toolchain directive %q names a toolchain other than the baked go%s",
					directive.line, strings.Join(directive.args, " "), goToolchainVersion)
			}
		case "replace":
			arrow := -1
			for index, arg := range directive.args {
				if arg == "=>" {
					arrow = index
				}
			}
			if arrow < 0 || arrow == len(directive.args)-1 {
				return fmt.Errorf("go.mod line %d: malformed replace directive", directive.line)
			}
			if target := directive.args[arrow+1]; goReplacementIsLocalPath(target) {
				return fmt.Errorf("go.mod line %d: replace target %q is a local path, which no project image serves",
					directive.line, target)
			}
		}
	}
	return nil
}

// goReplacementIsLocalPath reports whether a replace target is a filesystem
// path, by Go's rule: absolute, or ".", "..", or starting "./" or "../". A
// module-path target is not one.
func goReplacementIsLocalPath(target string) bool {
	return strings.HasPrefix(target, "/") || strings.HasPrefix(target, "./") ||
		strings.HasPrefix(target, "../") || target == "." || target == ".."
}

type goModDirective struct {
	line int
	verb string
	args []string
}

// goModDirectives splits go.mod into directives, expanding "verb ( ... )"
// blocks into one directive per line. Comments are dropped and quoted
// arguments unquoted.
func goModDirectives(goMod []byte) ([]goModDirective, error) {
	var (
		directives []goModDirective
		block      string
	)
	scanner := bufio.NewScanner(bytes.NewReader(goMod))
	scanner.Buffer(make([]byte, 0, 64<<10), maxManifestBytes)
	for line := 1; scanner.Scan(); line++ {
		fields, err := goModFields(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("go.mod line %d: %w", line, err)
		}
		switch {
		case len(fields) == 0:
		case block != "" && len(fields) == 1 && fields[0] == ")":
			block = ""
		case block != "":
			directives = append(directives, goModDirective{line: line, verb: block, args: fields})
		case len(fields) == 2 && fields[1] == "(":
			block = fields[0]
		default:
			directives = append(directives, goModDirective{line: line, verb: fields[0], args: fields[1:]})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read go.mod: %w", err)
	}
	if block != "" {
		return nil, fmt.Errorf("go.mod: unterminated %s block", block)
	}
	return directives, nil
}

// goModFields tokenizes one go.mod line as Go's own lexer does: words end at
// whitespace, a parenthesis, or a comment; "(" and ")" are words of their
// own, so "replace(" opens a block; Go interpreted or raw string literals are
// one unquoted word; and "//" starts a comment outside a literal.
func goModFields(line string) ([]string, error) {
	var fields []string
	for {
		line = strings.TrimLeft(line, " \t\r")
		switch {
		case line == "" || strings.HasPrefix(line, "//"):
			return fields, nil
		case line[0] == '(' || line[0] == ')':
			fields = append(fields, line[:1])
			line = line[1:]
		case line[0] == '"' || line[0] == '`':
			quoted, err := strconv.QuotedPrefix(line)
			if err != nil {
				return nil, fmt.Errorf("malformed quoted string")
			}
			value, err := strconv.Unquote(quoted)
			if err != nil {
				return nil, fmt.Errorf("malformed quoted string")
			}
			fields = append(fields, value)
			line = line[len(quoted):]
		default:
			end := strings.IndexAny(line, " \t\r()")
			if comment := strings.Index(line, "//"); comment >= 0 && (end < 0 || comment < end) {
				end = comment
			}
			if end < 0 {
				end = len(line)
			}
			fields = append(fields, line[:end])
			line = line[end:]
		}
	}
}
