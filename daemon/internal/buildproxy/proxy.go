// Package buildproxy is the host-side egress path for image-build RUN steps.
//
// Apple container guests normally reach the internet through vmnet's kernel
// NAT, which binds to the host's primary interface. A VPN that moves the
// default route without becoming the primary service (observed with Mullvad,
// NordVPN, and Tailscale exit nodes; apple/container#1881) leaves that NAT
// rule unmatched, so guest DNS and direct-IP traffic die while host processes
// keep working. A build that egresses through this proxy depends only on
// guest-to-host reachability, which is the same dependency ward's runtime
// provider proxy already has.
//
// The proxy is a guest-reachable host service, so it carries its own binding
// policy (docs/plan.md §5.4). It lives only for one build. It admits only
// connections addressed to the build network's gateway from that network's
// subnet, so ward's per-run writer networks and LAN peers cannot use it. It
// refuses loopback destinations, which a host-side forwarder would newly
// expose (a guest's own loopback never reached the host), along with
// private, shared-address (tailnet), and link-local destinations, which a
// build never needs.
package buildproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Network is the runtime network Apple container attaches its BuildKit VM
// to, and therefore the network whose guests the proxy admits.
const Network = "default"

// natMode is the runtime's mode for a network with outside reachability
// (ward.NetworkNAT). Ward's credential-bearing writer networks are host-only.
const natMode = "nat"

// BuildNetwork is what the runtime reported when asked to inspect Network.
type BuildNetwork struct {
	Name        string
	Mode        string
	IPv4Gateway string
	IPv4Subnet  string
}

const (
	dialTimeout       = 30 * time.Second
	readHeaderTimeout = 10 * time.Second
	maxHeaderBytes    = 64 << 10
	// minClientPrefixBits bounds how wide a reported build subnet may be.
	minClientPrefixBits = 16
)

// carrierGradeNAT is RFC 6598 shared address space. Tailscale assigns tailnet
// addresses from it, and netip.Addr.IsPrivate does not cover it.
var carrierGradeNAT = netip.MustParsePrefix("100.64.0.0/10")

type dialControl func(network, address string, conn syscall.RawConn) error

// Proxy is one running build proxy. It serves CONNECT tunnels and ordinary
// absolute-URI HTTP requests; the pinned Debian bases fetch apt indexes over
// plain http, so a CONNECT-only proxy fails those builds.
type Proxy struct {
	server    *http.Server
	transport *http.Transport
	url       string
	dialer    *net.Dialer
	cancel    context.CancelFunc
	ctx       context.Context
	done      chan struct{}

	mu  sync.Mutex
	err error
}

// Start listens on an ephemeral host port and returns a proxy whose URL names
// the build network's gateway, the only host address a guest can reach.
//
// The report's subnet becomes the admission rule, so every field is gated and
// fails closed. The inspection decoder proves a record is self-consistent,
// not that it is the network that was asked for: a record for a host-only
// writer network would otherwise admit credential-bearing guests.
func Start(network BuildNetwork) (*Proxy, error) {
	if network.Name != Network || network.Mode != natMode {
		return nil, fmt.Errorf("build network inspection returned network %q in mode %q, want %q in mode %q",
			network.Name, network.Mode, Network, natMode)
	}
	gateway, subnet := network.IPv4Gateway, network.IPv4Subnet
	gatewayAddr, err := netip.ParseAddr(gateway)
	if err != nil || !gatewayAddr.Is4() {
		return nil, fmt.Errorf("build network reported an invalid IPv4 gateway %q", gateway)
	}
	// A malformed or implausibly wide subnet must not admit the LAN or the
	// internet.
	clients, err := netip.ParsePrefix(subnet)
	if err != nil || clients.Bits() < minClientPrefixBits || !clients.Masked().Addr().IsPrivate() ||
		!clients.Contains(gatewayAddr) {
		return nil, fmt.Errorf("build network reported an invalid IPv4 subnet %q for gateway %q", subnet, gateway)
	}
	return serve(gatewayAddr, clients, forwardableDestinationOnly)
}

