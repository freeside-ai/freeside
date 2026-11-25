// Stand-in LaunchAgent and supervisor for the shared-host spike (#3596,
// docs/spikes/cordoned-shared-host.md), items 1, 4, and 5. Standard library
// only; build it outside the daemon module:
//
//	go build -ldflags "-X main.version=v1" -o $ROOT/helper/helper cordoned-shared-host-helper.go
//
// serve listens on a Unix socket in the account's gui/<uid> launchd domain
// and answers one JSON request per connection:
//
//	{"cmd":"whoami"}  its version, pid, uid, parent, and launchctl managername
//	{"cmd":"run","rules":F,"params":F,"dir":D,"argv":[...]}
//	                  starts "helper supervise", which starts argv under
//	                  sandbox-exec -f F with the -D parameters in F, so the
//	                  chain is LaunchAgent, supervisor, sandbox-exec, tool,
//	                  as the daemon's cordon supervisor sits between the
//	                  daemon and the run
//
// client sends one request and prints the reply. Nothing it runs reads a
// secret; replies carry exit statuses and the tool's own output.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

var version = "dev"

type request struct {
	Cmd    string   `json:"cmd"`
	Rules  string   `json:"rules,omitempty"`
	Params string   `json:"params,omitempty"`
	Dir    string   `json:"dir,omitempty"`
	Argv   []string `json:"argv,omitempty"`
}

type reply struct {
	Version     string `json:"version"`
	PID         int    `json:"pid"`
	UID         int    `json:"uid"`
	PPID        int    `json:"ppid"`
	ManagerName string `json:"managername,omitempty"`
	Exit        int    `json:"exit"`
	Output      string `json:"output,omitempty"`
	Error       string `json:"error,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: helper serve SOCKET | supervise RULES PARAMS DIR -- ARGV... | client SOCKET JSON | version")
		os.Exit(2)
	}
	need := map[string]int{"serve": 3, "supervise": 2, "client": 4, "version": 2}
	if n, ok := need[os.Args[1]]; !ok || len(os.Args) < n {
		fmt.Fprintln(os.Stderr, "usage: helper serve SOCKET | supervise RULES PARAMS DIR -- ARGV... | client SOCKET JSON | version")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(os.Args[2]))
	case "supervise":
		os.Exit(supervise(os.Args[2:]))
	case "client":
		os.Exit(client(os.Args[2], os.Args[3]))
	case "version":
		fmt.Println(version)
	default:
		os.Exit(2)
	}
}

func serve(path string) int {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 1
	}
	_ = os.Chmod(path, 0o600)
	fmt.Fprintf(os.Stderr, "%s helper %s serving %s as pid %d\n", time.Now().UTC().Format(time.RFC3339), version, path, os.Getpid())
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "accept:", err)
			return 1
		}
		go handle(conn)
	}
}

func base() reply {
	r := reply{Version: version, PID: os.Getpid(), UID: os.Getuid(), PPID: os.Getppid()}
	if out, err := exec.Command("/bin/launchctl", "managername").Output(); err == nil {
		r.ManagerName = strings.TrimSpace(string(out))
	}
	return r
}

func handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	r := base()
	var req request
	if err == nil {
		err = json.Unmarshal(line, &req)
	}
	switch {
	case err != nil:
		r.Error = err.Error()
	case req.Cmd == "whoami":
	case req.Cmd == "run":
		self, _ := os.Executable()
		args := append([]string{"supervise", req.Rules, req.Params, req.Dir, "--"}, req.Argv...)
		cmd := exec.Command(self, args...) //nolint:gosec // the spike's own chain
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		r.Output = out.String()
		r.Exit = exitCode(err)
		if err != nil && r.Exit < 0 {
			r.Error = err.Error()
		}
	default:
		r.Error = "unknown cmd " + req.Cmd
	}
	body, _ := json.Marshal(r)
	_, _ = conn.Write(append(body, '\n'))
	fmt.Fprintf(os.Stderr, "%s %s exit=%d\n", time.Now().UTC().Format(time.RFC3339), req.Cmd, r.Exit)
}

// supervise runs argv under sandbox-exec with the rendered rule file and
// its -D parameters, one per line in the params file.
func supervise(args []string) int {
	if len(args) < 5 || args[3] != "--" {
		fmt.Fprintln(os.Stderr, "usage: supervise RULES PARAMS DIR -- ARGV...")
		return 2
	}
	rules, paramsFile, dir, argv := args[0], args[1], args[2], args[4:]
	body, err := os.ReadFile(paramsFile) //nolint:gosec // the spike's own params
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sb := []string{"-f", rules}
	for _, p := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		sb = append(sb, p)
	}
	cmd := exec.Command("/usr/bin/sandbox-exec", append(sb, argv...)...) //nolint:gosec // the spike's own chain
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, os.Stdout, os.Stderr
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=" + dir}
	return exitCode(cmd.Run())
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	return -1
}

func client(path, body string) int {
	conn, err := net.Dial("unix", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		return 1
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(body + "\n")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	out, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Print(string(out))
	return 0
}
