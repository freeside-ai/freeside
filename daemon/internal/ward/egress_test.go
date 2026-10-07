package ward

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCanonicalAuthority(t *testing.T) {
	t.Parallel()
	if got, err := canonicalAuthority("api.anthropic.com:443"); err != nil || got != "api.anthropic.com:443" {
		t.Fatalf("canonicalAuthority = %q, %v", got, err)
	}
	for _, value := range []string{
		"", "api.anthropic.com", "API.anthropic.com:443", "api.anthropic.com.:443",
		"api.anthropic.com:0443", "127.0.0.1:443", "[::1]:443",
		"127.1:443", "127.0.1:443", "2130706433:443", "017700000001:443",
		"0x7f000001:443", "localhost:443", "user@api.anthropic.com:443",
		"*.anthropic.com:443",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := canonicalAuthority(value); err == nil {
				t.Fatalf("canonicalAuthority(%q) accepted", value)
			}
		})
	}
}

func TestConnectProxyExactAllowlist(t *testing.T) {
	const allowed = "provider.example:443"
	payload := strings.Repeat("p", maxProxyHeaderBytes*2)
	upstreamResult := make(chan error, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err == nil && string(body) != payload {
			err = fmt.Errorf("upstream payload differed")
		}
		if err == nil {
			_, err = w.Write([]byte("pong"))
		}
		upstreamResult <- err
	}))
	defer upstream.Close()

	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != allowed {
			return nil, fmt.Errorf("unexpected dial")
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", upstream.Listener.Addr().String())
	}
	proxy, err := startConnectProxy(context.Background(), "127.0.0.1", "127.0.0.0/24", []string{allowed}, time.Second, dial, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := proxy.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	dialProxy := func() net.Conn {
		t.Helper()
		address, err := proxyAddress(proxy.URL())
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.DialTimeout("tcp4", address, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	readStatus := func(conn net.Conn) string {
		t.Helper()
		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		for {
			header, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if header == "\r\n" {
				break
			}
		}
		return strings.TrimSpace(line)
	}
	assertStatus := func(request, want string) {
		t.Helper()
		conn := dialProxy()
		defer func() { _ = conn.Close() }()
		if _, err := fmt.Fprint(conn, request); err != nil {
			t.Fatal(err)
		}
		if got := readStatus(conn); got != want {
			t.Fatalf("status = %q, want %q", got, want)
		}
	}

	allowedConn := dialProxy()
	if _, err := fmt.Fprint(allowedConn, "CONNECT provider.example:443 HTTP/1.1\r\nHost: provider.example:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if got := readStatus(allowedConn); got != "HTTP/1.1 200 OK" {
		t.Fatalf("allowed status = %q", got)
	}
	tlsConn := tls.Client(allowedConn, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         "provider.example",
		InsecureSkipVerify: true, //nolint:gosec // test server certificate is intentionally local
	})
	request, err := http.NewRequest(http.MethodPost, "https://provider.example/", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(tlsConn), request)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(reply) != "pong" {
		t.Fatalf("tunnel reply = %q, want pong", reply)
	}
	if err := <-upstreamResult; err != nil {
		t.Fatalf("upstream tunnel: %v", err)
	}
	_ = tlsConn.Close()
	_ = allowedConn.Close()

	mismatchedConn := dialProxy()
	if _, err := fmt.Fprint(mismatchedConn, "CONNECT provider.example:443 HTTP/1.1\r\nHost: provider.example:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if got := readStatus(mismatchedConn); got != "HTTP/1.1 200 OK" {
		t.Fatalf("mismatched-SNI CONNECT status = %q", got)
	}
	mismatchedTLS := tls.Client(mismatchedConn, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         "other.example",
		InsecureSkipVerify: true, //nolint:gosec // no handshake should complete
	})
	if err := mismatchedTLS.Handshake(); err == nil {
		t.Fatal("mismatched TLS server name passed through the allowed CONNECT authority")
	}
	_ = mismatchedTLS.Close()
	_ = mismatchedConn.Close()

	assertStatus("CONNECT other.example:443 HTTP/1.1\r\nHost: other.example:443\r\n\r\n", "HTTP/1.1 403 Forbidden")
	assertStatus("GET http://provider.example/ HTTP/1.1\r\nHost: provider.example\r\n\r\n", "HTTP/1.1 400 Bad Request")
	assertStatus("CONNECT provider.example:443 HTTP/1.1\r\nHost: provider.example:443\r\nX-Fill: "+
		strings.Repeat("x", maxProxyHeaderBytes)+"\r\n\r\n", "HTTP/1.1 400 Bad Request")
}

