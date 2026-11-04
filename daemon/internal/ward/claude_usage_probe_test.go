package ward

import (
	"bufio"
	"bytes"
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
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/strayrequest"
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
	// Path 2 only: the read's status, bounded body and failure flags.
	ReadStatus      *int   `json:"readStatus"`
	ReadBody        string `json:"readBody"`
	TransportFailed bool   `json:"transportFailed"`
}

// claudeUsageVariants are the live launches in run order. Every variant but
// read drives the pinned CLI (path 1); read is the direct usage read (path
// 2). Only turn and turn_seeded run inference.
var claudeUsageVariants = []string{"idle", "idle_profile_scope", "turn", "turn_seeded", "read"}

func claudeUsageInfers(mode string) bool { return mode == "turn" || mode == "turn_seeded" }

// The usage read is the measured answer in these modes, so its refusal is a
// provider answer for the analyzer, not a transport failure.
func claudeUsageReadsUsage(mode string) bool { return mode == "idle_profile_scope" || mode == "read" }

// Variants that make the CLI assume user:profile also send GET
// /api/oauth/profile. Neither the usage read nor the header seed reads its
// answer, so its refusal is recorded as a status, not a failed measurement.
func claudeUsageAssumesProfile(mode string) bool {
	return mode == "idle_profile_scope" || mode == "turn_seeded"
}

// Reviewed inference response headers the pinned CLI stores as its usage
// seed. Only their presence enters evidence, never their values.
// Fixed-path GETs the pinned CLI sends at startup once nonessential traffic is
// allowed, observed with a synthetic token. Their paths carry no IDs.
var claudeUsageNonessentialRoutes = map[string]bool{
	"/api/claude_cli/bootstrap": true, "/api/claude_code_penguin_mode": true, "/mcp-registry/v0/servers": true,
}

var claudeUsageSeedHeaders = []string{"5h-utilization", "5h-reset", "7d-utilization", "7d-reset", "7d_oi-utilization", "7d_oi-reset", "overage-utilization", "overage-reset"}

// Bound on a usage answer the proxy classifies; path 2's driver uses the same.
const claudeUsageReadBodyLimit = 64 << 10

