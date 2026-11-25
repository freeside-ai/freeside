//go:build darwin

package cordon

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeAgentArg makes the test binary the fake agent CLI a darwin test
// launches: the launch runs os.Executable() under the rule file with this as
// its first argument.
const fakeAgentArg = "cordon-fake-agent"

func TestMain(m *testing.M) {
	args := os.Args[1:]
	// A launch's supervisor runs from the daemon's executable, which in a
	// test is this binary.
	if len(args) >= len(superviseVerb) && slices.Equal(args[:len(superviseVerb)], superviseVerb) {
		os.Exit(RunSupervisor(args[len(superviseVerb):]))
	}
	// A launch passes the CLI's fixed settings first (cliSettings).
	if len(args) >= len(cliSettings) && slices.Equal(args[:len(cliSettings)], cliSettings) {
		args = args[len(cliSettings):]
	}
	// A probe launch runs the staged copy of this binary as the daemon.
	if len(args) > len(probeVerb) && slices.Equal(args[:len(probeVerb)], probeVerb) {
		os.Exit(RunProbe(args[len(probeVerb):], os.Stdout))
	}
	if len(args) > 0 && args[0] == fakeAgentArg {
		os.Exit(fakeAgent(args[1:]))
	}
	if len(args) == 3 && args[0] == daemonStandInArg {
		os.Exit(daemonStandIn(args[1], args[2]))
	}
	os.Exit(m.Run())
}

// fakeAgent is the agent CLI's stand-in.
//
//	exit CODE        exit with CODE
//	idle             sleep until killed (a stand-in outside a launch)
//	env              print the environment, one variable a line
//	token            read the descriptor CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR
//	                 names to end of file and print its length and SHA-256,
//	                 never its bytes; then print stdin
//	probe            run each probe line from stdin; print ok or FAIL
//	hop              start a chain of processes that each fork the next and
//	                 exit at once, appending to ./hops as it goes; exit
//	spawn THEN       write its own pid to ./agent.pid, start three detached
//	                 children, write their pids to ./pids, then THEN: exit,
//	                 chatter (print until killed), silent (print nothing
//	                 until killed), or release (print until ./release
//	                 exists, then exit 0)
func fakeAgent(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "exit":
		code, _ := strconv.Atoi(args[1])
		return code
	case "idle":
		select {}
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		return 0
	case "token":
		return readToken()
	case "probe":
		return runProbes()
	case "hop":
		if err := startHopChain(); err != nil {
			fmt.Println("FAIL hop:", err)
			return 1
		}
		return 0
	case "spawn":
		if err := writeByRename("agent.pid", fmt.Sprintf("%d\n", os.Getpid())); err != nil {
			fmt.Println("FAIL agent.pid:", err)
			return 1
		}
		if err := spawnSurvivors(); err != nil {
			fmt.Println("FAIL spawn:", err)
			return 1
		}
		switch args[1] {
		case "chatter":
			for {
				fmt.Println("working")
				time.Sleep(100 * time.Millisecond)
			}
		case "silent":
			select {}
		case "release":
			for {
				if _, err := os.Stat("release"); err == nil {
					return 0
				}
				fmt.Println("working")
				time.Sleep(100 * time.Millisecond)
			}
		}
		return 0
	}
	return 2
}

// readToken reads the token the way the CLI does: the named descriptor, to
// end of file. A write end left open anywhere would hang it until the stall
// limit ends the launch.
func readToken() int {
	fd, err := strconv.Atoi(os.Getenv("CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR"))
	if err != nil {
		fmt.Println("FAIL token descriptor:", err)
		return 1
	}
	token, err := io.ReadAll(os.NewFile(uintptr(fd), "token"))
	if err != nil {
		fmt.Println("FAIL token read:", err)
		return 1
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Println("FAIL stdin read:", err)
		return 1
	}
	fmt.Printf("token %d %x\nstdin %q\n", len(token), sha256.Sum256(token), stdin)
	return 0
}

// spawnSurvivors starts the three children #3463 found outlive a CLI: one
// under nohup in the agent's group, one double-forked out of its parent, and
// one that called setsid and so left the group and the session.
func spawnSurvivors() error {
	nohup := exec.Command("/usr/bin/nohup", "/bin/sleep", "600")
	if err := nohup.Start(); err != nil {
		return err
	}
	out, err := exec.Command("/bin/sh", "-c", "/bin/sleep 600 >/dev/null 2>&1 & echo $!").Output()
	if err != nil {
		return err
	}
	doubleFork := strings.TrimSpace(string(out))
	setsid := exec.Command("/bin/sleep", "600")
	setsid.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := setsid.Start(); err != nil {
		return err
	}
	return writeByRename("pids", fmt.Sprintf("%d\n%s\n%d\n", nohup.Process.Pid, doubleFork, setsid.Process.Pid))
}

// writeByRename writes body to name through a rename: a test acts once
// name appears, and a kill between creating and writing it would leave it
// empty.
func writeByRename(name, body string) error {
	if err := os.WriteFile(name+".tmp", []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(name+".tmp", name)
}

// startHopChain starts a process that is never alive for long: each link
// forks the next and exits at once, so one link, briefly two, runs at a
// time. perl forks without exec, a few hundred microseconds a link; every
// 200 links one appends its pid to ./hops, so a test can see it run.
func startHopChain() error {
	const chain = `for (my $n = 0; ; $n++) { exit 0 if fork; ` +
		`if ($n % 200 == 0) { open(my $f, ">>", "hops") or die; print $f "$$\n"; close $f } }`
	return exec.Command("/usr/bin/perl", "-e", chain).Run()
}

// runProbes reads tab-separated probe lines, "deny|allow|any KIND ARG...",
// and prints "ok LINE" when the outcome matches or "FAIL LINE: DETAIL" when
// it doesn't. "any" is for a probe whose effect the test checks itself.
func runProbes() int {
	status := 0
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		err := fakeProbe(fields[1], fields[2:])
		switch {
		case fields[0] == "allow" && err == nil, fields[0] == "deny" && errors.Is(err, errDenied), fields[0] == "any":
			fmt.Println("ok", line)
		default:
			fmt.Printf("FAIL %s: %v\n", line, err)
			status = 1
		}
	}
	return status
}

