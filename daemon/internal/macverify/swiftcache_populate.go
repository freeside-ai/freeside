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
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

// PopulateOptions names the cache a populate run fills and the project
// checkout it fills it for.
type PopulateOptions struct {
	// ExecutorRoot is the executor root whose swift-cache or go-cache
	// directory the entry goes in. The operator creates that directory,
	// owned by the daemon user and not group- or world-writable.
	ExecutorRoot string
	// Source is a checkout of the project at the commit to serve. Populate
	// works on scratch copies and never writes to it.
	Source string
	// Progress, when set, receives one line per step.
	Progress io.Writer
}

// PopulateResult reports a populate run: the key, the Package.resolved
// files it covers, and whether this run filled the entry (false when it was
// already complete).
type PopulateResult struct {
	Key       string
	Files     []string
	Populated bool
}

// PopulateSwiftCache fills the cache entry for opts.Source's Swift
// dependencies, with network, outside any job. In order, it:
//
//  1. Copies the source twice into scratch, leaving out .git and .build,
//     and keys the copy. A key whose entry is complete is left as it is.
//  2. Classifies each Package.resolved as a SwiftPM package's or one Xcode
//     project's or workspace's, refusing anything else and a second Xcode
//     container by name.
//  3. Removes an entry lookup does not accept, then resolves the first copy
//     online with SwiftPM's manifest sandbox left on and every shared cache
//     bypassed.
//  4. Lists every repository that resolve cloned, from the clones' own
//     origin URLs, and every submodule of each checkout, recursively, as
//     #2183's discovery does, refusing a URL that is not https or that
//     holds a space or control character.
//  5. Clones each as a bare mirror and writes the seed: a gitconfig that
//     rewrites both spellings of each URL to its mirror, and a mirrors.json
//     for each SwiftPM package mapping both spellings.
//  6. Proves the entry offline in the second copy, overlaid with the seed as
//     a job's workspace is: with the seed gitconfig, every proxy pointed at
//     a closed port, and no shared cache, each resolve must succeed and
//     leave every Package.resolved byte-identical. The Xcode resolve writes
//     the seed's SourcePackages.
//  7. Writes record.json last. On any failure it removes the entry, so no
//     partial entry ever looks complete.
//
// One populate runs at a time per cache, under a lock file in it.
func PopulateSwiftCache(ctx context.Context, opts PopulateOptions) (PopulateResult, error) {
	return populateSwiftCache(ctx, opts, hostPopulateTools())
}

// toolCommand is one host tool invocation populate makes.
type toolCommand struct {
	Path string
	Args []string
	Env  []string
}

// populateTools is populate's seam over the host tools, so its sequencing
// and checks run with fakes.
type populateTools struct {
	// run runs a command and returns its standard output.
	run       func(ctx context.Context, c toolCommand) ([]byte, error)
	toolchain func(ctx context.Context) (Toolchain, error)
}

const (
	xcrunPath = "/usr/bin/xcrun"
	gitPath   = "/usr/bin/git"
	// closedProxy is a port nothing listens on: a fetch the mirrors do not
	// serve fails instead of reaching the network.
	closedProxy = "http://127.0.0.1:9"
	// maxToolStdout bounds what populate keeps of a tool's standard output;
	// it parses only git's, which is a URL list.
	maxToolStdout = 1 << 20
)

func hostPopulateTools() populateTools {
	return populateTools{run: runHostTool, toolchain: HostToolchain}
}

func runHostTool(ctx context.Context, c toolCommand) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...) //nolint:gosec // G204: fixed tool paths; arguments are populate's own
	cmd.Env = c.Env
	stdout := procbound.NewTailBuffer(maxToolStdout)
	report := procbound.NewTailBuffer(maxHelperReport)
	cmd.Stdout = io.MultiWriter(stdout, report)
	cmd.Stderr = report
	if err := procbound.Run(cmd, hostToolWaitDelay); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", filepath.Base(c.Path), strings.Join(c.Args, " "), err,
			strings.TrimSpace(string(report.Bytes())))
	}
	return stdout.Bytes(), nil
}

// swiftLayout is where a workspace's Package.resolved files belong.
type swiftLayout struct {
	// packages are the SwiftPM package directories, slash-separated and
	// relative to the workspace.
	packages []string
	// container is the Xcode project or workspace, relative to the
	// workspace, or "" for none; containerFlag is its xcodebuild flag.
	container     string
	containerFlag string
}

type swiftMirror struct {
	url, key, path string
}