// The probe terminates TLS only inside this disposable test boundary. The CLI
// has a host-only network and trusts this ephemeral CA. Provider TLS is still
// verified normally. Only reviewed routes leave the host; refresh is blocked
// before forwarding. No headers, bodies, query values or raw errors are logged.
type claudeUsageProxy struct {
	mu                           sync.Mutex
	inference, refresh, failures int
	requests                     []string
	usageStatuses                []int
	usageVerdicts                []string
	seedHeaders                  []string
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
	p.server = httptest.NewUnstartedServer(strayrequest.Ignore(http.HandlerFunc(p.serve)))
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
	// The direct read admits only the production Claude route's authority.
	authorityAllowed := r.Host == "api.anthropic.com:443" ||
		(p.mode != "read" && (r.Host == "claude.ai:443" || r.Host == "console.anthropic.com:443"))
	if r.Method != http.MethodConnect || !authorityAllowed {
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
	usage := request.Method == http.MethodGet && path == "/api/oauth/usage"
	nonessential := request.Method == http.MethodGet && claudeUsageNonessentialRoutes[path]
	allowed := !refresh && ((inference && claudeUsageInfers(p.mode) && request.Method == http.MethodPost) || usage ||
		(p.mode != "read" && request.Method == http.MethodGet && (path == "/api/oauth/profile" || path == "/api/hello" ||
			path == "/api/claude_code/policy_limits" || path == "/api/claude_code/settings")))
	p.mu.Lock()
	if inference {
		p.inference++
	}
	if refresh {
		p.refresh++
	}
	// The profile-scope variant re-enables nonessential traffic only so the
	// CLI sends its usage read; the startup routes that also come back are
	// refused unforwarded and do not feed that read.
	if !allowed && !refresh && !inference && (!nonessential || p.mode != "idle_profile_scope") {
		p.failures++
	}
	// Only known paths can appear in evidence; arbitrary paths may carry IDs.
	label := "blocked_other"
	if nonessential {
		label = "blocked GET " + path
	}
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
		if usage && claudeUsageReadsUsage(p.mode) {
			// The CLI asks for encodings this test cannot decode (brotli). Without
			// the header, Go's transport negotiates gzip and decodes it, so the
			// answer can be classified; a measurement deviation on this route only.
			request.Header.Del("Accept-Encoding")
		}
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
			if inference {
				p.mu.Lock()
				for _, name := range claudeUsageSeedHeaders {
					if response.Header.Get("anthropic-ratelimit-unified-"+name) != "" {
						p.seedHeaders = append(p.seedHeaders, name)
					}
				}
				p.mu.Unlock()
			}
			if usage && claudeUsageReadsUsage(p.mode) {
				// Classify the answer here, by the same rule as path 2, because the
				// CLI folds any refusal into a null rate_limits. Only the verdict and
				// a reviewed error type are kept; the bytes are relayed unchanged.
				body, err := io.ReadAll(io.LimitReader(response.Body, claudeUsageReadBodyLimit+1))
				_ = response.Body.Close()
				verdict, errorType := "failed:body", ""
				encoded := response.Header.Get("Content-Encoding") != "" && !strings.EqualFold(response.Header.Get("Content-Encoding"), "identity")
				if err == nil && len(body) <= claudeUsageReadBodyLimit && !encoded {
					verdict, errorType, _ = claudeUsageReadClass(response.StatusCode, string(body))
				}
				response.Body, response.ContentLength, response.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
				p.mu.Lock()
				p.usageStatuses = append(p.usageStatuses, response.StatusCode)
				p.usageVerdicts = append(p.usageVerdicts, verdict)
				p.requests = append(p.requests, strings.TrimSpace(fmt.Sprintf("%s usage_status_%d %s %s", label, response.StatusCode, verdict, errorType)))
				if verdict == "failed:body" {
					p.failures++
				}
				p.mu.Unlock()
			} else if path == "/api/oauth/profile" && response.StatusCode >= 400 && response.StatusCode < 500 && claudeUsageAssumesProfile(p.mode) {
				p.mu.Lock()
				p.requests = append(p.requests, fmt.Sprintf("%s profile_status_%d", label, response.StatusCode))
				p.mu.Unlock()
			} else if emptyStartup {
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
		mode             string
	}{
		{"/api/claude_code/settings", 404, 0, "idle"},
		{"/api/claude_code/policy_limits", 404, 0, "idle"},
		{"/api/claude_code/settings", 401, 1, "idle"},
		{"/api/claude_code/policy_limits", 403, 1, "idle"},
		{"/api/claude_code/settings", 500, 1, "idle"},
		{"/api/oauth/usage", 404, 1, "idle"},
		{"/api/oauth/profile", 403, 1, "idle"},
		{"/api/oauth/profile", 403, 0, "idle_profile_scope"},
		{"/api/oauth/profile", 403, 0, "turn_seeded"},
		{"/api/oauth/profile", 500, 1, "turn_seeded"},
		{"/api/oauth/usage", 403, 0, "idle_profile_scope"},
		{"/api/oauth/usage", 403, 1, "turn_seeded"},
	} {
		t.Run(fmt.Sprintf("%s-%s-%d", tc.mode, tc.path, tc.status), func(t *testing.T) {
			p := newClaudeUsageProxy(t, tc.mode, "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
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
	for _, mode := range []string{"idle", "idle_profile_scope", "turn", "turn_seeded"} {
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
			if claudeUsageInfers(mode) {
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
	// Path 2 only. ErrorType is a reviewed provider error type, never a message.
	Status    *int   `json:"status,omitempty"`
	ErrorType string `json:"error_type,omitempty"`
}

var claudeUsageDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)

// claudeUsageAuthUnchanged reports whether snapshotToken, the token bytes
// read back from the probe's snapshot volume, still equal token and the
// isolated auth store in config still holds the empty sentinel.
func claudeUsageAuthUnchanged(snapshotToken []byte, config, token string) bool {
	if string(snapshotToken) != token {
		return false
	}
	body, err := os.ReadFile(filepath.Join(config, ".credentials.json")) //nolint:gosec // fixed sentinel basename in test-owned isolated configuration
	return err == nil && string(body) == "{}"
}

func TestClaudeUsageAuthMutation(t *testing.T) {
	config := t.TempDir()
	if err := os.WriteFile(filepath.Join(config, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !claudeUsageAuthUnchanged([]byte("synthetic-token"), config, "synthetic-token") {
		t.Fatal("baseline mismatch")
	}
	if err := os.WriteFile(filepath.Join(config, ".credentials.json"), []byte(`{"refreshToken":"synthetic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if claudeUsageAuthUnchanged([]byte("synthetic-token"), config, "synthetic-token") {
		t.Fatal("auth store mutation missed")
	}
	if err := os.WriteFile(filepath.Join(config, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if claudeUsageAuthUnchanged([]byte("changed"), config, "synthetic-token") {
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

// claudeUsageAnalyze classifies one path 1 variant. usageVerdicts holds the
// proxy's classification of each usage-route answer it relayed; only
// idle_profile_scope makes that read, and its answer decides what a null
// rate_limits means.
func claudeUsageAnalyze(c claudeUsageCapture, inference, refresh, transportFailures int, usageVerdicts []string) claudeUsageEvidence {
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
	idle := (c.Mode == "idle" || c.Mode == "idle_profile_scope") && inference == 0
	seeded := c.Mode == "turn_seeded" && e.Result && inference == 1
	if (idle || seeded) && e.Initialized && matched {
		var availability struct {
			Available *bool           `json:"rate_limits_available"`
			Limits    json.RawMessage `json:"rate_limits"`
		}
		_ = json.Unmarshal(e.Response, &availability)
		switch {
		case unsupported:
			e.Verdict = "unsupported_request"
		case availability.Available != nil && !*availability.Available:
			e.Verdict = "usage_unavailable"
		// Available but null: the CLI tried its read or seed and got nothing.
		case availability.Available != nil && (availability.Limits == nil || string(availability.Limits) == "null"):
			e.Verdict = "usage_withheld"
		case len(e.Response) > 2:
			e.Verdict = "usage_observed"
		case e.Response != nil:
			e.Verdict = "empty_response"
		}
	}
	// The CLI turns any refusal of its own read into a null rate_limits, so the
	// relayed answer decides: a supported read keeps the CLI's verdict unless
	// the CLI dropped it, a scope refusal is usage_refused, and anything else,
	// or no read, proves nothing.
	if c.Mode == "idle_profile_scope" && e.Verdict != "probe_failed" {
		switch {
		case len(usageVerdicts) != 1:
			e.Verdict = "probe_failed"
		case usageVerdicts[0] == "supported" && e.Verdict != "usage_withheld":
		case usageVerdicts[0] == "unsupported" && e.Verdict == "usage_withheld":
			e.Verdict = "usage_refused"
		default:
			e.Verdict = "probe_failed"
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

// claudeUsageReadAnalyze classifies path 2's one direct read. A harness
// failure is probe_failed; a read that reached no reviewed answer is
// failed:<cause>. Only a 403 permission_error that names a scope is
// unsupported: an expired or revoked token, a throttle, a missing route or an
// unreviewed refusal never proves the credential cannot read usage.
func claudeUsageReadAnalyze(c claudeUsageCapture, refresh, transportFailures int, usageStatuses []int) claudeUsageEvidence {
	e := claudeUsageEvidence{Verdict: "probe_failed", Events: []json.RawMessage{}, Status: c.ReadStatus}
	if c.Mode != "read" || c.Failure != "" || c.Version != "2.1.220" || c.Overflow || c.Malformed || c.SpawnFailed ||
		c.Code == nil || *c.Code != 0 || c.Signal != nil || !c.AuthUnchanged || refresh != 0 || transportFailures != 0 {
		return e
	}
	switch {
	case c.TimedOut:
		e.Verdict = "failed:timeout"
		return e
	case c.TransportFailed || c.ReadStatus == nil:
		e.Verdict = "failed:transport"
		return e
	}
	// The proxy saw exactly the one read the driver reports, or the evidence
	// disagrees about what the provider answered.
	if len(usageStatuses) != 1 || usageStatuses[0] != *c.ReadStatus {
		return e
	}
	e.Verdict, e.ErrorType, e.Response = claudeUsageReadClass(*c.ReadStatus, c.ReadBody)
	return e
}

// claudeUsageReadClass classifies one provider answer on the usage route, for
// path 2's direct read and for the CLI's own read alike. A 200 is supported
// only when every field is reviewed. A refusal that names an expired, revoked
// or invalid token is failed:auth even when it also names a scope; only a
// remaining 403 permission_error that names a scope is unsupported. A
// throttle, a missing route or an unreviewed refusal never proves the
// credential cannot read usage.
func claudeUsageReadClass(status int, body string) (verdict, errorType string, response json.RawMessage) {
	if status == http.StatusOK {
		sanitized, err := claudeUsageSanitize(json.RawMessage(body))
		if err != nil {
			return "failed:unreviewed", "", nil
		}
		return "supported", "", sanitized
	}
	var refusal struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// The usage route's refusals omit the top-level "type":"error" that
	// inference errors carry, so only the error object is read.
	if json.Unmarshal([]byte(body), &refusal) == nil {
		switch refusal.Error.Type {
		case "permission_error", "authentication_error", "rate_limit_error", "not_found_error", "invalid_request_error", "api_error", "overloaded_error":
			errorType = refusal.Error.Type
		}
	}
	message := strings.ToLower(refusal.Error.Message)
	authCause := strings.Contains(message, "revoked") || strings.Contains(message, "expired") || strings.Contains(message, "invalid")
	switch {
	case status == http.StatusUnauthorized || (status == http.StatusForbidden && authCause):
		return "failed:auth", errorType, nil
	case status == http.StatusForbidden && errorType == "permission_error" && strings.Contains(message, "scope"):
		return "unsupported", errorType, nil
	default:
		return "failed:unreviewed", errorType, nil
	}
}

// Provider window keys mapped to the verdict's window kinds. Other windows
// (oauth apps, overage) are recorded in evidence but are not a kind #1721
// shows.
var claudeUsageWindowKinds = map[string]string{
	"five_hour": "five_hour", "seven_day": "weekly", "seven_day_opus": "model_scoped", "seven_day_sonnet": "model_scoped",
}

// claudeUsageWindows computes one path's verdict per window kind from its
// probes' sanitized answers: share only for a numeric, non-null utilization;
// status_and_reset for a status or reset without one; unsupported otherwise.
// It returns nil when the path has no probe or any probe failed, because a
// failed or unrun probe is evidence of nothing.
func claudeUsageWindows(probes []claudeUsageEvidence) map[string]string {
	if len(probes) == 0 {
		return nil
	}
	rank := map[string]int{"unsupported": 0, "status_and_reset": 1, "share": 2}
	verdicts := map[string]string{"five_hour": "unsupported", "weekly": "unsupported", "model_scoped": "unsupported"}
	raise := func(kind, verdict string) {
		if kind != "" && rank[verdict] > rank[verdicts[kind]] {
			verdicts[kind] = verdict
		}
	}
	decode := func(raw json.RawMessage) map[string]any {
		var fields map[string]any
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if decoder.Decode(&fields) != nil {
			return nil
		}
		return fields
	}
	window := func(value any, utilization, reset string) string {
		object, ok := value.(map[string]any)
		if !ok {
			return "unsupported"
		}
		if _, ok := object[utilization].(json.Number); ok {
			return "share"
		}
		if object[reset] != nil || object["status"] != nil {
			return "status_and_reset"
		}
		return "unsupported"
	}
	for _, probe := range probes {
		if probe.Verdict == "probe_failed" || strings.HasPrefix(probe.Verdict, "failed") {
			return nil
		}
		answer := decode(probe.Response)
		if limits, ok := answer["rate_limits"].(map[string]any); ok {
			answer = limits
		}
		for key, value := range answer {
			raise(claudeUsageWindowKinds[key], window(value, "utilization", "resets_at"))
		}
		for _, event := range probe.Events {
			info := decode(event)
			kind, _ := info["rateLimitType"].(string)
			raise(claudeUsageWindowKinds[kind], window(info, "utilization", "resetsAt"))
		}
	}
	return verdicts
}

func TestClaudeUsageObservedFixtures(t *testing.T) {
	zero := 0
	// idle and turn are the #1713 observations (2026-10-03); idle_profile_scope
	// and turn_seeded are #2389's (2026-11-03). The event names whichever window
	// binds at run time, so the two turns name different windows.
	unsupported := "unsupported"
	path1 := map[string]claudeUsageEvidence{}
	for _, tc := range []struct {
		mode, verdict string
		inference     int
		usage         []string
		windows       map[string]string
	}{
		{"idle", "usage_unavailable", 0, nil, map[string]string{"five_hour": unsupported, "weekly": unsupported, "model_scoped": unsupported}},
		{"idle_profile_scope", "usage_refused", 0, []string{"unsupported"}, map[string]string{"five_hour": unsupported, "weekly": unsupported, "model_scoped": unsupported}},
		{"turn", "rate_limit_event_observed", 1, nil, map[string]string{"five_hour": "status_and_reset", "weekly": unsupported, "model_scoped": unsupported}},
		{"turn_seeded", "usage_observed", 1, nil, map[string]string{"five_hour": unsupported, "weekly": "share", "model_scoped": unsupported}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			body, err := os.ReadFile("testdata/claude_usage_observed_" + tc.mode + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			c := claudeUsageCapture{Mode: tc.mode, Version: "2.1.220", Code: &zero, AuthUnchanged: true, Output: string(body)}
			e := claudeUsageAnalyze(c, tc.inference, 0, 0, tc.usage)
			if e.Verdict != tc.verdict {
				t.Fatalf("verdict = %s", e.Verdict)
			}
			if tc.mode == "idle" && (!strings.Contains(string(e.Response), `"rate_limits":null`) || !strings.Contains(string(e.Response), `"rate_limits_available":false`)) {
				t.Fatal("availability or null was lost")
			}
			if tc.mode == "turn" && (len(e.Events) != 1 || strings.Contains(string(e.Events[0]), "utilization") || !strings.Contains(string(e.Events[0]), `"overageDisabledReason":"org_level_disabled"`)) {
				t.Fatal("event fields changed or usage invented")
			}
			if tc.mode == "turn_seeded" && !strings.Contains(string(e.Response), `"seven_day":{"resets_at":"2026-11-06T03:00:00.000Z","utilization":56.99999999999999}`) {
				t.Fatalf("seeded share was lost: %s", e.Response)
			}
			if got := claudeUsageWindows([]claudeUsageEvidence{e}); !maps.Equal(got, tc.windows) {
				t.Fatalf("windows = %v, want %v", got, tc.windows)
			}
			path1[tc.mode] = e
		})
	}
	// The note's path 1 row on 2026-11-03. That day's idle answer matched
	// #1713's fixture, and its turn event matched the one in turn_seeded, so
	// the October turn is left out.
	got := claudeUsageWindows([]claudeUsageEvidence{path1["idle"], path1["idle_profile_scope"], path1["turn_seeded"]})
	if want := map[string]string{"five_hour": unsupported, "weekly": "share", "model_scoped": unsupported}; !maps.Equal(got, want) {
		t.Fatalf("path 1 windows = %v, want %v", got, want)
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
		usage                        []string
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
		{name: "profile scope correlated", edit: func(c *claudeUsageCapture) { c.Mode = "idle_profile_scope" }, usage: []string{"supported"}, want: "usage_observed"},
		{name: "profile scope inference fails", edit: func(c *claudeUsageCapture) { c.Mode = "idle_profile_scope" }, inference: 1, usage: []string{"supported"}, want: "probe_failed"},
		{name: "profile scope without its read fails", edit: func(c *claudeUsageCapture) { c.Mode = "idle_profile_scope" }, want: "probe_failed"},
		{name: "profile scope with two reads fails", edit: func(c *claudeUsageCapture) { c.Mode = "idle_profile_scope" }, usage: []string{"supported", "supported"}, want: "probe_failed"},
		{name: "refused read cannot carry windows", edit: func(c *claudeUsageCapture) { c.Mode = "idle_profile_scope" }, usage: []string{"unsupported"}, want: "probe_failed"},
		{name: "readable answer dropped by the CLI fails", edit: func(c *claudeUsageCapture) {
			c.Mode = "idle_profile_scope"
			c.Output = strings.ReplaceAll(c.Output, `{"five_hour":{"utilization":12.5,"resets_at":"2026-10-03T06:00:00Z"},"seven_day_opus":null}`, `{"rate_limits_available":true,"rate_limits":null}`)
		}, usage: []string{"supported"}, want: "probe_failed"},
		{name: "withheld after a scope refusal", edit: func(c *claudeUsageCapture) {
			c.Mode = "idle_profile_scope"
			c.Output = strings.ReplaceAll(c.Output, `{"five_hour":{"utilization":12.5,"resets_at":"2026-10-03T06:00:00Z"},"seven_day_opus":null}`, `{"rate_limits_available":true,"rate_limits":null}`)
		}, usage: []string{"unsupported"}, want: "usage_refused"},
		{name: "withheld after an auth failure fails", edit: func(c *claudeUsageCapture) {
			c.Mode = "idle_profile_scope"
			c.Output = strings.ReplaceAll(c.Output, `{"five_hour":{"utilization":12.5,"resets_at":"2026-10-03T06:00:00Z"},"seven_day_opus":null}`, `{"rate_limits_available":true,"rate_limits":null}`)
		}, usage: []string{"failed:auth"}, want: "probe_failed"},
		{name: "withheld after a throttle fails", edit: func(c *claudeUsageCapture) {
			c.Mode = "idle_profile_scope"
			c.Output = strings.ReplaceAll(c.Output, `{"five_hour":{"utilization":12.5,"resets_at":"2026-10-03T06:00:00Z"},"seven_day_opus":null}`, `{"rate_limits_available":true,"rate_limits":null}`)
		}, usage: []string{"failed:unreviewed"}, want: "probe_failed"},
		{name: "withheld after an unread body fails", edit: func(c *claudeUsageCapture) {
			c.Mode = "idle_profile_scope"
			c.Output = strings.ReplaceAll(c.Output, `{"five_hour":{"utilization":12.5,"resets_at":"2026-10-03T06:00:00Z"},"seven_day_opus":null}`, `{"rate_limits_available":true,"rate_limits":null}`)
		}, usage: []string{"failed:body"}, want: "probe_failed"},
		{name: "seeded after one turn", edit: func(c *claudeUsageCapture) {
			c.Mode = "turn_seeded"
			c.Output = strings.Replace(c.Output, "\n", "\n{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}\n", 1)
		}, inference: 1, want: "usage_observed"},
		{name: "seeded without inference fails", edit: func(c *claudeUsageCapture) {
			c.Mode = "turn_seeded"
			c.Output = strings.Replace(c.Output, "\n", "\n{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}\n", 1)
		}, want: "probe_failed"},
		{name: "seeded without a result fails", edit: func(c *claudeUsageCapture) { c.Mode = "turn_seeded" }, inference: 1, want: "probe_failed"},
		{name: "seeded with a failed turn fails", edit: func(c *claudeUsageCapture) {
			c.Mode = "turn_seeded"
			c.Output = strings.Replace(c.Output, "\n", "\n{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true}\n", 1)
		}, inference: 1, want: "probe_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			if tc.edit != nil {
				tc.edit(&c)
			}
			if got := claudeUsageAnalyze(c, tc.inference, tc.refresh, tc.failures, tc.usage); got.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s", got.Verdict, tc.want)
			}
		})
	}
	e := claudeUsageAnalyze(base, 0, 0, 0, nil)
	if !strings.Contains(string(e.Response), `"seven_day_opus":null`) || !strings.Contains(string(e.Response), `"utilization":12.5`) || strings.Contains(string(e.Response), "windowDurationMins") {
		t.Fatal("field types, nulls, or absent fields changed")
	}
	failedTurn := base
	failedTurn.Mode, failedTurn.TimedOut = "turn", true
	failedTurn.Output = "{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true}\n" +
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1791000000}}`
	if got := claudeUsageAnalyze(failedTurn, 1, 0, 1, nil); got.Verdict != "probe_failed" || len(got.Events) != 1 {
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

// The synthetic answers are evidence checks for the path 2 classifier and
// the window verdicts, never positive provider fixtures; the observed file
// holds the sanitized answers #2389's live run received.
func TestClaudeUsageReadClassification(t *testing.T) {
	type readCase struct {
		Name            string            `json:"name"`
		Status          *int              `json:"status"`
		Body            string            `json:"body"`
		TimedOut        bool              `json:"timedOut"`
		TransportFailed bool              `json:"transportFailed"`
		Want            string            `json:"want"`
		ErrorType       *string           `json:"errorType"`
		Windows         map[string]string `json:"windows"`
	}
	var cases []readCase
	for _, name := range []string{"synthetic", "observed"} {
		body, err := os.ReadFile("testdata/claude_usage_read_" + name + ".json") //nolint:gosec // name is one of two fixed fixture names
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var file []readCase
		if err := decoder.Decode(&file); err != nil || len(file) == 0 {
			t.Fatalf("%s fixture: %v", name, err)
		}
		cases = append(cases, file...)
	}
	zero := 0
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			c := claudeUsageCapture{
				Mode: "read", Version: "2.1.220", Code: &zero, AuthUnchanged: true,
				ReadStatus: tc.Status, ReadBody: tc.Body, TimedOut: tc.TimedOut, TransportFailed: tc.TransportFailed,
			}
			statuses := []int{}
			if tc.Status != nil {
				statuses = append(statuses, *tc.Status)
			}
			e := claudeUsageReadAnalyze(c, 0, 0, statuses)
			if e.Verdict != tc.Want {
				t.Fatalf("verdict = %s, want %s", e.Verdict, tc.Want)
			}
			if tc.ErrorType != nil && e.ErrorType != *tc.ErrorType {
				t.Fatalf("error type = %q", e.ErrorType)
			}
			if strings.Contains(string(e.Response)+e.ErrorType, "synthetic-secret") {
				t.Fatal("unreviewed provider text entered evidence")
			}
			windows := claudeUsageWindows([]claudeUsageEvidence{e})
			if tc.Windows == nil && windows != nil && tc.Want != "unsupported" {
				t.Fatalf("failed read produced window verdicts %v", windows)
			}
			for kind, want := range tc.Windows {
				if windows[kind] != want {
					t.Fatalf("%s = %s, want %s", kind, windows[kind], want)
				}
			}
		})
	}
	full := cases[0]
	e := claudeUsageReadAnalyze(claudeUsageCapture{Mode: "read", Version: "2.1.220", Code: &zero, AuthUnchanged: true, ReadStatus: full.Status, ReadBody: full.Body}, 0, 0, []int{200})
	if !strings.Contains(string(e.Response), `"seven_day_opus":null`) || !strings.Contains(string(e.Response), `"utilization":12.5`) || !strings.Contains(string(e.Response), `"monthly_limit":null`) {
		t.Fatal("field types or nulls changed")
	}
	for _, tc := range cases {
		if tc.Want != "unsupported" {
			continue
		}
		e := claudeUsageReadAnalyze(claudeUsageCapture{Mode: "read", Version: "2.1.220", Code: &zero, AuthUnchanged: true, ReadStatus: tc.Status, ReadBody: tc.Body}, 0, 0, []int{*tc.Status})
		if got := claudeUsageWindows([]claudeUsageEvidence{e}); got["five_hour"] != "unsupported" || got["weekly"] != "unsupported" || got["model_scoped"] != "unsupported" {
			t.Fatalf("%s windows = %v", tc.Name, got)
		}
	}
	ok := 200
	good := claudeUsageCapture{Mode: "read", Version: "2.1.220", Code: &zero, AuthUnchanged: true, ReadStatus: &ok, ReadBody: "{}"}
	for _, tc := range []struct {
		name              string
		edit              func(*claudeUsageCapture)
		refresh, failures int
		statuses          []int
	}{
		{name: "auth mutation", edit: func(c *claudeUsageCapture) { c.AuthUnchanged = false }, statuses: []int{200}},
		{name: "wrong pin", edit: func(c *claudeUsageCapture) { c.Version = "other" }, statuses: []int{200}},
		{name: "driver failure", edit: func(c *claudeUsageCapture) { c.Failure = "auth_write_permitted" }, statuses: []int{200}},
		{name: "wrong mode", edit: func(c *claudeUsageCapture) { c.Mode = "idle" }, statuses: []int{200}},
		{name: "refresh attempt", refresh: 1, statuses: []int{200}},
		{name: "blocked route", failures: 1, statuses: []int{200}},
		{name: "unseen read", statuses: []int{}},
		{name: "second read", statuses: []int{200, 200}},
		{name: "status mismatch", statuses: []int{403}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			if tc.edit != nil {
				tc.edit(&c)
			}
			e := claudeUsageReadAnalyze(c, tc.refresh, tc.failures, tc.statuses)
			if e.Verdict != "probe_failed" || claudeUsageWindows([]claudeUsageEvidence{e}) != nil {
				t.Fatalf("verdict = %s", e.Verdict)
			}
		})
	}
}

func TestClaudeUsageWindows(t *testing.T) {
	event := func(raw string) []json.RawMessage { return []json.RawMessage{json.RawMessage(raw)} }
	for _, tc := range []struct {
		name   string
		probes []claudeUsageEvidence
		want   map[string]string
	}{
		{name: "no probe", want: nil},
		{name: "failed probe", probes: []claudeUsageEvidence{{Verdict: "probe_failed"}}, want: nil},
		{name: "one failed probe voids the path", probes: []claudeUsageEvidence{
			{Verdict: "usage_observed", Response: json.RawMessage(`{"rate_limits":{"five_hour":{"utilization":1,"resets_at":null}}}`)},
			{Verdict: "failed:timeout"},
		}, want: nil},
		{
			name: "unavailable", probes: []claudeUsageEvidence{{Verdict: "usage_unavailable", Response: json.RawMessage(`{"subscription_type":null,"rate_limits_available":false,"rate_limits":null}`)}},
			want: map[string]string{"five_hour": "unsupported", "weekly": "unsupported", "model_scoped": "unsupported"},
		},
		{
			name: "status event", probes: []claudeUsageEvidence{{Verdict: "rate_limit_event_observed", Events: event(`{"status":"allowed","resetsAt":1791072000,"rateLimitType":"five_hour","overageStatus":"rejected"}`)}},
			want: map[string]string{"five_hour": "status_and_reset", "weekly": "unsupported", "model_scoped": "unsupported"},
		},
		{
			name: "warning event carries a share", probes: []claudeUsageEvidence{{Verdict: "rate_limit_event_observed", Events: event(`{"status":"allowed_warning","resetsAt":1791072000,"rateLimitType":"seven_day","utilization":0.8}`)}},
			want: map[string]string{"five_hour": "unsupported", "weekly": "share", "model_scoped": "unsupported"},
		},
		{
			name: "seeded control response", probes: []claudeUsageEvidence{{Verdict: "usage_observed", Response: json.RawMessage(`{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":4,"resets_at":"2026-10-03T06:00:00.000Z"},"seven_day":{"utilization":null,"resets_at":"2026-10-08T00:00:00.000Z"}}}`)}},
			want: map[string]string{"five_hour": "share", "weekly": "status_and_reset", "model_scoped": "unsupported"},
		},
		{name: "strongest answer wins across variants", probes: []claudeUsageEvidence{
			{Verdict: "usage_unavailable", Response: json.RawMessage(`{"rate_limits_available":false,"rate_limits":null}`)},
			{Verdict: "rate_limit_event_observed", Events: event(`{"status":"allowed","rateLimitType":"five_hour"}`)},
			{Verdict: "usage_observed", Response: json.RawMessage(`{"rate_limits_available":true,"rate_limits":{"seven_day_opus":{"utilization":0,"resets_at":null}}}`)},
		}, want: map[string]string{"five_hour": "status_and_reset", "weekly": "unsupported", "model_scoped": "share"}},
		{
			name: "unmapped windows are not kinds", probes: []claudeUsageEvidence{{Verdict: "supported", Response: json.RawMessage(`{"seven_day_oauth_apps":{"utilization":9,"resets_at":null},"extra_usage":{"utilization":9}}`)}},
			want: map[string]string{"five_hour": "unsupported", "weekly": "unsupported", "model_scoped": "unsupported"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeUsageWindows(tc.probes)
			if (got == nil) != (tc.want == nil) || len(got) != len(tc.want) {
				t.Fatalf("windows = %v, want %v", got, tc.want)
			}
			for kind, want := range tc.want {
				if got[kind] != want {
					t.Fatalf("%s = %s, want %s", kind, got[kind], want)
				}
			}
		})
	}
}

// claudeUsageProxyClient returns a client that reaches p's TLS observer as
// the probe container would.
func claudeUsageProxyClient(t *testing.T, p *claudeUsageProxy) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(p.ca)
	proxyURL, err := url.Parse(p.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func TestClaudeUsageReadProxy(t *testing.T) {
	forwarded := []string{}
	p := newClaudeUsageProxy(t, "read", "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		forwarded = append(forwarded, r.Method+" "+r.Host+r.URL.Path)
		return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error"}`)), ProtoMajor: 2}, nil
	}))
	client := claudeUsageProxyClient(t, p)
	for _, attempt := range []struct{ method, target string }{
		{http.MethodPost, "https://api.anthropic.com/v1/messages"},
		{http.MethodPost, "https://api.anthropic.com/v1/oauth/token"},
		{http.MethodGet, "https://api.anthropic.com/api/claude_code/settings"},
		{http.MethodPost, "https://api.anthropic.com/api/oauth/usage"},
		{http.MethodGet, "https://api.anthropic.com/api/oauth/usage"},
	} {
		request, err := http.NewRequest(attempt.method, attempt.target, strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		r, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	// A second authority is refused at CONNECT, before any TLS.
	if r, err := client.Get("https://claude.ai/api/oauth/usage"); err == nil {
		_ = r.Body.Close()
		t.Fatal("read mode admitted a second authority")
	}
	p.server.Close()
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(forwarded) != 1 || forwarded[0] != "GET api.anthropic.com/api/oauth/usage" {
		t.Fatalf("forwarded = %v", forwarded)
	}
	// The refused read is a provider answer; the other attempts are failures.
	if p.inference != 1 || p.refresh != 1 || p.failures != 3 || len(p.usageStatuses) != 1 || p.usageStatuses[0] != 403 {
		t.Fatalf("inference=%d refresh=%d failures=%d statuses=%v", p.inference, p.refresh, p.failures, p.usageStatuses)
	}
}

// TestClaudeUsageProxyClassifiesUsage pins that the CLI's own usage read in
// idle_profile_scope is classified by path 2's rule and relayed unchanged.
func TestClaudeUsageProxyClassifiesUsage(t *testing.T) {
	for _, tc := range []struct {
		name, encoding string
		status         int
		body, verdict  string
		failures       int
	}{
		{"scope refusal", "", 403, `{"error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`, "unsupported", 0},
		{"auth failure", "", 401, `{"error":{"type":"authentication_error","message":"synthetic-secret"}}`, "failed:auth", 0},
		{"throttle", "", 429, `{"error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`, "failed:unreviewed", 0},
		{"answer", "", 200, `{"five_hour":null}`, "supported", 0},
		{"identity encoding", "identity", 200, `{"five_hour":null}`, "supported", 0},
		{"oversized", "", 200, strings.Repeat(" ", claudeUsageReadBodyLimit+1), "failed:body", 1},
		{"undecoded encoding", "br", 403, `{"error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`, "failed:body", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newClaudeUsageProxy(t, "idle_profile_scope", "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Accept-Encoding") != "" {
					return nil, errors.New("accept-encoding forwarded")
				}
				header := http.Header{}
				if tc.encoding != "" {
					header.Set("Content-Encoding", tc.encoding)
				}
				return &http.Response{StatusCode: tc.status, Header: header, Body: io.NopCloser(strings.NewReader(tc.body)), ContentLength: -1, ProtoMajor: 2}, nil
			}))
			request, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
			r, err := claudeUsageProxyClient(t, p).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			relayed, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			p.server.Close()
			p.wg.Wait()
			p.mu.Lock()
			defer p.mu.Unlock()
			if r.StatusCode != tc.status || (tc.name != "oversized" && (err != nil || string(relayed) != tc.body)) {
				t.Fatalf("relayed %d %q", r.StatusCode, relayed)
			}
			if !slices.Equal(p.usageVerdicts, []string{tc.verdict}) || p.failures != tc.failures || strings.Contains(strings.Join(p.requests, " "), "synthetic-secret") {
				t.Fatalf("verdicts=%v failures=%d requests=%v", p.usageVerdicts, p.failures, p.requests)
			}
		})
	}
}

// TestClaudeUsageAcceptEncodingScope pins that the proxy drops the client's
// Accept-Encoding only on the usage route in the modes that classify it.
func TestClaudeUsageAcceptEncodingScope(t *testing.T) {
	for _, tc := range []struct {
		mode, path string
		dropped    bool
	}{
		{"idle_profile_scope", "/api/oauth/usage", true},
		{"read", "/api/oauth/usage", true},
		{"idle", "/api/oauth/usage", false},
		{"idle_profile_scope", "/api/claude_code/settings", false},
	} {
		t.Run(tc.mode+tc.path, func(t *testing.T) {
			var forwarded string
			p := newClaudeUsageProxy(t, tc.mode, "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				forwarded = r.Header.Get("Accept-Encoding")
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), ProtoMajor: 2}, nil
			}))
			request, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com"+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
			r, err := claudeUsageProxyClient(t, p).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
			p.server.Close()
			p.wg.Wait()
			if (forwarded == "") != tc.dropped {
				t.Fatalf("forwarded Accept-Encoding = %q", forwarded)
			}
		})
	}
}

func TestClaudeUsageSeedHeaders(t *testing.T) {
	p := newClaudeUsageProxy(t, "turn_seeded", "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		header := http.Header{}
		header.Set("anthropic-ratelimit-unified-5h-utilization", "0.04")
		header.Set("anthropic-ratelimit-unified-5h-reset", "1791072000")
		header.Set("anthropic-ratelimit-unified-synthetic-secret", "synthetic-secret")
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader("{}")), ProtoMajor: 2}, nil
	}))
	r, err := claudeUsageProxyClient(t, p).Post("https://api.anthropic.com/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	p.server.Close()
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.Join(p.seedHeaders, ",") != "5h-utilization,5h-reset" || strings.Contains(strings.Join(p.requests, " "), "0.04") {
		t.Fatalf("seed headers = %v", p.seedHeaders)
	}
}

