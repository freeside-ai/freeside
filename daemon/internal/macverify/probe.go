package macverify

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// probeDialTimeout bounds each connection attempt and the wait for a UDP
// answer. Under the sandbox every attempt fails at once; only the
// packet-filter mode, whose packets are dropped, waits it out.
const probeDialTimeout = 5 * time.Second

// probeHomeMarker is the file home-mark leaves and home-clean looks for.
const probeHomeMarker = "freeside-probe-marker"

// RunProbe is the confinement probe, run as a slot by the conformance pass
// and before every recipe command (freesided macverify probe). It prints
// "ok <check>" or "FAIL <check>: <detail>" per check, with indented
// "  (…)" detail lines, to stdout and nothing else, and returns 0 when every
// check passed, 1 when any failed, and 2 for a usage error.
//
//	sandbox <loopback-port> <resolver-host> <tcp-addr> <udp-addr> <ports> <foreign-port> <outside-dir>…
//	packet-filter <tcp-addr> <udp-addr>
//	home-mark
//	home-clean
//
// The sandbox mode requires that each of the slot's reserved ports (<ports>,
// comma-separated) listens, accepts, and exchanges a datagram on 127.0.0.1
// and ::1, and the sandbox's own refusal, EPERM, from every other network
// attempt (a bind and a connect on <foreign-port> among them) and every
// write outside the working directory, so a refusal for any other reason
// (an unreachable target, an absent directory, file permissions) fails the
// check. The packet-filter mode runs with the
// sandbox off, where EPERM would mean the sandbox was on after all, so it
// requires the packet filter's refusal instead.
func RunProbe(args []string, stdout io.Writer) int {
	p := &probe{out: stdout}
	if len(args) == 0 {
		return p.usage()
	}
	mode, args := args[0], args[1:]
	switch mode {
	case probeModeSandbox:
		if len(args) < 7 {
			return p.usage()
		}
		ports, err := parsePorts(args[4])
		if err != nil {
			return p.usage()
		}
		foreign, err := strconv.Atoi(args[5])
		if err != nil {
			return p.usage()
		}
		p.sandbox(sandboxTargets{
			loopbackPort: args[0], resolverHost: args[1], tcp: args[2], udp: args[3],
			ports: ports, foreignPort: foreign, outsideDirs: args[6:],
		})
	case probeModePacketFilter:
		if len(args) != 2 {
			return p.usage()
		}
		p.packetFilter(args[0], args[1])
	case probeModeHomeMark:
		if len(args) != 0 {
			return p.usage()
		}
		p.check(checkHomeMarked, writeNew(filepath.Join(os.Getenv("HOME"), probeHomeMarker)))
	case probeModeHomeClean:
		if len(args) != 0 {
			return p.usage()
		}
		p.check(checkHomeReset, homeClean())
	default:
		return p.usage()
	}
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
	_, _ = fmt.Fprintln(p.out, "usage: freesided macverify probe sandbox|packet-filter|home-mark|home-clean …")
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

func (p *probe) sandbox(t sandboxTargets) {
	p.check(checkLoopbackRefused, p.sandboxRefused(dialTCP(net.JoinHostPort("127.0.0.1", t.loopbackPort))))
	for _, port := range t.ports {
		p.check(checkReservedPort(port), reservedPortUsable(port))
	}
	foreign := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.foreignPort))
	p.check(checkForeignBindRefused, p.sandboxRefused(listenTCP(foreign)))
	p.check(checkForeignConnectRefused, p.sandboxRefused(dialTCP(foreign)))
	p.check(checkResolverEmpty, p.resolverEmpty(t.resolverHost))
	p.check(checkTCPRefused, p.sandboxRefused(dialTCP(t.tcp)))
	p.check(checkUDPRefused, p.sandboxRefused(sendUDP(t.udp)))
	// A random name, so no file planted in advance turns the sandbox's
	// refusal into EEXIST.
	marker := "freeside-probe-" + rand.Text()
	for _, dir := range t.outsideDirs {
		path := filepath.Join(dir, marker)
		err := writeNew(path)
		if err == nil {
			_ = os.Remove(path)
		}
		p.check(checkWriteDenied(dir), p.sandboxRefused(err))
	}
	wd, err := os.Getwd()
	if err == nil {
		err = writeNew(filepath.Join(wd, marker))
	}
	if err == nil {
		err = os.Remove(filepath.Join(wd, marker))
	}
	p.check(checkWriteJobDir, err)
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

func (p *probe) resolverEmpty(host string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeDialTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err == nil || len(addrs) != 0 {
		return fmt.Errorf("resolved %s to %v", host, addrs)
	}
	p.detail("refused as expected: %v", err)
	return nil
}

