package macverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// goWorkspace is a workspace shaped like Freeside's: two Go modules, one
// with a go.sum, beside an app with no Go.
func goWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeWorkspaceFile(t, dir, "daemon/go.mod", "module example.com/daemon\n\ngo 1.27.4\n")
	writeWorkspaceFile(t, dir, "daemon/go.sum", "example.com/dep v1.0.0 h1:abc=\n")
	writeWorkspaceFile(t, dir, "scripts/tool/go.mod", "module example.com/tool\n\ngo 1.26\n")
	writeWorkspaceFile(t, dir, "app/Package.swift", "// swift-tools-version:6.0\n")
	return dir
}

func mustGoKey(t *testing.T, dir string) goCacheKey {
	t.Helper()
	key, err := workspaceGoCacheKey(dir)
	if err != nil {
		t.Fatalf("workspaceGoCacheKey: %v", err)
	}
	return key
}

func TestGoCacheKeyCoversEveryGoModAndGoSum(t *testing.T) {
	t.Parallel()
	dir := goWorkspace(t)
	key := mustGoKey(t, dir)
	if want := []string{"daemon/go.mod", "daemon/go.sum", "scripts/tool/go.mod"}; !slices.Equal(key.paths(), want) {
		t.Fatalf("keyed files = %v, want %v", key.paths(), want)
	}
	if want := []string{"daemon", "scripts/tool"}; !slices.Equal(key.modules(), want) {
		t.Fatalf("modules = %v, want %v", key.modules(), want)
	}
	if !swiftCacheKeyPattern.MatchString(key.Key) {
		t.Fatalf("key %q is not 64 lowercase hex", key.Key)
	}

	writeWorkspaceFile(t, dir, "daemon/go.sum", "example.com/dep v1.0.1 h1:def=\n")
	if changed := mustGoKey(t, dir); changed.Key == key.Key {
		t.Fatal("a changed go.sum kept the key")
	}

	root := t.TempDir()
	writeWorkspaceFile(t, root, "go.mod", "module example.com/root\n")
	if got := mustGoKey(t, root).modules(); !slices.Equal(got, []string{""}) {
		t.Fatalf("a root go.mod's modules = %q, want the workspace root", got)
	}
}

func TestGoCacheKeySkipsTestdataGitAndBuildDirectories(t *testing.T) {
	t.Parallel()
	dir := goWorkspace(t)
	want := mustGoKey(t, dir).Key
	for _, rel := range []string{"daemon/testdata/mod/go.mod", ".git/go.mod", "app/.build/checkouts/dep/go.mod"} {
		writeWorkspaceFile(t, dir, rel, "module example.com/skipped\n")
	}
	if got := mustGoKey(t, dir).Key; got != want {
		t.Fatalf("a skipped directory's go.mod changed the key: %s, want %s", got, want)
	}
}

func TestGoCacheKeyWithoutGoModIsEmpty(t *testing.T) {
	t.Parallel()
	if key := mustGoKey(t, swiftWorkspace(t)); key.Key != "" || len(key.Files) != 0 {
		t.Fatalf("key = %+v, want none", key)
	}
}

