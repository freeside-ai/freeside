package ward

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// These are measurement helpers, not a production collector. Unknown fields
// fail the capture instead of silently dropping an unreviewed provider field.
type claudeUsageCapture struct {
	Mode          string            `json:"mode"`
	Version       string            `json:"version"`
	Args          []string          `json:"args"`
	Requests      []json.RawMessage `json:"requests"`
	Initialized   bool              `json:"initialized"`
	Answered      bool              `json:"answered"`
	TimedOut      bool              `json:"timedOut"`
	Overflow      bool              `json:"overflow"`
	Malformed     bool              `json:"malformed"`
	Code          *int              `json:"code"`
	Signal        *string           `json:"signal"`
	SpawnFailed   bool              `json:"spawnFailed"`
	Output        string            `json:"output"`
	AuthUnchanged bool              `json:"authUnchanged"`
	Failure       string            `json:"failure"`
}

// The probe terminates TLS only inside this disposable test boundary. The CLI
// has a host-only network and trusts this ephemeral CA. Provider TLS is still
// verified normally. Only reviewed routes leave the host; refresh is blocked
// before forwarding. No headers, bodies, query values or raw errors are logged.
type claudeUsageProxy struct {
	mu                           sync.Mutex
	inference, refresh, failures int
	requests                     []string
	active                       map[net.Conn]bool
	wg                           sync.WaitGroup
	server                       *httptest.Server
	certificate                  tls.Certificate
	ca                           []byte
	mode                         string
	subnet                       *net.IPNet
	transport                    http.RoundTripper
}

