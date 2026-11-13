package macverify

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// memRecorder is an in-memory ConformanceRecorder. failAt fails the call with
// that 1-based index.
type memRecorder struct {
	mu      sync.Mutex
	calls   int
	failAt  int
	records []domain.BackendConformance
}

func (r *memRecorder) RecordBackendConformance(_ context.Context, record domain.BackendConformance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls == r.failAt {
		return errors.New("disk full")
	}
	r.records = append(r.records, record)
	return nil
}

func (r *memRecorder) outcomes() []domain.ConformanceOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.ConformanceOutcome
	for _, rec := range r.records {
		out = append(out, rec.Outcome)
	}
	return out
}

// newTestSuite builds a suite over a fake-launcher room whose positive
// controls all pass: a local TCP listener and a local DNS echo stand in for
// the off-host targets.
func newTestSuite(t *testing.T, fake *fakeLauncher, slots ...string) (*Suite, *memRecorder) {
	t.Helper()
	room := newTestRoom(t, fake, slots...)
	rec := &memRecorder{}
	s, err := NewSuite(room.backend, SuiteFixture{}, WithConformanceRecorder(rec))
	if err != nil {
		t.Fatalf("NewSuite: %v", err)
	}
	s.room = room
	offHost, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = offHost.Close() })
	s.fx.OffHostTCP = offHost.Addr().String()
	s.fx.OffHostUDP = udpAnswerer(t)
	s.lookupHost = func(context.Context, string) ([]string, error) { return []string{"192.0.2.7"}, nil }
	s.controlTimeout = time.Second
	return s, rec
}

func probeMode(req runRequest) string { return req.Argv[len(probeCommand)] }

func TestFullProvesEverySlotAndDeclares(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	s, rec := newTestSuite(t, fake, "slot1", "slot2")
	if err := s.Full(t.Context()); err != nil {
		t.Fatalf("Full: %v", err)
	}
	if !s.b.proven.Load() {
		t.Fatal("a recorded passing pass did not declare")
	}
	if got := s.b.Capabilities().Has(exec.CapCredentialFreeHostVerification); got != hostSupported {
		t.Fatalf("Capabilities has the declaration = %v, want %v on this host", got, hostSupported)
	}
	if got := rec.outcomes(); !slices.Equal(got, []domain.ConformanceOutcome{domain.ConformanceSuperseded, domain.ConformancePassed}) {
		t.Fatalf("records = %v, want superseded then passed", got)
	}
	passed := rec.records[1]
	if passed.Backend != domain.BackendCredentialFreeHostVerification ||
		passed.ConfigurationDigest != s.b.ConfigurationDigest() ||
		!passed.Capabilities.Has(domain.CapCredentialFreeHostVerification) {
		t.Fatalf("passed record = %+v", passed)
	}
	if rec.records[0].Capabilities.Has(domain.CapCredentialFreeHostVerification) {
		t.Fatal("the superseding marker carries the capability")
	}

	_, prepares, runs := fake.snapshot()
	var got []string
	for i, req := range runs {
		got = append(got, req.Slot+" "+probeMode(req)+" "+string(prepares[i].profile))
		if prepares[i].workdir != fake.probeDir {
			t.Fatalf("job %d ran in %s, not the probe workspace", i, prepares[i].workdir)
		}
	}
	var want []string
	for _, slot := range []string{"slot1", "slot2"} {
		want = append(want,
			slot+" "+probeModeSandbox+" "+testProfile,
			slot+" "+probeModePacketFilter+" "+string(permissiveProfile),
			slot+" "+probeModeHomeMark+" "+testProfile,
			slot+" "+probeModeHomeClean+" "+testProfile,
		)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("jobs =\n%q\nwant\n%q", got, want)
	}
	sandbox, filter := specFromArgv(runs[0].Argv), specFromArgv(runs[1].Argv)
	// Each slot proves its own ports and that the next slot's first port
	// is refused.
	wantTail := append([]string{
		defaultResolverHost, s.fx.OffHostTCP, s.fx.OffHostUDP, "47000,47001,47002,47003", "47004",
	}, s.room.outsideDirs...)
	if !slices.Equal(sandbox.args[1:], wantTail) || !slices.Equal(filter.args, []string{s.fx.OffHostTCP, s.fx.OffHostUDP}) {
		t.Fatalf("targets: sandbox %q, packet filter %q", sandbox.args, filter.args)
	}
	if got := specFromArgv(runs[4].Argv).args[4:6]; !slices.Equal(got, []string{"47004,47005,47006,47007", "47000"}) {
		t.Fatalf("slot2's ports and foreign port = %q", got)
	}
	for _, slot := range []string{"slot1", "slot2"} {
		if err := s.room.pool.leaseNamed(slot); err != nil {
			t.Fatalf("slot %s not released: %v", slot, err)
		}
	}
}