func TestGoCacheKeyRefusesASymlinkedGoMod(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkspaceFile(t, dir, "elsewhere/real.mod", "module example.com/x\n")
	if err := os.MkdirAll(filepath.Join(dir, "mod"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "elsewhere", "real.mod"), filepath.Join(dir, "mod", "go.mod")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceGoCacheKey(dir); err == nil {
		t.Fatal("a symlinked go.mod was keyed")
	}
}

// writeGoToolchain puts a stand-in for the pinned toolchain under root.
func writeGoToolchain(t *testing.T, root string) {
	t.Helper()
	bin := filepath.Join(goToolchainRoot(root), "bin")
	if err := os.MkdirAll(bin, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: a stand-in toolchain binary
		t.Fatal(err)
	}
}

func writeGoEntry(t *testing.T, root, key string) {
	t.Helper()
	entry := goCacheEntry(root, key)
	if err := os.MkdirAll(filepath.Join(entry, goCacheModcache), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeRecord(entry, goCacheRecord{Format: goCacheRecordFormat, Key: key, Go: goToolchainVersion}); err != nil {
		t.Fatal(err)
	}
}

func replaceGoRecord(t *testing.T, root, key string, record goCacheRecord) {
	t.Helper()
	entry := goCacheEntry(root, key)
	if err := os.Remove(filepath.Join(entry, cacheRecordName)); err != nil {
		t.Fatal(err)
	}
	if err := writeRecord(entry, record); err != nil {
		t.Fatal(err)
	}
}

func TestRunServesTheGoToolchainAndACompleteGoCacheEntry(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	workdir := goWorkspace(t)
	key := mustGoKey(t, workdir)
	writeGoToolchain(t, room.root)
	writeGoEntry(t, room.root, key.Key)

	if _, err := room.Run(t.Context(), workdir, []string{"go", "test", "./..."}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	prepares, runs := fake.recipeJobs()
	if prepares[0].caches != (jobCaches{Go: key.Key}) {
		t.Fatalf("prepare caches = %+v, want Go %s only", prepares[0].caches, key.Key)
	}
	home := filepath.Join(fake.root, "homes", "slot1")
	want := []string{
		"HOME=" + home, "TMPDIR=/private/var/folders/ab/slot1/T",
		"PATH=" + filepath.Join(fake.root, goCacheDirName, "go"+goToolchainVersion, "bin") + ":" + commandPath,
		"LC_ALL=C", "GOFLAGS=-mod=readonly",
		"GOMODCACHE=" + filepath.Join(fake.root, goCacheDirName, key.Key, goCacheModcache),
		"GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off",
	}
	if !slices.Equal(runs[0].Env, want) {
		t.Fatalf("environment = %v, want exactly %v", runs[0].Env, want)
	}
}

func TestRunRefusesAMissingOrIncompleteGoCacheEntry(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(t *testing.T, root, key string){
		"missing entry": func(t *testing.T, root, _ string) { writeGoToolchain(t, root) },
		"missing toolchain": func(t *testing.T, root, key string) {
			writeGoEntry(t, root, key)
		},
		"toolchain without a go binary": func(t *testing.T, root, key string) {
			writeGoEntry(t, root, key)
			if err := os.MkdirAll(filepath.Join(goToolchainRoot(root), "bin", "go"), 0o750); err != nil {
				t.Fatal(err)
			}
		},
		"no record": func(t *testing.T, root, key string) {
			writeGoToolchain(t, root)
			writeGoEntry(t, root, key)
			if err := os.Remove(filepath.Join(goCacheEntry(root, key), cacheRecordName)); err != nil {
				t.Fatal(err)
			}
		},
		"no module cache": func(t *testing.T, root, key string) {
			writeGoToolchain(t, root)
			writeGoEntry(t, root, key)
			if err := os.Remove(filepath.Join(goCacheEntry(root, key), goCacheModcache)); err != nil {
				t.Fatal(err)
			}
		},
		"record for another key": func(t *testing.T, root, key string) {
			writeGoToolchain(t, root)
			writeGoEntry(t, root, key)
			replaceGoRecord(t, root, key, goCacheRecord{
				Format: goCacheRecordFormat, Key: strings.Repeat("0", 64), Go: goToolchainVersion,
			})
		},
		"record from another Go": func(t *testing.T, root, key string) {
			writeGoToolchain(t, root)
			writeGoEntry(t, root, key)
			replaceGoRecord(t, root, key, goCacheRecord{Format: goCacheRecordFormat, Key: key, Go: "1.26.0"})
		},
		"symlinked entry": func(t *testing.T, root, key string) {
			writeGoToolchain(t, root)
			elsewhere := t.TempDir()
			writeGoEntry(t, elsewhere, key)
			if err := os.Symlink(goCacheEntry(elsewhere, key), goCacheEntry(root, key)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeLauncher()
			room := newTestRoom(t, fake)
			workdir := goWorkspace(t)
			key := mustGoKey(t, workdir)
			if err := os.MkdirAll(goCacheDir(room.root), 0o750); err != nil {
				t.Fatal(err)
			}
			setup(t, room.root, key.Key)

			_, err := room.Run(t.Context(), workdir, []string{"go", "test", "./..."})
			if !errors.Is(err, ErrGoCacheMissing) {
				t.Fatalf("Run error = %v, want ErrGoCacheMissing", err)
			}
			for _, want := range []string{key.Key, "daemon/go.mod", "scripts/tool/go.mod", "go-cache populate"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
			if calls, _, _ := fake.snapshot(); len(calls) != 0 {
				t.Fatalf("the room reached the helper: %v", calls)
			}
			if _, err := room.pool.lease(); err != nil {
				t.Fatalf("the refused job held its slot: %v", err)
			}
		})
	}
}

func TestRunRefusesAPrepareThatSelectedAnotherGoCache(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	fake.reportCaches = &jobCaches{Go: strings.Repeat("0", 64)}
	room := newTestRoom(t, fake)
	workdir := goWorkspace(t)
	writeGoToolchain(t, room.root)
	writeGoEntry(t, room.root, mustGoKey(t, workdir).Key)

	_, err := room.Run(t.Context(), workdir, []string{"go", "test", "./..."})
	if err == nil || !strings.Contains(err.Error(), "Go cache") || errors.Is(err, ErrAbsenceUnproven) {
		t.Fatalf("Run error = %v, want a refusal of the selected cache", err)
	}
	calls, _, _ := fake.snapshot()
	if want := withProbe("prepare slot1", "reap slot1", "cleanup slot1"); !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v: no run, then a teardown", calls, want)
	}
}

// fakeGoArchive stands in for the pinned toolchain archive.
var fakeGoArchive = []byte("a fake Go toolchain archive")

// fakeGoTools plays the toolchain download, tar, and the go command. tar
// makes a toolchain with an owner-only file in it; go mod download writes a
// read-only module into GOMODCACHE, as the go command does.
type fakeGoTools struct {
	sum      string // the pinned digest; defaults to fakeGoArchive's
	proofErr error

	mu      sync.Mutex
	calls   []toolCommand
	fetches int
}

func (f *fakeGoTools) tools() goPopulateTools {
	return goPopulateTools{
		run: f.run,
		fetch: func(_ context.Context, url string, w io.Writer) error {
			f.mu.Lock()
			f.fetches++
			f.mu.Unlock()
			if url != goToolchainBaseURL+"go-test.tar.gz" {
				return fmt.Errorf("fetched %s", url)
			}
			_, err := w.Write(fakeGoArchive)
			return err
		},
		pin: func() (string, string, error) {
			if f.sum != "" {
				return "go-test.tar.gz", f.sum, nil
			}
			sum := sha256.Sum256(fakeGoArchive)
			return "go-test.tar.gz", hex.EncodeToString(sum[:]), nil
		},
	}
}

func (f *fakeGoTools) commands() []toolCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeGoTools) run(_ context.Context, c toolCommand) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if c.Path == tarPath {
		archive, dest := flagValue(c.Args, "-f"), flagValue(c.Args, "-C")
		if body, err := os.ReadFile(archive); err != nil || string(body) != string(fakeGoArchive) { //nolint:gosec // G304: the archive populate names
			return nil, fmt.Errorf("tar read %s: %w", archive, err)
		}
		bin := filepath.Join(dest, "go", "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: the fake toolchain's binary
			return nil, err
		}
		return nil, os.WriteFile(filepath.Join(dest, "go", "VERSION"), []byte("go"+goToolchainVersion), 0o600)
	}
	if !strings.HasSuffix(c.Path, filepath.Join(goCacheDirName, "go"+goToolchainVersion, "bin", "go")) {
		return nil, fmt.Errorf("unexpected tool %s %v", c.Path, c.Args)
	}
	for k, want := range map[string]string{"GOTOOLCHAIN": "local", "GOWORK": "off", "GOENV": "off", "GOFLAGS": "-mod=readonly -modcacherw"} {
		if got := envValue(c.Env, k); got != want {
			return nil, fmt.Errorf("%s=%q, want %q", k, got, want)
		}
	}
	modcache := envValue(c.Env, "GOMODCACHE")
	switch {
	case slices.Contains(c.Args, "download"):
		if envValue(c.Env, "GOPROXY") != "https://proxy.golang.org" || envValue(c.Env, "GOSUMDB") != "sum.golang.org" {
			return nil, errors.New("the download ran without the proxy and checksum database")
		}
		mod := filepath.Join(modcache, "example.com", "dep@v1.0.0")
		if err := os.MkdirAll(mod, 0o750); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(filepath.Join(mod, "go.mod")); err == nil {
			return nil, nil
		}
		return nil, os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module example.com/dep\n"), 0o444) //nolint:gosec // G306: read-only, as the go command writes it
	case slices.Contains(c.Args, "list"):
		if envValue(c.Env, "GOPROXY") != "off" || envValue(c.Env, "GOSUMDB") != "off" {
			return nil, errors.New("the offline proof could reach the proxy")
		}
		if envValue(c.Env, "HTTPS_PROXY") != closedProxy || envValue(c.Env, "https_proxy") != closedProxy {
			return nil, errors.New("the offline proof left a proxy open")
		}
		if _, err := os.Stat(filepath.Join(modcache, "example.com", "dep@v1.0.0")); err != nil {
			return nil, fmt.Errorf("the proof found no downloaded module: %w", err)
		}
		return nil, f.proofErr
	}
	return nil, fmt.Errorf("unexpected go %v", c.Args)
}

// goCacheRoot makes an executor root with a go-cache directory this user
// owns at 0755.
func goCacheRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(goCacheDir(root), 0o755); err != nil { //nolint:gosec // G301: the layout under test
		t.Fatal(err)
	}
	return root
}