func populateSwiftCache(ctx context.Context, opts PopulateOptions, tools populateTools) (PopulateResult, error) {
	progress := opts.Progress
	if progress == nil {
		progress = io.Discard
	}
	for name, dir := range map[string]string{"executor root": opts.ExecutorRoot, "source": opts.Source} {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return PopulateResult{}, fmt.Errorf("populate Swift cache: %s %q is not a clean absolute path", name, dir)
		}
	}
	if info, err := os.Stat(opts.Source); err != nil || !info.IsDir() {
		return PopulateResult{}, fmt.Errorf("populate Swift cache: source %s is not a directory", opts.Source)
	}
	cacheDir := filepath.Join(opts.ExecutorRoot, swiftCacheDirName)
	if err := checkOwnedCacheDir(cacheDir); err != nil {
		return PopulateResult{}, fmt.Errorf("populate Swift cache: %w", err)
	}
	unlock, err := lockPopulate(filepath.Join(cacheDir, ".populate.lock"))
	if err != nil {
		return PopulateResult{}, fmt.Errorf("populate Swift cache: %w", err)
	}
	defer unlock()

	scratch, err := os.MkdirTemp("", "freeside-swift-cache-")
	if err != nil {
		return PopulateResult{}, fmt.Errorf("populate Swift cache: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	online, proof := filepath.Join(scratch, "online"), filepath.Join(scratch, "proof")
	key, err := copySource(opts.Source, online, proof)
	if err != nil {
		return PopulateResult{}, fmt.Errorf("populate Swift cache: %w", err)
	}
	result := PopulateResult{Key: key.Key, Files: key.paths()}
	if checkSwiftCacheEntry(opts.ExecutorRoot, key.Key) == nil {
		return result, nil
	}
	layout, err := classifyResolved(online, key)
	if err != nil {
		return result, fmt.Errorf("populate Swift cache: %w", err)
	}
	toolchain, err := tools.toolchain(ctx)
	if err != nil {
		return result, fmt.Errorf("populate Swift cache: %w", err)
	}

	entry := swiftCacheEntry(opts.ExecutorRoot, key.Key)
	if err := removeUnacceptedEntry(entry); err != nil {
		return result, fmt.Errorf("populate Swift cache: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(entry)
		}
	}()
	for _, dir := range []string{"mirrors", "seed/home", "seed/job/workspace", "seed/job/source-packages"} {
		if err := os.MkdirAll(filepath.Join(entry, filepath.FromSlash(dir)), 0o755); err != nil { //nolint:gosec // G301: slots read the entry
			return result, fmt.Errorf("populate Swift cache: %w", err)
		}
	}

	_, _ = fmt.Fprintf(progress, "resolving %s online\n", strings.Join(key.paths(), ", "))
	onlineEnv := append(toolEnv(), "GIT_CONFIG_GLOBAL="+os.DevNull)
	if err := resolveLayout(ctx, tools, online, layout, filepath.Join(scratch, "online-source-packages"),
		filepath.Join(scratch, "online-caches"), onlineEnv); err != nil {
		return result, fmt.Errorf("populate Swift cache: online resolve: %w", err)
	}
	stores := dependencyStores(online, layout, filepath.Join(scratch, "online-source-packages"))
	if err := refuseDownloads(stores); err != nil {
		return result, fmt.Errorf("populate Swift cache: %w", err)
	}
	urls, err := discoverDependencyURLs(ctx, tools, stores, onlineEnv)
	if err != nil {
		return result, fmt.Errorf("populate Swift cache: %w", err)
	}
	mirrors, err := planMirrors(entry, urls)
	if err != nil {
		return result, fmt.Errorf("populate Swift cache: %w", err)
	}
	for _, m := range mirrors {
		_, _ = fmt.Fprintf(progress, "mirroring %s\n", m.url)
		if err := os.Mkdir(filepath.Dir(m.path), 0o755); err != nil { //nolint:gosec // G301: slots read the mirrors
			return result, fmt.Errorf("populate Swift cache: %w", err)
		}
		if _, err := tools.run(ctx, toolCommand{
			Path: gitPath, Env: onlineEnv,
			Args: []string{"clone", "--quiet", "--mirror", "--", m.url, m.path},
		}); err != nil {
			return result, fmt.Errorf("populate Swift cache: mirror %s: %w", m.url, err)
		}
	}
	gitconfig := filepath.Join(entry, "seed", "home", ".gitconfig")
	if err := writeSeed(ctx, tools, entry, gitconfig, layout, mirrors, onlineEnv); err != nil {
		return result, fmt.Errorf("populate Swift cache: %w", err)
	}

	_, _ = fmt.Fprintln(progress, "proving the entry offline")
	if err := copyTree(filepath.Join(entry, "seed", "job", "workspace"), proof, nil); err != nil {
		return result, fmt.Errorf("populate Swift cache: overlay the seed: %w", err)
	}
	proofEnv := append(toolEnv(), "GIT_CONFIG_GLOBAL="+gitconfig)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		proofEnv = append(proofEnv, name+"="+closedProxy)
	}
	if err := resolveLayout(ctx, tools, proof, layout, filepath.Join(entry, "seed", "job", "source-packages"),
		filepath.Join(scratch, "proof-caches"), proofEnv); err != nil {
		return result, fmt.Errorf("populate Swift cache: offline proof: %w", err)
	}
	if err := checkResolvedUnchanged(proof, key); err != nil {
		return result, fmt.Errorf("populate Swift cache: offline proof: %w", err)
	}

	if err := makeEntryReadable(entry); err != nil {
		return result, fmt.Errorf("populate Swift cache: %w", err)
	}
	record := swiftCacheRecord{
		Format: swiftCacheRecordFormat, Key: key.Key, Files: key.Files,
		URLs: urls, Packages: layout.packages, Container: layout.container,
	}
	record.Toolchain.XcodeBuild, record.Toolchain.Swift = toolchain.XcodeBuild, toolchain.Swift
	if err := writeRecord(entry, record); err != nil {
		return result, fmt.Errorf("populate Swift cache: %w", err)
	}
	complete = true
	result.Populated = true
	return result, nil
}

