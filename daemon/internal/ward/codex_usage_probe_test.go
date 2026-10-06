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
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// The usage read calls the CLI's self-refreshing authentication path, which
// refreshes once expiry is five minutes away or less (upstream
// CHATGPT_ACCESS_TOKEN_REFRESH_WINDOW_MINUTES). A collection launch must stay
// outside that window for its whole bounded invocation, with room for the
// host and guest clocks to disagree.
const (
	codexUsageRefreshWindow      = 5 * time.Minute
	codexUsageInvocationDeadline = 60 * time.Second
	codexUsageSkewAllowance      = 60 * time.Second
	codexUsageLaunchFloor        = codexUsageRefreshWindow + codexUsageInvocationDeadline + codexUsageSkewAllowance

	codexUsageReadRoute = "GET /backend-api/wham/usage"
	codexUsageTurnRoute = "POST /backend-api/codex/responses"
)

// codexUsageLaunchGate decides whether a production-shaped collection may
// launch. A missing expiry defers exactly like a short one, so the caller
// never starts the app-server to find out.
func codexUsageLaunchGate(expiry *time.Time, now time.Time) (bool, time.Duration) {
	if expiry == nil || expiry.IsZero() {
		return false, 0
	}
	remaining := expiry.Sub(now)
	return remaining >= codexUsageLaunchFloor, remaining
}

func TestCodexUsageLaunchGate(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { expiry := now.Add(d); return &expiry }
	if codexUsageLaunchFloor != 7*time.Minute {
		t.Fatal("launch floor changed; rerun the live measurement and update the decision note")
	}
	for _, tc := range []struct {
		name     string
		expiry   *time.Time
		admitted bool
	}{
		{"missing", nil, false},
		{"zero", &time.Time{}, false},
		{"expired", at(-time.Second), false},
		{"inside refresh window", at(codexUsageRefreshWindow), false},
		{"refresh window plus deadline", at(codexUsageRefreshWindow + codexUsageInvocationDeadline), false},
		{"one second short", at(codexUsageLaunchFloor - time.Second), false},
		{"floor", at(codexUsageLaunchFloor), true},
		{"one hour", at(time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admitted, remaining := codexUsageLaunchGate(tc.expiry, now)
			if admitted != tc.admitted || (admitted && remaining < codexUsageLaunchFloor) {
				t.Fatalf("admitted=%t remaining=%s", admitted, remaining)
			}
		})
	}
}

// Only this closed vocabulary crosses the container boundary: field names,
// JSON types, the plan enum, counts, and flags. No usage value does.
type codexUsageCapture struct {
	Version       string            `json:"version"`
	Mode          string            `json:"mode"`
	Initialized   bool              `json:"initialized"`
	Answered      bool              `json:"answered"`
	Usage         bool              `json:"usage"`
	Fields        map[string]string `json:"fields"`
	Buckets       int               `json:"buckets"`
	CodexBucket   bool              `json:"codexBucket"`
	Plan          string            `json:"plan"`
	TurnStarted   bool              `json:"turnStarted"`
	TurnCompleted bool              `json:"turnCompleted"`
	TurnFailed    bool              `json:"turnFailed"`
	Updated       int               `json:"updated"`
	UpdatedFields map[string]string `json:"updatedFields"`
	InitSeconds   int               `json:"initSeconds"`
	ReadSeconds   int               `json:"readSeconds"`
	TotalSeconds  int               `json:"totalSeconds"`
	// Container clock at session start minus host clock at the last gate
	// check: launch latency plus clock offset, both spent from the skew
	// allowance.
	LaunchSeconds int  `json:"launchSeconds"`
	RPCError      bool `json:"rpcError"`
	Malformed     bool `json:"malformed"`
	TimedOut      bool `json:"timedOut"`
	Overflow      bool `json:"overflow"`
	DriverFailed  bool `json:"driverFailed"`
	AuthUnchanged bool `json:"authUnchanged"`
	SymlinkSame   bool `json:"symlinkSame"`
	AppendDenied  bool `json:"appendDenied"`
	UnlinkDenied  bool `json:"unlinkDenied"`
}

// Gate is "admitted" or "deferred" for a real credential and "not_applied"
// for the synthetic experiment. HostStore is "unchanged" or "changed" for a
// real credential and "not_read" otherwise. A deferred launch has no capture.
type codexUsageEvidence struct {
	Case       string             `json:"case"`
	Credential string             `json:"credential"`
	Gate       string             `json:"gate"`
	HostStore  string             `json:"hostStore"`
	Capture    *codexUsageCapture `json:"capture"`
	Requests   []string           `json:"requests"`
	Failures   int                `json:"failures"`
	Verdict    string             `json:"verdict"`
}

var codexUsageSnapshotTypes = map[string]string{ //nolint:gosec // G101: response field names and JSON types, not a credential
	"":                                  "object",
	".limitId":                          "string null absent",
	".limitName":                        "string null absent",
	".spendControlReached":              "boolean null absent",
	".planType":                         "string null absent",
	".rateLimitReachedType":             "string null absent",
	".primary":                          "object null absent",
	".primary.usedPercent":              "integer",
	".primary.windowDurationMins":       "integer null absent",
	".primary.resetsAt":                 "integer null absent",
	".secondary":                        "object null absent",
	".secondary.usedPercent":            "integer",
	".secondary.windowDurationMins":     "integer null absent",
	".secondary.resetsAt":               "integer null absent",
	".credits":                          "object null absent",
	".credits.hasCredits":               "boolean",
	".credits.unlimited":                "boolean",
	".credits.balance":                  "string null absent",
	".individualLimit":                  "object null absent",
	".individualLimit.limit":            "string",
	".individualLimit.used":             "string",
	".individualLimit.remainingPercent": "integer",
	".individualLimit.resetsAt":         "integer",
}