// errDenied marks a probe the rules refused, as opposed to one that failed
// for another reason, so a missing stand-in never reads as a denial.
var errDenied = errors.New("denied")

func fakeProbe(kind string, args []string) error {
	switch kind {
	case "read":
		_, err := os.ReadFile(args[0])
		return denial(err)
	case "write":
		return denial(os.WriteFile(args[0], []byte("x"), 0o600))
	case "rename":
		return denial(os.Rename(args[0], args[1]))
	case "selfsocket":
		// A socket the launch serves and dials in its own TMPDIR.
		path := filepath.Join(os.Getenv("TMPDIR"), "s.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			return err
		}
		defer func() { _ = ln.Close() }()
		conn, err := net.Dial("unix", path) //nolint:gosec // G704: the launch's own socket in its TMPDIR
		if err == nil {
			_ = conn.Close()
		}
		return denial(err)
	case "launchd":
		// A launchd job runs outside the rules; job-creation refuses it.
		return commandDenial(exec.Command("/bin/launchctl", "submit", "-l", args[0], "--", "/usr/bin/true")) //nolint:gosec // G204: launchctl with the test's own job label
	case "signal":
		pid, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		return denial(syscall.Kill(pid, syscall.SIGCONT))
	case "signal-child":
		child := exec.Command("/bin/sleep", "600")
		if err := child.Start(); err != nil {
			return err
		}
		defer func() { _ = child.Wait() }()
		return denial(child.Process.Signal(syscall.SIGKILL))
	case "attach":
		// vmmap reads another process's memory through its task port.
		return commandDenial(exec.Command("/usr/bin/vmmap", "-summary", args[0])) //nolint:gosec // G204: vmmap on the test's stand-in pid
	case "connect":
		conn, err := net.Dial("unix", args[0])
		if err == nil {
			_ = conn.Close()
		}
		return denial(err)
	case "bind":
		ln, err := net.Listen("tcp", "127.0.0.1:"+args[0])
		if err == nil {
			_ = ln.Close()
		}
		return denial(err)
	case "defaults":
		return commandDenial(exec.Command("/usr/bin/defaults", "write", "-g", args[0], "-string", "planted")) //nolint:gosec // G204: the test's own preference key
	case "run":
		return commandDenial(exec.Command(args[0], args[1:]...)) //nolint:gosec // G204: a command the live test names
	case "gobuild":
		cmd := exec.Command("go", "build", "-o", "built", ".")
		cmd.Dir = args[0]
		cmd.Env = append(os.Environ(), "GOCACHE="+args[1], "GOPATH="+filepath.Join(os.Getenv("TMPDIR"), "gopath"),
			"GOTOOLCHAIN=local", "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go build: %w: %s", err, out)
		}
		return nil
	case "mise":
		return miseInstall(args[0], filepath.Join(os.Getenv("TMPDIR"), "mise"), os.Environ())
	}
	return fmt.Errorf("unknown probe %q", kind)
}

// miseTool is the tool the mise probe installs, the one the shared-host
// spike installed (#3596).
const miseTool, miseToolVersion = "jq", "1.7.1"

// miseInstall installs miseTool with mise into fresh data, cache, state, and
// config directories under root and runs it. Trust in the project's
// mise.toml comes from the environment alone, so neither the install nor
// the trust leaves anything in the real home (#3601).
func miseInstall(mise, root string, env []string) error {
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o700); err != nil { //nolint:gosec // G703: the launch's TMPDIR or the test's temporary directory
		return err
	}
	config := fmt.Sprintf("[tools]\n%s = %q\n", miseTool, miseToolVersion)
	if err := os.WriteFile(filepath.Join(project, "mise.toml"), []byte(config), 0o600); err != nil { //nolint:gosec // G703: the launch's TMPDIR or the test's temporary directory
		return err
	}
	env = append(slices.Clone(env),
		"MISE_DATA_DIR="+filepath.Join(root, "data"), "MISE_CACHE_DIR="+filepath.Join(root, "cache"),
		"MISE_STATE_DIR="+filepath.Join(root, "state"), "MISE_CONFIG_DIR="+filepath.Join(root, "config"),
		"MISE_TRUSTED_CONFIG_PATHS="+project)
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(mise, args...) //nolint:gosec // G204: the mise the test found, with fixed arguments
		cmd.Dir, cmd.Env = project, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("mise %s: %w: %s", strings.Join(args, " "), err, out)
		}
		return out, nil
	}
	if _, err := run("install"); err != nil {
		return err
	}
	out, err := run("exec", "--", miseTool, "--version")
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), miseToolVersion) {
		return fmt.Errorf("%s --version printed %q, want %s", miseTool, out, miseToolVersion)
	}
	return nil
}

func denial(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %w", errDenied, err)
	}
	return err
}

// commandDenial reads a command's failure as a denial. The test runs each
// such probe unconfined first, so a command that fails everywhere is
// reported rather than counted.
func commandDenial(cmd *exec.Cmd) error {
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %w: %s", errDenied, err, strings.TrimSpace(string(out)))
	}
	return nil
}
