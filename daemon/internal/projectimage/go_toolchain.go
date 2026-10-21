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
)

// goToolchainLauncher is installed at goLauncherPath, gofmtLauncherPath, and
// golangciLintLauncherPath and dispatches on the name it was run as. Like the
// Node launcher, it extracts the verified archives once per process tree into
// a private directory, and every descendant reuses that tree through
// FREESIDE_PROJECT_GO_ROOT.
//
// It owns the Go environment of a project room: the room passes only HOME and
// LC_ALL, and the image may set no ENV. GOTOOLCHAIN=local forbids Go from
// fetching a toolchain, GOPROXY=off makes a missing module an immediate
// offline refusal instead of a network attempt, GOWORK=off ignores any
// go.work, and GOMODCACHE is the baked module cache. GOCACHE keeps a caller's
// value and otherwise lives under HOME, so build output never lands in the
// image's own paths. The umask is left alone: the extraction directory is
// already private, and a recipe's tests may assert the modes they create.
const goToolchainLauncher = `#!/usr/bin/busybox sh
set -eu
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
*)
	echo "unknown project Go launcher name: ${0##*/}" >&2
	exit 127
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

# Verification toolchains belong to the project image, not the provider base.
# This stage downloads and verifies the exact Go and golangci-lint archives
# independently and proves each runs. The final image carries those
# compressed bytes plus a fixed launcher, so host-side provenance can hash the
# runtime source instead of trusting an extracted tree.
FROM ${PROJECT_TOOLCHAIN_BASE_IMAGE} AS project-toolchain

ARG PROJECT_GO_VERSION
ARG PROJECT_GO_SHA256
ARG PROJECT_GOLANGCI_LINT_VERSION
ARG PROJECT_GOLANGCI_LINT_SHA256

RUN set -eux; \
	apt-get update; \
	apt-get install -y --no-install-recommends ca-certificates curl; \
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

COPY --from=project-toolchain /opt/freeside/project-toolchain/go.tar.gz %s
COPY --from=project-toolchain /opt/freeside/project-toolchain/golangci-lint.tar.gz %s
# Clear the launcher paths with the base's own already-trusted static BusyBox
# before each COPY, so COPY lands a fresh regular file and never writes
# through a symlink an approved base may carry there (the Node path's #518).
RUN ["/usr/bin/busybox", "rm", "-f", "%s", "%s", "%s"]
COPY toolchain-launcher %s
COPY toolchain-launcher %s
COPY toolchain-launcher %s

RUN set -eux; \
	test "$(go env GOVERSION)" = "go${PROJECT_GO_VERSION}"; \
	test "$(go env GOTOOLCHAIN GOPROXY GOWORK GOMODCACHE)" = "$(printf '%%s\n' local off off %s)"; \
	golangci-lint --version | grep -F "version ${PROJECT_GOLANGCI_LINT_VERSION} "; \
	mkdir -p %s /opt/freeside/project-seed; \
	rm -rf /tmp/freeside-project-go.*

COPY go.mod go.sum /opt/freeside/project-seed/
COPY recipe.json %s
COPY prepare %s

# Cache warming builds and runs nothing from the repository: it downloads the
# module closure go.mod and go.sum pin into the module cache, verifying every
# module against go.sum. The launcher pins GOPROXY=off for every later
# process, so this one step runs the same verified archive directly with the
# default proxy. The fresh verification workspace hydrates offline from this
# cache through the image-owned preparation helper.
RUN set -eux; \
	warm=$(mktemp -d /tmp/freeside-go-warm.XXXXXX); \
	tar -xzf %s -C "$warm"; \
	cd /opt/freeside/project-seed; \
	env GOTOOLCHAIN=local GOWORK=off GOFLAGS= GOMODCACHE=%s GOCACHE="$warm/cache" \
		"$warm/go/bin/go" mod download; \
	rm -rf "$warm"

LABEL org.opencontainers.image.title="freeside-project-image" \
	ai.freeside.base.digest="${BASE_DIGEST}" \
	ai.freeside.project.toolchain.go.version="${PROJECT_GO_VERSION}" \
	ai.freeside.project.toolchain.go.archive-sha256="${PROJECT_GO_SHA256}" \
	ai.freeside.project.toolchain.golangci-lint.version="${PROJECT_GOLANGCI_LINT_VERSION}" \
	ai.freeside.project.toolchain.golangci-lint.archive-sha256="${PROJECT_GOLANGCI_LINT_SHA256}" \
	ai.freeside.project.repository="${PROJECT_REPOSITORY}" \
	ai.freeside.project.repository-id="${PROJECT_REPOSITORY_ID}" \
	ai.freeside.project.commit="${PROJECT_COMMIT}" \
	ai.freeside.project.recipe-digest="${PROJECT_RECIPE_DIGEST}"
`, request.Repository, request.CommitSHA,
		nodeToolchainBaseImage, goToolchainVersion, goToolchainArchiveSHA256,
		golangciLintVersion, golangciLintArchiveSHA256,
		goToolchainArchivePath, golangciLintArchivePath,
		goLauncherPath, gofmtLauncherPath, golangciLintLauncherPath,
		goLauncherPath, gofmtLauncherPath, golangciLintLauncherPath,
		GoModuleCachePath, GoModuleCachePath,
		ward.ProjectRecipePath, PreparationPath,
		goToolchainArchivePath, GoModuleCachePath) +
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
