package elaborate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	maxResearchRedirects        = 5
	maxResearchContentTypeBytes = 8 << 10
)

var nonPublicResearchPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
}

var (
	ErrResearchURLRefused  = errors.New("research URL is not allowed")
	ErrResearchTooLarge    = errors.New("research response exceeds the size limit")
	ErrResearchFetchFailed = errors.New("research fetch failed")
)

// IsResearchRequestFailure reports failures caused by untrusted request data
// or external research service behavior. Persistence and reconstruction
// failures remain operational errors so reconciliation can retry them.
func IsResearchRequestFailure(err error) bool {
	return errors.Is(err, ErrResearchURLRefused) ||
		errors.Is(err, ErrResearchTooLarge) ||
		errors.Is(err, ErrResearchFetchFailed)
}

// ResearchArtifact is the immutable result of one daemon fetch.
type ResearchArtifact struct {
	Artifact domain.Artifact
	URL      string
	FinalURL string
}

// ResearchSource attributes prompt-readable evidence to its requested and
// final URL plus the daemon-observed HTTP metadata.
type ResearchSource struct {
	URL         string `json:"url"`
	Purpose     string `json:"purpose"`
	FinalURL    string `json:"final_url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
}

// ResearchEvidence is the prompt-readable reconstruction of one stored
// research envelope. Body is decoded from storage's base64 transport form;
// the source fields keep the evidence attributable without exposing the
// transport wrapper to the elaborator.
type ResearchEvidence struct {
	Source ResearchSource
	Body   string
}

// DecodeResearchEvidence strictly reconstructs prompt-readable evidence from
// the daemon's canonical stored envelope.
func DecodeResearchEvidence(data []byte) (ResearchEvidence, error) {
	var envelope researchEnvelope
	if err := strictjson.Decode(
		data, &envelope, strictjson.RejectInvalidUTF8,
		strictjson.Limit(exec.ProductionMaxInputBytes),
	); err != nil {
		return ResearchEvidence{}, fmt.Errorf("decode research evidence: %w", err)
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, data) {
		return ResearchEvidence{}, fmt.Errorf("decode research evidence: non-canonical envelope: %w",
			domain.ErrImmutableTransition)
	}
	body, err := base64.StdEncoding.DecodeString(envelope.BodyBase64)
	if err != nil || int64(len(body)) > MaxResearchResponseBytes || !utf8.Valid(body) {
		return ResearchEvidence{}, fmt.Errorf("decode research evidence body: %w", domain.ErrParentKeyMismatch)
	}
	request := FetchRequest{URL: envelope.URL, Purpose: envelope.Purpose}
	if err := request.validate(); err != nil {
		return ResearchEvidence{}, fmt.Errorf("decode research evidence request: %w", err)
	}
	final, err := url.Parse(envelope.FinalURL)
	if err != nil || final.Scheme != "https" || final.User != nil || final.Hostname() == "" ||
		final.Fragment != "" || net.ParseIP(final.Hostname()) != nil ||
		envelope.Status < 200 || envelope.Status > 299 {
		return ResearchEvidence{}, fmt.Errorf("decode research evidence metadata: %w", domain.ErrParentKeyMismatch)
	}
	if len(envelope.ContentType) > maxResearchContentTypeBytes {
		return ResearchEvidence{}, fmt.Errorf("decode research evidence content type: %w", ErrResearchTooLarge)
	}
	return ResearchEvidence{
		Source: ResearchSource{
			URL: envelope.URL, Purpose: envelope.Purpose, FinalURL: envelope.FinalURL,
			Status: envelope.Status, ContentType: envelope.ContentType,
		},
		Body: string(body),
	}, nil
}

// Fetcher is the daemon-owned HTTP and persistence boundary for research.
type Fetcher struct {
	store     *store.Store
	blobs     *signet.BlobStore
	transport http.RoundTripper
}

// NewFetcher constructs a fetcher. A nil transport selects the production
// transport, which resolves and dials the same public address and uses no
// ambient proxy. Tests may inject a transport without weakening URL policy.
func NewFetcher(st *store.Store, blobs *signet.BlobStore, transport http.RoundTripper) (*Fetcher, error) {
	if st == nil || blobs == nil {
		return nil, errors.New("research fetcher requires store and blob store")
	}
	if transport == nil {
		transport = secureResearchTransport()
	}
	return &Fetcher{store: st, blobs: blobs, transport: transport}, nil
}

// Fetch enforces the allowlist, bounds the response, stores a URL-bound
// envelope in the blob store, then registers its immutable research artifact.
func (f *Fetcher) Fetch(
	ctx context.Context,
	producer domain.InvocationID,
	ordinal int,
	request FetchRequest,
	allowlist []string,
	maxBytes int64,
) (ResearchArtifact, error) {
	if producer == "" || ordinal < 1 || maxBytes < 1 {
		return ResearchArtifact{}, errors.New("research fetch requires producer, positive ordinal, and size limit")
	}
	if maxBytes > MaxResearchResponseBytes {
		return ResearchArtifact{}, fmt.Errorf("research response size limit exceeds %d", MaxResearchResponseBytes)
	}
	allowed, err := parseAllowlist(allowlist)
	if err != nil {
		return ResearchArtifact{}, err
	}
	parsed, err := validateResearchURL(request.URL, allowed)
	if err != nil {
		return ResearchArtifact{}, err
	}
	artifactID := domain.ArtifactID(fmt.Sprintf("research-%s-%d", producer, ordinal))
	if recovered, found, err := f.recover(ctx, artifactID, producer, request, allowed, maxBytes); err != nil {
		return ResearchArtifact{}, err
	} else if found {
		return recovered, nil
	}

	client := &http.Client{
		Transport: f.transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(next *http.Request, prior []*http.Request) error {
			if len(prior) >= maxResearchRedirects {
				return fmt.Errorf("research redirect limit %d exceeded", maxResearchRedirects)
			}
			if _, err := validateResearchURL(next.URL.String(), allowed); err != nil {
				return fmt.Errorf("research redirect: %w", err)
			}
			// Requests are created from daemon-owned fields only. Explicitly
			// drop credential-bearing headers even if a future caller adds one.
			next.Header.Del("Authorization")
			next.Header.Del("Cookie")
			next.Header.Del("Proxy-Authorization")
			return nil
		},
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return ResearchArtifact{}, fmt.Errorf("create research request: %w", err)
	}
	httpRequest.Header.Set("Accept", "text/plain, text/html, application/json;q=0.9, */*;q=0.1")
	httpRequest.Header.Set("User-Agent", "freeside-research-fetcher/1")
	response, err := client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return ResearchArtifact{}, ctx.Err()
		}
		return ResearchArtifact{}, fmt.Errorf("%w: fetch research %q: %w", ErrResearchFetchFailed, request.URL, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Request == nil || response.Request.URL == nil {
		return ResearchArtifact{}, fmt.Errorf("%w: fetch research %q: response has no final URL", ErrResearchFetchFailed, request.URL)
	}
	finalURL, err := validateResearchURL(response.Request.URL.String(), allowed)
	if err != nil {
		return ResearchArtifact{}, fmt.Errorf("fetch research %q final URL: %w", request.URL, err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return ResearchArtifact{}, fmt.Errorf("%w: fetch research %q: HTTP %d", ErrResearchFetchFailed, request.URL, response.StatusCode)
	}
	limited := &io.LimitedReader{R: response.Body, N: maxBytes + 1}
	body, err := io.ReadAll(limited)
	if err != nil {
		if ctx.Err() != nil {
			return ResearchArtifact{}, ctx.Err()
		}
		return ResearchArtifact{}, fmt.Errorf("%w: read research %q: %w", ErrResearchFetchFailed, request.URL, err)
	}
	if int64(len(body)) > maxBytes {
		return ResearchArtifact{}, fmt.Errorf("%w: fetch research %q: %w", ErrResearchFetchFailed, request.URL, ErrResearchTooLarge)
	}
	if !utf8.Valid(body) {
		return ResearchArtifact{}, fmt.Errorf("%w: fetch research %q: response is not valid UTF-8",
			ErrResearchFetchFailed, request.URL)
	}

	contentType := response.Header.Get("Content-Type")
	if len(contentType) > maxResearchContentTypeBytes {
		return ResearchArtifact{}, fmt.Errorf("%w: fetch research %q content type exceeds %d bytes: %w",
			ErrResearchFetchFailed, request.URL, maxResearchContentTypeBytes, ErrResearchTooLarge)
	}
	envelope, err := json.Marshal(researchEnvelope{
		URL: request.URL, Purpose: request.Purpose, FinalURL: finalURL.String(),
		Status: response.StatusCode, ContentType: contentType,
		BodyBase64: base64.StdEncoding.EncodeToString(body),
	})
	if err != nil {
		return ResearchArtifact{}, fmt.Errorf("encode research %q: %w", request.URL, err)
	}
	if int64(len(envelope)) > maxResearchEnvelopeBytes(maxBytes) {
		return ResearchArtifact{}, fmt.Errorf("%w: fetch research %q encoded envelope is %d bytes: %w",
			ErrResearchFetchFailed, request.URL, len(envelope), ErrResearchTooLarge)
	}
	digest := domain.Digest(contentaddr.Sum(envelope))
	if _, err := f.blobs.Put(digest, bytes.NewReader(envelope)); err != nil {
		return ResearchArtifact{}, fmt.Errorf("store research %q: %w", request.URL, err)
	}
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID:   artifactID,
		Type: domain.ArtifactKindResearch, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: producer,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		// The digest names the JSON research envelope. This registration is
		// replay-guarded by recover (an existing artifact is authenticated and
		// reused, never re-put), so a wall-clock created_at is stamped once.
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(envelope)),
			CreatedAt: time.Now().UTC(), Source: domain.EvidenceSourceRun,
			Availability: domain.EvidenceAvailable,
		},
	}, nil)
	if err != nil {
		return ResearchArtifact{}, err
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutArtifact(ctx, artifact)
	}); err != nil {
		return ResearchArtifact{}, fmt.Errorf("register research %q: %w", request.URL, err)
	}
	return ResearchArtifact{Artifact: artifact, URL: request.URL, FinalURL: finalURL.String()}, nil
}

// recover returns the immutable result already registered for an invocation
// ordinal. The artifact and its bytes are authenticated before reuse, and the
// original URL must match, so retrying after a crash neither refetches mutable
// web content nor lets the same durable identity be retargeted.
func (f *Fetcher) recover(
	ctx context.Context,
	id domain.ArtifactID,
	producer domain.InvocationID,
	request FetchRequest,
	allowed allowedOrigins,
	maxBytes int64,
) (ResearchArtifact, bool, error) {
	var artifact domain.Artifact
	err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		artifact, err = tx.GetArtifact(ctx, id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return ResearchArtifact{}, false, nil
	}
	if err != nil {
		return ResearchArtifact{}, false, err
	}
	if artifact.Type != domain.ArtifactKindResearch ||
		artifact.Provenance.ProducerClass != domain.ProducerDaemon ||
		artifact.Provenance.ProducerInvocationID != producer ||
		artifact.Provenance.HeadBinding != domain.HeadIndependent {
		return ResearchArtifact{}, false, fmt.Errorf("recover research %q: %w", id, domain.ErrParentKeyMismatch)
	}
	reader, err := f.blobs.OpenContext(ctx, artifact.Digest)
	if err != nil {
		return ResearchArtifact{}, false, fmt.Errorf("recover research %q: %w", id, err)
	}
	defer func() { _ = reader.Close() }()
	limit := strictjson.Limit(maxResearchEnvelopeBytes(maxBytes))
	var envelope researchEnvelope
	if err := strictjson.DecodeReader(reader, &envelope, strictjson.RejectInvalidUTF8, limit); err != nil {
		if errors.Is(err, strictjson.ErrLimitExceeded) {
			return ResearchArtifact{}, false, fmt.Errorf("%w: recover research %q: %w",
				ErrResearchTooLarge, id, err)
		}
		return ResearchArtifact{}, false, fmt.Errorf("recover research %q: %w", id, err)
	}
	body, err := json.Marshal(envelope)
	if err != nil || domain.Digest(contentaddr.Sum(body)) != artifact.Digest {
		return ResearchArtifact{}, false, fmt.Errorf("recover research %q: blob digest mismatch", id)
	}
	decoded, err := base64.StdEncoding.DecodeString(envelope.BodyBase64)
	if err != nil || int64(len(decoded)) > maxBytes || envelope.URL != request.URL ||
		envelope.Purpose != request.Purpose ||
		envelope.Status < 200 || envelope.Status > 299 {
		return ResearchArtifact{}, false, fmt.Errorf("recover research %q: envelope disagrees: %w", id, domain.ErrParentKeyMismatch)
	}
	if !utf8.Valid(decoded) {
		return ResearchArtifact{}, false, fmt.Errorf("%w: recover research %q: response is not valid UTF-8",
			ErrResearchFetchFailed, id)
	}
	final, err := validateResearchURL(envelope.FinalURL, allowed)
	if err != nil {
		return ResearchArtifact{}, false, fmt.Errorf("recover research %q final URL: %w", id, err)
	}
	return ResearchArtifact{Artifact: artifact, URL: envelope.URL, FinalURL: final.String()}, true, nil
}

func maxResearchEnvelopeBytes(maxBytes int64) int64 {
	return min(maxBytes*2+(1<<20), exec.ProductionMaxInputBytes)
}

type researchEnvelope struct {
	URL         string `json:"url"`
	Purpose     string `json:"purpose"`
	FinalURL    string `json:"final_url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	BodyBase64  string `json:"body_base64"`
}

