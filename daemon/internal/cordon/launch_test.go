package cordon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// fakeHost is the kernel as the proof sees it: a set of live members, and
// the live supervisors, which aren't members.
type fakeHost struct {
	mu      sync.Mutex
	members map[int]bool
	// supervisors maps each live supervisor's pid to what SIGTERM does to
	// it.
	supervisors map[int]func()
	// killErr, when set, fails the kill of that pid.
	killErr map[int]error
	// terminated lists the pids sent SIGTERM.
	terminated []int
}

func newFakeHost() *fakeHost {
	return &fakeHost{members: map[int]bool{}, supervisors: map[int]func(){}, killErr: map[int]error{}}
}

func (h *fakeHost) live() ([]hostProcess, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	listed := []hostProcess{{pid: 1, settled: true}, {pid: os.Getpid(), settled: true}}
	// A fake process's start time is its pid, so same can check it.
	for pid := range h.members {
		listed = append(listed, hostProcess{pid: pid, start: int64(pid), settled: true})
	}
	for pid := range h.supervisors {
		listed = append(listed, hostProcess{pid: pid, start: int64(pid), settled: true})
	}
	slices.SortFunc(listed, func(a, b hostProcess) int { return a.pid - b.pid })
	return listed, nil
}

func (h *fakeHost) same(p hostProcess) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return (h.members[p.pid] || h.supervisors[p.pid] != nil) && p.start == int64(p.pid), nil
}

func (h *fakeHost) lookup(pid int) (hostProcess, bool, error) {
	if alive, _ := h.same(hostProcess{pid: pid, start: int64(pid)}); !alive {
		return hostProcess{}, false, nil
	}
	return hostProcess{pid: pid, start: int64(pid), settled: true}, true, nil
}

func (h *fakeHost) terminate(pid int) error {
	h.mu.Lock()
	h.terminated = append(h.terminated, pid)
	term := h.supervisors[pid]
	h.mu.Unlock()
	if term != nil {
		term()
	}
	return nil
}

func (h *fakeHost) setSupervisor(pid int, term func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if term == nil {
		delete(h.supervisors, pid)
	} else {
		h.supervisors[pid] = term
	}
}

func (h *fakeHost) sigterms() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.terminated)
}

func (h *fakeHost) member(pid int, _ proofTarget) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.members[pid], nil
}

func (h *fakeHost) kill(pid int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.killErr[pid]; err != nil {
		return err
	}
	delete(h.members, pid)
	return nil
}

func (h *fakeHost) set(pid int, alive bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if alive {
		h.members[pid] = true
	} else {
		delete(h.members, pid)
	}
}

func (h *fakeHost) alive() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	pids := slices.Collect(maps.Keys(h.members))
	slices.Sort(pids)
	return pids
}

// fakeProcess is a started process whose exit the test controls.
type fakeProcess struct {
	host     *fakeHost
	id       int
	exited   chan int
	once     sync.Once
	mu       sync.Mutex
	signals  []syscall.Signal
	obeyTerm bool
}

func (p *fakeProcess) pid() int { return p.id }

func (p *fakeProcess) exit(code int) {
	p.once.Do(func() {
		p.host.set(p.id, false)
		p.exited <- code
	})
}

func (p *fakeProcess) wait() (int, error) { return <-p.exited, nil }

func (p *fakeProcess) signalGroup(sig syscall.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	if sig == syscall.SIGKILL || (sig == syscall.SIGTERM && p.obeyTerm) {
		p.exit(-1)
	}
	return nil
}

func (p *fakeProcess) sent() []syscall.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.signals)
}

func (p *fakeProcess) closeIO(time.Duration) {}

// agentScript is what the fake agent does once started: it gets its spec
// and its process, and runs in its own goroutine.
type agentScript func(spec processSpec, p *fakeProcess)

// fakeLauncher starts a canary (pid 100) and an agent (pid 200) that leaves
// a detached child (pid 201), as a CLI's tool shell does.
type fakeLauncher struct {
	host   *fakeHost
	script agentScript
	specs  []processSpec
	// secrets holds what each started process could read from its token
	// pipe, nil for none.
	secrets [][]byte
	agent   *fakeProcess
	// canaryNeverJoins keeps the canary out of the member set.
	canaryNeverJoins bool
	// obeyTerm makes the agent exit on SIGTERM.
	obeyTerm bool
	// agentStartErr, when set, fails the agent's start.
	agentStartErr error
}

