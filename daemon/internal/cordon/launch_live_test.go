//go:build darwin && cgo

package cordon

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
	// The launch reaches securityd (#3599), so the file rule alone keeps the
	// login keychain closed. A planted item that trusts /usr/bin/security
	// reads outside a launch and not inside. Its secret is a throwaway, and a
	// probe that wrongly succeeds prints nothing it read. Adding an item
	// isn't probed: inside a launch the add asks authd for a login prompt and
	// waits on it, under these rules and the ones before (#3599's note).
	item := fmt.Sprintf("freeside-cordon-probe-%d", os.Getpid())
	if unconfined(t, "/usr/bin/security", "add-generic-password", "-s", item, "-a", "probe", "-w", rand.Text(), "-T", "/usr/bin/security", keychain) {
		t.Cleanup(func() {
			_ = exec.Command("/usr/bin/security", "delete-generic-password", "-s", item, keychain).Run() //nolint:gosec // G204: the test's own item
		})
		if unconfined(t, "/usr/bin/security", "find-generic-password", "-s", item, "-w", keychain) {
			probes = append(probes, probeLine("deny", "run", "/usr/bin/security", "find-generic-password", "-s", item, "-w", keychain))
		}
	}
	if len(probes) == 0 {
		t.Skip("no session service answers outside a launch on this host")
	}
	runProbeLaunch(t, f.backend(t), "services", probes)
}

