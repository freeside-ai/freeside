package projectimage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// swiftShippedBakeRun is the shipped stage's one read of the discovery stage.
const swiftShippedBakeRun = "RUN --mount=type=bind,from=project-swift-discovery,source=" +
	swiftMirrorURLsPath + ",target=" + swiftMirrorListMount +
	` ["/usr/bin/busybox", "sh", "` + swiftMirrorBakePath + `", "` + swiftMirrorListMount + `"]`

const (
	testPackageSwift = "// swift-tools-version: 6.1\nimport PackageDescription\n\n" +
		"let package = Package(name: \"App\", dependencies: [\n" +
		"\t.package(url: \"https://github.com/apple/swift-collections\", from: \"1.1.0\"),\n])\n"
	testPackageResolved = `{
  "originHash" : "0d5a3d8c",
  "pins" : [
    {
      "identity" : "swift-collections",
      "kind" : "remoteSourceControl",
      "location" : "https://github.com/apple/swift-collections",
      "state" : {
        "revision" : "671108c96644956dddcd89dd59c203dcdb36cec7",
        "version" : "1.1.4"
      }
    }
  ],
  "version" : 3
}
`
)

// writeSwiftFixture writes a Swift package at packageDir under directory,
// with no package.json or go.mod anywhere in the tree.
func writeSwiftFixture(directory, packageDir string) error {
	pkg := filepath.Join(directory, filepath.FromSlash(packageDir))
	if err := os.MkdirAll(filepath.Join(pkg, "Sources", "App"), 0o700); err != nil {
		return err
	}
	for name, content := range map[string]string{
		"Package.swift": testPackageSwift, "Package.resolved": testPackageResolved,
		"Sources/App/App.swift": "public let app = 1\n",
	} {
		if err := os.WriteFile(filepath.Join(pkg, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func swiftRequest(packageDir string) Request {
	request := validRequest()
	request.SwiftPackageDir = packageDir
	return request
}

// resolvedWithPin is testPackageResolved with its one pin's body replaced.
func resolvedWithPin(pin string) string {
	return `{"pins":[` + pin + `],"version":3}`
}

func TestBuildContextPreparationBindsSwiftInputs(t *testing.T) {
	t.Parallel()
	for _, packageDir := range []string{"", "app", "apps/mac"} {
		t.Run("package="+packageDir, func(t *testing.T) {
			source := t.TempDir()
			if err := writeSwiftFixture(source, packageDir); err != nil {
				t.Fatal(err)
			}
			contextDir := filepath.Join(t.TempDir(), "context")
			layout, err := createBuildContext(contextDir, source, swiftRequest(packageDir), "sha256:recipe")
			if err != nil {
				t.Fatal(err)
			}
			want := domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainSwift, SwiftPackageDir: packageDir}
			if layout != want {
				t.Fatalf("layout = %+v, want %+v", layout, want)
			}
			read := func(name string) []byte {
				t.Helper()
				content, err := os.ReadFile(filepath.Join(contextDir, name)) //nolint:gosec // fixed file under test-owned build context
				if err != nil {
					t.Fatal(err)
				}
				return content
			}
			if string(read("Package.swift")) != testPackageSwift ||
				string(read("Package.resolved")) != testPackageResolved {
				t.Fatal("build context does not seed the package's exact Package.swift and Package.resolved")
			}
			if string(read("prepare")) != swiftPrepareScript(packageDir) {
				t.Fatal("build context preparation helper is not the Swift helper for this package")
			}
			if string(read("discover-swift-mirrors")) != swiftMirrorDiscovery ||
				string(read("bake-swift-mirrors")) != swiftMirrorBake {
				t.Fatal("build context does not carry the fixed mirror scripts")
			}
			info, err := os.Stat(filepath.Join(contextDir, "toolchain-launcher"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(read("toolchain-launcher"), []byte(swiftToolchainLauncher)) || info.Mode().Perm() != 0o755 {
				t.Fatal("generated Swift toolchain launcher differs from the fixed executable input")
			}
			containerfile := string(read("Containerfile"))
			snapshot := "http://snapshot.debian.org/archive/debian/" + swiftAptSnapshot + "/ trixie main"
			for _, binding := range []string{
				"ARG PROJECT_SWIFT_VERSION=" + swiftToolchainVersion,
				"ARG PROJECT_SWIFT_SHA256=" + swiftToolchainArchiveSHA256,
				"ARG PROJECT_SWIFT_APT_SNAPSHOT=" + swiftAptSnapshot,
				`ARG PROJECT_SWIFT_APT_PACKAGES="` + strings.Join(swiftAptPackages, " ") + `"`,
				"https://download.swift.org/swift-${PROJECT_SWIFT_VERSION}-release/debian12-aarch64/",
				`echo "${PROJECT_SWIFT_SHA256}  /opt/freeside/project-toolchain/swift.tar.gz" | sha256sum -c -`,
				// Both stages install from the snapshot alone.
				snapshot,
				"http://snapshot.debian.org/archive/debian-security/" + swiftAptSnapshot + "/ trixie-security main",
				`-o "Dir::Etc::SourceParts=$apt_dir/parts" -o Acquire::Check-Valid-Until=false`,
				"install -y --no-install-recommends ca-certificates curl;",
				"install -y --no-install-recommends ${PROJECT_SWIFT_APT_PACKAGES};",
				"COPY --from=project-toolchain /opt/freeside/project-toolchain/swift.tar.gz " + swiftToolchainArchivePath,
				`RUN ["/usr/bin/busybox", "rm", "-f", "/usr/local/bin/swift"]`,
				"COPY toolchain-launcher " + swiftLauncherPath,
				`grep -F "Swift version ${PROJECT_SWIFT_VERSION} (swift-${PROJECT_SWIFT_VERSION}-RELEASE)"`,
				"COPY Package.swift Package.resolved /opt/freeside/project-seed/",
				"COPY prepare " + PreparationPath,
				"FROM ${BASE_IMAGE} AS project-swift-runtime",
				"FROM project-swift-runtime AS project-swift-discovery",
				"COPY discover-swift-mirrors " + swiftMirrorDiscoveryPath,
				`RUN ["/usr/bin/busybox", "sh", "` + swiftMirrorDiscoveryPath + `"]`,
				"COPY bake-swift-mirrors " + swiftMirrorBakePath,
				swiftShippedBakeRun,
				"rm -rf /tmp/freeside-project-swift*",
				`ai.freeside.project.toolchain.swift.version="${PROJECT_SWIFT_VERSION}"`,
				`ai.freeside.project.toolchain.swift.archive-sha256="${PROJECT_SWIFT_SHA256}"`,
				`ai.freeside.project.toolchain.swift.apt-snapshot="${PROJECT_SWIFT_APT_SNAPSHOT}"`,
				`ai.freeside.project.toolchain.swift.apt-packages="${PROJECT_SWIFT_APT_PACKAGES}"`,
				"# Bound recipe digest: sha256:recipe",
			} {
				if !strings.Contains(containerfile, binding) {
					t.Errorf("Swift Containerfile lacks %q", binding)
				}
			}
			if got := strings.Count(containerfile, snapshot); got != 2 {
				t.Errorf("Swift Containerfile installs from the snapshot in %d stages, want 2", got)
			}
			// Discovery runs manifest code, so the stage the image ships
			// takes only the URL list from it and never runs a resolve.
			_, shipped, ok := strings.Cut(containerfile, "\nFROM project-swift-runtime\n")
			if !ok {
				t.Fatal("Swift Containerfile has no shipped stage built on the runtime")
			}
			for _, forbidden := range []string{"discover-swift-mirrors", "swift package", "\nFROM "} {
				if strings.Contains(shipped, forbidden) {
					t.Errorf("the shipped stage holds %q", forbidden)
				}
			}
			// The one line that reads the discovery stage is the bake's
			// read-only bind mount of the URL list; a COPY from it would ship
			// whatever manifest code left at that path.
			for line := range strings.SplitSeq(shipped, "\n") {
				if strings.Contains(line, "from=") && line != swiftShippedBakeRun {
					t.Errorf("the shipped stage reads another stage outside the bake's mount: %q", line)
				}
			}
			if !strings.Contains(shipped, "\n"+swiftShippedBakeRun+"\n") {
				t.Error("the shipped stage does not bake from the mounted URL list")
			}
			for _, forbidden := range []string{
				"\nENV ", "\nWORKDIR ", "\nENTRYPOINT ", "\nCMD ", "\nUSER ", "\nVOLUME ",
				"node", "npm", "GOPROXY", "deb.debian.org",
			} {
				if strings.Contains(containerfile, forbidden) {
					t.Errorf("Swift Containerfile carries %q", strings.TrimSpace(forbidden))
				}
			}
		})
	}
}

func TestBuildContextRefusesUnsupportedSwiftInputs(t *testing.T) {
	t.Parallel()
	type tree map[string]string
	pin := func(body string) tree {
		return tree{"app/Package.resolved": resolvedWithPin(body)}
	}
	cases := []struct {
		name       string
		packageDir string
		// extra edits the app-package fixture as applyTree does.
		extra   tree
		refusal string
		invalid bool
	}{
		{
			"root package.json beside a declared package", "app",
			tree{"package.json": `{"scripts":{}}`},
			"root package.json beside the declared Swift package", false,
		},
		{"missing Package.swift", "app", tree{"app/Package.swift": ""}, "copy app/Package.swift", false},
		{"missing Package.resolved", "app", tree{"app/Package.resolved": ""}, "copy app/Package.resolved", false},
		{
			"symlinked Package.resolved", "app",
			tree{"real.resolved": testPackageResolved, "app/Package.resolved#": "->../real.resolved"},
			"not a regular file", false,
		},
		{
			"mirrors file", "app",
			tree{"app/.swiftpm/configuration/mirrors.json": `{"object":[],"version":1}`},
			"unsupported Swift input app/.swiftpm/configuration/mirrors.json", false,
		},
		{
			"legacy mirrors config", "app",
			tree{"app/.swiftpm/config": `{"object":[],"version":1}`},
			"unsupported Swift input app/.swiftpm/config", false,
		},
		{
			"version-specific manifest", "app",
			tree{"app/Package@swift-6.swift": testPackageSwift},
			"unsupported Swift input app/Package@swift-6.swift", false,
		},
		{
			"mirrors file as a symlink", "app",
			tree{"app/.swiftpm/configuration/a": "a", "app/.swiftpm/configuration/mirrors.json#": "->a"},
			"unsupported Swift input app/.swiftpm/configuration/mirrors.json", false,
		},
		{
			"version 1 Package.resolved", "app",
			tree{"app/Package.resolved": `{"object":{"pins":[]},"version":1}`},
			"version 1 is unsupported", false,
		},
		{
			"branch pin", "app",
			pin(`{"identity":"x","kind":"remoteSourceControl","location":"https://example.test/x",` +
				`"state":{"branch":"main","revision":"671108c96644956dddcd89dd59c203dcdb36cec7"}}`),
			`follows branch "main"`, false,
		},
		{
			"local source control pin", "app",
			pin(`{"identity":"x","kind":"localSourceControl","location":"/src/x",` +
				`"state":{"revision":"671108c96644956dddcd89dd59c203dcdb36cec7"}}`),
			`kind "localSourceControl"`, false,
		},
		{
			"registry pin", "app",
			pin(`{"identity":"x.y","kind":"registry","location":"","state":{"version":"1.0.0"}}`),
			`kind "registry"`, false,
		},
		{
			"ssh location", "app",
			pin(`{"identity":"x","kind":"remoteSourceControl","location":"git@github.com:x/x.git",` +
				`"state":{"revision":"671108c96644956dddcd89dd59c203dcdb36cec7"}}`),
			"is not an https URL", false,
		},
		{
			"location with a newline", "app",
			pin(`{"identity":"x","kind":"remoteSourceControl","location":"https://example.test/x\n[core]",` +
				`"state":{"revision":"671108c96644956dddcd89dd59c203dcdb36cec7"}}`),
			"is not an https URL", false,
		},
		{
			"abbreviated revision", "app",
			pin(`{"identity":"x","kind":"remoteSourceControl","location":"https://example.test/x",` +
				`"state":{"revision":"671108c"}}`),
			"has no exact revision", false,
		},
		{
			"duplicate key", "app",
			tree{"app/Package.resolved": `{"pins":[],"version":3,"version":2}`},
			"duplicate", false,
		},
		{"malformed Package.resolved", "app", tree{"app/Package.resolved": "{"}, "decode", false},
		{
			"package directory is a file", "app",
			tree{"app#": "file"},
			"is not a directory at the commit", false,
		},
		{"package directory is a symlink", "linked", tree{"linked#": "->app"}, "is not a directory at the commit", false},
		{"package directory is missing", "absent", nil, "is not a directory at the commit", false},
		{"absolute package directory", "/app", nil, "", true},
		{"package directory climbs out", "app/../..", nil, "", true},
		{"package directory is dot", ".", nil, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := t.TempDir()
			if err := writeSwiftFixture(source, "app"); err != nil {
				t.Fatal(err)
			}
			applyTree(t, source, tc.extra)
			request := swiftRequest(tc.packageDir)
			_, err := createBuildContext(filepath.Join(t.TempDir(), "context"), source, request, "sha256:recipe")
			if tc.invalid {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("createBuildContext = %v, want ErrInvalidRequest", err)
				}
				if _, _, _, err := validateRequest(request); !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("validateRequest = %v, want ErrInvalidRequest before any source access", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("createBuildContext = %v, want a refusal naming %q", err, tc.refusal)
			}
		})
	}
}

// Without a declaration, a root Package.swift selects Swift only when no
// earlier toolchain claims the tree.
func TestBuildContextSelectsSwiftAtTheRootLast(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		extra map[string]string
		want  domain.ProjectToolchain
	}{
		"Package.swift alone":       {want: domain.ProjectToolchainSwift},
		"beside a root go.mod":      {extra: map[string]string{"go.mod": testGoMod, "go.sum": testGoSum}, want: domain.ProjectToolchainGo},
		"beside a root packagejson": {extra: map[string]string{"package.json": "{}", "package-lock.json": "{}"}, want: domain.ProjectToolchainNPM},
	} {
		t.Run(name, func(t *testing.T) {
			source := t.TempDir()
			if err := writeSwiftFixture(source, ""); err != nil {
				t.Fatal(err)
			}
			applyTree(t, source, tc.extra)
			layout, err := selectInputLayout(source, "", "")
			if err != nil || layout.Toolchain != tc.want {
				t.Fatalf("selectInputLayout = %+v, %v; want %s", layout, err, tc.want)
			}
		})
	}
}

func TestCheckSwiftPackageResolvedAcceptsPinnedRemotePins(t *testing.T) {
	t.Parallel()
	for _, resolved := range []string{
		testPackageResolved,
		`{"pins":[],"version":2}`,
		resolvedWithPin(`{"identity":"x","kind":"remoteSourceControl","location":"https://example.test/x.git",` +
			`"state":{"revision":"671108c96644956dddcd89dd59c203dcdb36cec7"}}`),
	} {
		if err := checkSwiftPackageResolved([]byte(resolved)); err != nil {
			t.Errorf("checkSwiftPackageResolved(%q) = %v, want nil", resolved, err)
		}
	}
}

func TestSwiftPrepareScriptAndLauncherBindTheImagePaths(t *testing.T) {
	t.Parallel()
	for packageDir, want := range map[string][]string{
		"": {"package_dir='.'", "for entry in '.swiftpm/configuration/mirrors.json' '.swiftpm/config' " +
			`"$package_dir"/Package@swift*; do`},
		"app": {"package_dir='app'", "for entry in 'app/.swiftpm/configuration/mirrors.json' 'app/.swiftpm/config' " +
			`"$package_dir"/Package@swift*; do`},
	} {
		script := swiftPrepareScript(packageDir)
		for _, line := range append(want,
			`cmp -s "$package_dir/Package.swift" `+seedPackageSwiftPath,
			`cmp -s "$package_dir/Package.resolved" `+seedPackageResolvedPath,
			"exit 42",
			`swift package dump-package --package-path "$package_dir" >"$manifest"`,
			`select(has("fileSystem"))`,
			`swift package resolve --package-path "$package_dir" --only-use-versions-from-resolved-file`,
			"resolved differently from the baked mirrors",
		) {
			if !strings.Contains(script, line) {
				t.Errorf("swiftPrepareScript(%q) lacks %q", packageDir, line)
			}
		}
	}
	for _, binding := range []string{
		"toolchain=/tmp/freeside-project-swift\n",
		"staging=$(/usr/bin/busybox mktemp -d /tmp/freeside-project-swift.XXXXXX)",
		"/usr/bin/busybox gzip -dc " + swiftToolchainArchivePath,
		`/usr/bin/busybox mv -T "$staging" "$toolchain" 2>/dev/null ||`,
		"export GIT_CONFIG_GLOBAL=" + SwiftMirrorsPath + "/gitconfig\n",
		`exec "$toolchain/usr/bin/swift" "$@"`,
	} {
		if !strings.Contains(swiftToolchainLauncher, binding) {
			t.Errorf("Swift launcher lacks %q", binding)
		}
	}
	for _, binding := range []string{
		"seed=/opt/freeside/project-seed\n",
		`select(has("fileSystem"))`,
		"--only-use-versions-from-resolved-file",
		`git -C "$repository" config --get remote.origin.url`,
		"submodule foreach --quiet --recursive",
		"| /usr/bin/busybox sort -u >" + swiftMirrorURLsPath + "\n",
		"/usr/bin/busybox sh " + swiftMirrorBakePath + " \\\n\t" + swiftMirrorURLsPath + "\n",
		"HTTPS_PROXY=$closed",
		`cmp -s "$work/package/Package.resolved" "$seed/Package.resolved"`,
	} {
		if !strings.Contains(swiftMirrorDiscovery, binding) {
			t.Errorf("Swift mirror discovery lacks %q", binding)
		}
	}
	for _, binding := range []string{
		"list=$1\n",
		"mirrors=" + SwiftMirrorsPath + "\n",
		`if [ ! -f "$list" ] || [ -L "$list" ]; then`,
		"https://*) ;;",
		`grep -q '[^!-~]'`,
		`git clone --quiet --mirror -- "$url" "$mirrors/$name"`,
		`"url.file://$mirrors/$name.insteadOf" "$key"`,
		`"url.file://$mirrors/$name.insteadOf" "$key.git"`,
		"protocol.file.allow always",
		`printf '%s\n' "$url" >>"$mirrors/urls"`,
		`done <"$list"` + "\n",
	} {
		if !strings.Contains(swiftMirrorBake, binding) {
			t.Errorf("Swift mirror bake lacks %q", binding)
		}
	}
	// The bake runs in a layer the image ships, so it must run no Swift.
	if strings.Contains(swiftMirrorBake, "swift ") {
		t.Error("the Swift mirror bake runs swift, which evaluates manifest code")
	}
	if swiftMirrorRecordPath != SwiftMirrorsPath+"/urls" {
		t.Error("the bake's record is not where swiftMirrorRecordPath names it")
	}
	if filepath.Dir(seedPackageSwiftPath) != "/opt/freeside/project-seed" ||
		filepath.Dir(seedPackageResolvedPath) != "/opt/freeside/project-seed" {
		t.Fatal("the discovery's seed directory no longer holds the seeds")
	}
}

func TestBuildProvesASwiftPackageInEveryFreshWorkspace(t *testing.T) {
	t.Parallel()
	for _, packageDir := range []string{"", "app"} {
		t.Run("package="+packageDir, func(t *testing.T) {
			source := &fakeSource{fixture: func(dir string) error { return writeSwiftFixture(dir, packageDir) }}
			backend := newFakeBackend()
			backend.probe = swiftMaskedProbe(packageDir)
			backend.negativeOutput = []byte("error: Failed to clone repository https://github.com/apple/swift-collections:\n" +
				"fatal: unable to access 'https://github.com/apple/swift-collections/': Could not resolve host: github.com")
			got, err := newBuilder(source, backend, t.TempDir()).Build(t.Context(), swiftRequest(packageDir))
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			wantEnvironment := domain.ProjectImageEnvironment{
				Toolchain: domain.ProjectToolchainSwift, SwiftPackageDir: packageDir,
				PackageSwiftSHA256:    manifestSHA256([]byte(testPackageSwift)),
				PackageResolvedSHA256: manifestSHA256([]byte(testPackageResolved)),
				PreparationDigest:     PreparationDigest(),
			}
			if got.Environment == nil || *got.Environment != wantEnvironment {
				t.Fatalf("environment = %+v, want %+v", got.Environment, wantEnvironment)
			}
			spec := backend.provenanceSpec
			if spec.Layout != got.InputLayout() ||
				spec.SwiftVersion != swiftToolchainVersion ||
				spec.SwiftToolchainArchiveSHA256 != swiftToolchainArchiveSHA256 ||
				spec.SwiftAptSnapshot != swiftAptSnapshot ||
				spec.SwiftAptPackages != strings.Join(swiftAptPackages, " ") ||
				spec.PackageSwiftSHA256 != wantEnvironment.PackageSwiftSHA256 ||
				spec.PackageResolvedSHA256 != wantEnvironment.PackageResolvedSHA256 ||
				spec.NodeVersion != "" || spec.GoVersion != "" || spec.PackageJSONSHA256 != "" || spec.GoModSHA256 != "" {
				t.Fatalf("provenance spec = %+v, want the Swift pins and the recorded package hashes", spec)
			}
			if !strings.Contains(backend.containerfile, "ai.freeside.project.toolchain.swift.version") {
				t.Fatal("Swift build did not render the Swift Containerfile")
			}
			if len(backend.runs) != 8 {
				t.Fatalf("runs = %d, want preflight + preparation/recipe per command + negative", len(backend.runs))
			}
			for index := range 3 {
				if preparation := backend.runs[1+index*2]; !slices.Equal(preparation.Argv, []string{PreparationPath}) ||
					preparation.MaskedCache != "" {
					t.Errorf("preparation %d = %+v, want the unmasked Swift helper", index, preparation)
				}
			}
			probe := backend.runs[7]
			if probe.MaskedCache != SwiftMirrorsPath || !slices.Equal(probe.Argv, swiftMaskedProbe(packageDir)) {
				t.Fatalf("negative run = %+v, want the resolve with the mirrors masked", probe)
			}
			if !slices.Equal(probe.Argv, []string{
				"swift", "package", "resolve", "--package-path", packageDirArg(packageDir),
				"--only-use-versions-from-resolved-file", "--disable-dependency-cache",
			}) {
				t.Fatalf("negative probe = %q, want it run against the package directory", probe.Argv)
			}
		})
	}
}

func packageDirArg(packageDir string) string {
	if packageDir == "" {
		return "."
	}
	return packageDir
}

func TestBuildAcceptsOnlySwiftOfflineRefusalsAsTheNegativeResult(t *testing.T) {
	t.Parallel()
	for output, ok := range map[string]bool{
		"fatal: unable to access 'https://github.com/x/y/': Could not resolve host: github.com": true,
		"fatal: unable to access 'https://github.com/x/y/': Failed to connect to github.com":    true,
		"fatal: Could not read from remote repository.":                                         true,
		"error: the package at 'x' cannot be accessed (permission denied)":                      false,
		"go: example.test/dep@v1.0.0: module lookup disabled by GOPROXY=off":                    false,
		"npm error request to https://registry.npmjs.org/pkg failed: ENOTFOUND":                 false,
	} {
		t.Run(output, func(t *testing.T) {
			source := &fakeSource{fixture: func(dir string) error { return writeSwiftFixture(dir, "app") }}
			backend := newFakeBackend()
			backend.probe = swiftMaskedProbe("app")
			backend.negativeOutput = []byte(output)
			_, err := newBuilder(source, backend, t.TempDir()).Build(t.Context(), swiftRequest("app"))
			if ok && err != nil {
				t.Fatalf("Build = %v, want the offline refusal accepted", err)
			}
			if !ok && !errors.Is(err, ErrProofFailed) {
				t.Fatalf("Build = %v, want ErrProofFailed", err)
			}
		})
	}
}

func TestBuildProvesACacheIndependentSwiftPackageInMaskedWorkspaces(t *testing.T) {
	t.Parallel()
	source := &fakeSource{fixture: func(dir string) error { return writeSwiftFixture(dir, "app") }}
	backend := newFakeBackend()
	backend.probe = swiftMaskedProbe("app")
	backend.negativeErr = nil
	if _, err := newBuilder(source, backend, t.TempDir()).Build(t.Context(), swiftRequest("app")); err != nil {
		t.Fatal(err)
	}
	if len(backend.runs) != 14 || len(backend.publishes) != 1 {
		t.Fatalf("runs/publications = %d/%d, want 14/1", len(backend.runs), len(backend.publishes))
	}
	for _, run := range backend.runs[8:] {
		if run.MaskedCache != SwiftMirrorsPath {
			t.Fatalf("masked proof run %+v does not mask the Swift mirrors", run)
		}
	}
}

func swiftTestEnvironment() *domain.ProjectImageEnvironment {
	return &domain.ProjectImageEnvironment{
		Toolchain: domain.ProjectToolchainSwift, SwiftPackageDir: "app",
		PackageSwiftSHA256:    manifestSHA256([]byte(testPackageSwift)),
		PackageResolvedSHA256: manifestSHA256([]byte(testPackageResolved)),
		PreparationDigest:     PreparationDigest(),
	}
}

func TestObserveSwiftBaseInputsAtThePackageDirectory(t *testing.T) {
	t.Parallel()
	repo := newBaseInputsRepo(t)
	build := repo.commit(map[string]string{
		"app/Package.swift": testPackageSwift, "app/Package.resolved": testPackageResolved,
		"app/Sources/App/App.swift": "public let app = 1\n", verify.DefaultRecipePath: testRecipe,
	})
	layout := domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainSwift, SwiftPackageDir: "app"}
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeside-ai/freeside", RepositoryID: 1023456789,
		CommitSHA: build, RecipeDigest: verify.RecipeDigest([]byte(testRecipe)),
		PreparationCommand: []string{PreparationPath},
		BaseImageRef:       validRequest().BaseImageRef,
		ImageRef:           domain.ImageRef("127.0.0.1:5100/project@" + testImageDigest),
		Environment:        swiftTestEnvironment(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if image.InputLayout() != layout {
		t.Fatalf("image layout = %+v, want %+v", image.InputLayout(), layout)
	}
	observe := func(t *testing.T, commit string) domain.ProjectImageBaseInputs {
		t.Helper()
		inputs, err := ObserveBaseInputs(t.Context(), "git", repo.dir, commit, layout)
		if err != nil {
			t.Fatalf("ObserveBaseInputs: %v", err)
		}
		return inputs
	}
	sourceOnly := repo.commit(map[string]string{"app/Sources/App/App.swift": "public let app = 2\n"})
	inputs := observe(t, sourceOnly)
	if inputs.PackageSwiftSHA256 != image.Environment.PackageSwiftSHA256 ||
		inputs.PackageResolvedSHA256 != image.Environment.PackageResolvedSHA256 ||
		inputs.PackageJSONSHA256 != "" || inputs.GoModSHA256 != "" || len(inputs.UnsupportedInputs) != 0 {
		t.Fatalf("observed %+v, want the app package's hashes only", inputs)
	}
	if err := image.AdmissibleAt(inputs, PreparationDigest()); err != nil {
		t.Fatalf("source-only commit refused: %v", err)
	}

	mirrors := repo.commit(map[string]string{"app/.swiftpm/configuration/mirrors.json": "{}"})
	inputs = observe(t, mirrors)
	if !slices.Equal(inputs.UnsupportedInputs, []string{"app/.swiftpm/configuration/mirrors.json"}) {
		t.Fatalf("unsupported inputs = %q, want the mirrors file", inputs.UnsupportedInputs)
	}
	if err := image.AdmissibleAt(inputs, PreparationDigest()); err == nil {
		t.Fatal("base with a mirrors file admitted")
	}

	repo.git("rm", "-q", "app/.swiftpm/configuration/mirrors.json")
	legacy := repo.commit(map[string]string{"app/.swiftpm/config": "{}"})
	inputs = observe(t, legacy)
	if !slices.Equal(inputs.UnsupportedInputs, []string{"app/.swiftpm/config"}) {
		t.Fatalf("unsupported inputs = %q, want the legacy mirrors config", inputs.UnsupportedInputs)
	}

	repo.git("rm", "-q", "app/.swiftpm/config")
	branch := repo.commit(map[string]string{"app/Package.resolved": resolvedWithPin(
		`{"identity":"x","kind":"remoteSourceControl","location":"https://example.test/x",` +
			`"state":{"branch":"main","revision":"671108c96644956dddcd89dd59c203dcdb36cec7"}}`)})
	inputs = observe(t, branch)
	if !slices.Equal(inputs.UnsupportedInputs, []string{"app/Package.resolved with an unsupported pin"}) {
		t.Fatalf("unsupported inputs = %q, want the branch-pinned Package.resolved", inputs.UnsupportedInputs)
	}
	if err := image.AdmissibleAt(inputs, PreparationDigest()); err == nil ||
		!strings.Contains(err.Error(), "app/Package.resolved") {
		t.Fatalf("AdmissibleAt at a branch pin = %v, want a refusal naming app/Package.resolved", err)
	}
}

func TestEvaluateRebuildAppliesTheSwiftClauses(t *testing.T) {
	t.Parallel()
	const otherRecipe = `{"commands":[["true"]],"capture":"none"}`
	newBase := func(t *testing.T) (baseInputsRepo, string, domain.ProjectImage) {
		t.Helper()
		repo := newBaseInputsRepo(t)
		base := repo.commit(map[string]string{
			"app/Package.swift": testPackageSwift, "app/Package.resolved": testPackageResolved,
			"app/Sources/App/App.swift": "public let app = 1\n", verify.DefaultRecipePath: testRecipe,
		})
		image := domain.ProjectImage{
			CommitSHA: base, RecipeDigest: verify.RecipeDigest([]byte(testRecipe)),
			Environment: swiftTestEnvironment(),
		}
		return repo, base, image
	}
	for _, test := range []struct {
		name    string
		changes map[string]string
		needed  bool
		want    []RebuildRefusal
		detail  []string
	}{
		{name: "source-only head", changes: map[string]string{"app/Sources/App/App.swift": "public let app = 2\n"}},
		{
			// Go and npm manifests are not a Swift image's inputs.
			name:    "root go.mod added",
			changes: map[string]string{"go.mod": testGoMod, "go.sum": testGoSum},
		},
		{
			name: "changed Package.resolved", needed: true,
			changes: map[string]string{"app/Package.resolved": strings.Replace(testPackageResolved, "1.1.4", "1.1.5", 1)},
			want:    []RebuildRefusal{RebuildRefusalSwiftDependencyChanged},
			detail:  []string{"app/Package.resolved"},
		},
		{
			name: "changed Package.swift", needed: true,
			changes: map[string]string{"app/Package.swift": testPackageSwift + "// edit\n"},
			want:    []RebuildRefusal{RebuildRefusalSwiftDependencyChanged},
			detail:  []string{"app/Package.swift"},
		},
		{
			name: "mirrors file added", needed: true,
			changes: map[string]string{"app/.swiftpm/configuration/mirrors.json": "{}"},
			want:    []RebuildRefusal{RebuildRefusalSwiftDependencyChanged},
			detail:  []string{"app/.swiftpm/configuration/mirrors.json"},
		},
		{
			name: "legacy mirrors config added", needed: true,
			changes: map[string]string{"app/.swiftpm/config": "{}"},
			want:    []RebuildRefusal{RebuildRefusalSwiftDependencyChanged},
			detail:  []string{"app/.swiftpm/config"},
		},
		{
			name: "every clause in gate order", needed: true,
			changes: map[string]string{
				"app/Package.swift": testPackageSwift + "// edit\n", verify.DefaultRecipePath: otherRecipe,
			},
			want:   []RebuildRefusal{RebuildRefusalSwiftDependencyChanged, RebuildRefusalRecipeChanged},
			detail: []string{"app/Package.swift"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo, base, image := newBase(t)
			head := repo.commit(test.changes)
			check := func(t *testing.T, decision RebuildDecision) {
				t.Helper()
				if got := findingClauses(decision.Findings); !slices.Equal(got, test.want) {
					t.Fatalf("findings = %+v, want clauses %v", decision.Findings, test.want)
				}
				if len(test.want) != 0 && (decision.Refusal != test.want[0] || decision.Detail != decision.Findings[0].Detail) {
					t.Fatalf("decision = %+v, want the first finding as its refusal", decision)
				}
				for _, name := range test.detail {
					if !strings.Contains(decision.Detail, name) {
						t.Errorf("detail %q does not name %q", decision.Detail, name)
					}
				}
				if decision.Delta.Changed != 0 || decision.Delta.Removed != 0 || len(decision.Delta.Hosts) != 0 {
					t.Fatalf("delta = %+v, want none: no rebuild bounds a Swift change", decision.Delta)
				}
			}
			decision, err := EvaluateRebuild(t.Context(), "git", repo.dir, image, base, head, gatePolicy(t, ""), nil)
			if err != nil {
				t.Fatalf("EvaluateRebuild: %v", err)
			}
			if decision.Needed != test.needed || decision.Head.CommitSHA != head {
				t.Fatalf("decision = %+v, want needed=%t", decision, test.needed)
			}
			check(t, decision)
			// Upkeep applies the same clauses, from the image's build commit.
			decision, err = EvaluateUpkeep(t.Context(), "git", repo.dir, image, head, gatePolicy(t, ""), nil)
			if err != nil || !decision.Needed {
				t.Fatalf("EvaluateUpkeep = %+v, %v; want the gate applied", decision, err)
			}
			check(t, decision)
			if len(test.want) != 0 {
				class, err := ClassifyUpkeepFindings(decision.Findings)
				if err != nil {
					t.Fatal(err)
				}
				if class != domain.ImageUpkeepRefusalSwiftPackageDelta {
					t.Fatalf("class = %q, want %q", class, domain.ImageUpkeepRefusalSwiftPackageDelta)
				}
			}
		})
	}
	t.Run("unreadable Package.resolved", func(t *testing.T) {
		t.Parallel()
		repo, base, image := newBase(t)
		repo.git("rm", "-q", "app/Package.resolved")
		repo.git("update-index", "--add", "--cacheinfo", "120000,"+
			repo.git("hash-object", "-w", "app/Package.swift")+",app/Package.resolved")
		repo.git("commit", "-q", "-m", "symlink")
		head := repo.git("rev-parse", "HEAD")
		decision, err := EvaluateRebuild(t.Context(), "git", repo.dir, image, base, head, gatePolicy(t, ""), nil)
		if err != nil || !decision.Needed || decision.Refusal != RebuildRefusalSwiftDependencyChanged {
			t.Fatalf("decision = %+v, %v; want the Swift clause", decision, err)
		}
	})
}

func TestRebuildRequestKeepsTheDeclaredSwiftPackage(t *testing.T) {
	t.Parallel()
	const head = "1111111111111111111111111111111111111111"
	image := rebuildTestImage(t, "127.0.0.1:5100/freeside-project-x@"+testImageDigest)
	image.Environment = swiftTestEnvironment()
	request, err := RebuildRequest(image, head, "/checkout", []byte(testRecipe),
		BuildInputs{BaseBuildRef: "freeside-agent-claude:local"})
	if err != nil || request.SwiftPackageDir != "app" || request.GoModuleDir != "" {
		t.Fatalf("RebuildRequest = %+v, %v; want the image's package directory", request, err)
	}
}
