package macverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
)

// runGit runs the host's git for a fixture, never inheriting a GIT_DIR or
// the developer's configuration.
func runGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: git from PATH on test fixtures
	cmd.Env = append(gitenv.Scrubbed(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// submoduleCheckout builds a checkout with a submodule that has its own
// submodule, as GRDB's SQLiteLib nests, and points each submodule's origin
// at an https URL, as a resolve's checkout would.
func submoduleCheckout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo := func(name string) string {
		path := filepath.Join(dir, name)
		runGit(t, "init", "--quiet", path)
		runGit(t, "-C", path, "commit", "--quiet", "--allow-empty", "-m", name)
		return path
	}
	addSubmodule := func(parent, child, name string) {
		runGit(t, "-C", parent, "-c", "protocol.file.allow=always", "submodule", "--quiet", "add", child, name)
		runGit(t, "-C", parent, "commit", "--quiet", "-m", "add "+name)
	}
	leaf, mid, top := repo("leaf"), repo("mid"), repo("top")
	addSubmodule(mid, leaf, "leaf")
	addSubmodule(top, mid, "mid")
	checkout := filepath.Join(dir, "checkout")
	runGit(t, "-c", "protocol.file.allow=always", "clone", "--quiet", "--recursive", top, checkout)
	runGit(t, "-C", filepath.Join(checkout, "mid"), "config", "remote.origin.url", "https://example.com/org/mid.git")
	runGit(t, "-C", filepath.Join(checkout, "mid", "leaf"), "config", "remote.origin.url", "https://example.com/org/leaf")
	return checkout
}

// fakePopulateTools plays xcrun's swift and xcodebuild: an online resolve
// clones the configured repositories and checkouts; the offline proof checks
// what it was given and may fail or rewrite a Package.resolved. git runs for
// real, except that a mirror clone makes an empty bare repository.
type fakePopulateTools struct {
	t *testing.T
	// swiftRepos and xcodeRepos are the origins the online resolve clones
	// into repositories/; checkout is copied into checkouts/.
	swiftRepos, xcodeRepos []string
	checkout               string
	toolchainErr           error
	// proofErr fails each offline resolve; rewriteResolved makes the offline
	// SwiftPM resolve rewrite its Package.resolved.
	proofErr        error
	rewriteResolved bool
	// artifact makes the online resolve download a binary artifact.
	artifact bool

	mu    sync.Mutex
	calls []toolCommand
}

func (f *fakePopulateTools) tools() populateTools {
	return populateTools{
		run: f.run,
		toolchain: func(context.Context) (Toolchain, error) {
			return testToolchain, f.toolchainErr
		},
	}
}

func (f *fakePopulateTools) commands() []toolCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func (f *fakePopulateTools) run(_ context.Context, c toolCommand) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	switch {
	case c.Path == gitPath && len(c.Args) > 2 && c.Args[0] == "clone":
		dest := c.Args[len(c.Args)-1]
		runGit(f.t, "init", "--quiet", "--bare", dest)
		return nil, nil
	case c.Path == gitPath:
		cmd := exec.Command("git", c.Args...) //nolint:gosec // G204: the fake runs the git command populate asked for, on test fixtures
		cmd.Env = c.Env
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git %v: %w", c.Args, err)
		}
		return out, nil
	case c.Path == xcrunPath && c.Args[0] == "swift":
		return nil, f.resolve(c, filepath.Join(flagValue(c.Args, "--package-path"), ".build"), f.swiftRepos,
			flagValue(c.Args, "--package-path"))
	case c.Path == xcrunPath && c.Args[0] == "xcodebuild":
		return nil, f.resolve(c, flagValue(c.Args, "-clonedSourcePackagesDirPath"), f.xcodeRepos, "")
	}
	return nil, fmt.Errorf("unexpected tool %s %v", c.Path, c.Args)
}

