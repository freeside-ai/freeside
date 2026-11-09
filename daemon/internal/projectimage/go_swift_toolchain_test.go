package projectimage

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// writeGoSwiftFixture writes a Go module at moduleDir and a Swift package at
// packageDir under directory.
func writeGoSwiftFixture(directory, moduleDir, packageDir string) error {
	if err := writeGoFixture(directory, moduleDir); err != nil {
		return err
	}
	return writeSwiftFixture(directory, packageDir)
}

func goSwiftRequest(moduleDir, packageDir string) Request {
	request := validRequest()
	request.GoModuleDir, request.SwiftPackageDir = moduleDir, packageDir
	return request
}

// goSwiftProbes answers both of a Go and Swift layout's probes with an
// offline refusal, or with success for the caches named in reachable.
func goSwiftProbes(moduleDir, packageDir string, reachable ...string) map[string]fakeProbe {
	probes := map[string]fakeProbe{
		GoModuleCachePath: {
			argv:     goMaskedProbe(moduleDir),
			output:   []byte("go: example.test/dep@v1.0.0: module lookup disabled by GOPROXY=off"),
			exitCode: 1,
		},
		SwiftMirrorsPath: {
			argv: swiftMaskedProbe(packageDir),
			output: []byte("fatal: unable to access 'https://github.com/apple/swift-collections/': " +
				"Could not resolve host: github.com"),
			exitCode: 1,
		},
	}
	for _, cache := range reachable {
		probe := probes[cache]
		probe.output, probe.exitCode = nil, 0
		probes[cache] = probe
	}
	return probes
}

