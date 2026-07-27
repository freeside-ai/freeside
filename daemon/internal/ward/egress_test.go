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
	"strings"
	"testing"
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
	proxy, err := startConnectProxy(context.Background(), "127.0.0.1", "127.0.0.0/24", []string{allowed}, time.Second, dial)
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