func (l *fakeLauncher) start(spec processSpec) (process, error) {
	// The supervisor closes the token's pipe once the process has started,
	// so keep what this one could read from it.
	var secret []byte
	if spec.secret != nil {
		var err error
		if secret, err = io.ReadAll(spec.secret); err != nil {
			return nil, err
		}
	}
	// A merged stderr shares stdout's pipe, as startExec does.
	if spec.mergeStderr {
		spec.stderr = spec.stdout
	}
	l.specs, l.secrets = append(l.specs, spec), append(l.secrets, secret)
	if slices.Contains(spec.argv, canaryProgram) {
		p := &fakeProcess{host: l.host, id: 100, exited: make(chan int, 1)}
		l.host.set(p.id, !l.canaryNeverJoins)
		return p, nil
	}
	if l.agentStartErr != nil {
		return nil, l.agentStartErr
	}
	p := &fakeProcess{host: l.host, id: 200, exited: make(chan int, 1), obeyTerm: l.obeyTerm}
	l.agent = p
	l.host.set(200, true)
	l.host.set(201, true)
	go l.script(spec, p)
	return p, nil
}

// fakeSupervisorPID is the pid an in-process supervisor claims as.
const fakeSupervisorPID = 300

// startSupervisor runs the supervisor's body in process, as
// fakeSupervisorPID on the host, with the launcher as its process starter:
// what RunSupervisor runs in its own process, given what b's newHost
// returns.
func (l *fakeLauncher) startSupervisor(b *Backend) func(supervisorSpec) (supervisorProcess, error) {
	return func(spec supervisorSpec) (supervisorProcess, error) {
		runsRoot, runID := spec.argv[len(spec.argv)-2], spec.argv[len(spec.argv)-1]
		ctx, cancel := context.WithCancel(context.Background())
		l.host.setSupervisor(fakeSupervisorPID, cancel)
		done := make(chan int, 1)
		go func() {
			defer func() { _ = spec.stderr.Close() }()
			env := superviseEnv{
				l: layoutFor(runsRoot, runID), runID: runID, start: l.start, secret: spec.secret,
				self: hostProcess{pid: fakeSupervisorPID, start: fakeSupervisorPID, settled: true},
			}
			var err error
			status := supervisorUnproven
			if env.host, err = b.newHost(); err == nil {
				status = env.supervise(ctx, spec.stderr)
			}
			l.host.setSupervisor(fakeSupervisorPID, nil)
			cancel()
			done <- status
		}()
		return fakeSupervisor(done), nil
	}
}

type fakeSupervisor chan int

func (s fakeSupervisor) wait() int { return <-s }

// useFakes makes b run on l: its host, its process starter, and an
// in-process supervisor.
func useFakes(b *Backend, l *fakeLauncher) {
	b.facts = func() (HostFacts, error) { return HostFacts{}, nil }
	b.newHost = func() (hostProcesses, error) { return l.host, nil }
	b.start = l.start
	b.startSupervisor = l.startSupervisor(b)
}

type fakeSeeder struct{ calls int }

func (s *fakeSeeder) FetchBaseWorktree(_ context.Context, _, _, _, dir string) error {
	s.calls++
	return os.Mkdir(dir, 0o700)
}

func fakeBackend(t *testing.T, cfg Config, l *fakeLauncher) *Backend {
	t.Helper()
	b, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	useFakes(b, l)
	return b
}

// fixtureToken is a setup token's shape, split so no tracked line matches
// the credential rules (scripts/check-credential-literals.sh).
const fixtureToken = "sk-ant-" + "oat01-cordon-fixture-0123456789abcdefghijklmnopqrstuvwxyz"

func request(id string) LaunchRequest {
	token, err := ward.NewSetupToken([]byte(fixtureToken))
	if err != nil {
		panic(err)
	}
	return LaunchRequest{
		RunID: id, Repository: "owner/repo", BaseRef: "main", BaseSHA: "0123456789abcdef0123456789abcdef01234567",
		Prompt: []byte("the prompt"), Args: []string{"-p"}, Token: token,
	}
}