func TestBuildContextPreparationBindsGoSwiftInputs(t *testing.T) {
	t.Parallel()
	// The image adds the Go path's labels and pinned arguments to the Swift
	// path's, so each single-toolchain Containerfile names what this one
	// must carry.
	labelKey := regexp.MustCompile(`ai\.freeside\.[a-z0-9.-]+=`)
	pinnedArg := regexp.MustCompile(`(?m)^ARG PROJECT_[A-Z0-9_]+=.*$`)
	var required []string
	for _, single := range []string{
		renderGoContainerfile(Request{}, ""), renderSwiftContainerfile(Request{}, ""),
	} {
		required = append(required, labelKey.FindAllString(single, -1)...)
		required = append(required, pinnedArg.FindAllString(single, -1)...)
	}
	for _, dirs := range [][2]string{{"daemon", "app"}, {"tools", "tools"}} {
		moduleDir, packageDir := dirs[0], dirs[1]
		t.Run(moduleDir+"+"+packageDir, func(t *testing.T) {
			source := t.TempDir()
			if err := writeGoSwiftFixture(source, moduleDir, packageDir); err != nil {
				t.Fatal(err)
			}
			contextDir := filepath.Join(t.TempDir(), "context")
			layout, err := createBuildContext(contextDir, source, goSwiftRequest(moduleDir, packageDir), "sha256:recipe")
			if err != nil {
				t.Fatal(err)
			}
			want := domain.ProjectImageInputLayout{
				Toolchain: domain.ProjectToolchainGoSwift, GoModuleDir: moduleDir, SwiftPackageDir: packageDir,
			}
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
			if string(read("go.mod")) != testGoMod || string(read("go.sum")) != testGoSum ||
				string(read("Package.swift")) != testPackageSwift ||
				string(read("Package.resolved")) != testPackageResolved {
				t.Fatal("build context does not seed both toolchains' exact manifests")
			}
			if string(read("prepare")) != goSwiftPrepareScript(moduleDir, packageDir) {
				t.Fatal("build context preparation helper is not the Go and Swift helper for these directories")
			}
			if string(read("discover-swift-mirrors")) != swiftMirrorDiscovery ||
				string(read("bake-swift-mirrors")) != swiftMirrorBake {
				t.Fatal("build context does not carry the fixed mirror scripts")
			}
			for name, launcher := range map[string]string{
				"toolchain-launcher":     goToolchainLauncher,
				swiftLauncherContextFile: swiftToolchainLauncher,
			} {
				info, err := os.Stat(filepath.Join(contextDir, name))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(read(name), []byte(launcher)) || info.Mode().Perm() != 0o755 {
					t.Fatalf("generated %s differs from the fixed executable input", name)
				}
			}

			containerfile := string(read("Containerfile"))
			for _, binding := range append(slices.Clone(required),
				"FROM ${PROJECT_TOOLCHAIN_BASE_IMAGE} AS project-go-toolchain",
				"FROM ${PROJECT_TOOLCHAIN_BASE_IMAGE} AS project-swift-toolchain",
				"FROM ${BASE_IMAGE} AS project-swift-runtime",
				"COPY --from=project-swift-toolchain /opt/freeside/project-toolchain/swift.tar.gz "+swiftToolchainArchivePath,
				"COPY "+swiftLauncherContextFile+" "+swiftLauncherPath,
				"FROM project-swift-runtime AS project-swift-discovery",
				"COPY --from=project-go-toolchain /opt/freeside/project-toolchain/go.tar.gz "+goToolchainArchivePath,
				"COPY --from=project-go-toolchain /opt/freeside/project-toolchain/libproc2.deb "+libproc2ArchivePath,
				"COPY toolchain-launcher "+goLauncherPath,
				"COPY toolchain-launcher "+psLauncherPath,
				"COPY go.mod go.sum /opt/freeside/project-seed/",
				"COPY Package.swift Package.resolved /opt/freeside/project-seed/",
				"COPY prepare "+PreparationPath,
				`"$warm/go/bin/go" mod download`,
				"# Bound recipe digest: sha256:recipe",
			) {
				if !strings.Contains(containerfile, binding) {
					t.Errorf("Go and Swift Containerfile lacks %q", binding)
				}
			}
			// The Go launcher never stands in for swift, nor the Swift one for go.
			if strings.Contains(containerfile, "COPY toolchain-launcher "+swiftLauncherPath+"\n") ||
				strings.Contains(containerfile, "COPY "+swiftLauncherContextFile+" "+goLauncherPath+"\n") {
				t.Error("a launcher is installed under the other toolchain's name")
			}
			// The shipped stage holds both toolchains on the Swift runtime and
			// takes only the URL list from the stage that ran manifest code.
			_, shipped, ok := strings.Cut(containerfile, "\nFROM project-swift-runtime\n")
			if !ok {
				t.Fatal("Go and Swift Containerfile has no shipped stage built on the Swift runtime")
			}
			for _, forbidden := range []string{"discover-swift-mirrors", "swift package", "\nFROM "} {
				if strings.Contains(shipped, forbidden) {
					t.Errorf("the shipped stage holds %q", forbidden)
				}
			}
			for line := range strings.SplitSeq(shipped, "\n") {
				if strings.Contains(line, "from=") && line != swiftShippedBakeRun &&
					!strings.HasPrefix(line, "COPY --from=project-go-toolchain /opt/freeside/project-toolchain/") {
					t.Errorf("the shipped stage reads another stage outside the Go archives and the bake's mount: %q", line)
				}
			}
			for _, forbidden := range []string{
				"\nENV ", "\nWORKDIR ", "\nENTRYPOINT ", "\nCMD ", "\nUSER ", "\nVOLUME ",
				"node", "npm", ",direct", "deb.debian.org",
			} {
				if strings.Contains(containerfile, forbidden) {
					t.Errorf("Go and Swift Containerfile carries %q", strings.TrimSpace(forbidden))
				}
			}
		})
	}
}

func TestGoSwiftPrepareScriptRunsTheGoHalfThenTheSwiftHalf(t *testing.T) {
	t.Parallel()
	script := goSwiftPrepareScript("daemon", "app")
	goHalf := goPrepareChecks("daemon") + "(cd \"$module_dir\" && go mod download)\n"
	swiftHalf := swiftPrepareBody("app")
	if script != "#!/usr/bin/busybox sh\nset -eu\n"+goHalf+swiftHalf {
		t.Fatalf("script = %q, want the Go checks and download, then the Swift body", script)
	}
	// The Go download must not replace the shell, or the Swift half never runs.
	if strings.Contains(script, "exec ") {
		t.Fatal("the Go and Swift helper execs before its Swift half")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to parse the helper")
	}
	check := exec.CommandContext(t.Context(), sh, "-n") //nolint:gosec // fixed shell parsing a test-rendered helper
	check.Stdin = strings.NewReader(script)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v: %s", err, output)
	}
}