// codexUsageFieldsValid mirrors testdata/codex_usage_sanitize.jq against the
// upstream v2 schema at rust-v0.147.0. A notification carries one snapshot;
// a read also carries the per-limit buckets and the reset-credit summary.
func codexUsageFieldsValid(fields map[string]string, notification bool) bool {
	reviewed := map[string]string{}
	for suffix, types := range codexUsageSnapshotTypes {
		reviewed["rateLimits"+suffix] = types
		if !notification {
			reviewed["rateLimitsByLimitId.*"+suffix] = types
		}
	}
	if !notification {
		reviewed["rateLimitsByLimitId"] = "object null absent"
		reviewed["rateLimitResetCredits"] = "object null absent"
		reviewed["rateLimitResetCredits.availableCount"] = "integer"
		reviewed["rateLimitResetCredits.credits"] = "array null absent"
	}
	if fields["rateLimits"] != "object" {
		return false
	}
	for key, value := range fields {
		allowed, ok := reviewed[key]
		observed := strings.Split(value, "|")
		if !ok || !slices.IsSorted(observed) || len(slices.Compact(slices.Clone(observed))) != len(observed) {
			return false
		}
		for _, kind := range observed {
			if !slices.Contains(strings.Fields(allowed), kind) {
				return false
			}
		}
	}
	return true
}

func codexUsagePlanValid(plan string) bool {
	switch plan {
	case "", "free", "go", "plus", "pro", "prolite", "team", "self_serve_business_prolite", "self_serve_business_usage_based", "business", "ent26", "enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise", "edu", "unknown":
		return true
	default:
		return false
	}
}

func codexUsageCaptureValid(c codexUsageCapture) bool {
	if c.Version != "0.147.0" || (c.Mode != "idle" && c.Mode != "turn") || !codexUsagePlanValid(c.Plan) ||
		c.Buckets < 0 || c.Updated < 0 || c.InitSeconds < 0 || c.ReadSeconds < 0 || c.TotalSeconds < 0 {
		return false
	}
	if c.Usage != (len(c.Fields) > 0) || (c.Usage && !codexUsageFieldsValid(c.Fields, false)) ||
		(!c.Usage && (c.Buckets != 0 || c.CodexBucket || c.Plan != "")) {
		return false
	}
	return (c.Updated > 0) == (len(c.UpdatedFields) > 0) && (c.Updated == 0 || codexUsageFieldsValid(c.UpdatedFields, true))
}

func codexUsageDecode(body []byte) (codexUsageCapture, error) {
	var c codexUsageCapture
	err := strictjson.Decode(body, &c, strictjson.RejectInvalidUTF8, strictjson.Limit(16<<10))
	if err == nil && !codexUsageCaptureValid(c) {
		err = errors.New("unreviewed capture fields or CLI version")
	}
	return c, err
}

var codexUsageForwardedLabel = regexp.MustCompile(`^(?:` + regexp.QuoteMeta(codexUsageReadRoute) + `|` + regexp.QuoteMeta(codexUsageTurnRoute) +
	`)(?: (?:relay_failed|upstream_status_[1-5][0-9]{2}|upstream_transport_failed(?::(?:timeout|certificate|no such host|connection refused|EOF|connection reset))?))?$`)

func codexUsageLabelFailed(label string) bool {
	return label == "transport_failed" || label == "tls_failed" || label == "http_decode_failed" ||
		label == "auth_handshake_closed" || label == "auth_connection_closed" ||
		strings.HasSuffix(label, " relay_failed") || strings.Contains(label, " upstream_transport_failed")
}

// codexUsageAnalyze returns pass, fail, probe_failed, event_not_observed, or
// deferred. fail is a safety or support finding: a token-endpoint POST on a
// launch that was meant to avoid one, a lost protection, or a provider that
// refuses the access-only credential. probe_failed is a broken or incomplete
// measurement and proves nothing either way.
func codexUsageAnalyze(e codexUsageEvidence) string {
	if e.Gate == "deferred" {
		// The gate refused before any launch; anything observed contradicts that.
		if e.Credential != "real" || e.Capture != nil || len(e.Requests) != 0 || e.Failures != 0 {
			return "probe_failed"
		}
		return "deferred"
	}
	real := e.Case == "real_idle" || e.Case == "real_turn"
	synthetic := e.Case == "synthetic_fresh" || e.Case == "synthetic_near_expiry"
	if (!real && !synthetic) || e.Capture == nil ||
		(real && (e.Credential != "real" || e.Gate != "admitted" || (e.HostStore != "unchanged" && e.HostStore != "changed"))) ||
		(synthetic && (e.Credential != "synthetic" || e.Gate != "not_applied" || e.HostStore != "not_read")) {
		return "probe_failed"
	}
	c := *e.Capture
	refresh, refused, failures := false, false, 0
	for _, label := range e.Requests {
		switch {
		case label == "blocked_refresh":
			refresh = true
		case label == "blocked_other" || label == "connection_closed" || label == "handshake_closed":
		case codexUsageLabelFailed(label) && (!strings.Contains(label, " ") || codexUsageForwardedLabel.MatchString(label)):
			failures++
		case codexUsageForwardedLabel.MatchString(label):
			refused = refused || label == codexUsageReadRoute+" upstream_status_401" || label == codexUsageReadRoute+" upstream_status_403"
		default:
			return "probe_failed"
		}
	}
	wantMode := "idle"
	if e.Case == "real_turn" {
		wantMode = "turn"
	}
	// A launch that took longer than the skew allowance may have run inside
	// the refresh window, so a refresh it shows says nothing about the gate.
	skew := int(codexUsageSkewAllowance / time.Second)
	if failures != e.Failures || !codexUsageCaptureValid(c) || c.Mode != wantMode ||
		c.DriverFailed || c.Malformed || c.Overflow || !c.Initialized || c.LaunchSeconds > skew || c.LaunchSeconds < -skew {
		return "probe_failed"
	}
	if !c.AuthUnchanged || !c.SymlinkSame || !c.AppendDenied || !c.UnlinkDenied || e.HostStore == "changed" {
		return "fail"
	}
	if e.Case == "synthetic_near_expiry" {
		// This case is the detector control: inside the refresh window the
		// usage read must show the token-endpoint POST the gate schedules around.
		if refresh && c.Answered {
			return "pass"
		}
		return "probe_failed"
	}
	// A transport error prevents proving absence; it cannot erase a refresh
	// that was decoded and blocked.
	if refresh {
		return "fail"
	}
	if real && refused && c.Answered && c.RPCError && failures == 0 {
		return "fail"
	}
	// An answer the proxy did not carry came by a route this measurement
	// cannot see.
	if failures != 0 || c.TimedOut || !c.Answered || c.RPCError || !c.Usage || !slices.Contains(e.Requests, codexUsageReadRoute) {
		return "probe_failed"
	}
	if e.Case != "real_turn" {
		return "pass"
	}
	if !c.TurnStarted || !c.TurnCompleted || c.TurnFailed || !slices.Contains(e.Requests, codexUsageTurnRoute) {
		return "probe_failed"
	}
	if c.Updated == 0 {
		return "event_not_observed"
	}
	return "pass"
}

