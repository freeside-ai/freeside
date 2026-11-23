//go:build darwin && cgo

package cordon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// credentialProbe runs as one of the agent's tools. It takes the token's
// length and SHA-256 and looks for those bytes, by digest, in its own
// environment, on its inherited descriptors 3 to 9, and in the argv and
// environment of every process KERN_PROCARGS2 lets it read, which are the
// account's own and so include its ancestor chain. It runs after the CLI
// has read its descriptor, so it shows what a tool started then can reach;
// open-fds, logged, says whether any of 3 to 9 is still open at all. It
// never holds the token itself, matches no other session's token, and
// writes counts only, to ./probe.out, so neither the transcript nor the
// model's reply carries one. It also tries the owner's Claude config,
// which the rules deny. Adapted from envscan.py in
// docs/spikes/cordoned-writer.zsh.
//
//nolint:gosec // G101: names the token's variable and prefix, holds no token
const credentialProbe = `import ctypes, ctypes.util, hashlib, os, select, sys
length, digest = int(sys.argv[1]), sys.argv[2]
prefix = b"sk-ant-" + b"oat"
def holds(buf):
    at = buf.find(prefix)
    while at >= 0:
        if hashlib.sha256(buf[at:at + length]).hexdigest() == digest:
            return True
        at = buf.find(prefix, at + 1)
    return False
fds = opened = 0
for fd in range(3, 10):
    try:
        os.fstat(fd)
    except OSError:
        continue
    opened += 1
    try:
        # select, not O_NONBLOCK, which would change a description the CLI
        # may share.
        if not select.select([fd], [], [], 0)[0]:
            continue
        fds += holds(os.read(fd, 1 << 16))
    except OSError:
        continue
own = holds(b"\0".join(k + b"=" + v for k, v in os.environb.items()))
var = int("CLAUDE_CODE_OAUTH_TOKEN" in os.environ)
libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
buf = ctypes.create_string_buffer(1 << 20)
readable = fdvar = hits = 0
for pid in range(1, 100000):
    mib = (ctypes.c_int * 3)(1, 49, pid)  # CTL_KERN, KERN_PROCARGS2
    size = ctypes.c_size_t(1 << 20)
    if libc.sysctl(mib, 3, buf, ctypes.byref(size), None, 0) != 0 or size.value <= 64:
        continue
    readable += 1
    raw = buf.raw[:size.value]
    fdvar += b"\0CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3\0" in raw
    hits += holds(raw)
try:
    open(os.path.join(os.environ["HOME"], ".claude.json"), "rb").close()
    owner = "readable"
except PermissionError:
    owner = "denied"
except OSError as e:
    owner = "error-%d" % e.errno
with open("probe.out", "w") as out:
    out.write("own-env %d token-var %d fds %d open-fds %d readable %d fdvar %d procs %d owner-config %s\n"
              % (own, var, fds, opened, readable, fdvar, hits, owner))
print("probe done")
`

// probeSeeder seeds a worktree holding the credential probe.
type probeSeeder struct{}

func (probeSeeder) FetchBaseWorktree(_ context.Context, _, _, _, dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "probe.py"), []byte(credentialProbe), 0o600)
}

// liveCredentialEnv returns a required input once the credential suite is
// opted into: a missing one fails rather than skips.
func liveCredentialEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is unset; the credential live suite needs it", name)
	}
	return v
}

