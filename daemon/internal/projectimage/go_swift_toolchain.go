package projectimage

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// The Go and Swift toolchain path (#2881) bakes both single paths into one
// image for a repository that holds one Go module and one Swift package. It
// adds no pin, launcher, or unsupported input of its own: every part is the
// Go path's or the Swift path's, composed.

// swiftLauncherContextFile is the build-context name of the Swift launcher
// in a Go and Swift image, where toolchain-launcher holds the Go launcher.
const swiftLauncherContextFile = "swift-launcher"

// goSwiftPrepareScript renders the Go and Swift preparation helper: the Go
// helper's checks and module download, then the Swift helper's checks and
// resolve. The download runs in a subshell, so the Swift half starts from
// the workspace root as the Swift helper does.
func goSwiftPrepareScript(moduleDir, packageDir string) string {
	return "#!/usr/bin/busybox sh\nset -eu\n" + goPrepareChecks(moduleDir) +
		"(cd \"$module_dir\" && go mod download)\n" + swiftPrepareBody(packageDir)
}

// renderGoSwiftContainerfile composes the two paths. Each toolchain keeps
// its own download stage, renamed so the two do not collide, and the Swift
// runtime and discovery stages are the Swift path's. The final stage starts
// from the Swift runtime, which already holds the Swift archive, libraries,
// and launcher, and adds the Go path's archives, launchers, and module-cache
// warm, both seeds, the Swift mirrors, and both label sets.
//
// The composed stages drop the full-line comments the shared stage text
// carries, because with them the file passes containerfileByteLimit; the
// single-toolchain Containerfiles keep them, and the header and the bound
// recipe digest stay.
func renderGoSwiftContainerfile(request Request, recipeDigest domain.Digest) string {
	// Every interpolated value is a builder constant or has passed
	// validateRequest's restricted grammar. The manifests and exact recipe
	// bytes enter through COPY, never through instruction text.
	return fmt.Sprintf(`# Generated runtime artifact for %s at %s.
# It deliberately contributes no ENV, WORKDIR, ENTRYPOINT, CMD, USER, or
# VOLUME: project images pass the same ward realized-shape gate as agent bases.
ARG BASE_IMAGE
ARG PROJECT_TOOLCHAIN_BASE_IMAGE=%s
`, request.Repository, request.CommitSHA, nodeToolchainBaseImage) + withoutCommentLines(
		goToolchainArgs()+swiftToolchainArgs()+"\n"+
			goToolchainStage("project-go-toolchain")+"\n"+
			swiftToolchainStage("project-swift-toolchain")+"\n"+
			swiftRuntimeStage("project-swift-toolchain", swiftLauncherContextFile)+"\n"+
			swiftDiscoveryStage()+`
FROM project-swift-runtime

ARG BASE_DIGEST
ARG PROJECT_REPOSITORY
ARG PROJECT_REPOSITORY_ID
ARG PROJECT_COMMIT
ARG PROJECT_RECIPE_DIGEST
`+goFinalArgs+swiftFinalArgs+"\n"+
			goToolchainInstall("project-go-toolchain", "toolchain-launcher")+"\n"+
			swiftMirrorsBake()+"\n"+
			goSeedCopy+swiftSeedCopy+
			fmt.Sprintf("COPY recipe.json %s\nCOPY prepare %s\n\n", ward.ProjectRecipePath, PreparationPath)+
			goModuleWarm()+`
LABEL org.opencontainers.image.title="freeside-project-image" \
	ai.freeside.base.digest="${BASE_DIGEST}" \
`+goLabels()+swiftLabels+projectLabels) +
		"# Bound recipe digest: " + string(recipeDigest) + "\n"
}

// containerfileByteLimit is the largest Containerfile Apple container
// builds (apple/container#735); a larger one fails before any step runs.
const containerfileByteLimit = 16384

// withoutCommentLines drops every line whose first non-blank character is
// #. The build would ignore them anyway: the generated Containerfiles use no
// heredoc and no parser directive, so such a line is always a comment, even
// inside a continued RUN.
func withoutCommentLines(text string) string {
	var kept strings.Builder
	for line := range strings.Lines(text) {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			kept.WriteString(line)
		}
	}
	return kept.String()
}

// splitGoSwiftInputs partitions names, each a changed manifest or an
// unsupported input a Go and Swift layout observed, into the Go module's and
// the Swift package's, preserving order. A name is the Go module's when it is
// one of the paths the Go path itself names (its manifests, its unsupported
// entries, and its local-replace input); every other name is the Swift
// package's. The two paths name disjoint files (go.mod, go.sum, vendor, and
// go.work against Package.swift, Package.resolved, and .swiftpm), so the
// split holds even when both directories are the same.
func splitGoSwiftInputs(layout domain.ProjectImageInputLayout, names []string) (goNames, swiftNames []string) {
	goOwned := append(goUnsupportedEntries(layout.GoModuleDir),
		path.Join(layout.GoModuleDir, "go.mod"), path.Join(layout.GoModuleDir, "go.sum"),
		goLocalReplaceInput(layout.GoModuleDir))
	for _, name := range names {
		if slices.Contains(goOwned, name) {
			goNames = append(goNames, name)
		} else {
			swiftNames = append(swiftNames, name)
		}
	}
	return goNames, swiftNames
}
