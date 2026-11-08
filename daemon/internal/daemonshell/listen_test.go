package daemonshell

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestListenPrivileged(t *testing.T) {
	t.Parallel()
	listener, err := ListenPrivileged("127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPrivileged(loopback): %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close loopback listener: %v", err)
	}

	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "192.0.2.1:0"} {
		t.Run(addr, func(t *testing.T) {
			if listener, err := ListenPrivileged(addr); err == nil {
				_ = listener.Close()
				t.Fatalf("ListenPrivileged(%q) accepted a wildcard or arbitrary non-loopback address", addr)
			}
		})
	}
}

func TestListenPrivilegedRejectsBeforeBind(t *testing.T) {
	t.Parallel()
	bindCalled := false
	_, err := listenPrivilegedWith(
		"0.0.0.0:0",
		func(string, *net.TCPAddr) (net.Listener, error) {
			bindCalled = true
			return nil, errors.New("unsafe bind was attempted")
		},
		func() ([]netip.Addr, error) {
			t.Fatal("wildcard validation queried Tailscale")
			return nil, nil
		},
	)
	if err == nil {
		t.Fatal("listenPrivilegedWith accepted a wildcard address")
	}
	if bindCalled {
		t.Fatal("listenPrivilegedWith called the binder before rejecting a wildcard address")
	}
}

// TestConnectionModeOfBoundListener pins the pairing-facts derivation to
// the two address classes listenPrivilegedWith admits, and refuses anything
// else rather than inventing a mode.
func TestConnectionModeOfBoundListener(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		addr string
		want domain.ConnectionMode
	}{
		{addr: "127.0.0.1:8443", want: domain.ConnectionLoopback},
		{addr: "[::1]:8443", want: domain.ConnectionLoopback},
		{addr: "100.64.0.7:8443", want: domain.ConnectionTailscale},
		{addr: "[fd7a:115c:a1e0::7]:8443", want: domain.ConnectionTailscale},
		{addr: "192.0.2.7:8443"},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			resolved, err := net.ResolveTCPAddr("tcp", tc.addr)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ConnectionModeOf(resolved)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ConnectionModeOf(%q) = %q, want an error", tc.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConnectionModeOf(%q): %v", tc.addr, err)
			}
			if got != tc.want {
				t.Errorf("ConnectionModeOf(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
	if _, err := ConnectionModeOf(&net.UnixAddr{Name: "/tmp/sock", Net: "unix"}); err == nil {
		t.Fatal("ConnectionModeOf accepted a non-TCP address")
	}
}

type listenerStub struct {
	addr   net.Addr
	closed bool
}

func (*listenerStub) Accept() (net.Conn, error) {
	return nil, errors.New("stub listener does not accept")
}
func (l *listenerStub) Close() error   { l.closed = true; return nil }
func (l *listenerStub) Addr() net.Addr { return l.addr }

func TestSameHostReadinessFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		primary        string
		twin           string
		wantPrimaryURL string
	}{
		{name: "Tailscale IPv4", primary: "100.64.0.7:8443", twin: "127.0.0.1:8443", wantPrimaryURL: "http://100.64.0.7:8443"},
		{name: "Tailscale IPv6", primary: "[fd7a:115c:a1e0::7]:8443", twin: "127.0.0.1:8443", wantPrimaryURL: "http://[fd7a:115c:a1e0::7]:8443"},
		{name: "loopback only", primary: "127.0.0.1:8443", wantPrimaryURL: "http://127.0.0.1:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			primary, err := net.ResolveTCPAddr("tcp", tc.primary)
			if err != nil {
				t.Fatal(err)
			}
			d := &Shell{listener: &listenerStub{addr: primary}, pairingCode: "test-code"}
			if tc.twin != "" {
				twin, err := net.ResolveTCPAddr("tcp", tc.twin)
				if err != nil {
					t.Fatal(err)
				}
				d.loopbackTwin = &listenerStub{addr: twin}
			}
			if got := d.Readiness().APIURL; got != tc.wantPrimaryURL {
				t.Fatalf("primary readiness URL = %q, want %q", got, tc.wantPrimaryURL)
			}
			stateDir := t.TempDir()
			if err := publishReadiness(stateDir, d.sameHostReadiness()); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(stateDir, ReadinessFileName)) //nolint:gosec // test-owned temporary path
			if err != nil {
				t.Fatal(err)
			}
			var ready Readiness
			if err := json.Unmarshal(body, &ready); err != nil {
				t.Fatal(err)
			}
			if ready.APIURL != "http://127.0.0.1:8443" || ready.PairingCode != "test-code" {
				t.Fatalf("same-host file readiness = %+v", ready)
			}
			parsed, err := url.Parse(ready.APIURL)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() != "8443" ||
				parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
				t.Fatalf("file URL is not a same-host app URL: %q", ready.APIURL)
			}
		})
	}
}

