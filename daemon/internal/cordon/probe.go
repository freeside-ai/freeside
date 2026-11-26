package cordon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// probeVerb is the arguments before a probe's spec: the daemon's own hidden
// verb (freesided cordon probe), which the test binary answers too
// (TestMain).
var probeVerb = []string{"cordon", "probe"}

// maxProbeSpec bounds the spec argument, far above any spec the suite
// builds.
const maxProbeSpec strictjson.Limit = 1 << 20

// probeCommandTimeout bounds each command a probe runs. Under the rules
// each is refused at once.
const probeCommandTimeout = 30 * time.Second

// probeSpec is what one probe launch tries. The suite builds it and passes
// it as the probe's one argument, in JSON; checks names what the probe's
// report must prove, in the order the probe prints it.
type probeSpec struct {
	// Reads are paths the rules deny reading, which the probe opens. Present
	// says the suite found the path outside the launch; one it didn't find
	// has nothing to read, and the pre-job probe opens it once it exists.
	Reads []probeRead `json:"reads,omitempty"`
	// Creates are new paths the rules deny creating, in places the account
	// can write outside the launch.
	Creates []probeCreate `json:"creates,omitempty"`
	// Sockets are Unix sockets under a protected root that the suite serves.
	Sockets []string `json:"sockets,omitempty"`
	// Ports are reserved ports the suite could bind outside the launch.
	Ports []int `json:"ports,omitempty"`
	// Services are Mach services the rules deny looking up. Registered says
	// the suite looked it up outside the launch.
	Services []probeService `json:"services,omitempty"`
	// Denied are sandbox operations the rules deny without a filter, which
	// the probe asks the sandbox about where a live attempt has no safe
	// target: creating a launchd job, opening through Launch Services,
	// another process's task port, and obtaining an authorization right
	// from authd, which would put a prompt on the owner's screen.
	Denied []string `json:"denied,omitempty"`
	// Preferences are preference domains the rules deny writing, which the
	// probe asks the sandbox about: cfprefsd makes the write outside the
	// launch, and the real domains are the owner's, so no live attempt has
	// a safe target.
	Preferences []string `json:"preferences,omitempty"`
	// StandIn is a process of the suite's own that the probe tries to signal.
	StandIn *probeStandIn `json:"stand_in,omitempty"`
	// Commands each must fail inside the launch; the suite checks their
	// effects after it ends.
	Commands []probeCommand `json:"commands,omitempty"`
	// Detach starts a process that leaves the probe's group and session,
	// which the run-end proof must end.
	Detach bool `json:"detach,omitempty"`
}

type probeRead struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
}

type probeCreate struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
}

type probeService struct {
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
}

// probeStandIn names a process by pid and start time, so a pid the system
// reused is never signaled.
type probeStandIn struct {
	Pid   int   `json:"pid"`
	Start int64 `json:"start"`
}

type probeCommand struct {
	Name string   `json:"name"`
	Argv []string `json:"argv"`
}

func checkRead(path string) string         { return "read " + path }
func checkCreate(path string) string       { return "create " + path }
func checkConnect(path string) string      { return "connect " + path }
func checkBind(port int) string            { return "bind " + strconv.Itoa(port) }
func checkLookup(name string) string       { return "lookup " + name }
func checkDenied(op string) string         { return "denied " + op }
func checkPreference(domain string) string { return "preference-write " + domain }
func checkCommand(name string) string      { return "command " + name }
func checkSignal(pid int) string           { return "signal " + strconv.Itoa(pid) }
func detachedLine(pid int, start int64) string {
	return fmt.Sprintf("detached %d %d", pid, start)
}

// checks lists the report's checks in the order the probe runs them.
func (s probeSpec) checks() []string {
	var checks []string
	for _, r := range s.Reads {
		checks = append(checks, checkRead(r.Path))
	}
	for _, c := range s.Creates {
		checks = append(checks, checkCreate(c.Path))
	}
	for _, path := range s.Sockets {
		checks = append(checks, checkConnect(path))
	}
	for _, port := range s.Ports {
		checks = append(checks, checkBind(port))
	}
	for _, svc := range s.Services {
		checks = append(checks, checkLookup(svc.Name))
	}
	for _, op := range s.Denied {
		checks = append(checks, checkDenied(op))
	}
	for _, domain := range s.Preferences {
		checks = append(checks, checkPreference(domain))
	}
	if s.StandIn != nil {
		checks = append(checks, checkSignal(s.StandIn.Pid))
	}
	for _, c := range s.Commands {
		checks = append(checks, checkCommand(c.Name))
	}
	return checks
}