func newClaudeUsageProxy(t *testing.T, mode, subnet string, transport http.RoundTripper) *claudeUsageProxy {
	t.Helper()
	_, network, err := net.ParseCIDR(subnet)
	if err != nil {
		t.Fatal(err)
	}
	// The pinned native CLI does not offer Ed25519 TLS signature algorithms.
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Freeside usage spike"},
		DNSNames:  []string{"api.anthropic.com", "claude.ai", "console.anthropic.com"},
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
	certificate, err := tls.X509KeyPair(ca, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	if err != nil {
		t.Fatal(err)
	}
	if transport == nil {
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		transport = &http.Transport{Protocols: protocols, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	}
	p := &claudeUsageProxy{mode: mode, subnet: network, certificate: certificate, ca: ca, transport: transport, active: map[net.Conn]bool{}}
	p.server = httptest.NewUnstartedServer(http.HandlerFunc(p.serve))
	if err := p.server.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	// The guest gateway is not assigned to a host interface. Admit only the
	// realized private subnet below, as the production CONNECT proxy does.
	p.server.Listener, err = net.Listen("tcp4", "0.0.0.0:0") //nolint:gosec // test-only private subnet admission
	if err != nil {
		t.Fatal(err)
	}
	p.server.Start()
	t.Cleanup(func() {
		p.server.Close()
		p.mu.Lock()
		for conn := range p.active {
			_ = conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
		if tr, ok := p.transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	})
	return p
}

func (p *claudeUsageProxy) serve(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !p.subnet.Contains(net.ParseIP(host)) {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodConnect || (r.Host != "api.anthropic.com:443" && r.Host != "claude.ai:443" && r.Host != "console.anthropic.com:443") {
		p.mu.Lock()
		p.failures++
		p.requests = append(p.requests, "connect_denied")
		p.mu.Unlock()
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	// Register before hijacking removes this handler from Server.Close's wait.
	p.wg.Add(1)
	defer p.wg.Done()
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	p.mu.Lock()
	p.active[conn] = true
	p.mu.Unlock()
	defer func() { _ = conn.Close(); p.mu.Lock(); delete(p.active, conn); p.mu.Unlock() }()
	_ = conn.SetDeadline(time.Now().Add(70 * time.Second))
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{p.certificate}, MinVersion: tls.VersionTLS12})
	if err := secure.Handshake(); err != nil {
		p.mu.Lock()
		p.failures++
		failure := "tls_failed"
		for _, category := range []string{"signature algorithms", "bad certificate", "unknown certificate", "certificate authority", "EOF", "timeout"} {
			if strings.Contains(err.Error(), category) {
				failure += ":" + category
				break
			}
		}
		p.requests = append(p.requests, failure)
		p.mu.Unlock()
		return
	}
	request, err := http.ReadRequest(bufio.NewReader(secure))
	if err != nil {
		p.mu.Lock()
		p.failures++
		p.requests = append(p.requests, "http_decode_failed")
		p.mu.Unlock()
		return
	}
	defer func() { _ = request.Body.Close() }()
	path := request.URL.Path
	inference := path == "/v1/messages"
	refresh := strings.Contains(path, "token") && path != "/v1/messages/count_tokens"
	allowed := !refresh && ((inference && p.mode == "turn" && request.Method == http.MethodPost) ||
		(request.Method == http.MethodGet && (path == "/api/oauth/usage" || path == "/api/oauth/profile" || path == "/api/hello" ||
			path == "/api/claude_code/policy_limits" || path == "/api/claude_code/settings")))
	p.mu.Lock()
	if inference {
		p.inference++
	}
	if refresh {
		p.refresh++
	}
	if !allowed && !refresh && !inference {
		p.failures++
	}
	// Only known paths can appear in evidence; arbitrary paths may carry IDs.
	label := "blocked_other"
	if allowed || inference {
		label = request.Method + " " + path
	}
	if refresh {
		label = "blocked_refresh"
	}
	p.requests = append(p.requests, label)
	p.mu.Unlock()
	response := &http.Response{StatusCode: http.StatusForbidden, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("probe route denied")), Close: true}
	if allowed {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		request = request.WithContext(ctx)
		request.URL.Scheme, request.URL.Host = "https", r.Host
		request.Host, request.RequestURI = strings.TrimSuffix(r.Host, ":443"), ""
		request.Header.Del("Proxy-Authorization")
		request.Header.Del("Connection")
		request.Body = http.MaxBytesReader(nil, request.Body, 1<<20)
		upstream, err := p.transport.RoundTrip(request)
		if err != nil {
			p.mu.Lock()
			p.failures++
			failure := "upstream_transport_failed"
			for _, category := range []string{"timeout", "deadline exceeded", "certificate", "no such host", "connection refused", "EOF", "stream error", "PROTOCOL_ERROR", "connection reset", "invalid header", "malformed", "client connection", "GOAWAY", "INTERNAL_ERROR", "CANCEL"} {
				if strings.Contains(err.Error(), category) {
					failure += ":" + category
					break
				}
			}
			p.requests = append(p.requests, label+" "+failure)
			p.mu.Unlock()
		} else {
			response = upstream
			response.Close = true
			// The pinned CLI explicitly accepts 404 as empty settings/policy,
			// unlike an auth error or a failed usage/inference request.
			emptyStartup := request.Method == http.MethodGet && response.StatusCode == http.StatusNotFound &&
				(path == "/api/claude_code/settings" || path == "/api/claude_code/policy_limits")
			if emptyStartup {
				p.mu.Lock()
				p.requests = append(p.requests, label+" empty_404")
				p.mu.Unlock()
			} else if response.StatusCode >= 400 {
				p.mu.Lock()
				p.failures++
				p.requests = append(p.requests, fmt.Sprintf("%s upstream_status_%d", label, response.StatusCode))
				p.mu.Unlock()
			}
		}
	}
	defer func() { _ = response.Body.Close() }()
	// The CLI-facing connection speaks HTTP/1.1 even when upstream uses HTTP/2.
	response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
	if err := response.Write(secure); err != nil {
		p.mu.Lock()
		p.failures++
		p.requests = append(p.requests, label+" relay_failed")
		p.mu.Unlock()
	}
}

func TestClaudeUsageRelayFailure(t *testing.T) {
	p := newClaudeUsageProxy(t, "idle", "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)), ProtoMajor: 1, ProtoMinor: 1}, nil
	}))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(p.ca)
	proxyURL, err := url.Parse(p.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	r, err := client.Get("https://api.anthropic.com/api/oauth/usage")
	if err == nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	p.server.Close()
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failures != 1 {
		t.Fatal("upstream body failure was lost")
	}
}

func TestClaudeUsageStartupStatus(t *testing.T) {
	for _, tc := range []struct {
		path             string
		status, failures int
	}{
		{"/api/claude_code/settings", 404, 0},
		{"/api/claude_code/policy_limits", 404, 0},
		{"/api/claude_code/settings", 401, 1},
		{"/api/claude_code/policy_limits", 403, 1},
		{"/api/claude_code/settings", 500, 1},
		{"/api/oauth/usage", 404, 1},
	} {
		t.Run(fmt.Sprintf("%s-%d", tc.path, tc.status), func(t *testing.T) {
			p := newClaudeUsageProxy(t, "idle", "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), ProtoMajor: 2}, nil
			}))
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(p.ca)
			proxyURL, err := url.Parse(p.server.URL)
			if err != nil {
				t.Fatal(err)
			}
			tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
			r, err := client.Get("https://api.anthropic.com" + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.failures != tc.failures || p.inference != 0 || p.refresh != 0 {
				t.Fatal("startup status misclassified")
			}
		})
	}
}