// codexUsageOverall needs every case. The synthetic cases prove only refresh
// behavior, so without both real cases the result is never pass. A fail is a
// positive finding, so an incomplete measurement elsewhere cannot hide it.
func codexUsageOverall(verdicts map[string]string) string {
	cases := []string{"synthetic_fresh", "synthetic_near_expiry", "real_idle", "real_turn"}
	for _, verdict := range []string{"fail", "", "probe_failed", "deferred", "event_not_observed"} {
		for _, name := range cases {
			if verdicts[name] == verdict {
				if verdict == "" {
					return "probe_failed"
				}
				return verdict
			}
		}
	}
	return "pass"
}

// The CLI's only route out is this proxy on a host-only network. It
// terminates TLS with an ephemeral test CA, forwards the reviewed usage read
// (and, in turn mode, the inference call) to chatgpt.com with provider TLS
// verified normally, and answers everything else 403 itself. Nothing is ever
// forwarded to auth.openai.com. No header, body, query, or unreviewed path
// is recorded.
type codexUsageProxy struct {
	mu          sync.Mutex
	requests    []string
	failures    int
	active      map[net.Conn]bool
	wg          sync.WaitGroup
	subnet      *net.IPNet
	certificate tls.Certificate
	ca          []byte
	turn        bool
	transport   http.RoundTripper
	server      *httptest.Server
}

func newCodexUsageProxy(t *testing.T, turn bool, subnet string, transport http.RoundTripper) *codexUsageProxy {
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
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Freeside usage spike"},
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
	// Rust's verifier rejects a CA used as a server leaf, so the root and the
	// server certificate stay separate.
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Freeside usage proxy"},
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
	if transport == nil {
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		transport = &http.Transport{Protocols: protocols, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	}
	p := &codexUsageProxy{subnet: network, certificate: cert, ca: ca, turn: turn, transport: transport, active: map[net.Conn]bool{}, requests: []string{}}
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
		if tr, ok := p.transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	})
	return p
}

// codexUsageRequestLabel names a request by a fixed string. Only an exact
// reviewed route on chatgpt.com is forwarded, and its label is that route: a
// query or an escaped path would forward something the label does not name.
// A token-endpoint POST on either admitted host is a refresh and is never
// forwarded.
func codexUsageRequestLabel(connectHost string, turn bool, method string, target *url.URL) (string, bool) {
	if label := codexAccountRequestLabel(method, target.Path); label == "blocked_refresh" {
		return label, false
	}
	route := method + " " + target.Path
	exact := target.RawPath == "" && target.RawQuery == "" && !target.ForceQuery
	if connectHost == "chatgpt.com:443" && exact && (route == codexUsageReadRoute || (turn && route == codexUsageTurnRoute)) {
		return route, true
	}
	return "blocked_other", false
}

func (p *codexUsageProxy) record(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, label)
	// A denied route is the intended policy, and an upstream status is the
	// provider's answer. Only a broken exchange counts against the measurement.
	if codexUsageLabelFailed(label) {
		p.failures++
	}
}

