package ward

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// Only this closed vocabulary crosses the container boundary. Neither an RPC
// error message nor an arbitrary field name is safe to copy into evidence.
type codexAccountCapture struct {
	Version       string            `json:"version"`
	Initialized   bool              `json:"initialized"`
	Answered      bool              `json:"answered"`
	Account       bool              `json:"account"`
	Fields        map[string]string `json:"fields"`
	Plan          string            `json:"plan"`
	RPCError      bool              `json:"rpcError"`
	Malformed     bool              `json:"malformed"`
	TimedOut      bool              `json:"timedOut"`
	Overflow      bool              `json:"overflow"`
	DriverFailed  bool              `json:"driverFailed"`
	AuthUnchanged bool              `json:"authUnchanged"`
	SymlinkSame   bool              `json:"symlinkSame"`
	AppendDenied  bool              `json:"appendDenied"`
	UnlinkDenied  bool              `json:"unlinkDenied"`
}

type codexAccountEvidence struct {
	Case     string              `json:"case"`
	Capture  codexAccountCapture `json:"capture"`
	Requests []string            `json:"requests"`
	Failures int                 `json:"failures"`
	Verdict  string              `json:"verdict"`
}

func codexAccountDecode(body []byte) (codexAccountCapture, error) {
	var c codexAccountCapture
	err := strictjson.Decode(body, &c, strictjson.RejectInvalidUTF8, strictjson.Limit(16<<10))
	if err == nil && (c.Version != "0.147.0" || ((len(c.Fields) > 0 || c.Plan != "" || c.Account) && !codexAccountFieldsValid(c))) {
		err = errors.New("unreviewed capture fields or CLI version")
	}
	return c, err
}

func codexAccountFieldsValid(c codexAccountCapture) bool {
	if c.Version != "0.147.0" {
		return false
	}
	want := map[string]string{"requiresOpenaiAuth": "boolean", "account": "null"}
	if c.Account {
		want = map[string]string{"requiresOpenaiAuth": "boolean", "account": "object", "account.type": "string", "account.email": "string", "account.planType": "string"}
		if c.Fields["account.email"] == "null" {
			want["account.email"] = "null"
		}
	}
	if len(c.Fields) != len(want) {
		return false
	}
	for key, value := range c.Fields {
		if want[key] != value {
			return false
		}
	}
	// This is a versioned observation, not an open-ended text field.
	switch c.Plan {
	case "":
		return !c.Account
	case "free", "go", "plus", "pro", "prolite", "team", "self_serve_business_prolite", "self_serve_business_usage_based", "business", "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise", "edu", "unknown":
		return c.Account
	default:
		return false
	}
}

func codexAccountAnalyze(c codexAccountCapture, control bool, requests []string, failures int) string {
	refresh := false
	for _, label := range requests {
		if label != "blocked_refresh" && label != "blocked_other" && label != "connection_closed" && label != "transport_failed" && label != "tls_failed" && label != "http_decode_failed" {
			return "probe_failed"
		}
		refresh = refresh || label == "blocked_refresh"
	}
	if c.Version != "0.147.0" || c.DriverFailed || c.Malformed || c.Overflow || c.TimedOut || !c.Initialized || !c.Answered || (!c.RPCError && !codexAccountFieldsValid(c)) {
		return "probe_failed"
	}
	if !c.AuthUnchanged || !c.SymlinkSame || !c.AppendDenied || !c.UnlinkDenied {
		return "fail"
	}
	// Transport errors prevent proving absence; they cannot erase a positively
	// observed refresh. A control pass proves only the detector, not a clean
	// account probe. Preserve all error labels and counts in the evidence.
	if refresh {
		if control {
			return "pass"
		}
		return "fail"
	}
	if control || failures != 0 || c.RPCError {
		return "probe_failed"
	}
	if !c.Account {
		return "fail"
	}
	return "pass"
}

// The host-only network has no direct route out. This proxy terminates TLS to
// count attempted refreshes, but NEVER forwards anything, including credentials.
type codexAccountProxy struct {
	mu          sync.Mutex
	requests    []string
	failures    int
	active      map[net.Conn]bool
	wg          sync.WaitGroup
	subnet      *net.IPNet
	certificate tls.Certificate
	ca          []byte
	server      *httptest.Server
}

