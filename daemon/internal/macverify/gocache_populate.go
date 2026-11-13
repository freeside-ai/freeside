package macverify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/version"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// goToolchainBaseURL is where populate downloads the pinned archive from.
const goToolchainBaseURL = "https://go.dev/dl/"

// tarPath extracts the toolchain archive.
const tarPath = "/usr/bin/tar"

// goPopulateTools is Go populate's seam over the host tools and the
// toolchain download, so its sequencing and checks run with fakes.
type goPopulateTools struct {
	run func(ctx context.Context, c toolCommand) ([]byte, error)
	// fetch writes the body at url to w.
	fetch func(ctx context.Context, url string, w io.Writer) error
	// pin names the toolchain archive to fetch and its SHA-256
	// (goToolchainPin).
	pin func() (archive, sha256 string, err error)
}

func hostGoPopulateTools() goPopulateTools {
	return goPopulateTools{run: runHostTool, fetch: httpFetch, pin: goToolchainPin}
}

func httpFetch(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// PopulateGoCache fills the Go module cache entry for opts.Source, with
// network, outside any job, and installs the pinned Go toolchain when it is
// absent. In order, it:
//
//  1. Copies the source into scratch, leaving out .git and .build, and keys
//     the copy by every go.mod and go.sum outside testdata.
//  2. Refuses a go.mod whose go or toolchain line asks for a Go newer than
//     the pin, since a slot runs with GOTOOLCHAIN=local.
//  3. Installs the pinned toolchain if it is absent: downloads the archive,
//     refuses it unless its SHA-256 is the pin's, extracts it into scratch
//     space in the cache, and renames it into place.
//  4. Leaves a complete entry as it is. Otherwise removes what is there and
//     runs go mod download for each module into the entry's module cache,
//     through the Go module proxy and checksum database.
//  5. Proves the entry offline as a job would use it: GOPROXY=off, no
//     checksum database, every proxy pointed at a closed port, and
//     go list -deps -test ./... for each module.
//  6. Writes record.json last. On any failure it removes the entry, so no
//     partial entry ever looks complete.
//
// It shares Swift populate's lock discipline: one populate at a time per
// cache, under a lock file in it.
func PopulateGoCache(ctx context.Context, opts PopulateOptions) (PopulateResult, error) {
	return populateGoCache(ctx, opts, hostGoPopulateTools())
}

func populateGoCache(ctx context.Context, opts PopulateOptions, tools goPopulateTools) (PopulateResult, error) {
	progress := opts.Progress
	if progress == nil {
		progress = io.Discard
	}
	for name, dir := range map[string]string{"executor root": opts.ExecutorRoot, "source": opts.Source} {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return PopulateResult{}, fmt.Errorf("populate Go cache: %s %q is not a clean absolute path", name, dir)
		}
	}
	if info, err := os.Stat(opts.Source); err != nil || !info.IsDir() {
		return PopulateResult{}, fmt.Errorf("populate Go cache: source %s is not a directory", opts.Source)
	}
	cacheDir := goCacheDir(opts.ExecutorRoot)
	if err := checkOwnedCacheDir(cacheDir); err != nil {
		return PopulateResult{}, fmt.Errorf("populate Go cache: %w", err)
	}
	unlock, err := lockPopulate(filepath.Join(cacheDir, ".populate.lock"))
	if err != nil {
		return PopulateResult{}, fmt.Errorf("populate Go cache: %w", err)
	}
	defer unlock()

	scratch, err := os.MkdirTemp("", "freeside-go-cache-")
	if err != nil {
		return PopulateResult{}, fmt.Errorf("populate Go cache: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	src := filepath.Join(scratch, "source")
	if err := copyTree(opts.Source, src, skipSourceDir); err != nil {
		return PopulateResult{}, fmt.Errorf("populate Go cache: copy %s: %w", opts.Source, err)
	}
	key, err := workspaceGoCacheKey(src)
	if err != nil {
		return PopulateResult{}, fmt.Errorf("populate Go cache: %w", err)
	}
	result := PopulateResult{Key: key.Key, Files: key.paths()}
	modules := key.modules()
	if len(modules) == 0 {
		return result, fmt.Errorf("populate Go cache: %s holds no go.mod, so its jobs need no Go", opts.Source)
	}
	if err := refuseNewerGo(src, modules); err != nil {
		return result, fmt.Errorf("populate Go cache: %w", err)
	}
	if err := ensureGoToolchain(ctx, tools, opts.ExecutorRoot, scratch, progress); err != nil {
		return result, fmt.Errorf("populate Go cache: %w", err)
	}
	if checkGoCacheEntry(opts.ExecutorRoot, key.Key) == nil {
		return result, nil
	}

	entry := goCacheEntry(opts.ExecutorRoot, key.Key)
	if err := removeUnacceptedEntry(entry); err != nil {
		return result, fmt.Errorf("populate Go cache: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(entry)
		}
	}()
	modcache := filepath.Join(entry, goCacheModcache)
	if err := os.MkdirAll(modcache, 0o755); err != nil { //nolint:gosec // G301: slots read the entry
		return result, fmt.Errorf("populate Go cache: %w", err)
	}
	goBin := filepath.Join(goToolchainRoot(opts.ExecutorRoot), "bin", "go")
	base := populateGoEnv(opts.ExecutorRoot, modcache, filepath.Join(scratch, "gocache"))
	online := append(slices.Clone(base), "GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org")
	for _, mod := range modules {
		_, _ = fmt.Fprintf(progress, "downloading the modules of %s\n", displayModule(mod))
		if _, err := tools.run(ctx, toolCommand{
			Path: goBin, Env: online, Args: []string{"-C", filepath.Join(src, filepath.FromSlash(mod)), "mod", "download"},
		}); err != nil {
			return result, fmt.Errorf("populate Go cache: download the modules of %s: %w", displayModule(mod), err)
		}
	}

	_, _ = fmt.Fprintln(progress, "proving the entry offline")
	offline := append(slices.Clone(base), "GOPROXY=off", "GOSUMDB=off")
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		offline = append(offline, name+"="+closedProxy)
	}
	for _, mod := range modules {
		if _, err := tools.run(ctx, toolCommand{
			Path: goBin, Env: offline,
			Args: []string{"-C", filepath.Join(src, filepath.FromSlash(mod)), "list", "-deps", "-test", "./..."},
		}); err != nil {
			return result, fmt.Errorf("populate Go cache: offline proof of %s: %w", displayModule(mod), err)
		}
	}

	if err := makeEntryReadable(entry); err != nil {
		return result, fmt.Errorf("populate Go cache: %w", err)
	}
	if err := writeRecord(entry, goCacheRecord{
		Format: goCacheRecordFormat, Key: key.Key, Files: key.Files, Go: goToolchainVersion,
	}); err != nil {
		return result, fmt.Errorf("populate Go cache: %w", err)
	}
	complete = true
	result.Populated = true
	return result, nil
}

func displayModule(mod string) string {
	if mod == "" {
		return "the workspace root"
	}
	return mod
}

// populateGoEnv is the environment both of populate's Go phases share: the
// pinned toolchain only, no user go env file, writable module directories so
// a failed entry can be removed (makeEntryReadable then closes them), and a
// build cache in scratch.
func populateGoEnv(root, modcache, gocache string) []string {
	env := []string{"PATH=" + filepath.Join(goToolchainRoot(root), "bin") + ":" + commandPath}
	for _, kv := range toolEnv() {
		if !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GOENV=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly -modcacherw",
		"GOMODCACHE="+modcache, "GOCACHE="+gocache)
}

// refuseNewerGo refuses a module whose go.mod asks, by its go or toolchain
// line, for a Go newer than the pin: with GOTOOLCHAIN=local the pinned
// toolchain would refuse to build it in every job.
func refuseNewerGo(src string, modules []string) error {
	pin := "go" + goToolchainVersion
	for _, mod := range modules {
		rel := filepath.ToSlash(filepath.Join(mod, "go.mod"))
		body, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel))) //nolint:gosec // G304: a keyed go.mod in populate's own scratch copy
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(bytes.NewReader(body))
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 2 {
				continue
			}
			var want string
			switch fields[0] {
			case "go":
				want = "go" + fields[1]
			case "toolchain":
				want = fields[1]
			default:
				continue
			}
			if !version.IsValid(want) {
				return fmt.Errorf("%s names Go %q, which is not a Go version", rel, fields[1])
			}
			if version.Compare(want, pin) > 0 {
				return fmt.Errorf("%s asks for %s, newer than the pinned %s", rel, want, pin)
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
	}
	return nil
}

