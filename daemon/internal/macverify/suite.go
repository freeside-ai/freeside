package macverify

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// defaultResolverHost is the name a slot must fail to resolve and the daemon
// must resolve, and whose address is the default off-host TCP target.
const defaultResolverHost = "github.com"

const resolvConfPath = "/etc/resolv.conf"

var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// Suite runs the executor's conformance pass. Full proves every confinement
// on every configured slot and, only when a recorder durably records the
// pass, earns the backend's declaration.
type Suite struct {
	b    *Backend
	room *Room
	fx   SuiteFixture
	// recorder receives the durable record of every completed,
	// generation-current pass. A pass with none never declares, as the
	// ward's does: declaring from an unrecorded pass would let admission
	// ride the fresh flag while the store gates against an older row.
	recorder ConformanceRecorder
	// lookupHost and dial run the positive controls; resolvConf supplies the
	// default off-host UDP target; controlTimeout bounds each control.
	lookupHost     func(ctx context.Context, host string) ([]string, error)
	dial           func(ctx context.Context, network, addr string) (net.Conn, error)
	resolvConf     func() ([]byte, error)
	controlTimeout time.Duration
}

// SuiteFixture names the targets the pass's network checks try to reach.
// The zero value selects every default.
type SuiteFixture struct {
	// ResolverHost is the name a slot's resolver must return nothing for
	// and the daemon's must resolve. Default github.com.
	ResolverHost string
	// OffHostTCP is an IP:port the daemon connects to and a slot must not.
	// Default: ResolverHost's first IPv4 address as the daemon resolves it,
	// port 443.
	OffHostTCP string
	// OffHostUDP is the IP:port of a DNS server that answers the daemon and
	// must not answer a slot. Default: the first nameserver in
	// /etc/resolv.conf that is not loopback, port 53. Override it when that
	// server does not answer the daemon user.
	OffHostUDP string
}

func (fx SuiteFixture) validate() error {
	if fx.ResolverHost != "" && !hostnamePattern.MatchString(fx.ResolverHost) {
		return fmt.Errorf("%w: ResolverHost %q is not a host name", ErrInvalidConfig, fx.ResolverHost)
	}
	for name, addr := range map[string]string{"OffHostTCP": fx.OffHostTCP, "OffHostUDP": fx.OffHostUDP} {
		if addr == "" {
			continue
		}
		if _, err := netip.ParseAddrPort(addr); err != nil {
			return fmt.Errorf("%w: %s %q is not an IP:port literal: %w", ErrInvalidConfig, name, addr, err)
		}
	}
	return nil
}

// SuiteOption configures optional Suite collaborators.
type SuiteOption func(*Suite)

// WithConformanceRecorder makes every completed, generation-current Full pass
// durably record its outcome through r.
func WithConformanceRecorder(r ConformanceRecorder) SuiteOption {
	return func(s *Suite) { s.recorder = r }
}

