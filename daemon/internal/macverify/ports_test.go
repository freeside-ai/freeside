package macverify

import (
	"context"
	"errors"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestReservedPortsAreDisjointRunsFromTheBase(t *testing.T) {
	t.Parallel()
	got := reservedPorts(47000, []string{"a", "b", "c"})
	want := map[string]slotPortSet{
		"a": {47000, 47001, 47002, 47003},
		"b": {47004, 47005, 47006, 47007},
		"c": {47008, 47009, 47010, 47011},
	}
	if len(got) != len(want) {
		t.Fatalf("reservedPorts = %v", got)
	}
	for slot, set := range want {
		if got[slot] != set {
			t.Errorf("slot %s = %v, want %v", slot, got[slot], set)
		}
	}
	if s := want["a"].String(); s != "47000,47001,47002,47003" {
		t.Errorf("String = %q", s)
	}
}

func TestValidatePortBase(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		base, slots int
		ok          bool
	}{
		"default":                 {DefaultPortBase, 2, true},
		"lowest":                  {1024, 1, true},
		"privileged":              {1023, 1, false},
		"zero":                    {0, 2, false},
		"last below ephemeral":    {49148, 1, true},
		"last at ephemeral":       {49149, 1, false},
		"a second slot reaches":   {49145, 2, false},
		"inside the ephemeral":    {50000, 1, false},
		"overflowing":             {math.MaxInt - 2, 2, false},
		"many slots from default": {DefaultPortBase, 538, true},
	} {
		err := validatePortBase(tc.base, tc.slots, 49152)
		if (err == nil) != tc.ok {
			t.Errorf("%s: validatePortBase(%d, %d) = %v, want ok %t", name, tc.base, tc.slots, err, tc.ok)
		}
	}
}

func TestNewRefusesAPortBaseThatBreaksARule(t *testing.T) {
	t.Parallel()
	first, err := ephemeralPortFirst()
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []int{0, 1023, first - 1} {
		_, err := New(Config{
			ExecutorRoot: "/var/freeside/macverify", SandboxProfile: DefaultSandboxProfile(),
			PacketFilterRules: []byte("rules"), Slots: []string{"slot1"}, Toolchain: testToolchain, PortBase: base,
		})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("port base %d = %v, want ErrInvalidConfig", base, err)
		}
	}
}

func TestConfigReservesPort(t *testing.T) {
	t.Parallel()
	cfg := Config{Slots: []string{"slot1", "slot2"}, PortBase: 47000}
	for port, want := range map[int]bool{46999: false, 47000: true, 47003: true, 47004: true, 47007: true, 47008: false} {
		if got := cfg.ReservesPort(port); got != want {
			t.Errorf("ReservesPort(%d) = %t, want %t", port, got, want)
		}
	}
}

func TestRenderPortsConfSortsTheSlots(t *testing.T) {
	t.Parallel()
	got := string(RenderPortsConf(47000, []string{"fvslot2", "fvslot1"}))
	if want := "fvslot1 47000 47001 47002 47003\nfvslot2 47004 47005 47006 47007\n"; got != want {
		t.Fatalf("RenderPortsConf = %q, want %q", got, want)
	}
}

func TestParsePortsRefusesAnythingButFourCanonicalPorts(t *testing.T) {
	t.Parallel()
	if got, err := parsePorts("47000,47001,47002,47003"); err != nil || got != (slotPortSet{47000, 47001, 47002, 47003}) {
		t.Fatalf("parsePorts = %v, %v", got, err)
	}
	for _, s := range []string{
		"", "47000,47001,47002", "47000,47001,47002,47003,47004", "1023,47001,47002,47003",
		"47000,47001,47002,65536", "047000,47001,47002,47003", "+47000,47001,47002,47003",
		"47000, 47001,47002,47003", "47000,,47002,47003",
	} {
		if got, err := parsePorts(s); err == nil {
			t.Errorf("parsePorts(%q) = %v, want an error", s, got)
		}
	}
}

