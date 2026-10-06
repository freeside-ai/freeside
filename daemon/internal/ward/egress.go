package ward

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxProxyHeaderBytes = 8 << 10
	maxClientHelloBytes = 64 << 10
	maxProxyConnections = 32
	// registryDialStagger is how long the proxy gives one registry address
	// before it also tries the next. An address that never answers (a
	// blackholed IPv6 route ahead of a working IPv4 one) then delays the
	// tunnel by this much instead of spending its dial budget.
	registryDialStagger = 250 * time.Millisecond
)

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// lookupIPFunc resolves a registry host name to its addresses.
type lookupIPFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// registryRoutes is the provider_registry half of a proxy's allowlist: the
// declared registry authorities and the resolver their addresses come from.
// The zero value admits no registry, which is every provider_only proxy.
type registryRoutes struct {
	authorities []string
	lookup      lookupIPFunc
}

var errClientHelloCaptured = errors.New("TLS ClientHello captured")

// connectProxy is the daemon-side half of provider_only and
// provider_registry. The writer sits on a host-only runtime network, so its
// only route beyond that network is this CONNECT-only listener on the
// network's host gateway. Exact authorities are admitted; ordinary HTTP,
// alternate ports, IP literals, and every undeclared destination are refused.
type connectProxy struct {
	listener net.Listener
	url      string
	// allowed holds every admitted CONNECT authority; the value is true for
	// a declared registry and false for a provider endpoint. It is written
	// once, before serve starts.
	allowed   map[string]bool
	clientNet *net.IPNet
	dial      dialContextFunc
	lookup    lookupIPFunc
	timeout   time.Duration
	now       func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	sem    chan struct{}
	wg     sync.WaitGroup

	mu     sync.Mutex
	err    error
	active map[net.Conn]struct{}
	// lastProviderByte is when a provider last sent the writer a byte: the
	// stall heartbeat. Only the provider-to-writer leg stamps it, so bytes
	// the writer uploads can neither refresh nor suppress it.
	lastProviderByte time.Time
}

// startConnectProxy starts a proxy that admits the provider endpoints only.
func startConnectProxy(parent context.Context, gateway, subnet string, allowed []string, timeout time.Duration, dial dialContextFunc, now func() time.Time) (*connectProxy, error) {
	return startRegistryConnectProxy(parent, gateway, subnet, allowed, registryRoutes{}, timeout, dial, now)
}

// startRegistryConnectProxy starts a proxy that admits the provider endpoints
// and the declared registry authorities. The two differ in how the proxy
// reaches them. A provider endpoint is daemon configuration and is dialed by
// name. A registry authority comes from project policy, whose host check is
// only syntactic, so the proxy resolves the name itself and refuses it unless
// every address is public (dialRegistry).
func startRegistryConnectProxy(parent context.Context, gateway, subnet string, providers []string, registry registryRoutes, timeout time.Duration, dial dialContextFunc, now func() time.Time) (*connectProxy, error) {
	ip := net.ParseIP(gateway)
	if ip == nil || ip.To4() == nil {
		return nil, errors.New("egress network reported an invalid IPv4 gateway")
	}
	_, clientNet, err := net.ParseCIDR(subnet)
	ones, bits := 0, 0
	if err == nil {
		ones, bits = clientNet.Mask.Size()
	}
	networkIP := clientNetIP(clientNet)
	gatewayIP := ip.To4()
	if err != nil || networkIP == nil || bits != 32 || ones != 24 ||
		!clientNet.Contains(gatewayIP) ||
		gatewayIP[0] != networkIP[0] || gatewayIP[1] != networkIP[1] ||
		gatewayIP[2] != networkIP[2] || gatewayIP[3] != networkIP[3]+1 {
		return nil, errors.New("egress network reported an invalid IPv4 subnet")
	}
	// The vmnet gateway is the address the guest uses for its host, not an
	// address assigned to a macOS interface, so macOS refuses a direct bind.
	// Bind the ephemeral provider proxy on all host interfaces and advertise
	// only the host-only gateway into the writer. Keeping unrelated daemon
	// listeners off this gateway is the separate listener-isolation contract.
	listener, err := net.Listen("tcp4", "0.0.0.0:0") //nolint:gosec // vmnet's guest-visible gateway is not host-bindable; source admission is restricted to its attested per-run /24 below
	if err != nil {
		return nil, fmt.Errorf("listen for provider proxy: %w", err)
	}
	if dial == nil {
		d := net.Dialer{Timeout: timeout}
		dial = d.DialContext
	}
	lookup := registry.lookup
	if lookup == nil {
		lookup = rootedLookup(net.DefaultResolver)
	}
	ctx, cancel := context.WithCancel(parent)
	p := &connectProxy{
		listener: listener,
		url: "http://" + net.JoinHostPort(
			gateway,
			strconv.Itoa(listener.Addr().(*net.TCPAddr).Port),
		),
		allowed:   make(map[string]bool, len(providers)+len(registry.authorities)),
		clientNet: clientNet,
		dial:      dial,
		lookup:    lookup,
		timeout:   timeout,
		now:       now,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		sem:       make(chan struct{}, maxProxyConnections),
		active:    make(map[net.Conn]struct{}),
	}
	for _, authority := range registry.authorities {
		p.allowed[authority] = true
	}
	// A provider endpoint is admitted under every profile, so an authority
	// declared as both stays on the provider path: declaring it grants nothing.
	for _, authority := range providers {
		p.allowed[authority] = false
	}
	go p.serve()
	return p, nil
}