func TestConnectProxyClientSubnetAdmission(t *testing.T) {
	t.Parallel()
	_, subnet, err := net.ParseCIDR("192.168.128.0/24")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &connectProxy{clientNet: subnet}
	if !proxy.clientAllowed(&net.TCPAddr{IP: net.ParseIP("192.168.128.2"), Port: 1234}) {
		t.Fatal("per-run subnet client rejected")
	}
	if proxy.clientAllowed(&net.TCPAddr{IP: net.ParseIP("192.168.129.2"), Port: 1234}) {
		t.Fatal("client from another subnet accepted")
	}
	if proxy.clientAllowed(&net.UnixAddr{Name: "/tmp/not-a-tcp-client", Net: "unix"}) {
		t.Fatal("non-TCP client accepted")
	}
}

func TestConnectProxyCloseInterruptsPartialClientHello(t *testing.T) {
	proxySide, upstreamSide := net.Pipe()
	defer func() { _ = upstreamSide.Close() }()
	dial := func(context.Context, string, string) (net.Conn, error) {
		return proxySide, nil
	}
	proxy, err := startConnectProxy(
		context.Background(),
		"127.0.0.1",
		"127.0.0.0/24",
		[]string{"provider.example:443"},
		time.Hour,
		dial,
		time.Now,
	)
	if err != nil {
		t.Fatal(err)
	}
	address, err := proxyAddress(proxy.URL())
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.DialTimeout("tcp4", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if _, err := fmt.Fprint(client, "CONNECT provider.example:443 HTTP/1.1\r\nHost: provider.example:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := client.Write([]byte{0x16, 0x03, 0x03, 0x01}); err != nil {
		t.Fatal(err)
	}

	closed := make(chan error, 1)
	go func() { closed <- proxy.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close waited for the one-hour proxy timeout instead of interrupting the partial ClientHello")
	}
}

func TestConnectProxyRejectsInvalidNetworkMetadata(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, gateway, subnet string
	}{
		{"invalid gateway", "not-an-ip", "127.0.0.0/24"},
		{"invalid subnet", "127.0.0.1", "not-a-subnet"},
		{"broad subnet", "127.0.0.1", "0.0.0.0/0"},
		{"gateway outside subnet", "127.0.1.1", "127.0.0.0/24"},
		{"gateway not first host", "127.0.0.2", "127.0.0.0/24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, err := startConnectProxy(
				context.Background(),
				tc.gateway,
				tc.subnet,
				[]string{"provider.example:443"},
				time.Second,
				nil,
				time.Now,
			)
			if err == nil {
				_ = proxy.Close()
				t.Fatal("invalid network metadata accepted")
			}
		})
	}
}

func TestSameEnvironmentExactByKey(t *testing.T) {
	want := []string{"PATH=/bin", "A=1", "B=two=parts"}
	for _, got := range [][]string{
		{"B=two=parts", "PATH=/bin", "A=1"},
		{"A=1", "B=two=parts", "PATH=/bin"},
	} {
		if !sameEnvironment(got, want) {
			t.Errorf("permutation %q did not match", got)
		}
	}
	for _, got := range [][]string{
		{"PATH=/bin", "A=1"},
		{"PATH=/bin", "A=1", "B=two=parts", "C=3"},
		{"PATH=/bin", "A=1", "A=1", "B=two=parts"},
		{"PATH=/bin", "A=other", "B=two=parts"},
	} {
		if sameEnvironment(got, want) {
			t.Errorf("non-exact environment %q matched", got)
		}
	}
}

