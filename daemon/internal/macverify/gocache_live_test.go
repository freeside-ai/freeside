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

// The Go live suite (#3155) populates the Go module cache, installing the
// pinned toolchain if it is absent, for Freeside's tree at a commit, then
// builds the daemon through the production room, offline, and shows the job
// can write neither the cache nor the toolchain. It needs everything the
// room's live suite needs, the executor's go-cache directory (daemon/README.md,
// Set Up the macOS Verification Executor by Hand), and:
//
//	FREESIDE_MACVERIFY_GO_LIVE_TEST=1
//	FREESIDE_MACVERIFY_GO_COMMIT=<commit>   optional; defaults to HEAD
//
// The first run downloads the toolchain and every module and builds the
// daemon, so it takes minutes.

// goLiveWorkspace extracts the tree at the commit into a fresh workspace
// the daemon user owns, as the verifier's materialized one is.
func goLiveWorkspace(t *testing.T) string {
	t.Helper()
	commit := os.Getenv("FREESIDE_MACVERIFY_GO_COMMIT")
	if commit == "" {
		commit = "HEAD"
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "tree.tar")
	top := strings.TrimSpace(runGit(t, "rev-parse", "--show-toplevel"))
	runGit(t, "-C", top, "archive", "-o", archive, commit)
	if out, err := exec.Command("/usr/bin/tar", "-x", "-f", archive, "-C", dir).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed tar extracting the archive this test wrote
		t.Fatalf("extract the tree at %s: %v\n%s", commit, err, out)
	}
	return dir
}

//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLiveRoomBuildsGoOffline(t *testing.T) {
	if os.Getenv("FREESIDE_MACVERIFY_GO_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_MACVERIFY_GO_LIVE_TEST=1 with the room's live variables (daemon/README.md)")
	}
	room, cfg := liveRoom(t)
	source := goLiveWorkspace(t)

	populate := func() PopulateResult {
		t.Helper()
		res, err := PopulateGoCache(t.Context(), PopulateOptions{
			ExecutorRoot: cfg.ExecutorRoot,
			Source:       source,
			Progress:     t.Output(),
		})
		if err != nil {
			t.Fatalf("PopulateGoCache: %v", err)
		}
		return res
	}
	first := populate()
	t.Logf("cache key %s for %v (populated %t)", first.Key, first.Files, first.Populated)
	if again := populate(); again.Key != first.Key || again.Populated {
		t.Fatalf("second populate = %+v, want key %s already cached", again, first.Key)
	}

	res, err := room.Run(t.Context(), goLiveWorkspace(t), []string{"go", "-C", "daemon", "build", "./..."})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("go build from the cache = exit %d, %v\n%s", res.ExitCode, err, res.Output)
	}

	// The job reads the cache and the toolchain in place and writes neither.
	for _, path := range []string{
		filepath.Join(goCacheEntry(cfg.ExecutorRoot, first.Key), goCacheModcache, "freeside-job-write"),
		filepath.Join(goToolchainRoot(cfg.ExecutorRoot), "freeside-job-write"),
	} {
		res, err := room.Run(t.Context(), goLiveWorkspace(t), []string{"/usr/bin/touch", path})
		if err != nil {
			t.Fatalf("touch %s: %v", path, err)
		}
		if res.ExitCode == 0 {
			_ = os.Remove(path)
			t.Errorf("the job wrote %s", path)
		}
	}
}

// TestLiveHelperRefusesAnUnsafeGoCacheEntry has slot-run prepare refuse a Go
// key whose entry has no record, or a module cache others could write,
// before it touches the slot. It needs the room's live variables, the
// executor's go-cache directory, and the pinned toolchain in it.
//
//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLiveHelperRefusesAnUnsafeGoCacheEntry(t *testing.T) {
	_, cfg := liveRoom(t)
	if err := checkGoToolchain(cfg.ExecutorRoot); err != nil {
		t.Skipf("populate the Go cache first: %v", err)
	}
	key := strings.Repeat("e", 64)
	entry := goCacheEntry(cfg.ExecutorRoot, key)
	t.Cleanup(func() { _ = os.RemoveAll(entry) })
	if err := os.MkdirAll(filepath.Join(entry, goCacheModcache), 0o755); err != nil { //nolint:gosec // G301: the modes slot-run accepts
		t.Fatal(err)
	}
	prepare := func(name string) {
		t.Helper()
		cmd := exec.Command(sudoPath, "-n", helperPath(cfg.ExecutorRoot), "prepare", cfg.Slots[0], //nolint:gosec // G204: fixed sudo and helper path; the refusal under test is the point
			strings.Repeat("a", 32), t.TempDir(), "go="+key)
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != helperRefused {
			t.Errorf("prepare with %s = %v\n%s\nwant a refusal with status %d", name, err, out, helperRefused)
		}
	}
	prepare("no record")

	if err := writeRecord(entry, goCacheRecord{Format: goCacheRecordFormat, Key: key, Go: goToolchainVersion}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(entry, goCacheModcache), 0o775); err != nil { //nolint:gosec // G302: the group-writable module cache under test
		t.Fatal(err)
	}
	prepare("a group-writable module cache")
}