// ipResolver is the part of net.Resolver a registry lookup uses.
type ipResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// rootedLookup resolves a host through resolver as a rooted name. A declared
// registry host is relative as written, so without the trailing dot the
// resolver's search list could answer for a different name under a local
// suffix.
func rootedLookup(resolver ipResolver) lookupIPFunc {
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		return resolver.LookupNetIP(ctx, "ip", host+".")
	}
}

func clientNetIP(network *net.IPNet) net.IP {
	if network == nil {
		return nil
	}
	return network.IP.To4()
}

func (p *connectProxy) URL() string {
	return p.url
}

// Allowlist reports the authorities this proxy admits, read from the table
// handle consults rather than from what the caller asked for: the provider
// endpoints it dials by name, and the registry authorities it resolves and
// holds to public addresses. Each list is sorted.
func (p *connectProxy) Allowlist() (providers, registries []string) {
	for authority, registry := range p.allowed {
		if registry {
			registries = append(registries, authority)
		} else {
			providers = append(providers, authority)
		}
	}
	slices.Sort(providers)
	slices.Sort(registries)
	return providers, registries
}

// LastProviderByte reports when a provider last sent the writer a byte, or
// the zero time before the first one.
func (p *connectProxy) LastProviderByte() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastProviderByte
}

func (p *connectProxy) serve() {
	defer close(p.done)
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if p.ctx.Err() == nil {
				p.mu.Lock()
				p.err = err
				p.mu.Unlock()
			}
			return
		}
		if !p.clientAllowed(conn.RemoteAddr()) {
			_ = conn.Close()
			continue
		}
		select {
		case p.sem <- struct{}{}:
			p.mu.Lock()
			p.active[conn] = struct{}{}
			p.mu.Unlock()
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer func() { <-p.sem }()
				defer func() {
					p.mu.Lock()
					delete(p.active, conn)
					p.mu.Unlock()
				}()
				p.handle(conn)
			}()
		default:
			_ = writeProxyResponse(conn, http.StatusServiceUnavailable)
			_ = conn.Close()
		}
	}
}

func (p *connectProxy) clientAllowed(address net.Addr) bool {
	remote, ok := address.(*net.TCPAddr)
	return ok && p.clientNet.Contains(remote.IP)
}