// A Go and Swift layout is chosen only when both directories are declared;
// without declarations, detection still picks one toolchain.
func TestSelectInputLayoutChoosesGoSwiftOnlyWhenBothAreDeclared(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		moduleDir, packageDir string
		root                  map[string]string
		want                  domain.ProjectImageInputLayout
		refusal               string
	}{
		"both declared": {
			moduleDir: "daemon", packageDir: "app",
			want: domain.ProjectImageInputLayout{
				Toolchain: domain.ProjectToolchainGoSwift, GoModuleDir: "daemon", SwiftPackageDir: "app",
			},
		},
		"only the module declared": {
			moduleDir: "daemon",
			want:      domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainGo, GoModuleDir: "daemon"},
		},
		"only the package declared": {
			packageDir: "app",
			want:       domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainSwift, SwiftPackageDir: "app"},
		},
		"both declared beside a root package.json": {
			moduleDir: "daemon", packageDir: "app",
			root:    map[string]string{"package.json": `{"scripts":{}}`},
			refusal: "root package.json beside the declared Go module daemon and Swift package app",
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := t.TempDir()
			if err := writeGoSwiftFixture(source, "daemon", "app"); err != nil {
				t.Fatal(err)
			}
			applyTree(t, source, tc.root)
			layout, err := selectInputLayout(source, tc.moduleDir, tc.packageDir)
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("selectInputLayout = %+v, %v; want a refusal naming %q", layout, err, tc.refusal)
				}
				return
			}
			if err != nil || layout != tc.want {
				t.Fatalf("selectInputLayout = %+v, %v; want %+v", layout, err, tc.want)
			}
		})
	}
	// Undeclared, a root go.mod beside a root Package.swift selects Go, as it
	// did before a mixed image existed.
	source := t.TempDir()
	if err := writeGoSwiftFixture(source, "", ""); err != nil {
		t.Fatal(err)
	}
	if layout, err := selectInputLayout(source, "", ""); err != nil || layout.Toolchain != domain.ProjectToolchainGo {
		t.Fatalf("undeclared root module and package = %+v, %v; want Go", layout, err)
	}
}

func TestSplitGoSwiftInputsAssignsEachNameToItsToolchain(t *testing.T) {
	t.Parallel()
	for _, layout := range []domain.ProjectImageInputLayout{
		{Toolchain: domain.ProjectToolchainGoSwift, GoModuleDir: "daemon", SwiftPackageDir: "app"},
		{Toolchain: domain.ProjectToolchainGoSwift, GoModuleDir: "tools", SwiftPackageDir: "tools"},
		{Toolchain: domain.ProjectToolchainGoSwift},
	} {
		join := func(name string) string { return filepath.ToSlash(filepath.Join(layout.GoModuleDir, name)) }
		swiftJoin := func(name string) string { return filepath.ToSlash(filepath.Join(layout.SwiftPackageDir, name)) }
		goNames := append([]string{join("go.mod"), join("go.sum"), goLocalReplaceInput(layout.GoModuleDir)},
			goUnsupportedEntries(layout.GoModuleDir)...)
		swiftNames := append([]string{
			swiftJoin("Package.swift"), swiftJoin("Package.resolved"),
			swiftJoin("Package.resolved") + " with an unsupported pin",
			swiftJoin("Package@swift-6.swift"),
		}, swiftUnsupportedEntries(layout.SwiftPackageDir)...)
		// Interleave the two so the split must preserve each side's order.
		var names []string
		for index := range max(len(goNames), len(swiftNames)) {
			if index < len(goNames) {
				names = append(names, goNames[index])
			}
			if index < len(swiftNames) {
				names = append(names, swiftNames[index])
			}
		}
		gotGo, gotSwift := splitGoSwiftInputs(layout, names)
		if !slices.Equal(gotGo, goNames) || !slices.Equal(gotSwift, swiftNames) {
			t.Errorf("layout %+v: split = %q / %q, want %q / %q", layout, gotGo, gotSwift, goNames, swiftNames)
		}
	}
}

