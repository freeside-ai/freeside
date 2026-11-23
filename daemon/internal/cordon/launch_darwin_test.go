//go:build darwin && cgo

package cordon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These tests run real launches under sandbox-exec: the test binary is the
// agent (fakeagent_darwin_test.go). The home is a fake one under the test's
// directory with a stand-in at every denied secret path, so no test reads a
// real secret.

type hostFixture struct {
	cfg        Config
	daemonRoot string
	otherRoot  string
}

// newHostFixture is the fixture of every test that starts a real launch, so
// it is where those tests skip under a sandbox that refuses another.
func newHostFixture(t *testing.T) hostFixture {
	t.Helper()
	requireNestedSandbox(t)
	cfg := testConfig(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentCLI, err = filepath.EvalSymlinks(self); err != nil {
		t.Fatal(err)
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find go: %v", err)
	}
	if gobin, err = filepath.EvalSymlinks(gobin); err != nil {
		t.Fatal(err)
	}
	cfg.Path = []string{filepath.Dir(gobin), "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	// A home with a dot shows the regex inputs match only themselves.
	cfg.Projects = filepath.Join(cfg.Home, "first.last", "Code")
	cfg.EndGrace, cfg.EndProofTimeout, cfg.StallLimit = 2*time.Second, 20*time.Second, time.Minute
	for _, dir := range cfg.ProtectedRoots {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return hostFixture{cfg: cfg, daemonRoot: cfg.ProtectedRoots[0], otherRoot: cfg.ProtectedRoots[1]}
}

// nestedSandbox applies a trivial profile through the sandbox-exec a launch
// uses. It fails when this process already runs under a profile with a deny
// rule, inside which macOS refuses to apply another; only a bare
// allow-default outer profile permits it.
var nestedSandbox = sync.OnceValues(func() ([]byte, error) {
	return exec.Command(sandboxExec, "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput()
})

// requireNestedSandbox skips a test that starts a real launch when this
// process's own sandbox refuses another, as the sandbox scripts/check.sh puts
// tests under on macOS does (#3564): no launch could start there. CI and a
// bare go test have no outer sandbox and run these tests. Any other failure
// fails the test.
func requireNestedSandbox(t *testing.T) {
	t.Helper()
	out, err := nestedSandbox()
	if err == nil {
		return
	}
	if strings.Contains(string(out), "sandbox_apply: Operation not permitted") {
		t.Skipf("a launch can't apply its rule file inside this process's sandbox: %s", strings.TrimSpace(string(out)))
	}
	t.Fatalf("%s with a trivial profile: %v: %s", sandboxExec, err, out)
}

// moduleSeeder seeds a worktree holding a small Go module.
type moduleSeeder struct{}

func (moduleSeeder) FetchBaseWorktree(_ context.Context, _, _, _, dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/seeded\n\ngo 1.21\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600)
}

func (f hostFixture) backend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(f.cfg, moduleSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hostRequest(id string, prompt string, args ...string) LaunchRequest {
	r := request(id)
	r.Prompt, r.Args = []byte(prompt), append([]string{fakeAgentArg}, args...)
	return r
}

func standIn(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func probeLine(fields ...string) string { return strings.Join(fields, "\t") + "\n" }

// runProbeLaunch launches the fake agent over probes and fails the test on
// any line that isn't ok.
func runProbeLaunch(t *testing.T, b *Backend, id string, probes []string) {
	t.Helper()
	got, err := b.Launch(t.Context(), hostRequest(id, strings.Join(probes, ""), "probe"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	transcript, err := os.ReadFile(got.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	ok := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(transcript)), "\n") {
		if strings.HasPrefix(line, "ok ") {
			ok++
			continue
		}
		t.Errorf("probe: %s", line)
	}
	if ok != len(probes) || got.ExitCode != 0 {
		t.Fatalf("%d of %d probes ok, exit %d:\n%s", ok, len(probes), got.ExitCode, transcript)
	}
}

func TestHostLaunchPassesTheExitStatusThrough(t *testing.T) {
	t.Parallel()
	got, err := newHostFixture(t).backend(t).Launch(t.Context(), hostRequest("exit", "", "exit", "7"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got.ExitCode != 7 || got.EndedBy != EndedByExited {
		t.Fatalf("result = %+v, want exit 7 ended by exited", got)
	}
}

// TestHostLaunchExports shows a real launch's worktree exports once its end
// is proven: the agent writes a file and exits 0, and nothing the seed or
// the launch left in the worktree trips the hard-link refusal.
func TestHostLaunchExports(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	b := f.backend(t)
	written := filepath.Join(layoutFor(f.cfg.RunsRoot, "export").worktree, "written.txt")
	runProbeLaunch(t, b, "export", []string{probeLine("allow", "write", written)})
	res, err := b.Export(t.Context(), exportRequest("export"))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var paths []string
	for _, e := range res.Manifest.Entries {
		paths = append(paths, e.Path)
	}
	if want := []string{"go.mod", "main.go", "written.txt"}; !slices.Equal(paths, want) {
		t.Fatalf("exported %q, want %q", paths, want)
	}
	if err := b.AuthenticateReleasedExport(t.Context(), "export", res.Dir); err != nil {
		t.Fatalf("AuthenticateReleasedExport: %v", err)
	}
}

// TestHostLaunchEnvironmentIsTheFixedList shows what reaches the agent is
// exactly the fixed list and its token's descriptor: neither os/exec nor
// sandbox-exec adds to it.
func TestHostLaunchEnvironmentIsTheFixedList(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	got, err := f.backend(t).Launch(t.Context(), hostRequest("env", "", "env"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	transcript, err := os.ReadFile(got.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Split(strings.TrimSpace(string(transcript)), "\n")
	want := agentEnv(f.cfg, layoutFor(f.cfg.RunsRoot, "env"))
	slices.Sort(env)
	slices.Sort(want)
	if !slices.Equal(env, want) {
		t.Fatalf("agent environment = %q, want %q", env, want)
	}
}

// TestHostLaunchHandsTheTokenOnItsDescriptor shows the token's bytes reach
// the agent on the descriptor its environment names, that the read reaches
// end of file, that stdin holds the prompt alone, and that nothing the
// launch leaves in its directory holds the token.
func TestHostLaunchHandsTheTokenOnItsDescriptor(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	got, err := f.backend(t).Launch(t.Context(), hostRequest("token", "the prompt", "token"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	transcript, err := os.ReadFile(got.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("token %d %x\nstdin %q\n", len(fixtureToken), sha256.Sum256([]byte(fixtureToken)), "the prompt")
	if got.ExitCode != 0 || string(transcript) != want {
		t.Fatalf("exit %d, transcript %q, want %q", got.ExitCode, transcript, want)
	}
	requireNoToken(t, filepath.Dir(layoutFor(f.cfg.RunsRoot, "token").runRoot), []byte(fixtureToken))
}

// requireNoToken fails the test when any file under dir holds token. It
// names the file and the offset, never the bytes around them.
func requireNoToken(t *testing.T, dir string, token []byte) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		body, err := os.ReadFile(path) //nolint:gosec // G304: a file under the test's own launch directory
		if err != nil {
			return err
		}
		if at := bytes.Index(body, token); at >= 0 {
			t.Errorf("%s holds the token at offset %d", path, at)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

func TestHostLaunchDeniesSecretsAndLaunches(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	home := f.cfg.Home
	var probes []string
	for _, rel := range []string{
		".ssh/id_ed25519", ".config/gh/hosts.yml", ".git-credentials", ".netrc", ".aws/credentials",
		".config/gcloud/credentials.db", ".azure/msal_token_cache.json", ".kube/config", ".docker/config.json",
		".codex/auth.json", "Library/Application Support/Google/Chrome/Default/Cookies",
		"Library/Application Support/Firefox/profiles.ini", "Library/Safari/History.db",
		"Library/Cookies/Cookies.binarycookies", ".claude/.credentials.json", ".claude.json",
		"Library/Application Support/com.apple.container/volumes/token", ".freeside/roots/state",
		"Library/Application Support/Freeside/freeside.db", "Library/Application Support/Freeside Dev/freeside.db",
		"Library/Keychains/login.keychain-db", "first.last/Code/other/.env", "first.last/Code/other/.envrc",
		"first.last/Code/other/.env.local",
	} {
		probes = append(probes, probeLine("deny", "read", standIn(t, filepath.Join(home, rel))))
	}
	probes = append(probes,
		probeLine("deny", "read", standIn(t, filepath.Join(f.cfg.OwnerClaudeConfig, "settings.json"))),
		probeLine("deny", "read", standIn(t, filepath.Join(f.daemonRoot, "freeside.db"))),
		probeLine("deny", "read", standIn(t, filepath.Join(f.otherRoot, "freeside.db"))),
		probeLine("deny", "write", filepath.Join(f.daemonRoot, "planted")),
		probeLine("deny", "write", filepath.Join(f.otherRoot, "planted")),
		probeLine("deny", "write", filepath.Join(home, "Library/LaunchAgents/com.example.agent.plist")),
		// The .env rule's input is quoted: a "." in Projects matches only
		// itself, and a project's ordinary files stay readable.
		probeLine("allow", "read", standIn(t, filepath.Join(home, "firstXlast/Code/other/.env"))),
		probeLine("allow", "read", standIn(t, filepath.Join(home, "first.last/Code/other/README"))),
		probeLine("allow", "write", filepath.Join(home, "first.last/Code/other/notes")),
		// Renaming a denied path's parent would bring the path out under
		// another name; the directories stay open to new entries.
		probeLine("deny", "rename", home, home+"-moved"),
		probeLine("deny", "rename", filepath.Join(home, ".config"), filepath.Join(home, "config-moved")),
		probeLine("deny", "rename", filepath.Join(home, "Library"), filepath.Join(home, "Library-moved")),
		probeLine("deny", "rename", filepath.Join(home, "Library/Application Support"), filepath.Join(home, "Library/moved")),
		probeLine("deny", "rename", filepath.Dir(f.daemonRoot), filepath.Dir(f.daemonRoot)+"-moved"),
		probeLine("deny", "rename", filepath.Join(home, "first.last/Code/other"), filepath.Join(home, "other-moved")),
		probeLine("allow", "write", filepath.Join(home, ".config/new")),
	)
	if err := os.MkdirAll(filepath.Join(home, "Library/LaunchAgents"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := f.backend(t)
	worktree := layoutFor(f.cfg.RunsRoot, "secrets").worktree
	gocache, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		t.Fatalf("go env GOCACHE: %v", err)
	}
	probes = append(probes,
		probeLine("allow", "read", filepath.Join(worktree, "main.go")),
		probeLine("allow", "write", filepath.Join(worktree, "written")),
		probeLine("allow", "rename", filepath.Join(worktree, "written"), filepath.Join(worktree, "renamed")),
		probeLine("allow", "gobuild", worktree, strings.TrimSpace(string(gocache))),
	)
	label := fmt.Sprintf("dev.freeside.cordon-test.%d", os.Getpid())
	if unconfinedLaunchdSubmits(t, label) {
		probes = append(probes, probeLine("deny", "launchd", label))
	}
	t.Cleanup(func() { _ = exec.Command("/bin/launchctl", "remove", label).Run() }) //nolint:gosec // G204: the test's own job label
	runProbeLaunch(t, b, "secrets", probes)
}

// unconfinedLaunchdSubmits is the launchd probe's control: a host whose
// own launchctl submit fails can't show the rules refused it.
func unconfinedLaunchdSubmits(t *testing.T, label string) bool {
	t.Helper()
	control := label + ".control"
	out, err := exec.Command("/bin/launchctl", "submit", "-l", control, "--", "/usr/bin/true").CombinedOutput() //nolint:gosec // G204: the test's own job label
	_ = exec.Command("/bin/launchctl", "remove", control).Run()                                                 //nolint:gosec // G204: the test's own job label
	if err != nil {
		t.Logf("launchd probe skipped: unconfined launchctl submit fails here: %v: %s", err, out)
		return false
	}
	return true
}

// TestHostLaunchProtectsTheDaemonAndVerification probes the rules that keep
// a launch off the daemon and off verification against stand-ins the test
// starts outside the launch.
func TestHostLaunchProtectsTheDaemonAndVerification(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	// A Unix socket path is limited to 104 bytes, so the socket's protected
	// root is a short directory of its own; t.TempDir's path is too long.
	// The cleanup removes created, which nothing assigns again.
	created, err := os.MkdirTemp("", "cdn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(created) })
	sockRoot, err := filepath.EvalSymlinks(created)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.ProtectedRoots = append(f.cfg.ProtectedRoots, sockRoot)
	slices.Sort(f.cfg.ProtectedRoots)
	// The launch's own TMPDIR sits under the same root, short enough for a
	// socket of its own.
	f.cfg.RunsRoot = filepath.Join(sockRoot, "r")
	f.cfg.ReservedPorts = []int{freePort(t)}
	open := freePort(t)

	daemonSocket := listenUnix(t, filepath.Join(sockRoot, "control.sock"))
	otherSocket := listenUnix(t, filepath.Join(filepath.Dir(sockRoot), filepath.Base(sockRoot)+".open"))
	standInPid := startStandIn(t, f.cfg.AgentCLI)

	probes := []string{
		probeLine("deny", "signal", strconv.Itoa(standInPid)),
		probeLine("allow", "signal-child"),
		probeLine("deny", "connect", daemonSocket),
		probeLine("allow", "connect", otherSocket),
		probeLine("allow", "selfsocket"),
		probeLine("deny", "bind", strconv.Itoa(f.cfg.ReservedPorts[0])),
		probeLine("allow", "bind", strconv.Itoa(open)),
		probeLine("deny", "write", filepath.Join(f.cfg.UserTemp, "freeside-verify-job", "planted")),
		probeLine("deny", "write", filepath.Join(f.cfg.UserCache, "freeside-verify-job", "planted")),
		probeLine("allow", "write", filepath.Join(f.cfg.UserTemp, "owner-scratch")),
		probeLine("deny", "rename", f.cfg.UserTemp, f.cfg.UserTemp+"-moved"),
		probeLine("deny", "write", filepath.Join(f.cfg.Home, "Library/Preferences/.GlobalPreferences.plist")),
	}
	for _, dir := range []string{
		filepath.Join(f.cfg.UserTemp, "freeside-verify-job"), filepath.Join(f.cfg.UserCache, "freeside-verify-job"),
		filepath.Join(f.cfg.Home, "Library/Preferences"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if dir := ownedExecutorDir(t); dir != "" {
		planted := filepath.Join(dir, fmt.Sprintf("cordon-planted-%d", os.Getpid()))
		t.Cleanup(func() { _ = os.Remove(planted) })
		probes = append(probes, probeLine("deny", "write", planted))
	}
	if unconfinedAttaches(t, standInPid) {
		probes = append(probes, probeLine("deny", "attach", strconv.Itoa(standInPid)))
	}
	runProbeLaunch(t, f.backend(t), "daemon", probes)
	if !processLive(t, standInPid) {
		t.Error("the stand-in outside the launch died")
	}
}

// ownedExecutorDir finds a directory under /Library/Freeside the test's
// account can write, such as an installed executor's Go cache, so the write
// probe shows the rule refusing it rather than the directory's owner.
func ownedExecutorDir(t *testing.T) string {
	t.Helper()
	executors, _ := filepath.Glob("/Library/Freeside/*/*")
	for _, dir := range executors {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o200 == 0 {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() {
			return dir
		}
	}
	t.Log("/Library/Freeside write probe skipped: no installed executor directory this account owns")
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func listenUnix(t *testing.T, path string) string {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return path
}

// startStandIn starts a process outside every launch: the test binary, so
// it isn't a hardened platform binary a task port is refused for anyway.
func startStandIn(t *testing.T, self string) int {
	t.Helper()
	cmd := exec.Command(self, fakeAgentArg, "idle")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// unconfinedAttaches is the attach probe's control: macOS refuses an
// unentitled task_for_pid until the owner approves developer tools, so a
// host that refuses it unconfined can't show the rules refused it.
func unconfinedAttaches(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("/usr/bin/vmmap", "-summary", strconv.Itoa(pid)).CombinedOutput() //nolint:gosec // G204: vmmap on the test's stand-in pid
	if err != nil {
		t.Logf("attach probe skipped: unconfined vmmap fails here: %v: %.200s", err, out)
		return false
	}
	return true
}

// TestHostLaunchEndsEverySurvivor shows each ending leaves nothing alive,
// including a child that left the launch's process group and session.
func TestHostLaunchEndsEverySurvivor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		then string
		want EndedBy
	}{
		{"exit", EndedByExited},
		{"chatter", EndedByCanceled},
		{"silent", EndedByStalled},
	}
	for _, tc := range cases {
		t.Run(string(tc.want), func(t *testing.T) {
			t.Parallel()
			f := newHostFixture(t)
			if tc.want == EndedByStalled {
				f.cfg.StallLimit = 2 * time.Second
			}
			b := f.backend(t)
			id := "survivors-" + tc.then
			pidsFile := filepath.Join(layoutFor(f.cfg.RunsRoot, id).worktree, "pids")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.want == EndedByCanceled {
				go func() {
					awaitFile(t, pidsFile)
					cancel()
				}()
			}
			got, err := b.Launch(ctx, hostRequest(id, "", "spawn", tc.then))
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if got.EndedBy != tc.want {
				t.Fatalf("EndedBy = %s, want %s", got.EndedBy, tc.want)
			}
			body, err := os.ReadFile(pidsFile) //nolint:gosec // G304: the fake agent's file in the test's worktree
			if err != nil {
				t.Fatal(err)
			}
			pids := strings.Fields(string(body))
			if len(pids) != 3 {
				t.Fatalf("pids = %q, want three", pids)
			}
			for _, field := range pids {
				pid, err := strconv.Atoi(field)
				if err != nil {
					t.Fatal(err)
				}
				if processLive(t, pid) {
					t.Errorf("survivor %d is still alive after the launch returned", pid)
				}
			}
		})
	}
}

// TestHostLaunchEndsAForkChain launches a chain whose every link forks the
// next and exits at once, so a pass that lists the host and then queries
// each pid finds each link gone. The proof may pass only once the chain is
// dead; otherwise the launch ends blocked.
func TestHostLaunchEndsAForkChain(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	f.cfg.EndProofTimeout = 3 * time.Second
	id := "hops"
	l := layoutFor(f.cfg.RunsRoot, id)
	hops := filepath.Join(l.worktree, "hops")
	_, err := f.backend(t).Launch(t.Context(), hostRequest(id, "", "hop"))
	var blocked *BlockedError
	switch {
	case err == nil:
		if growing(t, hops) {
			t.Error("the proof passed while the fork chain was still running")
		}
	case errors.As(err, &blocked):
		t.Logf("the launch ended blocked: %v", err)
	default:
		t.Fatalf("Launch: %v", err)
	}
	// Whatever the result, stop the chain before the test ends.
	target := proofTarget{tag: l.tag, control: l.control}
	host := darwinHost{}
	for deadline := time.Now().Add(30 * time.Second); growing(t, hops); {
		if time.Now().After(deadline) {
			t.Fatal("the fork chain outlived the test's own cleanup")
		}
		procs, err := host.live()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range procs {
			if member, err := host.member(p.pid, target); err == nil && member {
				_ = host.kill(p.pid)
			}
		}
	}
}

// growing reports whether the chain appended to hops within 300 ms.
func growing(t *testing.T, hops string) bool {
	t.Helper()
	size := func() int64 {
		info, err := os.Stat(hops)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	before := size()
	time.Sleep(300 * time.Millisecond)
	return size() != before
}

// awaitFile waits for path to appear, written by a real process.
func awaitFile(t *testing.T, path string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("%s never appeared", path)
}

// processLive reports whether pid is a live, non-zombie process.
func processLive(t *testing.T, pid int) bool {
	t.Helper()
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if errors.Is(err, syscall.EIO) {
		// The sysctl answers a pid with no process with no record, which
		// golang.org/x/sys reports as EIO.
		return false
	}
	if err != nil {
		t.Fatalf("look up %d: %v", pid, err)
	}
	return int(p.Proc.P_pid) == pid && p.Proc.P_stat != stateZombie
}

// TestNoHostProcessReadsAsAMemberOfAnUnusedTag guards the proof against
// killing what isn't the launch's: with a tag no rule names, no process on
// the host, sandboxed apps included, reads as a member.
func TestNoHostProcessReadsAsAMemberOfAnUnusedTag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := proofTarget{tag: standIn(t, filepath.Join(dir, "tag")), control: standIn(t, filepath.Join(dir, "control"))}
	host := darwinHost{}
	procs, err := host.live()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		isMember, err := host.member(p.pid, target)
		if err != nil {
			t.Fatalf("query %d: %v", p.pid, err)
		}
		if isMember {
			t.Errorf("process %d reads as a member of an unused tag", p.pid)
		}
	}
	self := slices.IndexFunc(procs, func(p hostProcess) bool { return p.pid == os.Getpid() })
	if len(procs) < 2 || self < 0 {
		t.Fatalf("live() = %d processes, without this one", len(procs))
	}
	if same, err := host.same(procs[self]); err != nil || !same {
		t.Fatalf("same(this process) = %v, %v", same, err)
	}
	if same, err := host.same(hostProcess{pid: procs[self].pid, start: procs[self].start + 1}); err != nil || same {
		t.Fatalf("same(this pid, another start) = %v, %v", same, err)
	}
}
