package cordon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/credentialpattern"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"golang.org/x/sys/unix"
)

// probeExecutable is the probe's file name in a probe launch's run
// directory: a copy of the daemon executable that hashes to the digest the
// configuration digest names.
const probeExecutable = "probe"

// probeLabelPrefix is the only launchd label space the suite creates,
// signals, or removes.
const probeLabelPrefix = "com.freeside.cordon-probe."

// managedSettingsPath is where the agent CLI reads the settings an
// administrator manages, which outrank the launch's own --settings.
const managedSettingsPath = "/Library/Application Support/ClaudeCode/managed-settings.json"

// maxProjectEnvFiles bounds the other projects' .env files a pass opens.
const maxProjectEnvFiles = 4

// The HOME-relative paths rules.sb denies. secretReadPaths are denied
// reading, so the probe opens each that exists; writeOnlyPaths are denied
// writing only. Together they are every HOME path the rule file names,
// which a test pins against rules.sb.
var (
	secretReadPaths = []string{
		"/.ssh", "/.config/gh", "/.git-credentials", "/.netrc", "/.aws", "/.config/gcloud", "/.azure",
		"/.kube", "/.docker/config.json", "/.codex",
		"/Library/Application Support/Google/Chrome", "/Library/Application Support/Firefox",
		"/Library/Safari", "/Library/Cookies",
		"/.claude", "/.claude.json",
		"/Library/Application Support/com.apple.container", "/.freeside",
		"/Library/Application Support/Freeside", "/Library/Application Support/Freeside Dev",
		"/Library/Keychains",
	}
	writeOnlyPaths = []string{"/Library/LaunchAgents", "/Library/Preferences/.GlobalPreferences"}
)

// loginKeychain is the login keychain's file, which the probe opens beside
// its directory.
const loginKeychain = "/Library/Keychains/login.keychain-db"

// deniedServices are the Mach services the rules deny: the keychain's
// securityd, the Simulator, and the container runtime. securityd is always
// registered, so its lookup outside the launch is the lookups' control.
var deniedServices = []string{
	"com.apple.SecurityServer", "com.apple.securityd.xpc",
	"com.apple.CoreSimulator.CoreSimulatorService", "com.apple.container.apiserver",
}

const controlService = "com.apple.SecurityServer"

// deniedOperations are the operations the probe asks the sandbox about.
var deniedOperations = []string{"job-creation", "lsopen", "mach-priv-task-port"}