func TestClaudeUsageRequestObservation(t *testing.T) {
	for _, mode := range []string{"idle", "turn"} {
		t.Run(mode, func(t *testing.T) {
			forwarded := 0
			p := newClaudeUsageProxy(t, mode, "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				forwarded++
				if r.Host != "api.anthropic.com" {
					t.Error("forwarded Host changed the provider authority")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), ProtoMajor: 2, ProtoMinor: 0}, nil
			}))
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(p.ca)
			proxyURL, err := url.Parse(p.server.URL)
			if err != nil {
				t.Fatal(err)
			}
			tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
			for _, path := range []string{"/v1/messages", "/v1/oauth/token", "/private-synthetic-secret"} {
				r, err := client.Post("https://api.anthropic.com"+path, "application/json", strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
			}
			for _, path := range []string{"/api/claude_code/policy_limits", "/api/claude_code/settings"} {
				r, err := client.Get("https://api.anthropic.com" + path)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if err != nil || r.ProtoMajor != 1 || r.StatusCode != 200 || string(body) != "{}" {
					t.Fatal("HTTP/2 startup response was not relayed as HTTP/1.1")
				}
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.inference != 1 || p.refresh != 1 || p.failures != 1 || strings.Contains(strings.Join(p.requests, " "), "synthetic-secret") {
				t.Fatal("request observation or sanitization failed")
			}
			want := 2
			if mode == "turn" {
				want++
			}
			if forwarded != want {
				t.Fatal("forbidden request reached provider")
			}
		})
	}
}

type claudeUsageEvidence struct {
	Verdict     string            `json:"verdict"`
	Initialized bool              `json:"initialized"`
	Response    json.RawMessage   `json:"response,omitempty"`
	Events      []json.RawMessage `json:"events"`
	Result      bool              `json:"successful_result"`
}

var claudeUsageDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)

func claudeUsageAuthUnchanged(auth, config, token string) bool {
	body, err := os.ReadFile(filepath.Join(auth, "token")) //nolint:gosec // fixed basename in test-owned private input directory
	if err != nil || string(body) != token {
		return false
	}
	body, err = os.ReadFile(filepath.Join(config, ".credentials.json")) //nolint:gosec // fixed sentinel basename in test-owned isolated configuration
	return err == nil && string(body) == "{}"
}