// NewSuite builds the conformance pass over b. It runs its jobs through a
// room over b, sharing b's slot pool with every other room.
func NewSuite(b *Backend, fx SuiteFixture, opts ...SuiteOption) (*Suite, error) {
	if err := fx.validate(); err != nil {
		return nil, err
	}
	room, err := NewRoom(b, RoomOptions{})
	if err != nil {
		return nil, err
	}
	if fx.ResolverHost == "" {
		fx.ResolverHost = defaultResolverHost
	}
	var dialer net.Dialer
	s := &Suite{
		b: b, room: room, fx: fx,
		lookupHost:     net.DefaultResolver.LookupHost,
		dial:           dialer.DialContext,
		resolvConf:     func() ([]byte, error) { return os.ReadFile(resolvConfPath) },
		controlTimeout: probeDialTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// recordStep returns the durable record step the generation guard runs, or
// nil with no recorder. Only a passed record carries the capability.
func (s *Suite) recordStep(ctx context.Context, outcome domain.ConformanceOutcome) func() error {
	if s.recorder == nil {
		return nil
	}
	return func() error {
		in := domain.BackendConformanceInput{
			Backend:             domain.BackendCredentialFreeHostVerification,
			Outcome:             outcome,
			ConfigurationDigest: s.b.ConfigurationDigest(),
			ProvedAt:            time.Now().UTC(),
		}
		if outcome == domain.ConformancePassed {
			in.Capabilities = domain.NewCapabilitySnapshot(domain.CapCredentialFreeHostVerification)
		}
		record, err := domain.NewBackendConformance(in)
		if err != nil {
			return err
		}
		return s.recorder.RecordBackendConformance(ctx, record)
	}
}

// Full runs the conformance pass: the positive controls as the daemon user,
// then, on every configured slot, the reserved-port listener check and a
// positive control on the slot's ports as the daemon user, and as that slot
// the sandbox network and write checks under the configured profile, the
// packet-filter checks with the sandbox off, and the home reset across two
// jobs. A nil error means every
// check passed and, with a recorder, the pass is recorded and declared.
//
// Tier A gate. Full refuses off macOS before taking a generation, so it
// records nothing there. Otherwise it supersedes the current proof first, in
// memory and durably, and records its own outcome last, under the
// generation guard: a pass that a newer pass or a pre-job probe failure
// superseded records and declares nothing, and returns an error saying so. It
// refuses a host whose toolchain or installed helper differs from what the
// configuration digest names, and a slot that is leased or quarantined.
func (s *Suite) Full(ctx context.Context) (err error) {
	if !s.room.hostSupported {
		return fmt.Errorf("%w: the conformance pass cannot run here", ErrHostUnsupported)
	}
	generation, beginErr := s.b.beginConformanceProof(s.recordStep(ctx, domain.ConformanceSuperseded))
	if beginErr != nil {
		return fmt.Errorf("record superseding conformance marker: %w", beginErr)
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
		case !published && outcome:
			err = errors.New("macverify: the conformance pass was superseded before it finished; it declares nothing")
		case published && recordErr != nil:
			err = errors.Join(err, fmt.Errorf("record backend conformance: %w", recordErr))
		}
	}()
	if err := checkInstalledHelper(s.room.helperPath, s.room.helperOwner); err != nil {
		return err
	}
	if err := s.room.checkToolchain(ctx); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("conformance listener: %w", err)
	}
	defer func() { _ = listener.Close() }()
	loopback := listener.Addr().String()
	tcp, udp, err := s.controls(ctx, loopback)
	if err != nil {
		return err
	}
	workspace, err := s.room.probeWorkspace()
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(loopback)
	if err != nil {
		return err
	}
	targets := sandboxTargets{
		loopbackPort: port, resolverHost: s.fx.ResolverHost, tcp: tcp, udp: udp, outsideDirs: s.room.outsideDirs,
	}
	for _, slot := range s.b.cfg.Slots {
		set := s.room.ports[slot]
		if err := checkReservedPortsFree(ctx, s.room.portHolders, slot, set); err != nil {
			return fmt.Errorf("conformance: %w", err)
		}
		for _, port := range set {
			if err := s.room.portUsable(port); err != nil {
				return &ConformanceFailure{
					Confinement: ConfinementSandboxNetwork, Slot: slot,
					Reason: fmt.Sprintf("positive control as the daemon user: reserved port %d: %v", port, err),
				}
			}
		}
		slotTargets := targets
		slotTargets.ports, slotTargets.foreignPort = set, s.room.foreignPort(slot)
		if err := s.proveSlot(ctx, slot, workspace, slotTargets); err != nil {
			return err
		}
	}
	proved = true
	return nil
}