// TestHostPortHoldersFindsWhatHoldsAPort holds a port over TCP, then UDP,
// and checks the kernel-backed check names it; a free port has no holder.
func TestHostPortHoldersFindsWhatHoldsAPort(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	tcpPort := ln.Addr().(*net.TCPAddr).Port
	holders, err := hostPortHolders(t.Context(), tcpPort)
	if err != nil || !slices.Contains(holders, "tcp 127.0.0.1:"+strconv.Itoa(tcpPort)) {
		t.Fatalf("hostPortHolders with a TCP listener = %v, %v", holders, err)
	}

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	udpPort := pc.LocalAddr().(*net.UDPAddr).Port
	holders, err = hostPortHolders(t.Context(), udpPort)
	if err != nil || !slices.Contains(holders, "udp 127.0.0.1:"+strconv.Itoa(udpPort)) {
		t.Fatalf("hostPortHolders with a bound UDP socket = %v, %v", holders, err)
	}

	free := freePorts(t, 1)[0]
	if holders, err := hostPortHolders(t.Context(), free); err != nil || len(holders) != 0 {
		t.Fatalf("hostPortHolders on a free port = %v, %v", holders, err)
	}
}

// TestHostPortHoldersIgnoresTimeWait: a connection whose reserved-port end
// closed first sits in TIME_WAIT on that port after its listener is gone,
// as a finished job's can, and does not hold the port for the next job.
// Here the test owns the TIME_WAIT; the slot-owned case, where a bind probe
// would be refused, is the room's live suite.
//
//nolint:paralleltest // Serial: once its listener closes, the port is free for a parallel test to listen on, which would read as a holder.
func TestHostPortHoldersIgnoresTimeWait(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	_ = server.Close() // the listening end closes first and keeps TIME_WAIT
	_, _ = client.Read(make([]byte, 1))
	_ = client.Close()
	if holders, err := hostPortHolders(t.Context(), port); err != nil || len(holders) != 0 {
		t.Fatalf("hostPortHolders after TIME_WAIT = %v, %v; want none", holders, err)
	}
}

func TestCheckReservedPortsFree(t *testing.T) {
	t.Parallel()
	set := slotPortSet{47000, 47001, 47002, 47003}
	none := func(context.Context, int) ([]string, error) { return nil, nil }
	if err := checkReservedPortsFree(t.Context(), none, "slot1", set); err != nil {
		t.Fatalf("free ports = %v", err)
	}
	busy := func(_ context.Context, port int) ([]string, error) {
		if port == 47002 {
			return []string{"udp *:47002"}, nil
		}
		return nil, nil
	}
	err := checkReservedPortsFree(t.Context(), busy, "slot1", set)
	if !errors.Is(err, ErrReservedPortBusy) || !strings.Contains(err.Error(), "udp *:47002") {
		t.Fatalf("a held port = %v, want ErrReservedPortBusy naming its holder", err)
	}
	broken := errors.New("bind: permission denied")
	failing := func(context.Context, int) ([]string, error) { return nil, broken }
	err = checkReservedPortsFree(t.Context(), failing, "slot1", set)
	if !errors.Is(err, broken) || errors.Is(err, ErrReservedPortBusy) {
		t.Fatalf("a failed check = %v, want its error and not a busy port", err)
	}
}

// TestRunRefusesABusyReservedPortAndReleasesTheSlot: a host listener on a
// reserved port refuses the job before prepare, and the slot stays usable,
// since a host listener is not a broken confinement.
func TestRunRefusesABusyReservedPortAndReleasesTheSlot(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	room.portHolders = func(_ context.Context, port int) ([]string, error) {
		if port == 47001 {
			return []string{"tcp 127.0.0.1:47001"}, nil
		}
		return nil, nil
	}
	_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
	if !errors.Is(err, ErrReservedPortBusy) || !strings.Contains(err.Error(), "tcp 127.0.0.1:47001") {
		t.Fatalf("Run = %v, want ErrReservedPortBusy naming the holder", err)
	}
	if calls, _, _ := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("the refused job reached the launcher: %v", calls)
	}
	if err := room.pool.leaseNamed("slot1"); err != nil {
		t.Fatalf("slot not released after the refusal: %v", err)
	}
}

func TestRunRefusesAPrepareReportingOtherPorts(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	fake.ports["slot1"] = slotPortSet{48000, 48001, 48002, 48003}
	_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
	if err == nil || !strings.Contains(err.Error(), "reserved ports 48000,48001,48002,48003 for slot slot1") {
		t.Fatalf("Run = %v, want a refusal of the reported ports", err)
	}
}

