package macverify

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeWorkspaceFile writes rel under dir, creating its parents.
func writeWorkspaceFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const xcodeResolved = "app/Freeside.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved"

// swiftWorkspace is a workspace shaped like Freeside's app/: one SwiftPM
// package and one Xcode project, each with a Package.resolved.
func swiftWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeWorkspaceFile(t, dir, "app/Package.swift", "// swift-tools-version:6.0\n")
	writeWorkspaceFile(t, dir, "app/Package.resolved", `{"pins":[],"version":3}`)
	writeWorkspaceFile(t, dir, xcodeResolved, `{"pins":[],"version":2}`)
	return dir
}

func mustKey(t *testing.T, dir string) swiftCacheKey {
	t.Helper()
	key, err := workspaceSwiftCacheKey(dir)
	if err != nil {
		t.Fatalf("workspaceSwiftCacheKey: %v", err)
	}
	return key
}

func TestSwiftCacheKeyCoversEveryPackageResolved(t *testing.T) {
	t.Parallel()
	dir := swiftWorkspace(t)
	base := mustKey(t, dir)
	if !swiftCacheKeyPattern.MatchString(base.Key) {
		t.Fatalf("key %q is not 64 lowercase hex", base.Key)
	}
	if want := []string{xcodeResolved, "app/Package.resolved"}; !slices.Equal(base.paths(), want) {
		t.Fatalf("keyed files = %v, want %v", base.paths(), want)
	}
	if again := mustKey(t, dir); again.Key != base.Key {
		t.Fatalf("key is not stable: %s then %s", base.Key, again.Key)
	}

	for name, mutate := range map[string]func(t *testing.T, dir string){
		"package file changes": func(t *testing.T, dir string) {
			writeWorkspaceFile(t, dir, "app/Package.resolved", `{"pins":[],"version":2}`)
		},
		"Xcode file changes": func(t *testing.T, dir string) {
			writeWorkspaceFile(t, dir, xcodeResolved, `{"pins":[{}],"version":2}`)
		},
		"a file moves": func(t *testing.T, dir string) {
			if err := os.Rename(filepath.Join(dir, "app"), filepath.Join(dir, "client")); err != nil {
				t.Fatal(err)
			}
		},
		"a file is added": func(t *testing.T, dir string) {
			writeWorkspaceFile(t, dir, "tools/Package.resolved", `{}`)
		},
		"a file is removed": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(xcodeResolved))); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := swiftWorkspace(t)
			mutate(t, dir)
			if got := mustKey(t, dir); got.Key == base.Key {
				t.Fatalf("key unchanged at %s", got.Key)
			}
		})
	}
}

func TestSwiftCacheKeySkipsGitAndBuildDirectories(t *testing.T) {
	t.Parallel()
	dir := swiftWorkspace(t)
	base := mustKey(t, dir)
	writeWorkspaceFile(t, dir, "app/.build/checkouts/dep/Package.resolved", "{}")
	writeWorkspaceFile(t, dir, ".git/modules/x/Package.resolved", "{}")
	if got := mustKey(t, dir); got.Key != base.Key {
		t.Fatalf("key moved to %s (%v) for files under .build or .git", got.Key, got.paths())
	}
}

func TestSwiftCacheKeyWithoutPackageResolvedIsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkspaceFile(t, dir, "app/Package.swift", "// no dependencies\n")
	if key := mustKey(t, dir); key.Key != "" || len(key.Files) != 0 {
		t.Fatalf("key = %+v, want none", key)
	}
}

func TestSwiftCacheKeyRefusesSymlinkedPackageResolved(t *testing.T) {
	t.Parallel()
	dir := swiftWorkspace(t)
	outside := t.TempDir()
	writeWorkspaceFile(t, outside, "Package.resolved", "{}")
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "Package.resolved"), filepath.Join(dir, "tools", "Package.resolved")); err != nil {
		t.Fatal(err)
	}
	if key, err := workspaceSwiftCacheKey(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("workspaceSwiftCacheKey = %+v, %v; want a refusal of the symlink", key, err)
	}
}