// ensureGoToolchain installs the pinned toolchain under root's Go cache
// when it is absent. It extracts into a scratch directory inside the cache
// and renames the toolchain into place, so the toolchain is whole or absent;
// a directory at the toolchain's name that lacks its go binary is refused
// for the operator to remove.
func ensureGoToolchain(ctx context.Context, tools goPopulateTools, root, scratch string, progress io.Writer) error {
	if checkGoToolchain(root) == nil {
		return nil
	}
	goroot := goToolchainRoot(root)
	if _, err := os.Lstat(goroot); err == nil {
		return fmt.Errorf("%s exists without a complete toolchain; remove it and populate again", goroot)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	archive, want, err := tools.pin()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(progress, "downloading %s\n", archive)
	archivePath := filepath.Join(scratch, archive)
	f, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: populate's own scratch file
	if err != nil {
		return err
	}
	h := sha256.New()
	fetchErr := tools.fetch(ctx, goToolchainBaseURL+archive, io.MultiWriter(f, h))
	if err := errors.Join(fetchErr, f.Close()); err != nil {
		return fmt.Errorf("download %s: %w", archive, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s has SHA-256 %s, want the pinned %s", archive, got, want)
	}
	staging, err := os.MkdirTemp(goCacheDir(root), ".go-toolchain-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if _, err := tools.run(ctx, toolCommand{
		Path: tarPath, Env: toolEnv(), Args: []string{"-x", "-z", "-f", archivePath, "-C", staging},
	}); err != nil {
		return fmt.Errorf("extract %s: %w", archive, err)
	}
	extracted := filepath.Join(staging, "go")
	if err := makeEntryReadable(extracted); err != nil {
		return err
	}
	if err := os.Rename(extracted, goroot); err != nil {
		return err
	}
	return checkGoToolchain(root)
}