func (f *fakePopulateTools) resolve(c toolCommand, store string, repos []string, pkg string) error {
	if slices.Contains(c.Args, "--disable-sandbox") {
		return errors.New("populate turned SwiftPM's manifest sandbox off")
	}
	gitconfig := envValue(c.Env, "GIT_CONFIG_GLOBAL")
	if gitconfig != os.DevNull {
		// The offline proof.
		if envValue(c.Env, "HTTPS_PROXY") != closedProxy || envValue(c.Env, "https_proxy") != closedProxy {
			return errors.New("the offline proof left a proxy open")
		}
		if f.proofErr != nil {
			return f.proofErr
		}
		config, err := os.ReadFile(gitconfig) //nolint:gosec // G304: the seed gitconfig populate names
		if err != nil {
			return err
		}
		for _, url := range repos {
			if !strings.Contains(string(config), strings.TrimSuffix(url, ".git")) {
				return fmt.Errorf("no mirror serves %s", url)
			}
		}
		if pkg != "" {
			if _, err := os.Stat(filepath.Join(pkg, ".swiftpm", "configuration", "mirrors.json")); err != nil {
				return fmt.Errorf("the proof workspace has no seeded mirrors.json: %w", err)
			}
			if f.rewriteResolved {
				if err := os.WriteFile(filepath.Join(pkg, "Package.resolved"), []byte("{}"), 0o600); err != nil {
					return err
				}
			}
		}
		return os.MkdirAll(filepath.Join(store, "checkouts", "resolved-offline"), 0o750)
	}
	for i, url := range repos {
		repo := filepath.Join(store, "repositories", fmt.Sprintf("dep-%d", i))
		runGit(f.t, "init", "--quiet", "--bare", repo)
		runGit(f.t, "-C", repo, "config", "remote.origin.url", url)
	}
	if f.artifact {
		if err := os.MkdirAll(filepath.Join(store, "artifacts", "binary-dep"), 0o750); err != nil {
			return err
		}
	}
	if f.checkout != "" {
		return copyTree(f.checkout, filepath.Join(store, "checkouts", "with-submodules"), nil)
	}
	return nil
}

// cacheRoot makes an executor root with a swift-cache directory this user
// owns at 0755.
func cacheRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, swiftCacheDirName), 0o755); err != nil { //nolint:gosec // G301: the layout under test
		t.Fatal(err)
	}
	return root
}

func mirrorPath(root, key, url string) string {
	trimmed := strings.TrimSuffix(url, ".git")
	sum := sha256.Sum256([]byte(trimmed))
	return filepath.Join(swiftCacheEntry(root, key), "mirrors", hex.EncodeToString(sum[:]),
		filepath.Base(trimmed)+".git")
}

