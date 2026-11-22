//go:build darwin

package cordon

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
	if len(os.Args) > 1 && os.Args[1] == fakeAgentArg {
		os.Exit(fakeAgent(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// fakeAgent is the agent CLI's stand-in.
//
//	exit CODE        exit with CODE
//	env              print the environment, one variable a line
//	probe            run each probe line from stdin; print ok or FAIL
//	hop              start a chain of processes that each fork the next and
//	                 exit at once, appending to ./hops as it goes; exit
//	spawn THEN       start three detached children, write their pids to
//	                 ./pids, then THEN: exit, chatter (print until killed),
//	                 or silent (print nothing until killed)
func fakeAgent(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "exit":
		code, _ := strconv.Atoi(args[1])
		return code
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		return 0
	case "probe":
		return runProbes()
	case "hop":
		if err := startHopChain(); err != nil {
			fmt.Println("FAIL hop:", err)
			return 1
		}
		return 0
	case "spawn":
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
		}
		return 0
	}
	return 2
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
	pids := fmt.Sprintf("%d\n%s\n%d\n", nohup.Process.Pid, doubleFork, setsid.Process.Pid)
	return os.WriteFile("pids", []byte(pids), 0o600)
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

// runProbes reads tab-separated probe lines, "deny|allow KIND ARG...", and
// prints "ok LINE" when the outcome matches or "FAIL LINE: DETAIL" when it
// doesn't.
func runProbes() int {
	status := 0
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		err := probe(fields[1], fields[2:])
		switch {
		case fields[0] == "allow" && err == nil, fields[0] == "deny" && errors.Is(err, errDenied):
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

func probe(kind string, args []string) error {
	switch kind {
	case "read":
		_, err := os.ReadFile(args[0])
		return denial(err)
	case "write":
		return denial(os.WriteFile(args[0], []byte("x"), 0o600))
	case "rename":
		return denial(os.Rename(args[0], args[1]))
	case "launchd":
		// A launchd job runs outside the rules; job-creation refuses it.
		return commandDenial(exec.Command("/bin/launchctl", "submit", "-l", args[0], "--", "/usr/bin/true")) //nolint:gosec // G204: launchctl with the test's own job label
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
	}
	return fmt.Errorf("unknown probe %q", kind)
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