// readFrame reads one line of a streamed launch's input, as the CLI does,
// and returns the line and its message's text.
func readFrame(r *bufio.Reader) (line []byte, text string, err error) {
	line, err = r.ReadBytes('\n')
	if err != nil {
		return line, "", err
	}
	var frame struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &frame); err != nil {
		return line, "", err
	}
	if frame.Type != "user" || frame.Message.Role != "user" {
		return line, "", fmt.Errorf("frame %q is not a user message", line)
	}
	return line, frame.Message.Content, nil
}

// firstMessage returns the text of a streamed launch's first message: the
// prompt.
func firstMessage(stdin io.Reader) string {
	_, text, _ := readFrame(bufio.NewReader(stdin))
	return text
}

func TestLaunchReturnsTheExitStatusOnceNoMemberIsAlive(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		l := &fakeLauncher{host: host, script: func(spec processSpec, p *fakeProcess) {
			_, _ = fmt.Fprintf(spec.stdout, "read %q\n", firstMessage(spec.stdin))
			_, _ = io.WriteString(spec.stderr, "a warning\n")
			p.exit(3)
		}}
		got, err := fakeBackend(t, cfg, l).Launch(t.Context(), request("run-1"))
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if got.ExitCode != 3 || got.EndedBy != EndedByExited {
			t.Fatalf("result = %+v, want exit 3 ended by exited", got)
		}
		if alive := host.alive(); len(alive) != 0 {
			t.Fatalf("members still alive after the launch: %v", alive)
		}
		transcript, err := os.ReadFile(got.TranscriptPath)
		if err != nil || string(transcript) != "read \"the prompt\"\n" {
			t.Fatalf("transcript = %q, %v", transcript, err)
		}
		stderr, err := os.ReadFile(got.StderrPath)
		if err != nil || string(stderr) != "a warning\n" {
			t.Fatalf("stderr = %q, %v", stderr, err)
		}
		agent := l.specs[1]
		if agent.dir != filepath.Join(cfg.RunsRoot, "run-1", "run", "worktree") {
			t.Fatalf("agent dir = %s", agent.dir)
		}
		want := append(confinedArgv(cfg, layoutFor(cfg.RunsRoot, "run-1"), cfg.AgentCLI),
			"--settings",
			`{"sandbox":{"enabled":false},"attribution":{"commit":"","pr":""},"enabledPlugins":{"agents-md@builtin":false}}`,
			"--input-format", "stream-json",
			"-p")
		if !slices.Equal(agent.argv, want) {
			t.Fatalf("agent argv = %q, want %q", agent.argv, want)
		}
	})
}

// TestLaunchEnvironmentIsTheFixedList shows the daemon's own environment
// never reaches a launch: the canary gets the fixed list, and the agent the
// fixed list plus the name of its token's descriptor. The PATH is the
// launch PATH, spelled out so a change to the list fails here. Each process
// starts sandbox-exec by absolute path, and sandbox-exec starts the canary
// and the agent by absolute path, so neither resolves through that PATH.
func TestLaunchEnvironmentIsTheFixedList(t *testing.T) {
	t.Setenv("FREESIDE_CORDON_DAEMON_ONLY", "leak")
	cfg := testConfig(t)
	cfg.Path = LaunchPath(filepath.Dir(cfg.AgentCLI), cfg.Home)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
		if _, err := fakeBackend(t, cfg, l).Launch(t.Context(), request("run-env")); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		lay := layoutFor(cfg.RunsRoot, "run-env")
		path := "PATH=" + filepath.Dir(cfg.AgentCLI) + ":/opt/homebrew/bin:/opt/homebrew/opt/rustup/bin:" +
			cfg.Home + "/.cargo/bin:" + cfg.Home + "/.local/bin:" + cfg.Home + "/.local/share/mise/shims:" +
			"/usr/bin:/bin:/usr/sbin:/sbin"
		want := []string{
			"HOME=" + cfg.Home, path, "TMPDIR=" + lay.tmp, "CLAUDE_CONFIG_DIR=" + lay.config,
			"MISE_STATE_DIR=" + lay.miseState, "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		}
		if len(l.specs) != 2 {
			t.Fatalf("started %d processes, want the canary and the agent", len(l.specs))
		}
		if canary := l.specs[0]; !slices.Equal(canary.env, want) {
			t.Fatalf("canary env = %q, want %q", canary.env, want)
		}
		if agent := l.specs[1]; !slices.Equal(agent.env, append(want, "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3")) {
			t.Fatalf("agent env = %q, want the fixed list and the token's descriptor", agent.env)
		}
		for i, program := range []string{"/bin/cat", cfg.AgentCLI} {
			if got := confinedProgram(t, l.specs[i].argv); got != program {
				t.Errorf("process %d's argv %q confines %q, want %q", i, l.specs[i].argv, got, program)
			}
		}
	})
}