// checkOwnedCacheDir refuses a cache directory that is not a real directory
// owned by this user and closed to group and other writes, as slot-run
// refuses to seed from one.
func checkOwnedCacheDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%w; create it with sudo freesided macverify install", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not a directory this user owns; sudo freesided macverify install hands it to the operator", dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (%v)", dir, info.Mode().Perm())
	}
	return nil
}

func lockPopulate(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the cache's own lock file
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// copySource makes the online and proof copies of src and keys them,
// refusing a source with no Package.resolved or one that changed between
// the copies.
func copySource(src, online, proof string) (swiftCacheKey, error) {
	var keys [2]swiftCacheKey
	for i, dst := range []string{online, proof} {
		if err := copyTree(src, dst, skipSourceDir); err != nil {
			return swiftCacheKey{}, fmt.Errorf("copy %s: %w", src, err)
		}
		key, err := workspaceSwiftCacheKey(dst)
		if err != nil {
			return swiftCacheKey{}, err
		}
		keys[i] = key
	}
	if keys[0].Key == "" {
		return swiftCacheKey{}, fmt.Errorf("%s holds no Package.resolved, so its jobs need no cache", src)
	}
	if keys[0].Key != keys[1].Key {
		return swiftCacheKey{}, fmt.Errorf("%s changed while it was copied", src)
	}
	return keys[0], nil
}

// copyTree copies src's directories, regular files, and symlinks into dst,
// skipping directories skip names. It replaces a file or symlink already at
// a target and refuses to write through a symlink, as tar's extraction
// does, so overlaying the seed matches what slot-run's copy would do.
func copyTree(src, dst string, skip func(name string) bool) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if rel != "." && skip != nil && skip(d.Name()) {
				return fs.SkipDir
			}
			info, err := os.Lstat(target)
			switch {
			case errors.Is(err, fs.ErrNotExist) && rel == ".":
				return os.MkdirAll(target, 0o755) //nolint:gosec // G301: slots read the seed
			case errors.Is(err, fs.ErrNotExist):
				return os.Mkdir(target, 0o755) //nolint:gosec // G301: slots read the seed
			case err != nil:
				return err
			case !info.IsDir():
				return fmt.Errorf("%s is not a directory", target)
			}
			return nil
		}
		if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			// Sockets, devices, and pipes: nothing a resolve reads.
			return nil
		}
		if info, err := os.Lstat(target); err == nil {
			if info.IsDir() {
				return fmt.Errorf("%s is a directory", target)
			}
			if err := os.Remove(target); err != nil {
				return err
			}
		}
		if d.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target) //nolint:gosec // G122: populate's own scratch trees; only this user writes them
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // G304: a file inside the tree being copied
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: the copy's own target
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

