package buildproxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

var (
	loopbackGateway = netip.MustParseAddr("127.0.0.1")
	loopbackClients = netip.MustParsePrefix("127.0.0.0/8")
)

// startLoopback serves test clients, which arrive from loopback, and lets the
// proxy dial loopback test upstreams unless the test asks for the real
// destination policy.
func startLoopback(t *testing.T, control dialControl) *Proxy {
	t.Helper()
	proxy, err := serve(loopbackGateway, loopbackClients, control)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return proxy
}

func proxiedClient(t *testing.T, proxy *Proxy, tlsConfig *tls.Config) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(proxy.URL())
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: tlsConfig}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func get(t *testing.T, client *http.Client, target string) (int, string) {
	t.Helper()
	resp, err := client.Get(target) //nolint:noctx // test request against a local server
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// connectStatus issues a raw CONNECT and returns the proxy's status line
// response, keeping the connection open for the caller.
func connectStatus(t *testing.T, proxy *Proxy, authority string) (net.Conn, *http.Response) {
	t.Helper()
	proxyURL, _ := url.Parse(proxy.URL())
	conn, err := net.Dial("tcp", proxyURL.Host) //nolint:noctx // test dial to a local listener
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return conn, resp
}

func TestStartValidatesTheReportedNetwork(t *testing.T) {
	t.Parallel()
	valid := BuildNetwork{Name: "default", Mode: "nat", IPv4Gateway: "192.168.64.1", IPv4Subnet: "192.168.64.0/24"}
	for name, mutate := range map[string]func(*BuildNetwork){
		"another network":        func(n *BuildNetwork) { n.Name = "freeside-writer-1" },
		"host-only mode":         func(n *BuildNetwork) { n.Mode = "host_only" },
		"empty gateway":          func(n *BuildNetwork) { n.IPv4Gateway = "" },
		"IPv6 gateway":           func(n *BuildNetwork) { n.IPv4Gateway = "fd00::1" },
		"malformed subnet":       func(n *BuildNetwork) { n.IPv4Subnet = "192.168.64.0" },
		"gateway outside subnet": func(n *BuildNetwork) { n.IPv4Gateway = "192.168.65.1" },
		"public subnet":          func(n *BuildNetwork) { n.IPv4Gateway, n.IPv4Subnet = "8.8.8.1", "8.8.8.0/24" },
		"everything":             func(n *BuildNetwork) { n.IPv4Subnet = "0.0.0.0/0" },
		"wider than the bound":   func(n *BuildNetwork) { n.IPv4Gateway, n.IPv4Subnet = "10.0.0.1", "10.0.0.0/8" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			network := valid
			mutate(&network)
			proxy, err := Start(network)
			if err == nil {
				_ = proxy.Close()
				t.Fatalf("Start(%+v) succeeded, want a refusal", network)
			}
		})
	}

	proxy, err := Start(valid)
	if err != nil {
		t.Fatalf("Start with the default vmnet network: %v", err)
	}
	if !strings.HasPrefix(proxy.URL(), "http://192.168.64.1:") {
		t.Errorf("URL = %q, want the gateway address", proxy.URL())
	}
	if err := proxy.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

func TestForwardsPlainHTTP(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			t.Errorf("upstream saw X-Forwarded-For %q, want the client address withheld", forwarded)
		}
		_, _ = io.WriteString(w, "index")
	}))
	t.Cleanup(upstream.Close)

	status, body := get(t, proxiedClient(t, startLoopback(t, nil), nil), upstream.URL)
	if status != http.StatusOK || body != "index" {
		t.Fatalf("got %d %q, want 200 \"index\"", status, body)
	}
}

func TestTunnelsConnect(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "tarball")
	}))
	t.Cleanup(upstream.Close)

	client := proxiedClient(t, startLoopback(t, nil), upstream.Client().Transport.(*http.Transport).TLSClientConfig)
	status, body := get(t, client, upstream.URL)
	if status != http.StatusOK || body != "tarball" {
		t.Fatalf("got %d %q, want 200 \"tarball\"", status, body)
	}
}