// TestLiveLaunchCantChangeTheDefaultKeychain makes a keychain inside a
// launch and tries to make it the account's default and put it on the
// search list, through Security.framework and through cfprefsd. Either would
// send the owner's next saved credential, or the next lookup, to a keychain
// the launch holds the password to (#3599). The account's settings must read
// the same after the launch, and the test-only key must not be written.
func TestLiveLaunchCantChangeTheDefaultKeychain(t *testing.T) {
	t.Parallel()
	requireLive(t)
	f := liveFixture(t)
	// The settings compare as security prints them, errors included: a
	// default that names a keychain the launch's directory held reads as
	// missing once the run is gone.
	settings := func() (string, string) {
		def, _ := exec.Command("/usr/bin/security", "default-keychain", "-d", "user").CombinedOutput()
		list, _ := exec.Command("/usr/bin/security", "list-keychains", "-d", "user").CombinedOutput()
		return string(def), string(list)
	}
	def, list := settings()
	if len(keychainPaths(def)) != 1 || len(keychainPaths(list)) == 0 {
		t.Skipf("the account's keychain settings don't read as a default and a search list: %q, %q", def, list)
	}
	t.Cleanup(func() {
		if d, l := settings(); d != def || l != list {
			restoreKeychainSettings(t, def, list)
		}
	})
	key := fmt.Sprintf("FreesideCordonTest%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("/usr/bin/defaults", "delete", "com.apple.security", key).Run() }) //nolint:gosec // G204: the test's own preference key
	login := filepath.Join(f.cfg.Home, "Library/Keychains/login.keychain-db")
	// The launch creating its own keychain is the control: it shows the
	// launch reaches securityd and has a keychain to name.
	const own = `"$TMPDIR/probe.keychain"`
	runProbeLaunch(t, f.backend(t), "default-keychain", []string{
		probeLine("allow", "run", "/bin/sh", "-c", "/usr/bin/security create-keychain -p probe "+own),
		probeLine("any", "run", "/bin/sh", "-c", "/usr/bin/security default-keychain -d user -s "+own),
		probeLine("any", "run", "/bin/sh", "-c", "/usr/bin/security list-keychains -d user -s "+login+" "+own),
		probeLine("deny", "run", "/usr/bin/defaults", "write", "com.apple.security", key, "-string", "planted"),
	})
	if out, err := exec.Command("/usr/bin/defaults", "read", "com.apple.security", key).CombinedOutput(); err == nil { //nolint:gosec // G204: the test's own preference key
		t.Errorf("a launch wrote the keychain preference domain: %s", out)
	}
	if d, l := settings(); d != def || l != list {
		t.Errorf("a launch changed the keychain settings: default %q to %q, search list %q to %q", def, d, list, l)
	}
}

// keychainPaths reads the keychain paths security prints, one quoted path
// a line.
func keychainPaths(out string) []string {
	var paths []string
	for line := range strings.Lines(out) {
		if p := strings.Trim(strings.TrimSpace(line), `"`); strings.HasPrefix(p, "/") {
			paths = append(paths, p)
		}
	}
	return paths
}

// restoreKeychainSettings puts back the default keychain and search list
// that security printed before a launch changed them.
func restoreKeychainSettings(t *testing.T, def, list string) {
	t.Helper()
	for _, args := range [][]string{
		append([]string{"default-keychain", "-d", "user", "-s"}, keychainPaths(def)...),
		append([]string{"list-keychains", "-d", "user", "-s"}, keychainPaths(list)...),
	} {
		if out, err := exec.Command("/usr/bin/security", args...).CombinedOutput(); err != nil { //nolint:gosec // G204: the settings security printed
			t.Errorf("restoring the keychain settings: security %v: %v: %s", args, err, out)
		}
	}
}

// TestLiveLaunchInstallsAMiseTool installs a tool with mise from inside a
// launch. mise verifies TLS through Security.framework, which needs
// securityd's lookup (#3599).
func TestLiveLaunchInstallsAMiseTool(t *testing.T) {
	t.Parallel()
	requireLive(t)
	f := liveFixture(t)
	mise, err := exec.LookPath("mise")
	if err != nil {
		t.Skipf("mise probe skipped: %v", err)
	}
	env := []string{"HOME=" + f.cfg.Home, "PATH=" + strings.Join(f.cfg.Path, ":"), "TMPDIR=" + t.TempDir()}
	if err := miseInstall(mise, t.TempDir(), env); err != nil {
		t.Skipf("mise probe skipped: it fails outside a launch too: %.500v", err)
	}
	runProbeLaunch(t, f.backend(t), "mise", []string{probeLine("allow", "mise", mise)})
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

// TestLiveFullProvesTheCordonOnTheOwnersAccount runs the whole suite with
// the real outside host against the account's real home, launchd domain,
// and Simulator. It writes nothing to the home: the controls open the
// account's own secret paths only to see whether they are readable, and
// print nothing they read. Its launchd job and app are the suite's own
// stand-ins.
func TestLiveFullProvesTheCordonOnTheOwnersAccount(t *testing.T) {
	t.Parallel()
	requireLive(t)
	cfg := liveFixture(t).cfg
	cfg.OwnerClaudeConfig = filepath.Join(cfg.Home, ".claude")
	// A short protected root of its own holds the runs root, for the
	// suite's socket; the cleanup removes created, which nothing assigns
	// again.
	created, err := os.MkdirTemp("", "cdn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(created) })
	sockRoot, err := filepath.EvalSymlinks(created)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProtectedRoots = append(cfg.ProtectedRoots, sockRoot)
	slices.Sort(cfg.ProtectedRoots)
	cfg.RunsRoot = filepath.Join(sockRoot, "r")
	if cfg.AgentCLISHA256, err = fileSHA256(cfg.AgentCLI); err != nil {
		t.Fatal(err)
	}
	ports := []int{freePort(t), freePort(t)}
	slices.Sort(ports)
	cfg.ReservedPorts = slices.Compact(ports)
	b, err := New(cfg, moduleSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &fakeRecorder{}
	suite, err := NewSuite(b, WithConformanceRecorder(recorder))
	if err != nil {
		t.Fatal(err)
	}
	if err := suite.Full(t.Context()); err != nil {
		t.Fatalf("Full on the owner's account: %v", err)
	}
	if !b.Capabilities().Has(domain.CordonedCapabilities()[0]) {
		t.Fatal("a passed live pass declares nothing")
	}
	if err := suite.ProbeBeforeJob(t.Context()); err != nil {
		t.Fatalf("the pre-job probe after a live pass: %v", err)
	}
}