const (
	xcodeProjectSuffix   = "/project.xcworkspace/xcshareddata/swiftpm"
	xcodeWorkspaceSuffix = "/xcshareddata/swiftpm"
)

// classifyResolved places each Package.resolved: beside a Package.swift it
// is a SwiftPM package's; at <X>.xcodeproj/project.xcworkspace/
// xcshareddata/swiftpm or <X>.xcworkspace/xcshareddata/swiftpm it is that
// Xcode container's. Anything else, or a second container, is refused by
// name.
func classifyResolved(workdir string, key swiftCacheKey) (swiftLayout, error) {
	var layout swiftLayout
	var containers []string
	for _, f := range key.Files {
		dir := path.Dir(f.Path)
		manifest := filepath.Join(workdir, filepath.FromSlash(dir), "Package.swift")
		if info, err := os.Lstat(manifest); err == nil && info.Mode().IsRegular() {
			layout.packages = append(layout.packages, dir)
			continue
		}
		slashed := "/" + dir
		switch {
		case strings.HasSuffix(slashed, xcodeProjectSuffix) &&
			strings.HasSuffix(strings.TrimSuffix(slashed, xcodeProjectSuffix), ".xcodeproj"):
			layout.container = strings.TrimPrefix(strings.TrimSuffix(slashed, xcodeProjectSuffix), "/")
			layout.containerFlag = "-project"
		case strings.HasSuffix(slashed, xcodeWorkspaceSuffix) &&
			strings.HasSuffix(strings.TrimSuffix(slashed, xcodeWorkspaceSuffix), ".xcworkspace"):
			layout.container = strings.TrimPrefix(strings.TrimSuffix(slashed, xcodeWorkspaceSuffix), "/")
			layout.containerFlag = "-workspace"
		default:
			return swiftLayout{}, fmt.Errorf(
				"%s is neither beside a Package.swift nor in an Xcode project or workspace", f.Path)
		}
		containers = append(containers, layout.container)
	}
	for _, f := range key.Files {
		if err := refuseNonGitPins(workdir, f.Path); err != nil {
			return swiftLayout{}, err
		}
	}
	if len(containers) > 1 {
		return swiftLayout{}, fmt.Errorf(
			"the source holds Package.resolved for more than one Xcode project or workspace: %s",
			strings.Join(containers, ", "))
	}
	return layout, nil
}

// refuseNonGitPins refuses a Package.resolved that pins a package by any
// kind but remoteSourceControl: a registry package is fetched over HTTP, not
// git, so no mirror can serve it (refuseDownloads), and a local package lives
// outside the cache. A version 1 file has no kinds and pins git only.
func refuseNonGitPins(workdir, rel string) error {
	body, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(rel))) //nolint:gosec // G304: a keyed file in populate's own scratch copy
	if err != nil {
		return err
	}
	var resolved struct {
		Pins []struct {
			Identity string `json:"identity"`
			Kind     string `json:"kind"`
		} `json:"pins"`
	}
	if err := json.Unmarshal(body, &resolved); err != nil {
		return fmt.Errorf("decode %s: %w", rel, err)
	}
	for _, pin := range resolved.Pins {
		if pin.Kind != "remoteSourceControl" {
			return fmt.Errorf("%s pins %s as %q: the cache serves git dependencies only", rel, pin.Identity, pin.Kind)
		}
	}
	return nil
}

// removeUnacceptedEntry removes whatever is at entry, which the caller has
// found lookup does not accept: a directory a failed or interrupted populate
// left without a record. entry is <root>/swift-cache/<64 hex>, under a cache
// directory this user owns, and RemoveAll never follows a symlink inside it.
func removeUnacceptedEntry(entry string) error {
	if _, err := os.Lstat(entry); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return os.RemoveAll(entry)
}