// TestFullFailsEachConfinement fails one check of each confinement on the
// second slot: the pass stops there, records a failure, declares nothing,
// and names the confinement and slot.
func TestFullFailsEachConfinement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode, check string
		want        Confinement
	}{
		{probeModeSandbox, checkLoopbackRefused, ConfinementSandboxNetwork},
		{probeModeSandbox, checkResolverEmpty, ConfinementSandboxNetwork},
		{probeModeSandbox, checkTCPRefused, ConfinementSandboxNetwork},
		{probeModeSandbox, checkUDPRefused, ConfinementSandboxNetwork},
		{probeModeSandbox, checkWriteDenied("/private/tmp"), ConfinementWriteConfinement},
		{probeModeSandbox, checkWriteJobDir, ConfinementWriteConfinement},
		{probeModePacketFilter, checkTCPDropped, ConfinementPacketFilter},
		{probeModePacketFilter, checkUDPDropped, ConfinementPacketFilter},
		{probeModeHomeMark, checkHomeMarked, ConfinementHomeReset},
		{probeModeHomeClean, checkHomeReset, ConfinementHomeReset},
	} {
		t.Run(tc.check, func(t *testing.T) {
			t.Parallel()
			fake := newFakeLauncher()
			fake.probe = func(req runRequest, _ []byte) (string, int) {
				spec := specFromArgv(req.Argv)
				if req.Slot == "slot2" && probeMode(req) == tc.mode {
					return failingReport(spec, tc.check), 1
				}
				return passingReport(spec), 0
			}
			s, rec := newTestSuite(t, fake, "slot1", "slot2")
			err := s.Full(t.Context())
			var failure *ConformanceFailure
			if !errors.As(err, &failure) || failure.Confinement != tc.want || failure.Slot != "slot2" {
				t.Fatalf("Full = %v, want a %s failure on slot2", err, tc.want)
			}
			if !strings.Contains(err.Error(), string(tc.want)) || !strings.Contains(err.Error(), "slot2") {
				t.Fatalf("error %q does not name the confinement and slot", err)
			}
			if s.b.proven.Load() {
				t.Fatal("a failed pass declared")
			}
			if got := rec.outcomes(); !slices.Equal(got, []domain.ConformanceOutcome{domain.ConformanceSuperseded, domain.ConformanceFailed}) {
				t.Fatalf("records = %v, want superseded then failed", got)
			}
			_, _, runs := fake.snapshot()
			if last := runs[len(runs)-1]; last.Slot != "slot2" || probeMode(last) != tc.mode {
				t.Fatalf("the pass ran on past the failure: last job %s %s", last.Slot, probeMode(last))
			}
		})
	}
}

// TestFullFailsAFailedPositiveControl fails each control the daemon user
// runs: a slot's refusal of an unreachable target proves nothing, so the
// pass fails before any slot runs and names the confinement the control
// stands for.
func TestFullFailsAFailedPositiveControl(t *testing.T) {
	t.Parallel()
	unreachable := errors.New("network is unreachable")
	for name, tc := range map[string]struct {
		setup func(t *testing.T, s *Suite)
		want  Confinement
	}{
		"loopback listener unreachable": {
			setup: func(t *testing.T, s *Suite) {
				dial := s.dial
				s.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
					if network == "tcp" && addr != s.fx.OffHostTCP {
						return nil, unreachable
					}
					return dial(ctx, network, addr)
				}
			},
			want: ConfinementSandboxNetwork,
		},
		"resolver fails": {
			setup: func(t *testing.T, s *Suite) {
				s.lookupHost = func(context.Context, string) ([]string, error) { return nil, unreachable }
			},
			want: ConfinementSandboxNetwork,
		},
		"resolver answers nothing": {
			setup: func(t *testing.T, s *Suite) {
				s.lookupHost = func(context.Context, string) ([]string, error) { return nil, nil }
			},
			want: ConfinementSandboxNetwork,
		},
		"off-host TCP unreachable": {
			setup: func(t *testing.T, s *Suite) {
				dial := s.dial
				s.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
					if addr == s.fx.OffHostTCP {
						return nil, unreachable
					}
					return dial(ctx, network, addr)
				}
			},
			want: ConfinementPacketFilter,
		},
		"off-host DNS silent": {
			setup: func(t *testing.T, s *Suite) {
				silent, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = silent.Close() })
				s.fx.OffHostUDP = silent.LocalAddr().String()
				s.controlTimeout = 100 * time.Millisecond
			},
			want: ConfinementPacketFilter,
		},
		"no off-host nameserver": {
			setup: func(t *testing.T, s *Suite) {
				s.fx.OffHostUDP = ""
				s.resolvConf = func() ([]byte, error) { return []byte("nameserver 127.0.0.1\n"), nil }
			},
			want: ConfinementPacketFilter,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeLauncher()
			s, rec := newTestSuite(t, fake)
			tc.setup(t, s)
			err := s.Full(t.Context())
			var failure *ConformanceFailure
			if !errors.As(err, &failure) || failure.Confinement != tc.want || failure.Slot != "" {
				t.Fatalf("Full = %v, want a %s control failure", err, tc.want)
			}
			if !strings.Contains(err.Error(), "positive control") || !strings.Contains(err.Error(), string(tc.want)) {
				t.Fatalf("error %q does not name the control and confinement", err)
			}
			if calls, _, _ := fake.snapshot(); len(calls) != 0 {
				t.Fatalf("a slot ran after a failed control: %v", calls)
			}
			if s.b.proven.Load() {
				t.Fatal("a failed pass declared")
			}
			if got := rec.outcomes(); !slices.Equal(got, []domain.ConformanceOutcome{domain.ConformanceSuperseded, domain.ConformanceFailed}) {
				t.Fatalf("records = %v", got)
			}
		})
	}
}