func TestBuildProvesAGoSwiftLayoutInEveryFreshWorkspace(t *testing.T) {
	t.Parallel()
	source := &fakeSource{fixture: func(dir string) error { return writeGoSwiftFixture(dir, "daemon", "app") }}
	backend := newFakeBackend()
	backend.probes = goSwiftProbes("daemon", "app")
	got, err := newBuilder(source, backend, t.TempDir()).Build(t.Context(), goSwiftRequest("daemon", "app"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantEnvironment := domain.ProjectImageEnvironment{
		Toolchain: domain.ProjectToolchainGoSwift, GoModuleDir: "daemon", SwiftPackageDir: "app",
		GoModSHA256:           manifestSHA256([]byte(testGoMod)),
		GoSumSHA256:           manifestSHA256([]byte(testGoSum)),
		PackageSwiftSHA256:    manifestSHA256([]byte(testPackageSwift)),
		PackageResolvedSHA256: manifestSHA256([]byte(testPackageResolved)),
		PreparationDigest:     PreparationDigest(),
	}
	if got.Environment == nil || *got.Environment != wantEnvironment {
		t.Fatalf("environment = %+v, want %+v", got.Environment, wantEnvironment)
	}
	spec := backend.provenanceSpec
	if spec.Layout != got.InputLayout() ||
		spec.GoVersion != goToolchainVersion || spec.GoToolchainArchiveSHA256 != goToolchainArchiveSHA256 ||
		spec.Libproc2ArchiveSHA256 != libproc2ArchiveSHA256 ||
		spec.SwiftVersion != swiftToolchainVersion ||
		spec.SwiftToolchainArchiveSHA256 != swiftToolchainArchiveSHA256 ||
		spec.SwiftAptPackages != strings.Join(swiftAptPackages, " ") ||
		spec.GoModSHA256 != wantEnvironment.GoModSHA256 || spec.GoSumSHA256 != wantEnvironment.GoSumSHA256 ||
		spec.PackageSwiftSHA256 != wantEnvironment.PackageSwiftSHA256 ||
		spec.PackageResolvedSHA256 != wantEnvironment.PackageResolvedSHA256 ||
		spec.NodeVersion != "" || spec.PackageJSONSHA256 != "" {
		t.Fatalf("provenance spec = %+v, want both toolchains' pins and the recorded manifest hashes", spec)
	}
	if len(backend.runs) != 9 {
		t.Fatalf("runs = %d, want preflight + preparation/recipe per command + one negative per toolchain",
			len(backend.runs))
	}
	for index := range 3 {
		if preparation := backend.runs[1+index*2]; !slices.Equal(preparation.Argv, []string{PreparationPath}) ||
			preparation.MaskedCache != "" {
			t.Errorf("preparation %d = %+v, want the unmasked helper", index, preparation)
		}
	}
	// Each probe masks only its own toolchain's cache, Go first.
	for index, want := range []struct {
		cache string
		argv  []string
	}{
		{GoModuleCachePath, goMaskedProbe("daemon")},
		{SwiftMirrorsPath, swiftMaskedProbe("app")},
	} {
		probe := backend.runs[7+index]
		if probe.MaskedCache != want.cache || !slices.Equal(probe.Argv, want.argv) {
			t.Errorf("negative run %d = %+v, want %q with %s masked", index, probe, want.argv, want.cache)
		}
	}
}

// Each toolchain's offline refusal is checked against its own pattern: a
// refusal that would satisfy the other toolchain fails the proof.
func TestBuildAcceptsEachToolchainsOwnOfflineRefusal(t *testing.T) {
	t.Parallel()
	for cache, foreign := range map[string]string{
		GoModuleCachePath: "fatal: unable to access 'https://github.com/x/y/': Could not resolve host: github.com",
		SwiftMirrorsPath:  "go: example.test/dep@v1.0.0: module lookup disabled by GOPROXY=off",
	} {
		t.Run(cache, func(t *testing.T) {
			source := &fakeSource{fixture: func(dir string) error { return writeGoSwiftFixture(dir, "daemon", "app") }}
			backend := newFakeBackend()
			backend.probes = goSwiftProbes("daemon", "app")
			probe := backend.probes[cache]
			probe.output = []byte(foreign)
			backend.probes[cache] = probe
			_, err := newBuilder(source, backend, t.TempDir()).Build(t.Context(), goSwiftRequest("daemon", "app"))
			if err == nil || !strings.Contains(err.Error(), "without a registry/network attempt") {
				t.Fatalf("Build = %v, want the foreign refusal to fail the proof", err)
			}
		})
	}
}

// A probe that hydrates without its cache proves the recipe with that cache
// masked, and only that one; the other toolchain's probe still runs.
func TestBuildProvesEachCacheIndependentToolchainWithItsOwnCacheMasked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reachable []string
		// masked lists the cache masked in each run after the preflight and
		// positive pass, in order.
		masked []string
	}{
		{
			reachable: []string{GoModuleCachePath},
			masked: []string{
				GoModuleCachePath, // probe
				GoModuleCachePath, GoModuleCachePath, GoModuleCachePath,
				GoModuleCachePath, GoModuleCachePath, GoModuleCachePath,
				SwiftMirrorsPath, // probe
			},
		},
		{
			reachable: []string{SwiftMirrorsPath},
			masked: []string{
				GoModuleCachePath, // probe
				SwiftMirrorsPath,  // probe
				SwiftMirrorsPath, SwiftMirrorsPath, SwiftMirrorsPath,
				SwiftMirrorsPath, SwiftMirrorsPath, SwiftMirrorsPath,
			},
		},
	} {
		t.Run(strings.Join(tc.reachable, ","), func(t *testing.T) {
			source := &fakeSource{fixture: func(dir string) error { return writeGoSwiftFixture(dir, "daemon", "app") }}
			backend := newFakeBackend()
			backend.probes = goSwiftProbes("daemon", "app", tc.reachable...)
			if _, err := newBuilder(source, backend, t.TempDir()).Build(
				t.Context(), goSwiftRequest("daemon", "app"),
			); err != nil {
				t.Fatal(err)
			}
			var masked []string
			for _, run := range backend.runs[7:] {
				masked = append(masked, run.MaskedCache)
			}
			if !slices.Equal(masked, tc.masked) {
				t.Fatalf("masked caches = %q, want %q", masked, tc.masked)
			}
		})
	}
}