func TestRefusesNonPublicDestinations(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the proxy reached a loopback upstream")
	}))
	t.Cleanup(upstream.Close)
	proxy := startLoopback(t, forwardableDestinationOnly)

	if status, _ := get(t, proxiedClient(t, proxy, nil), upstream.URL); status != http.StatusBadGateway {
		t.Errorf("plain HTTP to loopback: status %d, want 502", status)
	}
	// "localhost" exercises the post-resolution check, not literal matching.
	_, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	for _, authority := range []string{upstream.Listener.Addr().String(), "localhost:" + port} {
		_, resp := connectStatus(t, proxy, authority)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("CONNECT %s: status %d, want 502", authority, resp.StatusCode)
		}
	}
}

func TestDialsIPv4Only(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp6", "[::1]:0") //nolint:noctx // test listener
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		if conn, err := listener.Accept(); err == nil {
			t.Error("the proxy dialed an IPv6 destination")
			_ = conn.Close()
		}
	}()

	// No destination policy: only the address family keeps this one out.
	_, resp := connectStatus(t, startLoopback(t, nil), listener.Addr().String())
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("CONNECT to an IPv6 literal: status %d, want 502", resp.StatusCode)
	}
}

func TestRejectsRequestsThatAreNotProxyRequests(t *testing.T) {
	t.Parallel()
	proxy := startLoopback(t, nil)
	resp, err := http.Get(proxy.URL() + "/") //nolint:noctx // test request against a local server
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("origin-form request: status %d, want 400", resp.StatusCode)
	}
}

func TestDropsClientsOutsideTheBuildNetwork(t *testing.T) {
	t.Parallel()
	proxy, err := serve(loopbackGateway, netip.MustParsePrefix("192.0.2.0/24"), nil)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	proxyURL, _ := url.Parse(proxy.URL())
	conn, err := net.Dial("tcp", proxyURL.Host) //nolint:noctx // test dial to a local listener
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, _ = io.WriteString(conn, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if response, err := io.ReadAll(conn); err == nil && len(response) > 0 {
		t.Fatalf("unadmitted client got a response: %q", response)
	}
}

func TestDropsClientsThatDidNotAddressTheGateway(t *testing.T) {
	t.Parallel()
	// The admitted gateway is a second loopback address, so a client from the
	// admitted subnet that dials the proxy at 127.0.0.1 models a LAN peer
	// reaching the host at an address other than the build gateway.
	proxy, err := serve(netip.MustParseAddr("127.0.0.2"), loopbackClients, nil)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	proxyURL, _ := url.Parse(proxy.URL())
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", proxyURL.Port())) //nolint:noctx // test dial to a local listener
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, _ = io.WriteString(conn, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if response, err := io.ReadAll(conn); err == nil && len(response) > 0 {
		t.Fatalf("a client that did not address the gateway got a response: %q", response)
	}
}

func TestCloseSeversOpenTunnels(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // test listener
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		// Hold the upstream open so only Close can end the tunnel.
		if conn, err := listener.Accept(); err == nil {
			_, _ = io.Copy(io.Discard, conn)
			_ = conn.Close()
		}
	}()

	proxy, err := serve(loopbackGateway, loopbackClients, nil)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	conn, resp := connectStatus(t, proxy, listener.Addr().String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: status %d, want 200", resp.StatusCode)
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatalf("tunnel survived Close: read %d bytes", n)
	}
}

func TestForwardable(t *testing.T) {
	t.Parallel()
	for address, want := range map[string]bool{
		"1.1.1.1":              true,
		"2606:4700:4700::1111": true,
		"127.0.0.1":            false,
		"::1":                  false,
		"::ffff:127.0.0.1":     false,
		"0.0.0.0":              false,
		"10.64.0.1":            false,
		"172.16.0.1":           false,
		"192.168.64.1":         false,
		"::ffff:192.168.64.1":  false,
		"169.254.169.254":      false,
		"100.100.100.100":      false,
		"fe80::1":              false,
		"fd7a:115c:a1e0::1":    false,
		"224.0.0.1":            false,
		"255.255.255.255":      false,
	} {
		if got := forwardable(netip.MustParseAddr(address)); got != want {
			t.Errorf("forwardable(%s) = %v, want %v", address, got, want)
		}
	}
}
