package macverify

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The Go toolchain and offline Go module cache (#3155, plan §5.7). A job
// whose workspace holds a go.mod runs Go from a toolchain the daemon pins by
// version and archive digest, with GOTOOLCHAIN=local, and takes its modules
// from the cache entry keyed by every go.mod and go.sum in the workspace,
// with GOPROXY=off. A job whose key has no complete entry fails before its
// command runs. Both live under <root>/go-cache/, which the operator owns
// and populate fills, as it does swift-cache/:
//
//	go<version>/              the pinned toolchain's GOROOT, renamed into
//	                          place once extracted, so it is whole or absent
//	<key>/record.json         written last; marks the entry complete
//	<key>/modcache/           the entry's GOMODCACHE, read in place
//
// The slot reads both in place: neither the files' modes nor the sandbox
// profile let a job write them, and Go reads a complete module cache without
// writing to it (the #3155 spike built the convergence harness that way).

const (
	goCacheDirName      = "go-cache"
	goCacheModcache     = "modcache"
	goCacheKeyVersion   = "freeside.macverify.go-cache-key/v1"
	goCacheRecordFormat = "freeside.macverify.go-cache-record/v1"
)

// goToolchainVersion is the Go every slot runs. It matches the version the
// project image pins (projectimage's goToolchainVersion), and GO_VERSION in
// slot-run.sh must name it too (TestHelperScriptConstants).
const goToolchainVersion = "1.27.4"

// goToolchainArchives pins each darwin archive of goToolchainVersion by its
// SHA-256 from go.dev/dl. Populate refuses a download whose digest differs.
var goToolchainArchives = map[string]string{
	"arm64": "91a44a2b8a716600ad94a4f143e156116620db8828086a6e3602291be9bd0bb4",
	"amd64": "2b799377704ef12db39bd254995d5c348c9cc8d86562ca817724390e56c38f43",
}

// goToolchainPin names the pinned toolchain for this host's architecture:
// its archive's file name and SHA-256.
func goToolchainPin() (archive, sha256 string, err error) {
	sum, ok := goToolchainArchives[runtime.GOARCH]
	if !ok {
		return "", "", fmt.Errorf("no Go %s toolchain is pinned for darwin-%s", goToolchainVersion, runtime.GOARCH)
	}
	return "go" + goToolchainVersion + ".darwin-" + runtime.GOARCH + ".tar.gz", sum, nil
}

// ErrGoCacheMissing refuses a job whose workspace's Go module key has no
// complete cache entry, or whose host lacks the pinned toolchain. The
// command never runs.
var ErrGoCacheMissing = errors.New("macverify: no complete Go module cache entry")

// skipGoSourceDir names the directories the Go key leaves out: skipSourceDir's,
// and testdata, which the go command never builds a module from.
func skipGoSourceDir(name string) bool { return skipSourceDir(name) || name == "testdata" }

func isGoModuleFile(name string) bool { return name == "go.mod" || name == "go.sum" }

// goCacheKey is a workspace's Go module key. A zero Key means the workspace
// holds no go.mod or go.sum and needs no Go.
type goCacheKey struct {
	Key   string
	Files []keyedFile
}

func (k goCacheKey) paths() []string {
	paths := make([]string, len(k.Files))
	for i, f := range k.Files {
		paths[i] = f.Path
	}
	return paths
}

// modules lists the module directories, slash-separated and relative to the
// workspace, that hold a keyed go.mod.
func (k goCacheKey) modules() []string {
	var dirs []string
	for _, f := range k.Files {
		if dir, ok := strings.CutSuffix(f.Path, "go.mod"); ok && (dir == "" || strings.HasSuffix(dir, "/")) {
			dirs = append(dirs, strings.TrimSuffix(dir, "/"))
		}
	}
	return dirs
}

// workspaceGoCacheKey computes the key over every go.mod and go.sum under
// workdir, outside skipGoSourceDir's directories (workspaceFileKey).
func workspaceGoCacheKey(workdir string) (goCacheKey, error) {
	key, files, err := workspaceFileKey(workdir, goCacheKeyVersion, skipGoSourceDir, isGoModuleFile)
	if err != nil {
		return goCacheKey{}, fmt.Errorf("compute Go module key: %w", err)
	}
	return goCacheKey{Key: key, Files: files}, nil
}

// goCacheRecord is an entry's record.json, written last by populate. Go is
// the toolchain version that proved the entry; lookup refuses an entry
// another version proved.
type goCacheRecord struct {
	Format string      `json:"format"`
	Key    string      `json:"key"`
	Files  []keyedFile `json:"files"`
	Go     string      `json:"go"`
}

func goCacheDir(root string) string { return filepath.Join(root, goCacheDirName) }

func goCacheEntry(root, key string) string { return filepath.Join(goCacheDir(root), key) }

// goToolchainRoot is the pinned toolchain's GOROOT under root.
func goToolchainRoot(root string) string {
	return filepath.Join(goCacheDir(root), "go"+goToolchainVersion)
}

// lookupGoCache reports whether key has a complete entry under root and the
// pinned toolchain is in place. Anything else is ErrGoCacheMissing, naming
// the key and its files.
func lookupGoCache(root string, key goCacheKey) error {
	err := checkGoToolchain(root)
	if err == nil {
		err = checkGoCacheEntry(root, key.Key)
	}
	if err != nil {
		return fmt.Errorf("%w: key %s for %s: %w; run freesided go-cache populate on a checkout of this commit",
			ErrGoCacheMissing, key.Key, strings.Join(key.paths(), ", "), err)
	}
	return nil
}

// checkGoToolchain requires the pinned toolchain's go binary as a regular
// file. Populate renames the toolchain into place only once it is extracted
// whole, so its presence is its completeness.
func checkGoToolchain(root string) error {
	goBin := filepath.Join(goToolchainRoot(root), "bin", "go")
	info, err := os.Lstat(goBin)
	if err != nil {
		return fmt.Errorf("the Go %s toolchain is missing: %w", goToolchainVersion, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", goBin)
	}
	return nil
}

func checkGoCacheEntry(root, key string) error {
	if !swiftCacheKeyPattern.MatchString(key) {
		return fmt.Errorf("malformed key %q", key)
	}
	entry := goCacheEntry(root, key)
	for _, dir := range []string{entry, filepath.Join(entry, goCacheModcache)} {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
	}
	recordPath := filepath.Join(entry, cacheRecordName)
	info, err := os.Lstat(recordPath)
	if err != nil {
		return fmt.Errorf("the entry is incomplete: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", recordPath)
	}
	body, err := os.ReadFile(recordPath) //nolint:gosec // G304: the executor's own cache record
	if err != nil {
		return err
	}
	var record goCacheRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return fmt.Errorf("decode %s: %w", recordPath, err)
	}
	if record.Format != goCacheRecordFormat || record.Key != key || record.Go != goToolchainVersion {
		return fmt.Errorf("%s records format %q, key %q, and Go %q", recordPath, record.Format, record.Key, record.Go)
	}
	return nil
}