// The stall heartbeat is the provider's response bytes, observed in the
// daemon: a writer that uploads while the provider stays silent never
// refreshes it, and one provider byte does.
func TestConnectProxyHeartbeatCountsOnlyProviderBytes(t *testing.T) {
	proxySide, upstreamSide := net.Pipe()
	defer func() { _ = upstreamSide.Close() }()
	dial := func(context.Context, string, string) (net.Conn, error) {
		return proxySide, nil
	}
	stamp := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	proxy, err := startConnectProxy(
		context.Background(), "127.0.0.1", "127.0.0.0/24",
		[]string{"provider.example:443"}, time.Second, dial,
		func() time.Time { return stamp },
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()
	address, err := proxyAddress(proxy.URL())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp4", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprint(conn, "CONNECT provider.example:443 HTTP/1.1\r\nHost: provider.example:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "HTTP/1.1 200 OK" {
		t.Fatalf("CONNECT status = %q, %v", line, err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	// The writer's ClientHello is an upload: the provider receives it and
	// the heartbeat does not move.
	handshake := make(chan error, 1)
	go func() {
		handshake <- tls.Client(conn, &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: "provider.example",
		}).Handshake()
	}()
	uploaded := make([]byte, 5)
	if _, err := io.ReadFull(upstreamSide, uploaded); err != nil {
		t.Fatalf("provider read the upload: %v", err)
	}
	if got := proxy.LastProviderByte(); !got.IsZero() {
		t.Fatalf("heartbeat after writer upload = %v, want zero", got)
	}

	// One provider byte, then the provider hangs up, which ends the writer's
	// handshake; the heartbeat was stamped before the byte was forwarded.
	if _, err := upstreamSide.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = upstreamSide.Close()
	if err := <-handshake; err == nil {
		t.Fatal("handshake against a one-byte provider reply succeeded")
	}
	if got := proxy.LastProviderByte(); !got.Equal(stamp) {
		t.Fatalf("heartbeat after provider byte = %v, want %v", got, stamp)
	}
}

// registryProxy is a provider_registry proxy over one provider endpoint and
// one declared registry, with a scripted resolver and a recording dialer that
// reaches a local TLS server in place of every upstream.
type registryProxy struct {
	t        *testing.T
	proxy    *connectProxy
	upstream *httptest.Server

	mu      sync.Mutex
	addrs   []netip.Addr
	lookErr error
	looked  []string
	dialed  []string
	refuse  map[string]bool
}

const (
	testProviderAuthority = "provider.example:443"
	testRegistryHost      = "registry.npmjs.org"
	testRegistryAuthority = testRegistryHost + ":443"
	testRegistryAddr      = "104.16.3.34"
)

func newRegistryProxy(t *testing.T) *registryProxy {
	t.Helper()
	rp := &registryProxy{
		t:      t,
		addrs:  []netip.Addr{netip.MustParseAddr(testRegistryAddr)},
		refuse: map[string]bool{},
	}
	rp.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	t.Cleanup(rp.upstream.Close)
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		rp.mu.Lock()
		rp.dialed = append(rp.dialed, address)
		refuse := rp.refuse[address]
		rp.mu.Unlock()
		if refuse {
			return nil, fmt.Errorf("scripted dial refusal")
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", rp.upstream.Listener.Addr().String())
	}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		rp.mu.Lock()
		defer rp.mu.Unlock()
		rp.looked = append(rp.looked, host)
		return slices.Clone(rp.addrs), rp.lookErr
	}
	proxy, err := startRegistryConnectProxy(
		context.Background(), "127.0.0.1", "127.0.0.0/24",
		[]string{testProviderAuthority},
		registryRoutes{authorities: []string{testRegistryAuthority}, lookup: lookup},
		time.Second, dial, time.Now, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	rp.proxy = proxy
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return rp
}

func (rp *registryProxy) resolveTo(err error, addrs ...string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.lookErr = err
	rp.addrs = rp.addrs[:0]
	for _, addr := range addrs {
		rp.addrs = append(rp.addrs, netip.MustParseAddr(addr))
	}
	rp.looked, rp.dialed = nil, nil
}

func (rp *registryProxy) calls() (looked, dialed []string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return slices.Clone(rp.looked), slices.Clone(rp.dialed)
}

// connect sends one CONNECT and returns the status line with the connection
// positioned at the tunnel.
func (rp *registryProxy) connect(authority string) (net.Conn, string) {
	rp.t.Helper()
	address, err := proxyAddress(rp.proxy.URL())
	if err != nil {
		rp.t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp4", address, time.Second)
	if err != nil {
		rp.t.Fatal(err)
	}
	rp.t.Cleanup(func() { _ = conn.Close() })
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority); err != nil {
		rp.t.Fatal(err)
	}
	// One byte at a time, so nothing of the tunnel is buffered away from the
	// TLS client that takes the connection over.
	var head []byte
	for !strings.HasSuffix(string(head), "\r\n\r\n") {
		var b [1]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			rp.t.Fatalf("read CONNECT response: %v", err)
		}
		head = append(head, b[0])
	}
	status, _, _ := strings.Cut(string(head), "\r\n")
	return conn, status
}

// get runs one HTTPS request through an established tunnel under serverName.
func (rp *registryProxy) get(conn net.Conn, serverName string) (string, error) {
	rp.t.Helper()
	tlsConn := tls.Client(conn, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         serverName,
		InsecureSkipVerify: true, //nolint:gosec // test server certificate is intentionally local
	})
	request, err := http.NewRequest(http.MethodGet, "https://"+serverName+"/", nil)
	if err != nil {
		rp.t.Fatal(err)
	}
	if err := request.Write(tlsConn); err != nil {
		return "", err
	}
	response, err := http.ReadResponse(bufio.NewReader(tlsConn), request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	return string(body), err
}

func TestConnectProxyRegistryAllowlist(t *testing.T) {
	rp := newRegistryProxy(t)

	providers, registries := rp.proxy.Allowlist()
	if !slices.Equal(providers, []string{testProviderAuthority}) ||
		!slices.Equal(registries, []string{testRegistryAuthority}) {
		t.Fatalf("Allowlist = %v, %v; want the provider endpoint and the registry authority", providers, registries)
	}

	// The registry is resolved by the proxy and dialed at the address it
	// checked, never by name.
	conn, status := rp.connect(testRegistryAuthority)
	if status != "HTTP/1.1 200 OK" {
		t.Fatalf("declared registry status = %q", status)
	}
	if reply, err := rp.get(conn, testRegistryHost); err != nil || reply != "pong" {
		t.Fatalf("registry tunnel reply = %q, %v", reply, err)
	}
	looked, dialed := rp.calls()
	if !slices.Equal(looked, []string{testRegistryHost}) ||
		!slices.Equal(dialed, []string{testRegistryAddr + ":443"}) {
		t.Fatalf("registry lookups = %v, dials = %v; want one lookup of the host and one dial of its address", looked, dialed)
	}
	if got := rp.proxy.LastProviderByte(); !got.IsZero() {
		t.Fatalf("heartbeat after registry bytes = %v, want zero: a registry is not the provider", got)
	}

	// A provider endpoint keeps the dial-by-name path and is never resolved
	// by the proxy.
	rp.resolveTo(nil, testRegistryAddr)
	conn, status = rp.connect(testProviderAuthority)
	if status != "HTTP/1.1 200 OK" {
		t.Fatalf("provider status = %q", status)
	}
	if reply, err := rp.get(conn, "provider.example"); err != nil || reply != "pong" {
		t.Fatalf("provider tunnel reply = %q, %v", reply, err)
	}
	looked, dialed = rp.calls()
	if len(looked) != 0 || !slices.Equal(dialed, []string{testProviderAuthority}) {
		t.Fatalf("provider lookups = %v, dials = %v; want no lookup and a dial by name", looked, dialed)
	}
	if got := rp.proxy.LastProviderByte(); got.IsZero() {
		t.Fatal("heartbeat still zero after provider bytes")
	}

	// A shared-CDN neighbor: the CONNECT names the declared registry and the
	// ClientHello names another tenant at the same address.
	conn, status = rp.connect(testRegistryAuthority)
	if status != "HTTP/1.1 200 OK" {
		t.Fatalf("neighbor CONNECT status = %q", status)
	}
	if _, err := rp.get(conn, "neighbor.npmjs.org"); err == nil {
		t.Fatal("a TLS server name other than the declared registry passed through its CONNECT authority")
	}

	for _, authority := range []string{
		"undeclared.example:443",
		"registry.npmjs.org:8443",
		"sub.registry.npmjs.org:443",
		"npmjs.org:443",
	} {
		if _, status := rp.connect(authority); status != "HTTP/1.1 403 Forbidden" {
			t.Errorf("CONNECT %s status = %q, want 403", authority, status)
		}
	}
}

func TestConnectProxyRegistryRequiresPublicAddresses(t *testing.T) {
	rp := newRegistryProxy(t)
	for _, tc := range []struct {
		name  string
		addrs []string
	}{
		{"loopback", []string{"127.0.0.1"}},
		{"private", []string{"10.0.0.5"}},
		{"public and private", []string{testRegistryAddr, "192.168.1.10"}},
		{"private and public", []string{"172.16.0.9", testRegistryAddr}},
		{"mapped private", []string{"::ffff:10.0.0.5"}},
		{"carrier-grade NAT", []string{"100.64.0.1"}},
		{"IPv4 link-local", []string{"169.254.169.254"}},
		{"IPv6 loopback", []string{"::1"}},
		{"IPv6 unique local", []string{"fd00::1"}},
		{"IPv6 link-local", []string{"fe80::1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rp.resolveTo(nil, tc.addrs...)
			if _, status := rp.connect(testRegistryAuthority); status != "HTTP/1.1 403 Forbidden" {
				t.Errorf("status = %q, want 403", status)
			}
			if _, dialed := rp.calls(); len(dialed) != 0 {
				t.Errorf("dialed %v for a registry with a non-public address", dialed)
			}
		})
	}

	t.Run("resolver error", func(t *testing.T) {
		rp.resolveTo(fmt.Errorf("no such host"))
		if _, status := rp.connect(testRegistryAuthority); status != "HTTP/1.1 502 Bad Gateway" {
			t.Errorf("status = %q, want 502", status)
		}
		if _, dialed := rp.calls(); len(dialed) != 0 {
			t.Errorf("dialed %v after a resolver error", dialed)
		}
	})
	t.Run("no address", func(t *testing.T) {
		rp.resolveTo(nil)
		if _, status := rp.connect(testRegistryAuthority); status != "HTTP/1.1 502 Bad Gateway" {
			t.Errorf("status = %q, want 502", status)
		}
	})
	t.Run("next address after a failed dial", func(t *testing.T) {
		rp.resolveTo(nil, "2606:4700::6810:322", testRegistryAddr)
		rp.mu.Lock()
		rp.refuse["[2606:4700::6810:322]:443"] = true
		rp.mu.Unlock()
		conn, status := rp.connect(testRegistryAuthority)
		if status != "HTTP/1.1 200 OK" {
			t.Fatalf("status = %q, want 200 through the second address", status)
		}
		if reply, err := rp.get(conn, testRegistryHost); err != nil || reply != "pong" {
			t.Fatalf("tunnel reply = %q, %v", reply, err)
		}
		if _, dialed := rp.calls(); !slices.Equal(dialed, []string{"[2606:4700::6810:322]:443", testRegistryAddr + ":443"}) {
			t.Errorf("dials = %v, want both addresses in resolver order", dialed)
		}
	})
	t.Run("every dial fails", func(t *testing.T) {
		rp.resolveTo(nil, testRegistryAddr)
		rp.mu.Lock()
		rp.refuse[testRegistryAddr+":443"] = true
		rp.mu.Unlock()
		if _, status := rp.connect(testRegistryAuthority); status != "HTTP/1.1 502 Bad Gateway" {
			t.Errorf("status = %q, want 502", status)
		}
	})
}

// An authority declared as a registry that is also a provider endpoint stays
// on the provider path: it was already admitted, so the declaration adds
// nothing and must not be read back as a registry.
func TestConnectProxyProviderOutranksRegistryDeclaration(t *testing.T) {
	proxy, err := startRegistryConnectProxy(
		context.Background(), "127.0.0.1", "127.0.0.0/24",
		[]string{testProviderAuthority},
		registryRoutes{authorities: []string{testRegistryAuthority, testProviderAuthority}},
		time.Second, nil, time.Now, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()
	providers, registries := proxy.Allowlist()
	if !slices.Equal(providers, []string{testProviderAuthority}) ||
		!slices.Equal(registries, []string{testRegistryAuthority}) {
		t.Fatalf("Allowlist = %v, %v", providers, registries)
	}
}

func TestPublicUnicast(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{
		"104.16.3.34", "1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.0",
		"172.15.255.255", "172.32.0.0", "::ffff:104.16.3.34",
		"2606:4700::6810:322", "2a04:4e42::81", "2001:200::1",
	} {
		if !publicUnicast(netip.MustParseAddr(addr)) {
			t.Errorf("publicUnicast(%s) = false, want true", addr)
		}
	}
	for _, addr := range []string{
		"0.0.0.0", "0.1.2.3", "10.255.255.255", "100.64.0.0", "100.127.255.255",
		"127.0.0.1", "127.255.255.254", "169.254.169.254", "172.16.0.1", "172.31.255.255",
		"192.0.0.8", "192.0.2.1", "192.88.99.1", "192.168.0.1", "198.18.0.1", "198.19.255.255",
		"198.51.100.1", "203.0.113.1", "224.0.0.1", "239.255.255.255", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::ffff:127.0.0.1", "::ffff:10.0.0.5", "::ffff:169.254.169.254",
		"64:ff9b::a00:5", "100::1", "2001::1", "2001:db8::1", "2002:a00:5::1", "3fff::1",
		"fc00::1", "fd12:3456::1", "fe80::1", "fe80::1%en0", "2606:4700::6810:322%en0", "ff02::1",
	} {
		if publicUnicast(netip.MustParseAddr(addr)) {
			t.Errorf("publicUnicast(%s) = true, want false", addr)
		}
	}
	if publicUnicast(netip.Addr{}) {
		t.Error("publicUnicast(zero Addr) = true, want false")
	}
}

// A registry address that never answers must not spend the dial budget: the
// proxy also tries the next one after the stagger, so a working address
// behind several silent ones is reached.
func TestDialRegistryReachesAnAddressBehindSilentOnes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstream, peer := net.Pipe()
		defer func() { _ = upstream.Close() }()
		defer func() { _ = peer.Close() }()
		p := &connectProxy{
			lookup: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{
					netip.MustParseAddr("2606:4700::6810:322"),
					netip.MustParseAddr("2606:4700::6810:422"),
					netip.MustParseAddr("2606:4700::6810:522"),
					netip.MustParseAddr(testRegistryAddr),
				}, nil
			},
			dial: func(ctx context.Context, _, address string) (net.Conn, error) {
				if address == testRegistryAddr+":443" {
					return upstream, nil
				}
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		start := time.Now()
		conn, status := p.dialRegistry(ctx, testRegistryAuthority)
		if conn != upstream || status != http.StatusOK {
			t.Fatalf("dialRegistry = %v, %d; want the working address's connection", conn, status)
		}
		if waited := time.Since(start); waited != 3*registryDialStagger {
			t.Errorf("reached the fourth address after %v, want three staggers (%v)", waited, 3*registryDialStagger)
		}
	})
}

