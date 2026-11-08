package daemonshell

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

var (
	tailscaleIPv4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

const tailscaleIPTimeout = 5 * time.Second

func ListenPrivileged(addr string) (net.Listener, error) {
	return listenPrivilegedWith(
		addr,
		func(network string, resolved *net.TCPAddr) (net.Listener, error) {
			return net.ListenTCP(network, resolved)
		},
		ReadTailscaleIPs,
	)
}

func listenPrivilegedWith(
	addr string,
	bind func(string, *net.TCPAddr) (net.Listener, error),
	tailscaleIPs func() ([]netip.Addr, error),
) (net.Listener, error) {
	resolved, err := ResolveListenAddress(addr, tailscaleIPs)
	if err != nil {
		return nil, err
	}
	network := "tcp6"
	if resolved.IP.To4() != nil {
		network = "tcp4"
	}
	listener, err := bind(network, resolved)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", addr, err)
	}
	bound, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !bound.IP.Equal(resolved.IP) {
		_ = listener.Close()
		return nil, fmt.Errorf("listen %q bound unexpected address %q", addr, listener.Addr())
	}
	return listener, nil
}

// ResolveListenAddress checks listener policy without binding, so deputy
// settings can be refused before taking a production rig lease.
func ResolveListenAddress(addr string, tailscaleIPs func() ([]netip.Addr, error)) (*net.TCPAddr, error) {
	resolved, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve listen address %q: %w", addr, err)
	}
	if resolved.IP == nil || resolved.IP.IsUnspecified() || resolved.Zone != "" {
		return nil, fmt.Errorf("listen %q resolved to an unsupported address %q", addr, resolved)
	}
	if !resolved.IP.IsLoopback() {
		if !isTailscaleIP(resolved.IP) {
			return nil, fmt.Errorf("listen %q resolved to non-loopback, non-Tailscale address %q", addr, resolved)
		}
		addrs, err := tailscaleIPs()
		if err != nil {
			return nil, fmt.Errorf("query Tailscale addresses for listener %q: %w", addr, err)
		}
		if !tailscaleOwnsIP(resolved.IP, addrs) {
			return nil, fmt.Errorf("listen %q resolved to address %q not reported by Tailscale", addr, resolved)
		}
	}
	return resolved, nil
}

// loopbackTwinAddr returns the loopback address that mirrors a non-loopback
// primary listener: 127.0.0.1 at the primary's port. It reports false when the
// primary is already loopback, so a loopback listener stays single. An IPv6
// Tailscale primary still twins onto 127.0.0.1, so same-host clients follow one
// rule (#1449).
func loopbackTwinAddr(bound *net.TCPAddr) (*net.TCPAddr, bool) {
	if bound == nil || bound.IP.IsLoopback() {
		return nil, false
	}
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: bound.Port}, true
}

// ListenLoopbackTwin opens the loopback companion for a primary bound to a
// Tailscale-owned address, so a same-host client always has a path a host VPN
// cannot drop (#1449). It returns a nil listener when the primary is already
// loopback. It binds after the primary so a :0 primary has its real port. A
// bind failure, or a bound address that is not loopback at that port, is
// returned as an error naming the loopback address; startup then fails, which
// is the contract: the daemon never runs reachable only over Tailscale.
func ListenLoopbackTwin(
	primary net.Listener,
	bind func(string, *net.TCPAddr) (net.Listener, error),
) (net.Listener, error) {
	bound, ok := primary.Addr().(*net.TCPAddr)
	if !ok {
		return nil, fmt.Errorf("primary listener address %q is not a TCP address", primary.Addr())
	}
	twinAddr, ok := loopbackTwinAddr(bound)
	if !ok {
		return nil, nil
	}
	listener, err := bind("tcp4", twinAddr)
	if err != nil {
		return nil, fmt.Errorf("listen loopback twin %q: %w", twinAddr, err)
	}
	loopbackBound, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !loopbackBound.IP.IsLoopback() || loopbackBound.Port != bound.Port {
		_ = listener.Close()
		return nil, fmt.Errorf("loopback twin bound unexpected address %q, want loopback at port %d",
			listener.Addr(), bound.Port)
	}
	return listener, nil
}

// LoopbackTwinListenAddress mirrors loopbackTwinAddr for a listen address in
// host:port text, so the daemon and the rig and preflight probes share one rule
// for which primary addresses also answer on loopback (#1449). It reports false
// for a loopback address. The callers pass an IP literal, so no DNS occurs.
func LoopbackTwinListenAddress(address string) (string, bool, error) {
	resolved, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return "", false, fmt.Errorf("resolve listen address %q: %w", address, err)
	}
	twin, ok := loopbackTwinAddr(resolved)
	if !ok {
		return "", false, nil
	}
	return twin.String(), true, nil
}

// ConnectionModeOf names how clients reach a listener listenPrivilegedWith
// bound: loopback for a loopback address, tailscale for a Tailscale-owned
// one. Those are the only two addresses it admits, so any other bound
// address is a composition defect, not a third mode; a relay mode arrives
// with the §5.19 relay.
func ConnectionModeOf(bound net.Addr) (domain.ConnectionMode, error) {
	tcp, ok := bound.(*net.TCPAddr)
	if !ok {
		return "", fmt.Errorf("listener address %q is not a TCP address", bound)
	}
	switch {
	case tcp.IP.IsLoopback():
		return domain.ConnectionLoopback, nil
	case isTailscaleIP(tcp.IP):
		return domain.ConnectionTailscale, nil
	}
	return "", fmt.Errorf("listener address %q is neither loopback nor Tailscale-owned", bound)
}

func isTailscaleIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	return isTailscaleAddr(addr)
}

func ReadTailscaleIPs() ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tailscaleIPTimeout)
	defer cancel()

	// The executable name and arguments are fixed; only the trusted supervisor
	// controls the daemon's PATH.
	cmd := osexec.CommandContext(ctx, "tailscale", "ip") //nolint:gosec // G204 has no untrusted command input.
	procbound.Bind(cmd, procbound.DefaultWaitDelay)
	defer procbound.Reap(cmd)
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tailscale ip: %w", ctx.Err())
		}
		return nil, fmt.Errorf("tailscale ip: %w", err)
	}
	return parseTailscaleIPs(string(output))
}

func parseTailscaleIPs(output string) ([]netip.Addr, error) {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return nil, errors.New("tailscale ip returned no addresses")
	}
	addrs := make([]netip.Addr, 0, len(fields))
	for _, field := range fields {
		addr, err := netip.ParseAddr(field)
		if err != nil || addr.Zone() != "" || !isTailscaleAddr(addr) {
			return nil, fmt.Errorf("tailscale ip returned unsupported address %q", field)
		}
		addrs = append(addrs, addr.Unmap())
	}
	return addrs, nil
}

func tailscaleOwnsIP(ip net.IP, addrs []netip.Addr) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, assigned := range addrs {
		if assigned.Unmap() == addr {
			return true
		}
	}
	return false
}

func isTailscaleAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	return tailscaleIPv4.Contains(addr) || tailscaleIPv6.Contains(addr)
}