// secretName finds a variable or argument name that holds a credential.
// A value that is an absolute path names where a credential lives, as
// -publication-credentials-dir does, not the credential (namesSecret).
var secretName = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PASSWD|API[_-]?KEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY|CREDENTIAL)`)

// namesSecret reports whether name, a variable or a flag, holds a
// credential in value.
func namesSecret(name, value string) bool {
	return secretName.MatchString(name) && !filepath.IsAbs(value)
}

// Suite is the cordon's conformance suite: Full proves every denial and
// declares the class, and ProbeBeforeJob re-proves the cheap ones before an
// unattended launch. Construct it with NewSuite, which attaches it to its
// backend.
type Suite struct {
	b        *Backend
	recorder ConformanceRecorder
	outside  outsideHost
	// environ and args are the daemon's own environment and arguments,
	// which any process on the account can read, a launch's among them.
	environ         func() []string
	args            []string
	managedSettings string
	now             func() time.Time
}

// SuiteOption configures a Suite.
type SuiteOption func(*Suite)

// WithConformanceRecorder makes every pass durable: Full records a
// superseding marker before it starts and its outcome when it ends, and a
// failed pre-job probe records a failed pass. Without one nothing is
// declared, because a declaration needs a recorded pass.
func WithConformanceRecorder(r ConformanceRecorder) SuiteOption {
	return func(s *Suite) { s.recorder = r }
}

// NewSuite builds the suite over b and attaches it, so a launch that
// requires proof runs its pre-job probe. A backend takes one suite.
func NewSuite(b *Backend, opts ...SuiteOption) (*Suite, error) {
	if b == nil || b.rules == nil {
		return nil, errors.New("cordon: a suite needs a backend from New")
	}
	s := &Suite{
		b: b, outside: systemOutside{}, environ: os.Environ, args: os.Args,
		managedSettings: managedSettingsPath, now: time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	if !b.suite.CompareAndSwap(nil, s) {
		return nil, errors.New("cordon: the backend already has a suite")
	}
	return s, nil
}

// recordStep returns the durable record step the generation guard runs, or
// nil with no recorder. Only a passed record carries the capabilities. The
// write ignores ctx's cancellation: a pass or withdrawal already decided
// must reach the store, or a shutdown would leave a stale latest row.
func (s *Suite) recordStep(ctx context.Context, outcome domain.ConformanceOutcome) func() error {
	if s.recorder == nil {
		return nil
	}
	return func() error {
		in := domain.BackendConformanceInput{
			Backend:             domain.BackendCordonedHostProcess,
			Outcome:             outcome,
			ConfigurationDigest: s.b.ConfigurationDigest(),
			ProvedAt:            s.now().UTC(),
		}
		if outcome == domain.ConformancePassed {
			in.Capabilities = domain.NewCapabilitySnapshot(domain.CordonedCapabilities()...)
		}
		record, err := domain.NewBackendConformance(in)
		if err != nil {
			return err
		}
		return s.recorder.RecordBackendConformance(context.WithoutCancel(ctx), record)
	}
}

// Full runs the whole suite: the preconditions, then every probe inside a
// real cordoned launch with its positive control outside, then the effects
// the probe must not have had, and the run-end proof's end of a detached
// process. A nil error means every check passed and, with a recorder, the
// pass is recorded and the class declared.
//
// Tier A gate. Full supersedes the current proof first, in memory and
// durably, and records its own outcome last, under the generation guard: a
// pass that a newer pass or a failed pre-job probe superseded records and
// declares nothing. A failure to write either record wraps
// ErrConformanceRecord; any other failure is a ConformanceFailure or an
// error that leaves the class undeclared.
func (s *Suite) Full(ctx context.Context) (err error) {
	generation, beginErr := s.b.beginConformanceProof(s.recordStep(ctx, domain.ConformanceSuperseded))
	if beginErr != nil {
		return fmt.Errorf("%w: superseding marker: %w", ErrConformanceRecord, beginErr)
	}
	proved := false
	defer func() {
		if recovered := recover(); recovered != nil {
			// Best effort: the panic is the failure that matters, and an
			// unrecorded failure leaves nothing declared.
			_, _ = s.b.concludeConformanceProof(generation, false, s.recordStep(ctx, domain.ConformanceFailed))
			panic(recovered)
		}
		outcome := proved && err == nil
		result := domain.ConformanceFailed
		if outcome {
			result = domain.ConformancePassed
		}
		published, recordErr := s.b.concludeConformanceProof(generation, outcome && s.recorder != nil, s.recordStep(ctx, result))
		switch {
		case published && recordErr != nil:
			err = errors.Join(err, fmt.Errorf("%w: %w", ErrConformanceRecord, recordErr))
		case !published && outcome:
			err = errors.New("cordon: the conformance pass was superseded before it finished; it declares nothing")
		}
	}()
	if err := s.preconditions(); err != nil {
		return err
	}
	if err := s.full(ctx); err != nil {
		return err
	}
	proved = true
	return nil
}

// ProbeBeforeJob re-proves, inside a launch of its own, the checks with no
// lasting effect: the preconditions, the secret reads, the protected
// writes, the reserved ports, and the service and operation denials. It
// refuses when no recorded pass declares the class, and passes only when
// the declaration it began under still stands at its end, so a pass that
// began or failed meanwhile refuses the job. A failed probe withdraws the
// declaration, in memory and by a durable failed record, so only a new
// Full declares again; a failure to write that record wraps
// ErrConformanceRecord. The record is failed rather than superseded
// because the store reads a superseded latest row as a re-proof under way
// and lets admitted work authenticate against the pass before it. A
// canceled ctx withdraws nothing, since a job that went away proved
// nothing broken, unless the failure is a ConformanceFailure.
//
// Tier A gate: it runs before every launch that requires proof.
func (s *Suite) ProbeBeforeJob(ctx context.Context) error {
	generation, declared := s.b.declaredGeneration()
	if !declared {
		return failure("declaration", "no recorded pass declares the cordoned class")
	}
	err := s.preconditions()
	if err == nil {
		err = s.prejob(ctx)
	}
	if err == nil {
		if current, declared := s.b.declaredGeneration(); current != generation || !declared {
			return failure("declaration", "a conformance pass began or failed while the probe ran")
		}
		return nil
	}
	// A canceled job proved nothing broken, unless its report already did.
	var proven *ConformanceFailure
	if ctx.Err() != nil && !errors.As(err, &proven) {
		return err
	}
	if wErr := s.b.withdrawConformance(s.recordStep(ctx, domain.ConformanceFailed)); wErr != nil {
		return errors.Join(err, fmt.Errorf("%w: withdrawal marker: %w", ErrConformanceRecord, wErr))
	}
	return err
}

// preconditions are the checks outside any launch: nothing secret in what
// a launch can read of the daemon, the CLI's own sandbox off, the rule file
// its digest, and the CLI its pin.
func (s *Suite) preconditions() error {
	var names []string
	for _, kv := range s.environ() {
		name, value, _ := strings.Cut(kv, "=")
		if value != "" && (namesSecret(name, value) || credentialpattern.Match([]byte(value))) {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		return failure("daemon environment", "a launch can read the daemon's environment, and it holds a credential in %s",
			strings.Join(names, ", "))
	}
	for i, arg := range s.args {
		name, value, hasValue := strings.Cut(arg, "=")
		if credentialpattern.Match([]byte(arg)) || (hasValue && namesSecret(name, value)) {
			return failure("daemon arguments", "a launch can read the daemon's arguments, and argument %d holds a credential", i)
		}
	}
	if err := checkManagedSettings(s.managedSettings); err != nil {
		return failure("agent CLI sandbox", "%v", err)
	}
	if contentaddr.Sum(s.b.rules) != RuleDigest(s.b.cfg) {
		return failure("rule file", "the backend's rule file is not its configuration's")
	}
	digest, err := fileSHA256(s.b.cfg.AgentCLI)
	if err != nil {
		return failure("agent CLI pin", "%v", err)
	}
	if digest != s.b.cfg.AgentCLISHA256 {
		return failure("agent CLI pin", "%s is %s, not the pinned %s", s.b.cfg.AgentCLI, digest, s.b.cfg.AgentCLISHA256)
	}
	return nil
}

// checkManagedSettings refuses managed settings that set the CLI's own
// sandbox: they outrank the launch's --settings, so the CLI's sandbox could
// be on under the cordon, where it fails every Bash call. Absent managed
// settings pass.
func checkManagedSettings(path string) error {
	body, err := os.ReadFile(path) //nolint:gosec // G304: the CLI's fixed managed-settings path
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read managed settings: %w", err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(body, &settings); err != nil {
		return fmt.Errorf("managed settings %s: %w", path, err)
	}
	if _, ok := settings["sandbox"]; ok {
		return fmt.Errorf("managed settings %s set the CLI's sandbox, which outranks the launch's --settings", path)
	}
	return nil
}

// targets is what a pass found outside the launch: each target's positive
// control has passed, and the spec holds only targets the probe can prove.
type targets struct {
	spec probeSpec
	// cleanup undoes what the controls set up.
	cleanup []func()
}

func (t *targets) close() {
	for i := len(t.cleanup) - 1; i >= 0; i-- {
		t.cleanup[i]()
	}
}

// commonTargets are the targets both Full and ProbeBeforeJob probe, with
// their controls run.
func (s *Suite) commonTargets(nonce string) (*targets, error) {
	t := &targets{}
	reads, err := s.readTargets()
	if err != nil {
		return nil, err
	}
	t.spec.Reads = reads
	if t.spec.Creates, err = s.createTargets(nonce); err != nil {
		return nil, err
	}
	if t.spec.Ports, err = s.portTargets(); err != nil {
		return nil, err
	}
	if t.spec.Services, err = serviceTargets(); err != nil {
		return nil, err
	}
	for _, op := range deniedOperations {
		denied, err := sandboxDeniesOperation(op)
		if err != nil {
			return nil, failure(checkDenied(op), "control: %v", err)
		}
		if denied {
			return nil, failure(checkDenied(op), "control: the daemon itself is denied it")
		}
	}
	t.spec.Denied = deniedOperations
	return t, nil
}

// readTargets are the secret paths, the login keychain's file, the owner's
// Claude configuration, and a few of the other projects' .env files. A
// present path's control is opening it outside the launch; a path the
// account can't open either is already denied to every process the account
// runs and is left out.
func (s *Suite) readTargets() ([]probeRead, error) {
	paths := make([]string, 0, len(secretReadPaths)+3+maxProjectEnvFiles)
	for _, rel := range secretReadPaths {
		paths = append(paths, s.b.cfg.Home+rel)
	}
	paths = append(paths, s.b.cfg.Home+loginKeychain, s.b.cfg.OwnerClaudeConfig)
	envs, err := projectEnvFiles(s.b.cfg.Projects)
	if err != nil {
		return nil, err
	}
	paths = append(paths, envs...)
	// The owner's Claude configuration is usually one of the rule file's
	// home paths too, and the report proves each check once.
	slices.Sort(paths)
	paths = slices.Compact(paths)
	reads := make([]probeRead, 0, len(paths))
	for _, path := range paths {
		f, err := os.Open(path) //nolint:gosec // G304: a configured secret path, opened and closed, never read
		switch {
		case err == nil:
			_ = f.Close()
			reads = append(reads, probeRead{Path: path, Present: true})
		case errors.Is(err, fs.ErrNotExist):
			reads = append(reads, probeRead{Path: path})
		case errors.Is(err, fs.ErrPermission):
			// Unreadable to the account itself, so the cordon's denial
			// can't be told from the system's.
		default:
			return nil, failure(checkRead(path), "control: %v", err)
		}
	}
	return reads, nil
}

// projectEnvFiles finds .env, .env.* and .envrc files one level under the
// projects directory, the shape the rules deny, up to maxProjectEnvFiles.
func projectEnvFiles(projects string) ([]string, error) {
	var found []string
	for _, pattern := range []string{"*/.env", "*/.env.*", "*/.envrc"} {
		matches, err := filepath.Glob(filepath.Join(projects, pattern))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if info, err := os.Lstat(m); err == nil && info.Mode().IsRegular() && len(found) < maxProjectEnvFiles {
				found = append(found, m)
			}
		}
	}
	return found, nil
}

// createTargets are new nonce-named paths in each place the rules deny
// writing: every protected root, /Library/Freeside and the directories under
// it, LaunchAgents, the global preferences, and the verification jobs'
// temporary and cache directories. A target's control is that the account
// may write its parent; a place the account can't write is already denied
// to every process it runs and is left out. A protected root that doesn't
// exist yet is probed by creating the root itself.
func (s *Suite) createTargets(nonce string) ([]probeCreate, error) {
	cfg := s.b.cfg
	name := "freeside-probe-" + nonce
	var places []string
	places = append(places, cfg.ProtectedRoots...)
	places = append(places, "/Library/Freeside")
	for _, pattern := range []string{"/Library/Freeside/*", "/Library/Freeside/*/*"} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if info, err := os.Lstat(m); err == nil && info.IsDir() {
				places = append(places, m)
			}
		}
	}
	// A protected root may be /Library/Freeside or one of its directories.
	slices.Sort(places)
	places = slices.Compact(places)
	var creates []probeCreate
	for _, place := range places {
		info, err := os.Lstat(place)
		switch {
		case err == nil && info.IsDir():
			creates = appendWritable(creates, place, probeCreate{Path: filepath.Join(place, name)})
		case errors.Is(err, fs.ErrNotExist):
			creates = appendWritable(creates, filepath.Dir(place), probeCreate{Path: place, Dir: true})
		}
	}
	agents := cfg.Home + "/Library/LaunchAgents"
	if _, err := os.Lstat(agents); errors.Is(err, fs.ErrNotExist) {
		creates = appendWritable(creates, filepath.Dir(agents), probeCreate{Path: agents, Dir: true})
	} else {
		creates = appendWritable(creates, agents, probeCreate{Path: filepath.Join(agents, probeLabelPrefix+nonce+".plist")})
	}
	prefs := cfg.Home + "/Library/Preferences"
	creates = appendWritable(creates, prefs, probeCreate{Path: filepath.Join(prefs, ".GlobalPreferences."+name)})
	for _, dir := range []string{cfg.UserTemp, cfg.UserCache} {
		creates = appendWritable(creates, dir, probeCreate{Path: filepath.Join(dir, "freeside-verify-"+name), Dir: true})
	}
	if len(creates) == 0 {
		return nil, failure("protected writes", "control: the account can write none of the protected places")
	}
	return creates, nil
}

func appendWritable(creates []probeCreate, parent string, c probeCreate) []probeCreate {
	if unix.Access(parent, unix.W_OK) != nil {
		return creates
	}
	if _, err := os.Lstat(c.Path); err == nil {
		return creates
	}
	return append(creates, c)
}

// portTargets are the reserved ports the daemon can bind outside the
// launch. A port a verification job holds right now is left out; when every
// reserved port is held, the control fails.
func (s *Suite) portTargets() ([]int, error) {
	var ports []int
	for _, port := range s.b.cfg.ReservedPorts {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if errors.Is(err, syscall.EADDRINUSE) {
			continue
		}
		if err != nil {
			return nil, failure(checkBind(port), "control: %v", err)
		}
		_ = ln.Close()
		ports = append(ports, port)
	}
	if len(ports) == 0 && len(s.b.cfg.ReservedPorts) > 0 {
		return nil, failure("reserved ports", "control: every reserved port is in use")
	}
	return ports, nil
}

func serviceTargets() ([]probeService, error) {
	if kr := lookUpService(controlService); kr != 0 {
		return nil, failure(checkLookup(controlService), "control: the daemon's own lookup failed with %d", kr)
	}
	services := make([]probeService, 0, len(deniedServices))
	for _, name := range deniedServices {
		services = append(services, probeService{Name: name, Registered: lookUpService(name) == 0})
	}
	return services, nil
}

func (s *Suite) prejob(ctx context.Context) error {
	nonce := newNonce()
	t, err := s.commonTargets(nonce)
	if err != nil {
		return err
	}
	defer t.close()
	return s.probeLaunch(ctx, nonce, func(layout) (probeSpec, error) { return t.spec, nil }, nil)
}

func (s *Suite) full(ctx context.Context) error {
	nonce := newNonce()
	t, err := s.commonTargets(nonce)
	if err != nil {
		return err
	}
	defer t.close()
	if err := s.socketTarget(t, nonce); err != nil {
		return err
	}
	if err := s.standInTarget(t); err != nil {
		return err
	}
	label := probeLabelPrefix + strings.ToLower(nonce)
	var stand launchStandIns
	return s.probeLaunch(ctx, nonce, func(l layout) (probeSpec, error) {
		spec := t.spec
		spec.Detach = true
		var err error
		stand, err = s.launchTargets(ctx, t, l, label)
		if err != nil {
			return probeSpec{}, err
		}
		spec.Commands = stand.commands
		return spec, nil
	}, func(detached *probeStandIn) error {
		if detached == nil {
			return failure(checkDetached, "the probe named no detached process")
		}
		if alive(*detached) {
			_ = syscall.Kill(detached.Pid, syscall.SIGKILL)
			return failure(checkDetached, "process %d outlived the launch", detached.Pid)
		}
		return s.effects(ctx, stand)
	})
}

// launchStandIns are Full's launchd, Launch Services, and Simulator
// stand-ins, and the commands the probe aims at them.
type launchStandIns struct {
	job, submitted, device, opened string
	jobPid                         int
	commands                       []probeCommand
}

// alive reports whether the process d names still runs: its pid is live
// and its start time is d's.
func alive(d probeStandIn) bool {
	start, err := processStart(d.Pid)
	return err == nil && start == d.Start
}

// maxSocketPath is the size of sockaddr_un's sun_path on macOS, which
// holds the path and its NUL.
const maxSocketPath = 104

// socketTarget serves a Unix socket in the runs root, which a protected
// root holds; its control is a connection from outside the launch.
func (s *Suite) socketTarget(t *targets, nonce string) error {
	path := filepath.Join(s.b.cfg.RunsRoot, "p"+nonce[:10]+".sock")
	if len(path) >= maxSocketPath {
		return failure(checkConnect(path), "control: the runs root is too deep for a Unix socket's %d-byte path", maxSocketPath)
	}
	if err := os.MkdirAll(s.b.cfg.RunsRoot, 0o700); err != nil {
		return failure(checkConnect(path), "control: %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return failure(checkConnect(path), "control: serve: %v", err)
	}
	t.cleanup = append(t.cleanup, func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	conn, err := net.Dial("unix", path)
	if err != nil {
		return failure(checkConnect(path), "control: %v", err)
	}
	_ = conn.Close()
	t.spec.Sockets = []string{path}
	return nil
}

// standInTarget starts the process the probe tries to signal: the suite's
// own child, which keeps its pid until the suite reaps it.
func (s *Suite) standInTarget(t *targets) error {
	cmd := newSleeper()
	if err := cmd.Start(); err != nil {
		return failure("signal", "control: start the stand-in: %v", err)
	}
	t.cleanup = append(t.cleanup, func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := cmd.Process.Pid
	if err := syscall.Kill(pid, 0); err != nil {
		return failure(checkSignal(pid), "control: %v", err)
	}
	start, err := processStart(pid)
	if err != nil {
		return failure(checkSignal(pid), "control: %v", err)
	}
	t.spec.StandIn = &probeStandIn{Pid: pid, Start: start}
	return nil
}

// launchTargets sets up, once the probe launch's directory exists, the
// launchd and Launch Services stand-ins and the commands aimed at them: a
// job of the suite's own under probeLabelPrefix, which the probe tries to
// kickstart, stop, and remove; a label under it the probe tries to submit;
// an app bundle in the launch's run directory that leaves a marker when it
// runs; and, when simctl runs outside the launch, a device it tries to
// create. A Simulator that can't run outside the launch is unreachable
// inside it, which the service lookups prove.
func (s *Suite) launchTargets(ctx context.Context, t *targets, l layout, label string) (launchStandIns, error) {
	gui := s.outside.guiDomain()
	stand := launchStandIns{
		job: gui + "/" + label, submitted: gui + "/" + label + ".submit", device: label + ".device",
		opened: filepath.Join(l.ctl, "opened"),
	}
	plist := filepath.Join(l.ctl, label+".plist")
	if err := os.WriteFile(plist, standInPlist(label), 0o600); err != nil {
		return stand, err
	}
	// The stand-in job, a submitted job, and a created device are removed
	// whatever the outcome: a bootstrap that errors may still have loaded
	// the job, the other two exist only when the rules let the probe
	// through, and the pass may stop before effects looks. A submitted job
	// is kept alive by launchd, so it would otherwise rerun outside the
	// cordon.
	t.cleanup = append(t.cleanup,
		func() { _ = s.outside.stopJob(context.WithoutCancel(ctx), stand.job) },
		func() { _ = s.outside.stopJob(context.WithoutCancel(ctx), stand.submitted) })
	if err := s.outside.startJob(ctx, plist); err != nil {
		return stand, failure("launchd stand-in", "control: %v", err)
	}
	pid, err := s.awaitJobPid(ctx, stand.job, 0)
	if err != nil {
		return stand, err
	}
	// The control for the probe's kickstart, kill, and bootout: the
	// account itself can kill and restart its stand-in, so a refusal
	// inside the launch is the sandbox's.
	if err := s.outside.kickstartJob(ctx, stand.job); err != nil {
		return stand, failure("launchd stand-in", "control: %v", err)
	}
	if stand.jobPid, err = s.awaitJobPid(ctx, stand.job, pid); err != nil {
		return stand, err
	}
	bundle := filepath.Join(l.runRoot, "Probe.app")
	if err := writeStandInApp(bundle, label, stand.opened); err != nil {
		return stand, err
	}
	stand.commands = []probeCommand{
		{Name: "launchctl submit", Argv: []string{"/bin/launchctl", "submit", "-l", label + ".submit", "--", "/usr/bin/true"}},
		{Name: "launchctl kickstart", Argv: []string{"/bin/launchctl", "kickstart", "-k", stand.job}},
		{Name: "launchctl kill", Argv: []string{"/bin/launchctl", "kill", "TERM", stand.job}},
		{Name: "launchctl bootout", Argv: []string{"/bin/launchctl", "bootout", stand.job}},
		{Name: "open", Argv: []string{"/usr/bin/open", "-W", "-g", "-j", bundle}},
	}
	deviceType, err := s.outside.simDeviceType(ctx)
	if err != nil {
		return stand, failure("simctl", "control: %v", err)
	}
	if deviceType != "" {
		t.cleanup = append(t.cleanup, func() { _ = s.outside.simDelete(context.WithoutCancel(ctx), stand.device) })
		stand.commands = append(stand.commands, probeCommand{
			Name: "simctl create", Argv: []string{"/usr/bin/xcrun", "simctl", "create", stand.device, deviceType},
		})
	}
	return stand, nil
}

// jobSpawnTimeout bounds the wait for launchd to run the stand-in job after
// a bootstrap or kickstart, which it may only schedule before returning.
const jobSpawnTimeout = 5 * time.Second

// awaitJobPid waits for the stand-in job at target to run as a pid other
// than previous, and fails the launchd control if it doesn't in time.
func (s *Suite) awaitJobPid(ctx context.Context, target string, previous int) (int, error) {
	deadline := time.Now().Add(jobSpawnTimeout)
	for {
		pid, loaded, err := s.outside.job(ctx, target)
		if err == nil && loaded && pid > 1 && pid != previous {
			return pid, nil
		}
		if err != nil || !loaded || time.Now().After(deadline) || ctx.Err() != nil {
			return 0, failure("launchd stand-in", "control: the stand-in job does not run anew (loaded %v, pid %d, was %d): %v",
				loaded, pid, previous, errors.Join(err, ctx.Err()))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// effects checks, after the launch ended, that nothing the probe tried took
// effect: the stand-in job runs on with its pid, no submitted job exists,
// the stand-in app never ran, and no device has the probe's name.
func (s *Suite) effects(ctx context.Context, stand launchStandIns) error {
	if pid, loaded, err := s.outside.job(ctx, stand.job); err != nil || !loaded || pid != stand.jobPid {
		return failure("launchctl stop", "the stand-in job ran as %d and is now loaded %v as %d: %v",
			stand.jobPid, loaded, pid, err)
	}
	if _, loaded, err := s.outside.job(ctx, stand.submitted); err != nil || loaded {
		return failure("launchctl submit", "the launch created a launchd job (loaded %v): %v", loaded, err)
	}
	if _, err := os.Lstat(stand.opened); !errors.Is(err, fs.ErrNotExist) {
		return failure("open", "the stand-in app ran: %v", err)
	}
	if n, err := s.outside.simDevices(ctx, stand.device); err != nil || n != 0 {
		return failure("simctl create", "%d devices named %s after the launch: %v", n, stand.device, err)
	}
	return nil
}

// probeLaunch runs the probe once, in a cordoned launch of its own under
// RunsRoot/conformance-<nonce>: it stages the probe copy in the launch's run
// directory, has build set up the spec once the canary is a member, checks
// the rule file the launch runs under, runs the probe, checks its report,
// and, with the launch ended, runs ended (nil for none) on the detached
// process the report named, before anything in the launch's directory is
// gone. It removes the launch's directory, the one path it deletes, only
// when this call claimed it and nothing from the launch can be alive: the
// run-end proof passed, or nothing but the canary, which close kills and
// reaps, ever started.
func (s *Suite) probeLaunch(ctx context.Context, nonce string, build func(layout) (probeSpec, error),
	ended func(*probeStandIn) error,
) (err error) {
	b := s.b
	runID := "conformance-" + nonce
	if !runIDPattern.MatchString(runID) {
		return fmt.Errorf("cordon: probe run id %q", runID)
	}
	launchDir := filepath.Join(b.cfg.RunsRoot, runID)
	// claimed is set once openLaunch's exclusive mkdir of launchDir has
	// succeeded, which is when it calls prepare.
	claimed := false
	defer func() {
		var blocked *BlockedError
		if !claimed || errors.As(err, &blocked) {
			return
		}
		if rmErr := os.RemoveAll(launchDir); rmErr != nil && err == nil {
			err = fmt.Errorf("cordon: remove the probe launch: %w", rmErr)
		}
	}()
	var probe string
	ln, err := b.openLaunch(ctx, runID, func(l layout) error {
		claimed = true
		probe = filepath.Join(l.runRoot, probeExecutable)
		return stageProbe(b.daemonPath, b.daemonDigest, probe)
	})
	if err != nil {
		return fmt.Errorf("cordon: open the probe launch: %w", err)
	}
	detached, err := s.runProbe(ctx, ln, probe, build)
	ln.close()
	if err != nil || ended == nil {
		return err
	}
	return ended(detached)
}

func (s *Suite) runProbe(ctx context.Context, ln *launch, probe string, build func(layout) (probeSpec, error)) (*probeStandIn, error) {
	spec, err := build(ln.l)
	if err != nil {
		return nil, err
	}
	rules, err := os.ReadFile(ln.l.rules)
	if err != nil || !bytes.Equal(rules, s.b.rules) {
		return nil, failure("rule file", "the probe launch's rule file is not the backend's: %v", err)
	}
	args := append(slices.Clone(probeVerb), spec.arg())
	// The probe gets the launch's environment, not the agent's: it
	// receives no token, so it names no token descriptor.
	exitCode, endedBy, err := ln.run(ctx, processSpec{
		argv: confinedArgv(s.b.cfg, ln.l, probe, args...),
		dir:  ln.l.runRoot,
		env:  launchEnv(s.b.cfg, ln.l),
	})
	if err != nil {
		return nil, err
	}
	if endedBy != EndedByExited {
		return nil, failure("probe report", "the probe launch ended %s", endedBy)
	}
	report, err := os.ReadFile(ln.l.transcript)
	if err != nil {
		return nil, fmt.Errorf("cordon: read the probe report: %w", err)
	}
	return checkProbeReport(spec, report, exitCode)
}

// stageProbe copies the daemon executable at src to dst, a new file, and
// refuses a copy whose digest is not want: the probe must be the binary the
// configuration digest names.
func stageProbe(src, want, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: the daemon's own executable
	if err != nil {
		return fmt.Errorf("stage probe: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700) //nolint:gosec // G302,G304: the probe launch runs its copy
	if err != nil {
		return fmt.Errorf("stage probe: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("stage probe: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("stage probe: %w", err)
	}
	digest, err := fileSHA256(dst)
	if err != nil {
		return fmt.Errorf("stage probe: %w", err)
	}
	if digest != want {
		return fmt.Errorf("stage probe: the daemon executable %s is %s on disk, but the daemon started as %s; restart the daemon",
			src, digest, want)
	}
	return nil
}

// newNonce names one pass's targets, so nothing planted in advance turns a
// refusal into EEXIST.
func newNonce() string { return rand.Text() }

// newSleeper is the signal probe's stand-in, bound by procbound so the
// suite's own exit can't leave it running.
func newSleeper() *exec.Cmd {
	cmd := exec.CommandContext(context.Background(), "/bin/sleep", "3600")
	procbound.Bind(cmd, 0)
	return cmd
}

// standInPlist is the stand-in job's plist. Its one-second throttle keeps
// the kickstart control from waiting out launchd's default ten.
func standInPlist(label string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>/bin/sleep</string><string>3600</string></array>
<key>RunAtLoad</key><true/>
<key>ThrottleInterval</key><integer>1</integer>
</dict></plist>
`, label))
}

// writeStandInApp writes an app bundle whose executable creates marker, so
// an open that Launch Services carried out leaves a trace.
func writeStandInApp(bundle, label, marker string) error {
	macOS := filepath.Join(bundle, "Contents", "MacOS")
	if err := os.MkdirAll(macOS, 0o700); err != nil {
		return err
	}
	info := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>%s.app</string>
<key>CFBundleExecutable</key><string>probe-app</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>LSUIElement</key><true/>
</dict></plist>
`, label)
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(info), 0o600); err != nil {
		return err
	}
	script := "#!/bin/sh\nexec /usr/bin/touch '" + strings.ReplaceAll(marker, "'", `'\''`) + "'\n"
	return os.WriteFile(filepath.Join(macOS, "probe-app"), []byte(script), 0o700) //nolint:gosec // G306: the stand-in app's executable
}
