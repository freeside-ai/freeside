package macverify

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePrepared(t *testing.T) {
	t.Parallel()
	valid := "root=/opt/fv\nuid=401\nhome=/opt/fv/homes/slot1\ntmp=/private/var/folders/ab/x/T\ncache=/private/var/folders/ab/x/C\n" +
		"ports=47000,47001,47002,47003\nswift_cache=none\ngo_cache=none\n"
	got, err := parsePrepared([]byte(valid))
	if err != nil {
		t.Fatalf("parsePrepared: %v", err)
	}
	want := preparedJob{
		Root: "/opt/fv", UID: 401, Home: "/opt/fv/homes/slot1",
		Tmp: "/private/var/folders/ab/x/T", Cache: "/private/var/folders/ab/x/C",
		Ports: slotPortSet{47000, 47001, 47002, 47003},
	}
	if got != want {
		t.Fatalf("parsePrepared = %+v, want %+v", got, want)
	}
	key := strings.Repeat("ab", 32)
	got, err = parsePrepared([]byte(strings.Replace(valid, "swift_cache=none", "swift_cache="+key, 1)))
	if err != nil || got.Caches != (jobCaches{Swift: key}) {
		t.Fatalf("parsePrepared with a Swift cache = %+v, %v; want key %s", got, err, key)
	}
	got, err = parsePrepared([]byte(strings.Replace(valid, "go_cache=none", "go_cache="+key, 1)))
	if err != nil || got.Caches != (jobCaches{Go: key}) {
		t.Fatalf("parsePrepared with a Go cache = %+v, %v; want key %s", got, err, key)
	}
	for name, report := range map[string]string{
		"missing key":       strings.Replace(valid, "cache=/private/var/folders/ab/x/C\n", "", 1),
		"duplicate key":     valid + "uid=401\n",
		"extra key":         valid + "shell=/bin/sh\n",
		"not key=value":     valid + "noise\n",
		"relative path":     strings.Replace(valid, "home=/opt/fv/homes/slot1", "home=homes/slot1", 1),
		"unclean path":      strings.Replace(valid, "tmp=/private/var/folders/ab/x/T", "tmp=/private/var/folders/ab/x/T/", 1),
		"root uid":          strings.Replace(valid, "uid=401", "uid=0", 1),
		"signed uid":        strings.Replace(valid, "uid=401", "uid=+401", 1),
		"padded uid":        strings.Replace(valid, "uid=401", "uid=0401", 1),
		"no swift cache":    strings.Replace(valid, "swift_cache=none\n", "", 1),
		"empty swift cache": strings.Replace(valid, "swift_cache=none", "swift_cache=", 1),
		"short swift cache": strings.Replace(valid, "swift_cache=none", "swift_cache=abc", 1),
		"upper swift cache": strings.Replace(valid, "swift_cache=none", "swift_cache="+strings.Repeat("AB", 32), 1),
		"no go cache":       strings.Replace(valid, "go_cache=none\n", "", 1),
		"short go cache":    strings.Replace(valid, "go_cache=none", "go_cache=abc", 1),
		"no ports":          strings.Replace(valid, "ports=47000,47001,47002,47003\n", "", 1),
		"three ports":       strings.Replace(valid, "47000,47001,47002,47003", "47000,47001,47002", 1),
		"five ports":        strings.Replace(valid, "47000,47001,47002,47003", "47000,47001,47002,47003,47004", 1),
		"privileged port":   strings.Replace(valid, "47000,", "1023,", 1),
		"port past 65535":   strings.Replace(valid, "47003", "65536", 1),
		"padded port":       strings.Replace(valid, "47000,", "047000,", 1),
		"signed port":       strings.Replace(valid, "47000,", "+47000,", 1),
		"spaced ports":      strings.Replace(valid, "47000,47001", "47000, 47001", 1),
		"empty":             "",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := parsePrepared([]byte(report)); err == nil {
				t.Fatalf("parsePrepared accepted %q as %+v", report, got)
			}
		})
	}
}

func TestOutcomeOf(t *testing.T) {
	t.Parallel()
	waitErr := func(script string) error {
		return exec.Command("/bin/sh", "-c", script).Run() //nolint:gosec // G204: fixed shell running test-chosen scripts
	}
	for name, tc := range map[string]struct {
		err      error
		wantExit int
		wantErr  bool
	}{
		"success":        {err: nil, wantExit: 0},
		"nonzero exit":   {err: waitErr("exit 3"), wantExit: 3},
		"signal death":   {err: waitErr("kill -9 $$"), wantExit: -1},
		"lingering pipe": {err: exec.ErrWaitDelay, wantExit: -1},
		"helper refusal": {err: waitErr(fmt.Sprintf("exit %d", helperRefused)), wantErr: true},
		"not an exit":    {err: errors.New("wait: no child processes"), wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got := outcomeOf(tc.err)
			if (got.Err != nil) != tc.wantErr || (!tc.wantErr && got.ExitCode != tc.wantExit) {
				t.Fatalf("outcomeOf(%v) = %+v", tc.err, got)
			}
		})
	}
}

// TestHelperScriptConstants keeps the daemon's copies of slot-run's
// constants in step with the script.
func TestHelperScriptConstants(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		fmt.Sprintf("REFUSED=%d\n", helperRefused),
		"COMMAND_PATH=" + commandPath + "\n",
		"GO_VERSION=" + goToolchainVersion + "\n",
	} {
		if !bytes.Contains(helperScript, []byte("\n"+line)) {
			t.Errorf("slot-run.sh does not define %q", strings.TrimSpace(line))
		}
	}
}

func TestHelperScriptParses(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "slot-run")
	if err := os.WriteFile(path, helperScript, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/bin/sh", "-n", path).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed shell parse-checking a test-owned copy of the helper
		t.Fatalf("sh -n slot-run: %v\n%s", err, out)
	}
}

func TestEmbeddedAssetsAreCopies(t *testing.T) {
	t.Parallel()
	HelperScript()[0] = 'X'
	DefaultSandboxProfile()[0] = 'X'
	if helperScript[0] != '#' || defaultSandboxProfile[0] != ';' {
		t.Fatal("an accessor handed out the embedded bytes themselves")
	}
}
