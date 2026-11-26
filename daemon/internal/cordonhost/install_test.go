package cordonhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeInstall says what the check would find: every path root-owned and
// unwritable unless named otherwise, links resolved by resolved.
type fakeInstall struct {
	euid     int
	owners   map[string]int
	writable map[string]bool
	resolved map[string]string
}

func (f fakeInstall) system() installSystem {
	return installSystem{
		euid:     f.euid,
		owner:    func(path string) (int, error) { return f.owners[path], nil },
		writable: func(path string) bool { return f.writable[path] },
		resolve: func(path string) (string, error) {
			if r, ok := f.resolved[path]; ok {
				return r, nil
			}
			return path, nil
		},
	}
}

var goodInstall = Install{
	Binary:         "/opt/freeside/bin/freesided",
	Plist:          "/Library/LaunchAgents/ai.freeside.cordon.plist",
	Socket:         "/Users/run/Library/Application Support/Freeside/cordon/launcher.sock",
	ProtectedRoots: []string{"/Users/run/Library/Application Support/Freeside"},
}

// TestCheckInstallRefusesAnInstallTheAccountCouldChange shows each refusal
// names the path that caused it.
func TestCheckInstallRefusesAnInstallTheAccountCouldChange(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		install Install
		found   fakeInstall
		names   string
	}{
		"good":             {install: goodInstall, found: fakeInstall{euid: 501}},
		"root":             {install: goodInstall, found: fakeInstall{euid: 0}, names: "runs as root"},
		"binary owner":     {install: goodInstall, found: fakeInstall{euid: 501, owners: map[string]int{goodInstall.Binary: 501}}, names: goodInstall.Binary},
		"binary directory": {install: goodInstall, found: fakeInstall{euid: 501, owners: map[string]int{"/opt/freeside": 501}}, names: "/opt/freeside "},
		"writable binary":  {install: goodInstall, found: fakeInstall{euid: 501, writable: map[string]bool{goodInstall.Binary: true}}, names: goodInstall.Binary},
		"writable plist directory": {
			install: goodInstall, found: fakeInstall{euid: 501, writable: map[string]bool{"/Library/LaunchAgents": true}},
			names: "/Library/LaunchAgents ",
		},
		"plist resolves through the account": {install: goodInstall, found: fakeInstall{
			euid: 501, resolved: map[string]string{goodInstall.Plist: "/Users/run/agent.plist"}, owners: map[string]int{"/Users/run": 501},
		}, names: "/Users/run "},
		"socket outside": {install: Install{
			Binary: goodInstall.Binary, Plist: goodInstall.Plist, Socket: "/Users/run/launcher.sock", ProtectedRoots: goodInstall.ProtectedRoots,
		}, found: fakeInstall{euid: 501}, names: "/Users/run/launcher.sock"},
		"socket resolves outside": {install: goodInstall, found: fakeInstall{euid: 501, resolved: map[string]string{
			filepath.Dir(goodInstall.Socket): "/tmp/cordon",
		}}, names: goodInstall.Socket},
		"socket too long": {install: Install{
			Binary: goodInstall.Binary, Plist: goodInstall.Plist, ProtectedRoots: goodInstall.ProtectedRoots,
			Socket: goodInstall.ProtectedRoots[0] + "/" + strings.Repeat("s", 60) + ".sock",
		}, found: fakeInstall{euid: 501}, names: "longer than a socket address"},
	}
	for name, tc := range cases {
		err := checkInstall(tc.install, tc.found.system())
		if tc.names == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error()+" ", tc.names) {
			t.Errorf("%s = %v, want a refusal naming %q", name, err, tc.names)
		}
	}
}

// TestCheckInstallReadsTheRealHost runs the real check: system files pass,
// and a file the account wrote is refused by its path.
func TestCheckInstallReadsTheRealHost(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("the check refuses root, which runs this test")
	}
	root := shortDir(t)
	install := Install{Binary: "/bin/ls", Plist: "/etc/hosts", Socket: filepath.Join(root, "l.sock"), ProtectedRoots: []string{root}}
	if err := CheckInstall(install); err != nil {
		t.Fatalf("CheckInstall: %v", err)
	}
	own := filepath.Join(root, "freesided")
	if err := os.WriteFile(own, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	install.Binary = own
	if err := CheckInstall(install); err == nil || !strings.Contains(err.Error(), own) {
		t.Fatalf("an account-owned binary = %v, want a refusal naming it", err)
	}
}