func TestListenPrivilegedAcceptsOnlyTailscaleOwnedAddresses(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"100.64.0.7:8443", "[fd7a:115c:a1e0::7]:8443"} {
		t.Run(addr, func(t *testing.T) {
			resolved, err := net.ResolveTCPAddr("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			tailscaleIP, ok := netip.AddrFromSlice(resolved.IP)
			if !ok {
				t.Fatalf("parse test Tailscale address %q", resolved.IP)
			}
			stub := &listenerStub{addr: &net.TCPAddr{IP: resolved.IP, Port: resolved.Port}}
			listener, err := listenPrivilegedWith(
				addr,
				func(_ string, _ *net.TCPAddr) (net.Listener, error) { return stub, nil },
				func() ([]netip.Addr, error) { return []netip.Addr{tailscaleIP}, nil },
			)
			if err != nil {
				t.Fatalf("listenPrivilegedWith(%q): %v", addr, err)
			}
			if listener != stub || stub.closed {
				t.Fatal("accepted Tailscale listener was replaced or closed")
			}
		})
	}

	for _, tc := range []struct {
		name      string
		addr      string
		tailscale []netip.Addr
		queryErr  error
	}{
		{name: "unreported Tailscale", addr: "100.64.0.7:8443"},
		{name: "different reported Tailscale", addr: "100.64.0.7:8443", tailscale: []netip.Addr{
			netip.MustParseAddr("100.64.0.8"),
		}},
		{name: "Tailscale query failure", addr: "100.64.0.7:8443", queryErr: errors.New("tailscaled unavailable")},
		{name: "arbitrary non-loopback", addr: "192.0.2.7:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bindCalled := false
			_, err := listenPrivilegedWith(
				tc.addr,
				func(string, *net.TCPAddr) (net.Listener, error) {
					bindCalled = true
					return nil, errors.New("unsafe bind was attempted")
				},
				func() ([]netip.Addr, error) { return tc.tailscale, tc.queryErr },
			)
			if err == nil {
				t.Fatalf("listenPrivilegedWith(%q) accepted unsupported address", tc.addr)
			}
			if bindCalled {
				t.Fatalf("listenPrivilegedWith(%q) attempted a bind", tc.addr)
			}
		})
	}
}

func TestLoopbackTwinAddr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   *net.TCPAddr
		want string // "" means no twin
	}{
		{name: "loopback v4", in: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8443}},
		{name: "loopback v6", in: &net.TCPAddr{IP: net.IPv6loopback, Port: 8443}},
		{name: "tailscale v4", in: &net.TCPAddr{IP: net.ParseIP("100.64.0.7"), Port: 8443}, want: "127.0.0.1:8443"},
		{name: "tailscale v6", in: &net.TCPAddr{IP: net.ParseIP("fd7a:115c:a1e0::7"), Port: 8443}, want: "127.0.0.1:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			twin, ok := loopbackTwinAddr(tc.in)
			if tc.want == "" {
				if ok {
					t.Fatalf("loopbackTwinAddr(%v) = %v, want no twin", tc.in, twin)
				}
				return
			}
			if !ok {
				t.Fatalf("loopbackTwinAddr(%v) reported no twin, want %q", tc.in, tc.want)
			}
			if twin.String() != tc.want {
				t.Fatalf("loopbackTwinAddr(%v) = %q, want %q", tc.in, twin, tc.want)
			}
			if !twin.IP.IsLoopback() {
				t.Fatalf("twin %q is not loopback", twin)
			}
		})
	}
}