func (p *connectProxy) handle(client net.Conn) {
	defer func() { _ = client.Close() }()
	_ = client.SetReadDeadline(time.Now().Add(p.timeout))
	limited := &io.LimitedReader{R: client, N: maxProxyHeaderBytes + 1}
	reader := bufio.NewReader(limited)
	req, err := http.ReadRequest(reader)
	headerBytes := maxProxyHeaderBytes + 1 - limited.N - int64(reader.Buffered())
	if err != nil || headerBytes > maxProxyHeaderBytes ||
		req.Method != http.MethodConnect || req.RequestURI == "" ||
		req.Host != req.RequestURI || req.ContentLength > 0 || len(req.TransferEncoding) > 0 {
		_ = writeProxyResponse(client, http.StatusBadRequest)
		return
	}
	authority, err := canonicalAuthority(req.RequestURI)
	if err != nil || authority != req.RequestURI {
		_ = writeProxyResponse(client, http.StatusForbidden)
		return
	}
	registry, ok := p.allowed[authority]
	if !ok {
		_ = writeProxyResponse(client, http.StatusForbidden)
		return
	}
	dialCtx, cancel := context.WithTimeout(p.ctx, p.timeout)
	var upstream net.Conn
	if registry {
		var status int
		upstream, status = p.dialRegistry(dialCtx, authority)
		cancel()
		if upstream == nil {
			_ = writeProxyResponse(client, status)
			return
		}
	} else {
		upstream, err = p.dial(dialCtx, "tcp", authority)
		cancel()
		if err != nil {
			_ = writeProxyResponse(client, http.StatusBadGateway)
			return
		}
	}
	defer func() { _ = upstream.Close() }()
	if err := writeProxyResponse(client, http.StatusOK); err != nil {
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	// The header cap ends at the CONNECT boundary. Drain any tunnel bytes
	// bufio prefetched through the LimitedReader, then continue from the raw
	// client; using reader alone would cap the entire upload at the header
	// limit.
	clientTunnel := io.MultiReader(reader, client)
	expectedServerName, _, _ := net.SplitHostPort(authority)
	clientTunnel, err = requireTLSServerName(p.ctx, client, clientTunnel, expectedServerName, p.timeout)
	if err != nil {
		return
	}
	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, clientTunnel)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		copyDone <- struct{}{}
	}()
	// Only a provider's bytes are the stall heartbeat: a registry download
	// is tool activity, not a response from the model.
	var fromUpstream io.Reader = upstream
	if !registry {
		fromUpstream = providerBytes{r: upstream, p: p}
	}
	go func() {
		_, _ = io.Copy(client, fromUpstream)
		if tcp, ok := client.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		copyDone <- struct{}{}
	}()
	for completed := 0; completed < 2; completed++ {
		select {
		case <-copyDone:
		case <-p.ctx.Done():
			return
		}
	}
}

// dialRegistry connects to a declared registry authority, or reports the
// status to refuse the CONNECT with. It resolves the name once and dials an
// address it checked, so a second resolution cannot hand the dialer a
// different one. Any address outside public unicast space refuses the whole
// name, including one that also has a public address: a declared registry is
// a public service, and a name that answers with a private address would let
// the writer reach the host's own networks through the allowlist.
func (p *connectProxy) dialRegistry(ctx context.Context, authority string) (net.Conn, int) {
	host, port, _ := net.SplitHostPort(authority)
	addrs, err := p.lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, http.StatusBadGateway
	}
	for _, addr := range addrs {
		if !publicUnicast(addr) {
			return nil, http.StatusForbidden
		}
	}
	conn := p.dialFirst(ctx, addrs, port)
	if conn == nil {
		return nil, http.StatusBadGateway
	}
	return conn, http.StatusOK
}

// dialFirst dials addrs in resolver order, starting each one
// registryDialStagger after the one before, and returns the first connection,
// or nil when every attempt fails or ctx ends. Trying them strictly in turn
// would let unanswering addresses at the head of the list spend the budget
// before a working one is tried. It returns only once every attempt has
// ended, so none outlives the call and a connection that loses is closed.
func (p *connectProxy) dialFirst(ctx context.Context, addrs []netip.Addr, port string) net.Conn {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	attempts := make(chan net.Conn)
	for i, addr := range addrs {
		go func() {
			select {
			case <-time.After(time.Duration(i) * registryDialStagger):
			case <-ctx.Done():
				attempts <- nil
				return
			}
			conn, err := p.dial(ctx, "tcp", net.JoinHostPort(addr.Unmap().String(), port))
			if err != nil {
				conn = nil
			}
			attempts <- conn
		}()
	}
	var first net.Conn
	for range addrs {
		switch conn := <-attempts; {
		case conn == nil:
		case first == nil:
			first = conn
			cancel()
		default:
			_ = conn.Close()
		}
	}
	return first
}