// When no address answers, the dial ends with the CONNECT's own budget and
// reports a gateway failure.
func TestDialRegistryEndsWithTheBudgetWhenNoAddressAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &connectProxy{
			lookup: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{
					netip.MustParseAddr("2606:4700::6810:322"),
					netip.MustParseAddr(testRegistryAddr),
				}, nil
			},
			dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		const budget = 15 * time.Second
		ctx, cancel := context.WithTimeout(t.Context(), budget)
		defer cancel()
		start := time.Now()
		conn, status := p.dialRegistry(ctx, testRegistryAuthority)
		if conn != nil || status != http.StatusBadGateway {
			t.Fatalf("dialRegistry = %v, %d; want no connection and 502", conn, status)
		}
		if waited := time.Since(start); waited != budget {
			t.Errorf("gave up after %v, want the %v budget", waited, budget)
		}
	})
}

// closeRecorder reports whether its connection was closed.
type closeRecorder struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// Two attempts can both connect when the earlier one is slow. The tunnel
// takes the first to finish and closes the other.
func TestDialRegistryClosesAConnectionThatLosesTheRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slowConn, slowPeer := net.Pipe()
		fastConn, fastPeer := net.Pipe()
		defer func() { _ = slowPeer.Close() }()
		defer func() { _ = fastPeer.Close() }()
		slow := &closeRecorder{Conn: slowConn}
		fast := &closeRecorder{Conn: fastConn}
		defer func() { _ = fast.Close() }()
		p := &connectProxy{
			lookup: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{
					netip.MustParseAddr("2606:4700::6810:322"),
					netip.MustParseAddr(testRegistryAddr),
				}, nil
			},
			// The first address connects only after the second attempt has
			// started and won, and ignores the cancellation that follows.
			dial: func(_ context.Context, _, address string) (net.Conn, error) {
				if address == testRegistryAddr+":443" {
					return fast, nil
				}
				time.Sleep(2 * registryDialStagger)
				return slow, nil
			},
		}
		conn, status := p.dialRegistry(t.Context(), testRegistryAuthority)
		if conn != net.Conn(fast) || status != http.StatusOK {
			t.Fatalf("dialRegistry = %v, %d; want the connection that finished first", conn, status)
		}
		if !slow.closed.Load() {
			t.Error("the connection that lost the race was left open")
		}
		if fast.closed.Load() {
			t.Error("the winning connection was closed")
		}
	})
}

// recordingResolver records the one lookup it is asked for.
type recordingResolver struct {
	network, host string
}

func (r *recordingResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	r.network, r.host = network, host
	return []netip.Addr{netip.MustParseAddr(testRegistryAddr)}, nil
}

// A declared registry host is asked of the resolver as a rooted name, so the
// resolver's search list cannot answer for another name.
func TestRootedLookupAsksForTheRootedName(t *testing.T) {
	resolver := &recordingResolver{}
	addrs, err := rootedLookup(resolver)(context.Background(), testRegistryHost)
	if err != nil || !slices.Equal(addrs, []netip.Addr{netip.MustParseAddr(testRegistryAddr)}) {
		t.Fatalf("lookup = %v, %v; want the resolver's answer", addrs, err)
	}
	if resolver.network != "ip" || resolver.host != testRegistryHost+"." {
		t.Errorf("resolver asked for %q over %q, want the rooted name %q over \"ip\"",
			resolver.host, resolver.network, testRegistryHost+".")
	}
}
