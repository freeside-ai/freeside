//go:build darwin && cgo

package cordon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The live suite probes the services a launch must not reach on the
// owner's account: Launch Services, the Simulator, the container service,
// and the keychain. Each probe first runs
// unconfined, so a service the host lacks is skipped rather than counted as
// refused. It opens and quits Calculator, so it is opt-in.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("FREESIDE_CORDON_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_CORDON_LIVE_TEST=1 to probe the owner's session services")
	}
}

// liveFixture is a host fixture with the account's real home, which the
// keychain rules are written against.
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
