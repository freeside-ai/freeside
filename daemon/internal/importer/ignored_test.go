package importer

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func TestImportExcludesBaseIgnoredBuildOutput(t *testing.T) {
	baseFiles := map[string]string{".gitignore": "dist/\n", "src/main.ts": "old\n"}
	checkout, base := initBaseRepo(t, baseFiles)
	workspace := t.TempDir()
	for path, body := range map[string]string{
		".gitignore": "dist/\n", "src/main.ts": "new\n", "dist/main.js": "generated\n",
	} {
		full := filepath.Join(workspace, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handoff := exportWorkspace(t, workspace)
	opts := testImportOptions(base)
	opts.Policy.Allowlist = []string{"src/*.ts"}
	clone := cloneAtBase(t, checkout)
	result, err := Import(t.Context(), handoff, clone, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 0 || len(result.Changes) != 1 || result.Changes[0].Path != "src/main.ts" {
		t.Fatalf("candidate = %+v, want only the allowed source edit", result)
	}
	if tree := rungit(t, clone, "ls-tree", "-r", "--name-only", result.CommitSHA); strings.Contains(tree, "dist/") {
		t.Fatalf("generated output reached the commit: %s", tree)
	}
	replay, err := Import(t.Context(), handoff, cloneAtBase(t, checkout), opts)
	if err != nil || replay.CommitSHA != result.CommitSHA {
		t.Fatalf("replay = %+v, %v; want head %s", replay, err, result.CommitSHA)
	}
}

func TestImportIgnoredOutputKeepsScopeAndSecurityGates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rules   string
		path    string
		body    string
		kind    FindingKind
		wantErr error
		prepare func(*testing.T, string, map[string]string, *Options)
	}{
		{name: "unignored", rules: "other/\n", path: "dist/out.js", body: "generated", kind: FindingAllowlistViolation},
		{name: "negated", rules: "dist/*\n!dist/keep.js\n", path: "dist/keep.js", body: "keep", kind: FindingAllowlistViolation},
		{
			name: "candidate rule", rules: "other/\n", path: "dist/out.js", body: "generated", kind: FindingAllowlistViolation,
			prepare: func(_ *testing.T, _ string, files map[string]string, _ *Options) { files[".gitignore"] = "*\n" },
		},
		{name: "secret", rules: "dist/\n", path: "dist/out.js", body: "sk-ant-" + strings.Repeat("X", 20), kind: FindingSecret},
		{
			name: "scan cap", rules: "dist/\n", path: "dist/out.js", body: strings.Repeat("x", 100), kind: FindingSecretScanSkipped,
			prepare: func(_ *testing.T, _ string, _ map[string]string, opts *Options) { opts.Policy.SecretMaxScanBytes = 50 },
		},
		{
			name: "size", rules: "dist/\n", path: "dist/out.js", body: strings.Repeat("x", 100), wantErr: ErrBlobTooLarge,
			prepare: func(_ *testing.T, _ string, _ map[string]string, opts *Options) { opts.Policy.MaxBlobBytes = 50 },
		},
		{name: "automation", rules: ".github/\n", path: ".github/workflows/new.yml", body: "name: candidate", kind: FindingAutomationControlPath},
		{
			name: "ambient rules", rules: "other/\n", path: "dist/out.js", body: "generated", kind: FindingAllowlistViolation,
			prepare: func(t *testing.T, clone string, _ map[string]string, _ *Options) {
				for _, name := range []string{".gitignore", ".git/info/exclude"} {
					if err := os.WriteFile(filepath.Join(clone, name), []byte("*\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				global := filepath.Join(t.TempDir(), "ignore")
				if err := os.WriteFile(global, []byte("*\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				rungit(t, clone, "config", "core.excludesFile", global)
				t.Setenv("GIT_CONFIG_COUNT", "1")
				t.Setenv("GIT_CONFIG_KEY_0", "core.excludesFile")
				t.Setenv("GIT_CONFIG_VALUE_0", global)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseFiles := map[string]string{".gitignore": tc.rules, "src/main.ts": "old"}
			checkout, base := initBaseRepo(t, baseFiles)
			clone := cloneAtBase(t, checkout)
			files := maps.Clone(baseFiles)
			files[tc.path] = tc.body
			files["src/main.ts"] = "new"
			opts := testImportOptions(base)
			opts.Policy.Allowlist = []string{"src/*.ts"}
			if tc.prepare != nil {
				tc.prepare(t, clone, files, &opts)
			}
			result, err := Import(t.Context(), ignoredFixtureHandoff(t, files), clone, opts)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(result.Findings, func(f Finding) bool { return f.Kind == tc.kind && f.Path == tc.path }) {
				t.Fatalf("findings = %+v, want %s on %s", result.Findings, tc.kind, tc.path)
			}
		})
	}
}

func TestImportIgnoredOutputPreservesTrackedAndInScopeFiles(t *testing.T) {
	baseFiles := map[string]string{".gitignore": "dist/\n", "dist/tracked.js": "old", "dist/deleted.js": "delete"}
	checkout, _ := initBaseRepo(t, baseFiles)
	rungit(t, checkout, "add", "-f", "--", "dist")
	rungit(t, checkout, "commit", "-qm", "track base outputs")
	base := rungit(t, checkout, "rev-parse", "HEAD")
	files := map[string]string{".gitignore": "dist/\n", "dist/tracked.js": "new", "dist/allowed.js": "allowed", "dist/debris.js": "ignored"}
	opts := testImportOptions(base)
	opts.Policy.Allowlist = []string{"dist/allowed.js"}
	result, err := Import(t.Context(), ignoredFixtureHandoff(t, files), cloneAtBase(t, checkout), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 3 || slices.ContainsFunc(result.Changes, func(c Change) bool { return c.Path == "dist/debris.js" }) {
		t.Fatalf("changes = %+v", result.Changes)
	}
	for _, name := range []string{"dist/tracked.js", "dist/deleted.js"} {
		if !slices.ContainsFunc(result.Findings, func(f Finding) bool { return f.Kind == FindingAllowlistViolation && f.Path == name }) {
			t.Fatalf("tracked path %s lost its scope check: %+v", name, result.Findings)
		}
	}
}

func TestImportUsesNestedBaseIgnoreRules(t *testing.T) {
	baseFiles := map[string]string{".gitignore": "*.js\n", "pkg/.gitignore": "!keep.js\n", "src/main.ts": "old"}
	checkout, base := initBaseRepo(t, baseFiles)
	files := maps.Clone(baseFiles)
	files["pkg/keep.js"], files["pkg/drop.js"], files["root.js"] = "keep", "drop", "drop"
	files["src/main.ts"] = "new"
	opts := testImportOptions(base)
	opts.Policy.Allowlist = []string{"src/*.ts"}
	result, err := Import(t.Context(), ignoredFixtureHandoff(t, files), cloneAtBase(t, checkout), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 2 || len(result.Findings) != 1 || result.Findings[0].Path != "pkg/keep.js" {
		t.Fatalf("nested rules: %+v", result)
	}
}

func TestIgnoreMatcherDoesNotInventDirectoryCandidates(t *testing.T) {
	baseFiles := map[string]string{".gitignore": "dist/\nhome/\n", "dist/.gitignore": "*.js\n"}
	checkout, _ := initBaseRepo(t, baseFiles)
	rungit(t, checkout, "add", "-f", "--", "dist/.gitignore")
	rungit(t, checkout, "commit", "-qm", "track nested rule")
	base := rungit(t, checkout, "rev-parse", "HEAD")
	files := map[string]string{".gitignore": baseFiles[".gitignore"], "dist": "regular file", "home": "regular file"}
	opts := testImportOptions(base)
	opts.Policy.Allowlist = []string{"dist/.gitignore"}
	result, err := Import(t.Context(), ignoredFixtureHandoff(t, files), cloneAtBase(t, checkout), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dist", "home"} {
		if !slices.ContainsFunc(result.Findings, func(f Finding) bool { return f.Kind == FindingAllowlistViolation && f.Path == name }) {
			t.Fatalf("directory rule incorrectly ignored file %s: %+v", name, result)
		}
	}
}

func TestIgnoredOutputDoesNotChangeNoChangesOrSpecificationMode(t *testing.T) {
	for _, specification := range []bool{false, true} {
		t.Run(map[bool]string{false: "no changes", true: "specification"}[specification], func(t *testing.T) {
			files := map[string]string{".gitignore": "dist/\n"}
			checkout, base := initBaseRepo(t, files)
			files["dist/out.js"] = "generated"
			opts := testImportOptions(base)
			opts.Policy.Allowlist = []string{"src/*.ts"}
			if specification {
				profile := FindingProfileSpecification
				opts.Policy.FindingProfile = &profile
			} else {
				opts.ExpectNoChanges = true
			}
			result, err := Import(t.Context(), ignoredFixtureHandoff(t, files), cloneAtBase(t, checkout), opts)
			if !specification {
				if !errors.Is(err, ErrUnexpectedChanges) {
					t.Fatalf("no-changes error = %v", err)
				}
			} else if err != nil || len(result.Changes) != 1 || len(result.Findings) != 1 {
				t.Fatalf("specification result changed: %+v, %v", result, err)
			}
		})
	}
}

func ignoredFixtureHandoff(t *testing.T, files map[string]string) string {
	t.Helper()
	var entries []export.Entry
	var bodies []string
	for name, body := range files {
		entries = append(entries, regularEntryFor(name, body, false))
		bodies = append(bodies, body)
	}
	return handoffFromEntries(t, entries, bodies...)
}

func TestIgnoredOutputRetainsNonRegularAndIntegrityChecks(t *testing.T) {
	files := map[string]string{".gitignore": "dist/\n"}
	checkout, base := initBaseRepo(t, files)
	opts := testImportOptions(base)
	opts.Policy.Allowlist = []string{"src/*.ts"}
	target := "../src/main.ts"
	entries := []export.Entry{regularEntryFor(".gitignore", files[".gitignore"], false), {Path: "dist/link", Kind: export.EntrySymlink, Target: &target}}
	result, err := Import(t.Context(), handoffFromEntries(t, entries, files[".gitignore"]), cloneAtBase(t, checkout), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.CommitSHA != "" || !slices.ContainsFunc(result.Findings, func(f Finding) bool { return f.Kind == FindingNonRegularChange && f.Path == "dist/link" }) {
		t.Fatalf("ignored symlink lost its gate: %+v", result)
	}
	files["dist/out.js"] = "generated"
	handoff := ignoredFixtureHandoff(t, files)
	digest := strings.TrimPrefix(string(sha256Digest("generated")), "sha256:")
	if err := os.WriteFile(filepath.Join(handoff, "blobs", "sha256", digest), []byte("tampered!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(t.Context(), handoff, cloneAtBase(t, checkout), opts); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("ignored corrupt blob was not refused: %v", err)
	}
}

func TestIgnoreMatcherRejectsDirectoryAliases(t *testing.T) {
	files := map[string]string{"Build/.gitignore": "*.js\n"}
	checkout, base := initBaseRepo(t, files)
	files["Build/out.js"], files["build/other.js"] = "first", "second"
	opts := testImportOptions(base)
	opts.Policy.Allowlist = []string{"src/*.ts"}
	if _, err := Import(t.Context(), ignoredFixtureHandoff(t, files), cloneAtBase(t, checkout), opts); !errors.Is(err, ErrUnsupportedRepo) {
		t.Fatalf("ambiguous directory spelling was not refused: %v", err)
	}
}

func TestIgnoreSpellingsKeepAncestorGroupsTogether(t *testing.T) {
	eligible := map[string]bool{"Foo/out.js": true, "foo-bridge/out.js": true, "foo/sub/out.js": true}
	if err := validateIgnoreSpellings([]string{".gitignore"}, eligible); !errors.Is(err, ErrUnsupportedRepo) {
		t.Fatalf("ancestor spelling alias was not refused: %v", err)
	}
}

func BenchmarkIgnoreRulesDeepPaths(b *testing.B) {
	base := map[string]treeEntry{".gitignore": {mode: "100644"}}
	eligible := make(map[string]bool)
	for i := range 1000 {
		eligible[fmt.Sprintf("%04d/", i)+strings.Repeat("nested-component/", 248)+"out.js"] = true
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rules := selectIgnoreRules(base, eligible)
		if len(rules) != 1 {
			b.Fatalf("selected %d rules, want only the root rule", len(rules))
		}
		if err := validateIgnoreSpellings(rules, eligible); err != nil {
			b.Fatal(err)
		}
	}
}

func TestIgnoredOutputNeverEntersCommitPlan(t *testing.T) {
	for _, namesExcluded := range []bool{false, true} {
		t.Run(fmt.Sprint(namesExcluded), func(t *testing.T) {
			paths := `"src/main.ts"`
			if namesExcluded {
				paths += `,"dist/out.js"`
			}
			plan := []byte(`{"version":"freeside.commit-plan/v1","groups":[` +
				`{"name":"source","message":"Update source","paths":[` + paths + `]},` +
				`{"name":"rest","message":"Update remaining source","remainder":true}]}`)
			result, err, clone, base := runCommitPlanImport(t,
				map[string]string{".gitignore": "dist/\n", "src/main.ts": "old"},
				map[string]string{".gitignore": "dist/\n", "src/main.ts": "new", "src/extra.ts": "added", "dist/out.js": "generated"},
				plan, domain.CommitPlanPlanPreferred, func(opts *Options, _ string) { opts.Policy.Allowlist = []string{"src/*.ts"} })
			if err != nil || result.CommitSHA == "" || len(result.Findings) != 0 {
				t.Fatalf("import = %+v, %v", result, err)
			}
			if namesExcluded {
				if !noticeIs(result, domain.CommitPlanNoticeStructural) {
					t.Fatalf("excluded path did not trigger structural fallback: %+v", result)
				}
			} else if result.CommitPlanNotice != nil || rungit(t, clone, "rev-list", "--count", base+".."+result.CommitSHA) != "2" {
				t.Fatalf("valid two-commit plan was not honored: %+v", result)
			}
			for _, commit := range strings.Fields(rungit(t, clone, "rev-list", base+".."+result.CommitSHA)) {
				if tree := rungit(t, clone, "ls-tree", "-r", "--name-only", commit); strings.Contains(tree, "dist/") {
					t.Fatalf("ignored output reached intermediate commit %s: %s", commit, tree)
				}
			}
		})
	}
}

func TestIgnoreMatcherRejectsMalformedOutput(t *testing.T) {
	for _, output := range []string{
		"", ".gitignore\x001\x00*\x00dist/out.js", ".gitignore\x001\x00*\x00unknown.js\x00",
		".gitignore\x001\x00*\x00dist/out.js\x00.gitignore\x001\x00*\x00dist/out.js\x00",
	} {
		if _, err := decodeIgnoredAdditions([]byte(output), map[string]bool{"dist/out.js": true}, []string{".gitignore"}); !errors.Is(err, ErrGitPlumbing) {
			t.Fatalf("malformed output %q was not refused: %v", output, err)
		}
	}
}