// TestClaudeUsageReadDriver runs path 2's read child against the TLS
// observer, so the request shape the live run sends is pinned offline.
func TestClaudeUsageReadDriver(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	var seen *http.Request
	p := newClaudeUsageProxy(t, "read", "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r
		return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`)), ProtoMajor: 2}, nil
	}))
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, p.ca, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "testdata/claude_usage_read.cjs", "read") //nolint:gosec // fixed test fixture
	command.Env = []string{"PROBE_PROXY=" + p.server.URL, "PROBE_CA=" + ca, "PROBE_TOKEN=synthetic-token"}
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	p.server.Close()
	p.wg.Wait()
	var result struct {
		Status          *int   `json:"status"`
		Body            string `json:"body"`
		TimedOut        bool   `json:"timedOut"`
		TransportFailed bool   `json:"transportFailed"`
		Overflow        bool   `json:"overflow"`
	}
	if err := json.Unmarshal(output, &result); err != nil || strings.Contains(string(output), "synthetic-token") {
		t.Fatal("read result is not the bounded, token-free shape")
	}
	if result.Status == nil || *result.Status != 403 || result.TransportFailed || result.TimedOut || !strings.Contains(result.Body, "permission_error") {
		t.Fatalf("read result = %+v", result)
	}
	if seen == nil || seen.Method != http.MethodGet || seen.Host != "api.anthropic.com" || seen.URL.Path != "/api/oauth/usage" ||
		seen.Header.Get("Authorization") != "Bearer synthetic-token" || seen.Header.Get("anthropic-beta") != "oauth-2025-04-20" ||
		seen.Header.Get("User-Agent") != "claude-cli/2.1.220 (external, sdk-cli)" || seen.Header.Get("Accept") != "application/json, text/plain, */*" {
		t.Fatal("read request differs from the pinned CLI's usage read")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.usageStatuses) != 1 || p.usageStatuses[0] != 403 || p.failures != 0 || p.refresh != 0 || p.inference != 0 {
		t.Fatalf("proxy statuses=%v failures=%d", p.usageStatuses, p.failures)
	}
}

// TestClaudeUsageReadDriverVerifiesProxy pins that the read child sends the
// bearer only to a certificate the given CA signed: an interceptor on the
// host-only network gets a failed handshake, never a request.
func TestClaudeUsageReadDriverVerifiesProxy(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	seen := false
	p := newClaudeUsageProxy(t, "read", "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = true
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), ProtoMajor: 2}, nil
	}))
	other := newClaudeUsageProxy(t, "read", "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	}))
	other.server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, other.ca, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "testdata/claude_usage_read.cjs", "read") //nolint:gosec // fixed test fixture
	command.Env = []string{"PROBE_PROXY=" + p.server.URL, "PROBE_CA=" + ca, "PROBE_TOKEN=synthetic-token"}
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	p.server.Close()
	p.wg.Wait()
	var result struct {
		Status          *int `json:"status"`
		TransportFailed bool `json:"transportFailed"`
	}
	if err := json.Unmarshal(output, &result); err != nil || !result.TransportFailed || result.Status != nil || seen {
		t.Fatalf("untrusted proxy was not refused: %s seen=%v", output, seen)
	}
}

func TestClaudeUsageNonessentialRoutes(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		failures int
	}{{"idle_profile_scope", 1}, {"idle", 4}, {"read", 4}} {
		t.Run(tc.mode, func(t *testing.T) {
			forwarded := 0
			p := newClaudeUsageProxy(t, tc.mode, "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				forwarded++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), ProtoMajor: 2}, nil
			}))
			client := claudeUsageProxyClient(t, p)
			for _, path := range []string{"/api/claude_cli/bootstrap", "/api/claude_code_penguin_mode", "/mcp-registry/v0/servers", "/api/private-synthetic-secret"} {
				r, err := client.Get("https://api.anthropic.com" + path)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
			}
			p.server.Close()
			p.wg.Wait()
			p.mu.Lock()
			defer p.mu.Unlock()
			trace := strings.Join(p.requests, ",")
			if forwarded != 0 || p.failures != tc.failures || strings.Contains(trace, "synthetic-secret") || !strings.Contains(trace, "blocked GET /mcp-registry/v0/servers") {
				t.Fatalf("forwarded=%d failures=%d trace=%s", forwarded, p.failures, trace)
			}
		})
	}
}
