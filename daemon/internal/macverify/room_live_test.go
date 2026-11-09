//go:build darwin

package macverify

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The live suite runs the probe in testdata/probe through the production
// room as a real slot. It needs the executor installed by hand on the
// reference host (daemon/README.md, Set Up the macOS Verification Executor
// by Hand) and the daemon user's sudoers rule for the helper:
//
//	FREESIDE_MACVERIFY_LIVE_TEST=1
//	FREESIDE_MACVERIFY_ROOT=<executor root>
//	FREESIDE_MACVERIFY_SLOTS=<slot>[,<slot>…]
const liveSkip = "set FREESIDE_MACVERIFY_LIVE_TEST=1, FREESIDE_MACVERIFY_ROOT, and FREESIDE_MACVERIFY_SLOTS " +
	"on a host with the executor installed by hand (daemon/README.md)"

func liveRoom(t *testing.T) (*Room, Config) {
	t.Helper()
	if os.Getenv("FREESIDE_MACVERIFY_LIVE_TEST") != "1" {
		t.Skip(liveSkip)
	}
	root, slots := os.Getenv("FREESIDE_MACVERIFY_ROOT"), os.Getenv("FREESIDE_MACVERIFY_SLOTS")
	if root == "" || slots == "" {
		t.Skip(liveSkip)
	}
	cfg := Config{
		ExecutorRoot:      root,
		SandboxProfile:    DefaultSandboxProfile(),
		PacketFilterRules: []byte("# the live suite does not load packet-filter rules (#2879)\n"),
		Slots:             strings.Split(slots, ","),
		ToolchainIdentity: "live-test",
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	room, err := NewRoom(b, RoomOptions{})
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}
	return room, cfg
}

// probeWorkspace builds the probe into a fresh workspace the daemon user
// owns, as the verifier's materialized workspace is.
func probeWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find go: %v", err)
	}
	build := exec.Command(gobin, "build", "-o", filepath.Join(dir, "probe"), "./testdata/probe") //nolint:gosec // G204: the go tool from PATH building the fixed probe package
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v\n%s", err, out)
	}
	return dir
}

//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLiveRoomConfinesASlot(t *testing.T) {
	room, _ := liveRoom(t)
	workspace := probeWorkspace(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	operatorHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	res, err := room.Run(t.Context(), workspace, []string{"./probe", "confine", port, operatorHome})
	t.Logf("confine probe exit %d:\n%s", res.ExitCode, res.Output)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("confine probe = exit %d, %v", res.ExitCode, err)
	}
	for _, check := range []string{
		"loopback-refused", "resolver-empty", "write-denied /private/tmp", "write-denied " + operatorHome,
		"write-job-dir", "write-home", "job-dir-owner", "environment", "no-window-server-session",
	} {
		if !strings.Contains(string(res.Output), "ok "+check+"\n") {
			t.Errorf("probe did not pass %s", check)
		}
	}

	// The confine probe left a marker in the slot's home. Sequential jobs
	// lease the first free slot, so the next job runs as the same slot and
	// must find its home reset.
	res, err = room.Run(t.Context(), workspace, []string{"./probe", "home-clean"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("home-clean probe = exit %d, %v\n%s", res.ExitCode, err, res.Output)
	}
}

//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLiveRoomReportsSignalDeath(t *testing.T) {
	room, _ := liveRoom(t)
	res, err := room.Run(t.Context(), probeWorkspace(t), []string{"./probe", "kill-self"})
	if err != nil || res.ExitCode != -1 {
		t.Fatalf("kill-self probe = exit %d, %v; want -1 through launchctl and sudo\n%s", res.ExitCode, err, res.Output)
	}
}

//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLiveRoomTimeoutReapsEscapedDescendant(t *testing.T) {
	room, _ := liveRoom(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res, err := room.Run(ctx, probeWorkspace(t), []string{"./probe", "linger"})
	if err != nil || res.ExitCode != -1 {
		t.Fatalf("linger probe = exit %d, %v; want -1 with absence proven\n%s", res.ExitCode, err, res.Output)
	}
}

//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLiveHelperRefusesUnlistedAccounts(t *testing.T) {
	_, cfg := liveRoom(t)
	for _, name := range []string{"root", "nobody", "daemon"} {
		out, err := exec.Command(sudoPath, "-n", helperPath(cfg.ExecutorRoot), "reap", name).CombinedOutput() //nolint:gosec // G204: fixed sudo and helper path; the refusal under test is the point
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != helperRefused {
			t.Errorf("slot-run reap %s = %v\n%s\nwant a refusal with status %d", name, err, out, helperRefused)
		}
	}
}

//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLiveHelperRefusesMalformedArguments(t *testing.T) {
	_, cfg := liveRoom(t)
	slot := cfg.Slots[0]
	for _, args := range [][]string{
		{"cleanup", slot, "not-a-job-id"},
		{"cleanup", slot, strings.Repeat("A", 32)},
		{"prepare", slot, strings.Repeat("a", 32), "relative/workspace"},
		{"bogus", slot},
	} {
		cmd := exec.Command(sudoPath, append([]string{"-n", helperPath(cfg.ExecutorRoot)}, args...)...) //nolint:gosec // G204: fixed sudo and helper path; the refusal under test is the point
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != helperRefused {
			t.Errorf("slot-run %v = %v\n%s\nwant a refusal with status %d", args, err, out, helperRefused)
		}
	}
}