// controls runs each network check's positive control as the daemon user,
// outside any sandbox, and returns the off-host targets: a slot's refusal
// proves nothing when the target was unreachable anyway. The loopback and
// resolver controls stand for the sandbox network confinement and the
// off-host ones for the packet filter, whose check has no errno to tell a
// drop from an unreachable target.
func (s *Suite) controls(ctx context.Context, loopback string) (tcp, udp string, err error) {
	fail := func(c Confinement, format string, args ...any) error {
		return &ConformanceFailure{Confinement: c, Reason: "positive control as the daemon user: " + fmt.Sprintf(format, args...)}
	}
	if err := s.controlDial(ctx, loopback); err != nil {
		return "", "", fail(ConfinementSandboxNetwork, "connect to the loopback listener %s: %v", loopback, err)
	}
	lookupCtx, cancel := context.WithTimeout(ctx, s.controlTimeout)
	addrs, err := s.lookupHost(lookupCtx, s.fx.ResolverHost)
	cancel()
	if err != nil || len(addrs) == 0 {
		return "", "", fail(ConfinementSandboxNetwork, "resolve %s: %v %v", s.fx.ResolverHost, addrs, err)
	}
	tcp = s.fx.OffHostTCP
	if tcp == "" {
		tcp = net.JoinHostPort(preferIPv4(addrs), "443")
	}
	if err := s.controlDial(ctx, tcp); err != nil {
		return "", "", fail(ConfinementPacketFilter, "connect to %s: %v", tcp, err)
	}
	udp = s.fx.OffHostUDP
	if udp == "" {
		if udp, err = s.defaultNameserver(); err != nil {
			return "", "", fail(ConfinementPacketFilter, "%v", err)
		}
	}
	if err := s.controlExchangeUDP(ctx, udp); err != nil {
		return "", "", fail(ConfinementPacketFilter, "DNS query to %s: %v", udp, err)
	}
	return tcp, udp, nil
}

func (s *Suite) controlDial(ctx context.Context, addr string) error {
	dialCtx, cancel := context.WithTimeout(ctx, s.controlTimeout)
	defer cancel()
	conn, err := s.dial(dialCtx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func (s *Suite) controlExchangeUDP(ctx context.Context, addr string) error {
	dialCtx, cancel := context.WithTimeout(ctx, s.controlTimeout)
	defer cancel()
	conn, err := s.dial(dialCtx, "udp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(dnsRootQuery); err != nil {
		return err
	}
	if err := conn.SetReadDeadline(time.Now().Add(s.controlTimeout)); err != nil {
		return err
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	if n < 2 || !bytes.Equal(buf[:2], dnsRootQuery[:2]) {
		return fmt.Errorf("an unexpected %d-byte answer", n)
	}
	return nil
}

// defaultNameserver is the first nameserver resolv.conf lists that is not
// loopback, at port 53.
func (s *Suite) defaultNameserver() (string, error) {
	conf, err := s.resolvConf()
	if err != nil {
		return "", fmt.Errorf("read the default off-host UDP target: %w", err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(conf))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		addr, err := netip.ParseAddr(fields[1])
		if err != nil || addr.IsLoopback() || addr.Zone() != "" {
			continue
		}
		return netip.AddrPortFrom(addr, 53).String(), nil
	}
	return "", errors.New("resolv.conf names no off-host nameserver; set SuiteFixture.OffHostUDP")
}

func preferIPv4(addrs []string) string {
	for _, a := range addrs {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() {
			return a
		}
	}
	return addrs[0]
}

// proveSlot leases exactly slot and runs the pass's four probe jobs on it.
// Every job runs through the room's job path, so the slot is prepared, torn
// down, and proved empty between them as for a recipe command.
func (s *Suite) proveSlot(ctx context.Context, slot, workspace string, targets sandboxTargets) (err error) {
	if err := s.room.pool.leaseNamed(slot); err != nil {
		return fmt.Errorf("conformance: lease slot %s: %w", slot, err)
	}
	defer func() {
		if !errors.Is(err, ErrAbsenceUnproven) {
			s.room.pool.release(slot)
		}
	}()
	for _, job := range []struct {
		profile []byte
		spec    probeSpec
	}{
		{s.room.profile, sandboxProbe(targets)},
		{permissiveProfile, packetFilterProbe(targets.tcp, targets.udp)},
		{s.room.profile, homeMarkProbe()},
		{s.room.profile, homeCleanProbe()},
	} {
		if err := s.room.runProbe(ctx, slot, workspace, job.profile, job.spec); err != nil {
			return fmt.Errorf("conformance: slot %s: %w", slot, err)
		}
	}
	return nil
}