// writeEntry makes a complete cache entry for key under root, as populate
// leaves one.
func writeEntry(t *testing.T, root, key string) {
	t.Helper()
	entry := swiftCacheEntry(root, key)
	for _, dir := range []string{"seed/home", "seed/job/workspace", "seed/job/source-packages", "mirrors"} {
		if err := os.MkdirAll(filepath.Join(entry, filepath.FromSlash(dir)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRecord(entry, swiftCacheRecord{Format: swiftCacheRecordFormat, Key: key}); err != nil {
		t.Fatal(err)
	}
}

func TestRunServesACompleteSwiftCacheEntry(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	workdir := swiftWorkspace(t)
	key := mustKey(t, workdir)
	writeEntry(t, room.root, key.Key)

	if _, err := room.Run(t.Context(), workdir, []string{"swift", "test"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	prepares, _ := fake.recipeJobs()
	if prepares[0].caches.Swift != key.Key {
		t.Fatalf("prepare seeded %q, want %s", prepares[0].caches.Swift, key.Key)
	}
}

func TestRunWithoutPackageResolvedSeedsNoCache(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	if _, err := room.Run(t.Context(), t.TempDir(), []string{"true"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prepares, _ := fake.recipeJobs(); prepares[0].caches.Swift != "" {
		t.Fatalf("prepare seeded %q for a workspace with no Package.resolved", prepares[0].caches.Swift)
	}
}

func TestRunRefusesAMissingOrIncompleteSwiftCacheEntry(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(t *testing.T, root, key string){
		"missing": func(*testing.T, string, string) {},
		"no record": func(t *testing.T, root, key string) {
			writeEntry(t, root, key)
			if err := os.Remove(filepath.Join(swiftCacheEntry(root, key), cacheRecordName)); err != nil {
				t.Fatal(err)
			}
		},
		"record for another key": func(t *testing.T, root, key string) {
			writeEntry(t, root, key)
			other := swiftCacheRecord{Format: swiftCacheRecordFormat, Key: strings.Repeat("0", 64)}
			if err := os.Remove(filepath.Join(swiftCacheEntry(root, key), cacheRecordName)); err != nil {
				t.Fatal(err)
			}
			if err := writeRecord(swiftCacheEntry(root, key), other); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked entry": func(t *testing.T, root, key string) {
			elsewhere := t.TempDir()
			writeEntry(t, elsewhere, key)
			if err := os.MkdirAll(filepath.Join(root, swiftCacheDirName), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(swiftCacheEntry(elsewhere, key), swiftCacheEntry(root, key)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeLauncher()
			room := newTestRoom(t, fake)
			workdir := swiftWorkspace(t)
			key := mustKey(t, workdir)
			setup(t, room.root, key.Key)

			_, err := room.Run(t.Context(), workdir, []string{"swift", "test"})
			if !errors.Is(err, ErrSwiftCacheMissing) {
				t.Fatalf("Run error = %v, want ErrSwiftCacheMissing", err)
			}
			for _, want := range []string{key.Key, "app/Package.resolved", xcodeResolved, "swift-cache populate"} {
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

func TestRunMaskedSwiftCacheSeedsNothing(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	room.swiftCacheMasked = true
	if _, err := room.Run(t.Context(), swiftWorkspace(t), []string{"swift", "test"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prepares, _ := fake.recipeJobs(); prepares[0].caches.Swift != "" {
		t.Fatalf("masked room seeded %q", prepares[0].caches.Swift)
	}
}

func TestRunRefusesAPrepareThatSeededAnotherSwiftCache(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	other := strings.Repeat("0", 64)
	fake.reportCaches = &jobCaches{Swift: other}
	room := newTestRoom(t, fake)
	workdir := swiftWorkspace(t)
	writeEntry(t, room.root, mustKey(t, workdir).Key)

	_, err := room.Run(t.Context(), workdir, []string{"swift", "test"})
	if err == nil || !strings.Contains(err.Error(), "seeded Swift cache") || errors.Is(err, ErrAbsenceUnproven) {
		t.Fatalf("Run error = %v, want a refusal of the seeded cache", err)
	}
	calls, _, _ := fake.snapshot()
	if want := withProbe("prepare slot1", "reap slot1", "cleanup slot1"); !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v: no run, then a teardown", calls, want)
	}
}
