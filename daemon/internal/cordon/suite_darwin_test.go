//go:build darwin && cgo

package cordon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// fakeOutside stands in for the account's launchd and Simulator, so no test
// starts a job in the real GUI domain. Its stand-in job reads as running,
// with a new pid after each kickstart unless stuck; nothing else is
// loaded, and devices devices carry any name asked for.
type fakeOutside struct {
	mu      sync.Mutex
	started []string
	stopped []string
	kicked  int
	stuck   bool
	devices int
}

func (*fakeOutside) guiDomain() string { return "gui/501" }

func (o *fakeOutside) startJob(_ context.Context, plist string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.started = append(o.started, strings.TrimSuffix(filepath.Base(plist), ".plist"))
	return nil
}

func (o *fakeOutside) job(_ context.Context, target string) (int, bool, error) {
	if err := ownTarget(target); err != nil {
		return 0, false, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if slices.Contains(o.started, filepath.Base(target)) && !slices.Contains(o.stopped, target) {
		return 4242 + o.kicked, true, nil
	}
	return 0, false, nil
}

func (o *fakeOutside) kickstartJob(_ context.Context, target string) error {
	if err := ownTarget(target); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.stuck {
		o.kicked++
	}
	return nil
}

func (o *fakeOutside) stopJob(_ context.Context, target string) error {
	if err := ownTarget(target); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stopped = append(o.stopped, target)
	return nil
}

func (*fakeOutside) simDeviceType(context.Context) (string, error) { return "", nil }

func (o *fakeOutside) simDevices(context.Context, string) (int, error) { return o.devices, nil }

func (*fakeOutside) simDelete(context.Context, string) error { return nil }

// suiteFixture is a host fixture whose fake home holds a stand-in at a few
// secret paths, a second Claude configuration directory, and a project
// .env file, pinned to the test binary as the agent CLI, with reserved
// ports nothing holds.
func suiteFixture(t *testing.T) Config {
	t.Helper()
	cfg := newHostFixture(t).cfg
	// The suite serves its socket in the runs root, so the runs root sits
	// in a short protected root of its own (a Unix socket path is limited
	// to 104 bytes). The cleanup removes created, which nothing assigns
	// again.
	created, err := os.MkdirTemp("", "cdn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(created) })
	sockRoot, err := filepath.EvalSymlinks(created)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProtectedRoots = append(cfg.ProtectedRoots, sockRoot)
	slices.Sort(cfg.ProtectedRoots)
	cfg.RunsRoot = filepath.Join(sockRoot, "r")
	digest, err := fileSHA256(cfg.AgentCLI)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AgentCLISHA256 = digest
	for _, dir := range []string{
		"/.ssh", "/Library/Keychains", "/Library/Preferences", "/Library/Application Support/Freeside",
		promptClone, skillClone,
	} {
		if err := os.MkdirAll(cfg.Home+dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		cfg.Home + "/.ssh/id_stand_in", cfg.Home + "/.netrc", cfg.Home + loginKeychain,
		cfg.Home + fleetConfig + "/signing/key.pem",
		filepath.Join(cfg.Projects, "app", ".env"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("stand-in"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A second Claude configuration directory, which the rule file's
	// .claude prefix denies (#3600).
	for _, dir := range []string{cfg.UserTemp, cfg.UserCache, cfg.OwnerClaudeConfig, cfg.Home + "/.claude-bnw"} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ports := []int{freePort(t), freePort(t)}
	slices.Sort(ports)
	cfg.ReservedPorts = slices.Compact(ports)
	return cfg
}

// TestFullProvesEveryDenialInARealLaunch runs the whole suite through real
// cordoned launches and shows it declares the class, records the pass, and
// leaves nothing behind in the runs root; the pre-job probe then passes.
func TestFullProvesEveryDenialInARealLaunch(t *testing.T) {
	t.Parallel()
	cfg := suiteFixture(t)
	b, err := New(cfg, moduleSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeRecorder{}
	s := testSuite(t, b, WithConformanceRecorder(rec))
	outside := &fakeOutside{}
	s.outside = outside
	if err := s.Full(t.Context()); err != nil {
		t.Fatalf("Full: %v", err)
	}
	if !declares(b) {
		t.Fatal("a passed pass does not declare the class")
	}
	if got, want := rec.recorded(), []domain.ConformanceOutcome{domain.ConformanceSuperseded, domain.ConformancePassed}; !slices.Equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	// The stand-in job, and the submitted label whatever the outcome.
	if len(outside.started) != 1 || len(outside.stopped) != 2 || outside.kicked != 1 {
		t.Fatalf("stand-in jobs started %v, kickstarted %d times, stopped %v", outside.started, outside.kicked, outside.stopped)
	}
	if err := s.ProbeBeforeJob(t.Context()); err != nil {
		t.Fatalf("ProbeBeforeJob: %v", err)
	}
	if !declares(b) {
		t.Fatal("a passing pre-job probe withdrew the declaration")
	}
	left, err := os.ReadDir(cfg.RunsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("the runs root keeps %d entries after the suite", len(left))
	}
}

// TestFailedPreJobProbeWithdrawsTheDeclaration shows a pre-job probe whose
// report proves a check broken withdraws the declaration and records a
// failed pass. The fake launcher stands in for a launch whose rules
// let a read through.
func TestFailedPreJobProbeWithdrawsTheDeclaration(t *testing.T) {
	t.Parallel()
	if denied, _ := sandboxDeniesOperation("job-creation"); denied {
		t.Skip("this process's own sandbox denies what the suite's controls need")
	}
	cfg := testConfig(t)
	pinnedCLI(t, &cfg)
	for _, dir := range cfg.ProtectedRoots[:1] {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg.ReservedPorts = []int{freePort(t)}
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost(), script: func(spec processSpec, p *fakeProcess) {
			var probe probeSpec
			_ = json.Unmarshal([]byte(spec.argv[len(spec.argv)-1]), &probe)
			_, _ = spec.stdout.Write([]byte("FAIL " + checkRead(probe.Reads[0].Path) + ": opened\n"))
			p.exit(1)
		}}
		b := fakeBackend(t, cfg, l)
		rec := &fakeRecorder{}
		s := testSuite(t, b, WithConformanceRecorder(rec))
		generation, _ := b.beginConformanceProof(nil)
		if _, err := b.concludeConformanceProof(generation, true, nil); err != nil || !declares(b) {
			t.Fatalf("declare: %v", err)
		}
		var f *ConformanceFailure
		if err := s.ProbeBeforeJob(t.Context()); !errors.As(err, &f) || !strings.HasPrefix(f.Check, "read ") {
			t.Fatalf("ProbeBeforeJob = %v", err)
		}
		if declares(b) {
			t.Fatal("a failed pre-job probe left the declaration standing")
		}
		if got := rec.recorded(); !slices.Equal(got, []domain.ConformanceOutcome{domain.ConformanceFailed}) {
			t.Fatalf("recorded %v", got)
		}
	})
}

// TestPreJobProbeRefusesWhenAnotherPassRanMeanwhile shows a pre-job probe
// that passes still refuses the job when another pass began and declared
// while it ran: the class is declared again, but not by the pass the
// probe began under.
func TestPreJobProbeRefusesWhenAnotherPassRanMeanwhile(t *testing.T) {
	t.Parallel()
	if denied, _ := sandboxDeniesOperation("job-creation"); denied {
		t.Skip("this process's own sandbox denies what the suite's controls need")
	}
	cfg := testConfig(t)
	pinnedCLI(t, &cfg)
	for _, dir := range cfg.ProtectedRoots[:1] {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg.ReservedPorts = []int{freePort(t)}
	synctest.Test(t, func(t *testing.T) {
		var b *Backend
		l := &fakeLauncher{host: newFakeHost(), script: func(spec processSpec, p *fakeProcess) {
			generation, err := b.beginConformanceProof(nil)
			if err != nil {
				t.Errorf("begin a pass: %v", err)
			}
			if _, err := b.concludeConformanceProof(generation, true, nil); err != nil || !declares(b) {
				t.Errorf("declare again: %v", err)
			}
			passingProbe(spec, p)
		}}
		b = fakeBackend(t, cfg, l)
		rec := &fakeRecorder{}
		s := testSuite(t, b, WithConformanceRecorder(rec))
		generation, _ := b.beginConformanceProof(nil)
		if _, err := b.concludeConformanceProof(generation, true, nil); err != nil || !declares(b) {
			t.Fatalf("declare: %v", err)
		}
		var f *ConformanceFailure
		if err := s.ProbeBeforeJob(t.Context()); !errors.As(err, &f) || f.Check != "declaration" {
			t.Fatalf("ProbeBeforeJob = %v, want the declaration refused", err)
		}
		if got := rec.recorded(); len(got) != 0 {
			t.Fatalf("recorded %v, want nothing: the probe found nothing broken", got)
		}
	})
}

// TestEffectsFailOnEveryTrace shows each trace a probe command that got
// through leaves fails its own check, and no trace passes.
func TestEffectsFailOnEveryTrace(t *testing.T) {
	t.Parallel()
	const job = "gui/501/" + probeLabelPrefix + "x"
	cases := map[string]struct {
		change    func(o *fakeOutside, stand *launchStandIns)
		wantCheck string
	}{
		"no trace": {change: func(*fakeOutside, *launchStandIns) {}},
		"the app ran": {
			change: func(_ *fakeOutside, stand *launchStandIns) {
				if err := os.WriteFile(stand.opened, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantCheck: "open",
		},
		"the job restarted": {
			change:    func(_ *fakeOutside, stand *launchStandIns) { stand.jobPid = 4241 },
			wantCheck: "launchctl stop",
		},
		"the job is gone": {
			change:    func(o *fakeOutside, _ *launchStandIns) { o.stopped = append(o.stopped, job) },
			wantCheck: "launchctl stop",
		},
		"a job was submitted": {
			change:    func(o *fakeOutside, _ *launchStandIns) { o.started = append(o.started, probeLabelPrefix+"x.submit") },
			wantCheck: "launchctl submit",
		},
		"a device was created": {
			change:    func(o *fakeOutside, _ *launchStandIns) { o.devices = 1 },
			wantCheck: "simctl create",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, err := New(testConfig(t), &fakeSeeder{})
			if err != nil {
				t.Fatal(err)
			}
			s := testSuite(t, b)
			outside := &fakeOutside{started: []string{probeLabelPrefix + "x"}}
			s.outside = outside
			stand := launchStandIns{
				job: job, submitted: job + ".submit", device: probeLabelPrefix + "x.device",
				opened: filepath.Join(t.TempDir(), "opened"), jobPid: 4242,
			}
			c.change(outside, &stand)
			err = s.effects(t.Context(), stand)
			var f *ConformanceFailure
			switch {
			case c.wantCheck == "" && err != nil:
				t.Fatalf("effects = %v, want none", err)
			case c.wantCheck != "" && (!errors.As(err, &f) || f.Check != c.wantCheck):
				t.Fatalf("effects = %v, want a %q failure", err, c.wantCheck)
			}
		})
	}
}

// TestKickstartControlFailsWhenTheJobKeepsItsPid shows the stand-in job's
// control fails the pass when a kickstart outside the launch doesn't give
// it a new pid, so a refused kickstart inside proves nothing.
func TestKickstartControlFailsWhenTheJobKeepsItsPid(t *testing.T) {
	t.Parallel()
	b, err := New(testConfig(t), &fakeSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	s := testSuite(t, b)
	outside := &fakeOutside{stuck: true}
	s.outside = outside
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		var tg targets
		defer tg.close()
		_, err := s.launchTargets(t.Context(), &tg, layout{ctl: dir, runRoot: dir}, probeLabelPrefix+"x")
		var f *ConformanceFailure
		if !errors.As(err, &f) || f.Check != "launchd stand-in" {
			t.Fatalf("launchTargets = %v, want the launchd control failed", err)
		}
	})
	if len(outside.started) != 1 || len(outside.stopped) != 2 {
		t.Fatalf("a failed control left jobs: started %v, stopped %v", outside.started, outside.stopped)
	}
}