// resolveLayout resolves every SwiftPM package and the Xcode container in
// workdir, with SwiftPM's manifest sandbox on and every shared cache and
// configuration redirected under caches.
func resolveLayout(ctx context.Context, tools populateTools, workdir string, layout swiftLayout,
	sourcePackages, caches string, env []string,
) error {
	for i, pkg := range layout.packages {
		dir := filepath.Join(caches, fmt.Sprintf("swiftpm-%d", i))
		if _, err := tools.run(ctx, toolCommand{Path: xcrunPath, Env: env, Args: []string{
			"swift", "package", "resolve",
			"--package-path", filepath.Join(workdir, filepath.FromSlash(pkg)),
			"--only-use-versions-from-resolved-file",
			"--disable-dependency-cache",
			"--cache-path", filepath.Join(dir, "cache"),
			"--config-path", filepath.Join(dir, "configuration"),
		}}); err != nil {
			return err
		}
	}
	if layout.container == "" {
		return nil
	}
	_, err := tools.run(ctx, toolCommand{Path: xcrunPath, Env: env, Args: []string{
		"xcodebuild", "-resolvePackageDependencies",
		layout.containerFlag, filepath.Join(workdir, filepath.FromSlash(layout.container)),
		"-clonedSourcePackagesDirPath", sourcePackages,
		"-packageCachePath", filepath.Join(caches, "xcode"),
		"-disablePackageRepositoryCache",
		"-onlyUsePackageVersionsFromResolvedFile",
		"-scmProvider", "system",
	}})
	return err
}

// dependencyStores lists where a resolve of workdir leaves what it fetched:
// each SwiftPM package's .build and xcodebuild's SourcePackages.
func dependencyStores(workdir string, layout swiftLayout, sourcePackages string) []string {
	var stores []string
	for _, pkg := range layout.packages {
		stores = append(stores, filepath.Join(workdir, filepath.FromSlash(pkg), ".build"))
	}
	if layout.container != "" {
		stores = append(stores, sourcePackages)
	}
	return stores
}

// refuseDownloads refuses a resolve that downloaded binary artifacts or
// registry archives. SwiftPM fetches those over HTTP with URLSession, which
// ignores the proxy variables that close the offline proof, so the proof
// could pass while a job, with networking denied, cannot fetch them. The
// cache serves git dependencies only.
func refuseDownloads(stores []string) error {
	for _, store := range stores {
		for _, dir := range []string{"artifacts", filepath.Join("registry", "downloads")} {
			entries, err := os.ReadDir(filepath.Join(store, dir))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if len(entries) > 0 {
				return fmt.Errorf("the online resolve downloaded %s into %s, which the cache cannot serve: "+
					"it serves git dependencies only", entries[0].Name(), filepath.Join(store, dir))
			}
		}
	}
	return nil
}

// discoverDependencyURLs lists the origin URL of every repository the
// online resolve cloned into stores and of every submodule of each checkout,
// recursively, so clones git made on SwiftPM's or Xcode's behalf are
// covered without intercepting git. Each URL must be https, of printable
// ASCII with no space.
func discoverDependencyURLs(ctx context.Context, tools populateTools, stores []string, env []string) ([]string, error) {
	seen := map[string]bool{}
	add := func(out []byte) error {
		for line := range strings.Lines(string(out)) {
			url := strings.TrimSuffix(line, "\n")
			if url == "" {
				continue
			}
			if err := validateDependencyURL(url); err != nil {
				return err
			}
			seen[url] = true
		}
		return nil
	}
	for _, store := range stores {
		repositories, err := subdirectories(filepath.Join(store, "repositories"))
		if err != nil {
			return nil, err
		}
		for _, repo := range repositories {
			out, err := tools.run(ctx, toolCommand{
				Path: gitPath, Env: env,
				Args: []string{"-C", repo, "config", "--get", "remote.origin.url"},
			})
			if err != nil {
				return nil, fmt.Errorf("read the origin of %s: %w", repo, err)
			}
			if err := add(out); err != nil {
				return nil, err
			}
		}
		checkouts, err := subdirectories(filepath.Join(store, "checkouts"))
		if err != nil {
			return nil, err
		}
		for _, checkout := range checkouts {
			out, err := tools.run(ctx, toolCommand{Path: gitPath, Env: env, Args: []string{
				"-C", checkout, "submodule", "foreach", "--quiet", "--recursive",
				"git config --get remote.origin.url",
			}})
			if err != nil {
				return nil, fmt.Errorf("list the submodules of %s: %w", checkout, err)
			}
			if err := add(out); err != nil {
				return nil, err
			}
		}
	}
	urls := make([]string, 0, len(seen))
	for url := range seen {
		urls = append(urls, url)
	}
	slices.Sort(urls)
	return urls, nil
}

// subdirectories lists dir's real subdirectories; a missing dir has none.
func subdirectories(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(dir, e.Name()))
		}
	}
	return dirs, nil
}

// mirrorNamePattern is what a mirror's last path component may be: it
// becomes a directory name and SwiftPM's identity for the package.
var mirrorNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

