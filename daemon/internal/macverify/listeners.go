package macverify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"syscall"
)

// hostPortHolders reports what on the host holds port, on TCP or UDP, on any
// local address: one entry such as "tcp 127.0.0.1:47000" for each listener
// or bound UDP socket it finds. It asks the kernel rather than reading
// netstat, whose inet listing comes back empty when a Go process runs it
// (#3155).
//
// A finished job leaves its connections in TIME_WAIT on the reserved port,
// on the loopback address it used, and xnu refuses another user's bind of
// that address and port, or of the wildcard, even with SO_REUSEADDR, though
// such a socket accepts nothing. So TCP on loopback is checked by
// connecting: a listener there or on a wildcard accepts, and a refused
// connection means none. TCP on every other interface address is checked by
// binding, which a TIME_WAIT on another address does not refuse; connecting
// there can hang, as on a tunnel's address. UDP, which has no such state, is
// bound on the wildcard addresses, loopback, and every interface address, so
// any socket bound to the port refuses one of them. An address that vanished
// during the check, or a family the host lacks, is skipped; any other error,
// a loopback connection that times out included, is returned, so the check
// fails closed.
func hostPortHolders(ctx context.Context, port int) ([]string, error) {
	addrs, err := localAddrs()
	if err != nil {
		return nil, err
	}
	var holders []string
	var lc net.ListenConfig
	dialer := net.Dialer{Timeout: probeDialTimeout}
	for _, addr := range addrs {
		ap := netip.AddrPortFrom(addr, uint16(port)).String() //nolint:gosec // G115: a reserved port is 1024 to 65535
		family := "4"
		if addr.Is6() {
			family = "6"
		}
		switch {
		case addr.IsUnspecified():
			// A wildcard listener accepts the loopback connection.
		case addr.IsLoopback():
			conn, err := dialer.DialContext(ctx, "tcp"+family, ap)
			switch {
			case err == nil:
				_ = conn.Close()
				holders = append(holders, "tcp "+ap)
			case errors.Is(err, syscall.ECONNREFUSED), addrGone(err):
			default:
				return nil, fmt.Errorf("probe tcp %s: %w", ap, err)
			}
		default:
			held, err := bindRefused(lc.Listen(ctx, "tcp"+family, ap))
			if err != nil {
				return nil, fmt.Errorf("probe tcp %s: %w", ap, err)
			}
			if held {
				holders = append(holders, "tcp "+ap)
			}
		}
		held, err := bindRefused(lc.ListenPacket(ctx, "udp"+family, ap))
		if err != nil {
			return nil, fmt.Errorf("probe udp %s: %w", ap, err)
		}
		if held {
			holders = append(holders, "udp "+ap)
		}
	}
	return holders, nil
}

// bindRefused reads a probe bind: it closes a socket that bound and reports
// false, reports true when the port is in use, and false when the address is
// gone. Any other error is returned.
func bindRefused[S io.Closer](sock S, err error) (bool, error) {
	switch {
	case err == nil:
		return false, sock.Close()
	case errors.Is(err, syscall.EADDRINUSE):
		return true, nil
	case addrGone(err):
		return false, nil
	}
	return false, err
}

// addrGone reports an error meaning the address went away, or the host has
// no such family: nothing can hold the port there.
func addrGone(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT)
}

// localAddrs lists the wildcard and loopback addresses, then every address
// on the host's interfaces, each once. An IPv6 link-local address carries
// its interface as its zone, which binding it needs.
func localAddrs() ([]netip.Addr, error) {
	addrs := []netip.Addr{netip.IPv4Unspecified(), netip.IPv6Unspecified(), netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}
	seen := make(map[netip.Addr]bool)
	for _, a := range addrs {
		seen[a] = true
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list the host's interfaces: %w", err)
	}
	for _, iface := range ifaces {
		ifAddrs, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("list %s's addresses: %w", iface.Name, err)
		}
		for _, ifAddr := range ifAddrs {
			prefix, err := netip.ParsePrefix(ifAddr.String())
			if err != nil {
				continue
			}
			addr := prefix.Addr().Unmap()
			if addr.Is6() && addr.IsLinkLocalUnicast() {
				addr = addr.WithZone(iface.Name)
			}
			if !seen[addr] {
				seen[addr] = true
				addrs = append(addrs, addr)
			}
		}
	}
	return addrs, nil
}

// checkReservedPortsFree refuses with ErrReservedPortBusy when anything on
// the host holds one of set's ports, naming each holder. A probe that fails
// is an error too: the check fails closed.
func checkReservedPortsFree(
	ctx context.Context, holders func(context.Context, int) ([]string, error), slot string, set slotPortSet,
) error {
	var busy []string
	for _, port := range set {
		held, err := holders(ctx, port)
		if err != nil {
			return fmt.Errorf("check slot %s's reserved port %d: %w", slot, port, err)
		}
		busy = append(busy, held...)
	}
	if len(busy) != 0 {
		return fmt.Errorf("%w: slot %s's ports %s are held: %s", ErrReservedPortBusy, slot, set, strings.Join(busy, ", "))
	}
	return nil
}