func (s probeSpec) validate() error {
	paths := make([]string, 0, len(s.Reads)+len(s.Creates)+len(s.Sockets))
	for _, r := range s.Reads {
		paths = append(paths, r.Path)
	}
	for _, c := range s.Creates {
		paths = append(paths, c.Path)
	}
	paths = append(paths, s.Sockets...)
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("%q must be a clean absolute path", path)
		}
	}
	for _, port := range s.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("port %d out of range", port)
		}
	}
	for _, domain := range s.Preferences {
		if domain == "" {
			return errors.New("an empty preference domain")
		}
	}
	if s.StandIn != nil && s.StandIn.Pid <= 1 {
		return fmt.Errorf("stand-in pid %d", s.StandIn.Pid)
	}
	for _, c := range s.Commands {
		if c.Name == "" || len(c.Argv) == 0 || !filepath.IsAbs(c.Argv[0]) {
			return fmt.Errorf("command %q needs an absolute program", c.Name)
		}
	}
	return nil
}

func (s probeSpec) arg() string {
	body, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("marshal cordon probe spec: %v", err))
	}
	return string(body)
}

// RunProbe is the cordon's conformance probe, run inside a probe launch by
// the conformance suite and the pre-job probe (freesided cordon probe). Its
// one argument is the suite's spec in JSON. It prints "ok <check>" or
// "FAIL <check>: <detail>" per check, with indented "  (…)" detail lines,
// and "detached <pid> <start>" when the spec asks it to leave a process
// behind, to stdout and nothing else. It returns 0 when every check passed,
// 1 when any failed, and 2 for a usage error.
//
// Every refusal must be the sandbox's own: EPERM from a live attempt, the
// bootstrap server's sandbox refusal of a lookup, or the sandbox's own
// answer for the operation. A refusal for any other reason fails its check.
// No check prints anything it read: a read opens and closes its file, and a
// command's output is discarded.
func RunProbe(args []string, stdout io.Writer) int {
	p := &probe{out: stdout}
	if len(args) != 1 {
		return p.usage()
	}
	var spec probeSpec
	if err := strictjson.Decode([]byte(args[0]), &spec, strictjson.RejectInvalidUTF8, maxProbeSpec); err != nil ||
		spec.validate() != nil {
		return p.usage()
	}
	p.run(spec)
	if p.failed {
		return 1
	}
	return 0
}

type probe struct {
	out    io.Writer
	failed bool
}

func (p *probe) usage() int {
	_, _ = fmt.Fprintln(p.out, "usage: freesided cordon probe <spec-json>")
	return 2
}

func (p *probe) check(name string, err error) {
	if err != nil {
		p.failed = true
		_, _ = fmt.Fprintf(p.out, "FAIL %s: %v\n", name, err)
		return
	}
	_, _ = fmt.Fprintf(p.out, "ok %s\n", name)
}

func (p *probe) detail(format string, args ...any) {
	_, _ = fmt.Fprintf(p.out, "  ("+format+")\n", args...)
}

func (p *probe) run(s probeSpec) {
	for _, r := range s.Reads {
		p.check(checkRead(r.Path), p.readDenied(r))
	}
	for _, c := range s.Creates {
		p.check(checkCreate(c.Path), p.createDenied(c))
	}
	for _, path := range s.Sockets {
		conn, err := net.Dial("unix", path)
		if err == nil {
			_ = conn.Close()
		}
		p.check(checkConnect(path), p.sandboxRefused(err))
	}
	for _, port := range s.Ports {
		p.check(checkBind(port), p.bindDenied(port))
	}
	for _, svc := range s.Services {
		p.check(checkLookup(svc.Name), p.lookupDenied(svc))
	}
	for _, op := range s.Denied {
		p.check(checkDenied(op), p.operationDenied(op))
	}
	for _, domain := range s.Preferences {
		p.check(checkPreference(domain), p.preferenceDenied(domain))
	}
	if s.StandIn != nil {
		p.check(checkSignal(s.StandIn.Pid), p.signalDenied(*s.StandIn))
	}
	for _, c := range s.Commands {
		p.check(checkCommand(c.Name), p.commandRefused(c))
	}
	if s.Detach {
		p.detach()
	}
}

// sandboxRefused passes only the sandbox's own refusal of an attempt.
func (p *probe) sandboxRefused(err error) error {
	if err == nil {
		return errors.New("succeeded")
	}
	if !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("refused with %w, not the sandbox's EPERM", err)
	}
	p.detail("refused as expected: %v", err)
	return nil
}

// readDenied opens a path the rules deny. A path the suite found absent
// has nothing to read and passes when it is still absent; sandbox_check
// can't stand in for it, because it answers every missing path as denied,
// unconfined too.
func (p *probe) readDenied(r probeRead) error {
	f, err := os.Open(r.Path)
	if err == nil {
		_ = f.Close()
	}
	if !r.Present && errors.Is(err, fs.ErrNotExist) {
		p.detail("absent; nothing to read")
		return nil
	}
	return p.sandboxRefused(err)
}

// createDenied tries a new path, and removes it again only when the attempt
// itself created it, which the exclusive create proves.
func (p *probe) createDenied(c probeCreate) error {
	var err error
	if c.Dir {
		if err = os.Mkdir(c.Path, 0o700); err == nil {
			_ = os.Remove(c.Path)
		}
	} else {
		var f *os.File
		if f, err = os.OpenFile(c.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			_ = f.Close()
			_ = os.Remove(c.Path)
		}
	}
	return p.sandboxRefused(err)
}