func validateDependencyURL(url string) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("dependency %q is not an https URL", url)
	}
	for i := range len(url) {
		if url[i] < '!' || url[i] > '~' {
			return fmt.Errorf("dependency URL %q holds a space or control character", url)
		}
	}
	key := strings.TrimSuffix(url, ".git")
	if !mirrorNamePattern.MatchString(path.Base(key)) || strings.HasSuffix(key, "/") {
		return fmt.Errorf("dependency URL %q does not end in a repository name", url)
	}
	return nil
}

// planMirrors names each URL's mirror: mirrors/<SHA-256 of the URL without
// .git>/<its last component>.git, one mirror per URL spelling pair.
func planMirrors(entry string, urls []string) ([]swiftMirror, error) {
	byKey := map[string]swiftMirror{}
	for _, url := range urls {
		key := strings.TrimSuffix(url, ".git")
		if _, ok := byKey[key]; ok {
			continue
		}
		sum := sha256.Sum256([]byte(key))
		byKey[key] = swiftMirror{
			url: url, key: key,
			path: filepath.Join(entry, "mirrors", hex.EncodeToString(sum[:]), path.Base(key)+".git"),
		}
	}
	mirrors := make([]swiftMirror, 0, len(byKey))
	for _, m := range byKey {
		mirrors = append(mirrors, m)
	}
	slices.SortFunc(mirrors, func(a, b swiftMirror) int { return strings.Compare(a.key, b.key) })
	return mirrors, nil
}

type swiftpmMirrors struct {
	Object  []swiftpmMirror `json:"object"`
	Version int             `json:"version"`
}

type swiftpmMirror struct {
	Mirror   string `json:"mirror"`
	Original string `json:"original"`
}

// writeSeed writes the gitconfig the job's home gets, which rewrites both
// spellings of each URL to its mirror for every git a job runs (submodules
// never read mirrors.json), allows the file transport, and marks each
// mirror safe, since the slot reads repositories this user owns; and one
// mirrors.json per SwiftPM package. git writes the gitconfig, so every value
// is escaped.
func writeSeed(ctx context.Context, tools populateTools, entry, gitconfig string, layout swiftLayout,
	mirrors []swiftMirror, env []string,
) error {
	settings := [][2]string{{"protocol.file.allow", "always"}}
	doc := swiftpmMirrors{Version: 1, Object: []swiftpmMirror{}}
	for _, m := range mirrors {
		mirrorURL := "file://" + m.path
		settings = append(settings,
			[2]string{"url." + mirrorURL + ".insteadOf", m.key},
			[2]string{"url." + mirrorURL + ".insteadOf", m.key + ".git"},
			[2]string{"safe.directory", m.path})
		doc.Object = append(doc.Object,
			swiftpmMirror{Mirror: mirrorURL, Original: m.key},
			swiftpmMirror{Mirror: mirrorURL, Original: m.key + ".git"})
	}
	for _, s := range settings {
		if _, err := tools.run(ctx, toolCommand{
			Path: gitPath, Env: env,
			Args: []string{"config", "--file", gitconfig, "--add", s[0], s[1]},
		}); err != nil {
			return fmt.Errorf("write the seed gitconfig: %w", err)
		}
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	for _, pkg := range layout.packages {
		dir := filepath.Join(entry, "seed", "job", "workspace", filepath.FromSlash(pkg), ".swiftpm", "configuration")
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: slots read the seed
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "mirrors.json"), append(body, '\n'), 0o644); err != nil { //nolint:gosec // G306: slots read the seed
			return err
		}
	}
	return nil
}

// checkResolvedUnchanged requires every keyed Package.resolved in workdir to
// be byte-identical to the one the key names.
func checkResolvedUnchanged(workdir string, key swiftCacheKey) error {
	for _, f := range key.Files {
		body, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(f.Path))) //nolint:gosec // G304: a keyed file in populate's own scratch copy
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return fmt.Errorf("the mirrors resolve %s differently", f.Path)
		}
	}
	return nil
}

// makeEntryReadable gives every directory 0755 and every regular file 0644,
// or 0755 when it was executable, so slots can read the mirrors in place and
// no one but this user can write the entry.
func makeEntryReadable(entry string) error {
	return filepath.WalkDir(entry, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.Chmod(p, 0o755) //nolint:gosec // G302: slots read the entry
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			mode := fs.FileMode(0o644)
			if info.Mode().Perm()&0o111 != 0 {
				mode = 0o755
			}
			return os.Chmod(p, mode) //nolint:gosec // G122: a regular file in this user's own entry, after every tool exited
		}
		return nil
	})
}
