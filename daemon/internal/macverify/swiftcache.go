package macverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The offline Swift dependency cache (#2880, plan §5.7). Verification never
// fetches: a job whose workspace holds a Package.resolved takes its
// dependencies from the cache entry keyed by every Package.resolved in the
// workspace, and a job whose key has no complete entry fails before its
// command runs. An entry lives at <root>/swift-cache/<key>/:
//
//	record.json                       written last; marks the entry complete
//	mirrors/<h>/<name>.git            bare mirrors, read in place by the slot
//	seed/home/.gitconfig              copied into the slot's home
//	seed/job/workspace/<pkg>/.swiftpm/configuration/mirrors.json
//	seed/job/source-packages/         xcodebuild's pre-seeded SourcePackages
//
// <h> is the SHA-256 of the dependency URL without a trailing .git, the
// mirror name #2183's Linux bake uses. <name> is the URL's last path
// component: SwiftPM derives a mirrored package's identity from the mirror
// URL's last component, so a mirror named by digest alone would no longer
// match the identity Package.resolved records.

const (
	swiftCacheDirName = "swift-cache"
	// cacheRecordName is every cache entry's record, Swift's and Go's.
	cacheRecordName        = "record.json"
	swiftCacheKeyVersion   = "freeside.macverify.swift-cache-key/v1"
	swiftCacheRecordFormat = "freeside.macverify.swift-cache-record/v1"
	packageResolvedName    = "Package.resolved"
)

// ErrSwiftCacheMissing refuses a job whose workspace's Swift dependency key
// has no complete cache entry. The command never runs.
var ErrSwiftCacheMissing = errors.New("macverify: no complete Swift dependency cache entry")

var swiftCacheKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// keyedFile is one file a dependency cache key covers: its slash-separated
// path relative to the workspace and the SHA-256 of its bytes.
type keyedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// swiftCacheKey is a workspace's Swift dependency key. A zero Key means the
// workspace holds no Package.resolved and needs no cache.
type swiftCacheKey struct {
	Key   string
	Files []keyedFile
}

func (k swiftCacheKey) paths() []string {
	paths := make([]string, len(k.Files))
	for i, f := range k.Files {
		paths[i] = f.Path
	}
	return paths
}

// skipSourceDir names the directories the key and populate's copies leave
// out: a git directory and a SwiftPM build directory, which SwiftPM and
// Xcode never read a root Package.resolved from, and where a .build holds
// dependencies' own. Keying and copying by one rule keeps the room and
// populate on one key.
func skipSourceDir(name string) bool { return name == ".git" || name == ".build" }

// workspaceSwiftCacheKey computes the key over every Package.resolved under
// workdir, outside skipSourceDir's directories (workspaceFileKey).
func workspaceSwiftCacheKey(workdir string) (swiftCacheKey, error) {
	key, files, err := workspaceFileKey(workdir, swiftCacheKeyVersion, skipSourceDir,
		func(name string) bool { return name == packageResolvedName })
	if err != nil {
		return swiftCacheKey{}, fmt.Errorf("compute Swift dependency key: %w", err)
	}
	return swiftCacheKey{Key: key, Files: files}, nil
}

// workspaceFileKey computes a dependency cache key over every file under
// workdir whose name match accepts, outside the directories skip names: one
// SHA-256 over version and the sorted list of each file's relative path and
// its own SHA-256. The walk never follows a symlink, and a matched file that
// is not a regular file is refused, since a tool would read through it to
// bytes the key does not cover. No matched file means no key: "" and nil.
func workspaceFileKey(workdir, version string, skip, match func(name string) bool) (string, []keyedFile, error) {
	var files []keyedFile
	err := filepath.WalkDir(workdir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != workdir && skip(d.Name()) {
			return filepath.SkipDir
		}
		if !match(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(workdir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		body, err := os.ReadFile(path) //nolint:gosec // G304: a file inside the workspace being keyed
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		files = append(files, keyedFile{Path: rel, SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	if len(files) == 0 {
		return "", nil, nil
	}
	slices.SortFunc(files, func(a, b keyedFile) int { return strings.Compare(a.Path, b.Path) })
	// JSON keeps the encoding unambiguous for any file name.
	body, err := json.Marshal(struct {
		Version string      `json:"version"`
		Files   []keyedFile `json:"files"`
	}{version, files})
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), files, nil
}

// swiftCacheRecord is an entry's record.json, written last by populate.
type swiftCacheRecord struct {
	Format    string      `json:"format"`
	Key       string      `json:"key"`
	Files     []keyedFile `json:"files"`
	URLs      []string    `json:"urls"`
	Packages  []string    `json:"swiftpm_packages"`
	Container string      `json:"xcode_container,omitempty"`
	Toolchain struct {
		XcodeBuild string `json:"xcode_build"`
		Swift      string `json:"swift"`
	} `json:"toolchain"`
}

func swiftCacheEntry(root, key string) string {
	return filepath.Join(root, swiftCacheDirName, key)
}

// lookupSwiftCache reports whether key has a complete entry under root: a
// real directory holding a regular record.json that names the same key.
// Anything else is ErrSwiftCacheMissing, naming the key and its files.
func lookupSwiftCache(root string, key swiftCacheKey) error {
	if err := checkSwiftCacheEntry(root, key.Key); err != nil {
		return fmt.Errorf("%w: key %s for %s: %w; run freesided swift-cache populate on a checkout of this commit",
			ErrSwiftCacheMissing, key.Key, strings.Join(key.paths(), ", "), err)
	}
	return nil
}

func checkSwiftCacheEntry(root, key string) error {
	if !swiftCacheKeyPattern.MatchString(key) {
		return fmt.Errorf("malformed key %q", key)
	}
	entry := swiftCacheEntry(root, key)
	info, err := os.Lstat(entry)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", entry)
	}
	recordPath := filepath.Join(entry, cacheRecordName)
	info, err = os.Lstat(recordPath)
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
	var record swiftCacheRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return fmt.Errorf("decode %s: %w", recordPath, err)
	}
	if record.Format != swiftCacheRecordFormat || record.Key != key {
		return fmt.Errorf("%s records format %q and key %q", recordPath, record.Format, record.Key)
	}
	return nil
}

// writeRecord writes an entry's record.json through a rename, so the entry
// is never seen with a partial record.
func writeRecord(entry string, record any) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(entry, cacheRecordName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G302,G304: the entry's own record; slots may read it
	if err != nil {
		return err
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(entry, cacheRecordName))
}