// TestLiveCredentialLaunchAuthenticates runs the pinned CLI in a real
// cordoned launch with the token read from an enrolled identity's credential
// volume, and shows the CLI authenticates and that a tool it runs finds the
// token nowhere it can look. It reads a real credential and spends one short
// turn of the identity's quota, so it has its own opt-in:
//
//	FREESIDE_CORDON_CREDENTIAL_LIVE_TEST=1 FREESIDE_CORDON_LIVE_CLI=<pinned claude> \
//	  FREESIDE_CORDON_LIVE_TOKEN_VOLUME=<volume> FREESIDE_WARD_EXPORTER_IMAGE=<ref> \
//	  go test ./internal/cordon -run LiveCredential -v
//
// The CLI authenticating proves it used the descriptor's token: no
// environment holds one, and the rules deny the owner's Claude config and
// the keychain, which the probe checks for the config. Nothing this test
// prints may hold the token: a failure quotes the transcript only after
// checking it holds none.
func TestLiveCredentialLaunchAuthenticates(t *testing.T) {
	t.Parallel()
	if os.Getenv("FREESIDE_CORDON_CREDENTIAL_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_CORDON_CREDENTIAL_LIVE_TEST=1 to launch the real CLI with an enrolled token")
	}
	cli := liveCredentialEnv(t, "FREESIDE_CORDON_LIVE_CLI")
	volume := liveCredentialEnv(t, "FREESIDE_CORDON_LIVE_TOKEN_VOLUME")
	exporter := liveCredentialEnv(t, "FREESIDE_WARD_EXPORTER_IMAGE")
	containerBin, err := exec.LookPath("container")
	if err != nil {
		t.Fatalf("container CLI not on PATH: %v", err)
	}
	f := liveFixture(t)
	if f.cfg.AgentCLI, err = filepath.EvalSymlinks(cli); err != nil {
		t.Fatal(err)
	}
	f.cfg.Path = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	f.cfg.StallLimit = 5 * time.Minute
	version, err := exec.Command(f.cfg.AgentCLI, "--version").Output() //nolint:gosec // G204: the CLI the test was given
	if err != nil {
		t.Fatalf("%s --version: %v", f.cfg.AgentCLI, err)
	}
	t.Logf("agent CLI %s", bytes.TrimSpace(version))

	read, err := ward.ReadSetupToken(t.Context(), ward.NewCLIRuntime(containerBin), exporter, volume, nil)
	if err != nil {
		t.Fatalf("read the token from %s: %v", volume, err)
	}
	token := read.Token.Reveal()
	t.Cleanup(func() { clear(token) })
	sum := sha256.Sum256(token)

	keychainBefore := claudeKeychainItems(t)
	b, err := New(f.cfg, probeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	id := "credential"
	req := request(id)
	req.Token = read.Token
	req.Prompt = []byte("Run `python3 probe.py " + strconv.Itoa(len(token)) + " " + hex.EncodeToString(sum[:]) +
		"` in the working directory, then reply with the single word done.")
	req.Args = []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--dangerously-skip-permissions", "--safe-mode",
	}
	got, err := b.Launch(t.Context(), req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	transcript, err := os.ReadFile(got.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if at := bytes.Index(transcript, token); at >= 0 {
		t.Fatalf("the transcript holds the token at offset %d", at)
	}
	if result, ok := streamResult(transcript); !ok || result.IsError || got.ExitCode != 0 {
		t.Fatalf("the CLI did not finish authenticated: exit %d, result %+v, transcript:\n%s", got.ExitCode, result, transcript)
	}

	l := layoutFor(f.cfg.RunsRoot, id)
	out, err := os.ReadFile(filepath.Join(l.worktree, "probe.out"))
	if err != nil {
		t.Fatalf("the probe left no output: %v; transcript:\n%s", err, transcript)
	}
	if at := bytes.Index(out, token); at >= 0 {
		t.Fatalf("the probe's output holds the token at offset %d", at)
	}
	t.Logf("probe: %s", bytes.TrimSpace(out))
	counts, err := parseProbe(string(out))
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	for _, key := range []string{"own-env", "token-var", "fds", "procs"} {
		if counts[key] != "0" {
			t.Errorf("the probe found the token: %s %s", key, counts[key])
		}
	}
	// Controls: the scan read some process's arguments, the CLI's among
	// them, so a zero above isn't a scan that read nothing.
	if counts["fdvar"] == "0" || counts["readable"] == "0" {
		t.Errorf("the probe's scan reached no process carrying the descriptor variable: %s", bytes.TrimSpace(out))
	}
	if counts["owner-config"] != "denied" {
		t.Errorf("the owner's Claude config is %s to a tool, want denied", counts["owner-config"])
	}
	requireNoToken(t, filepath.Dir(l.runRoot), token)
	if after := claudeKeychainItems(t); !slices.Equal(after, keychainBefore) {
		t.Errorf("the login keychain's Claude Code items changed across the launch: %q, then %q", keychainBefore, after)
	}
}

type streamResultLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
}

// streamResult returns the stream's last result line.
func streamResult(transcript []byte) (streamResultLine, bool) {
	var last streamResultLine
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(transcript))
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for scanner.Scan() {
		var line streamResultLine
		if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Type == "result" {
			last, found = line, true
		}
	}
	return last, found
}

// parseProbe reads the probe's "key value ..." line.
func parseProbe(out string) (map[string]string, error) {
	fields := strings.Fields(out)
	if len(fields) != 16 {
		return nil, fmt.Errorf("%d fields, want 16: %q", len(fields), out)
	}
	counts := map[string]string{}
	for i := 0; i < len(fields); i += 2 {
		counts[fields[i]] = fields[i+1]
	}
	return counts, nil
}

// claudeKeychainItems lists the login keychain's Claude Code items, each
// as its service name and modification time, so a new item, a removed one,
// or one rewritten under any name the CLI derives shows as a change.
// dump-keychain without -d prints attributes, never a secret, and nothing
// but the matched attributes is kept.
func claudeKeychainItems(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("/usr/bin/security", "dump-keychain").Output()
	if err != nil {
		t.Fatalf("security dump-keychain: %v", err)
	}
	var items []string
	for _, block := range strings.Split(string(out), "keychain: ") {
		service := keychainService.FindStringSubmatch(block)
		if service == nil || !strings.HasPrefix(service[1], "Claude Code") {
			continue
		}
		modified := ""
		if m := keychainModified.FindStringSubmatch(block); m != nil {
			modified = m[1]
		}
		items = append(items, service[1]+" "+modified)
	}
	slices.Sort(items)
	return items
}

var (
	keychainService  = regexp.MustCompile(`"svce"<blob>="([^"]*)"`)
	keychainModified = regexp.MustCompile(`"mdat"<timedate>[^\n]*"(\d{14}Z)`)
)
