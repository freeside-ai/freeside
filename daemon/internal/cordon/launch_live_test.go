//go:build darwin && cgo

package cordon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// The live suite probes what a launch must not reach on the owner's
// account: Launch Services, the Simulator, the container service, the
// keychain, a launchd job outside the launch, and the global preference
// domain. Each service probe first runs unconfined, so a service the host
// lacks is skipped rather than counted as refused. It opens and quits
// Calculator and uses the real home, so it is opt-in: no default test sets
// the real home.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("FREESIDE_CORDON_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_CORDON_LIVE_TEST=1 to probe the owner's session services")
	}
}

// liveFixture is a host fixture with the account's real home, which the
// keychain and preference rules are written against.
func liveFixture(t *testing.T) hostFixture {
	t.Helper()
	f := newHostFixture(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if f.cfg.Home, err = filepath.EvalSymlinks(home); err != nil {
		t.Fatal(err)
	}
	return f
}

// unconfined runs argv outside every launch and reports whether it
// succeeded, logging why when it didn't.
func unconfined(t *testing.T, argv ...string) bool {
	t.Helper()
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // G204: the live test's own control commands
	if err != nil {
		t.Logf("%v skipped: it fails outside a launch too: %v: %.200s", argv, err, out)
		return false
	}
	return true
}

func TestLiveLaunchRefusesSessionServices(t *testing.T) {
	t.Parallel()
	requireLive(t)
	f := liveFixture(t)
	var probes []string
	quitCalculator := func() { _ = exec.Command("/usr/bin/osascript", "-e", `quit app "Calculator"`).Run() }
	if unconfined(t, "/usr/bin/open", "-g", "-j", "-a", "Calculator") {
		quitCalculator()
		t.Cleanup(quitCalculator)
		probes = append(probes, probeLine("deny", "run", "/usr/bin/open", "-g", "-j", "-a", "Calculator"))
	}
	device := fmt.Sprintf("freeside-cordon-probe-%d", os.Getpid())
	if unconfined(t, "/usr/bin/xcrun", "simctl", "list", "devicetypes") {
		t.Cleanup(func() { _ = exec.Command("/usr/bin/xcrun", "simctl", "delete", device).Run() }) //nolint:gosec // G204: the test's own device name
		probes = append(probes, probeLine("deny", "run", "/usr/bin/xcrun", "simctl", "create", device,
			"com.apple.CoreSimulator.SimDeviceType.iPhone-SE-3rd-generation"))
	}
	if container, err := exec.LookPath("container"); err != nil {
		t.Logf("container probe skipped: %v", err)
	} else if unconfined(t, container, "list") {
		probes = append(probes, probeLine("deny", "run", container, "list"))
	}
	keychain := filepath.Join(f.cfg.Home, "Library/Keychains/login.keychain-db")
	if unconfined(t, "/usr/bin/security", "show-keychain-info", keychain) {
		probes = append(probes, probeLine("deny", "run", "/usr/bin/security", "show-keychain-info", keychain))
	}
	if len(probes) == 0 {
		t.Skip("no session service answers outside a launch on this host")
	}
	runProbeLaunch(t, f.backend(t), "services", probes)
}

// TestLiveLaunchCantWriteTheGlobalPreferences writes a test-only key to the
// global preference domain from inside a launch. cfprefsd writes the
// account's own plist for the launch, so the rules are written against the
// real home and only a live fixture shows them refusing the write.
func TestLiveLaunchCantWriteTheGlobalPreferences(t *testing.T) {
	t.Parallel()
	requireLive(t)
	f := liveFixture(t)
	key := fmt.Sprintf("FreesideCordonTest%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("/usr/bin/defaults", "delete", "-g", key).Run() }) //nolint:gosec // G204: the test's own preference key
	runProbeLaunch(t, f.backend(t), "preferences", []string{probeLine("any", "defaults", key)})
	if out, err := exec.Command("/usr/bin/defaults", "read", "-g", key).CombinedOutput(); err == nil { //nolint:gosec // G204: the test's own preference key
		t.Errorf("a launch wrote the global preference domain: %s", out)
	}
}

// TestLiveLaunchCantStopALaunchdJob starts a launchd job outside the
// launch and shows launchd refuses the launch's kickstart, kill, and bootout
// of it: launchd applies the launch's signal rule.
func TestLiveLaunchCantStopALaunchdJob(t *testing.T) {
	t.Parallel()
	requireLive(t)
	f := liveFixture(t)
	label := fmt.Sprintf("com.freeside.cordon-test.%d", os.Getpid())
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	plist := filepath.Join(t.TempDir(), label+".plist")
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>/bin/sleep</string><string>600</string></array>
<key>RunAtLoad</key><true/>
</dict></plist>
`, label)
	if err := os.WriteFile(plist, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !unconfined(t, "/bin/launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist) {
		t.Skip("launchctl bootstrap fails outside a launch")
	}
	t.Cleanup(func() { _ = exec.Command("/bin/launchctl", "bootout", target).Run() }) //nolint:gosec // G204: the test's own job
	before := launchdPid(t, target)
	runProbeLaunch(t, f.backend(t), "launchd", []string{
		probeLine("deny", "run", "/bin/launchctl", "kickstart", "-k", target),
		probeLine("deny", "run", "/bin/launchctl", "kill", "TERM", target),
		probeLine("deny", "run", "/bin/launchctl", "bootout", target),
	})
	if after := launchdPid(t, target); after != before {
		t.Fatalf("the job's pid changed from %d to %d", before, after)
	}
}

var launchdPidLine = regexp.MustCompile(`(?m)^\s*pid = (\d+)$`)

func launchdPid(t *testing.T, target string) int {
	t.Helper()
	out, err := exec.Command("/bin/launchctl", "print", target).CombinedOutput() //nolint:gosec // G204: the test's own job
	if err != nil {
		t.Fatalf("launchctl print %s: %v: %s", target, err, out)
	}
	m := launchdPidLine.FindSubmatch(out)
	if m == nil {
		t.Fatalf("launchctl print %s shows no pid", target)
	}
	pid, _ := strconv.Atoi(string(m[1]))
	return pid
}