func TestFullRefusesAnUnavailableSlotOrHost(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup func(t *testing.T, s *Suite)
		want  error
		text  string
	}{
		"leased slot": {
			setup: func(t *testing.T, s *Suite) {
				if err := s.room.pool.leaseNamed("slot2"); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrNoFreeSlot,
		},
		"quarantined slot": {
			setup: func(_ *testing.T, s *Suite) { s.room.pool.quarantine("slot2") },
			text:  "slot slot2 is quarantined",
		},
		"toolchain mismatch": {
			setup: func(t *testing.T, s *Suite) {
				s.room.hostToolchain = func(context.Context) (Toolchain, error) {
					return Toolchain{XcodeBuild: "17A1", Swift: testToolchain.Swift}, nil
				}
			},
			want: ErrToolchainMismatch,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeLauncher()
			s, rec := newTestSuite(t, fake, "slot1", "slot2")
			tc.setup(t, s)
			err := s.Full(t.Context())
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Full = %v, want %v %q", err, tc.want, tc.text)
			}
			if s.b.proven.Load() {
				t.Fatal("a refused pass declared")
			}
			if got := rec.outcomes(); !slices.Equal(got, []domain.ConformanceOutcome{domain.ConformanceSuperseded, domain.ConformanceFailed}) {
				t.Fatalf("records = %v", got)
			}
			for _, prepare := range func() []prepareCall { _, p, _ := fake.snapshot(); return p }() {
				if prepare.slot == "slot2" {
					t.Fatal("the pass ran a job on the unavailable slot")
				}
			}
		})
	}
}

func TestFullOffMacOSRecordsNothing(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	s, rec := newTestSuite(t, fake)
	s.room.hostSupported = false
	if err := s.Full(t.Context()); !errors.Is(err, ErrHostUnsupported) {
		t.Fatalf("Full = %v, want ErrHostUnsupported", err)
	}
	if len(rec.outcomes()) != 0 || s.b.proofGeneration != 0 {
		t.Fatalf("an unsupported host recorded %v at generation %d", rec.outcomes(), s.b.proofGeneration)
	}
}

func TestFullWithoutRecorderNeverDeclares(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	s, _ := newTestSuite(t, fake)
	s.recorder = nil
	if err := s.Full(t.Context()); err != nil {
		t.Fatalf("Full: %v", err)
	}
	if s.b.proven.Load() {
		t.Fatal("an unrecorded pass declared")
	}
}

// TestFullSupersededPublishesNothing starts a newer pass, or fails a
// pre-job probe, while a pass is running: the older pass records and
// declares nothing and says so.
func TestFullSupersededPublishesNothing(t *testing.T) {
	t.Parallel()
	for name, supersede := range map[string]func(b *Backend){
		"newer pass began":     func(b *Backend) { _, _ = b.beginConformanceProof(nil) },
		"pre-job probe failed": func(b *Backend) { b.withdrawConformance() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeLauncher()
			s, rec := newTestSuite(t, fake)
			var once sync.Once
			fake.probe = func(req runRequest, _ []byte) (string, int) {
				once.Do(func() { supersede(s.b) })
				return passingReport(specFromArgv(req.Argv)), 0
			}
			err := s.Full(t.Context())
			if err == nil || !strings.Contains(err.Error(), "superseded") {
				t.Fatalf("Full = %v, want a superseded error", err)
			}
			if s.b.proven.Load() {
				t.Fatal("a superseded pass declared")
			}
			if got := rec.outcomes(); !slices.Equal(got, []domain.ConformanceOutcome{domain.ConformanceSuperseded}) {
				t.Fatalf("records = %v, want only the pass's superseding marker", got)
			}
		})
	}
}