func TestForeignPortIsAnotherSlotsOrBelowTheSet(t *testing.T) {
	t.Parallel()
	room := newTestRoom(t, newFakeLauncher(), "slot1", "slot2", "slot3")
	for slot, want := range map[string]int{"slot1": 47004, "slot2": 47008, "slot3": 47000} {
		if got := room.foreignPort(slot); got != want {
			t.Errorf("foreignPort(%s) = %d, want %d", slot, got, want)
		}
	}
	if got := newTestRoom(t, newFakeLauncher()).foreignPort("slot1"); got != 46999 {
		t.Errorf("a lone slot's foreign port = %d, want 46999", got)
	}
}

// TestFullRefusesABusyReservedPort: the pass does not run on a host where
// something holds a slot's reserved port, and records no pass.
func TestFullRefusesABusyReservedPort(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	s, rec := newTestSuite(t, fake, "slot1", "slot2")
	s.room.portHolders = func(_ context.Context, port int) ([]string, error) {
		if port == 47005 {
			return []string{"tcp *:47005"}, nil
		}
		return nil, nil
	}
	if err := s.Full(t.Context()); !errors.Is(err, ErrReservedPortBusy) {
		t.Fatalf("Full = %v, want ErrReservedPortBusy", err)
	}
	if s.b.proven.Load() {
		t.Fatal("a pass that did not run declared")
	}
	if got := rec.outcomes(); slices.Contains(got, domain.ConformancePassed) {
		t.Fatalf("records = %v, want no pass", got)
	}
}

// TestFullFailsAReservedPortTheDaemonCannotUse: a reserved port the daemon
// user cannot bind and reach fails the pass, as any positive control does,
// since a slot's success there would then prove nothing.
func TestFullFailsAReservedPortTheDaemonCannotUse(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	s, _ := newTestSuite(t, fake)
	s.room.portUsable = func(port int) error {
		if port == 47002 {
			return errors.New("bind: address already in use")
		}
		return nil
	}
	err := s.Full(t.Context())
	var failure *ConformanceFailure
	if !errors.As(err, &failure) || failure.Confinement != ConfinementSandboxNetwork || failure.Slot != "slot1" ||
		!strings.Contains(err.Error(), "reserved port 47002") {
		t.Fatalf("Full = %v, want a sandbox network control failure naming port 47002", err)
	}
	if s.b.proven.Load() {
		t.Fatal("a failed pass declared")
	}
}

// TestHelperSelectPorts runs slot-run's select_ports on ports.conf lines:
// it takes exactly four ports from 1024 to 65535 and refuses anything else.
func TestHelperSelectPorts(t *testing.T) {
	t.Parallel()
	script := string(helperScript)
	start := strings.Index(script, "\nselect_ports() {")
	if start < 0 {
		t.Fatal("slot-run.sh defines no select_ports")
	}
	end := strings.Index(script[start:], "\n}\n")
	if end < 0 {
		t.Fatal("select_ports has no end")
	}
	function := script[start : start+end+3]
	run := func(line string) (string, error) {
		body := "refuse() { printf 'refused: %s\\n' \"$*\"; exit 3; }\nslot=fvslot1\nportsconf=ports.conf\n" +
			function + "select_ports \"$1\"\nprintf '%s %s %s %s %s\\n' \"$port1\" \"$port2\" \"$port3\" \"$port4\" \"$ports_csv\"\n"
		path := filepath.Join(t.TempDir(), "select.sh")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/bin/sh", path, line).CombinedOutput() //nolint:gosec // G204: fixed shell running a test-owned script
		return string(out), err
	}
	if out, err := run("47000 47001 47002 47003"); err != nil || out != "47000 47001 47002 47003 47000,47001,47002,47003\n" {
		t.Fatalf("select_ports on a valid line = %q, %v", out, err)
	}
	for _, line := range []string{
		"", "47000 47001 47002", "47000 47001 47002 47003 47004", "1023 47001 47002 47003",
		"47000 47001 47002 65536", "047000 47001 47002 47003", "47000 47001 47002 4700x",
		"47000 47001 47002 *", "47000 47001 47002 470000",
	} {
		out, err := run(line)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.HasPrefix(out, "refused: ") {
			t.Errorf("select_ports(%q) = %q, %v; want a refusal", line, out, err)
		}
	}
}