func goSwiftTestEnvironment() *domain.ProjectImageEnvironment {
	return &domain.ProjectImageEnvironment{
		Toolchain: domain.ProjectToolchainGoSwift, GoModuleDir: "daemon", SwiftPackageDir: "app",
		GoModSHA256:           manifestSHA256([]byte(testGoMod)),
		GoSumSHA256:           manifestSHA256([]byte(testGoSum)),
		PackageSwiftSHA256:    manifestSHA256([]byte(testPackageSwift)),
		PackageResolvedSHA256: manifestSHA256([]byte(testPackageResolved)),
		PreparationDigest:     PreparationDigest(),
	}
}

// goSwiftBaseTree is a commit holding the module at daemon and the package
// at app.
func goSwiftBaseTree() map[string]string {
	return map[string]string{
		"daemon/go.mod": testGoMod, "daemon/go.sum": testGoSum, "daemon/main.go": "package main\n",
		"app/Package.swift": testPackageSwift, "app/Package.resolved": testPackageResolved,
		"app/Sources/App/App.swift": "public let app = 1\n", verify.DefaultRecipePath: testRecipe,
	}
}

func TestObserveGoSwiftBaseInputsAtBothDirectories(t *testing.T) {
	t.Parallel()
	repo := newBaseInputsRepo(t)
	build := repo.commit(goSwiftBaseTree())
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeside-ai/freeside", RepositoryID: 1023456789,
		CommitSHA: build, RecipeDigest: verify.RecipeDigest([]byte(testRecipe)),
		PreparationCommand: []string{PreparationPath},
		BaseImageRef:       validRequest().BaseImageRef,
		ImageRef:           domain.ImageRef("127.0.0.1:5100/project@" + testImageDigest),
		Environment:        goSwiftTestEnvironment(),
	})
	if err != nil {
		t.Fatal(err)
	}
	layout := image.InputLayout()
	observe := func(t *testing.T, commit string) domain.ProjectImageBaseInputs {
		t.Helper()
		inputs, err := ObserveBaseInputs(t.Context(), "git", repo.dir, commit, layout)
		if err != nil {
			t.Fatalf("ObserveBaseInputs: %v", err)
		}
		return inputs
	}
	sourceOnly := repo.commit(map[string]string{
		"daemon/main.go": "package main\n\nfunc main() {}\n", "app/Sources/App/App.swift": "public let app = 2\n",
	})
	inputs := observe(t, sourceOnly)
	if inputs.GoModSHA256 != image.Environment.GoModSHA256 || inputs.GoSumSHA256 != image.Environment.GoSumSHA256 ||
		inputs.PackageSwiftSHA256 != image.Environment.PackageSwiftSHA256 ||
		inputs.PackageResolvedSHA256 != image.Environment.PackageResolvedSHA256 ||
		inputs.PackageJSONSHA256 != "" || len(inputs.UnsupportedInputs) != 0 {
		t.Fatalf("observed %+v, want both toolchains' hashes only", inputs)
	}
	if err := image.AdmissibleAt(inputs, PreparationDigest()); err != nil {
		t.Fatalf("source-only commit refused: %v", err)
	}

	// Each toolchain's unsupported entries are observed at its own
	// directory, the module's before the package's.
	both := repo.commit(map[string]string{
		"app/.swiftpm/config": "{}", "daemon/vendor/modules.txt": "x\n", "go.work": "go 1.26\n",
	})
	inputs = observe(t, both)
	if want := []string{"daemon/vendor", "go.work", "app/.swiftpm/config"}; !slices.Equal(inputs.UnsupportedInputs, want) {
		t.Fatalf("unsupported inputs = %q, want %q", inputs.UnsupportedInputs, want)
	}
	for _, name := range []string{"daemon/vendor", "app/.swiftpm/config"} {
		if err := image.AdmissibleAt(inputs, PreparationDigest()); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("AdmissibleAt = %v, want a refusal naming %s", err, name)
		}
	}

	// The other toolchain's entries at a directory are not this one's: a
	// vendor directory beside the package is no Go input.
	repo.git("rm", "-q", "-r", "app/.swiftpm", "daemon/vendor", "go.work")
	elsewhere := repo.commit(map[string]string{"app/vendor/modules.txt": "x\n"})
	if inputs = observe(t, elsewhere); len(inputs.UnsupportedInputs) != 0 {
		t.Fatalf("unsupported inputs = %q, want none outside the module", inputs.UnsupportedInputs)
	}
}