// goArgs is each go command's arguments after -C <module>, keyed by module.
func goArgs(t *testing.T, calls []toolCommand) []string {
	t.Helper()
	var got []string
	for _, c := range calls {
		if c.Path == tarPath {
			got = append(got, "tar")
			continue
		}
		dir := flagValue(c.Args, "-C")
		got = append(got, filepath.Base(dir)+" "+strings.Join(c.Args[2:], " "))
	}
	return got
}

func TestPopulateGoCacheInstallsTheToolchainAndProvesAnEntry(t *testing.T) {
	t.Parallel()
	root, source := goCacheRoot(t), goWorkspace(t)
	fake := &fakeGoTools{}

	res, err := populateGoCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, fake.tools())
	if err != nil {
		t.Fatalf("populate: %v", err)
	}
	key := mustGoKey(t, source)
	if res.Key != key.Key || !res.Populated || !slices.Equal(res.Files, key.paths()) {
		t.Fatalf("result = %+v, want a populated entry for %s", res, key.Key)
	}
	want := []string{
		"tar", "daemon mod download", "tool mod download",
		"daemon list -deps -test ./...", "tool list -deps -test ./...",
	}
	if got := goArgs(t, fake.commands()); !slices.Equal(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	if err := lookupGoCache(root, key); err != nil {
		t.Fatalf("lookup after populate: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(goCacheEntry(root, key.Key), cacheRecordName))
	if err != nil {
		t.Fatal(err)
	}
	var record goCacheRecord
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if record.Go != goToolchainVersion || len(record.Files) != len(key.Files) {
		t.Fatalf("record = %+v", record)
	}
	// Slots read both in place: every directory 0755, every file 0644 or,
	// when executable, 0755.
	for _, dir := range []string{goToolchainRoot(root), goCacheEntry(root, key.Key)} {
		if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if perm := info.Mode().Perm(); perm != 0o755 && perm != 0o644 {
				t.Errorf("%s has mode %v", p, perm)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if leftover, _ := filepath.Glob(filepath.Join(goCacheDir(root), ".go-toolchain-*")); len(leftover) != 0 {
		t.Fatalf("staging left behind: %v", leftover)
	}

	again := &fakeGoTools{}
	res, err = populateGoCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, again.tools())
	if err != nil || res.Populated {
		t.Fatalf("second populate = %+v, %v; want the complete entry left alone", res, err)
	}
	if calls := again.commands(); len(calls) != 0 || again.fetches != 0 {
		t.Fatalf("second populate ran %v and fetched %d times", calls, again.fetches)
	}
}

func TestPopulateGoCacheRefusesAGoNewerThanThePin(t *testing.T) {
	t.Parallel()
	for name, line := range map[string]string{
		"go line":        "go 1.99",
		"toolchain line": "toolchain go1.99.0",
		"not a version":  "go banana",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, source := goCacheRoot(t), goWorkspace(t)
			writeWorkspaceFile(t, source, "scripts/tool/go.mod", "module example.com/tool\n\n"+line+"\n")
			fake := &fakeGoTools{}
			_, err := populateGoCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, fake.tools())
			if err == nil || !strings.Contains(err.Error(), "scripts/tool/go.mod") {
				t.Fatalf("populate error = %v, want a refusal naming scripts/tool/go.mod", err)
			}
			if fake.fetches != 0 || len(fake.commands()) != 0 {
				t.Fatal("populate fetched or ran a tool before refusing")
			}
		})
	}
}

