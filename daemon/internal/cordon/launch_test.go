package cordon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// fakeHost is the kernel as the proof sees it: a set of live members.
type fakeHost struct {
	mu      sync.Mutex
	members map[int]bool
	// killErr, when set, fails the kill of that pid.
	killErr map[int]error
}

func newFakeHost() *fakeHost {
	return &fakeHost{members: map[int]bool{}, killErr: map[int]error{}}
}

func (h *fakeHost) live() ([]hostProcess, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	listed := []hostProcess{{pid: 1, settled: true}, {pid: os.Getpid(), settled: true}}
	// A fake process's start time is its pid, so same can check it.
	for pid := range h.members {
		listed = append(listed, hostProcess{pid: pid, start: int64(pid), settled: true})
	}
	slices.SortFunc(listed, func(a, b hostProcess) int { return a.pid - b.pid })
	return listed, nil
}

func (h *fakeHost) same(p hostProcess) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.members[p.pid] && p.start == int64(p.pid), nil
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
	agent  *fakeProcess
	// canaryNeverJoins keeps the canary out of the member set.
	canaryNeverJoins bool
	// obeyTerm makes the agent exit on SIGTERM.
	obeyTerm bool
}

func (l *fakeLauncher) start(spec processSpec) (process, error) {
	l.specs = append(l.specs, spec)
	if slices.Contains(spec.argv, canaryProgram) {
		p := &fakeProcess{host: l.host, id: 100, exited: make(chan int, 1)}
		l.host.set(p.id, !l.canaryNeverJoins)
		return p, nil
	}
	p := &fakeProcess{host: l.host, id: 200, exited: make(chan int, 1), obeyTerm: l.obeyTerm}
	l.agent = p
	l.host.set(200, true)
	l.host.set(201, true)
	go l.script(spec, p)
	return p, nil
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
	b.newHost = func() (hostProcesses, error) { return l.host, nil }
	b.start = l.start
	return b
}

func request(id string) LaunchRequest {
	return LaunchRequest{
		RunID: id, Repository: "owner/repo", BaseRef: "main", BaseSHA: "0123456789abcdef0123456789abcdef01234567",
		Prompt: []byte("the prompt"), Args: []string{"-p"},
	}
}

func TestLaunchReturnsTheExitStatusOnceNoMemberIsAlive(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		l := &fakeLauncher{host: host, script: func(spec processSpec, p *fakeProcess) {
			prompt, _ := io.ReadAll(spec.stdin)
			_, _ = fmt.Fprintf(spec.stdout, "read %q\n", prompt)
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
		if want := append(confinedArgv(cfg, layoutFor(cfg.RunsRoot, "run-1"), cfg.AgentCLI), "-p"); !slices.Equal(agent.argv, want) {
			t.Fatalf("agent argv = %q, want %q", agent.argv, want)
		}
	})
}

// TestLaunchEnvironmentIsTheFixedList shows the daemon's own environment
// never reaches a launch.
func TestLaunchEnvironmentIsTheFixedList(t *testing.T) {
	t.Setenv("FREESIDE_CORDON_DAEMON_ONLY", "leak")
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
		if _, err := fakeBackend(t, cfg, l).Launch(t.Context(), request("run-env")); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		lay := layoutFor(cfg.RunsRoot, "run-env")
		want := []string{
			"HOME=" + cfg.Home, "PATH=/usr/bin:/bin", "TMPDIR=" + lay.tmp, "CLAUDE_CONFIG_DIR=" + lay.config,
			"DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		}
		for _, spec := range l.specs {
			if !slices.Equal(spec.env, want) {
				t.Fatalf("env = %q, want %q", spec.env, want)
			}
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

func TestLaunchBlocksWhenItsEndIsUnproven(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		host.killErr[201] = syscall.EPERM
		l := &fakeLauncher{host: host, script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
		_, err := fakeBackend(t, cfg, l).Launch(t.Context(), request("run-blocked"))
		var blocked *BlockedError
		if !errors.As(err, &blocked) || !errors.Is(err, ErrRunEndUnproven) || blocked.EndedBy != EndedByExited {
			t.Fatalf("Launch = %v, want a BlockedError wrapping ErrRunEndUnproven", err)
		}
	})
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

// TestLaunchesUnderOneConfigurationRunIdenticalRules shows the bytes a
// launch runs are the configuration's rendered rules, whatever the launch.
func TestLaunchesUnderOneConfigurationRunIdenticalRules(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
		b := fakeBackend(t, cfg, l)
		for _, id := range []string{"rules-a", "rules-b"} {
			if _, err := b.Launch(t.Context(), request(id)); err != nil {
				t.Fatalf("Launch %s: %v", id, err)
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

func TestBackendDeclaresTheCordonedClass(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	b, err := New(cfg, &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != string(domain.BackendCordonedHostProcess) {
		t.Fatalf("Name = %s", b.Name())
	}
	if got := b.Capabilities().Snapshot(); !slices.Equal(got, domain.NewCapabilitySnapshot(domain.CordonedCapabilities()...)) {
		t.Fatalf("Capabilities = %v", got)
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
	if (&Backend{}).ConfigurationDigest() != "" || len((&Backend{}).Capabilities()) != 0 {
		t.Fatal("the zero Backend declares something")
	}
}