// confinedProgram returns the program argv has sandbox-exec start, after
// the rule file, its parameters, and the "--" that ends them, and fails the
// test unless argv starts sandbox-exec by absolute path and ends its
// options there.
func confinedProgram(t *testing.T, argv []string) string {
	t.Helper()
	if len(argv) < 4 || argv[0] != "/usr/bin/sandbox-exec" || argv[1] != "-f" {
		t.Fatalf("argv %q doesn't start sandbox-exec by absolute path with a rule file", argv)
	}
	i := 3
	for i+1 < len(argv) && argv[i] == "-D" {
		i += 2
	}
	if i+1 >= len(argv) || argv[i] != "--" {
		t.Fatalf("argv %q doesn't end sandbox-exec's options with --", argv)
	}
	return argv[i+1]
}

// TestLaunchHandsTheTokenToTheAgentAlone shows the token reaches the agent
// on its own pipe and nowhere else a spec carries: not the canary, not an
// argument, not the environment, and not the prompt's stdin.
func TestLaunchHandsTheTokenToTheAgentAlone(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		var stdin []byte
		l := &fakeLauncher{host: newFakeHost(), script: func(spec processSpec, p *fakeProcess) {
			stdin, _, _ = readFrame(bufio.NewReader(spec.stdin))
			p.exit(0)
		}}
		if _, err := fakeBackend(t, cfg, l).Launch(t.Context(), request("run-token")); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		canary := l.specs[0]
		if canary.secret != nil {
			t.Fatal("the canary was given a secret")
		}
		if string(l.secrets[1]) != fixtureToken {
			t.Fatalf("the agent's secret is %d bytes, not the request's token", len(l.secrets[1]))
		}
		if want := `{"type":"user","message":{"role":"user","content":"the prompt"}}` + "\n"; string(stdin) != want {
			t.Fatalf("the agent's stdin = %q, want the prompt alone, as %q", stdin, want)
		}
		for _, spec := range l.specs {
			for _, field := range slices.Concat(spec.argv, spec.env, []string{spec.dir}) {
				if strings.Contains(field, fixtureToken) {
					t.Fatal("a process's argv, environment, or directory holds the token")
				}
			}
		}
	})
}

// TestLaunchRefusesAMissingTokenBeforeCreatingAnything covers a request
// with the zero token: nothing is claimed, seeded, or started.
func TestLaunchRefusesAMissingTokenBeforeCreatingAnything(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost()}
		seeder := &fakeSeeder{}
		b, err := New(cfg, seeder)
		if err != nil {
			t.Fatal(err)
		}
		useFakes(b, l)
		req := request("run-notoken")
		req.Token = ward.SetupToken{}
		if _, err := b.Launch(t.Context(), req); err == nil {
			t.Fatal("Launch without a token succeeded")
		}
		if len(l.specs) != 0 || seeder.calls != 0 {
			t.Fatalf("started %d processes and seeded %d times", len(l.specs), seeder.calls)
		}
		if _, err := os.Stat(filepath.Join(cfg.RunsRoot, "run-notoken")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the launch directory exists: %v", err)
		}
	})
}

// TestLaunchRequestRendersNoToken covers a request or a launch error that
// reaches a log line: neither carries the token.
func TestLaunchRequestRendersNoToken(t *testing.T) {
	t.Parallel()
	req := request("run-render")
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
		if got := fmt.Sprintf(verb, req); strings.Contains(got, fixtureToken) || strings.Contains(got, fmt.Sprintf("%x", fixtureToken)) {
			t.Errorf("%s of a LaunchRequest renders the token", verb)
		}
	}
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), canaryNeverJoins: true}
		_, err := fakeBackend(t, cfg, l).Launch(t.Context(), req)
		if err == nil || strings.Contains(fmt.Sprintf("%+v", err), fixtureToken) {
			t.Fatalf("Launch = %v, want an error that holds no token", err)
		}
	})
}