func TestEvaluateRebuildAppliesEachToolchainsClausesToItsOwnInputs(t *testing.T) {
	t.Parallel()
	const (
		proxy       = `["proxy.golang.org"]`
		uuid        = "require github.com/google/uuid v1.6.0\n"
		otherRecipe = `{"commands":[["true"]],"capture":"none"}`
	)
	newBase := func(t *testing.T) (baseInputsRepo, string, domain.ProjectImage) {
		t.Helper()
		repo := newBaseInputsRepo(t)
		base := repo.commit(goSwiftBaseTree())
		image := domain.ProjectImage{
			CommitSHA: base, RecipeDigest: verify.RecipeDigest([]byte(testRecipe)),
			Environment: goSwiftTestEnvironment(),
		}
		return repo, base, image
	}
	changedResolved := strings.Replace(testPackageResolved, "1.1.4", "1.1.5", 1)
	withUUID := map[string]string{"daemon/go.mod": testGoMod + uuid, "daemon/go.sum": testGoSum + uuidSum}
	for _, test := range []struct {
		name    string
		changes map[string]string
		policy  string
		needed  bool
		want    []RebuildRefusal
		detail  []string
		// absent lists what the first refusal must not name.
		absent []string
		delta  LockfileDelta
		class  domain.ImageUpkeepRefusalClass
	}{
		{
			name: "source-only head", policy: proxy,
			changes: map[string]string{"daemon/main.go": "package main\n\nfunc main() {}\n"},
		},
		{
			name: "changed Package.resolved", policy: proxy, needed: true,
			changes: map[string]string{"app/Package.resolved": changedResolved},
			want:    []RebuildRefusal{RebuildRefusalSwiftDependencyChanged},
			detail:  []string{"app/Package.resolved"}, absent: []string{"daemon/"},
			class: domain.ImageUpkeepRefusalSwiftPackageDelta,
		},
		{
			name: "Swift unsupported input", policy: proxy, needed: true,
			changes: map[string]string{"app/.swiftpm/config": "{}"},
			want:    []RebuildRefusal{RebuildRefusalSwiftDependencyChanged},
			detail:  []string{"app/.swiftpm/config"},
			class:   domain.ImageUpkeepRefusalSwiftPackageDelta,
		},
		{
			name: "Go unsupported input", policy: proxy, needed: true,
			changes: map[string]string{"daemon/vendor/modules.txt": "x\n"},
			want:    []RebuildRefusal{RebuildRefusalGoDependencyChanged},
			detail:  []string{"the candidate contains daemon/vendor"}, absent: []string{"app/"},
			class: domain.ImageUpkeepRefusalGoModuleDelta,
		},
		{
			// The Swift package unchanged, a Go change within policy holds
			// with the module's delta.
			name: "pinned module from the declared proxy", changes: withUUID, policy: proxy, needed: true,
			delta: LockfileDelta{Changed: 1, Hosts: []string{"proxy.golang.org"}},
		},
		{
			name: "Go module outside policy", changes: withUUID, needed: true,
			want: []RebuildRefusal{RebuildRefusalNoRegistrySet},
		},
		{
			name: "both toolchains changed", policy: proxy, needed: true,
			changes: map[string]string{
				"app/Package.resolved": changedResolved, "daemon/go.mod": testGoMod + uuid,
				"daemon/go.sum": testGoSum + uuidSum,
			},
			// The Go change is within policy, so the Swift change alone
			// refuses, and a refused decision carries no delta.
			want:   []RebuildRefusal{RebuildRefusalSwiftDependencyChanged},
			detail: []string{"app/Package.resolved"}, absent: []string{"daemon/"},
			class: domain.ImageUpkeepRefusalSwiftPackageDelta,
		},
		{
			name: "every clause in gate order", needed: true,
			changes: map[string]string{
				"app/Package.swift": testPackageSwift + "// edit\n", "daemon/go.mod": testGoMod + uuid,
				"daemon/vendor/modules.txt": "x\n", verify.DefaultRecipePath: otherRecipe,
			},
			want: []RebuildRefusal{
				RebuildRefusalSwiftDependencyChanged, RebuildRefusalGoDependencyChanged,
				RebuildRefusalRecipeChanged, RebuildRefusalNoRegistrySet, RebuildRefusalUnpinnedSource,
			},
			// Upkeep's class precedence ranks the Go class first; both hand
			// the image to a person.
			class: domain.ImageUpkeepRefusalGoModuleDelta,
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
				for _, name := range test.absent {
					if strings.Contains(decision.Detail, name) {
						t.Errorf("detail %q names the other toolchain's %q", decision.Detail, name)
					}
				}
				if decision.Delta.Changed != test.delta.Changed || decision.Delta.Removed != test.delta.Removed ||
					!slices.Equal(decision.Delta.Hosts, test.delta.Hosts) {
					t.Fatalf("delta = %+v, want %+v", decision.Delta, test.delta)
				}
			}
			decision, err := EvaluateRebuild(t.Context(), "git", repo.dir, image, base, head, gatePolicy(t, test.policy), nil)
			if err != nil {
				t.Fatalf("EvaluateRebuild: %v", err)
			}
			if decision.Needed != test.needed || decision.Head.CommitSHA != head {
				t.Fatalf("decision = %+v, want needed=%t", decision, test.needed)
			}
			check(t, decision)
			decision, err = EvaluateUpkeep(t.Context(), "git", repo.dir, image, head, gatePolicy(t, test.policy), nil)
			if err != nil || !decision.Needed {
				t.Fatalf("EvaluateUpkeep = %+v, %v; want the gate applied", decision, err)
			}
			check(t, decision)
			if test.class != "" {
				class, err := ClassifyUpkeepFindings(decision.Findings)
				if err != nil || class != test.class {
					t.Fatalf("class = %q, %v; want %q", class, err, test.class)
				}
			}
		})
	}
	// An unreadable manifest of either toolchain hands the image to a person.
	for _, name := range []string{"daemon/go.mod", "app/Package.resolved"} {
		t.Run("unreadable "+name, func(t *testing.T) {
			t.Parallel()
			repo, base, image := newBase(t)
			repo.git("rm", "-q", name)
			repo.git("update-index", "--add", "--cacheinfo", "120000,"+
				repo.git("hash-object", "-w", "app/Package.swift")+","+name)
			repo.git("commit", "-q", "-m", "symlink")
			head := repo.git("rev-parse", "HEAD")
			decision, err := EvaluateRebuild(t.Context(), "git", repo.dir, image, base, head, gatePolicy(t, proxy), nil)
			if err != nil || !decision.Needed || decision.Refusal != RebuildRefusalSwiftDependencyChanged {
				t.Fatalf("decision = %+v, %v; want the Swift clause", decision, err)
			}
		})
	}
}