func (p *codexUsageProxy) serve(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !p.subnet.Contains(net.ParseIP(host)) {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodConnect {
		p.record(codexAccountRequestLabel(r.Method, r.URL.Path))
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
	// Register before hijacking removes this handler from Server.Close's wait.
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
	_ = conn.SetDeadline(time.Now().Add(codexUsageInvocationDeadline + 10*time.Second))
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		p.record("transport_failed")
		return
	}
	secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{p.certificate}, MinVersion: tls.VersionTLS12})
	// A peer that hangs up before sending a request transmitted none, but it
	// may have meant to. On the auth host that request could have been a
	// refresh, so the hang-up counts against the measurement. On chatgpt.com
	// a lost request shows up as a failed read or turn instead. The pinned
	// CLI abandons handshakes routinely; the hang-up arrives as a reset when
	// the proxy's flight was still unread.
	closed := func(label string) {
		if r.Host == "auth.openai.com:443" {
			label = "auth_" + label
		}
		p.record(label)
	}
	if err := secure.Handshake(); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
			closed("handshake_closed")
		} else {
			p.record("tls_failed")
		}
		return
	}
	// The forwarded body is delimited by connection close. Without a TLS
	// close_notify the pinned Rust client discards it as truncated.
	defer func() { _ = secure.Close() }()
	reader := bufio.NewReader(secure)
	// Prove zero HTTP bytes before accepting a shutdown EOF. ReadRequest can
	// return EOF after consuming an unterminated first line; that is a broken
	// measurement, not an idle connection.
	if _, err := reader.Peek(1); errors.Is(err, io.EOF) {
		closed("connection_closed")
		return
	}
	request, err := http.ReadRequest(reader)
	if err != nil {
		p.record("http_decode_failed")
		return
	}
	defer func() { _ = request.Body.Close() }()
	label, forward := codexUsageRequestLabel(r.Host, p.turn, request.Method, request.URL)
	p.record(label)
	if !forward {
		_, _ = io.WriteString(secure, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexUsageInvocationDeadline)
	defer cancel()
	request = request.WithContext(ctx)
	// The upstream authority is fixed here, never taken from the inner request.
	request.URL.Scheme, request.URL.Host = "https", "chatgpt.com"
	request.Host, request.RequestURI = "chatgpt.com", ""
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Connection")
	request.Body = http.MaxBytesReader(nil, request.Body, 4<<20)
	response, err := p.transport.RoundTrip(request)
	if err != nil {
		failure := "upstream_transport_failed"
		for _, category := range []string{"timeout", "certificate", "no such host", "connection refused", "EOF", "connection reset"} {
			if strings.Contains(err.Error(), category) {
				failure += ":" + category
				break
			}
		}
		p.record(label + " " + failure)
		_, _ = io.WriteString(secure, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 400 {
		p.record(fmt.Sprintf("%s upstream_status_%d", label, response.StatusCode))
	}
	// The CLI-facing connection speaks HTTP/1.1 whatever upstream negotiated,
	// and carries one request so every request is labeled.
	response.Close = true
	response.Proto, response.ProtoMajor, response.ProtoMinor = "HTTP/1.1", 1, 1
	if err := response.Write(secure); err != nil {
		p.record(label + " relay_failed")
	}
}

func codexUsageProxyClient(t *testing.T, p *codexUsageProxy) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(p.ca)
	proxyURL, err := url.Parse(p.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestCodexUsageProxyRoutes(t *testing.T) {
	for _, turn := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "turn"}[turn], func(t *testing.T) {
			var forwarded []string
			p := newCodexUsageProxy(t, turn, "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Host != "chatgpt.com" || r.URL.Scheme != "https" || r.URL.Host != "chatgpt.com" {
					t.Error("forwarded request changed the provider authority")
				}
				forwarded = append(forwarded, r.Method+" "+r.URL.Path)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), ProtoMajor: 2}, nil
			}))
			client := codexUsageProxyClient(t, p)
			cases := []struct {
				method, target, label string
				forward               bool
			}{
				{http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", codexUsageReadRoute, true},
				{http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", "blocked_other", false},
				{http.MethodPost, "https://auth.openai.com/oauth/token?secret=never-log", "blocked_refresh", false},
				{http.MethodPost, "https://chatgpt.com/oauth/token", "blocked_refresh", false},
				{http.MethodPost, "http://auth.openai.com/oauth/token", "blocked_refresh", false},
				{http.MethodGet, "https://auth.openai.com/oauth/token", "blocked_other", false},
				{http.MethodGet, "https://auth.openai.com/backend-api/wham/usage", "blocked_other", false},
				{http.MethodPost, "https://chatgpt.com/backend-api/wham/usage", "blocked_other", false},
				{http.MethodGet, "https://chatgpt.com/backend-api/wham/usage/private-id", "blocked_other", false},
				{http.MethodGet, "https://chatgpt.com/prefix/backend-api/wham/usage", "blocked_other", false},
				{http.MethodGet, "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", "blocked_other", false},
				{http.MethodPost, "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", "blocked_other", false},
				{http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", "blocked_other", false},
				{http.MethodGet, "http://chatgpt.com/backend-api/wham/usage", "blocked_other", false},
				{http.MethodGet, "https://chatgpt.com/backend-api/wham/usage?account=other", "blocked_other", false},
				{http.MethodGet, "https://chatgpt.com/backend-api/wham/usage?", "blocked_other", false},
				{http.MethodGet, "https://chatgpt.com/backend-api/wham%2Fusage", "blocked_other", false},
			}
			if turn {
				cases[1].label, cases[1].forward = codexUsageTurnRoute, true
			}
			var wantLabels, wantForwarded []string
			for _, tc := range cases {
				request, err := http.NewRequest(tc.method, tc.target, strings.NewReader(`{"refresh_token":"synthetic-secret"}`))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer synthetic-secret")
				response, err := client.Do(request)
				if err != nil {
					t.Fatal("proxy request failed")
				}
				body, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				wantStatus, wantBody := http.StatusForbidden, ""
				if tc.forward {
					wantStatus, wantBody = http.StatusOK, "{}"
					wantForwarded = append(wantForwarded, tc.label)
				}
				if err != nil || response.ProtoMajor != 1 || response.StatusCode != wantStatus || (tc.forward && string(body) != wantBody) {
					t.Fatalf("%s %s: status %d", tc.method, tc.label, response.StatusCode)
				}
				wantLabels = append(wantLabels, tc.label)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if !slices.Equal(p.requests, wantLabels) || !slices.Equal(forwarded, wantForwarded) || p.failures != 0 {
				t.Fatalf("labels %q forwarded %q failures %d", p.requests, forwarded, p.failures)
			}
		})
	}
}