// The IANA special-purpose address blocks: nothing in them is a public
// package registry. IPv4 is public unless listed. IPv6 is public only inside
// the global unicast block, less the parts of it reserved for protocols,
// documentation, and 6to4 (which embeds an IPv4 address this check would not
// see). NAT64 (64:ff9b::/96) falls outside the global block and is refused
// for the same reason.
var (
	nonPublicIPv4 = parsePrefixes(
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	)
	globalUnicastIPv6 = netip.MustParsePrefix("2000::/3")
	nonPublicIPv6     = parsePrefixes("2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20")
)

func parsePrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, len(values))
	for i, value := range values {
		prefixes[i] = netip.MustParsePrefix(value)
	}
	return prefixes
}

// publicUnicast reports whether addr is a public unicast address. The zero
// Addr and a zoned (link-scoped) address are not.
func publicUnicast(addr netip.Addr) bool {
	if addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	inAny := func(prefixes []netip.Prefix) bool {
		return slices.ContainsFunc(prefixes, func(p netip.Prefix) bool { return p.Contains(addr) })
	}
	if addr.Is4() {
		return !inAny(nonPublicIPv4)
	}
	return globalUnicastIPv6.Contains(addr) && !inAny(nonPublicIPv6)
}

// providerBytes stamps the proxy's heartbeat on every read from the
// provider that returns data.
type providerBytes struct {
	r io.Reader
	p *connectProxy
}

func (b providerBytes) Read(buf []byte) (int, error) {
	n, err := b.r.Read(buf)
	if n > 0 {
		now := b.p.now()
		b.p.mu.Lock()
		b.p.lastProviderByte = now
		b.p.mu.Unlock()
	}
	return n, err
}

// requireTLSServerName asks the standard library TLS parser to read exactly
// the client's first handshake far enough to expose SNI, without terminating
// TLS at the proxy. Every byte it consumed is replayed to the provider before
// the remainder of the tunnel. A CONNECT line alone is not enough: on a
// shared CDN an adversarial writer could otherwise name an allowed authority
// in cleartext and select a different tenant inside TLS.
func requireTLSServerName(
	parent context.Context,
	client net.Conn,
	tunnel io.Reader,
	expected string,
	timeout time.Duration,
) (io.Reader, error) {
	_ = client.SetReadDeadline(time.Now().Add(timeout))
	limited := &io.LimitedReader{R: tunnel, N: maxClientHelloBytes + 1}
	capture := &clientHelloCaptureConn{
		reader: limited,
		local:  client.LocalAddr(),
		remote: client.RemoteAddr(),
	}
	var observed string
	parser := tls.Server(capture, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			observed = hello.ServerName
			return nil, errClientHelloCaptured
		},
	})
	ctx, cancel := context.WithTimeout(parent, timeout)
	err := parser.HandshakeContext(ctx)
	cancel()
	if !errors.Is(err, errClientHelloCaptured) ||
		capture.buf.Len() > maxClientHelloBytes ||
		observed != expected {
		return nil, errors.New("TLS ClientHello does not match CONNECT authority")
	}
	_ = client.SetReadDeadline(time.Time{})
	return io.MultiReader(bytes.NewReader(capture.buf.Bytes()), limited, tunnel), nil
}

type clientHelloCaptureConn struct {
	reader io.Reader
	buf    bytes.Buffer
	local  net.Addr
	remote net.Addr
}

func (c *clientHelloCaptureConn) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	_, _ = c.buf.Write(p[:n])
	return n, err
}

func (*clientHelloCaptureConn) Write(p []byte) (int, error) { return len(p), nil }
func (*clientHelloCaptureConn) Close() error                { return nil }
func (c *clientHelloCaptureConn) LocalAddr() net.Addr       { return c.local }
func (c *clientHelloCaptureConn) RemoteAddr() net.Addr      { return c.remote }
func (*clientHelloCaptureConn) SetDeadline(time.Time) error { return nil }
func (*clientHelloCaptureConn) SetReadDeadline(time.Time) error {
	return nil
}

func (*clientHelloCaptureConn) SetWriteDeadline(time.Time) error {
	return nil
}

