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
)

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

var errClientHelloCaptured = errors.New("TLS ClientHello captured")

// connectProxy is the daemon-side half of provider_only. The writer sits on a
// host-only runtime network, so its only route beyond that network is this
// CONNECT-only listener on the network's host gateway. Exact authorities are
// admitted; ordinary HTTP, alternate ports, IP literals, and every undeclared
// destination are refused.
type connectProxy struct {
	listener  net.Listener
	url       string
	allowed   map[string]struct{}
	clientNet *net.IPNet
	dial      dialContextFunc
	timeout   time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	sem    chan struct{}
	wg     sync.WaitGroup

	mu     sync.Mutex
	err    error
	active map[net.Conn]struct{}
}

func startConnectProxy(parent context.Context, gateway, subnet string, allowed []string, timeout time.Duration, dial dialContextFunc) (*connectProxy, error) {
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
	ctx, cancel := context.WithCancel(parent)
	p := &connectProxy{
		listener: listener,
		url: "http://" + net.JoinHostPort(
			gateway,
			strconv.Itoa(listener.Addr().(*net.TCPAddr).Port),
		),
		allowed:   make(map[string]struct{}, len(allowed)),
		clientNet: clientNet,
		dial:      dial,
		timeout:   timeout,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		sem:       make(chan struct{}, maxProxyConnections),
		active:    make(map[net.Conn]struct{}),
	}
	for _, authority := range allowed {
		p.allowed[authority] = struct{}{}
	}
	go p.serve()
	return p, nil
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
	if _, ok := p.allowed[authority]; !ok {
		_ = writeProxyResponse(client, http.StatusForbidden)
		return
	}
	dialCtx, cancel := context.WithTimeout(p.ctx, p.timeout)
	upstream, err := p.dial(dialCtx, "tcp", authority)
	cancel()
	if err != nil {
		_ = writeProxyResponse(client, http.StatusBadGateway)
		return
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
	go func() {
		_, _ = io.Copy(client, upstream)
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
	if err != nil || u.Scheme != "http" || u.Host == "" || u.Path != "" {
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
	)
	if err != nil {
		return NetworkReport{}, "", failf(CheckAgentEgress, "start provider proxy: %v", err)
	}
	st.proxy = proxy
	return report, proxy.URL(), nil
}