func TestCodexUsageProxyUpstream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		err      error
		want     string
		failures int
	}{
		{"refused", 401, nil, codexUsageReadRoute + " upstream_status_401", 0},
		{"throttled", 429, nil, codexUsageReadRoute + " upstream_status_429", 0},
		{"reset", 0, errors.New("read: connection reset by peer; token=synthetic-secret"), codexUsageReadRoute + " upstream_transport_failed:connection reset", 1},
		{"uncategorized", 0, errors.New("synthetic-secret"), codexUsageReadRoute + " upstream_transport_failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newCodexUsageProxy(t, false, "127.0.0.0/8", codexAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic-secret")), ProtoMajor: 2}, nil
			}))
			response, err := codexUsageProxyClient(t, p).Get("https://chatgpt.com/backend-api/wham/usage")
			if err != nil {
				t.Fatal("proxy request failed")
			}
			_ = response.Body.Close()
			p.mu.Lock()
			defer p.mu.Unlock()
			if !slices.Equal(p.requests, []string{codexUsageReadRoute, tc.want}) || p.failures != tc.failures ||
				!codexUsageForwardedLabel.MatchString(tc.want) {
				t.Fatalf("labels %q failures %d", p.requests, p.failures)
			}
		})
	}
}

func TestCodexUsageProxyDeniesOtherHosts(t *testing.T) {
	p := newCodexUsageProxy(t, true, "127.0.0.0/8", codexAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("request for another host reached the upstream transport")
		return nil, errors.New("denied")
	}))
	client := codexUsageProxyClient(t, p)
	for _, target := range []string{"https://api.openai.com/backend-api/wham/usage", "https://chatgpt.com:8443/backend-api/wham/usage"} {
		if response, err := client.Get(target); err == nil {
			_ = response.Body.Close()
			t.Fatal("CONNECT to an unreviewed authority was accepted")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Equal(p.requests, []string{"blocked_other", "blocked_other"}) || p.failures != 0 {
		t.Fatalf("labels %q", p.requests)
	}
}

func codexUsageProxyTunnel(t *testing.T, p *codexUsageProxy, authority string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", p.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil {
		t.Fatal(err)
	}
	return conn
}

func codexUsageProxySecure(t *testing.T, p *codexUsageProxy, authority string) *tls.Conn {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(p.ca)
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		t.Fatal(err)
	}
	secure := tls.Client(codexUsageProxyTunnel(t, p, authority), &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12})
	if err := secure.Handshake(); err != nil {
		t.Fatal(err)
	}
	return secure
}

// The inner request cannot choose where a forwarded route goes.
func TestCodexUsageProxyFixesAuthority(t *testing.T) {
	var authorities []string
	p := newCodexUsageProxy(t, false, "127.0.0.0/8", codexAuthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		authorities = append(authorities, r.URL.Scheme+"://"+r.URL.Host+" "+r.Host)
		return codexUsageSyntheticUpstream(r)
	}))
	for _, request := range []string{
		"GET /backend-api/wham/usage HTTP/1.1\r\nHost: other.example\r\n\r\n",
		"GET https://other.example/backend-api/wham/usage HTTP/1.1\r\nHost: other.example\r\n\r\n",
	} {
		secure := codexUsageProxySecure(t, p, "chatgpt.com:443")
		if _, err := io.WriteString(secure, request); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(secure); err != nil {
			t.Fatal(err)
		}
	}
	p.wg.Wait()
	want := "https://chatgpt.com chatgpt.com"
	if !slices.Equal(authorities, []string{want, want}) {
		t.Fatalf("forwarded to %q", authorities)
	}
}

func TestCodexUsageProxyHandshake(t *testing.T) {
	for _, tc := range []struct {
		name, authority, sent, want string
		reset                       bool
		failures                    int
	}{
		{"hung up", "chatgpt.com:443", "", "handshake_closed", false, 0},
		{"reset", "chatgpt.com:443", "", "handshake_closed", true, 0},
		{"hung up on the auth host", "auth.openai.com:443", "", "auth_handshake_closed", false, 1},
		{"reset on the auth host", "auth.openai.com:443", "", "auth_handshake_closed", true, 1},
		{"not TLS", "chatgpt.com:443", "GET / HTTP/1.1\r\n\r\n", "tls_failed", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newCodexUsageProxy(t, false, "127.0.0.0/8", nil)
			conn := codexUsageProxyTunnel(t, p, tc.authority)
			if _, err := io.WriteString(conn, tc.sent); err != nil {
				t.Fatal(err)
			}
			if tc.reset {
				// Zero linger turns the close into a reset.
				if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
					t.Fatal(err)
				}
			}
			_ = conn.Close()
			p.wg.Wait()
			p.mu.Lock()
			defer p.mu.Unlock()
			if !slices.Equal(p.requests, []string{tc.want}) || p.failures != tc.failures {
				t.Fatalf("labels %q failures %d", p.requests, p.failures)
			}
		})
	}
}

// The forwarded body has no length and ends with the connection. A client
// that requires a TLS close_notify, as the pinned CLI does, must get one.
func TestCodexUsageProxyClosesCleanly(t *testing.T) {
	p := newCodexUsageProxy(t, false, "127.0.0.0/8", codexAuthRoundTripFunc(codexUsageSyntheticUpstream))
	secure := codexUsageProxySecure(t, p, "chatgpt.com:443")
	if _, err := io.WriteString(secure, "GET /backend-api/wham/usage HTTP/1.1\r\nHost: chatgpt.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(secure)
	if err != nil || !bytes.HasSuffix(body, []byte(codexUsageSyntheticBody)) || bytes.Contains(body, []byte("Content-Length")) {
		t.Fatalf("forwarded body was truncated or not close-delimited: %v", err)
	}
}

func TestCodexUsageProxyClosedConnection(t *testing.T) {
	for _, tc := range []struct {
		name, authority, fragment, want string
		failures                        int
	}{
		{"idle", "chatgpt.com:443", "", "connection_closed", 0},
		{"idle on the auth host", "auth.openai.com:443", "", "auth_connection_closed", 1},
		{"partial request", "chatgpt.com:443", "GET /backend-api/", "http_decode_failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newCodexUsageProxy(t, false, "127.0.0.0/8", nil)
			secure := codexUsageProxySecure(t, p, tc.authority)
			if _, err := io.WriteString(secure, tc.fragment); err != nil {
				t.Fatal(err)
			}
			_ = secure.Close()
			p.wg.Wait()
			p.mu.Lock()
			defer p.mu.Unlock()
			if !slices.Equal(p.requests, []string{tc.want}) || p.failures != tc.failures {
				t.Fatalf("labels %q failures %d", p.requests, p.failures)
			}
		})
	}
}