type allowedOrigins map[string]struct{}

func parseAllowlist(raw []string) (allowedOrigins, error) {
	if len(raw) == 0 {
		return nil, errors.New("research allowlist is empty")
	}
	allowed := make(allowedOrigins, len(raw))
	for _, entry := range raw {
		parsed, err := url.Parse(entry)
		if err != nil || parsed.Scheme != "https" || parsed.User != nil ||
			parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
			net.ParseIP(parsed.Hostname()) != nil {
			return nil, fmt.Errorf("research allowlist entry %q must be an HTTPS origin with a DNS host", entry)
		}
		origin, err := canonicalOrigin(parsed)
		if err != nil {
			return nil, fmt.Errorf("research allowlist entry %q: %w", entry, err)
		}
		allowed[origin] = struct{}{}
	}
	return allowed, nil
}

func validateResearchURL(raw string, allowed allowedOrigins) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" ||
		parsed.Fragment != "" || net.ParseIP(parsed.Hostname()) != nil {
		return nil, fmt.Errorf("%w: %q must be HTTPS, credential-free, fragment-free, and DNS-hosted", ErrResearchURLRefused, raw)
	}
	origin, err := canonicalOrigin(parsed)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResearchURLRefused, err)
	}
	if _, ok := allowed[origin]; !ok {
		return nil, fmt.Errorf("%w: origin %s", ErrResearchURLRefused, origin)
	}
	return parsed, nil
}

func canonicalOrigin(parsed *url.URL) (string, error) {
	port := parsed.Port()
	if port == "" {
		port = "443"
	} else {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 || strconv.Itoa(value) != port {
			return "", errors.New("port is not canonical")
		}
	}
	return strings.ToLower(parsed.Hostname()) + ":" + port, nil
}

func secureResearchTransport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, candidate := range addresses {
			if !publicResearchIP(candidate.IP) {
				return nil, fmt.Errorf("research host %q resolved to a non-public address", host)
			}
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("research host %q resolved to no addresses", host)
		}
		dialer := net.Dialer{Timeout: 15 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}
	return transport
}

func publicResearchIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}
	for _, prefix := range nonPublicResearchPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