func TestPopulateGoCacheRefusesAnArchiveWithAnotherDigest(t *testing.T) {
	t.Parallel()
	root, source := goCacheRoot(t), goWorkspace(t)
	fake := &fakeGoTools{sum: strings.Repeat("0", 64)}
	_, err := populateGoCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, fake.tools())
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("populate error = %v, want a digest refusal", err)
	}
	if len(fake.commands()) != 0 {
		t.Fatalf("populate ran %v after a digest mismatch", goArgs(t, fake.commands()))
	}
	if _, err := os.Lstat(goToolchainRoot(root)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a toolchain is in place after a digest mismatch: %v", err)
	}
}

func TestPopulateGoCacheRefusesAnIncompleteToolchainDirectory(t *testing.T) {
	t.Parallel()
	root, source := goCacheRoot(t), goWorkspace(t)
	if err := os.MkdirAll(goToolchainRoot(root), 0o750); err != nil {
		t.Fatal(err)
	}
	fake := &fakeGoTools{}
	_, err := populateGoCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, fake.tools())
	if err == nil || !strings.Contains(err.Error(), "remove it") {
		t.Fatalf("populate error = %v, want a refusal of the incomplete toolchain", err)
	}
	if fake.fetches != 0 {
		t.Fatal("populate fetched over an incomplete toolchain")
	}
}