func TestLaunchCancellationEndsEveryMember(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		ctx, cancel := context.WithCancel(t.Context())
		l := &fakeLauncher{host: host, script: func(processSpec, *fakeProcess) { cancel() }}
		got, err := fakeBackend(t, cfg, l).Launch(ctx, request("run-cancel"))
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if got.EndedBy != EndedByCanceled {
			t.Fatalf("EndedBy = %s, want canceled", got.EndedBy)
		}
		// The fake ignores SIGTERM, so EndGrace passes before SIGKILL.
		if sent := l.agent.sent(); !slices.Equal(sent, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
			t.Fatalf("signals = %v, want SIGTERM then SIGKILL", sent)
		}
		if alive := host.alive(); len(alive) != 0 {
			t.Fatalf("members still alive: %v", alive)
		}
	})
}

func TestLaunchStallLimitEndsASilentAgent(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		l := &fakeLauncher{host: host, obeyTerm: true, script: func(spec processSpec, _ *fakeProcess) {
			// Output on either stream more often than the limit keeps the
			// launch alive.
			for _, stream := range []io.Writer{spec.stdout, spec.stderr, spec.stdout} {
				time.Sleep(cfg.StallLimit / 2)
				_, _ = io.WriteString(stream, "tick\n")
			}
		}}
		start := time.Now()
		got, err := fakeBackend(t, cfg, l).Launch(t.Context(), request("run-stall"))
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if got.EndedBy != EndedByStalled {
			t.Fatalf("EndedBy = %s, want stalled", got.EndedBy)
		}
		// The last tick is at 1.5 limits, so the stall is due at 2.5.
		if elapsed := time.Since(start); elapsed < cfg.StallLimit*5/2 {
			t.Fatalf("stalled after %v, before the limit passed silent", elapsed)
		}
		if sent := l.agent.sent(); !slices.Equal(sent, []syscall.Signal{syscall.SIGTERM}) {
			t.Fatalf("signals = %v, want SIGTERM alone", sent)
		}
		if alive := host.alive(); len(alive) != 0 {
			t.Fatalf("members still alive: %v", alive)
		}
	})
}

// TestLaunchBlocksWhenItsEndIsUnproven has a member the proof can't kill,
// after the agent ran and when it never started.
func TestLaunchBlocksWhenItsEndIsUnproven(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		unkilled int
		startErr error
	}{
		{"agent ran", 201, nil},
		{"agent never started", 100, errors.New("exec failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			synctest.Test(t, func(t *testing.T) {
				host := newFakeHost()
				host.killErr[tc.unkilled] = syscall.EPERM
				l := &fakeLauncher{host: host, agentStartErr: tc.startErr, script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
				_, err := fakeBackend(t, cfg, l).Launch(t.Context(), request("run-blocked"))
				var blocked *BlockedError
				if !errors.As(err, &blocked) || !errors.Is(err, ErrRunEndUnproven) || blocked.EndedBy != EndedByExited {
					t.Fatalf("Launch = %v, want a BlockedError wrapping ErrRunEndUnproven", err)
				}
			})
		})
	}
}

// TestLaunchRetriesAFailedCancellation fails the first, or every,
// cancellation request: Launch keeps waiting rather than return with the
// run alive, and reports a cancellation that never took once the run ends.
func TestLaunchRetriesAFailedCancellation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		failures int // -1 fails every request
		took     bool
	}{
		{"retried", 1, true},
		{"never took", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			cfg.StallLimit = 5 * time.Second // bounds the retries a failing row makes
			synctest.Test(t, func(t *testing.T) {
				host := newFakeHost()
				ctx, cancel := context.WithCancel(t.Context())
				var mu sync.Mutex
				failures := 0
				l := &fakeLauncher{host: host, obeyTerm: true, script: func(processSpec, *fakeProcess) {
					mu.Lock()
					failures = tc.failures
					mu.Unlock()
					cancel()
				}}
				b := fakeBackend(t, cfg, l)
				b.newHost = func() (hostProcesses, error) {
					mu.Lock()
					defer mu.Unlock()
					if failures != 0 {
						failures--
						return nil, errors.New("sysctl failed")
					}
					return host, nil
				}
				got, err := b.Launch(ctx, request("run-retry"))
				ended, readErr := readEndRecord(layoutFor(cfg.RunsRoot, "run-retry"), "run-retry")
				if readErr != nil {
					t.Fatalf("end record: %v", readErr)
				}
				if signaled := slices.Contains(host.sigterms(), fakeSupervisorPID); signaled != tc.took {
					t.Fatalf("supervisor signaled: %v, want %v", signaled, tc.took)
				}
				if tc.took {
					if err != nil || got.EndedBy != EndedByCanceled {
						t.Fatalf("Launch = %+v, %v; want canceled", got, err)
					}
				} else if err == nil || ended.EndedBy != EndedByCanceled {
					// The marker was written, so the end reads as canceled,
					// but the supervisor never heard and the stall limit
					// ended the agent.
					t.Fatalf("Launch = %+v, %v, end %+v; want the cancellation's failure", got, err, ended)
				}
				if alive := host.alive(); len(alive) != 0 {
					t.Fatalf("members alive after Launch returned: %v", alive)
				}
			})
		})
	}
}