// TestFullWithdrawsTheDeclarationWhenItBegins: a declared backend loses its
// declaration, in memory and durably, the moment a newer pass begins.
func TestFullWithdrawsTheDeclarationWhenItBegins(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	s, rec := newTestSuite(t, fake)
	if err := s.Full(t.Context()); err != nil {
		t.Fatalf("first Full: %v", err)
	}
	var sawDeclared bool
	var sawLatest domain.ConformanceOutcome
	fake.probe = func(req runRequest, _ []byte) (string, int) {
		if !sawDeclared {
			sawDeclared = s.b.proven.Load()
			outcomes := rec.outcomes()
			sawLatest = outcomes[len(outcomes)-1]
		}
		return failingReport(specFromArgv(req.Argv), checkLoopbackRefused), 1
	}
	if err := s.Full(t.Context()); !errors.Is(err, ErrConformance) {
		t.Fatalf("second Full = %v, want a confinement failure", err)
	}
	if sawDeclared || sawLatest != domain.ConformanceSuperseded {
		t.Fatalf("during the second pass: declared %v, latest record %s", sawDeclared, sawLatest)
	}
	if s.b.proven.Load() {
		t.Fatal("the declaration survived a failed newer pass")
	}
}

func TestFullFailsWhenItsRecordFails(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		failAt int
		want   string
		jobs   bool
	}{
		"superseding marker": {failAt: 1, want: "record superseding conformance marker"},
		"passed record":      {failAt: 2, want: "record backend conformance", jobs: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeLauncher()
			s, rec := newTestSuite(t, fake)
			rec.failAt = tc.failAt
			if err := s.Full(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Full = %v, want %q", err, tc.want)
			}
			if s.b.proven.Load() {
				t.Fatal("a pass whose record failed declared")
			}
			if calls, _, _ := fake.snapshot(); (len(calls) != 0) != tc.jobs {
				t.Fatalf("jobs ran = %v, want %v", len(calls) != 0, tc.jobs)
			}
		})
	}
}

func TestNewSuiteRefusesABadFixture(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	for _, fx := range []SuiteFixture{
		{ResolverHost: "github.com; rm"},
		{ResolverHost: "-github.com"},
		{OffHostTCP: "github.com:443"},
		{OffHostTCP: "192.0.2.1"},
		{OffHostUDP: "[::1]"},
	} {
		if _, err := NewSuite(room.backend, fx); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("NewSuite(%+v) = %v, want ErrInvalidConfig", fx, err)
		}
	}
	s, err := NewSuite(room.backend, SuiteFixture{OffHostTCP: "192.0.2.1:443", OffHostUDP: "[2001:db8::1]:53"})
	if err != nil || s.fx.ResolverHost != defaultResolverHost {
		t.Fatalf("NewSuite with defaults = %+v, %v", s, err)
	}
}

func TestDefaultNameserverSkipsLoopbackAndZones(t *testing.T) {
	t.Parallel()
	for conf, want := range map[string]string{
		"# comment\nnameserver 127.0.0.1\nnameserver fe80::1%en0\nnameserver 192.0.2.53\n": "192.0.2.53:53",
		"domain example\nnameserver 2001:db8::53\n":                                        "[2001:db8::53]:53",
		"nameserver ::1\nnameserver bogus\nsearch example\n":                               "",
	} {
		s := &Suite{resolvConf: func() ([]byte, error) { return []byte(conf), nil }}
		got, err := s.defaultNameserver()
		if got != want || (want == "") != (err != nil) {
			t.Errorf("defaultNameserver(%q) = %q, %v; want %q", conf, got, err, want)
		}
	}
}

func TestPreferIPv4(t *testing.T) {
	t.Parallel()
	if got := preferIPv4([]string{"2001:db8::1", "192.0.2.1"}); got != "192.0.2.1" {
		t.Errorf("preferIPv4 = %s", got)
	}
	if got := preferIPv4([]string{"2001:db8::1"}); got != "2001:db8::1" {
		t.Errorf("preferIPv4 with no IPv4 = %s", got)
	}
}