// bindDenied tries the port on loopback and on every address.
func (p *probe) bindDenied(port int) error {
	for _, host := range []string{"127.0.0.1", ""} {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			_ = ln.Close()
		}
		if err := p.sandboxRefused(err); err != nil {
			return fmt.Errorf("on %q: %w", host, err)
		}
	}
	return nil
}

// lookupDenied requires the rules to deny the service and the bootstrap
// server to refuse the lookup as the sandbox's. A service the suite found
// unregistered may instead read as unknown.
func (p *probe) lookupDenied(svc probeService) error {
	denied, err := sandboxDeniesService(svc.Name)
	if err != nil {
		return err
	}
	if !denied {
		return errors.New("the rules allow looking it up")
	}
	switch kr := lookUpService(svc.Name); {
	case kr == 0:
		return errors.New("looked up")
	case kr == bootstrapNotPrivileged:
		p.detail("refused as expected: bootstrap %d", kr)
		return nil
	case kr == bootstrapUnknownService && !svc.Registered:
		p.detail("not registered; the rules deny looking it up")
		return nil
	default:
		return fmt.Errorf("bootstrap refused with %d, not the sandbox's %d", kr, bootstrapNotPrivileged)
	}
}

func (p *probe) operationDenied(op string) error {
	denied, err := sandboxDeniesOperation(op)
	if err != nil {
		return err
	}
	if !denied {
		return errors.New("the rules allow it")
	}
	p.detail("the rules deny it")
	return nil
}

// unrelatedPreferenceDomain is a domain no rule names. The probe's answer
// for it must be "allowed", so a sandbox that stops matching the domain's
// name, and denies every domain once any is denied, fails the check rather
// than passing for a rule the file no longer has.
const unrelatedPreferenceDomain = "com.freeside.cordon-probe.unrelated"

// preferenceDenied asks the sandbox, with the preference-domain filter,
// whether the rules deny writing domain, and that they still allow an
// unrelated one. It writes nothing.
func (p *probe) preferenceDenied(domain string) error {
	denied, err := sandboxDeniesPreference(domain)
	if err != nil {
		return err
	}
	if !denied {
		return errors.New("the rules allow writing it")
	}
	if unrelated, err := sandboxDeniesPreference(unrelatedPreferenceDomain); err != nil {
		return err
	} else if unrelated {
		return fmt.Errorf("the sandbox denies writing %s too, so its answer doesn't name the domain", unrelatedPreferenceDomain)
	}
	p.detail("the rules deny writing it")
	return nil
}

// signalDenied sends SIGCONT, which a stopped-or-not stand-in ignores, and
// only to the stand-in the suite named: a pid whose start time still
// matches.
func (p *probe) signalDenied(s probeStandIn) error {
	start, err := processStart(s.Pid)
	if err != nil {
		return err
	}
	if start != s.Start {
		return fmt.Errorf("process %d is not the suite's stand-in", s.Pid)
	}
	return p.sandboxRefused(syscall.Kill(s.Pid, syscall.SIGCONT))
}

// commandRefused runs a command that must fail inside the launch. A
// program that isn't installed can't reach its service, which passes. The
// sandbox's own part in the refusal is proven by the operation and lookup
// checks beside it and by the effects the suite checks after the launch.
func (p *probe) commandRefused(c probeCommand) error {
	if _, err := os.Stat(c.Argv[0]); errors.Is(err, fs.ErrNotExist) {
		p.detail("%s is not installed", c.Argv[0])
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...) //nolint:gosec // G204: a command the suite names, aimed at its own stand-ins
	err := procbound.Run(cmd, 0)
	if err == nil {
		return errors.New("succeeded")
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || ctx.Err() != nil {
		return fmt.Errorf("did not run to a refusal: %w", err)
	}
	p.detail("refused as expected: exit %d", exit.ExitCode())
	return nil
}

// detach starts a process in its own session, outside the probe's group,
// and leaves it running for the run-end proof to end. It uses
// os.StartProcess, not os/exec: the child is released at once and never
// waited for, so no pipe can hold a Wait, and procbound's process group
// would undo the Setsid the check needs.
func (p *probe) detach() {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		p.check("detach", err)
		return
	}
	defer func() { _ = devnull.Close() }()
	proc, err := os.StartProcess("/bin/sleep", []string{"/bin/sleep", "3600"}, &os.ProcAttr{
		Files: []*os.File{devnull, devnull, devnull},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		p.check("detach", err)
		return
	}
	pid := proc.Pid
	start, err := processStart(pid)
	if err != nil {
		p.check("detach", err)
		return
	}
	// Release forgets the pid, so it is read first.
	_ = proc.Release()
	_, _ = fmt.Fprintln(p.out, detachedLine(pid, start))
}