func TestListenLoopbackTwin(t *testing.T) {
	t.Parallel()
	// A loopback primary needs no twin, and the binder is never called.
	twin, err := ListenLoopbackTwin(
		&listenerStub{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8443}},
		func(string, *net.TCPAddr) (net.Listener, error) {
			t.Fatal("loopback primary tried to bind a twin")
			return nil, nil
		},
	)
	if err != nil || twin != nil {
		t.Fatalf("ListenLoopbackTwin(loopback) = (%v, %v), want (nil, nil)", twin, err)
	}

	// A Tailscale primary (v4 and v6) twins onto 127.0.0.1 at the primary's
	// real port. The primary's Addr already carries the bound port, so a :0
	// primary passes its resolved port on.
	for _, primary := range []string{"100.64.0.7:8443", "[fd7a:115c:a1e0::7]:8443"} {
		t.Run(primary, func(t *testing.T) {
			resolved, err := net.ResolveTCPAddr("tcp", primary)
			if err != nil {
				t.Fatal(err)
			}
			var boundNetwork string
			var boundAddr *net.TCPAddr
			bound := &listenerStub{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: resolved.Port}}
			twin, err := ListenLoopbackTwin(
				&listenerStub{addr: resolved},
				func(network string, addr *net.TCPAddr) (net.Listener, error) {
					boundNetwork, boundAddr = network, addr
					return bound, nil
				},
			)
			if err != nil {
				t.Fatalf("ListenLoopbackTwin(%q): %v", primary, err)
			}
			if twin != bound {
				t.Fatalf("ListenLoopbackTwin(%q) returned a different listener", primary)
			}
			if boundNetwork != "tcp4" || boundAddr.String() != "127.0.0.1:8443" {
				t.Fatalf("bound %q on %q, want 127.0.0.1:8443 on tcp4", boundAddr, boundNetwork)
			}
		})
	}

	// A bind failure is returned with the loopback address named, so startup
	// fails rather than run reachable only over Tailscale.
	_, err = ListenLoopbackTwin(
		&listenerStub{addr: &net.TCPAddr{IP: net.ParseIP("100.64.0.7"), Port: 8443}},
		func(string, *net.TCPAddr) (net.Listener, error) {
			return nil, errors.New("address already in use")
		},
	)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:8443") {
		t.Fatalf("bind failure error = %v, want it to name 127.0.0.1:8443", err)
	}

	// A twin that binds a wrong address is closed and refused.
	for _, name := range []string{"non-loopback", "wrong port"} {
		t.Run(name, func(t *testing.T) {
			wrong := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}
			if name == "non-loopback" {
				wrong = &net.TCPAddr{IP: net.ParseIP("100.64.0.7"), Port: 8443}
			}
			stub := &listenerStub{addr: wrong}
			_, err := ListenLoopbackTwin(
				&listenerStub{addr: &net.TCPAddr{IP: net.ParseIP("100.64.0.7"), Port: 8443}},
				func(string, *net.TCPAddr) (net.Listener, error) { return stub, nil },
			)
			if err == nil {
				t.Fatalf("ListenLoopbackTwin accepted a twin bound to %q", wrong)
			}
			if !stub.closed {
				t.Fatal("refused twin listener was not closed")
			}
		})
	}
}

// TestServeTwoLoopbackListenersFromOneServer pins the property the twin relies
// on: one http.Server serves both listeners and Shutdown closes them both.
func TestServeTwoLoopbackListenersFromOneServer(t *testing.T) {
	t.Parallel()
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		}),
		ReadHeaderTimeout: time.Second,
	}
	var wg sync.WaitGroup
	exits := make(chan error, 2)
	for _, l := range []net.Listener{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exits <- server.Serve(l)
		}()
	}
	for _, l := range []net.Listener{first, second} {
		resp, err := http.Get("http://" + l.Addr().String() + "/health")
		if err != nil {
			t.Fatalf("GET %s: %v", l.Addr(), err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", l.Addr(), resp.StatusCode)
		}
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	wg.Wait()
	close(exits)
	for err := range exits {
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve exit = %v, want ErrServerClosed", err)
		}
	}
	// Shutdown closed both listeners, so a further accept fails on each.
	for _, l := range []net.Listener{first, second} {
		if _, err := l.Accept(); err == nil {
			t.Fatalf("listener %s still accepts after Shutdown", l.Addr())
		}
	}
}

func TestParseTailscaleIPs(t *testing.T) {
	t.Parallel()
	got, err := parseTailscaleIPs("100.64.0.7\nfd7a:115c:a1e0::7\n")
	if err != nil {
		t.Fatalf("parseTailscaleIPs: %v", err)
	}
	want := []netip.Addr{
		netip.MustParseAddr("100.64.0.7"),
		netip.MustParseAddr("fd7a:115c:a1e0::7"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseTailscaleIPs = %v, want %v", got, want)
	}

	for _, output := range []string{
		"",
		"not-an-ip",
		"192.0.2.7",
		"fe80::1%utun4",
		"100.64.0.7\n192.0.2.7",
	} {
		t.Run(output, func(t *testing.T) {
			if _, err := parseTailscaleIPs(output); err == nil {
				t.Fatalf("parseTailscaleIPs(%q) accepted unsupported output", output)
			}
		})
	}
}