func TestClaudeUsageAuthMutation(t *testing.T) {
	auth, config := t.TempDir(), t.TempDir()
	for _, tc := range []struct{ path, body string }{
		{filepath.Join(auth, "token"), "synthetic-token"},
		{filepath.Join(config, ".credentials.json"), "{}"},
	} {
		if err := os.WriteFile(tc.path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !claudeUsageAuthUnchanged(auth, config, "synthetic-token") {
		t.Fatal("baseline mismatch")
	}
	if err := os.WriteFile(filepath.Join(config, ".credentials.json"), []byte(`{"refreshToken":"synthetic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if claudeUsageAuthUnchanged(auth, config, "synthetic-token") {
		t.Fatal("auth store mutation missed")
	}
	if err := os.WriteFile(filepath.Join(config, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(auth, "token"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if claudeUsageAuthUnchanged(auth, config, "synthetic-token") {
		t.Fatal("token mutation missed")
	}
}

func claudeUsageSafeValue(key string, value any) bool {
	switch key {
	case "five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_oauth_apps", "seven_day_cowork", "extra_usage", "rate_limit_info", "rate_limits":
		if value == nil {
			return true
		}
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range object {
			if !claudeUsageSafeValue(k, v) {
				return false
			}
		}
		return true
	case "utilization", "resetsAt", "overageResetsAt", "monthly_limit", "used_credits", "windowDurationMins":
		if value == nil {
			return true
		}
		_, ok := value.(json.Number)
		return ok
	case "isUsingOverage", "is_enabled", "rate_limits_available":
		if value == nil {
			return true
		}
		_, ok := value.(bool)
		return ok
	case "resets_at":
		if value == nil {
			return true
		}
		s, ok := value.(string)
		return ok && claudeUsageDate.MatchString(s)
	case "status", "overageStatus":
		return value == nil || value == "allowed" || value == "allowed_warning" || value == "rejected"
	case "rateLimitType":
		return value == nil || value == "five_hour" || value == "seven_day" || value == "seven_day_opus" || value == "seven_day_sonnet"
	case "overageDisabledReason":
		return value == nil || value == "org_level_disabled"
	case "subscription_type":
		return value == nil // No plan string was returned under this credential.
	default:
		return false
	}
}

func claudeUsageSanitize(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if !json.Valid(raw) {
		return nil, errors.New("invalid usage JSON")
	}
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, errors.New("usage payload is not an object")
	}
	for key, value := range fields {
		if !claudeUsageSafeValue(key, value) {
			return nil, errors.New("unreviewed usage field or value; capture withheld")
		}
	}
	return json.Marshal(fields)
}

func claudeUsageControl(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("usage response is not an object")
	}
	// The observed envelope includes run consumption and local activity. Neither
	// is subscription allowance; their arbitrary details never enter evidence.
	delete(fields, "session")
	delete(fields, "behaviors")
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return claudeUsageSanitize(body)
}

func claudeUsageAnalyze(c claudeUsageCapture, inference, refresh, transportFailures int) claudeUsageEvidence {
	e := claudeUsageEvidence{Verdict: "probe_failed", Events: []json.RawMessage{}}
	failed := c.Failure != "" || c.Version != "2.1.220" || c.TimedOut || c.Overflow || c.Malformed || c.SpawnFailed ||
		c.Code == nil || *c.Code != 0 || c.Signal != nil || !c.AuthUnchanged || refresh != 0 || transportFailures != 0
	unsupported, matched := false, false
	scanner := bufio.NewScanner(strings.NewReader(c.Output))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var msg struct {
			Type          string          `json:"type"`
			Subtype       string          `json:"subtype"`
			IsError       bool            `json:"is_error"`
			RateLimitInfo json.RawMessage `json:"rate_limit_info"`
			Response      struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
				Error     string          `json:"error"`
			} `json:"response"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil || msg.Type == "" {
			failed = true
			continue
		}
		switch msg.Type {
		case "control_response":
			if msg.Response.RequestID == "usage-init" && msg.Response.Subtype == "success" {
				e.Initialized = true
			}
			if msg.Response.RequestID != "usage-read" {
				continue
			}
			if matched || !e.Initialized {
				failed = true
				continue
			}
			matched = true
			switch msg.Response.Subtype {
			case "error":
				unsupported = msg.Response.Error == "get_usage is not supported in this context (onGetUsage callback not registered)"
			case "success":
				var err error
				e.Response, err = claudeUsageControl(msg.Response.Response)
				if err != nil {
					failed = true
				}
			}
		case "rate_limit_event":
			value, err := claudeUsageSanitize(msg.RateLimitInfo)
			if err != nil {
				failed = true
				continue
			}
			e.Events = append(e.Events, value)
		case "result":
			if msg.IsError || msg.Subtype != "success" {
				failed = true
				continue
			}
			e.Result = true
		}
	}
	if scanner.Err() != nil || failed {
		return e
	}
	if c.Mode == "idle" && e.Initialized && matched && inference == 0 {
		var availability struct {
			Available *bool `json:"rate_limits_available"`
		}
		_ = json.Unmarshal(e.Response, &availability)
		if unsupported {
			e.Verdict = "unsupported_request"
		} else if availability.Available != nil && !*availability.Available {
			e.Verdict = "usage_unavailable"
		} else if len(e.Response) > 2 {
			e.Verdict = "usage_observed"
		} else if e.Response != nil {
			e.Verdict = "empty_response"
		}
	}
	if c.Mode == "turn" && e.Result && inference == 1 {
		e.Verdict = "event_not_observed"
		if len(e.Events) > 0 {
			e.Verdict = "rate_limit_event_observed"
		}
	}
	return e
}

func TestClaudeUsageObservedFixtures(t *testing.T) {
	zero := 0
	for _, tc := range []struct {
		mode, verdict string
		inference     int
	}{
		{"idle", "usage_unavailable", 0}, {"turn", "rate_limit_event_observed", 1},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			body, err := os.ReadFile("testdata/claude_usage_observed_" + tc.mode + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			c := claudeUsageCapture{Mode: tc.mode, Version: "2.1.220", Code: &zero, AuthUnchanged: true, Output: string(body)}
			e := claudeUsageAnalyze(c, tc.inference, 0, 0)
			if e.Verdict != tc.verdict {
				t.Fatalf("verdict = %s", e.Verdict)
			}
			if tc.mode == "idle" && (!strings.Contains(string(e.Response), `"rate_limits":null`) || !strings.Contains(string(e.Response), `"rate_limits_available":false`)) {
				t.Fatal("availability or null was lost")
			}
			if tc.mode == "turn" && (len(e.Events) != 1 || strings.Contains(string(e.Events[0]), "utilization") || !strings.Contains(string(e.Events[0]), `"overageDisabledReason":"org_level_disabled"`)) {
				t.Fatal("event fields changed or usage invented")
			}
		})
	}
	safe, err := claudeUsageControl(json.RawMessage(`{"session":{"total_cost_usd":999,"private":"synthetic-secret"},"behaviors":{"private":"synthetic-secret"},"rate_limits_available":false,"rate_limits":null}`))
	if err != nil || strings.Contains(string(safe), "synthetic-secret") || strings.Contains(string(safe), "999") {
		t.Fatal("non-allowance details leaked")
	}
}

func TestClaudeUsageProtocol(t *testing.T) {
	body, err := os.ReadFile("testdata/claude_usage_synthetic.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	base := claudeUsageCapture{Mode: "idle", Version: "2.1.220", Code: &zero, AuthUnchanged: true, Output: string(body)}
	for _, tc := range []struct {
		name                         string
		edit                         func(*claudeUsageCapture)
		inference, refresh, failures int
		want                         string
	}{
		{name: "correlated", want: "usage_observed"},
		{name: "wrong ID", edit: func(c *claudeUsageCapture) { c.Output = strings.ReplaceAll(c.Output, "usage-read", "other") }, want: "probe_failed"},
		{name: "truncated", edit: func(c *claudeUsageCapture) { c.Output += "{\"type\":" }, want: "probe_failed"},
		{name: "null envelope", edit: func(c *claudeUsageCapture) { c.Output += "\nnull\n" }, want: "probe_failed"},
		{name: "auth mutation", edit: func(c *claudeUsageCapture) { c.AuthUnchanged = false }, want: "probe_failed"},
		{name: "timeout", edit: func(c *claudeUsageCapture) { c.TimedOut = true }, want: "probe_failed"},
		{name: "wrong pin", edit: func(c *claudeUsageCapture) { c.Version = "other" }, want: "probe_failed"},
		{name: "inference during idle", inference: 1, want: "probe_failed"},
		{name: "refresh attempt", refresh: 1, want: "probe_failed"},
		{name: "transport failure", failures: 1, want: "probe_failed"},
		{name: "unsupported", edit: func(c *claudeUsageCapture) {
			c.Output = "{\"type\":\"control_response\",\"response\":{\"request_id\":\"usage-init\",\"subtype\":\"success\"}}\n{\"type\":\"control_response\",\"response\":{\"request_id\":\"usage-read\",\"subtype\":\"error\",\"error\":\"get_usage is not supported in this context (onGetUsage callback not registered)\"}}"
		}, want: "unsupported_request"},
		{name: "auth error is not unsupported", edit: func(c *claudeUsageCapture) {
			c.Output = strings.ReplaceAll(c.Output, `"subtype":"success","response":`, `"subtype":"error","error":"Unauthorized","response":`)
		}, want: "probe_failed"},
		{name: "result usage is not subscription", edit: func(c *claudeUsageCapture) {
			c.Mode = "turn"
			c.Output = `{"type":"result","subtype":"success","usage":{"input_tokens":3},"total_cost_usd":0.01}`
		}, inference: 1, want: "event_not_observed"},
		{name: "multiple inference turns fail the probe", edit: func(c *claudeUsageCapture) {
			c.Mode = "turn"
			c.Output = `{"type":"result","subtype":"success"}`
		}, inference: 2, want: "probe_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			if tc.edit != nil {
				tc.edit(&c)
			}
			if got := claudeUsageAnalyze(c, tc.inference, tc.refresh, tc.failures); got.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s", got.Verdict, tc.want)
			}
		})
	}
	e := claudeUsageAnalyze(base, 0, 0, 0)
	if !strings.Contains(string(e.Response), `"seven_day_opus":null`) || !strings.Contains(string(e.Response), `"utilization":12.5`) || strings.Contains(string(e.Response), "windowDurationMins") {
		t.Fatal("field types, nulls, or absent fields changed")
	}
	failedTurn := base
	failedTurn.Mode, failedTurn.TimedOut = "turn", true
	failedTurn.Output = "{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true}\n" +
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1791000000}}`
	if got := claudeUsageAnalyze(failedTurn, 1, 0, 1); got.Verdict != "probe_failed" || len(got.Events) != 1 {
		t.Fatal("failed probe lost its sanitized usage-bearing evidence")
	}
	for _, input := range []string{`{"account_id":null}`, `{"synthetic-secret":null}`, `{"account_id":"synthetic-secret"}`, `{"status":"synthetic-secret"}`, `{"resets_at":"synthetic-secret"}`, `{"utilization":"synthetic-secret"}`} {
		if safe, err := claudeUsageSanitize(json.RawMessage(input)); err == nil || len(safe) != 0 {
			t.Fatal("unsafe capture accepted")
		}
	}
	precise := json.RawMessage(`{"used_credits":9007199254740993}`)
	if safe, err := claudeUsageSanitize(precise); err != nil || string(safe) != string(precise) {
		t.Fatal("reported quantity lost precision")
	}
}
