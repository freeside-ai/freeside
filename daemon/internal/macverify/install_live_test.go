//go:build darwin

package macverify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLiveInstall installs two slots on the reference host through the real
// freesided binary, under names disjoint from the default install, runs the
// room's confinement probe as an installed slot, and removes everything. It
// runs install, check, and remove as root through sudo -n, so refresh the
// sudo timestamp in the same terminal first (sudo -v):
//
//	FREESIDE_MACVERIFY_INSTALL_LIVE_TEST=1
//
// Install refuses, changing nothing, if a name or id below is already held.
//
//nolint:paralleltest // Serial: it changes the host's accounts, launchd, and pf.
func TestLiveInstall(t *testing.T) {
	if os.Getenv("FREESIDE_MACVERIFY_INSTALL_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_MACVERIFY_INSTALL_LIVE_TEST=1 on the reference host after sudo -v")
	}
	if out, err := exec.Command(sudoPath, "-n", "true").CombinedOutput(); err != nil {
		t.Fatalf("sudo -n: %v\n%s\nrun sudo -v first", err, out)
	}
	spec := InstallSpec{
		Root: "/Library/Freeside/macverify-livetest", SlotPrefix: "fvtest", SlotCount: 2, Group: "fvtests",
		FirstID: 470, Operator: os.Getenv("USER"), Anchor: "freeside-macverify-livetest",
		Label: "ai.freeside.macverify.pf.livetest", SudoersName: "freeside-slot-run-livetest",
		PortBase: DefaultPortBase + 100,
	}
	flags := []string{
		"-root", spec.Root, "-slot-prefix", spec.SlotPrefix, "-slots", strconv.Itoa(spec.SlotCount),
		"-group", spec.Group, "-first-id", strconv.Itoa(spec.FirstID), "-anchor", spec.Anchor,
		"-label", spec.Label, "-sudoers-name", spec.SudoersName, "-port-base", strconv.Itoa(spec.PortBase),
	}
	bin := filepath.Join(t.TempDir(), "freesided")
	build := exec.Command("go", "build", "-o", bin, "github.com/freeside-ai/freeside/daemon/cmd/freesided") //nolint:gosec // G204: the go tool building this module's daemon
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build freesided: %v\n%s", err, out)
	}
	freesided := func(verb string) (string, error) {
		out, err := exec.Command(sudoPath, append([]string{"-n", bin, "macverify", verb}, flags...)...).CombinedOutput() //nolint:gosec // G204: the binary this test built, with fixed flags
		t.Logf("sudo freesided macverify %s:\n%s", verb, out)
		return string(out), err
	}
	t.Cleanup(func() {
		if _, err := freesided("remove"); err != nil {
			t.Errorf("cleanup remove: %v", err)
		}
	})

	if _, err := freesided("install"); err != nil {
		t.Fatalf("install: %v", err)
	}
	out, err := freesided("check")
	if err != nil {
		t.Fatalf("check after install: %v", err)
	}
	for _, piece := range []string{"loaded rules com.apple/" + spec.Anchor, "packet filter"} {
		if !strings.Contains(out, "present       "+piece+"\n") {
			t.Errorf("check does not report %s present", piece)
		}
	}
	host, err := NewSystemHost()
	if err != nil {
		t.Fatal(err)
	}
	findings, err := Inspect(spec, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if !f.Healthy() {
			t.Errorf("unprivileged Inspect: %s %s %s", f.Piece, f.State, f.Detail)
		}
	}
	for i, slot := range spec.Slots() {
		a, ok, err := host.User(slot)
		if err != nil || !ok || a.UID != spec.FirstID+i+1 || a.Shell != slotShell || !a.Hidden {
			t.Errorf("account %s = %+v, %v, %v", slot, a, ok, err)
		}
	}

	cfg := spec.Config()
	toolchain, err := HostToolchain(t.Context())
	if err != nil {
		t.Fatalf("HostToolchain: %v", err)
	}
	cfg.SandboxProfile, cfg.Toolchain = DefaultSandboxProfile(), toolchain
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	room, err := NewRoom(b, RoomOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runConfineProbe(t, room)

	if _, err := freesided("remove"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	findings, err = Inspect(spec, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		// /Library/Freeside stays when another install shares it.
		if f.State != StateMissing && f.State != StateUnread && !strings.HasPrefix(f.Piece, "ancestors of ") {
			t.Errorf("after remove: %s %s %s", f.Piece, f.State, f.Detail)
		}
	}
}