func codexUsageGoodCapture(mode string) *codexUsageCapture {
	c := &codexUsageCapture{
		Version: "0.147.0", Mode: mode, Initialized: true, Answered: true, Usage: true, Buckets: 1, CodexBucket: true, Plan: "plus",
		Fields: map[string]string{
			"rateLimits": "object", "rateLimits.limitId": "string", "rateLimits.primary": "object", "rateLimits.primary.usedPercent": "integer",
			"rateLimitsByLimitId": "object", "rateLimitsByLimitId.*": "object", "rateLimitsByLimitId.*.secondary": "null|object",
			"rateLimitResetCredits": "null",
		},
		UpdatedFields: map[string]string{},
		AuthUnchanged: true, SymlinkSame: true, AppendDenied: true, UnlinkDenied: true,
	}
	if mode == "turn" {
		c.TurnStarted, c.TurnCompleted, c.Updated = true, true, 2
		c.UpdatedFields = map[string]string{"rateLimits": "object", "rateLimits.limitId": "null|string", "rateLimits.primary": "object"}
	}
	return c
}

func TestCodexUsageVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name, kind, want string
		mutate           func(*codexUsageEvidence)
	}{
		{name: "fresh read", kind: "synthetic_fresh", want: "pass"},
		{name: "denied ancillary route", kind: "synthetic_fresh", want: "pass", mutate: func(e *codexUsageEvidence) {
			e.Requests = append(e.Requests, "blocked_other", "connection_closed", "handshake_closed")
		}},
		{name: "fresh refresh", kind: "synthetic_fresh", want: "fail", mutate: func(e *codexUsageEvidence) { e.Requests = append(e.Requests, "blocked_refresh") }},
		{name: "fresh refused is not provider evidence", kind: "synthetic_fresh", want: "probe_failed", mutate: func(e *codexUsageEvidence) {
			e.Requests, e.Capture.RPCError = append(e.Requests, codexUsageReadRoute+" upstream_status_401"), true
			e.Capture.Usage, e.Capture.Fields, e.Capture.Buckets, e.Capture.CodexBucket, e.Capture.Plan = false, map[string]string{}, 0, false, ""
		}},
		{name: "detector", kind: "synthetic_near_expiry", want: "pass", mutate: func(e *codexUsageEvidence) { e.Requests = append(e.Requests, "blocked_refresh") }},
		{name: "detector with read error", kind: "synthetic_near_expiry", want: "pass", mutate: func(e *codexUsageEvidence) {
			e.Requests, e.Capture.RPCError = []string{"blocked_refresh"}, true
			e.Capture.Usage, e.Capture.Fields, e.Capture.Buckets, e.Capture.CodexBucket, e.Capture.Plan = false, map[string]string{}, 0, false, ""
		}},
		{name: "detector unproven", kind: "synthetic_near_expiry", want: "probe_failed"},
		{name: "detector did not answer", kind: "synthetic_near_expiry", want: "probe_failed", mutate: func(e *codexUsageEvidence) {
			e.Requests, e.Capture.Answered, e.Capture.TimedOut = []string{"blocked_refresh"}, false, true
			e.Capture.Usage, e.Capture.Fields, e.Capture.Buckets, e.Capture.CodexBucket, e.Capture.Plan = false, map[string]string{}, 0, false, ""
		}},
		{name: "real read", kind: "real_idle", want: "pass"},
		{name: "real refresh", kind: "real_idle", want: "fail", mutate: func(e *codexUsageEvidence) { e.Requests = append(e.Requests, "blocked_refresh") }},
		{name: "real refresh with transport failure", kind: "real_idle", want: "fail", mutate: func(e *codexUsageEvidence) {
			e.Requests, e.Failures = append(e.Requests, "blocked_refresh", "tls_failed"), 1
		}},
		{name: "provider refuses access-only credential", kind: "real_idle", want: "fail", mutate: func(e *codexUsageEvidence) {
			e.Requests, e.Capture.RPCError = append(e.Requests, codexUsageReadRoute+" upstream_status_401"), true
			e.Capture.Usage, e.Capture.Fields, e.Capture.Buckets, e.Capture.CodexBucket, e.Capture.Plan = false, map[string]string{}, 0, false, ""
		}},
		{name: "provider throttles", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) {
			e.Requests, e.Capture.RPCError = append(e.Requests, codexUsageReadRoute+" upstream_status_429"), true
			e.Capture.Usage, e.Capture.Fields, e.Capture.Buckets, e.Capture.CodexBucket, e.Capture.Plan = false, map[string]string{}, 0, false, ""
		}},
		{name: "blocked read is not a usage observation", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) {
			e.Requests, e.Capture.RPCError = []string{"blocked_other"}, true
			e.Capture.Usage, e.Capture.Fields, e.Capture.Buckets, e.Capture.CodexBucket, e.Capture.Plan = false, map[string]string{}, 0, false, ""
		}},
		{name: "transport", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) {
			e.Requests, e.Failures = append(e.Requests, codexUsageReadRoute+" upstream_transport_failed:EOF"), 1
		}},
		{name: "failure count disagrees with labels", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Failures = 1 }},
		{name: "unreviewed label", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) {
			e.Requests = append(e.Requests, "GET /backend-api/private-id")
		}},
		{name: "auth host hung up", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) {
			e.Requests, e.Failures = append(e.Requests, "auth_connection_closed", "auth_handshake_closed"), 2
		}},
		{name: "answer the proxy did not carry", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Requests = []string{"blocked_other"} }},
		{name: "not initialized", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.Initialized = false }},
		{name: "slow launch", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) {
			e.Requests, e.Capture.LaunchSeconds = append(e.Requests, "blocked_refresh"), 61
		}},
		{name: "clock far behind", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.LaunchSeconds = -61 }},
		{name: "launch at the allowance", kind: "real_idle", want: "pass", mutate: func(e *codexUsageEvidence) { e.Capture.LaunchSeconds = 60 }},
		{name: "timeout", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.TimedOut = true }},
		{name: "overflow", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.Overflow = true }},
		{name: "parse", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.Malformed = true }},
		{name: "driver", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.DriverFailed = true }},
		{name: "unreviewed field", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.Fields["rateLimits.accountId"] = "string" }},
		{name: "unreviewed type", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.Fields["rateLimits.primary.usedPercent"] = "string" }},
		{name: "unreviewed plan", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.Plan = "private-id" }},
		{name: "wrong mode", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.Mode = "turn" }},
		{name: "write", kind: "real_idle", want: "fail", mutate: func(e *codexUsageEvidence) { e.Capture.AuthUnchanged = false }},
		{name: "replace symlink", kind: "real_idle", want: "fail", mutate: func(e *codexUsageEvidence) { e.Capture.SymlinkSame = false }},
		{name: "unprotected snapshot", kind: "real_idle", want: "fail", mutate: func(e *codexUsageEvidence) { e.Capture.AppendDenied = false }},
		{name: "removable snapshot", kind: "real_idle", want: "fail", mutate: func(e *codexUsageEvidence) { e.Capture.UnlinkDenied = false }},
		{name: "host store changed", kind: "real_idle", want: "fail", mutate: func(e *codexUsageEvidence) { e.HostStore = "changed" }},
		{name: "real launch without the gate", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Gate = "not_applied" }},
		{name: "synthetic claims in a real case", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Credential = "synthetic" }},
		{name: "deferred", kind: "real_idle", want: "deferred", mutate: func(e *codexUsageEvidence) { e.Gate, e.Capture, e.Requests = "deferred", nil, []string{} }},
		{name: "deferred but launched", kind: "real_idle", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Gate = "deferred" }},
		{name: "turn with notifications", kind: "real_turn", want: "pass"},
		{name: "turn without notification", kind: "real_turn", want: "event_not_observed", mutate: func(e *codexUsageEvidence) {
			e.Capture.Updated, e.Capture.UpdatedFields = 0, map[string]string{}
		}},
		{name: "turn failed", kind: "real_turn", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.TurnFailed = true }},
		{name: "turn incomplete", kind: "real_turn", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.TurnCompleted = false }},
		{name: "turn not started", kind: "real_turn", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Capture.TurnStarted = false }},
		{name: "turn the proxy did not carry", kind: "real_turn", want: "probe_failed", mutate: func(e *codexUsageEvidence) { e.Requests = []string{codexUsageReadRoute} }},
		{name: "turn refresh", kind: "real_turn", want: "fail", mutate: func(e *codexUsageEvidence) { e.Requests = append(e.Requests, "blocked_refresh") }},
		{name: "notification with bucket fields", kind: "real_turn", want: "probe_failed", mutate: func(e *codexUsageEvidence) {
			e.Capture.UpdatedFields["rateLimitsByLimitId"] = "object"
		}},
		{name: "unknown case", kind: "control", want: "probe_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := codexUsageEvidence{Case: tc.kind, Credential: "synthetic", Gate: "not_applied", HostStore: "not_read", Requests: []string{codexUsageReadRoute}}
			mode := "idle"
			if strings.HasPrefix(tc.kind, "real_") {
				e.Credential, e.Gate, e.HostStore = "real", "admitted", "unchanged"
			}
			if tc.kind == "real_turn" {
				mode, e.Requests = "turn", append(e.Requests, codexUsageTurnRoute)
			}
			e.Capture = codexUsageGoodCapture(mode)
			if tc.mutate != nil {
				tc.mutate(&e)
			}
			if got := codexUsageAnalyze(e); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCodexUsageOverall(t *testing.T) {
	for _, tc := range []struct {
		want                          string
		fresh, nearExpiry, idle, turn string
	}{
		{"pass", "pass", "pass", "pass", "pass"},
		{"probe_failed", "pass", "pass", "", ""},
		{"probe_failed", "pass", "probe_failed", "pass", "pass"},
		{"fail", "pass", "pass", "fail", "probe_failed"},
		{"fail", "pass", "pass", "", "fail"},
		{"probe_failed", "pass", "pass", "deferred", "probe_failed"},
		{"fail", "pass", "pass", "fail", "deferred"},
		{"fail", "fail", "pass", "pass", "pass"},
		{"deferred", "pass", "pass", "deferred", "deferred"},
		{"deferred", "pass", "pass", "pass", "deferred"},
		{"event_not_observed", "pass", "pass", "pass", "event_not_observed"},
	} {
		verdicts := map[string]string{"synthetic_fresh": tc.fresh, "synthetic_near_expiry": tc.nearExpiry, "real_idle": tc.idle, "real_turn": tc.turn}
		if got := codexUsageOverall(verdicts); got != tc.want {
			t.Fatalf("%v: got %s, want %s", verdicts, got, tc.want)
		}
	}
}

func TestCodexUsageCaptureStrict(t *testing.T) {
	good, err := json.Marshal(codexUsageGoodCapture("turn"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codexUsageDecode(good); err != nil {
		t.Fatal("reviewed capture rejected")
	}
	for _, body := range []string{
		`{"email":"private"}`, `{} {}`,
		strings.Replace(string(good), `"version":"0.147.0"`, `"version":"0.148.0"`, 1),
		strings.Replace(string(good), `"rateLimits.limitId":"string"`, `"rateLimits.limitId":"string|string"`, 1),
		strings.Replace(string(good), `"updated":2`, `"updated":0`, 1),
		strings.Replace(string(good), `"usage":true`, `"usage":false`, 1),
	} {
		if _, err := codexUsageDecode([]byte(body)); err == nil {
			t.Fatalf("unreviewed or ambiguous capture accepted: %s", body)
		}
	}
}

// Exercise the actual in-container reducer with synthetic values. jq is also
// a declared image/runtime tool; this test needs no provider or network.
func TestCodexUsageSanitizer(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal("jq is required for the Codex usage sanitizer tests; install jq and put it on PATH")
	}
	run := func(kind, body string) ([]byte, error) {
		cmd := exec.Command(jq, "-e", "--arg", "kind", kind, "-f", "testdata/codex_usage_sanitize.jq") //nolint:gosec // jq from PATH with a literal fixture and literal kinds
		cmd.Stdin = strings.NewReader(body)
		out, err := cmd.Output()
		if bytes.Contains(out, []byte("private")) {
			t.Fatal("private value escaped")
		}
		return out, err
	}
	window := `{"usedPercent":42,"windowDurationMins":300,"resetsAt":1735689720}`
	snapshot := `{"limitId":"private-limit","limitName":"private-name","primary":` + window + `,"secondary":null,"credits":{"hasCredits":true,"unlimited":false,"balance":"private-balance"},"individualLimit":{"limit":"private-limit","used":"private-used","remainingPercent":68,"resetsAt":1},"planType":"pro","rateLimitReachedType":"rate_limit_reached","spendControlReached":false}`
	for _, tc := range []struct {
		name, kind, body string
		valid            bool
	}{
		{"full read", "read", `{"rateLimits":` + snapshot + `,"rateLimitsByLimitId":{"codex":` + snapshot + `,"private-bucket":{"primary":{"usedPercent":1}}},"rateLimitResetCredits":{"availableCount":3,"credits":[{"id":"private-credit","grantedAt":1,"resetType":"codexRateLimits","status":"available","title":"private-title"}]}}`, true},
		{"minimal read", "read", `{"rateLimits":{}}`, true},
		{"null buckets", "read", `{"rateLimits":{"primary":null},"rateLimitsByLimitId":null,"rateLimitResetCredits":null}`, true},
		{"missing snapshot", "read", `{"rateLimitsByLimitId":{}}`, false},
		{"unknown root", "read", `{"rateLimits":{},"accountId":"private-id"}`, false},
		{"unknown snapshot key", "read", `{"rateLimits":{"userId":"private-id"}}`, false},
		{"unknown bucket key", "read", `{"rateLimits":{},"rateLimitsByLimitId":{"codex":{"email":"private@example.invalid"}}}`, false},
		{"unknown window key", "read", `{"rateLimits":{"primary":{"usedPercent":1,"owner":"private-id"}}}`, false},
		{"unknown credit key", "read", `{"rateLimits":{},"rateLimitResetCredits":{"availableCount":1,"credits":[{"id":"private-credit","token":"private-token"}]}}`, false},
		{"plan leak", "read", `{"rateLimits":{"planType":"private-plan"}}`, false},
		{"reached-type leak", "read", `{"rateLimits":{"rateLimitReachedType":"private-reason"}}`, false},
		{"wrong type", "read", `{"rateLimits":{"primary":{"usedPercent":"private-share"}}}`, false},
		{"fractional share", "read", `{"rateLimits":{"primary":{"usedPercent":1.5}}}`, false},
		{"bucket list", "read", `{"rateLimits":{},"rateLimitsByLimitId":["private-id"]}`, false},
		{"rpc error text", "read", `"private error"`, false},
		{"notification", "updated", `{"rateLimits":` + snapshot + `}`, true},
		{"sparse notification", "updated", `{"rateLimits":{"limitId":null,"primary":{"usedPercent":7}}}`, true},
		{"notification with buckets", "updated", `{"rateLimits":{},"rateLimitsByLimitId":{}}`, false},
		{"notification with thread", "updated", `{"rateLimits":{},"threadId":"private-id"}`, false},
		{"unknown kind", "other", `{"rateLimits":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run(tc.kind, tc.body)
			if (err == nil) != tc.valid {
				t.Fatal("sanitizer acceptance differs")
			}
			if !tc.valid {
				return
			}
			c := codexUsageCapture{Version: "0.147.0", Mode: "idle"}
			if tc.kind == "updated" {
				merged, err := run("merge", "["+string(out)+","+string(out)+"]")
				if err != nil {
					t.Fatal("merge rejected its own reducer output")
				}
				out, c.Usage, c.Fields = merged, true, map[string]string{"rateLimits": "object"}
			}
			if err := json.Unmarshal(out, &c); err != nil {
				t.Fatal(err)
			}
			if !codexUsageCaptureValid(c) || (tc.kind == "updated" && c.Updated != 2) {
				t.Fatalf("sanitizer and host schema disagree: %s", out)
			}
		})
	}
	if _, err := run("merge", `[{"fields":{},"raw":"private"}]`); err == nil {
		t.Fatal("merge accepted an unreviewed entry")
	}
}