func TestLaunchRefusesBeforeStartingWhenNoProofIsPossible(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost()}
		b := fakeBackend(t, cfg, l)
		b.newHost = func() (hostProcesses, error) { return nil, ErrRunEndUnproven }
		if _, err := b.Launch(t.Context(), request("run-nohost")); !errors.Is(err, ErrRunEndUnproven) {
			t.Fatalf("Launch = %v, want ErrRunEndUnproven", err)
		}
		if len(l.specs) != 0 {
			t.Fatalf("started %d processes", len(l.specs))
		}
	})
}

// TestLaunchNeverStartsTheAgentWithoutAMemberCanary covers a query that
// answers "not a member" for everything: the proof could never find the
// launch's processes, so the agent doesn't start.
func TestLaunchNeverStartsTheAgentWithoutAMemberCanary(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), canaryNeverJoins: true}
		if _, err := fakeBackend(t, cfg, l).Launch(t.Context(), request("run-canary")); !errors.Is(err, ErrRunEndUnproven) {
			t.Fatalf("Launch = %v, want ErrRunEndUnproven", err)
		}
		if len(l.specs) != 1 {
			t.Fatalf("started %d processes, want the canary alone", len(l.specs))
		}
	})
}

func TestLaunchRefusesAClaimedOrMalformedRunID(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
		b := fakeBackend(t, cfg, l)
		if _, err := b.Launch(t.Context(), request("run-twice")); err != nil {
			t.Fatalf("first Launch: %v", err)
		}
		for _, id := range []string{"run-twice", "../escape", "a/b", "", ".."} {
			if _, err := b.Launch(t.Context(), request(id)); err == nil {
				t.Fatalf("Launch(%q) succeeded", id)
			}
		}
	})
}

// TestLaunchRefusesACallerCLISettings shows a launch's caller can't set the
// agent CLI's settings, which would turn its own sandbox back on or
// replace the fixed settings, or its input format, which the supervisor
// writes, nor hand it a prompt that isn't UTF-8.
func TestLaunchRefusesACallerCLISettings(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
		b := fakeBackend(t, cfg, l)
		for i, args := range [][]string{
			{"--settings", "{}"},
			{"-p", `--settings={"sandbox":{"enabled":true}}`},
			{"--input-format", "text"},
			{"-p", "--input-format=text"},
		} {
			req := request(fmt.Sprintf("run-settings-%d", i))
			req.Args = args
			if _, err := b.Launch(t.Context(), req); err == nil {
				t.Fatalf("Launch with %q succeeded", args)
			}
		}
		req := request("run-settings-prompt")
		req.Prompt = []byte("not \xff UTF-8")
		if _, err := b.Launch(t.Context(), req); err == nil {
			t.Fatal("Launch with a prompt that isn't UTF-8 succeeded")
		}
		if len(l.specs) != 0 {
			t.Fatalf("a refused launch started %d processes", len(l.specs))
		}
	})
}