func TestRebuildRequestKeepsBothDeclaredDirectories(t *testing.T) {
	t.Parallel()
	const head = "1111111111111111111111111111111111111111"
	image := rebuildTestImage(t, "127.0.0.1:5100/freeside-project-x@"+testImageDigest)
	image.Environment = goSwiftTestEnvironment()
	request, err := RebuildRequest(image, head, "/checkout", []byte(testRecipe),
		BuildInputs{BaseBuildRef: "freeside-agent-claude:local"})
	if err != nil || request.GoModuleDir != "daemon" || request.SwiftPackageDir != "app" {
		t.Fatalf("RebuildRequest = %+v, %v; want both of the image's directories", request, err)
	}
}

// TestEveryContainerfileFitsTheBuildLimit renders each toolchain's
// Containerfile for the longest repository name GitHub allows, since only
// the header's repository and commit vary in size, and requires it to fit
// Apple container's limit, which a real Go and Swift build first exceeded.
// The Go and Swift render drops the composed stages' comments, which is
// sound only while no render carries a heredoc or a parser directive.
func TestEveryContainerfileFitsTheBuildLimit(t *testing.T) {
	t.Parallel()
	request := Request{
		Repository:  strings.Repeat("o", 39) + "/" + strings.Repeat("n", 100),
		CommitSHA:   strings.Repeat("a", 40),
		GoModuleDir: "daemon", SwiftPackageDir: "app",
	}
	digest := domain.Digest("sha256:" + strings.Repeat("b", 64))
	for _, toolchain := range domain.AllProjectToolchains {
		text := renderContainerfile(domain.ProjectImageInputLayout{Toolchain: toolchain}, request, digest)
		if text == "" {
			t.Fatalf("%s renders no Containerfile", toolchain)
		}
		if len(text) > containerfileByteLimit {
			t.Errorf("%s Containerfile is %d bytes, over the %d-byte limit", toolchain, len(text), containerfileByteLimit)
		}
		if strings.Contains(text, "<<") {
			t.Errorf("%s Containerfile carries a heredoc", toolchain)
		}
		if strings.HasPrefix(text, "# syntax=") || strings.HasPrefix(text, "# escape=") {
			t.Errorf("%s Containerfile starts with a parser directive", toolchain)
		}
	}
	var comments []string
	for line := range strings.Lines(renderGoSwiftContainerfile(request, digest)) {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			comments = append(comments, line)
		}
	}
	if len(comments) != 4 || !strings.HasPrefix(comments[3], "# Bound recipe digest: "+string(digest)) {
		t.Fatalf("Go and Swift Containerfile comments = %q, want the three header lines and the bound recipe digest",
			comments)
	}
}
