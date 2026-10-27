package projectimage

import (
	"bytes"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	testGoMod = "module example.test/m\n\ngo 1.26.9\n\nrequire example.test/dep v1.0.0\n"
	testGoSum = "example.test/dep v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n" +
		"example.test/dep v1.0.0/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=\n"
)

// writeGoFixture writes a Go module at moduleDir under directory, with no
// package.json anywhere in the tree.
func writeGoFixture(directory, moduleDir string) error {
	module := filepath.Join(directory, filepath.FromSlash(moduleDir))
	if err := os.MkdirAll(module, 0o700); err != nil {
		return err
	}
	for name, content := range map[string]string{
		"go.mod": testGoMod, "go.sum": testGoSum, "main.go": "package main\n\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(module, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func goRequest(moduleDir string) Request {
	request := validRequest()
	request.GoModuleDir = moduleDir
	return request
}

func TestBuildContextPreparationBindsGoInputs(t *testing.T) {
	for _, moduleDir := range []string{"", "daemon", "tools/lint"} {
		t.Run("module="+moduleDir, func(t *testing.T) {
			source := t.TempDir()
			if err := writeGoFixture(source, moduleDir); err != nil {
				t.Fatal(err)
			}
			contextDir := filepath.Join(t.TempDir(), "context")
			layout, err := createBuildContext(contextDir, source, goRequest(moduleDir), "sha256:recipe")
			if err != nil {
				t.Fatal(err)
			}
			want := domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainGo, GoModuleDir: moduleDir}
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
			if string(read("go.mod")) != testGoMod || string(read("go.sum")) != testGoSum {
				t.Fatal("build context does not seed the module's exact go.mod and go.sum")
			}
			if string(read("prepare")) != goPrepareScript(moduleDir) {
				t.Fatal("build context preparation helper is not the Go helper for this module")
			}
			info, err := os.Stat(filepath.Join(contextDir, "toolchain-launcher"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(read("toolchain-launcher"), []byte(goToolchainLauncher)) || info.Mode().Perm() != 0o755 {
				t.Fatal("generated Go toolchain launcher differs from the fixed executable input")
			}
			containerfile := string(read("Containerfile"))
			for _, binding := range []string{
				"ARG PROJECT_GO_VERSION=" + goToolchainVersion,
				"ARG PROJECT_GO_SHA256=" + goToolchainArchiveSHA256,
				"ARG PROJECT_GOLANGCI_LINT_VERSION=" + golangciLintVersion,
				"ARG PROJECT_GOLANGCI_LINT_SHA256=" + golangciLintArchiveSHA256,
				"ARG PROJECT_VACUUM_VERSION=" + vacuumVersion,
				"ARG PROJECT_VACUUM_SHA256=" + vacuumArchiveSHA256,
				"ARG PROJECT_SHELLCHECK_VERSION=" + shellcheckVersion,
				"ARG PROJECT_SHELLCHECK_SHA256=" + shellcheckArchiveSHA256,
				"ARG PROJECT_PYTHON_VERSION=" + pythonVersion,
				"ARG PROJECT_PYTHON_SHA256=" + pythonArchiveSHA256,
				"ARG PROJECT_PROCPS_VERSION=" + procpsVersion,
				"ARG PROJECT_PROCPS_SNAPSHOT=" + procpsSnapshot,
				"ARG PROJECT_PROCPS_SHA256=" + procpsArchiveSHA256,
				"ARG PROJECT_LIBPROC2_SHA256=" + libproc2ArchiveSHA256,
				`echo "${PROJECT_GO_SHA256}  /opt/freeside/project-toolchain/go.tar.gz" | sha256sum -c -`,
				`echo "${PROJECT_GOLANGCI_LINT_SHA256}  /opt/freeside/project-toolchain/golangci-lint.tar.gz" | sha256sum -c -`,
				`echo "${PROJECT_VACUUM_SHA256}  vacuum.tar.gz" | sha256sum -c -`,
				`echo "${PROJECT_SHELLCHECK_SHA256}  shellcheck.tar.xz" | sha256sum -c -`,
				`echo "${PROJECT_PYTHON_SHA256}  python.tar.gz" | sha256sum -c -`,
				`echo "${PROJECT_PROCPS_SHA256}  procps.deb" | sha256sum -c -`,
				`echo "${PROJECT_LIBPROC2_SHA256}  libproc2.deb" | sha256sum -c -`,
				"COPY --from=project-toolchain /opt/freeside/project-toolchain/go.tar.gz " + goToolchainArchivePath,
				"COPY --from=project-toolchain /opt/freeside/project-toolchain/golangci-lint.tar.gz " + golangciLintArchivePath,
				"COPY --from=project-toolchain /opt/freeside/project-toolchain/vacuum.tar.gz " + vacuumArchivePath,
				"COPY --from=project-toolchain /opt/freeside/project-toolchain/shellcheck.tar.xz " + shellcheckArchivePath,
				"COPY --from=project-toolchain /opt/freeside/project-toolchain/python.tar.gz " + pythonArchivePath,
				"COPY --from=project-toolchain /opt/freeside/project-toolchain/procps.deb " + procpsArchivePath,
				"COPY --from=project-toolchain /opt/freeside/project-toolchain/libproc2.deb " + libproc2ArchivePath,
				`RUN ["/usr/bin/busybox", "rm", "-f", "/usr/local/bin/go", "/usr/local/bin/gofmt", "/usr/local/bin/golangci-lint", ` +
					`"/usr/local/bin/vacuum", "/usr/local/bin/shellcheck", "/usr/bin/python3", "/usr/bin/ps"]`,
				"COPY toolchain-launcher " + goLauncherPath,
				"COPY toolchain-launcher " + gofmtLauncherPath,
				"COPY toolchain-launcher " + golangciLintLauncherPath,
				"COPY toolchain-launcher " + vacuumLauncherPath,
				"COPY toolchain-launcher " + shellcheckLauncherPath,
				"COPY toolchain-launcher " + pythonLauncherPath,
				"COPY toolchain-launcher " + psLauncherPath,
				`test "$(vacuum version)" = "${PROJECT_VACUUM_VERSION}"`,
				`shellcheck --version | grep -Fx "version: ${PROJECT_SHELLCHECK_VERSION}"`,
				`test "$(python3 --version)" = "Python ${PROJECT_PYTHON_VERSION%+*}"`,
				`test "$(ps --version)" = "ps from procps-ng ${PROJECT_PROCPS_VERSION%-*}"`,
				"COPY go.mod go.sum /opt/freeside/project-seed/",
				"COPY prepare " + PreparationPath,
				"GOMODCACHE=" + GoModuleCachePath,
				`"$warm/go/bin/go" mod download`,
				`ai.freeside.project.toolchain.go.version="${PROJECT_GO_VERSION}"`,
				`ai.freeside.project.toolchain.go.archive-sha256="${PROJECT_GO_SHA256}"`,
				`ai.freeside.project.toolchain.golangci-lint.version="${PROJECT_GOLANGCI_LINT_VERSION}"`,
				`ai.freeside.project.toolchain.golangci-lint.archive-sha256="${PROJECT_GOLANGCI_LINT_SHA256}"`,
				`ai.freeside.project.toolchain.vacuum.version="${PROJECT_VACUUM_VERSION}"`,
				`ai.freeside.project.toolchain.vacuum.archive-sha256="${PROJECT_VACUUM_SHA256}"`,
				`ai.freeside.project.toolchain.shellcheck.version="${PROJECT_SHELLCHECK_VERSION}"`,
				`ai.freeside.project.toolchain.shellcheck.archive-sha256="${PROJECT_SHELLCHECK_SHA256}"`,
				`ai.freeside.project.toolchain.python.version="${PROJECT_PYTHON_VERSION}"`,
				`ai.freeside.project.toolchain.python.archive-sha256="${PROJECT_PYTHON_SHA256}"`,
				`ai.freeside.project.toolchain.procps.version="${PROJECT_PROCPS_VERSION}"`,
				`ai.freeside.project.toolchain.procps.archive-sha256="${PROJECT_PROCPS_SHA256}"`,
				`ai.freeside.project.toolchain.libproc2.version="${PROJECT_PROCPS_VERSION}"`,
				`ai.freeside.project.toolchain.libproc2.archive-sha256="${PROJECT_LIBPROC2_SHA256}"`,
				"# Bound recipe digest: sha256:recipe",
			} {
				if !strings.Contains(containerfile, binding) {
					t.Errorf("Go Containerfile lacks %q", binding)
				}
			}
			for _, forbidden := range []string{
				"\nENV ", "\nWORKDIR ", "\nENTRYPOINT ", "\nCMD ", "\nUSER ", "\nVOLUME ", "node", "npm",
			} {
				if strings.Contains(containerfile, forbidden) {
					t.Errorf("Go Containerfile carries %q", strings.TrimSpace(forbidden))
				}
			}
		})
	}
}

func TestBuildContextRefusesUnsupportedGoInputs(t *testing.T) {
	type tree map[string]string
	cases := []struct {
		name      string
		moduleDir string
		// extra adds entries to the daemon-module fixture; "" removes one,
		// and a value starting "->" makes a symlink to the rest.
		extra   tree
		refusal string
		invalid bool
	}{
		{
			"root package.json beside a declared module", "daemon",
			tree{"package.json": `{"scripts":{}}`},
			"root package.json beside the declared Go module", false,
		},
		{"missing go.mod", "daemon", tree{"daemon/go.mod": ""}, "copy daemon/go.mod", false},
		{"missing go.sum", "daemon", tree{"daemon/go.sum": ""}, "copy daemon/go.sum", false},
		{
			"symlinked go.mod", "daemon",
			tree{"daemon/go.mod": "", "real.mod": testGoMod, "daemon/go.mod#": "->../real.mod"},
			"not a regular file", false,
		},
		{
			"vendor directory", "daemon",
			tree{"daemon/vendor/modules.txt": "# example.test/dep v1.0.0\n"},
			"unsupported Go input daemon/vendor", false,
		},
		{"go.work in the module", "daemon", tree{"daemon/go.work": "go 1.26\n"}, "unsupported Go input daemon/go.work", false},
		{"go.work above the module", "daemon", tree{"go.work": "go 1.26\n"}, "unsupported Go input go.work", false},
		{"go.work as a directory", "daemon", tree{"go.work/x": "x"}, "unsupported Go input go.work", false},
		{
			"replace leaves the module", "daemon",
			tree{"daemon/go.mod": testGoMod + "replace example.test/dep => ../dep\n"},
			"is a local path", false,
		},
		{
			"replace block leaves the module", "daemon",
			tree{"daemon/go.mod": testGoMod + "replace (\n\texample.test/dep v1.0.0 => ./x/../../dep\n)\n"},
			"is a local path", false,
		},
		// A seed holding only go.mod and go.sum cannot warm a directory the
		// module replaces, wherever it is.
		{
			"replace inside the module", "daemon",
			tree{
				"daemon/go.mod":                 testGoMod + "replace example.test/dep => ./third_party/dep\n",
				"daemon/third_party/dep/go.mod": "module example.test/dep\n",
			},
			"is a local path", false,
		},
		{
			"absolute replace", "daemon",
			tree{"daemon/go.mod": testGoMod + "replace example.test/dep => /opt/dep\n"},
			"is a local path", false,
		},
		{
			"replace block with no space before its parenthesis", "daemon",
			tree{"daemon/go.mod": testGoMod + "replace(\n\texample.test/dep => /opt/dep\n)\n"},
			"is a local path", false,
		},
		{
			"go directive above the baked version", "daemon",
			tree{"daemon/go.mod": "module example.test/m\n\ngo 1.27\n"},
			"above the baked Go " + goToolchainVersion, false,
		},
		{
			"toolchain directive naming another version", "daemon",
			tree{"daemon/go.mod": testGoMod + "toolchain go1.26.9\n"},
			"names a toolchain other than the baked", false,
		},
		{
			"module directory is a file", "daemon",
			tree{"daemon/go.mod": "", "daemon/go.sum": "", "daemon/main.go": "", "daemon#": "file"},
			"is not a directory at the commit", false,
		},
		{
			"module directory is a symlink", "linked",
			tree{"linked#": "->daemon"},
			"is not a directory at the commit", false,
		},
		{"module directory is missing", "absent", nil, "is not a directory at the commit", false},
		{"absolute module directory", "/daemon", nil, "", true},
		{"module directory climbs out", "daemon/../..", nil, "", true},
		{"module directory is dot", ".", nil, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := t.TempDir()
			if err := writeGoFixture(source, "daemon"); err != nil {
				t.Fatal(err)
			}
			applyTree(t, source, tc.extra)
			_, err := createBuildContext(
				filepath.Join(t.TempDir(), "context"), source, goRequest(tc.moduleDir), "sha256:recipe")
			if tc.invalid {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("createBuildContext = %v, want ErrInvalidRequest", err)
				}
				if _, _, _, err := validateRequest(goRequest(tc.moduleDir)); !errors.Is(err, ErrInvalidRequest) {
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

// applyTree edits a fixture: "" removes a path, a key ending "#" creates the
// entry named by the rest as a symlink ("->target") or a plain file.
func applyTree(t *testing.T, root string, entries map[string]string) {
	t.Helper()
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	// Removals and plain writes before the "#" entries that replace a path.
	slices.SortFunc(names, func(a, b string) int {
		return strings.Compare(strings.Replace(a, "#", "~", 1), strings.Replace(b, "#", "~", 1))
	})
	for _, name := range names {
		content := entries[name]
		target, special := strings.CutSuffix(name, "#")
		full := filepath.Join(root, filepath.FromSlash(target))
		switch {
		case special:
			if err := os.RemoveAll(full); err != nil {
				t.Fatal(err)
			}
			if link, ok := strings.CutPrefix(content, "->"); ok {
				if err := os.Symlink(link, full); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		case content == "":
			if err := os.RemoveAll(full); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestBuildContextKeepsNPMWhenNoModuleIsDeclared(t *testing.T) {
	// A root package.json keeps every existing npm image on the npm path,
	// even beside a root go.mod.
	source := t.TempDir()
	if err := writeNPMFixture(source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte(testGoMod), 0o600); err != nil {
		t.Fatal(err)
	}
	layout, err := createBuildContext(filepath.Join(t.TempDir(), "context"), source, validRequest(), "sha256:recipe")
	if err != nil || layout.Toolchain != domain.ProjectToolchainNPM {
		t.Fatalf("createBuildContext = %+v, %v; want the npm layout", layout, err)
	}
}

func TestCheckGoModuleAcceptsWhatTheBakedToolchainServes(t *testing.T) {
	for _, goMod := range []string{
		testGoMod,
		"module m\n\ngo " + goToolchainVersion + "\n\ntoolchain go" + goToolchainVersion + "\n",
		"module m\ngo 1.21\n",
		"module m // trailing comment\n\ngo 1.26 // older\n",
		"module m\n\nreplace example.test/dep => example.test/fork v1.2.3\n",
		"module m\n\nreplace (\n\texample.test/a => example.test/a2 v1.0.0 // fork\n\t\"example.test/b\" v1.0.0 => `example.test/c` v1.1.0\n)\n",
		"module m\n\nreplace(\n\texample.test/a => example.test/a2 v1.0.0\n)\n\nrequire(example.test/a v1.0.0)\n",
		"module m\n\nrequire (\n\texample.test/dep v1.0.0 // indirect\n)\n",
	} {
		if err := checkGoModule([]byte(goMod)); err != nil {
			t.Errorf("checkGoModule(%q) = %v, want nil", goMod, err)
		}
	}
	for _, goMod := range []string{
		"module m\n\ngo 1.26.11\n",
		"module m\n\ngo 2\n",
		"module m\n\ngo banana\n",
		"module m\n\ntoolchain default\n",
		"module m\n\ntoolchain go1.27.0\n",
		"module m\n\nreplace example.test/dep => ..\n",
		"module m\n\nreplace example.test/dep => ./third_party/dep\n",
		"module m\n\nreplace example.test/dep => .\n",
		"module m\n\nreplace(\n\texample.test/dep => ../outside\n)\n",
		"module m\n\nreplace (\n\texample.test/a => example.test/a2 v1.0.0\n\t\"example.test/b\" => `./b`\n)\n",
		"module m\n\nreplace example.test/dep =>\n",
		"module m\n\nreplace (\n\texample.test/dep => \"../dep\"\n)\n",
		"module m\n\nrequire (\n\texample.test/dep v1.0.0\n",
		"module m\n\nrequire \"unterminated\n",
	} {
		if err := checkGoModule([]byte(goMod)); err == nil {
			t.Errorf("checkGoModule(%q) = nil, want a refusal", goMod)
		}
	}
}

func TestGoPrepareScriptChecksEveryUnsupportedEntry(t *testing.T) {
	for moduleDir, want := range map[string][]string{
		"":     {"module_dir='.'", "for entry in 'vendor' 'go.work'; do"},
		"a/b":  {"module_dir='a/b'", "for entry in 'a/b/vendor' 'a/b/go.work' 'a/go.work' 'go.work'; do"},
		"tool": {"module_dir='tool'", "for entry in 'tool/vendor' 'tool/go.work' 'go.work'; do"},
	} {
		script := goPrepareScript(moduleDir)
		for _, line := range append(want,
			`cmp -s "$module_dir/go.mod" /opt/freeside/project-seed/go.mod`,
			`cmp -s "$module_dir/go.sum" /opt/freeside/project-seed/go.sum`,
			"exit 42", `cd "$module_dir"`, "exec go mod download",
		) {
			if !strings.Contains(script, line) {
				t.Errorf("goPrepareScript(%q) lacks %q", moduleDir, line)
			}
		}
	}
	for _, binding := range []string{
		"toolchain=$(/usr/bin/busybox mktemp -d /tmp/freeside-project-go.XXXXXX)",
		"export FREESIDE_PROJECT_GO_ROOT=$toolchain",
		`staging=$(/usr/bin/busybox mktemp -d "/tmp/freeside-project-$family.XXXXXX")`,
		`/usr/bin/busybox mv -T "$staging" "$toolchain" 2>/dev/null ||`,
		`exec "$toolchain/vacuum" "$@"`,
		`exec "$toolchain/shellcheck" "$@"`,
		`exec "$toolchain/bin/python3" "$@"`,
		`exec "$toolchain/usr/bin/ps" "$@"`,
		"export GOTOOLCHAIN=local GOPROXY=off GOWORK=off GOMODCACHE=" + GoModuleCachePath,
		`export GOCACHE="${GOCACHE:-${HOME:-/tmp}/.cache/go-build}"`,
		goToolchainArchivePath, golangciLintArchivePath, vacuumArchivePath, shellcheckArchivePath,
		pythonArchivePath, procpsArchivePath, libproc2ArchivePath,
	} {
		if !strings.Contains(goToolchainLauncher, binding) {
			t.Errorf("Go launcher lacks %q", binding)
		}
	}
}

func TestBuildProvesAGoModuleInEveryFreshWorkspace(t *testing.T) {
	for _, moduleDir := range []string{"", "daemon"} {
		t.Run("module="+moduleDir, func(t *testing.T) {
			source := &fakeSource{fixture: func(dir string) error { return writeGoFixture(dir, moduleDir) }}
			backend := newFakeBackend()
			backend.probe = goMaskedProbe(moduleDir)
			backend.negativeOutput = []byte("go: example.test/dep@v1.0.0: module lookup disabled by GOPROXY=off")
			got, err := newBuilder(source, backend, t.TempDir()).Build(t.Context(), goRequest(moduleDir))
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			wantEnvironment := domain.ProjectImageEnvironment{
				Toolchain: domain.ProjectToolchainGo, GoModuleDir: moduleDir,
				GoModSHA256:       manifestSHA256([]byte(testGoMod)),
				GoSumSHA256:       manifestSHA256([]byte(testGoSum)),
				PreparationDigest: PreparationDigest(),
			}
			if got.Environment == nil || *got.Environment != wantEnvironment {
				t.Fatalf("environment = %+v, want %+v", got.Environment, wantEnvironment)
			}
			spec := backend.provenanceSpec
			if spec.Layout != got.InputLayout() ||
				spec.GoVersion != goToolchainVersion || spec.GoToolchainArchiveSHA256 != goToolchainArchiveSHA256 ||
				spec.GolangciLintVersion != golangciLintVersion ||
				spec.GolangciLintArchiveSHA256 != golangciLintArchiveSHA256 ||
				spec.GoModSHA256 != wantEnvironment.GoModSHA256 || spec.GoSumSHA256 != wantEnvironment.GoSumSHA256 ||
				spec.NodeVersion != "" || spec.PackageJSONSHA256 != "" {
				t.Fatalf("provenance spec = %+v, want the Go pins and the recorded module hashes", spec)
			}
			if !strings.Contains(backend.containerfile, "AS project-toolchain") ||
				!strings.Contains(backend.containerfile, "ai.freeside.project.toolchain.go.version") {
				t.Fatal("Go build did not render the Go Containerfile")
			}
			if len(backend.runs) != 8 {
				t.Fatalf("runs = %d, want preflight + preparation/recipe per command + negative", len(backend.runs))
			}
			for index := range 3 {
				if preparation := backend.runs[1+index*2]; !slices.Equal(preparation.Argv, []string{PreparationPath}) ||
					preparation.MaskedCache != "" {
					t.Errorf("preparation %d = %+v, want the unmasked Go helper", index, preparation)
				}
			}
			probe := backend.runs[7]
			if probe.MaskedCache != GoModuleCachePath || !slices.Equal(probe.Argv, goMaskedProbe(moduleDir)) {
				t.Fatalf("negative run = %+v, want go mod download with the module cache masked", probe)
			}
			if moduleDir != "" && !slices.Equal(probe.Argv, []string{"go", "-C", moduleDir, "mod", "download"}) {
				t.Fatalf("negative probe = %q, want it run from the module directory", probe.Argv)
			}
		})
	}
}

func TestBuildAcceptsOnlyGoOfflineRefusalsAsTheNegativeResult(t *testing.T) {
	for output, ok := range map[string]bool{
		"go: example.test/dep@v1.0.0: module lookup disabled by GOPROXY=off":             true,
		"go: example.test/dep: Get \"https://proxy.golang.org/x\": dial tcp: lookup x":   true,
		"go: example.test/dep: lookup proxy.golang.org: no such host":                    true,
		"go: example.test/dep@v1.0.0: missing go.sum entry":                              false,
		"npm error request to https://registry.npmjs.org/pkg failed: ENOTFOUND":          false,
		"go: example.test/dep@v1.0.0: verifying module: checksum mismatch (permissions)": false,
	} {
		t.Run(output, func(t *testing.T) {
			source := &fakeSource{fixture: func(dir string) error { return writeGoFixture(dir, "daemon") }}
			backend := newFakeBackend()
			backend.probe = goMaskedProbe("daemon")
			backend.negativeOutput = []byte(output)
			_, err := newBuilder(source, backend, t.TempDir()).Build(t.Context(), goRequest("daemon"))
			if ok && err != nil {
				t.Fatalf("Build = %v, want the offline refusal accepted", err)
			}
			if !ok && !errors.Is(err, ErrProofFailed) {
				t.Fatalf("Build = %v, want ErrProofFailed", err)
			}
		})
	}
}

func TestBuildProvesACacheIndependentGoModuleInMaskedWorkspaces(t *testing.T) {
	source := &fakeSource{fixture: func(dir string) error { return writeGoFixture(dir, "daemon") }}
	backend := newFakeBackend()
	backend.probe = goMaskedProbe("daemon")
	backend.negativeErr = nil
	if _, err := newBuilder(source, backend, t.TempDir()).Build(t.Context(), goRequest("daemon")); err != nil {
		t.Fatal(err)
	}
	if len(backend.runs) != 14 || len(backend.publishes) != 1 {
		t.Fatalf("runs/publications = %d/%d, want 14/1", len(backend.runs), len(backend.publishes))
	}
	for _, run := range backend.runs[8:] {
		if run.MaskedCache != GoModuleCachePath {
			t.Fatalf("masked proof run %+v does not mask the Go module cache", run)
		}
	}
}

func TestObserveGoBaseInputsAtTheModuleDirectory(t *testing.T) {
	repo := newBaseInputsRepo(t)
	build := repo.commit(map[string]string{
		"daemon/go.mod": testGoMod, "daemon/go.sum": testGoSum, "daemon/main.go": "package main\n",
		"scripts/tool/go.mod": "module tool\n", verify.DefaultRecipePath: testRecipe,
	})
	layout := domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainGo, GoModuleDir: "daemon"}
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeside-ai/freeside", RepositoryID: 1023456789,
		CommitSHA: build, RecipeDigest: verify.RecipeDigest([]byte(testRecipe)),
		PreparationCommand: []string{PreparationPath},
		BaseImageRef:       validRequest().BaseImageRef,
		ImageRef:           domain.ImageRef("127.0.0.1:5100/project@" + testImageDigest),
		Environment: &domain.ProjectImageEnvironment{
			Toolchain: domain.ProjectToolchainGo, GoModuleDir: "daemon",
			GoModSHA256:       manifestSHA256([]byte(testGoMod)),
			GoSumSHA256:       manifestSHA256([]byte(testGoSum)),
			PreparationDigest: PreparationDigest(),
		},
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
	sourceOnly := repo.commit(map[string]string{
		"daemon/main.go": "package main\n\nfunc main() {}\n", "scripts/tool/go.mod": "module tool\n\ngo 1.26\n",
	})
	inputs := observe(t, sourceOnly)
	if inputs.GoModSHA256 != manifestSHA256([]byte(testGoMod)) || inputs.GoSumSHA256 != manifestSHA256([]byte(testGoSum)) ||
		inputs.PackageJSONSHA256 != "" || len(inputs.UnsupportedInputs) != 0 {
		t.Fatalf("observed %+v, want the daemon module's hashes only", inputs)
	}
	if err := image.AdmissibleAt(inputs, PreparationDigest()); err != nil {
		t.Fatalf("source-only commit refused: %v", err)
	}
	for _, tc := range []struct {
		name    string
		changes map[string]string
		revert  map[string]string
		names   string
	}{
		{
			"changed go.sum",
			map[string]string{"daemon/go.sum": testGoSum + "x v1 h1:y=\n"},
			map[string]string{"daemon/go.sum": testGoSum},
			"daemon/go.sum",
		},
		{
			"changed go.mod",
			map[string]string{"daemon/go.mod": testGoMod + "require x v1\n"},
			map[string]string{"daemon/go.mod": testGoMod},
			"daemon/go.mod",
		},
		{
			"vendor directory",
			map[string]string{"daemon/vendor/modules.txt": "x\n"},
			map[string]string{"daemon/vendor": ""},
			"daemon/vendor",
		},
		{
			"go.work at the root",
			map[string]string{"go.work": "go 1.26\n"},
			map[string]string{"go.work": ""},
			"go.work",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drifted := observe(t, repo.commit(tc.changes))
			err := image.AdmissibleAt(drifted, PreparationDigest())
			if !errors.Is(err, domain.ErrProjectImageIncompatible) || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("AdmissibleAt = %v, want a refusal naming %q", err, tc.names)
			}
			if err := image.AdmissibleAt(observe(t, repo.commit(tc.revert)), PreparationDigest()); err != nil {
				t.Fatalf("restored commit refused: %v", err)
			}
		})
	}
	// A local-path replace is reported beside the changed go.mod
	// hash, and nothing about the npm layout is read for a Go image.
	replaced := observe(t, repo.commit(map[string]string{
		"daemon/go.mod": testGoMod + "replace example.test/dep => ../dep\n", "package.json": "{}",
	}))
	if !slices.Equal(replaced.UnsupportedInputs, []string{goLocalReplaceInput("daemon")}) ||
		replaced.PackageJSONSHA256 != "" {
		t.Fatalf("observed %+v, want only the local-path replace reported", replaced)
	}
	if want := path.Join("daemon", "go.mod") + " replace with a local path"; replaced.UnsupportedInputs[0] != want {
		t.Fatalf("unsupported input = %q, want %q", replaced.UnsupportedInputs[0], want)
	}
	if _, err := ObserveBaseInputs(t.Context(), "git", repo.dir, build,
		domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainGo, GoModuleDir: "../x"}); !errors.Is(
		err, domain.ErrProjectImageInvalid) {
		t.Fatalf("invalid layout = %v, want ErrProjectImageInvalid", err)
	}
}

func TestEvaluateRebuildRefusesEveryGoDependencyChange(t *testing.T) {
	const declared = `["registry.npmjs.org"]`
	repo := newBaseInputsRepo(t)
	base := repo.commit(map[string]string{
		"daemon/go.mod": testGoMod, "daemon/go.sum": testGoSum, "daemon/main.go": "package main\n",
		verify.DefaultRecipePath: testRecipe,
	})
	image := domain.ProjectImage{Environment: &domain.ProjectImageEnvironment{
		Toolchain: domain.ProjectToolchainGo, GoModuleDir: "daemon",
		GoModSHA256:       manifestSHA256([]byte(testGoMod)),
		GoSumSHA256:       manifestSHA256([]byte(testGoSum)),
		PreparationDigest: PreparationDigest(),
	}}
	for _, test := range []struct {
		name    string
		changes map[string]string
		needed  bool
		// detail lists what the refusal names.
		detail []string
	}{
		{name: "source-only head", changes: map[string]string{"daemon/main.go": "package main\n\nfunc main() {}\n"}},
		{
			// npm manifests are not a Go image's inputs.
			name:    "root package.json added",
			changes: map[string]string{"package.json": gateBasePackageJSON, "package-lock.json": gateBasePackageLock},
		},
		{
			name: "vendor added alone", needed: true,
			changes: map[string]string{"daemon/vendor/modules.txt": "x\n"},
			detail:  []string{"the candidate contains daemon/vendor"},
		},
		{
			name: "go.work added above the module", needed: true,
			changes: map[string]string{"go.work": "go 1.26\n"},
			detail:  []string{"the candidate contains go.work"},
		},
		{
			name: "go.sum changed", needed: true,
			changes: map[string]string{"daemon/go.sum": testGoSum + "example.test/x v1.0.0 h1:y=\n"},
			detail:  []string{"the candidate changes daemon/go.sum"},
		},
		{
			name: "go.mod and go.sum changed with a vendor directory", needed: true,
			changes: map[string]string{
				"daemon/go.mod": testGoMod + "require example.test/x v1.0.0\n",
				"daemon/go.sum": "", "daemon/vendor/modules.txt": "x\n",
			},
			detail: []string{"the candidate changes daemon/go.mod, daemon/go.sum and contains daemon/vendor"},
		},
		{
			name: "local-path replace", needed: true,
			changes: map[string]string{"daemon/go.mod": testGoMod + "replace example.test/dep => ../dep\n"},
			detail:  []string{"daemon/go.mod", goLocalReplaceInput("daemon")},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo.git("checkout", "-q", "--detach", base)
			head := repo.commit(test.changes)
			decision, err := EvaluateRebuild(t.Context(), "git", repo.dir, image, base, head, gatePolicy(t, declared))
			if err != nil {
				t.Fatalf("EvaluateRebuild: %v", err)
			}
			want := RebuildRefusal("")
			if test.needed {
				want = RebuildRefusalGoDependencyChanged
			}
			if decision.Needed != test.needed || decision.Refusal != want || decision.Head.CommitSHA != head {
				t.Fatalf("decision = %+v, want needed=%t refusal=%q", decision, test.needed, want)
			}
			for _, name := range test.detail {
				if !strings.Contains(decision.Detail, name) {
					t.Errorf("detail %q does not name %q", decision.Detail, name)
				}
			}
		})
	}
	t.Run("unreadable go.mod", func(t *testing.T) {
		repo.git("checkout", "-q", "--detach", base)
		repo.git("rm", "-q", "daemon/go.mod")
		repo.git("update-index", "--add", "--cacheinfo", "120000,"+
			repo.git("hash-object", "-w", "daemon/go.sum")+",daemon/go.mod")
		repo.git("commit", "-q", "-m", "symlink")
		head := repo.git("rev-parse", "HEAD")
		decision, err := EvaluateRebuild(t.Context(), "git", repo.dir, image, base, head, gatePolicy(t, declared))
		if err != nil || !decision.Needed || decision.Refusal != RebuildRefusalGoDependencyChanged {
			t.Fatalf("decision = %+v, %v; want the Go clause", decision, err)
		}
	})
}

func TestRebuildRequestKeepsTheDeclaredGoModule(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	image := rebuildTestImage(t, "127.0.0.1:5100/freeside-project-x@"+testImageDigest)
	image.Environment = &domain.ProjectImageEnvironment{
		Toolchain: domain.ProjectToolchainGo, GoModuleDir: "daemon",
		GoModSHA256: manifestSHA256([]byte(testGoMod)), GoSumSHA256: manifestSHA256([]byte(testGoSum)),
		PreparationDigest: PreparationDigest(),
	}
	request, err := RebuildRequest(image, head, "/checkout", []byte(testRecipe),
		BuildInputs{BaseBuildRef: "freeside-agent-claude:local"})
	if err != nil || request.GoModuleDir != "daemon" {
		t.Fatalf("RebuildRequest = %+v, %v; want the image's module directory", request, err)
	}
}