// TestLaunchesUnderOneConfigurationRunIdenticalRules shows the bytes a
// launch runs are the configuration's rendered rules, whatever the launch,
// a check's included.
func TestLaunchesUnderOneConfigurationRunIdenticalRules(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
		b := fakeBackend(t, cfg, l)
		if _, err := b.Check(t.Context(), checkRequest("rules-check")); err != nil {
			t.Fatalf("Check: %v", err)
		}
		for _, id := range []string{"rules-a", "rules-b", "rules-check"} {
			if id != "rules-check" {
				if _, err := b.Launch(t.Context(), request(id)); err != nil {
					t.Fatalf("Launch %s: %v", id, err)
				}
			}
			body, err := os.ReadFile(layoutFor(cfg.RunsRoot, id).rules)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, RenderRules(cfg)) {
				t.Fatalf("launch %s ran rules other than the configuration's", id)
			}
		}
	})
}

func TestEndLaunchRefusesAMissingTag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	control := filepath.Join(dir, "control")
	if err := os.WriteFile(control, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	host := newFakeHost()
	host.set(100, true)
	target := proofTarget{tag: filepath.Join(dir, "tag"), control: control, canary: 100}
	if err := endLaunch(t.Context(), host, target, time.Second); !errors.Is(err, ErrRunEndUnproven) {
		t.Fatalf("endLaunch = %v, want ErrRunEndUnproven", err)
	}
}

func TestEndedByRegistration(t *testing.T) {
	t.Parallel()
	for _, e := range AllEndedBy {
		if !e.valid() {
			t.Fatalf("%q is registered but invalid", e)
		}
	}
	if EndedBy("").valid() {
		t.Fatal("the zero EndedBy is valid")
	}
}

func TestBackendIdentityEntersTheConfigurationDigest(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	b, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != string(domain.BackendCordonedHostProcess) {
		t.Fatalf("Name = %s", b.Name())
	}
	digest := b.ConfigurationDigest()
	changes := map[string]func(*Config){
		"RunsRoot":          func(c *Config) { c.RunsRoot += "2" },
		"Home":              func(c *Config) { c.Home += "2" },
		"OwnerClaudeConfig": func(c *Config) { c.OwnerClaudeConfig += "2" },
		"Projects":          func(c *Config) { c.Projects += "2" },
		"ProtectedRoots":    func(c *Config) { c.ProtectedRoots = append(c.ProtectedRoots, c.ProtectedRoots[1]+"2") },
		"UserTemp":          func(c *Config) { c.UserTemp += "2" },
		"UserCache":         func(c *Config) { c.UserCache += "2" },
		"ReservedPorts":     func(c *Config) { c.ReservedPorts = append(c.ReservedPorts, 47102) },
		"AgentCLI":          func(c *Config) { c.AgentCLI += "2" },
		"AgentCLISHA256":    func(c *Config) { c.AgentCLISHA256 = strings.Repeat("cd", 32) },
		"Path":              func(c *Config) { c.Path = append(c.Path, "/sbin") },
		"StallLimit":        func(c *Config) { c.StallLimit++ },
		"EndGrace":          func(c *Config) { c.EndGrace++ },
		"EndProofTimeout":   func(c *Config) { c.EndProofTimeout++ },
	}
	for name, change := range changes {
		other := cfg.clone()
		change(&other)
		ob, err := New(other, &fakeSeeder{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ob.ConfigurationDigest() == digest {
			t.Errorf("changing %s left the configuration digest unchanged", name)
		}
	}
	rebuilt, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ConfigurationDigest() != digest {
		t.Fatal("two backends over one configuration and binary have different digests")
	}
	rebuilt.daemonDigest = strings.Repeat("0", 64)
	if rebuilt.ConfigurationDigest() == digest {
		t.Error("a different daemon executable left the configuration digest unchanged")
	}
	if (&Backend{}).ConfigurationDigest() != "" || len((&Backend{}).Capabilities()) != 0 {
		t.Fatal("the zero Backend declares something")
	}
}

// TestLaunchSeedsFromTheRequestsSeeder shows a launch that brings its own
// seeder seeds from it alone: the backend's own is never called.
func TestLaunchSeedsFromTheRequestsSeeder(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
		own := &fakeSeeder{}
		b, err := New(cfg, own)
		if err != nil {
			t.Fatal(err)
		}
		useFakes(b, l)
		req := request("run-seeded")
		brought := &fakeSeeder{}
		req.Seeder = brought
		if _, err := b.Launch(t.Context(), req); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if own.calls != 0 || brought.calls != 1 {
			t.Fatalf("the backend's seeder ran %d times and the request's %d, want 0 and 1", own.calls, brought.calls)
		}
	})
}