func newCodexAccountProxy(t *testing.T, subnet string) *codexAccountProxy {
	t.Helper()
	_, network, err := net.ParseCIDR(subnet)
	if err != nil {
		t.Fatal(err)
	}
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Freeside account spike"},
		DNSNames:  []string{"auth.openai.com", "chatgpt.com"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// Rust's verifier rejects a CA used as a server leaf. Keep the root and
	// server certificate separate even though this disposable probe owns both.
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Freeside account proxy"},
		DNSNames: template.DNSNames, NotBefore: template.NotBefore, NotAfter: template.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	if err != nil {
		t.Fatal(err)
	}
	p := &codexAccountProxy{subnet: network, certificate: cert, ca: ca, active: map[net.Conn]bool{}, requests: []string{}}
	p.server = httptest.NewUnstartedServer(http.HandlerFunc(p.serve))
	if err := p.server.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	p.server.Listener, err = net.Listen("tcp4", "0.0.0.0:0") //nolint:gosec // test-only listener admits only its private subnet
	if err != nil {
		t.Fatal(err)
	}
	p.server.Start()
	t.Cleanup(func() {
		p.server.Close()
		p.mu.Lock()
		for c := range p.active {
			_ = c.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}

func codexAccountRequestLabel(host, path string) string {
	if strings.EqualFold(host, "auth.openai.com") || strings.Contains(strings.ToLower(path), "oauth/token") {
		return "blocked_refresh"
	}
	return "blocked_other"
}

func (p *codexAccountProxy) record(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, label)
	// Denying an ancillary route is the intended measurement policy. It is
	// not a broken proxy. Missing answers and actual transport errors are
	// assessed separately, so background traffic cannot hide a proven refresh.
	if label == "transport_failed" || label == "tls_failed" || label == "http_decode_failed" {
		p.failures++
	}
}

func (p *codexAccountProxy) serve(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !p.subnet.Contains(net.ParseIP(host)) {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodConnect {
		p.record(codexAccountRequestLabel(r.URL.Hostname(), r.URL.Path))
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	if r.Host != "auth.openai.com:443" && r.Host != "chatgpt.com:443" {
		p.record("blocked_other")
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		p.record("transport_failed")
		return
	}
	p.wg.Add(1)
	defer p.wg.Done()
	conn, _, err := hijacker.Hijack()
	if err != nil {
		p.record("transport_failed")
		return
	}
	p.mu.Lock()
	p.active[conn] = true
	p.mu.Unlock()
	defer func() { _ = conn.Close(); p.mu.Lock(); delete(p.active, conn); p.mu.Unlock() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		p.record("transport_failed")
		return
	}
	secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{p.certificate}, MinVersion: tls.VersionTLS12})
	if err := secure.Handshake(); err != nil {
		p.record("tls_failed")
		return
	}
	reader := bufio.NewReader(secure)
	// Prove zero HTTP bytes before accepting a shutdown EOF. ReadRequest can
	// return EOF after consuming an unterminated first line; that is a broken
	// measurement, not an idle connection.
	if _, err := reader.Peek(1); errors.Is(err, io.EOF) {
		p.record("connection_closed")
		return
	}
	request, err := http.ReadRequest(reader)
	if err != nil {
		p.record("http_decode_failed")
		return
	}
	defer func() { _ = request.Body.Close() }()
	p.record(codexAccountRequestLabel(strings.TrimSuffix(r.Host, ":443"), request.URL.Path))
	_, _ = io.WriteString(secure, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

func TestCodexAccountProxy(t *testing.T) {
	p := newCodexAccountProxy(t, "127.0.0.0/8")
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(p.ca)
	proxyURL, err := url.Parse(p.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for _, target := range []string{"https://auth.openai.com/oauth/token?secret=never-log", "https://chatgpt.com/oauth/token", "https://chatgpt.com/unknown-private-id"} {
		response, err := client.Post(target, "application/json", strings.NewReader(`{"refresh_token":"synthetic"}`))
		if err != nil {
			t.Fatal("proxy request failed")
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatal("request escaped the deny-all proxy")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.Join(p.requests, ",") != "blocked_refresh,blocked_refresh,blocked_other" || p.failures != 0 {
		t.Fatal("classification differs")
	}
}

func TestCodexAccountProxyClosedConnection(t *testing.T) {
	for _, fragment := range []string{"", "POST /oauth/", strings.Repeat("P", 8192)} {
		t.Run(map[bool]string{false: "idle", true: "partial request"}[fragment != ""], func(t *testing.T) {
			p := newCodexAccountProxy(t, "127.0.0.0/8")
			conn, err := net.Dial("tcp", p.server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := io.WriteString(conn, "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(p.ca)
			secure := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "chatgpt.com", MinVersion: tls.VersionTLS12})
			if err := secure.Handshake(); err != nil {
				t.Fatal(err)
			}
			if fragment != "" {
				if _, err := io.WriteString(secure, fragment); err != nil {
					t.Fatal(err)
				}
			}
			_ = secure.Close()
			p.wg.Wait()
			p.mu.Lock()
			defer p.mu.Unlock()
			want, wantFailures := "connection_closed", 0
			if fragment != "" {
				want, wantFailures = "http_decode_failed", 1
			}
			if len(p.requests) != 1 || p.requests[0] != want || p.failures != wantFailures {
				t.Fatal("idle close and partial request were conflated")
			}
		})
	}
}

func codexAccountGoodCapture() codexAccountCapture {
	return codexAccountCapture{
		Version: "0.147.0", Initialized: true, Answered: true, Account: true, Plan: "plus",
		Fields:        map[string]string{"requiresOpenaiAuth": "boolean", "account": "object", "account.type": "string", "account.email": "string", "account.planType": "string"},
		AuthUnchanged: true, SymlinkSame: true, AppendDenied: true, UnlinkDenied: true,
	}
}

func TestCodexAccountVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mutate     func(*codexAccountCapture)
		control    bool
		requests   []string
		failures   int
	}{
		{name: "read", want: "pass"},
		{name: "answered despite denied ancillary route", want: "pass", requests: []string{"blocked_other"}},
		{name: "refresh", want: "fail", requests: []string{"blocked_refresh"}},
		{name: "refresh with other transport failure", want: "fail", requests: []string{"blocked_refresh", "tls_failed"}, failures: 1},
		{name: "control", want: "pass", control: true, requests: []string{"blocked_refresh"}},
		{name: "detector proved with other transport failure", want: "pass", control: true, requests: []string{"blocked_refresh", "tls_failed"}, failures: 1},
		{name: "control did not answer", want: "probe_failed", control: true, requests: []string{"blocked_refresh"}, mutate: func(c *codexAccountCapture) { c.Answered = false }},
		{name: "missing control", want: "probe_failed", control: true},
		{name: "transport", want: "probe_failed", failures: 1},
		{name: "timeout", want: "probe_failed", mutate: func(c *codexAccountCapture) { c.TimedOut = true }},
		{name: "overflow", want: "probe_failed", mutate: func(c *codexAccountCapture) { c.Overflow = true }},
		{name: "parse", want: "probe_failed", mutate: func(c *codexAccountCapture) { c.Malformed = true }},
		{name: "rpc error", want: "probe_failed", mutate: func(c *codexAccountCapture) { c.RPCError = true }},
		{name: "write", want: "fail", mutate: func(c *codexAccountCapture) { c.AuthUnchanged = false }},
		{name: "replace symlink", want: "fail", mutate: func(c *codexAccountCapture) { c.SymlinkSame = false }},
		{name: "unprotected snapshot", want: "fail", mutate: func(c *codexAccountCapture) { c.UnlinkDenied = false }},
		{name: "unavailable account", want: "fail", mutate: func(c *codexAccountCapture) {
			c.Account, c.Plan = false, ""
			c.Fields = map[string]string{"requiresOpenaiAuth": "boolean", "account": "null"}
		}},
		{name: "unreviewed field", want: "probe_failed", mutate: func(c *codexAccountCapture) { c.Fields["private-id"] = "string" }},
		{name: "unreviewed plan", want: "probe_failed", mutate: func(c *codexAccountCapture) { c.Plan = "private-id" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := codexAccountGoodCapture()
			if tc.mutate != nil {
				tc.mutate(&capture)
			}
			if got := codexAccountAnalyze(capture, tc.control, tc.requests, tc.failures); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCodexAccountCaptureStrict(t *testing.T) {
	for _, body := range []string{`{"email":"private"}`, `{"version":"0.147.0","version":"other"}`, `{} {}`} {
		if _, err := codexAccountDecode([]byte(body)); err == nil {
			t.Fatal("unreviewed or ambiguous capture accepted")
		}
	}
}

// Exercise the actual in-container reducer with synthetic values. jq is also
// a declared image/runtime tool; this test needs no provider or network.
func TestCodexAccountSanitizer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		valid bool
	}{
		{"account", `{"requiresOpenaiAuth":true,"account":{"type":"chatgpt","email":"private@example.invalid","planType":"plus"}}`, true},
		{"none", `{"requiresOpenaiAuth":true,"account":null}`, true},
		{"nullable email", `{"requiresOpenaiAuth":true,"account":{"type":"chatgpt","email":null,"planType":"plus"}}`, true},
		{"unknown root", `{"requiresOpenaiAuth":true,"account":null,"token":"private-token"}`, false},
		{"account id", `{"requiresOpenaiAuth":true,"account":{"type":"chatgpt","email":"private@example.invalid","planType":"plus","accountId":"private-id"}}`, false},
		{"plan leak", `{"requiresOpenaiAuth":true,"account":{"type":"chatgpt","email":"private@example.invalid","planType":"private-token"}}`, false},
		{"wrong type", `{"requiresOpenaiAuth":true,"account":{"type":"chatgpt","email":{},"planType":"plus"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("jq", "-ef", "testdata/codex_account_sanitize.jq")
			cmd.Stdin = strings.NewReader(tc.body)
			out, err := cmd.Output()
			if (err == nil) != tc.valid {
				t.Fatal("sanitizer acceptance differs")
			}
			if bytes.Contains(out, []byte("private")) {
				t.Fatal("private value escaped")
			}
			if tc.valid {
				c := codexAccountCapture{Version: "0.147.0"}
				if err := json.Unmarshal(out, &c); err != nil {
					t.Fatal(err)
				}
				if !codexAccountFieldsValid(c) {
					t.Fatal("sanitizer and host schema disagree")
				}
			}
		})
	}
}