func TestPopulateSwiftCacheFillsAndProvesAnEntry(t *testing.T) {
	t.Parallel()
	root, source := cacheRoot(t), swiftWorkspace(t)
	// A developer checkout's own build output and git directory stay out.
	writeWorkspaceFile(t, source, "app/.build/checkouts/dep/Package.resolved", "{}")
	writeWorkspaceFile(t, source, ".git/Package.resolved", "{}")
	fake := &fakePopulateTools{
		t:          t,
		swiftRepos: []string{"https://example.com/org/alpha.git", "https://example.com/org/Beta"},
		xcodeRepos: []string{"https://example.com/org/alpha"},
		checkout:   submoduleCheckout(t),
	}

	res, err := populateSwiftCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, fake.tools())
	if err != nil {
		t.Fatalf("populate: %v", err)
	}
	clean := t.TempDir()
	writeWorkspaceFile(t, clean, "app/Package.swift", "// swift-tools-version:6.0\n")
	writeWorkspaceFile(t, clean, "app/Package.resolved", `{"pins":[],"version":3}`)
	writeWorkspaceFile(t, clean, xcodeResolved, `{"pins":[],"version":2}`)
	key := mustKey(t, clean)
	if !res.Populated || res.Key != key.Key || !slices.Equal(res.Files, key.paths()) {
		t.Fatalf("result = %+v, want the clean workspace's key %s populated", res, key.Key)
	}
	if err := lookupSwiftCache(root, key); err != nil {
		t.Fatalf("lookup after populate: %v", err)
	}

	entry := swiftCacheEntry(root, key.Key)
	var record swiftCacheRecord
	body, err := os.ReadFile(filepath.Join(entry, cacheRecordName)) //nolint:gosec // G304: the entry under test
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	wantURLs := []string{
		"https://example.com/org/Beta", "https://example.com/org/alpha", "https://example.com/org/alpha.git",
		"https://example.com/org/leaf", "https://example.com/org/mid.git",
	}
	if !slices.Equal(record.URLs, wantURLs) {
		t.Fatalf("discovered URLs = %v, want %v (repositories plus recursive submodules)", record.URLs, wantURLs)
	}
	if !slices.Equal(record.Packages, []string{"app"}) ||
		record.Container != "app/Freeside.xcodeproj" || record.Toolchain.XcodeBuild != testToolchain.XcodeBuild {
		t.Fatalf("record = %+v", record)
	}

	gitconfig := filepath.Join(entry, "seed", "home", ".gitconfig")
	var mirrorsJSON swiftpmMirrors
	body, err = os.ReadFile(filepath.Join(entry, "seed", "job", "workspace", "app", ".swiftpm", "configuration", "mirrors.json")) //nolint:gosec // G304: the entry under test
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &mirrorsJSON); err != nil {
		t.Fatal(err)
	}
	originals := map[string]string{}
	for _, m := range mirrorsJSON.Object {
		originals[m.Original] = m.Mirror
	}
	// alpha and alpha.git share one mirror; four mirrors in all.
	if len(mirrorsJSON.Object) != 8 || mirrorsJSON.Version != 1 {
		t.Fatalf("mirrors.json = %+v, want both spellings of four mirrors", mirrorsJSON)
	}
	for _, url := range wantURLs {
		mirror := mirrorPath(root, key.Key, url)
		if info, err := os.Stat(mirror); err != nil || !info.IsDir() {
			t.Fatalf("mirror of %s at %s: %v", url, mirror, err)
		}
		trimmed := strings.TrimSuffix(url, ".git")
		for _, spelling := range []string{trimmed, trimmed + ".git"} {
			if originals[spelling] != "file://"+mirror {
				t.Errorf("mirrors.json maps %s to %q, want file://%s", spelling, originals[spelling], mirror)
			}
			// git config --get-urlmatch is not portable across git versions;
			// read the rewrite back with the key git itself wrote.
			got := runGit(t, "config", "--file", gitconfig, "--get-all", "url.file://"+mirror+".insteadOf")
			if !slices.Contains(strings.Split(strings.TrimSpace(got), "\n"), spelling) {
				t.Errorf("gitconfig does not rewrite %s to %s: %q", spelling, mirror, got)
			}
		}
		safe := runGit(t, "config", "--file", gitconfig, "--get-all", "safe.directory")
		if !slices.Contains(strings.Split(strings.TrimSpace(safe), "\n"), mirror) {
			t.Errorf("gitconfig does not mark %s safe", mirror)
		}
	}
	if got := strings.TrimSpace(runGit(t, "config", "--file", gitconfig, "protocol.file.allow")); got != "always" {
		t.Errorf("protocol.file.allow = %q", got)
	}
	if _, err := os.Stat(filepath.Join(entry, "seed", "job", "source-packages", "checkouts", "resolved-offline")); err != nil {
		t.Errorf("the offline Xcode resolve did not write the seed SourcePackages: %v", err)
	}
	err = filepath.WalkDir(entry, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink == 0 && (info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o444 != 0o444) {
			t.Errorf("%s is %v, want readable by all and writable only by its owner", p, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fake.commands() {
		if c.Path == xcrunPath && envValue(c.Env, "GIT_CONFIG_GLOBAL") == "" {
			t.Errorf("%v ran with the operator's git configuration", c.Args)
		}
	}

	// A second run for the complete key changes nothing and runs nothing.
	before := len(fake.commands())
	again, err := populateSwiftCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, fake.tools())
	if err != nil || again.Populated || again.Key != key.Key {
		t.Fatalf("second populate = %+v, %v; want the complete entry left alone", again, err)
	}
	if after := len(fake.commands()); after != before {
		t.Fatalf("second populate ran %d tools", after-before)
	}
}

func TestPopulateSwiftCacheReplacesAnIncompleteEntry(t *testing.T) {
	t.Parallel()
	root, source := cacheRoot(t), swiftWorkspace(t)
	key := mustKey(t, source)
	leftover := filepath.Join(swiftCacheEntry(root, key.Key), "mirrors", "stale")
	if err := os.MkdirAll(leftover, 0o750); err != nil {
		t.Fatal(err)
	}
	fake := &fakePopulateTools{t: t, swiftRepos: []string{"https://example.com/org/alpha"}}
	res, err := populateSwiftCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, fake.tools())
	if err != nil || !res.Populated {
		t.Fatalf("populate = %+v, %v", res, err)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the incomplete entry's leftovers survived: %v", err)
	}
}