func serve(gatewayAddr netip.Addr, clients netip.Prefix, control dialControl) (*Proxy, error) {
	// Same constraint as ward's provider proxy: the guest-visible gateway is
	// not reliably host-bindable (apple/container#856), so bind every
	// interface and admit by source address instead.
	listener, err := net.Listen("tcp4", "0.0.0.0:0") //nolint:gosec // source admission is restricted to the build network's subnet in admittedListener
	if err != nil {
		return nil, fmt.Errorf("listen for build proxy: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Proxy{
		url: "http://" + net.JoinHostPort(
			gatewayAddr.String(), strconv.Itoa(listener.Addr().(*net.TCPAddr).Port),
		),
		dialer: &net.Dialer{Timeout: dialTimeout, Control: control},
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	p.transport = &http.Transport{
		// Never chain to a proxy from the daemon's own environment: the
		// dial-time destination check must see the real target.
		Proxy:       nil,
		DialContext: p.dial,
	}
	forward := &httputil.ReverseProxy{
		// A forward-proxy request already carries its absolute target URL.
		Rewrite:   func(*httputil.ProxyRequest) {},
		Transport: p.transport,
		ErrorLog:  log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	p.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodConnect:
				p.connect(w, r)
			case r.URL.IsAbs() && r.URL.Scheme == "http":
				forward.ServeHTTP(w, r)
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
		}),
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		defer close(p.done)
		err := p.server.Serve(admittedListener{Listener: listener, gateway: gatewayAddr, clients: clients})
		if !errors.Is(err, http.ErrServerClosed) {
			p.mu.Lock()
			p.err = err
			p.mu.Unlock()
		}
	}()
	return p, nil
}

// URL is the proxy address as a guest on the build network sees it.
func (p *Proxy) URL() string { return p.url }

// Close stops the listener and severs every open tunnel. It reports a serve
// failure that ended the proxy early, which would otherwise surface only as
// unexplained network errors inside the build.
func (p *Proxy) Close() error {
	p.cancel()
	closeErr := p.server.Close()
	<-p.done
	p.transport.CloseIdleConnections()
	p.mu.Lock()
	defer p.mu.Unlock()
	return errors.Join(p.err, closeErr)
}

// dial reaches a destination over IPv4 only. vmnet guest NAT was IPv4, and
// IPv6 carries forms the destination policy cannot judge from the address
// alone: NAT64 and 6to4 embed private IPv4 targets, and a globally addressed
// LAN peer looks public.
func (p *Proxy) dial(ctx context.Context, _, address string) (net.Conn, error) {
	return p.dialer.DialContext(ctx, "tcp4", address)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	upstream, err := p.dial(r.Context(), "tcp", r.Host)
	if err != nil {
		// One status for every failure: distinguishing a refused destination
		// from an unreachable one would tell a RUN step which internal names
		// the host resolves.
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()
	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer func() { _ = client.Close() }()
	// http.Server.Close does not reach hijacked connections.
	stop := context.AfterFunc(p.ctx, func() {
		_ = client.Close()
		_ = upstream.Close()
	})
	defer stop()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	copyDone := make(chan struct{}, 2)
	go func() {
		// buffered may hold tunnel bytes the server read past the header.
		_, _ = io.Copy(upstream, buffered)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if tcp, ok := client.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		copyDone <- struct{}{}
	}()
	<-copyDone
	<-copyDone
}

// admittedListener drops an unadmitted connection before any request parsing,
// so that peer gets no protocol surface at all. A guest reaches the proxy at
// the gateway address from the build subnet. The source check alone would
// admit a peer on a physical LAN numbered like the build network, because the
// listener binds every interface; such a peer reaches the host at its LAN
// address, not the gateway.
type admittedListener struct {
	net.Listener
	gateway netip.Addr
	clients netip.Prefix
}

func (l admittedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.admits(conn) {
			return conn, nil
		}
		_ = conn.Close()
	}
}

func (l admittedListener) admits(conn net.Conn) bool {
	local, localOK := conn.LocalAddr().(*net.TCPAddr)
	remote, remoteOK := conn.RemoteAddr().(*net.TCPAddr)
	if !localOK || !remoteOK {
		return false
	}
	localAddr, localOK := netip.AddrFromSlice(local.IP)
	remoteAddr, remoteOK := netip.AddrFromSlice(remote.IP)
	return localOK && remoteOK && localAddr.Unmap() == l.gateway && l.clients.Contains(remoteAddr.Unmap())
}

// forwardableDestinationOnly runs after name resolution, on the address actually
// being dialed, so a hostname that resolves to a refused address is refused
// the same as the literal.
func forwardableDestinationOnly(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil || !forwardable(addrPort.Addr()) {
		return fmt.Errorf("build proxy refuses the destination %q", address)
	}
	return nil
}

// forwardable refuses loopback, unspecified, link-local, multicast, private,
// and shared-address destinations. It is deliberately not a full special-use
// registry: guest NAT forwarded the remaining reserved ranges by the host's
// routing table as well, so the proxy adds no reach there, and fake-IP tunnel
// tools place public destinations in 198.18.0.0/15.
func forwardable(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsGlobalUnicast() && !addr.IsPrivate() && !carrierGradeNAT.Contains(addr)
}
