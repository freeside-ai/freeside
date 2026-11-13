//go:build darwin

package macverify

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Swift live suite (#2880) populates the cache for Freeside's tree at a
// pinned commit (app/'s tests read daemon test vectors, so the job gets the
// whole tree), then runs the documented recipe commands through the
// production room with networking denied, first served from the cache and
// then with the cache masked. It needs everything the room's live suite
// needs, the executor's swift-cache directory (daemon/README.md, Set Up the
// macOS Verification Executor by Hand), and:
//
//	FREESIDE_MACVERIFY_SWIFT_LIVE_TEST=1
//	FREESIDE_MACVERIFY_SWIFT_COMMIT=<commit>   optional; defaults to swiftLiveCommit
//
// The first run fetches every dependency and builds the app twice, so it
// takes minutes and leaves a few hundred megabytes in the cache.
//
// The pin is #3032's merge, the first main commit whose app tests pass in
// the room, which has no window-server session.
const swiftLiveCommit = "406563b94a95eb38dd3e8429bb509d3e425d6bba"

// swiftLiveRecipes are the recipe commands daemon/README.md documents, run
// from the job's workspace.
var swiftLiveRecipes = []struct {
	name string
	argv []string
}{
	{"swift test", []string{
		"swift", "test", "--package-path", "app",
		"--only-use-versions-from-resolved-file", "--disable-sandbox",
	}},
	{"xcodebuild", []string{
		"xcodebuild", "-project", "app/Freeside.xcodeproj", "-scheme", "FreesideMac",
		"-destination", "platform=macOS", "-derivedDataPath", "../derived-data",
		"-clonedSourcePackagesDirPath", "../source-packages",
		"-disablePackageRepositoryCache", "-onlyUsePackageVersionsFromResolvedFile",
		"-scmProvider", "system", "-skipPackagePluginValidation", "-skipMacroValidation",
		"-IDEPackageSupportDisableManifestSandbox=YES",
		"-IDEPackageSupportDisablePluginExecutionSandbox=YES",
		"OTHER_SWIFT_FLAGS=$(inherited) -disable-sandbox",
		"CODE_SIGNING_ALLOWED=NO", "build",
	}},
}

// networkAttempts are what SwiftPM and git print when they reach for the
// network the room's sandbox refuses. SwiftPM prints "Fetching <url>" before
// each clone; on the reference host (Xcode 27B52) a refused clone surfaces
// only as a later `git -C` on the missing checkout, not as a resolver error.
var networkAttempts = []string{"Could not resolve host", "nodename nor servname", "Fetching https://"}

// swiftLiveWorkspace extracts the tree at the pinned commit into a fresh
// workspace the daemon user owns, as the verifier's materialized one is.
func swiftLiveWorkspace(t *testing.T) string {
	t.Helper()
	commit := os.Getenv("FREESIDE_MACVERIFY_SWIFT_COMMIT")
	if commit == "" {
		commit = swiftLiveCommit
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "tree.tar")
	// git archive run below the top level holds only that directory, so it
	// runs at the top level for the whole tree.
	top := strings.TrimSpace(runGit(t, "rev-parse", "--show-toplevel"))
	runGit(t, "-C", top, "archive", "-o", archive, commit)
	if out, err := exec.Command("/usr/bin/tar", "-x", "-f", archive, "-C", dir).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed tar extracting the archive this test wrote
		t.Fatalf("extract the tree at %s: %v\n%s", commit, err, out)
	}
	return dir
}

//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLiveRoomServesSwiftDependenciesOffline(t *testing.T) {
	if os.Getenv("FREESIDE_MACVERIFY_SWIFT_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_MACVERIFY_SWIFT_LIVE_TEST=1 with the room's live variables (daemon/README.md)")
	}
	room, cfg := liveRoom(t)
	source := swiftLiveWorkspace(t)

	populate := func() PopulateResult {
		t.Helper()
		res, err := PopulateSwiftCache(t.Context(), PopulateOptions{
			ExecutorRoot: cfg.ExecutorRoot,
			Source:       source,
			Progress:     t.Output(),
		})
		if err != nil {
			t.Fatalf("PopulateSwiftCache: %v", err)
		}
		return res
	}
	first := populate()
	t.Logf("cache key %s for %v (populated %t)", first.Key, first.Files, first.Populated)
	if again := populate(); again.Key != first.Key || again.Populated {
		t.Fatalf("second populate = %+v, want key %s already cached", again, first.Key)
	}

	for _, recipe := range swiftLiveRecipes {
		res, err := room.Run(t.Context(), swiftLiveWorkspace(t), recipe.argv)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%s from the cache = exit %d, %v\n%s", recipe.name, res.ExitCode, err, res.Output)
		}
	}

	// Masked, the same commands find nothing to resolve from and must fail
	// by reaching for the network the sandbox denies, which shows the cache,
	// not some other copy on the host, served them.
	room.swiftCacheMasked = true
	for _, recipe := range swiftLiveRecipes {
		res, err := room.Run(t.Context(), swiftLiveWorkspace(t), recipe.argv)
		if err != nil {
			t.Fatalf("%s with the cache masked: %v", recipe.name, err)
		}
		attempted := false
		for _, want := range networkAttempts {
			attempted = attempted || strings.Contains(string(res.Output), want)
		}
		if res.ExitCode == 0 || !attempted {
			t.Errorf("%s with the cache masked = exit %d, want a failed network attempt\n%s",
				recipe.name, res.ExitCode, res.Output)
		}
	}
}

// TestLiveHelperRefusesAnUnsafeSwiftCacheEntry has slot-run prepare refuse a
// key whose entry has no record, or a seed directory others could write,
// before it touches the slot. It needs the room's live variables and the
// executor's swift-cache directory.
//
//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLiveHelperRefusesAnUnsafeSwiftCacheEntry(t *testing.T) {
	_, cfg := liveRoom(t)
	key := strings.Repeat("e", 64)
	entry := swiftCacheEntry(cfg.ExecutorRoot, key)
	t.Cleanup(func() { _ = os.RemoveAll(entry) })
	for _, dir := range []string{"seed/job", "seed/home"} {
		if err := os.MkdirAll(filepath.Join(entry, filepath.FromSlash(dir)), 0o755); err != nil { //nolint:gosec // G301: the modes slot-run accepts
			t.Fatal(err)
		}
	}
	prepare := func(name string) {
		t.Helper()
		cmd := exec.Command(sudoPath, "-n", helperPath(cfg.ExecutorRoot), "prepare", cfg.Slots[0], //nolint:gosec // G204: fixed sudo and helper path; the refusal under test is the point
			strings.Repeat("a", 32), t.TempDir(), "swift="+key)
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != helperRefused {
			t.Errorf("prepare with %s = %v\n%s\nwant a refusal with status %d", name, err, out, helperRefused)
		}
	}
	prepare("no record")

	if err := writeRecord(entry, swiftCacheRecord{Format: swiftCacheRecordFormat, Key: key}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(entry, "seed", "job"), 0o775); err != nil { //nolint:gosec // G302: the group-writable seed under test
		t.Fatal(err)
	}
	prepare("a group-writable seed")
}