func (p *probe) packetFilter(tcp, udp string) {
	p.check(checkTCPDropped, p.filterRefused(dialTCP(tcp)))
	p.check(checkUDPDropped, p.filterRefused(exchangeUDP(udp)))
}

// filterRefused passes only a refusal that shows no packet left the host and
// that the sandbox did not make: a timeout, or the local unreachable or
// permission error pf's block returns to the sender. A connection refused or
// an ICMP error would mean the packet got out; EPERM would mean the sandbox
// refused it, not the filter.
func (p *probe) filterRefused(err error) error {
	switch {
	case err == nil:
		return errors.New("succeeded")
	case errors.Is(err, syscall.EPERM):
		return fmt.Errorf("refused with the sandbox's %w; the packet filter went unexercised", err)
	case isTimeout(err), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.EACCES):
		p.detail("refused as expected: %v", err)
		return nil
	default:
		return fmt.Errorf("refused with %w, not a drop", err)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// reservedPortUsable listens on port and connects to it over TCP, and binds
// it and sends to it over UDP, on 127.0.0.1 and then ::1.
func reservedPortUsable(port int) error {
	for _, host := range []string{"127.0.0.1", "::1"} {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		if err := tcpRoundTrip(addr); err != nil {
			return fmt.Errorf("tcp %s: %w", addr, err)
		}
		if err := udpRoundTrip(addr); err != nil {
			return fmt.Errorf("udp %s: %w", addr, err)
		}
	}
	return nil
}

// tcpRoundTrip listens on addr, connects to it, and accepts the connection.
// Both ends close with a reset, so neither leaves a TIME_WAIT on the
// reserved port: one the daemon user left there would make the slot's own
// bind of that port fail.
func tcpRoundTrip(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	conn, err := net.DialTimeout("tcp", addr, probeDialTimeout)
	if err != nil {
		return err
	}
	if err := ln.(*net.TCPListener).SetDeadline(time.Now().Add(probeDialTimeout)); err != nil {
		_ = conn.Close()
		return err
	}
	peer, err := ln.Accept()
	if err != nil {
		_ = conn.Close()
		return err
	}
	// Both before either closes: macOS refuses SO_LINGER with EINVAL on a
	// socket its peer has already reset.
	lingerErr := errors.Join(conn.(*net.TCPConn).SetLinger(0), peer.(*net.TCPConn).SetLinger(0))
	return errors.Join(lingerErr, conn.Close(), peer.Close())
}

// udpRoundTrip binds addr and receives one datagram sent to it.
func udpRoundTrip(addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = pc.Close() }()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	want := []byte("freeside-probe")
	if _, err := conn.Write(want); err != nil {
		return err
	}
	if err := pc.SetReadDeadline(time.Now().Add(probeDialTimeout)); err != nil {
		return err
	}
	buf := make([]byte, 64)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		return err
	}
	if string(buf[:n]) != string(want) {
		return fmt.Errorf("received %q", buf[:n])
	}
	return nil
}

func listenTCP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close()
	}
	return err
}

func dialTCP(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, probeDialTimeout) //nolint:gosec // G704: the probe exists to attempt connections to the targets its caller names
	if err == nil {
		_ = conn.Close()
	}
	return err
}

// sendUDP sends one DNS query and reports the first error: the sandbox
// refuses the connect, or the send.
func sendUDP(addr string) error {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write(dnsRootQuery)
	return err
}

// exchangeUDP sends one DNS query and waits for any answer: nil means one
// came back.
func exchangeUDP(addr string) error {
	conn, err := net.Dial("udp", addr) //nolint:gosec // G704: the probe exists to attempt sends to the targets its caller names
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(dnsRootQuery); err != nil {
		return err
	}
	if err := conn.SetReadDeadline(time.Now().Add(probeDialTimeout)); err != nil {
		return err
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	if n < 2 || buf[0] != dnsRootQuery[0] || buf[1] != dnsRootQuery[1] {
		return fmt.Errorf("an unexpected %d-byte answer", n)
	}
	return nil
}

// dnsRootQuery asks for the root zone's NS records with recursion desired,
// which any resolver answers.
var dnsRootQuery = []byte{
	0x46, 0x53, // id
	0x01, 0x00, // flags: recursion desired
	0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // one question
	0x00,       // the root name
	0x00, 0x02, // type NS
	0x00, 0x01, // class IN
}

func writeNew(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the probe's own marker path
	if err != nil {
		return err
	}
	return f.Close()
}

func homeClean() error {
	_, err := os.Lstat(filepath.Join(os.Getenv("HOME"), probeHomeMarker)) //nolint:gosec // G703: the marker in the slot's own home
	switch {
	case err == nil:
		return fmt.Errorf("%s survived from an earlier job", probeHomeMarker)
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return err
	}
}