func TestPopulateSwiftCacheLeavesNoEntryWhenItFails(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fake      *fakePopulateTools
		workspace func(t *testing.T) string
		want      string
	}{
		"http URL": {
			fake: &fakePopulateTools{swiftRepos: []string{"http://example.com/org/alpha"}},
			want: "not an https URL",
		},
		"URL with a space": {
			fake: &fakePopulateTools{swiftRepos: []string{"https://example.com/org/al pha"}},
			want: "space or control character",
		},
		"URL with a control character": {
			fake: &fakePopulateTools{swiftRepos: []string{"https://example.com/org/al\tpha"}},
			want: "space or control character",
		},
		"URL with no repository name": {
			fake: &fakePopulateTools{swiftRepos: []string{"https://example.com/org/"}},
			want: "repository name",
		},
		"offline proof fails": {
			fake: &fakePopulateTools{
				swiftRepos: []string{"https://example.com/org/alpha"},
				proofErr:   errors.New("could not resolve host"),
			},
			want: "offline proof",
		},
		"offline proof rewrites Package.resolved": {
			fake:      &fakePopulateTools{swiftRepos: []string{"https://example.com/org/alpha"}, rewriteResolved: true},
			want:      "resolve app/Package.resolved differently",
			workspace: swiftWorkspace,
		},
		"registry pin": {
			workspace: func(t *testing.T) string {
				dir := swiftWorkspace(t)
				writeWorkspaceFile(t, dir, "app/Package.resolved",
					`{"pins":[{"identity":"swift-foo","kind":"registry","location":"","state":{}}],"version":3}`)
				return dir
			},
			want: `pins swift-foo as "registry"`,
		},
		"binary artifact downloaded": {
			fake: &fakePopulateTools{swiftRepos: []string{"https://example.com/org/alpha"}, artifact: true},
			want: "downloaded binary-dep",
		},
		"two Xcode containers": {
			workspace: func(t *testing.T) string {
				dir := swiftWorkspace(t)
				writeWorkspaceFile(t, dir, "Tools.xcworkspace/xcshareddata/swiftpm/Package.resolved", "{}")
				return dir
			},
			want: "Tools.xcworkspace, app/Freeside.xcodeproj",
		},
		"stray Package.resolved": {
			workspace: func(t *testing.T) string {
				dir := swiftWorkspace(t)
				writeWorkspaceFile(t, dir, "docs/Package.resolved", "{}")
				return dir
			},
			want: "docs/Package.resolved is neither",
		},
		"toolchain unreadable": {
			fake: &fakePopulateTools{toolchainErr: errors.New("xcrun: no developer tools")},
			want: "no developer tools",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := cacheRoot(t)
			workspace := tc.workspace
			if workspace == nil {
				workspace = swiftWorkspace
			}
			source := workspace(t)
			fake := tc.fake
			if fake == nil {
				fake = &fakePopulateTools{}
			}
			fake.t = t
			_, err := populateSwiftCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, fake.tools())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("populate error = %v, want one naming %q", err, tc.want)
			}
			entries, err := os.ReadDir(filepath.Join(root, swiftCacheDirName))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != ".populate.lock" {
					t.Fatalf("a failed populate left %s behind", e.Name())
				}
			}
		})
	}
}

func TestPopulateSwiftCacheRefusesAnUnsafeCacheDirectory(t *testing.T) {
	t.Parallel()
	source := swiftWorkspace(t)
	for name, setup := range map[string]func(t *testing.T, root string){
		"missing": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, swiftCacheDirName)); err != nil {
				t.Fatal(err)
			}
		},
		"group writable": func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, swiftCacheDirName), 0o775); err != nil { //nolint:gosec // G302: the unsafe mode under test
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, swiftCacheDirName)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(root, swiftCacheDirName)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := cacheRoot(t)
			setup(t, root)
			fake := &fakePopulateTools{t: t}
			if _, err := populateSwiftCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, fake.tools()); err == nil {
				t.Fatal("populate accepted the cache directory")
			}
			if calls := fake.commands(); len(calls) != 0 {
				t.Fatalf("populate ran tools: %v", calls)
			}
		})
	}
}

func TestPopulateSwiftCacheRefusesASourceWithoutPackageResolved(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeWorkspaceFile(t, source, "Package.swift", "// no dependencies\n")
	_, err := populateSwiftCache(t.Context(), PopulateOptions{ExecutorRoot: cacheRoot(t), Source: source},
		(&fakePopulateTools{t: t}).tools())
	if err == nil || !strings.Contains(err.Error(), "holds no Package.resolved") {
		t.Fatalf("populate error = %v", err)
	}
}