func TestPopulateGoCacheLeavesNoEntryWhenTheProofFails(t *testing.T) {
	t.Parallel()
	root, source := goCacheRoot(t), goWorkspace(t)
	fake := &fakeGoTools{proofErr: errors.New("module example.com/dep: GOPROXY=off")}
	_, err := populateGoCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, fake.tools())
	if err == nil || !strings.Contains(err.Error(), "offline proof of daemon") {
		t.Fatalf("populate error = %v, want the offline proof's failure", err)
	}
	key := mustGoKey(t, source)
	if _, err := os.Lstat(goCacheEntry(root, key.Key)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed populate left an entry: %v", err)
	}
	// The toolchain was installed whole before the entry failed; it stays.
	if err := checkGoToolchain(root); err != nil {
		t.Fatalf("toolchain after a failed proof: %v", err)
	}
}

func TestPopulateGoCacheReplacesAnIncompleteEntry(t *testing.T) {
	t.Parallel()
	root, source := goCacheRoot(t), goWorkspace(t)
	key := mustGoKey(t, source)
	stale := filepath.Join(goCacheEntry(root, key.Key), goCacheModcache, "stale")
	if err := os.MkdirAll(stale, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := populateGoCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: source}, (&fakeGoTools{}).tools()); err != nil {
		t.Fatalf("populate: %v", err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the incomplete entry's content survived: %v", err)
	}
}

func TestPopulateGoCacheRefusesASourceWithoutGoMod(t *testing.T) {
	t.Parallel()
	root := goCacheRoot(t)
	fake := &fakeGoTools{}
	_, err := populateGoCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: swiftWorkspace(t)}, fake.tools())
	if err == nil || !strings.Contains(err.Error(), "no go.mod") {
		t.Fatalf("populate error = %v, want a refusal of a source without Go", err)
	}
}

func TestPopulateGoCacheRefusesAnUnsafeCacheDirectory(t *testing.T) {
	t.Parallel()
	root := goCacheRoot(t)
	if err := os.Chmod(goCacheDir(root), 0o777); err != nil { //nolint:gosec // G302: the unsafe mode under test
		t.Fatal(err)
	}
	_, err := populateGoCache(t.Context(), PopulateOptions{ExecutorRoot: root, Source: goWorkspace(t)}, (&fakeGoTools{}).tools())
	if err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("populate error = %v, want a refusal of the open directory", err)
	}
}

// TestDaemonGoModIsNoNewerThanThePin keeps the daemon's own module buildable
// by the pinned toolchain: a go line bumped past it would fail every slot
// job, so the pin moves in the same change.
func TestDaemonGoModIsNoNewerThanThePin(t *testing.T) {
	t.Parallel()
	if err := refuseNewerGo(filepath.Join("..", ".."), []string{""}); err != nil {
		t.Fatalf("raise goToolchainVersion with the daemon's go line: %v", err)
	}
}