func writeProxyResponse(conn net.Conn, status int) error {
	connection := "Connection: close\r\n"
	if status == http.StatusOK {
		connection = ""
	}
	_, err := fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n%s\r\n", status, http.StatusText(status), connection)
	return err
}

func (p *connectProxy) Close() error {
	if p == nil {
		return nil
	}
	p.cancel()
	_ = p.listener.Close()
	<-p.done
	p.mu.Lock()
	active := make([]net.Conn, 0, len(p.active))
	for conn := range p.active {
		active = append(active, conn)
	}
	p.mu.Unlock()
	for _, conn := range active {
		_ = conn.Close()
	}
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func canonicalAuthority(value string) (string, error) {
	if strings.ContainsAny(value, "/\\@") {
		return "", errors.New("authority contains a forbidden delimiter")
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil || host == "" || portText == "" {
		return "", errors.New("authority must be host:port")
	}
	if host != strings.ToLower(host) || strings.HasSuffix(host, ".") || net.ParseIP(host) != nil {
		return "", errors.New("authority host is not a canonical DNS name")
	}
	// macOS resolves decimal, octal, and hexadecimal single labels and
	// shortened dotted numeric forms as IPv4 even though net.ParseIP rejects
	// them. Require a multi-label DNS name with at least one nonnumeric label
	// before the resolver sees it.
	labels := strings.Split(host, ".")
	hasNonnumericLabel := false
	for _, label := range labels {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("authority host is not a canonical DNS name")
		}
		labelNumeric := true
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", errors.New("authority host is not a canonical DNS name")
			}
			if r < '0' || r > '9' {
				labelNumeric = false
			}
		}
		if !labelNumeric {
			hasNonnumericLabel = true
		}
	}
	if len(labels) < 2 || !hasNonnumericLabel {
		return "", errors.New("authority host is not a canonical DNS name")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return "", errors.New("authority port is not canonical")
	}
	return net.JoinHostPort(host, portText), nil
}

func proxyEnvironment(proxyURL string) []string {
	return []string{
		"HTTP_PROXY=" + proxyURL,
		"HTTPS_PROXY=" + proxyURL,
		"http_proxy=" + proxyURL,
		"https_proxy=" + proxyURL,
		"NO_PROXY=",
		"no_proxy=",
	}
}

func proxyAddress(proxyURL string) (string, error) {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Host == "" || u.Path != "" {
		return "", errors.New("invalid proxy URL")
	}
	return u.Host, nil
}

func (b *Backend) prepareProviderEgress(ctx context.Context, hs HandoffSpec, names handoffNames, st *runState) (NetworkReport, string, error) {
	st.network.attempted = true
	labels := append(runLabels(hs.RunID), st.ownershipLabel)
	if err := b.rt.CreateNetwork(ctx, names.Network, slices.Clone(labels)); err != nil {
		return NetworkReport{}, "", failf(CheckAgentEgress, "create provider network: %v", err)
	}
	st.network.owned = true
	report, err := b.rt.InspectNetwork(ctx, names.Network)
	if err != nil {
		return NetworkReport{}, "", failf(CheckAgentEgress, "inspect provider network: %v", err)
	}
	if report.Name != names.Network {
		return NetworkReport{}, "", failf(CheckAgentEgress, "provider network inspection identified the wrong network")
	}
	if report.Mode != NetworkHostOnly {
		return NetworkReport{}, "", failf(CheckAgentEgress, "provider network is not host-only")
	}
	st.network.fingerprint, err = ownedFingerprint(report.CreationDate, report.Labels, report.LabelsObserved, st.ownershipLabel)
	if err != nil {
		return NetworkReport{}, "", failf(CheckAgentEgress, "provider network ownership is unproven: %v", err)
	}
	proxy, err := startConnectProxy(
		ctx,
		report.IPv4Gateway,
		report.IPv4Subnet,
		b.cfg.ProviderEndpoints,
		b.cfg.EgressProxyTimeout,
		b.cfg.EgressDialContext,
		b.cfg.Now,
	)
	if err != nil {
		return NetworkReport{}, "", failf(CheckAgentEgress, "start provider proxy: %v", err)
	}
	st.proxy = proxy
	return report, proxy.URL(), nil
}
